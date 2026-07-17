# forebay

A daemon-less command queue. Queue commands now — by hand, from a glob
template, or from an AI agent over MCP — and run them when you choose,
sequentially or in parallel.

There is no background process. State lives in a SQLite database under
`~/.forebay/`, and every forebay invocation (CLI, MCP server, runner)
coordinates through it. Nothing executes until you type `forebay run`
or your scheduler does it for you.

## Install

```
go install github.com/andrewsinnovations/forebay@latest
```

or build from source: `go build -o forebay .`

## The core loop

```sh
# Queue one task per *.js file, from a command template
forebay batch --name jsdoc --glob "src/**/*.js" -- \
  claude -p "analyze '{relpath}' and add jsdoc comments to every function"

# See what's queued
forebay status

# Drain the queue, four tasks at a time (omit -j for sequential)
forebay run -j 4
```

Commands are **argv arrays, never shell strings** — forebay execs the
program directly. That means no quoting differences between Linux,
macOS, and Windows, and no shell injection when an LLM composes the
command. Everything after `--` is the command.

## Commands

| Command | What it does |
| --- | --- |
| `forebay add [--batch NAME] [--dir DIR] -- CMD [ARGS...]` | Queue a single command. |
| `forebay batch --glob PAT [--glob ...] [--name N] [--dir ROOT] [--exclude PAT] [--dry-run] -- TEMPLATE...` | Expand globs into one task per matched file. |
| `forebay add-llm [--system TEXT] [--schema-file F] [--model M] -- PROMPT...` | Queue one direct LLM API call. |
| `forebay batch-llm --glob PAT ... [--system TEXT] [--schema-file F] -- PROMPT TEMPLATE...` | One LLM call per matched file. |
| `forebay results [--batch N] [--status S] [--json]` | Print saved LLM replies. |
| `forebay run [-j N] [--batch NAME] [--watch] [--interval SECS]` | Claim and execute pending tasks. `--watch` keeps polling after the queue drains. |
| `forebay status` | Per-batch counts. |
| `forebay list [--batch N] [--status S] [--limit N]` | Task detail. |
| `forebay logs TASK_ID` | Print a task's captured output. |
| `forebay cancel [TASK_ID] [--batch N] [--all]` | Cancel pending tasks immediately; running tasks are killed (whole process tree) within a few seconds. |
| `forebay reset [--failed] [--all]` | Requeue interrupted (and optionally failed) tasks. |
| `forebay clean [--batch N] [--all]` | Delete finished task history and its log files (`--all` also drops pending; running tasks always survive). |
| `forebay mcp` | Run the stdio MCP server. |

### Template placeholders

In `forebay batch` templates, each matched file substitutes:

| Placeholder | Example (root `C:\proj`) |
| --- | --- |
| `{path}` | `C:\proj\src\auth.js` (absolute, native separators) |
| `{slashpath}` | `C:/proj/src/auth.js` (absolute, forward slashes — safe inside prompts and JSON on Windows) |
| `{relpath}` | `src/auth.js` |
| `{name}` / `{base}` | `auth.js` / `auth` |
| `{dir}` | `C:\proj\src` |

`**/node_modules/**` and `**/.git/**` are excluded by default; pass your
own `--exclude` to override. Use `--dry-run` to preview the expansion
before queueing.

## LLM tasks: direct API calls without an agent

For bulk work that doesn't need a full coding agent — classification,
extraction, summarization — queue direct calls to any OpenAI-compatible
chat completions API. Configure the endpoint once in
`~/.forebay/config.json`:

```json
{
  "base_url": "https://api.openai.com/v1",
  "api_key": "sk-...",
  "model": "gpt-4o-mini"
}
```

`api_key` is optional (local endpoints like Ollama or llama.cpp need
none) and is stored in plaintext, so keep the file private.
`timeout_seconds` (default 300) and per-task `--model` overrides are
also supported.

```sh
# One call
forebay add-llm --system "You are terse." -- "Summarize the plot of Hamlet."

# One call per file, with structured JSON output
forebay batch-llm --name jssum --glob "src/**/*.js" \
  --schema-file summary-schema.json \
  -- "Summarize the purpose of {relpath}. File contents are at {path}."

forebay run -j 4

# Read the replies
forebay results --batch jssum          # human-readable
forebay results --batch jssum --json   # [{task_id, batch, status, user_prompt, result, error}]
```

`--system`/`--system-file` set the system prompt, `--schema`/
`--schema-file` attach a JSON schema (sent as OpenAI
`response_format: json_schema` with `strict: true`), and the user
prompt comes after `--`. In `batch-llm`, both prompts take the same
per-file placeholders as `forebay batch`. Each reply is saved on the
task (shown by `forebay results`); the raw API response goes to the
task log for debugging. Failed calls record the HTTP error — retry them
with `forebay reset --failed && forebay run`.

## MCP: letting an agent queue work

Register the server with Claude Code:

```sh
claude mcp add forebay -- forebay mcp
```

The agent gets four tools: `queue_tasks`, `queue_status`, `list_tasks`,
and `cancel`. Deliberately, there is **no execute tool** — an agent can
fan out 200 tasks, but nothing runs until you review the queue
(`forebay status`, `forebay list`) and type `forebay run`. The gap
between queueing and execution is a human review gate on LLM-composed
commands, and it's the point of the design. If you want tasks picked up
as they're queued, leave `forebay run --watch` running in a terminal
you can see.

## Scheduling

forebay has no scheduler; your OS already has one. Point it at
`forebay run`:

**cron (Linux):**
```cron
0 2 * * * /home/you/go/bin/forebay run -j 4 >> /home/you/.forebay/cron.log 2>&1
```

**launchd (macOS):** prefer a LaunchAgent over cron (cron needs Full
Disk Access on modern macOS). A minimal plist runs
`forebay run -j 4` on a `StartCalendarInterval`.

**Task Scheduler (Windows):**
```powershell
schtasks /Create /TN forebay /SC DAILY /ST 02:00 /TR "C:\path\to\forebay.exe run -j 4"
```

Two design choices make scheduled runs safe and reliable:

- **Overlap is harmless.** Tasks are claimed with a single atomic
  `UPDATE ... RETURNING`, so a cron-fired runner colliding with a manual
  one just means more workers on the same queue — never double
  execution.
- **Batches capture their environment.** `forebay add` / `forebay
  batch` / the MCP server snapshot the working directory and environment
  (PATH, credentials) at queue time, and the runner executes tasks under
  that snapshot. A cron daemon's near-empty environment won't break
  `claude` resolution or auth. Note this means the environment is stored
  in plaintext in `~/.forebay/forebay.db`, which is created `0700`.

## Behavior details

- **Crash recovery.** Runners heartbeat their claimed tasks every 5s.
  On startup, `forebay run` requeues any `running` task whose heartbeat
  is over 60s old (a previous runner crashed or the machine slept).
  Ctrl-C kills running process trees and requeues those tasks.
- **Cancellation kills the whole tree.** Process groups + SIGTERM/SIGKILL
  on Unix; a Job Object with `TerminateJobObject` on Windows, so an
  agent's grandchildren can't survive as orphans. Graceful shutdown is
  best-effort on Windows (CTRL_BREAK, then hard kill after 5s).
- **Logs** are written to `~/.forebay/logs/<batch-id>/<task-id>.log`.
  With `-j 1` output also streams to your terminal.
- **Failure** is exit code ≠ 0 or a spawn error; retry with
  `forebay reset --failed` then `forebay run`.
- Set `FOREBAY_HOME` to relocate all state (useful for tests).

## Windows notes

- Spawning many processes in a burst can attract Windows Defender's
  attention; consider an exclusion for your `forebay.exe` and
  `%USERPROFILE%\.forebay` if you see slow starts.
- Globs are matched case-insensitively deduped, since the filesystem is
  case-insensitive.
