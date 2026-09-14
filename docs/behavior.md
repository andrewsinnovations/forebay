# Behavior Details

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
  (see [Results](results.md)). With `-j 1` output also streams to your
  terminal.
- **Failure** is exit code ≠ 0 or a spawn error; retry with
  `forebay reset --failed` then `forebay run`.
- Set `FOREBAY_HOME` to relocate all state (useful for tests).
