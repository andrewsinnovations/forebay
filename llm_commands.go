package main

import (
	"encoding/json"
	"errors"
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
		return "", nil, errors.New("--system and --system-file are mutually exclusive")
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
		return "", nil, errors.New("--schema and --schema-file are mutually exclusive")
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

// llmFlagNames are forebay's own flags. One inside a prompt usually means
// it was typed after the "--" separator and so was never applied.
var llmFlagNames = map[string]bool{
	"--schema": true, "--schema-file": true, "--system": true,
	"--system-file": true, "--model": true, "--batch": true,
	"--name": true, "--dir": true, "--glob": true, "--exclude": true,
	"--dry-run": true,
}

// warnPromptFlags notes a forebay flag sitting in the prompt text.
func warnPromptFlags(prompt []string) {
	for _, a := range prompt {
		name := a
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		if llmFlagNames[name] {
			fmt.Fprintf(os.Stderr, "warning: %s appears after \"--\", so it is part of the prompt "+
				"text rather than an applied flag; move it before the \"--\" if that was not intended\n", name)
			return
		}
	}
}

// describeSchema summarizes a resolved schema for queue-time feedback.
func describeSchema(schema json.RawMessage) string {
	if len(schema) == 0 {
		return "free-form text (no --schema/--schema-file)"
	}
	return "structured output (json_schema, strict)"
}

// requireConfig verifies the LLM configuration is present and valid.
func requireConfig(db *store.DB, modelOverride string) error {
	cfg, err := llm.LoadConfig(db.Home())
	if err != nil {
		return err
	}
	if cfg.Model == "" && modelOverride == "" {
		return fmt.Errorf("no model: set \"model\" in %s or pass --model", llm.ConfigPath(db.Home()))
	}
	return nil
}

// specJSON serializes an LLM spec to a JSON string.
func specJSON(model, system, user string, schema json.RawMessage) (string, error) {
	b, err := json.Marshal(llm.Spec{Model: model, System: system, User: user, Schema: schema})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// addLLM queues a single LLM task onto a batch.
func addLLM(args []string) error {
	flagArgs, prompt := splitArgs(args)
	fs := flag.NewFlagSet("add-llm", flag.ExitOnError)
	batchName := fs.String("batch", "default", "batch name to queue onto (created if missing)")
	dir := fs.String("dir", "", "working directory for the batch (default: current directory; only applies on batch creation)")
	lf := addLLMFlags(fs)
	fs.Parse(flagArgs)
	if len(prompt) == 0 {
		return errors.New("no user prompt given; usage: forebay add-llm [flags] -- USER PROMPT...")
	}
	warnPromptFlags(prompt)
	system, schema, err := lf.resolve()
	if err != nil {
		return err
	}
	db, err := store.Open()
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
	fmt.Printf("queued llm task %s on batch %q — %s\n", id, batch.Name, describeSchema(schema))
	return nil
}

// batchLLM queues one LLM task per file matching the glob patterns.
func batchLLM(args []string) error {
	flagArgs, template := splitArgs(args)
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
		return errors.New("at least one --glob is required")
	}
	if len(template) == 0 {
		return errors.New("no user prompt template given after --")
	}
	warnPromptFlags(template)
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
	db, err := store.Open()
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
	fmt.Printf("queued %d llm tasks on batch %q, %s — run them with: forebay run --batch %s\n",
		len(files), batch.Name, describeSchema(schema), batch.Name)
	return nil
}
