// Package runner drains the queue: it claims tasks atomically, executes
// them as direct process spawns (no shell), heartbeats while they run,
// and kills whole process trees on cancellation — process groups on
// Unix, Job Objects on Windows (see proc_*.go).
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
	"sync"
	"time"

	"github.com/andrewsinnovations/forebay/internal/store"
)

const (
	heartbeatEvery = 5 * time.Second
	staleAfter     = 60 * time.Second
	// gracePeriod is the duration between requesting cancellation
	// and forcibly terminating the process tree.
	gracePeriod = 5 * time.Second
)

type Options struct {
	// Workers is the number of parallel task slots; 1 means sequential execution.
	Workers int
	// Batch restricts execution to a single batch by name or ID.
	// Empty means process tasks from all batches.
	Batch string
	// Watch keeps polling for new tasks after the queue drains.
	Watch bool
	// Interval is the poll interval when in watch mode.
	Interval time.Duration
}

// Run drains the queue until empty or interrupted. In watch mode it
// polls forever. Returns once all claimed work is settled.
func Run(db *store.DB, opts Options) error {
	if opts.Workers < 1 {
		opts.Workers = 1
	}
	if opts.Interval <= 0 {
		opts.Interval = 5 * time.Second
	}

	if n, err := db.ReclaimStale(staleAfter); err != nil {
		return err
	} else if n > 0 {
		fmt.Printf("reclaimed %d stale task(s) from a previous run\n", n)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	runnerID := runnerID()
	var wg sync.WaitGroup
	for i := 0; i < opts.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			workerLoop(ctx, db, runnerID, opts)
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		fmt.Println("interrupted: running tasks were killed and requeued")
	}
	return nil
}

// runnerID returns a unique identifier for this runner instance.
func runnerID() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

// workerLoop claims and executes tasks until the context is canceled
// or the queue is empty (in non-watch mode).
func workerLoop(ctx context.Context, db *store.DB, runnerID string, opts Options) {
	for {
		if ctx.Err() != nil {
			return
		}
		task, err := db.Claim(runnerID, opts.Batch)
		if errors.Is(err, store.ErrNoTask) {
			if !opts.Watch {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(opts.Interval):
				continue
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "claim failed: %v\n", err)
			return
		}
		if task.Kind == store.KindLLM {
			executeLLMTask(ctx, db, task, opts.Workers == 1)
		} else {
			executeTask(ctx, db, task, opts.Workers == 1)
		}
	}
}

// executeTask runs one claimed task to a terminal status. Every exit
// path settles the row: done/failed/canceled, or requeued on interrupt.
func executeTask(ctx context.Context, db *store.DB, task *store.Task, tee bool) {
	batch, err := db.GetBatch(task.BatchID)
	if err != nil {
		db.MarkFinished(task.ID, store.StatusFailed, -1, err.Error(), "")
		return
	}

	logDir := filepath.Join(db.LogsDir(), batch.ID)
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		db.MarkFinished(task.ID, store.StatusFailed, -1, fmt.Sprintf("create log dir: %v", err), "")
		return
	}
	logPath := filepath.Join(logDir, task.ID+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		db.MarkFinished(task.ID, store.StatusFailed, -1, fmt.Sprintf("create log file: %v", err), "")
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
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Stdin = nil
	setupProcAttr(cmd)

	fmt.Printf("[%s/%s] start: %s\n", batch.Name, task.ID, displayCommand(task.Argv))
	start := time.Now()
	if err := cmd.Start(); err != nil {
		db.MarkFinished(task.ID, store.StatusFailed, -1, fmt.Sprintf("spawn: %v", err), "")
		fmt.Printf("[%s/%s] failed to start: %v\n", batch.Name, task.ID, err)
		return
	}
	db.MarkStarted(task.ID, logPath)

	tree, err := newProcTree(cmd)
	if err != nil {
		// Tree tracking failed; we can still manage the direct child.
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
			exitCode, errMsg := 0, ""
			status := store.StatusDone
			if waitErr != nil {
				status = store.StatusFailed
				errMsg = waitErr.Error()
				exitCode = -1
				var exitErr *exec.ExitError
				if errors.As(waitErr, &exitErr) {
					exitCode = exitErr.ExitCode()
				}
			}
			db.MarkFinished(task.ID, status, exitCode, errMsg, "")
			fmt.Printf("[%s/%s] %s (exit %d, %s)\n",
				batch.Name, task.ID, status, exitCode, time.Since(start).Round(time.Second))
			return

		case <-ticker.C:
			cancel, err := db.Touch(task.ID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[%s/%s] heartbeat failed: %v\n", batch.Name, task.ID, err)
				continue
			}
			if cancel {
				killTree(cmd, tree, waitCh)
				db.MarkFinished(task.ID, store.StatusCanceled, -1, "canceled by user", "")
				fmt.Printf("[%s/%s] canceled\n", batch.Name, task.ID)
				return
			}

		case <-ctx.Done():
			killTree(cmd, tree, waitCh)
			db.Requeue(task.ID)
			return
		}
	}
}

// killTree soft-stops the task's process tree, waits out the grace
// period, then hard-kills whatever is left and reaps the child.
func killTree(cmd *exec.Cmd, tree *procTree, waitCh <-chan error) {
	if tree != nil {
		tree.Terminate()
	} else if cmd.Process != nil {
		cmd.Process.Signal(os.Interrupt)
	}
	select {
	case <-waitCh:
		return
	case <-time.After(gracePeriod):
	}
	if tree != nil {
		tree.Kill()
	} else if cmd.Process != nil {
		cmd.Process.Kill()
	}
	<-waitCh
}

// displayCommand formats an argument vector for logging, truncating
// long arguments for readability.
func displayCommand(argv []string) string {
	s := ""
	for i, a := range argv {
		if i > 0 {
			s += " "
		}
		if len(a) > 60 {
			a = a[:57] + "..."
		}
		s += a
	}
	return s
}
