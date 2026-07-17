// Package mcpserver implements a stdio MCP server over the queue.
// Deliberately, it can only queue and inspect work — never execute it.
// An agent fans out hundreds of tasks; nothing runs until a human (or
// their cron entry) invokes `forebay run`. That gap is the review gate.
package mcpserver

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/andrewsinnovations/forebay/internal/store"
)

// serverVersion is the MCP protocol version reported by the server.
const serverVersion = "0.1.0"

// supportedProtocols lists the MCP protocol versions this server can handle.
var supportedProtocols = map[string]bool{
	"2024-11-05": true,
	"2025-03-26": true,
	"2025-06-18": true,
}

// request is a JSON-RPC request from the MCP client.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// response is a JSON-RPC response to the MCP client.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is a JSON-RPC error response.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve runs the MCP server reading from stdin and writing to stdout.
func Serve(db *store.DB) error {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	out := bufio.NewWriter(os.Stdout)
	enc := json.NewEncoder(out)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			fmt.Fprintf(os.Stderr, "forebay mcp: bad JSON-RPC frame: %v\n", err)
			continue
		}
		if req.ID == nil {
			continue
		}
		resp := response{JSONRPC: "2.0", ID: req.ID}
		result, err := dispatch(db, &req)
		if err != nil {
			resp.Error = &rpcError{Code: -32603, Message: err.Error()}
		} else {
			resp.Result = result
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
		if err := out.Flush(); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return err
	}
	return nil
}

// dispatch routes an MCP request to the appropriate handler.
func dispatch(db *store.DB, req *request) (any, error) {
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &params)
		proto := params.ProtocolVersion
		if !supportedProtocols[proto] {
			proto = "2025-06-18"
		}
		return map[string]any{
			"protocolVersion": proto,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{
				"name":    "forebay",
				"version": serverVersion,
			},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": toolDefs}, nil
	case "tools/call":
		return callTool(db, req.Params)
	default:
		return nil, fmt.Errorf("method not supported: %s", req.Method)
	}
}

// toolDefs defines the MCP tools exposed by this server.
var toolDefs = []map[string]any{
	{
		"name": "queue_tasks",
		"description": "Queue commands on a named batch. Each command is an argv array " +
			"(program + args, no shell — do not shell-quote). Tasks do NOT run until the " +
			"user invokes `forebay run`; this tool only queues. Batches are created on " +
			"first use and capture the current working directory and environment.",
		"inputSchema": map[string]any{
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
	},
	{
		"name":        "queue_status",
		"description": "Per-batch task counts (pending/running/done/failed/canceled).",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	},
	{
		"name":        "list_tasks",
		"description": "List tasks, optionally filtered by batch name and/or status (pending|running|done|failed|canceled).",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"batch":  map[string]any{"type": "string"},
				"status": map[string]any{"type": "string"},
				"limit":  map[string]any{"type": "integer", "default": 100},
			},
		},
	},
	{
		"name":        "cancel",
		"description": "Cancel a task by id, or every pending/running task in a batch by name.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{"type": "string"},
				"batch":   map[string]any{"type": "string"},
			},
		},
	},
}

// callTool invokes a tool with the given JSON-encoded arguments.
func callTool(db *store.DB, rawParams json.RawMessage) (any, error) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(rawParams, &params); err != nil {
		return nil, fmt.Errorf("bad tools/call params: %w", err)
	}
	result, err := runTool(db, params.Name, params.Arguments)
	if err != nil {
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": err.Error()}},
			"isError": true,
		}, nil
	}
	text, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(text)}},
	}, nil
}

// runTool executes the named tool with the provided arguments.
func runTool(db *store.DB, name string, args json.RawMessage) (any, error) {
	switch name {
	case "queue_tasks":
		var a struct {
			Batch    string     `json:"batch"`
			Commands [][]string `json:"commands"`
			Workdir  string     `json:"workdir"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("bad arguments: %w", err)
		}
		if len(a.Commands) == 0 {
			return nil, fmt.Errorf("commands must not be empty")
		}
		workdir := a.Workdir
		if workdir == "" {
			if wd, err := os.Getwd(); err == nil {
				workdir = wd
			}
		}
		batch, err := db.EnsureBatch(a.Batch, workdir, os.Environ())
		if err != nil {
			return nil, err
		}
		taskIDs := make([]string, 0, len(a.Commands))
		for _, argv := range a.Commands {
			id, err := db.AddTask(batch.ID, argv)
			if err != nil {
				return nil, fmt.Errorf("after queueing %d task(s): %w", len(taskIDs), err)
			}
			taskIDs = append(taskIDs, id)
		}
		return map[string]any{
			"batch_id":   batch.ID,
			"batch_name": batch.Name,
			"task_ids":   taskIDs,
			"note":       "Tasks are queued but will not run until the user executes `forebay run`.",
		}, nil

	case "queue_status":
		statuses, err := db.Status()
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(statuses))
		for _, s := range statuses {
			out = append(out, map[string]any{
				"batch_id": s.Batch.ID, "batch_name": s.Batch.Name,
				"pending": s.Pending, "running": s.Running, "done": s.Done,
				"failed": s.Failed, "canceled": s.Canceled,
			})
		}
		return map[string]any{"batches": out}, nil

	case "list_tasks":
		var a struct {
			Batch  string `json:"batch"`
			Status string `json:"status"`
			Limit  int    `json:"limit"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("bad arguments: %w", err)
		}
		batchID := ""
		if a.Batch != "" {
			b, err := db.GetBatch(a.Batch)
			if err != nil {
				return nil, err
			}
			batchID = b.ID
		}
		if a.Limit <= 0 {
			a.Limit = 100
		}
		tasks, err := db.ListTasks(batchID, a.Status, "", a.Limit)
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(tasks))
		for _, t := range tasks {
			m := map[string]any{
				"id": t.ID, "batch": t.BatchName, "argv": t.Argv, "status": t.Status,
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

	case "cancel":
		var a struct {
			TaskID string `json:"task_id"`
			Batch  string `json:"batch"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("bad arguments: %w", err)
		}
		batchID := ""
		if a.Batch != "" {
			b, err := db.GetBatch(a.Batch)
			if err != nil {
				return nil, err
			}
			batchID = b.ID
		}
		n, err := db.Cancel(a.TaskID, batchID, false)
		if err != nil {
			return nil, err
		}
		return map[string]any{"canceled": n}, nil

	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}
