# Scheduling

forebay has no scheduler; your OS already has one. Point it at
`forebay run`:

## cron (Linux)

```cron
0 2 * * * /home/you/go/bin/forebay run -j 4 >> /home/you/.forebay/cron.log 2>&1
```

## launchd (macOS)

Prefer a LaunchAgent over cron (cron needs Full Disk Access on modern
macOS). A minimal plist runs `forebay run -j 4` on a
`StartCalendarInterval`.

## Task Scheduler (Windows)

```powershell
schtasks /Create /TN forebay /SC DAILY /ST 02:00 /TR "C:\path\to\forebay.exe run -j 4"
```

## Why scheduled runs are safe

Two design choices make scheduled runs safe and reliable:

- **Overlap is harmless.** Tasks are claimed with a single atomic
  `UPDATE ... RETURNING`, so a cron-fired runner colliding with a manual
  one just means more workers on the same queue - never double
  execution.
- **Batches capture their environment.** `forebay add` / `forebay
  batch` snapshot the working directory and environment
  (PATH, credentials) at queue time, and the runner executes tasks under
  that snapshot. A cron daemon's near-empty environment won't break
  `claude` resolution or auth. Note this means the environment is stored
  in plaintext in `~/.forebay/forebay.db`, which is created `0700`.
