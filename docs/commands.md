# Commands

| Command | What it does |
| --- | --- |
| `forebay add [--batch NAME] [--dir DIR] [--command CMD] -- CMD [ARGS...]` | Queue a single command onto a batch. Use `--command "CMD"` for complex commands with multiple `--`. Placeholders like `{relpath}` are not supported here; use `forebay batch` instead. |
| `forebay batch --glob PAT [--glob ...] [--name N] [--dir ROOT] [--exclude PAT] [--dry-run] -- TEMPLATE...` | Expand globs into tasks for execution. |
| `forebay results [TASK_ID] [--batch NAME] [--status STATUS] [--contains TEXT] [--limit N] [--json]` | Query saved task output (results and logs) with optional filtering. |
| `forebay run [-j N] [--batch NAME] [--watch] [--interval SECS]` | Claim and execute pending tasks in parallel. `--watch` keeps polling for new tasks after the queue drains. |
| `forebay status` | Show a summary of task counts per batch (pending, running, done, failed, canceled). |
| `forebay list [--batch NAME] [--status STATUS] [--limit N]` | List tasks with optional filtering by batch name or status. |
| `forebay logs TASK_ID` | Print a task's untruncated captured output. |
| `forebay cancel [TASK_ID] [--batch NAME] [--all]` | Cancel pending tasks immediately and kill running tasks within a few seconds. |
| `forebay reset [--failed] [--all]` | Requeue pending and/or failed tasks. |
| `forebay summary` | Show overall queue statistics (pending, running, done, failed, canceled). |
| `forebay clean [--batch NAME] [--all]` | Delete results and logs for finished (and optionally pending) tasks. Running tasks are never cleaned up. |

## When to use each command

- **`forebay add`**: For a single, ad-hoc task. Use when you know exactly what command to run right now.
- **`forebay batch`**: For templated tasks based on files. Ideal when you want to process many files with the same logic (e.g., adding JSDoc comments to all JS files).
