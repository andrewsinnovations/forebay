package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/andrewsinnovations/forebay/internal/expand"
	"github.com/andrewsinnovations/forebay/internal/llm"
	"github.com/andrewsinnovations/forebay/internal/store"
)

// llmFlags holds configuration flags for LLM task commands.
type llmFlags struct {
	system     *string
	systemFile *string
	schema     *string
	schemaFile *string
	model      *string
}

// addLLMFlags registers LLM-related flags with the provided FlagSet.
func addLLMFlags(fs *flag.FlagSet) llmFlags {
	return llmFlags{
		system:     fs.String("system", "", "system prompt text"),
		systemFile: fs.String("system-file", "", "read the system prompt from a file"),
		schema:     fs.String("schema", "", "JSON schema for structured output, inline"),
		schemaFile: fs.String("schema-file", "", "read the JSON schema from a file"),
		model:      fs.String("model", "", "model override (default: model from config.json)"),
	}
}

// resolve validates the flag combination and returns the system prompt and schema.
func (f llmFlags) resolve() (system string, schema json.RawMessage, err error) {
	if *f.system != "" && *f.systemFile != "" {
		return "", nil, fmt.Errorf("--system and --system-file are mutually exclusive")
	}
	system = *f.system
	if *f.systemFile != "" {
		data, err := os.ReadFile(*f.systemFile)
		if err != nil {
			return "", nil, err
		}
		system = string(llm.StripBOM(data))
	}
	if *f.schema != "" && *f.schemaFile != "" {
		return "", nil, fmt.Errorf("--schema and --schema-file are mutually exclusive")
	}
	raw := *f.schema
	if *f.schemaFile != "" {
		data, err := os.ReadFile(*f.schemaFile)
		if err != nil {
			return "", nil, err
		}
		raw = string(llm.StripBOM(data))
	}
	if raw != "" {
		var v map[string]any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return "", nil, fmt.Errorf("schema is not a valid JSON object: %w", err)
		}
		schema = json.RawMessage(raw)
	}
	return system, schema, nil
}

// requireConfig verifies the LLM configuration is present and valid.
func requireConfig(db *store.DB, modelOverride string) error {
	cfg, err := llm.LoadConfig(db.HomeDir())
	if err != nil {
		return err
	}
	if cfg.Model == "" && modelOverride == "" {
		return fmt.Errorf("no model: set \"model\" in %s or pass --model", llm.ConfigPath(db.HomeDir()))
	}
	return nil
}

// specJSON serializes an LLM spec to JSON string.
func specJSON(model, system, user string, schema json.RawMessage) (string, error) {
	b, err := json.Marshal(llm.Spec{Model: model, System: system, User: user, Schema: schema})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// cmdAddLLM handles the 'add-llm' subcommand.
func cmdAddLLM(args []string) error {
	flagArgs, prompt := splitAtDashDash(args)
	fs := flag.NewFlagSet("add-llm", flag.ExitOnError)
	batchName := fs.String("batch", "default", "batch name to queue onto (created if missing)")
	dir := fs.String("dir", "", "working directory for the batch (default: current directory; only applies on batch creation)")
	lf := addLLMFlags(fs)
	fs.Parse(flagArgs)
	if len(prompt) == 0 {
		return fmt.Errorf("no user prompt given; usage: forebay add-llm [flags] -- USER PROMPT...")
	}
	system, schema, err := lf.resolve()
	if err != nil {
		return err
	}
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := requireConfig(db, *lf.model); err != nil {
		return err
	}
	workdir, err := resolveWorkdir(*dir)
	if err != nil {
		return err
	}
	batch, err := db.EnsureBatch(*batchName, workdir, os.Environ())
	if err != nil {
		return err
	}
	payload, err := specJSON(*lf.model, system, strings.Join(prompt, " "), schema)
	if err != nil {
		return err
	}
	id, err := db.AddLLMTask(batch.ID, payload)
	if err != nil {
		return err
	}
	fmt.Printf("queued llm task %s on batch %q\n", id, batch.Name)
	return nil
}

// cmdBatchLLM handles the 'batch-llm' subcommand for queueing LLM tasks over files matching glob patterns.
func cmdBatchLLM(args []string) error {
	flagArgs, template := splitAtDashDash(args)
	fs := flag.NewFlagSet("batch-llm", flag.ExitOnError)
	name := fs.String("name", "", "batch name (default: batch-<id>)")
	dir := fs.String("dir", "", "root directory for glob expansion (default: current directory)")
	dryRun := fs.Bool("dry-run", false, "print the expanded prompts without queueing them")
	var globs, excludes multiFlag
	fs.Var(&globs, "glob", "glob pattern relative to --dir, e.g. \"src/**/*.js\" (repeatable)")
	fs.Var(&excludes, "exclude", "glob pattern to skip (repeatable; default: **/node_modules/**, **/.git/**)")
	lf := addLLMFlags(fs)
	fs.Parse(flagArgs)
	if len(globs) == 0 {
		return fmt.Errorf("at least one --glob is required")
	}
	if len(template) == 0 {
		return fmt.Errorf("no user prompt template given after --")
	}
	system, schema, err := lf.resolve()
	if err != nil {
		return err
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
	// Placeholder expansion for per-file prompts.
	renderPrompts := func(f string) (user, sys string) {
		return strings.Join(expand.Render(template, root, f), " "),
			strings.Join(expand.Render([]string{system}, root, f), " ")
	}
	if *dryRun {
		for _, f := range files {
			user, _ := renderPrompts(f)
			fmt.Printf("%s\n", user)
		}
		fmt.Printf("(%d llm tasks; dry run, nothing queued)\n", len(files))
		return nil
	}
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := requireConfig(db, *lf.model); err != nil {
		return err
	}
	batchName := *name
	if batchName == "" {
		batchName = "batch-" + store.NewID()
	}
	batch, err := db.EnsureBatch(batchName, root, os.Environ())
	if err != nil {
		return err
	}
	for _, f := range files {
		user, sys := renderPrompts(f)
		payload, err := specJSON(*lf.model, sys, user, schema)
		if err != nil {
			return err
		}
		if _, err := db.AddLLMTask(batch.ID, payload); err != nil {
			return err
		}
	}
	fmt.Printf("queued %d llm tasks on batch %q — run them with: forebay run --batch %s\n",
		len(files), batch.Name, batch.Name)
	return nil
}

// cmdResults handles the 'results' subcommand for LLM tasks.
func cmdResults(args []string) error {
	fs := flag.NewFlagSet("results", flag.ExitOnError)
	batchName := fs.String("batch", "", "filter by batch name")
	status := fs.String("status", "", "filter by status (default: all)")
	limit := fs.Int("limit", 0, "maximum tasks to show (default: no limit)")
	asJSON := fs.Bool("json", false, "output as a JSON array")
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
	tasks, err := db.ListTasks(batchID, *status, store.KindLLM, *limit)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		fmt.Println("no llm tasks found — queue some with `forebay add-llm` or `forebay batch-llm`")
		return nil
	}
	if *asJSON {
		type row struct {
			TaskID string `json:"task_id"`
			Batch  string `json:"batch"`
			Status string `json:"status"`
			User   string `json:"user_prompt"`
			Result string `json:"result,omitempty"`
			Error  string `json:"error,omitempty"`
		}
		out := make([]row, 0, len(tasks))
		for _, t := range tasks {
			spec, _ := llm.ParseSpec(t.Payload)
			out = append(out, row{
				TaskID: t.ID, Batch: t.BatchName, Status: t.Status,
				User: spec.User, Result: t.Result, Error: t.Error,
			})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	for _, t := range tasks {
		spec, _ := llm.ParseSpec(t.Payload)
		fmt.Printf("── %s  %s  %s\n", t.ID, t.BatchName, t.Status)
		fmt.Printf("   prompt: %s\n", truncate(spec.User, 100))
		switch {
		case t.Status == store.StatusDone:
			fmt.Println(t.Result)
		case t.Error != "":
			fmt.Printf("error: %s\n", t.Error)
		default:
			fmt.Println("(no output yet)")
		}
		fmt.Println()
	}
	return nil
}
