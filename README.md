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
| `forebay results [TASK_ID] [--batch N] [--status S] [--kind exec\|llm] [--contains TEXT] [--limit N] [--json]` | Query saved task output. |
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

## Results: every task's output, queryable

Each task stores its complete output in the database when it settles —
captured stdout+stderr for commands, the model reply for LLM tasks — so
the queue doubles as a result set you can query after the fact. Canceled
tasks keep whatever they produced before they were killed.

```sh
forebay results                          # everything, oldest first
forebay results --batch jsdoc            # one batch
forebay results --status failed          # only failures
forebay results --kind exec              # commands (or --kind llm)
forebay results --contains "TODO"        # output matching a substring
forebay results a1b2c3d4                 # one task by id
forebay results --batch jsdoc --json     # machine-readable
```

`--json` emits one object per task: `task_id`, `batch`, `kind`,
`status`, `command` or `user_prompt`, `exit_code`, `started_at`,
`finished_at`, `result`, `error`, `log_path` — pipe it to `jq` for
anything the flags don't cover, or query `~/.forebay/forebay.db`
directly (`SELECT id, result FROM tasks WHERE ...`).

Output is stored whole, with no truncation. If a task can emit more
than you want in SQLite, set `FOREBAY_MAX_RESULT_BYTES` when running:
the stored result keeps the head and tail up to that many bytes with a
marker naming the log file in between, while the log on disk stays
complete. `forebay logs TASK_ID` always prints the untruncated log, and
`forebay clean` drops results and logs together.

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

`extra_body` merges arbitrary fields into every request, for parameters
forebay doesn't model itself — sampling settings, or whatever the
endpoint you point at happens to accept:

```json
{
  "base_url": "https://api.openai.com/v1",
  "model": "gpt-4o-mini",
  "extra_body": { "temperature": 0, "seed": 42 }
}
```

forebay doesn't interpret these; they are passed through verbatim, so
only send fields your endpoint accepts — strict APIs reject unknown
ones. `extra_body` cannot set `model`, `messages`, or `response_format`,
which forebay derives per task; a config that tries is rejected.

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
forebay results --batch jssum --json   # machine-readable (see "Results")
```

`--system`/`--system-file` set the system prompt, `--schema`/
`--schema-file` attach a JSON schema (sent as OpenAI
`response_format: json_schema` with `strict: true`), and the user
prompt comes after `--`. In `batch-llm`, both prompts take the same
per-file placeholders as `forebay batch`. Each reply is saved on the
task (shown by `forebay results`); the full request and response go to
the task log for debugging. Failed calls record the HTTP error — retry
them with `forebay reset --failed && forebay run`.

**Flags go before `--`.** Everything after the separator is prompt text,
so `add-llm -- "summarize" --schema-file s.json` would send the flag to
the model rather than applying it. forebay rejects that at queue time,
and confirms on every queue whether a schema was attached:

```
queued llm task 6223b167 on batch "jssum" — structured output (json_schema, strict)
queued llm task 9d5bf81c on batch "default" — free-form text (no --schema/--schema-file)
```

If a schema was sent but the reply comes back as prose, the task fails
with that reason instead of silently saving text. Check `forebay logs
TASK_ID`: the request body it prints is exactly what forebay POSTed,
`response_format` included, followed by the raw response.

Two things routinely swallow a schema, and neither is forebay:

- **Gateways drop unsupported parameters silently.** A router that picks
  the model for you may forward `response_format` to models that support
  structured outputs and quietly discard it for those that don't, so the
  same command honors the schema one run and returns prose the next. Pin
  a model known to support structured outputs (`--model`, or `"model"` in
  the config) rather than an auto-routing alias. If your gateway has an
  opt-in for strict parameter handling, pass it via `extra_body` —
  OpenRouter, for instance, takes `{"provider": {"require_parameters":
  true}}`.
- **`strict: true` constrains the schema.** OpenAI requires
  `"additionalProperties": false` and every property listed in
  `required`; a schema missing either comes back as an HTTP 400, recorded
  as the task's error.

On **Windows PowerShell**, prefer `--schema-file`. PowerShell 5.1 strips
double quotes when passing arguments to a native executable, so inline
`--schema '{"type":"object"}'` arrives as `{type:object}` and is rejected
as invalid JSON. Backslash-escape them (`'{\"type\":\"object\"}'`) if you
must inline it. The same hazard applies to any JSON you pass through
`forebay add` to another CLI — `forebay results --json` shows the exact
argv that was stored, which is the fastest way to confirm what survived.

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
- **Logs** are written to `~/.forebay/logs/<batch-id>/<task-id>.log`, and
  the same output is saved on the task as its result when it settles
  (see [Results](#results-every-tasks-output-queryable)). With `-j 1`
  output also streams to your terminal.
- **Failure** is exit code ≠ 0 or a spawn error; retry with
  `forebay reset --failed` then `forebay run`.
- Set `FOREBAY_HOME` to relocate all state (useful for tests).

## Windows notes

- Spawning many processes in a burst can attract Windows Defender's
  attention; consider an exclusion for your `forebay.exe` and
  `%USERPROFILE%\.forebay` if you see slow starts.
- Globs are matched case-insensitively deduped, since the filesystem is
  case-insensitive.
