// forebay is a daemon-less task queue for scheduling and running commands.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrewsinnovations/forebay/internal/expand"
	"github.com/andrewsinnovations/forebay/internal/llm"
	"github.com/andrewsinnovations/forebay/internal/mcpserver"
	"github.com/andrewsinnovations/forebay/internal/runner"
	"github.com/andrewsinnovations/forebay/internal/store"
)

const usage = `forebay — queue commands now, run them when you choose

Usage:
  forebay add    [--batch NAME] [--dir DIR] -- CMD [ARGS...]
  forebay batch  [--name NAME] --glob PATTERN [--glob ...] [--dir ROOT]
                 [--exclude PATTERN ...] [--dry-run] -- CMD-TEMPLATE [ARGS...]
  forebay add-llm   [--batch NAME] [--system TEXT|--system-file F]
                    [--schema JSON|--schema-file F] [--model M] -- USER PROMPT...
  forebay batch-llm [--name NAME] --glob PATTERN [--glob ...] [--dir ROOT]
                    [--exclude PATTERN ...] [--system TEXT|--system-file F]
                    [--schema JSON|--schema-file F] [--model M] [--dry-run]
                    -- USER PROMPT TEMPLATE...
  forebay run    [-j N] [--batch NAME] [--watch] [--interval SECONDS]
  forebay status
  forebay list   [--batch NAME] [--status STATUS] [--limit N]
  forebay results [--batch NAME] [--status STATUS] [--limit N] [--json]
  forebay logs   TASK_ID
  forebay cancel [TASK_ID] [--batch NAME] [--all]
  forebay reset  [--failed] [--all]
  forebay clean  [--batch NAME] [--all]
  forebay mcp

Commands are argv arrays — forebay never invokes a shell. In batch and
batch-llm templates, use placeholders per matched file: {path}
{slashpath} {relpath} {name} {base} {dir}.

LLM tasks (add-llm, batch-llm) POST to the OpenAI-compatible API
configured in ~/.forebay/config.json and save each reply for
"forebay results".

Example:
  forebay batch --name jsdoc --glob "src/**/*.js" -- \
    claude -p "analyze '{relpath}' and add jsdoc comments to every function"
  forebay batch-llm --name summarize --glob "src/**/*.js" \
    --system "You are a code summarizer." -- "Summarize {relpath}."
  forebay run -j 4
  forebay results --batch summarize
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "add":
		err = cmdAdd(args)
	case "batch":
		err = cmdBatch(args)
	case "add-llm":
		err = cmdAddLLM(args)
	case "batch-llm":
		err = cmdBatchLLM(args)
	case "results":
		err = cmdResults(args)
	case "run":
		err = cmdRun(args)
	case "status":
		err = cmdStatus(args)
	case "list":
		err = cmdList(args)
	case "logs":
		err = cmdLogs(args)
	case "cancel":
		err = cmdCancel(args)
	case "reset":
		err = cmdReset(args)
	case "clean":
		err = cmdClean(args)
	case "mcp":
		err = cmdMCP()
	case "help", "-h", "--help":
		fmt.Print(usage)
	case "version", "--version":
		fmt.Println("forebay 0.1.0")
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "forebay %s: %v\n", cmd, err)
		os.Exit(1)
	}
}

// splitAtDashDash separates arguments before and after "--".
func splitAtDashDash(args []string) (flags, command []string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

// openDB opens the task database.
func openDB() (*store.DB, error) {
	return store.Open()
}

// cmdAdd handles the 'add' subcommand for queuing a single task.
func cmdAdd(args []string) error {
	flagArgs, command := splitAtDashDash(args)
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	batchName := fs.String("batch", "default", "batch name to queue onto (created if missing)")
	dir := fs.String("dir", "", "working directory for the batch (default: current directory; only applies on batch creation)")
	fs.Parse(flagArgs)
	if len(command) == 0 {
		return fmt.Errorf("no command given; usage: forebay add [--batch NAME] -- CMD [ARGS...]")
	}
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	workdir, err := resolveWorkdir(*dir)
	if err != nil {
		return err
	}
	batch, err := db.EnsureBatch(*batchName, workdir, os.Environ())
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

// cmdBatch handles the 'batch' subcommand for queuing tasks from glob patterns.
func cmdBatch(args []string) error {
	flagArgs, template := splitAtDashDash(args)
	fs := flag.NewFlagSet("batch", flag.ExitOnError)
	name := fs.String("name", "", "batch name (default: batch-<id>)")
	dir := fs.String("dir", "", "root directory for glob expansion and task execution (default: current directory)")
	dryRun := fs.Bool("dry-run", false, "print the expanded tasks without queueing them")
	var globs, excludes multiFlag
	fs.Var(&globs, "glob", "glob pattern relative to --dir, e.g. \"src/**/*.js\" (repeatable)")
	fs.Var(&excludes, "exclude", "glob pattern to skip (repeatable; default: **/node_modules/**, **/.git/**)")
	fs.Parse(flagArgs)
	if len(globs) == 0 {
		return fmt.Errorf("at least one --glob is required")
	}
	if len(template) == 0 {
		return fmt.Errorf("no command template given after --")
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
	db, err := openDB()
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
	fmt.Printf("queued %d tasks on batch %q — run them with: forebay run --batch %s\n",
		len(files), batch.Name, batch.Name)
	return nil
}

// cmdRun handles the 'run' subcommand for executing queued tasks.
func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	workers := fs.Int("j", 1, "number of tasks to run in parallel")
	batchName := fs.String("batch", "", "only run tasks from this batch")
	watch := fs.Bool("watch", false, "keep polling for new tasks after the queue drains")
	interval := fs.Int("interval", 5, "poll interval in seconds for --watch")
	fs.Parse(args)
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	// In watch mode, the batch may not exist yet.
	if *batchName != "" && !*watch {
		if _, err := db.GetBatch(*batchName); err != nil {
			return err
		}
	}
	return runner.Run(db, runner.Options{
		Workers:  *workers,
		Batch:    *batchName,
		Watch:    *watch,
		Interval: time.Duration(*interval) * time.Second,
	})
}

// cmdStatus handles the 'status' subcommand for displaying batch summaries.
func cmdStatus(args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	statuses, err := db.Status()
	if err != nil {
		return err
	}
	if len(statuses) == 0 {
		fmt.Println("queue is empty — add tasks with `forebay add` or `forebay batch`")
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

// cmdList handles the 'list' subcommand for displaying tasks.
func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	batchName := fs.String("batch", "", "filter by batch name")
	status := fs.String("status", "", "filter by status (pending|running|done|failed|canceled)")
	limit := fs.Int("limit", 100, "maximum tasks to show")
	fs.Parse(args)
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	batchID := ""
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
		fmt.Printf("%-8s %-16s %-9s %-5s %s\n",
			t.ID, truncate(t.BatchName, 16), t.Status, exit,
			truncate(describeTask(t), 80))
	}
	return nil
}

// describeTask renders a task's work for one-line displays.
func describeTask(t store.Task) string {
	if t.Kind == store.KindLLM {
		spec, err := llm.ParseSpec(t.Payload)
		if err != nil {
			return "llm: (corrupt payload)"
		}
		return "llm: " + spec.User
	}
	return strings.Join(t.Argv, " ")
}

// cmdLogs handles the 'logs' subcommand for displaying task output.
func cmdLogs(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: forebay logs TASK_ID")
	}
	db, err := openDB()
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
		return err
	}
	os.Stdout.Write(data)
	fmt.Fprintf(os.Stderr, "\n(log file: %s)\n", task.LogPath)
	return nil
}

// cmdCancel handles the 'cancel' subcommand for stopping tasks.
func cmdCancel(args []string) error {
	fs := flag.NewFlagSet("cancel", flag.ExitOnError)
	batchName := fs.String("batch", "", "cancel every pending/running task in this batch")
	all := fs.Bool("all", false, "cancel every pending/running task in the queue")
	fs.Parse(args)
	taskID := ""
	if fs.NArg() > 0 {
		taskID = fs.Arg(0)
	}
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	batchID := ""
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

// cmdReset handles the 'reset' subcommand for requeuing tasks.
func cmdReset(args []string) error {
	fs := flag.NewFlagSet("reset", flag.ExitOnError)
	failed := fs.Bool("failed", false, "also requeue failed tasks")
	all := fs.Bool("all", false, "requeue everything that is not done (running, failed, canceled)")
	fs.Parse(args)
	db, err := openDB()
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

// cmdMCP handles the 'mcp' subcommand for starting the MCP server.
func cmdMCP() error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	return mcpserver.Serve(db)
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

// truncate limits s to at most n characters, adding "..." if truncated.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// multiFlag collects repeated string flags.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ", ") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }
