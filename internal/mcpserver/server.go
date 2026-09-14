// Package mcpserver implements a stdio MCP server over the forebay queue.
//
// Deliberately, it can only queue and inspect work — never execute it. An
// agent fans out hundreds of tasks; nothing runs until a human (or their cron
// entry) invokes `forebay run`. That gap is the review gate.
package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/andrewsinnovations/forebay/internal/store"
)

// JSON-RPC 2.0 error codes used by this server.
const (
	errCodeInternal = -32603
	errCodeMethod   = -32601
)

// maxFrameBytes caps a single JSON-RPC frame read from the client.
const maxFrameBytes = 16 << 20

// ErrMethodUnsupported reports a JSON-RPC method this server does not
// implement. Callers can use errors.Is to distinguish an unknown method from a
// failure inside a known one.
var ErrMethodUnsupported = errors.New("mcpserver: method not supported")

// ErrToolUnknown reports a tools/call naming a tool this server does not
// expose. The set of tools is fixed by toolDefs.
var ErrToolUnknown = errors.New("mcpserver: unknown tool")

// protocolVersions lists the MCP protocol revisions this server can handle,
// newest first. A client asking for anything else is answered with
// defaultProtocol.
var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// defaultProtocol is the revision negotiated when the client requests a
// version this server does not know.
const defaultProtocol = "2025-06-18"

// Serve runs the MCP server over stdin and stdout until stdin reaches EOF or
// ctx is canceled. It writes one JSON-RPC response per request frame; frames it
// cannot parse are reported on stderr and skipped, since a malformed frame has
// no ID to answer. Frames without an ID are notifications and get no response.
func Serve(ctx context.Context, db *store.DB) error {
	s := &session{db: db}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(nil, maxFrameBytes)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		frame := scanner.Bytes()
		var req rpcRequest
		if err := json.Unmarshal(frame, &req); err != nil {
			fmt.Fprintf(os.Stderr, "forebay mcp: bad JSON-RPC frame: %v\n", err)
			continue
		}
		if len(req.ID) == 0 {
			continue // Notification: no response expected.
		}
		resp := rpcResponse{JSONRPC: req.JSONRPC, ID: req.ID}
		if result, err := s.handle(&req); err != nil {
			resp.Error = &rpcError{code: codeFor(err), message: err.Error()}
		} else {
			resp.Result = result
		}
		if err := encode(out, resp); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read stdin: %w", err)
	}
	return nil
}

// encode writes one response frame and flushes it so the client is never left
// waiting on buffered output.
func encode(out *bufio.Writer, resp rpcResponse) error {
	if err := json.NewEncoder(out).Encode(resp); err != nil {
		return fmt.Errorf("encode response: %w", err)
	}
	if err := out.Flush(); err != nil {
		return fmt.Errorf("write response: %w", err)
	}
	return nil
}

// codeFor maps an internal error to the JSON-RPC code the client should see.
func codeFor(err error) int {
	if errors.Is(err, ErrMethodUnsupported) {
		return errCodeMethod
	}
	return errCodeInternal
}

// rpcRequest is a JSON-RPC request from the MCP client.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcResponse is a JSON-RPC response to the MCP client.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is a JSON-RPC error object. It marshals in the wire layout required
// by JSON-RPC 2.0.
type rpcError struct {
	code    int
	message string
}

func (e *rpcError) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}{e.code, e.message})
}

// session holds the state needed to answer MCP requests against one database.
// A session is safe for simultaneous use by multiple goroutines only from a
// single Serve loop; Serve itself serializes requests.
type session struct{ db *store.DB }

// handle routes one request to its method implementation.
func (s *session) handle(req *rpcRequest) (any, error) {
	switch req.Method {
	case "initialize":
		return s.initialize(req.Params)
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": toolDefs()}, nil
	case "tools/call":
		return s.callTool(req.Params)
	default:
		return nil, fmt.Errorf("%w: %s", ErrMethodUnsupported, req.Method)
	}
}

// initialize replies to the MCP handshake, negotiating down to
// defaultProtocol when the client asks for a revision this server doesn't know.
func (s *session) initialize(params json.RawMessage) (any, error) {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	// A missing or malformed protocolVersion is tolerated: the spec expects
	// clients to send one, but answering with the default keeps older or
	// buggy clients working.
	_ = json.Unmarshal(params, &p)
	proto := p.ProtocolVersion
	if !supported(proto) {
		proto = defaultProtocol
	}
	return map[string]any{
		"protocolVersion": proto,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "forebay", "version": version},
	}, nil
}

// supported reports whether proto names a revision this server handles.
func supported(proto string) bool {
	for _, p := range protocolVersions {
		if p == proto {
			return true
		}
	}
	return false
}
