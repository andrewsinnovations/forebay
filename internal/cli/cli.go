// Package cli implements the forebay command-line interface.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrewsinnovations/forebay/internal/expand"
	"github.com/andrewsinnovations/forebay/internal/runner"
	"github.com/andrewsinnovations/forebay/internal/store"
)

const usage = `forebay - queue commands now, run them when you choose

Usage:
  forebay add    [--batch NAME] [--dir DIR] -- CMD [ARGS...]
  forebay batch  [--name NAME] --glob PATTERN [--glob ...] [--dir ROOT]
                 [--exclude PATTERN ...] [--dry-run] [--command CMD] -- CMD-TEMPLATE [ARGS...]
  forebay run    [-j N] [--batch NAME] [--watch] [--interval SECONDS]
  forebay status
  forebay list   [--batch NAME] [--status STATUS] [--limit N]
  forebay results [TASK_ID] [--batch NAME] [--status STATUS] [--contains TEXT]
                  [--limit N] [--json]
  forebay logs   TASK_ID
  forebay cancel [TASK_ID] [--batch NAME] [--all]
  forebay reset  [--failed] [--all]
  forebay clean  [--batch NAME] [--all]
  forebay summary
Commands are argv arrays - forebay never invokes a shell. In batch
templates, use placeholders per matched file: {path} {slashpath}
{relpath} {name} {base} {dir}.

Every task saves its output as a result: the captured stdout+stderr for
commands. Query them with "forebay results" (whole log; set
FOREBAY_MAX_RESULT_BYTES to cap what is stored).

Example:
  forebay batch --name jsdoc --glob "src/**/*.js" -- \
    claude -p "analyze '{relpath}' and add jsdoc comments to every function"
  forebay run -j 4
  forebay results --batch jsdoc
`

// Run is the main entry point for the forebay CLI. It returns an exit code.
func Run(args []string) int {
	if len(args) < 1 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "add":
		err = add(rest)
	case "batch":
		err = batchQueued(rest)
	case "results":
		err = results(rest)
	case "run":
		err = run(rest)
	case "status":
		err = status(rest)
	case "list":
		err = listTasks(rest)
	case "logs":
		err = logs(rest)
	case "cancel":
		err = cancel(rest)
	case "reset":
		err = reset(rest)
	case "clean":
		err = clean(rest)
	case "summary":
		err = summary(rest)
	case "help", "-h", "--help":
		fmt.Print(usage)
		return 0
	case "version", "--version":
		fmt.Println("forebay 0.4.0")
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "forebay %s: %v\n", cmd, err)
		return 1
	}
	return 0
}

// splitArgs splits args at the last "--" separator, returning the flags and
// the trailing command respectively. This allows multiple -- to be used for
// template placeholders in batch commands.
func splitArgs(args []string) (flags, rest []string) {
	lastDash := -1
	for i, a := range args {
		if a == "--" {
			lastDash = i
		}
	}
	if lastDash == -1 {
		return args, nil
	}
	rest = args[lastDash+1:]
	flags = args[:lastDash]
	return flags, rest
}

// add queues a single task onto a batch.
func add(args []string) error {
	flagArgs, command := splitArgs(args)
	batchName := "default"
	dirVal := ""
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	fs.String("batch", batchName, "batch name to queue onto (created if missing)")
	fs.String("dir", dirVal, "working directory for the batch (default: current directory; only applies on batch creation)")
	commandFlag := fs.String("command", "", "full command as a string (alternative to specifying command after --)")
	if err := fs.Parse(flagArgs); err != nil {
		return fmt.Errorf("parsing flags: %w", err)
	}
	if *commandFlag != "" {
		command = strings.Fields(*commandFlag)
	} else if len(command) == 0 {
		return errors.New("no command given; usage: forebay add [--batch NAME] -- CMD [ARGS...] or forebay add --command \"CMD [ARGS...]\"")
	}
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	workdir, err := resolveWorkdir(dirVal)
	if err != nil {
		return err
	}
	batch, err := db.EnsureBatch(batchName, workdir, os.Environ())
	if err != nil {
		return err
	}
	id, err := db.AddTask(batch.ID, command)
	if err != nil {
		return err
	}
	fmt.Printf("queued task %s on batch %q\n", id, batch.Name)
	return nil
}

// batchQueued queues tasks from glob patterns against a command template.
func batchQueued(args []string) error {
	flagArgs, template := splitArgs(args)
	fs := flag.NewFlagSet("batch", flag.ExitOnError)
	name := fs.String("name", "", "batch name (default: batch-<id>)")
	dir := fs.String("dir", "", "root directory for glob expansion and task execution (default: current directory)")
	dryRun := fs.Bool("dry-run", false, "print the expanded tasks without queueing them")
	var globs, excludes multiFlag
	fs.Var(&globs, "glob", "glob pattern relative to --dir, e.g. \"src/**/*.js\" (repeatable)")
	fs.Var(&excludes, "exclude", "glob pattern to skip (repeatable; default: **/node_modules/**, **/.git/**)")
	templateFlag := fs.String("command", "", "full command template as a string (alternative to specifying command after --)")
	fs.Parse(flagArgs)
	if len(globs) == 0 {
		return errors.New("at least one --glob is required")
	}
	if *templateFlag != "" {
		template = strings.Fields(*templateFlag)
	} else if len(template) == 0 {
		return errors.New("no command template given; usage: forebay batch [--name NAME] --glob PATTERN [--glob ...] [--command \"CMD-TEMPLATE [ARGS...]\"] -- CMD-TEMPLATE [ARGS...]")
	}
	root, err := resolveWorkdir(*dir)
	if err != nil {
		return err
	}
	ex := excludes
	if len(ex) == 0 {
		ex = expand.DefaultExcludes
	}
	files, err := expand.Files(root, globs, ex)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no files matched %s under %s", strings.Join(globs, ", "), root)
	}
	if *dryRun {
		for _, f := range files {
			fmt.Printf("%v\n", expand.Render(template, root, f))
		}
		fmt.Printf("(%d tasks; dry run, nothing queued)\n", len(files))
		return nil
	}
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	batchName := *name
	if batchName == "" {
		batchName = "batch-" + store.NewID()
	}
	batch, err := db.EnsureBatch(batchName, root, os.Environ())
	if err != nil {
		return err
	}
	for _, f := range files {
		if _, err := db.AddTask(batch.ID, expand.Render(template, root, f)); err != nil {
			return err
		}
	}
	fmt.Printf("queued %d tasks on batch %q - run them with: forebay run --batch %s\n",
		len(files), batch.Name, batch.Name)
	return nil
}

// run executes queued tasks.
func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	workers := fs.Int("j", 1, "number of tasks to run in parallel")
	batchName := fs.String("batch", "", "only run tasks from this batch")
	watch := fs.Bool("watch", false, "keep polling for new tasks after the queue drains")
	interval := fs.Int("interval", 5, "poll interval in seconds for --watch")
	fs.Parse(args)
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	if *batchName != "" && !*watch {
		if _, err := db.GetBatch(*batchName); err != nil {
			return err
		}
	}
	return runner.Run(context.Background(), db, runner.Options{
		Workers:  *workers,
		Batch:    *batchName,
		Watch:    *watch,
		Interval: time.Duration(*interval) * time.Second,
	})
}

// status prints per-batch task count summaries.
func status(args []string) error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	statuses, err := db.Status()
	if err != nil {
		return err
	}
	if len(statuses) == 0 {
		fmt.Println("queue is empty - add tasks with `forebay add` or `forebay batch`")
		return nil
	}
	fmt.Printf("%-16s %-8s %8s %8s %8s %8s %9s\n",
		"BATCH", "ID", "PENDING", "RUNNING", "DONE", "FAILED", "CANCELED")
	for _, s := range statuses {
		fmt.Printf("%-16s %-8s %8d %8d %8d %8d %9d\n",
			truncate(s.Batch.Name, 16), s.Batch.ID,
			s.Pending, s.Running, s.Done, s.Failed, s.Canceled)
	}
	return nil
}

// listTasks prints tasks matching the given filters.
func listTasks(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	batchName := fs.String("batch", "", "filter by batch name")
	status := fs.String("status", "", "filter by status (pending|running|done|failed|canceled)")
	limit := fs.Int("limit", 100, "maximum tasks to show")
	fs.Parse(args)
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	var batchID string
	if *batchName != "" {
		b, err := db.GetBatch(*batchName)
		if err != nil {
			return err
		}
		batchID = b.ID
	}
	tasks, err := db.ListTasks(batchID, *status, "", *limit)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		fmt.Println("no matching tasks")
		return nil
	}
	fmt.Printf("%-8s %-16s %-9s %-5s %s\n", "ID", "BATCH", "STATUS", "EXIT", "COMMAND")
	for _, t := range tasks {
		exit := "-"
		if t.ExitCode != nil {
			exit = fmt.Sprintf("%d", *t.ExitCode)
		}

		desc := strings.Join(t.Argv, " ")
		fmt.Printf("%-8s %-16s %-9s %-5s %s\n",
			t.ID, truncate(t.BatchName, 16), t.Status, exit,
			truncate(desc, 80))
	}
	return nil
}

// logs prints the captured output of a task.
func logs(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: forebay logs TASK_ID")
	}
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	task, err := db.GetTask(args[0])
	if err != nil {
		return err
	}
	if task.LogPath == "" {
		return fmt.Errorf("task %s has not started yet (status: %s)", task.ID, task.Status)
	}
	data, err := os.ReadFile(task.LogPath)
	if err != nil {
		return fmt.Errorf("read task log: %w", err)
	}
	if _, err := os.Stdout.Write(data); err != nil {
		return fmt.Errorf("write task log: %w", err)
	}
	fmt.Fprintf(os.Stderr, "\n(log file: %s)\n", task.LogPath)
	return nil
}

// cancel stops pending or running tasks.
func cancel(args []string) error {
	fs := flag.NewFlagSet("cancel", flag.ExitOnError)
	batchName := fs.String("batch", "", "cancel every pending/running task in this batch")
	all := fs.Bool("all", false, "cancel every pending/running task in the queue")
	fs.Parse(args)
	var taskID string
	if fs.NArg() > 0 {
		taskID = fs.Arg(0)
	}
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	var batchID string
	if *batchName != "" {
		b, err := db.GetBatch(*batchName)
		if err != nil {
			return err
		}
		batchID = b.ID
	}
	n, err := db.Cancel(taskID, batchID, *all)
	if err != nil {
		return err
	}
	fmt.Printf("canceled %d task(s); running tasks stop within a few seconds\n", n)
	return nil
}

// reset requeues tasks that are not done.
func reset(args []string) error {
	fs := flag.NewFlagSet("reset", flag.ExitOnError)
	failed := fs.Bool("failed", false, "also requeue failed tasks")
	all := fs.Bool("all", false, "requeue everything that is not done (running, failed, canceled)")
	fs.Parse(args)
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	n, err := db.Reset(*failed, *all)
	if err != nil {
		return err
	}
	fmt.Printf("requeued %d task(s)\n", n)
	return nil
}

// resolveWorkdir returns the absolute path to an existing directory.
func resolveWorkdir(dir string) (string, error) {
	if dir == "" {
		return os.Getwd()
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", abs)
	}
	return abs, nil
}

// summary handles the 'summary' subcommand for displaying overall queue statistics.
func summary(args []string) error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	statuses, err := db.Status()
	if err != nil {
		return err
	}
	if len(statuses) == 0 {
		fmt.Println("no batches found - add tasks with `forebay add` or `forebay batch`")
		return nil
	}
	var totalPending, totalRunning, totalDone, totalFailed, totalCanceled int
	for _, s := range statuses {
		totalPending += s.Pending
		totalRunning += s.Running
		totalDone += s.Done
		totalFailed += s.Failed
		totalCanceled += s.Canceled
	}
	fmt.Printf("Overall Queue Summary\n")
	fmt.Printf("====================\n")
	fmt.Printf("Pending: %d\n", totalPending)
	fmt.Printf("Running: %d\n", totalRunning)
	fmt.Printf("Done:    %d\n", totalDone)
	fmt.Printf("Failed:  %d\n", totalFailed)
	fmt.Printf("Canceled: %d\n", totalCanceled)
	return nil
}

// truncate limits s to at most n characters, adding "..." if truncated.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// multiFlag collects repeated string flags into a slice.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ", ") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}
