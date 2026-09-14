package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNoTask is returned by Claim when the queue has no pending work.
var ErrNoTask = errors.New("store: no pending tasks")

// ErrBatchNotFound reports that no batch matches the requested name or ID.
var ErrBatchNotFound = errors.New("store: batch not found")

// ErrTaskNotFound reports that no task matches the requested ID.
var ErrTaskNotFound = errors.New("store: task not found")

// ErrNothingSelected reports a mutation whose filters matched nothing to act on.
var ErrNothingSelected = errors.New("store: nothing selected")

// ErrEmptyCommand reports an argv with nothing to run.
var ErrEmptyCommand = errors.New("store: empty command")

// ErrNotRunning reports an operation on a task that is no longer running.
var ErrNotRunning = errors.New("store: task is not running")

// requeueSet resets a claimed task back to pending; used both by Requeue and
// by ReclaimStale so the two cannot drift apart.
const requeueSet = `status = 'pending', runner = NULL, claimed_at = NULL,
		heartbeat_at = NULL, started_at = NULL, result = NULL, cancel_requested = 0`

// EnsureBatch returns the batch with the given name, creating it if necessary.
// On creation, workdir and env are captured; an empty workdir means "use the
// process working directory" and an empty name means "default".
func (d *DB) EnsureBatch(name, workdir string, env []string) (Batch, error) {
	if name == "" {
		name = "default"
	}
	envJSON, err := json.Marshal(env)
	if err != nil {
		return Batch{}, fmt.Errorf("encode env for batch %q: %w", name, err)
	}
	_, err = d.Exec(`INSERT INTO batches (id, name, created_at, workdir, env)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(name) DO NOTHING`,
		NewID(), name, time.Now().UTC().Format(time.RFC3339), workdir, string(envJSON))
	if err != nil {
		return Batch{}, fmt.Errorf("ensure batch %q: %w", name, err)
	}
	return d.GetBatch(name)
}

// GetBatch resolves a batch by name or ID. If no batch matches, the returned
// error wraps ErrBatchNotFound.
func (d *DB) GetBatch(nameOrID string) (Batch, error) {
	row := d.QueryRow(`SELECT id, name, created_at, workdir, env
		FROM batches WHERE name = ? OR id = ?`, nameOrID, nameOrID)
	var b Batch
	var envJSON string
	if err := row.Scan(&b.ID, &b.Name, &b.CreatedAt, &b.Workdir, &envJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Batch{}, fmt.Errorf("%w: %q", ErrBatchNotFound, nameOrID)
		}
		return Batch{}, fmt.Errorf("query batch %q: %w", nameOrID, err)
	}
	if err := json.Unmarshal([]byte(envJSON), &b.Env); err != nil {
		return Batch{}, fmt.Errorf("batch %q has corrupt env: %w", b.Name, err)
	}
	return b, nil
}

// AddTask queues one command on the given batch and returns the new task ID.
// An empty argv is reported with ErrEmptyCommand.
func (d *DB) AddTask(batchID string, argv []string) (string, error) {
	if len(argv) == 0 || argv[0] == "" {
		return "", ErrEmptyCommand
	}
	argvJSON, err := json.Marshal(argv)
	if err != nil {
		return "", fmt.Errorf("encode command: %w", err)
	}
	id := NewID()
	if _, err := d.Exec(`INSERT INTO tasks (id, batch_id, argv) VALUES (?, ?, ?)`,
		id, batchID, string(argvJSON)); err != nil {
		return "", fmt.Errorf("queue task: %w", err)
	}
	return id, nil
}

// Claim atomically takes the oldest pending task for the given runner,
// optionally filtered by batch name or ID. When the queue holds no pending
// work, Claim returns an error wrapping ErrNoTask.
func (d *DB) Claim(runnerID, batch string) (*Task, error) {
	q := `UPDATE tasks SET status = 'running', runner = ?, claimed_at = ?, heartbeat_at = ?
		WHERE seq = (SELECT seq FROM tasks WHERE status = 'pending'`
	args := []any{runnerID, time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339)}
	if batch != "" {
		q += ` AND batch_id IN (SELECT id FROM batches WHERE name = ? OR id = ?)`
		args = append(args, batch, batch)
	}
	q += ` ORDER BY seq LIMIT 1)
		RETURNING seq, id, batch_id, kind, argv, COALESCE(payload, ''), cancel_requested`
	row := d.QueryRow(q, args...)
	var t Task
	var argvJSON string
	var cancel int
	if err := row.Scan(&t.Seq, &t.ID, &t.BatchID, &t.Kind, &argvJSON, &t.Payload, &cancel); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNoTask
		}
		return nil, fmt.Errorf("claim task: %w", err)
	}
	if err := json.Unmarshal([]byte(argvJSON), &t.Argv); err != nil {
		return nil, fmt.Errorf("task %s has corrupt argv: %w", t.ID, err)
	}
	t.Status = StatusRunning
	t.CancelRequested = cancel != 0
	return &t, nil
}

// Touch refreshes a running task's heartbeat and reports whether cancellation
// has been requested since the last touch. If the task is no longer running,
// the returned error wraps ErrNotRunning.
func (d *DB) Touch(taskID string) (cancelRequested bool, err error) {
	row := d.QueryRow(`UPDATE tasks SET heartbeat_at = ? WHERE id = ? AND status = 'running'
		RETURNING cancel_requested`, time.Now().UTC().Format(time.RFC3339), taskID)
	var cancel int
	if err := row.Scan(&cancel); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("%w: task %s is no longer running", ErrNotRunning, taskID)
		}
		return false, fmt.Errorf("touch task %s: %w", taskID, err)
	}
	return cancel != 0, nil
}

// MarkStarted records the exec start time and log location.
func (d *DB) MarkStarted(taskID, logPath string) error {
	if _, err := d.Exec(`UPDATE tasks SET started_at = ?, log_path = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), logPath, taskID); err != nil {
		return fmt.Errorf("mark task %s started: %w", taskID, err)
	}
	return nil
}

// MarkFinished settles a task into done, failed, or canceled. result is the
// captured stdout and stderr for the task.
func (d *DB) MarkFinished(taskID, status string, exitCode int, errMsg, result string) error {
	if _, err := d.Exec(`UPDATE tasks SET status = ?, finished_at = ?, exit_code = ?, error = ?,
		result = ? WHERE id = ?`, status, time.Now().UTC().Format(time.RFC3339), exitCode, errMsg, result, taskID); err != nil {
		return fmt.Errorf("mark task %s finished: %w", taskID, err)
	}
	return nil
}

// Requeue puts a claimed task back to pending, so an interrupted run retries
// the work on the next pass.
func (d *DB) Requeue(taskID string) error {
	if _, err := d.Exec(`UPDATE tasks SET `+requeueSet+` WHERE id = ?`, taskID); err != nil {
		return fmt.Errorf("requeue task %s: %w", taskID, err)
	}
	return nil
}

// ReclaimStale requeues running tasks whose heartbeat is older than the given
// duration and returns how many were reclaimed.
func (d *DB) ReclaimStale(olderThan time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-olderThan).Format(time.RFC3339)
	res, err := d.Exec(`UPDATE tasks SET `+requeueSet+`
		WHERE status = 'running' AND (heartbeat_at IS NULL OR heartbeat_at < ?)`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("reclaim stale tasks: %w", err)
	}
	return res.RowsAffected()
}

// Reset requeues tasks by status class: always running tasks, plus failed ones
// when failed is set, and everything but done when all is set.
func (d *DB) Reset(failed, all bool) (int64, error) {
	conds := []string{"status = 'running'"}
	if !all {
		if failed {
			conds = append(conds, "status = 'failed'")
		}
	} else {
		conds = []string{"status IN ('running', 'failed', 'canceled')"}
	}
	res, err := d.Exec(`UPDATE tasks SET status = 'pending', runner = NULL, claimed_at = NULL,
		heartbeat_at = NULL, started_at = NULL, finished_at = NULL, exit_code = NULL,
		error = NULL, result = NULL, cancel_requested = 0 WHERE ` + strings.Join(conds, " OR "))
	if err != nil {
		return 0, fmt.Errorf("reset tasks: %w", err)
	}
	return res.RowsAffected()
}

// Cancel marks pending tasks as canceled and requests cancellation of running
// tasks. Exactly one of taskID, batchID, or all must select the targets;
// otherwise the returned error wraps ErrNothingSelected.
func (d *DB) Cancel(taskID, batchID string, all bool) (int64, error) {
	var where string
	var args []any
	switch {
	case taskID != "":
		where, args = "id = ?", []any{taskID}
	case batchID != "":
		where, args = "batch_id = ?", []any{batchID}
	case all:
		where = "1=1"
	default:
		return 0, fmt.Errorf("%w: cancel needs a task ID, a batch, or --all", ErrNothingSelected)
	}
	res, err := d.Exec(`UPDATE tasks SET
		status = CASE WHEN status = 'pending' THEN 'canceled' ELSE status END,
		cancel_requested = CASE WHEN status = 'running' THEN 1 ELSE cancel_requested END
		WHERE (status = 'pending' OR status = 'running') AND `+where, args...)
	if err != nil {
		return 0, fmt.Errorf("cancel tasks: %w", err)
	}
	return res.RowsAffected()
}

// Clean deletes finished tasks and returns their log paths so the caller can
// remove the files. With all set, pending tasks are deleted too; running tasks
// are never deleted. It also drops batches left without tasks.
func (d *DB) Clean(batchID string, all bool) (logPaths []string, deleted int64, err error) {
	statuses := `('done', 'failed', 'canceled')`
	if all {
		statuses = `('done', 'failed', 'canceled', 'pending')`
	}
	selectQ := `SELECT COALESCE(log_path, '') FROM tasks WHERE status IN ` + statuses
	deleteQ := `DELETE FROM tasks WHERE status IN ` + statuses
	var args []any
	if batchID != "" {
		clause := ` AND batch_id = ?`
		selectQ += clause
		deleteQ += clause
		args = append(args, batchID)
	}

	rows, err := d.Query(selectQ, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("select tasks to clean: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, 0, fmt.Errorf("scan task log path: %w", err)
		}
		if p != "" {
			logPaths = append(logPaths, p)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate tasks to clean: %w", err)
	}
	// Close before the DELETE so SQLite does not hold a read transaction open.
	if err := rows.Close(); err != nil {
		return nil, 0, fmt.Errorf("close task rows: %w", err)
	}

	res, err := d.Exec(deleteQ, args...)
	if err != nil {
		return logPaths, 0, fmt.Errorf("delete cleaned tasks: %w", err)
	}
	deleted, err = res.RowsAffected()
	if err != nil {
		return logPaths, deleted, fmt.Errorf("count cleaned tasks: %w", err)
	}
	if _, err := d.Exec(`DELETE FROM batches WHERE NOT EXISTS
		(SELECT 1 FROM tasks t WHERE t.batch_id = batches.id)`); err != nil {
		return logPaths, deleted, fmt.Errorf("drop empty batches: %w", err)
	}
	return logPaths, deleted, nil
}

// Status returns per-batch task counts, oldest batch first.
func (d *DB) Status() ([]BatchStatus, error) {
	rows, err := d.Query(`SELECT b.id, b.name, b.created_at, b.workdir,
		SUM(CASE WHEN t.status = 'pending'  THEN 1 ELSE 0 END),
		SUM(CASE WHEN t.status = 'running'  THEN 1 ELSE 0 END),
		SUM(CASE WHEN t.status = 'done'     THEN 1 ELSE 0 END),
		SUM(CASE WHEN t.status = 'failed'   THEN 1 ELSE 0 END),
		SUM(CASE WHEN t.status = 'canceled' THEN 1 ELSE 0 END)
		FROM batches b LEFT JOIN tasks t ON t.batch_id = b.id
		GROUP BY b.id ORDER BY b.created_at, b.id`)
	if err != nil {
		return nil, fmt.Errorf("query batch statuses: %w", err)
	}
	defer rows.Close()
	var out []BatchStatus
	for rows.Next() {
		var s BatchStatus
		var p, r, dn, f, c sql.NullInt64
		if err := rows.Scan(&s.Batch.ID, &s.Batch.Name, &s.Batch.CreatedAt, &s.Batch.Workdir,
			&p, &r, &dn, &f, &c); err != nil {
			return nil, fmt.Errorf("scan batch status: %w", err)
		}
		s.Pending, s.Running, s.Done, s.Failed, s.Canceled =
			int(p.Int64), int(r.Int64), int(dn.Int64), int(f.Int64), int(c.Int64)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate batch statuses: %w", err)
	}
	return out, nil
}

// ListTasks returns tasks oldest first, optionally filtered by batch ID, task
// status, and task kind. A limit of zero or less means no limit.
func (d *DB) ListTasks(batchID, status, kind string, limit int) ([]Task, error) {
	q := taskSelect + ` WHERE 1=1`
	var args []any
	if batchID != "" {
		q += ` AND t.batch_id = ?`
		args = append(args, batchID)
	}
	if status != "" {
		q += ` AND t.status = ?`
		args = append(args, status)
	}
	if kind != "" {
		q += ` AND t.kind = ?`
		args = append(args, kind)
	}
	q += ` ORDER BY t.seq`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ResultFilter selects saved task outputs. Zero fields match everything.
type ResultFilter struct {
	TaskID   string
	BatchID  string
	Status   string
	Kind     string
	Contains string // substring of the saved result (ASCII case-insensitive)
	Limit    int
}

// Results returns tasks and their saved output, oldest first.
func (d *DB) Results(f ResultFilter) ([]Task, error) {
	q := taskSelect + ` WHERE 1=1`
	var args []any
	for _, c := range []struct {
		cond string
		val  string
	}{
		{` AND t.id = ?`, f.TaskID},
		{` AND t.batch_id = ?`, f.BatchID},
		{` AND t.status = ?`, f.Status},
		{` AND t.kind = ?`, f.Kind},
	} {
		if c.val != "" {
			q += c.cond
			args = append(args, c.val)
		}
	}
	if f.Contains != "" {
		q += ` AND t.result LIKE ? ESCAPE '\'`
		args = append(args, "%"+strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(f.Contains)+"%")
	}
	q += ` ORDER BY t.seq`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

const taskSelect = `SELECT t.seq, t.id, t.batch_id, b.name, t.kind, t.argv,
	COALESCE(t.payload, ''), COALESCE(t.result, ''), t.status,
	COALESCE(t.runner, ''), COALESCE(t.claimed_at, ''), COALESCE(t.heartbeat_at, ''),
	COALESCE(t.started_at, ''), COALESCE(t.finished_at, ''), t.exit_code,
	COALESCE(t.error, ''), COALESCE(t.log_path, ''), t.cancel_requested
	FROM tasks t JOIN batches b ON b.id = t.batch_id`

// GetTask resolves one task by ID. If no task matches, the returned error
// wraps ErrTaskNotFound.
func (d *DB) GetTask(id string) (Task, error) {
	row := d.QueryRow(taskSelect+` WHERE t.id = ?`, id)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, fmt.Errorf("%w: %q", ErrTaskNotFound, id)
	}
	if err != nil {
		return Task{}, fmt.Errorf("get task %s: %w", id, err)
	}
	return t, nil
}

// rowScanner covers both *sql.Row and *sql.Rows so one scan helper serves
// single-row and multi-row queries.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanTask deserializes one task row. If no row is available, it returns an
// error wrapping sql.ErrNoRows.
func scanTask(rows rowScanner) (Task, error) {
	var t Task
	var argvJSON string
	var exit sql.NullInt64
	var cancel int
	if err := rows.Scan(&t.Seq, &t.ID, &t.BatchID, &t.BatchName, &t.Kind, &argvJSON,
		&t.Payload, &t.Result, &t.Status,
		&t.Runner, &t.ClaimedAt, &t.HeartbeatAt, &t.StartedAt, &t.FinishedAt,
		&exit, &t.Error, &t.LogPath, &cancel); err != nil {
		return Task{}, err
	}
	if exit.Valid {
		t.ExitCode = &exit.Int64
	}
	t.CancelRequested = cancel != 0
	if err := json.Unmarshal([]byte(argvJSON), &t.Argv); err != nil {
		return Task{}, fmt.Errorf("task %s has corrupt argv: %w", t.ID, err)
	}
	return t, nil
}
