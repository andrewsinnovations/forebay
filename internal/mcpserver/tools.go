package mcpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/andrewsinnovations/forebay/internal/store"
)

// version is the server version reported in the MCP handshake. It tracks the
// tool surface, not the forebay CLI release.
const version = "0.3.0"

// defaultTaskLimit is how many tasks list_tasks returns when the caller omits
// a limit.
const defaultTaskLimit = 100

// A tool is one MCP tool exposed by this server: its wire schema plus the
// implementation that runs it. Keeping the definition and handler together
// means a new tool cannot be advertised without being callable.
type tool struct {
	name        string
	description string
	schema      map[string]any
	call        func(db *store.DB, args json.RawMessage) (any, error)
}

// tools lists every tool this server exposes, in the order tools/list reports
// them.
var tools = []tool{
	{
		name: "queue_tasks",
		description: "Queue commands on a named batch. Each command is an argv array " +
			"(program + args, no shell — do not shell-quote). Tasks do NOT run until the " +
			"user invokes `forebay run`; this tool only queues. Batches are created on " +
			"first use and capture the current working directory and environment.",
		schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"batch": map[string]any{
					"type":        "string",
					"description": "Batch name to queue onto (created if missing).",
				},
				"commands": map[string]any{
					"type":        "array",
					"description": "Commands to queue, each an argv array, e.g. [[\"claude\", \"-p\", \"analyze 'src/a.js' and add jsdoc comments to every function\"], ...]",
					"items": map[string]any{
						"type":     "array",
						"items":    map[string]any{"type": "string"},
						"minItems": 1,
					},
					"minItems": 1,
				},
				"workdir": map[string]any{
					"type":        "string",
					"description": "Working directory for the batch (only applies when the batch is being created; defaults to this server's cwd).",
				},
			},
			"required": []string{"batch", "commands"},
		},
		call: queueTasks,
	},
	{
		name:        "queue_status",
		description: "Per-batch task counts (pending/running/done/failed/canceled).",
		schema:      map[string]any{"type": "object", "properties": map[string]any{}},
		call:        queueStatus,
	},
	{
		name:        "list_tasks",
		description: "List tasks, optionally filtered by batch name and/or status (pending|running|done|failed|canceled).",
		schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"batch":  map[string]any{"type": "string"},
				"status": map[string]any{"type": "string"},
				"limit":  map[string]any{"type": "integer", "default": defaultTaskLimit},
			},
		},
		call: listTasks,
	},
	{
		name:        "cancel",
		description: "Cancel a task by id, or every pending/running task in a batch by name.",
		schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{"type": "string"},
				"batch":   map[string]any{"type": "string"},
			},
		},
		call: cancelTasks,
	},
}

// toolDefs renders the tool schemas in the shape tools/list expects.
func toolDefs() []map[string]any {
	defs := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		defs = append(defs, map[string]any{
			"name":        t.name,
			"description": t.description,
			"inputSchema": t.schema,
		})
	}
	return defs
}

// lookup finds a tool by name. If name is not a known tool, the returned error
// wraps ErrToolUnknown.
func lookup(name string) (tool, error) {
	for _, t := range tools {
		if t.name == name {
			return t, nil
		}
	}
	return tool{}, fmt.Errorf("%w: %s", ErrToolUnknown, name)
}

// callTool runs the tool named in a tools/call request. Tool failures are
// reported as MCP tool errors (an isError result) rather than JSON-RPC errors,
// so the client can show the message to the model that asked for the call.
func (s *session) callTool(rawParams json.RawMessage) (any, error) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(rawParams, &params); err != nil {
		return nil, fmt.Errorf("bad tools/call params: %w", err)
	}
	t, err := lookup(params.Name)
	if err != nil {
		return nil, err
	}
	result, err := t.call(s.db, params.Arguments)
	if err != nil {
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": err.Error()}},
			"isError": true,
		}, nil
	}
	text, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("render %s result: %w", t.name, err)
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(text)}},
	}, nil
}

// decodeArgs unmarshals a tool's arguments into dst, reporting bad input as a
// tool error the client can read.
func decodeArgs(args json.RawMessage, dst any) error {
	if err := json.Unmarshal(args, dst); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// batchID resolves a batch name or ID to its ID, or "" when name is empty.
func batchID(db *store.DB, name string) (string, error) {
	if name == "" {
		return "", nil
	}
	b, err := db.GetBatch(name)
	if err != nil {
		return "", err
	}
	return b.ID, nil
}

// queueTasks queues argv arrays onto a batch and returns their task IDs. The
// tasks are not executed; see the package comment for why.
func queueTasks(db *store.DB, args json.RawMessage) (any, error) {
	var a struct {
		Batch    string     `json:"batch"`
		Commands [][]string `json:"commands"`
		Workdir  string     `json:"workdir"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	if len(a.Commands) == 0 {
		return nil, errors.New("commands must not be empty")
	}
	workdir := a.Workdir
	if workdir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve working directory: %w", err)
		}
		workdir = wd
	}
	batch, err := db.EnsureBatch(a.Batch, workdir, os.Environ())
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(a.Commands))
	for i, argv := range a.Commands {
		id, err := db.AddTask(batch.ID, argv)
		if err != nil {
			return nil, fmt.Errorf("command %d of %d: %w", i+1, len(a.Commands), err)
		}
		ids = append(ids, id)
	}
	return map[string]any{
		"batch_id":   batch.ID,
		"batch_name": batch.Name,
		"task_ids":   ids,
		"note":       "Tasks are queued but will not run until the user executes `forebay run`.",
	}, nil
}

// queueStatus reports per-batch task counts.
func queueStatus(db *store.DB, _ json.RawMessage) (any, error) {
	statuses, err := db.Status()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(statuses))
	for _, s := range statuses {
		out = append(out, map[string]any{
			"batch_id":   s.Batch.ID,
			"batch_name": s.Batch.Name,
			"pending":    s.Pending,
			"running":    s.Running,
			"done":       s.Done,
			"failed":     s.Failed,
			"canceled":   s.Canceled,
		})
	}
	return map[string]any{"batches": out}, nil
}

// listTasks returns tasks matching the requested batch and status filters.
func listTasks(db *store.DB, args json.RawMessage) (any, error) {
	var a struct {
		Batch  string `json:"batch"`
		Status string `json:"status"`
		Limit  int    `json:"limit"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	id, err := batchID(db, a.Batch)
	if err != nil {
		return nil, err
	}
	if a.Limit <= 0 {
		a.Limit = defaultTaskLimit
	}
	tasks, err := db.ListTasks(id, a.Status, "", a.Limit)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		m := map[string]any{
			"id":     t.ID,
			"batch":  t.BatchName,
			"argv":   t.Argv,
			"status": t.Status,
		}
		if t.ExitCode != nil {
			m["exit_code"] = *t.ExitCode
		}
		if t.Error != "" {
			m["error"] = t.Error
		}
		out = append(out, m)
	}
	return map[string]any{"tasks": out}, nil
}

// cancelTasks cancels one task by ID, or every pending and running task in a
// batch. At least one selector must be given.
func cancelTasks(db *store.DB, args json.RawMessage) (any, error) {
	var a struct {
		TaskID string `json:"task_id"`
		Batch  string `json:"batch"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	id, err := batchID(db, a.Batch)
	if err != nil {
		return nil, err
	}
	n, err := db.Cancel(a.TaskID, id, false)
	if err != nil {
		return nil, err
	}
	return map[string]any{"canceled": n}, nil
}
