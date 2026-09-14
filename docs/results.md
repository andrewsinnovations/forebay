# Results: every task's output, queryable

Each task stores its complete output in the database when it settles -
captured stdout+stderr for commands - so
the queue doubles as a result set you can query after the fact. Canceled
tasks keep whatever they produced before they were killed.

```sh
forebay results                          # everything, oldest first
forebay results --batch jsdoc            # one batch
forebay results --status failed          # only failures

forebay results --contains "TODO"        # output matching a substring
forebay results a1b2c3d4                 # one task by id
forebay results --batch jsdoc --json     # machine-readable
```

`--json` emits one object per task: `task_id`, `batch`, `kind`,
`status`, `command`, `exit_code`, `started_at`,
`finished_at`, `result`, `error`, `log_path` - pipe it to `jq` for
anything the flags don't cover, or query `~/.forebay/forebay.db`
directly (`SELECT id, result FROM tasks WHERE ...`).

Output is stored whole, with no truncation. If a task can emit more
than you want in SQLite, set `FOREBAY_MAX_RESULT_BYTES` when running:
the stored result keeps the head and tail up to that many bytes with a
marker naming the log file in between, while the log on disk stays
complete. `forebay logs TASK_ID` always prints the untruncated log, and
`forebay clean` drops results and logs together.
