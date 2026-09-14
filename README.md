# forebay

A daemon-less command queue. Queue commands now - by hand or from a glob
template - and run them when you choose, sequentially or in parallel.

State lives in a SQLite database under `~/.forebay/`. Nothing executes
until you type `forebay run` or your scheduler does it for you.

## Install

```sh
go install github.com/andrewsinnovations/forebay/cmd/forebay@latest
```

or build from source:

```sh
go build -o forebay ./cmd/forebay
```

## Quick start

```sh
# Queue one task per *.js file, from a command template
forebay batch --name jsdoc --glob "src/**/*.js" -- \
  claude -p "analyze '{relpath}' and add jsdoc comments to every function"

# See overall queue statistics
forebay summary

# Queue a single task with a complex command
forebay add --batch mytask --command "echo hello -- arg1 -- arg2"

# See what's queued
forebay status

# Drain the queue, four tasks at a time (omit -j for sequential)
forebay run -j 4

# Query results
forebay results --batch jsdoc
```

## More examples

### Scheduling

```cron
0 2 * * * /home/you/go/bin/forebay run -j 4 >> /home/you/.forebay/cron.log 2>&1
```

## Documentation

- [Commands](docs/commands.md) - full command reference
- [Template placeholders](docs/template-placeholders.md) - batch template syntax
- [Results](docs/results.md) - querying saved task output
- [Scheduling](docs/scheduling.md) - cron, launchd, Task Scheduler
- [Behavior details](docs/behavior.md) - crash recovery, cancellation, logs
- [Windows notes](docs/windows.md) - Windows-specific guidance
