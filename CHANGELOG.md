# Changelog

## 0.3.0

- Added `summary` command for overall queue statistics.
- Added `--command` flag to `add` for specifying commands as a single string.
- `splitArgs` now splits at the *last* `--`, supporting multiple `--` in templates.
- Introduced structured MCP tools via `internal/mcpserver/tools.go` (`queue_tasks`, `queue_status`, `list_tasks`, `cancel`).
- Pass `context.Context` through `runner.Run` and `mcpserver.Serve` for graceful cancellation.
- Refactored error handling throughout: wrapped errors, sentinel errors in `store`, and clearer failure messages.
- Improved runner stability: better process tree signaling, dedicated log file creation helper, and explicit requeue on interruption.
- Aligned MCP server version with CLI release version.

## 0.2.0

- Added `clean` command for deleting finished tasks and pruning empty batches.
- Added `results` command for querying saved task output.
- Added LLM task support: `add-llm` and `batch-llm` for queuing OpenAI-compatible API calls.
- Added `mcp` command to expose forebay tools over the Model Context Protocol.
- All task runs (exec and LLM) now save output for later querying.
- Added `--watch` mode to `run` for continuous queue polling.

## 0.1.0

- Initial release.
- Core queue commands: `add`, `batch`, `run`, `status`, `list`, `logs`, `cancel`, `reset`.
- SQLite-backed task store with WAL mode.
