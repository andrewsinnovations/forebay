// Package runner drains the forebay queue: it claims tasks atomically,
// executes them as direct process spawns (never through a shell), heartbeats
// while they run, and kills whole process trees on cancellation - process
// groups on Unix, Job Objects on Windows (see proc_*.go).
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andrewsinnovations/forebay/internal/store"
)

const (
	// heartbeatEvery is how often a running task refreshes its heartbeat.
	heartbeatEvery = 5 * time.Second
	// staleAfter is the heartbeat age after which another run may reclaim a
	// task left in the running state by a crashed runner.
	staleAfter = 60 * time.Second
	// gracePeriod is how long a canceled task's process tree has to exit
	// after being asked to terminate before it is killed outright.
	gracePeriod = 5 * time.Second
	// defaultInterval is the poll interval used in watch mode when Options
	// leaves Interval unset.
	defaultInterval = 5 * time.Second
	// maxArgLen is the length beyond which a single command argument is
	// elided in status output.
	maxArgLen = 60
	// maxResultEnv is the environment variable name for the maximum result
	// bytes to capture from a task log.
	maxResultEnv = "FOREBAY_MAX_RESULT_BYTES"
)

// Options configures a call to Run. Zero values select the defaults noted per
// field.
type Options struct {
	// Workers is the number of tasks to run in parallel; 1 runs sequentially.
	Workers int
	// Batch restricts execution to a single batch by name or ID.
	// Empty means process tasks from all batches.
	Batch string
	// Watch keeps polling for new tasks after the queue drains.
	Watch bool
	// Interval is the poll interval in watch mode; zero selects 5s.
	Interval time.Duration
}

// Run drains the queue until it empties, ctx is canceled, or the process is
// interrupted. In watch mode it polls until interrupted. Run returns once all
// claimed work has settled: each task ends done, failed, canceled, or requeued
// for the next run.
func Run(ctx context.Context, db *store.DB, opts Options) error {
	if opts.Workers < 1 {
		opts.Workers = 1
	}
	if opts.Interval <= 0 {
		opts.Interval = defaultInterval
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	n, err := db.ReclaimStale(staleAfter)
	if err != nil {
		return fmt.Errorf("reclaim stale tasks: %w", err)
	}
	if n > 0 {
		fmt.Printf("reclaimed %d stale task(s) from a previous run\n", n)
	}

	// One identity is shared by every worker so a task claimed by this
	// process can be attributed to it in the database.
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	id := fmt.Sprintf("%s-%d", host, os.Getpid())
	var wg sync.WaitGroup
	for range opts.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			work(ctx, db, id, opts)
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		fmt.Println("interrupted: running tasks were killed and requeued")
	}
	return nil
}

// work claims and executes tasks until ctx is canceled, or until the queue is
// empty when opts.Watch is false. Claim failures are reported and end the
// worker: a queue that cannot be read will not become readable by trying
// again.
func work(ctx context.Context, db *store.DB, runnerID string, opts Options) {
	for {
		if ctx.Err() != nil {
			return
		}
		task, err := db.Claim(runnerID, opts.Batch)
		switch {
		case errors.Is(err, store.ErrNoTask):
			if !opts.Watch {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(opts.Interval):
				continue
			}
		case err != nil:
			fmt.Fprintf(os.Stderr, "claim failed: %v\n", err)
			return
		}
		// Tee only a solo worker's output: with several workers the lines
		// interleave into noise, and the log file holds everything anyway.
		tee := opts.Workers == 1
		runTask(ctx, db, task, tee)
	}
}

// runTask executes one claimed task and settles its row to a terminal status.
// Every exit path settles the row: done, failed, or canceled, or requeued when
// ctx is canceled mid-task. With tee set, the task's output is also copied to
// stdout.
func runTask(ctx context.Context, db *store.DB, task *store.Task, tee bool) {
	batch, err := db.GetBatch(task.BatchID)
	if err != nil {
		fail(db, task, fmt.Errorf("look up batch: %w", err))
		return
	}
	logFile, logPath, err := createLog(db.LogsDir(), batch.ID, task.ID)
	if err != nil {
		fail(db, task, err)
		return
	}
	defer logFile.Close()

	cmd := exec.Command(task.Argv[0], task.Argv[1:]...)
	cmd.Dir = batch.Workdir
	if len(batch.Env) > 0 {
		cmd.Env = batch.Env
	}
	var out io.Writer = logFile
	if tee {
		out = io.MultiWriter(logFile, os.Stdout)
	}
	cmd.Stdout, cmd.Stderr, cmd.Stdin = out, out, nil
	setupProcAttr(cmd)

	parts := make([]string, len(task.Argv))
	for i, a := range task.Argv {
		if len(a) <= maxArgLen {
			parts[i] = a
		} else {
			parts[i] = a[:maxArgLen-3] + "..."
		}
	}
	fmt.Printf("[%s/%s] start: %s\n", batch.Name, task.ID, strings.Join(parts, " "))
	start := time.Now()
	if err := cmd.Start(); err != nil {
		fail(db, task, fmt.Errorf("spawn %q: %w", task.Argv[0], err))
		fmt.Printf("[%s/%s] failed to start: %v\n", batch.Name, task.ID, err)
		return
	}
	if err := db.MarkStarted(task.ID, logPath); err != nil {
		fmt.Fprintf(os.Stderr, "[%s/%s] %v\n", batch.Name, task.ID, err)
	}

	tree, err := newProcTree(cmd)
	if err != nil {
		// Tree tracking is unavailable, but the direct child can still be
		// signaled, so keep running the task.
		fmt.Fprintf(os.Stderr, "[%s/%s] warning: process tree tracking unavailable: %v\n",
			batch.Name, task.ID, err)
	}
	if tree != nil {
		defer tree.Close()
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	ticker := time.NewTicker(heartbeatEvery)
	defer ticker.Stop()

	for {
		select {
		case waitErr := <-waitCh:
			exitCode, status, errMsg := 0, store.StatusDone, ""
			if waitErr != nil {
				status, errMsg, exitCode = store.StatusFailed, waitErr.Error(), -1
				var exitErr *exec.ExitError
				if errors.As(waitErr, &exitErr) {
					exitCode = exitErr.ExitCode()
				}
			}
			settle(db, task, status, exitCode, errMsg, logPath)
			fmt.Printf("[%s/%s] %s (exit %d, %s)\n",
				batch.Name, task.ID, status, exitCode, time.Since(start).Round(time.Second).String())
			return

		case <-ticker.C:
			cancelRequested, err := heartbeat(db, task.ID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[%s/%s] %v\n", batch.Name, task.ID, err)
				continue
			}
			if cancelRequested {
				stopTree(cmd, tree, waitCh)
				settle(db, task, store.StatusCanceled, -1, "canceled by user", logPath)
				fmt.Printf("[%s/%s] canceled\n", batch.Name, task.ID)
				return
			}

		case <-ctx.Done():
			stopTree(cmd, tree, waitCh)
			requeue(db, task.ID)
			return
		}
	}
}

// heartbeat refreshes a running task's heartbeat and reports whether
// cancellation has been requested. A failed heartbeat is returned as an error
// for the caller to report; the task keeps running either way.
func heartbeat(db *store.DB, taskID string) (cancelRequested bool, err error) {
	cancelled, err := db.Touch(taskID)
	if err != nil {
		return false, fmt.Errorf("heartbeat: %w", err)
	}
	return cancelled, nil
}

// settle records a terminal status for task and saves its log as the result.
func settle(db *store.DB, task *store.Task, status string, exitCode int, errMsg, logPath string) {
	var limit int64
	if v := os.Getenv(maxResultEnv); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			fmt.Fprintf(os.Stderr, "warning: ignoring invalid %s=%q\n", maxResultEnv, v)
		} else {
			limit = n
		}
	}

	var result string
	if logPath != "" {
		if f, err := os.Open(logPath); err == nil {
			defer f.Close()
			if info, err := f.Stat(); err == nil {
				size := info.Size()
				if limit <= 0 || size <= limit {
					if data, err := io.ReadAll(f); err == nil {
						result = string(data)
					}
				} else {
					head := make([]byte, limit/2)
					if _, err := io.ReadFull(f, head); err == nil {
						tail := make([]byte, limit-int64(len(head)))
						if _, err := f.ReadAt(tail, size-int64(len(tail))); err == nil {
							result = fmt.Sprintf("%s\n... [%d bytes omitted; full output: %s] ...\n%s",
								head, size-limit, logPath, tail)
						}
					}
				}
			}
		}
	}

	if err := db.MarkFinished(task.ID, status, exitCode, errMsg, result); err != nil {
		fmt.Fprintf(os.Stderr, "task %s: %v\n", task.ID, err)
	}
}

// fail records a startup failure for task: the task never ran, so there is no
// exit code and no output to save.
func fail(db *store.DB, task *store.Task, err error) {
	if ferr := db.MarkFinished(task.ID, store.StatusFailed, -1, err.Error(), ""); ferr != nil {
		fmt.Fprintf(os.Stderr, "task %s: %v\n", task.ID, err)
	}
}

// requeue returns a task to the pending set after its runner was interrupted.
func requeue(db *store.DB, taskID string) {
	if err := db.Requeue(taskID); err != nil {
		fmt.Fprintf(os.Stderr, "task %s: %v\n", taskID, err)
	}
}

// createLog opens the per-task log file under root/batchID/taskID.log,
// creating directories as needed, and returns it with its path.
func createLog(root, batchID, taskID string) (*os.File, string, error) {
	dir := filepath.Join(root, batchID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", fmt.Errorf("create log dir: %w", err)
	}
	path := filepath.Join(dir, taskID+".log")
	f, err := os.Create(path)
	if err != nil {
		return nil, "", fmt.Errorf("create log file: %w", err)
	}
	return f, path, nil
}

// stopTree soft-stops the task's process tree, waits out the grace period,
// then hard-kills whatever remains and reaps the child. It returns once the
// process has exited.
func stopTree(cmd *exec.Cmd, tree *procTree, waitCh <-chan error) {
	signalTree(cmd, tree, true)
	select {
	case <-waitCh:
		return
	case <-time.After(gracePeriod):
	}
	signalTree(cmd, tree, false)
	<-waitCh
}

// signalTree sends the graceful (soft) or fatal (hard) signal to the tracked
// process tree, falling back to the direct child when tree tracking is
// unavailable.
func signalTree(cmd *exec.Cmd, tree *procTree, soft bool) {
	switch {
	case tree != nil && soft:
		tree.Terminate()
	case tree != nil:
		tree.Kill()
	case cmd.Process == nil:
	case soft:
		// Best effort: a task that ignores SIGINT is killed after the grace
		// period.
		_ = cmd.Process.Signal(os.Interrupt)
	default:
		_ = cmd.Process.Kill()
	}
}
