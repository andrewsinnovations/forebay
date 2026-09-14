package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/andrewsinnovations/forebay/internal/llm"
	"github.com/andrewsinnovations/forebay/internal/store"
)

// resultRow is the JSON shape emitted by `forebay results --json`.
type resultRow struct {
	TaskID     string   `json:"task_id"`
	Batch      string   `json:"batch"`
	Kind       string   `json:"kind"`
	Status     string   `json:"status"`
	Command    []string `json:"command,omitempty"`
	UserPrompt string   `json:"user_prompt,omitempty"`
	ExitCode   *int64   `json:"exit_code,omitempty"`
	StartedAt  string   `json:"started_at,omitempty"`
	FinishedAt string   `json:"finished_at,omitempty"`
	Result     string   `json:"result,omitempty"`
	Error      string   `json:"error,omitempty"`
	LogPath    string   `json:"log_path,omitempty"`
}

// results prints the saved output of tasks matching the given filters.
func results(args []string) error {
	fs := flag.NewFlagSet("results", flag.ExitOnError)
	batchName := fs.String("batch", "", "filter by batch name")
	status := fs.String("status", "", "filter by status (pending|running|done|failed|canceled)")
	kind := fs.String("kind", "", "filter by task kind (exec|llm; default: both)")
	contains := fs.String("contains", "", "only results containing this substring (case-insensitive)")
	limit := fs.Int("limit", 0, "maximum tasks to show (default: no limit)")
	asJSON := fs.Bool("json", false, "output as a JSON array")
	// Parse in a loop so flags still apply after the positional TASK_ID;
	// flag.Parse otherwise stops at the first non-flag argument.
	var positional []string
	for rest := args; ; {
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(positional) > 1 {
		return fmt.Errorf("at most one TASK_ID may be given, got %d", len(positional))
	}
	if *kind != "" && *kind != store.KindExec && *kind != store.KindLLM {
		return errors.New("--kind must be \"exec\" or \"llm\"")
	}
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	filter := store.ResultFilter{
		Status: *status, Kind: *kind, Contains: *contains, Limit: *limit,
	}
	if len(positional) == 1 {
		filter.TaskID = positional[0]
	}
	if *batchName != "" {
		b, err := db.GetBatch(*batchName)
		if err != nil {
			return err
		}
		filter.BatchID = b.ID
	}
	tasks, err := db.Results(filter)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		fmt.Println("no matching results — queue work with `forebay add`/`batch`/`add-llm`, then `forebay run`")
		return nil
	}
	if *asJSON {
		out := make([]resultRow, 0, len(tasks))
		for _, t := range tasks {
			out = append(out, toResultRow(t))
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	for _, t := range tasks {
		printResult(t)
	}
	return nil
}

// toResultRow projects a task into its JSON representation.
func toResultRow(t store.Task) resultRow {
	r := resultRow{
		TaskID: t.ID, Batch: t.BatchName, Kind: t.Kind, Status: t.Status,
		ExitCode: t.ExitCode, StartedAt: t.StartedAt, FinishedAt: t.FinishedAt,
		Result: t.Result, Error: t.Error, LogPath: t.LogPath,
	}
	if t.Kind == store.KindLLM {
		spec, _ := llm.ParseSpec(t.Payload)
		r.UserPrompt = spec.User
	} else {
		r.Command = t.Argv
	}
	return r
}

// printResult renders one task's saved output for a terminal.
func printResult(t store.Task) {
	header := fmt.Sprintf("── %s  %s  %s  %s", t.ID, t.BatchName, t.Kind, t.Status)
	if t.ExitCode != nil {
		header += fmt.Sprintf("  exit %d", *t.ExitCode)
	}
	fmt.Println(header)
	if t.Kind == store.KindLLM {
		spec, _ := llm.ParseSpec(t.Payload)
		fmt.Printf("   prompt: %s\n", truncate(spec.User, 100))
	} else {
		fmt.Printf("   command: %s\n", truncate(strings.Join(t.Argv, " "), 100))
	}
	if t.Result != "" {
		fmt.Println(strings.TrimRight(t.Result, "\n"))
	}
	if t.Error != "" {
		fmt.Printf("error: %s\n", t.Error)
	}
	if t.Result == "" && t.Error == "" {
		if t.Status == store.StatusDone {
			fmt.Println("(no output)")
		} else {
			fmt.Println("(no output yet)")
		}
	}
	fmt.Println()
}
