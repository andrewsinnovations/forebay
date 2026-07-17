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
var ErrNoTask = errors.New("no pending tasks")

// EnsureBatch returns the batch with the given name, creating it if necessary.
// On creation, the current working directory and environment are captured.
func (d *DB) EnsureBatch(name, workdir string, env []string) (Batch, error) {
	if name == "" {
		name = "default"
	}
	envJSON, err := json.Marshal(env)
	if err != nil {
		return Batch{}, err
	}
	id := NewID()
	_, err = d.Exec(`INSERT INTO batches (id, name, created_at, workdir, env)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(name) DO NOTHING`,
		id, name, now(), workdir, string(envJSON))
	if err != nil {
		return Batch{}, fmt.Errorf("ensure batch %q: %w", name, err)
	}
	return d.GetBatch(name)
}

// GetBatch resolves a batch by name or ID.
func (d *DB) GetBatch(nameOrID string) (Batch, error) {
	row := d.QueryRow(`SELECT id, name, created_at, workdir, env
		FROM batches WHERE name = ? OR id = ?`, nameOrID, nameOrID)
	var b Batch
	var envJSON string
	if err := row.Scan(&b.ID, &b.Name, &b.CreatedAt, &b.Workdir, &envJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Batch{}, fmt.Errorf("no batch named %q", nameOrID)
		}
		return Batch{}, err
	}
	if err := json.Unmarshal([]byte(envJSON), &b.Env); err != nil {
		return Batch{}, fmt.Errorf("batch %q has corrupt env: %w", b.Name, err)
	}
	return b, nil
}

// AddTask queues one command on the given batch.
func (d *DB) AddTask(batchID string, argv []string) (string, error) {
	if len(argv) == 0 || argv[0] == "" {
		return "", errors.New("empty command")
	}
	argvJSON, err := json.Marshal(argv)
	if err != nil {
		return "", err
	}
	id := NewID()
	_, err = d.Exec(`INSERT INTO tasks (id, batch_id, argv) VALUES (?, ?, ?)`,
		id, batchID, string(argvJSON))
	if err != nil {
		return "", fmt.Errorf("queue task: %w", err)
	}
	return id, nil
}

// AddLLMTask queues an LLM call with the given payload.
func (d *DB) AddLLMTask(batchID, payload string) (string, error) {
	id := NewID()
	_, err := d.Exec(`INSERT INTO tasks (id, batch_id, kind, argv, payload)
		VALUES (?, ?, 'llm', '[]', ?)`, id, batchID, payload)
	if err != nil {
		return "", fmt.Errorf("queue llm task: %w", err)
	}
	return id, nil
}

// Claim atomically takes the oldest pending task for the given runner,
// optionally filtered by batch.
func (d *DB) Claim(runnerID, batch string) (*Task, error) {
	q := `UPDATE tasks SET status = 'running', runner = ?, claimed_at = ?, heartbeat_at = ?
		WHERE seq = (SELECT seq FROM tasks WHERE status = 'pending'`
	args := []any{runnerID, now(), now()}
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
	err := row.Scan(&t.Seq, &t.ID, &t.BatchID, &t.Kind, &argvJSON, &t.Payload, &cancel)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoTask
	}
	if err != nil {
		return nil, fmt.Errorf("claim task: %w", err)
	}
	if err := json.Unmarshal([]byte(argvJSON), &t.Argv); err != nil {
		return nil, fmt.Errorf("task %s has corrupt argv: %w", t.ID, err)
	}
	t.Status = StatusRunning
	t.CancelRequested = cancel != 0
	return &t, nil
}

// Touch refreshes a running task's heartbeat and reports whether
// cancellation has been requested since the last touch.
func (d *DB) Touch(taskID string) (cancelRequested bool, err error) {
	row := d.QueryRow(`UPDATE tasks SET heartbeat_at = ? WHERE id = ? AND status = 'running'
		RETURNING cancel_requested`, now(), taskID)
	var cancel int
	if err := row.Scan(&cancel); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("task %s is no longer running", taskID)
		}
		return false, err
	}
	return cancel != 0, nil
}

// MarkStarted records the exec start time and log location.
func (d *DB) MarkStarted(taskID, logPath string) error {
	_, err := d.Exec(`UPDATE tasks SET started_at = ?, log_path = ? WHERE id = ?`,
		now(), logPath, taskID)
	return err
}

// MarkFinished settles a task into done/failed/canceled. result is the
// saved output for llm tasks; exec tasks pass "".
func (d *DB) MarkFinished(taskID, status string, exitCode int, errMsg, result string) error {
	_, err := d.Exec(`UPDATE tasks SET status = ?, finished_at = ?, exit_code = ?, error = ?,
		result = ? WHERE id = ?`, status, now(), exitCode, errMsg, result, taskID)
	return err
}

// Requeue puts a claimed task back to pending (used when a runner is
// interrupted mid-task, so the work is retried on the next run).
func (d *DB) Requeue(taskID string) error {
	_, err := d.Exec(`UPDATE tasks SET status = 'pending', runner = NULL, claimed_at = NULL,
		heartbeat_at = NULL, started_at = NULL, cancel_requested = 0 WHERE id = ?`, taskID)
	return err
}

// ReclaimStale requeues running tasks whose heartbeat expired.
func (d *DB) ReclaimStale(olderThan time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-olderThan).Format(time.RFC3339)
	res, err := d.Exec(`UPDATE tasks SET status = 'pending', runner = NULL, claimed_at = NULL,
		heartbeat_at = NULL, started_at = NULL, cancel_requested = 0
		WHERE status = 'running' AND (heartbeat_at IS NULL OR heartbeat_at < ?)`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Reset requeues tasks by status class.
func (d *DB) Reset(failed, all bool) (int64, error) {
	var conds []string
	if all {
		conds = []string{"status IN ('running', 'failed', 'canceled')"}
	} else {
		conds = append(conds, "status = 'running'")
		if failed {
			conds = append(conds, "status = 'failed'")
		}
	}
	res, err := d.Exec(`UPDATE tasks SET status = 'pending', runner = NULL, claimed_at = NULL,
		heartbeat_at = NULL, started_at = NULL, finished_at = NULL, exit_code = NULL,
		error = NULL, cancel_requested = 0 WHERE ` + strings.Join(conds, " OR "))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Cancel marks pending tasks as canceled and requests cancellation of
// running tasks.
func (d *DB) Cancel(taskID, batchID string, all bool) (int64, error) {
	where := ""
	var args []any
	switch {
	case taskID != "":
		where = "id = ?"
		args = []any{taskID}
	case batchID != "":
		where = "batch_id = ?"
		args = []any{batchID}
	case all:
		where = "1=1"
	default:
		return 0, errors.New("nothing selected to cancel")
	}
	res, err := d.Exec(`UPDATE tasks SET
		status = CASE WHEN status = 'pending' THEN 'canceled' ELSE status END,
		cancel_requested = CASE WHEN status = 'running' THEN 1 ELSE cancel_requested END
		WHERE (status = 'pending' OR status = 'running') AND `+where, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Clean deletes finished tasks and returns their log paths.
func (d *DB) Clean(batchID string, all bool) (logPaths []string, deleted int64, err error) {
	statuses := `('done', 'failed', 'canceled')`
	if all {
		statuses = `('done', 'failed', 'canceled', 'pending')`
	}
	q := `SELECT COALESCE(log_path, '') FROM tasks WHERE status IN ` + statuses
	var args []any
	if batchID != "" {
		q += ` AND batch_id = ?`
		args = append(args, batchID)
	}
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, 0, err
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, 0, err
		}
		if p != "" {
			logPaths = append(logPaths, p)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, 0, err
	}
	rows.Close()

	del := `DELETE FROM tasks WHERE status IN ` + statuses
	if batchID != "" {
		del += ` AND batch_id = ?`
	}
	res, err := d.Exec(del, args...)
	if err != nil {
		return nil, 0, err
	}
	deleted, err = res.RowsAffected()
	if err != nil {
		return nil, 0, err
	}
	if _, err := d.Exec(`DELETE FROM batches WHERE NOT EXISTS
		(SELECT 1 FROM tasks t WHERE t.batch_id = batches.id)`); err != nil {
		return logPaths, deleted, err
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
		return nil, err
	}
	defer rows.Close()
	var out []BatchStatus
	for rows.Next() {
		var s BatchStatus
		var p, r, dn, f, c sql.NullInt64
		if err := rows.Scan(&s.Batch.ID, &s.Batch.Name, &s.Batch.CreatedAt, &s.Batch.Workdir,
			&p, &r, &dn, &f, &c); err != nil {
			return nil, err
		}
		s.Pending, s.Running, s.Done, s.Failed, s.Canceled =
			int(p.Int64), int(r.Int64), int(dn.Int64), int(f.Int64), int(c.Int64)
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListTasks returns tasks, optionally filtered by batch, status, and/or
// kind, oldest first.
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

// taskSelect is the base query for fetching tasks with batch names.
const taskSelect = `SELECT t.seq, t.id, t.batch_id, b.name, t.kind, t.argv,
	COALESCE(t.payload, ''), COALESCE(t.result, ''), t.status,
	COALESCE(t.runner, ''), COALESCE(t.claimed_at, ''), COALESCE(t.heartbeat_at, ''),
	COALESCE(t.started_at, ''), COALESCE(t.finished_at, ''), t.exit_code,
	COALESCE(t.error, ''), COALESCE(t.log_path, ''), t.cancel_requested
	FROM tasks t JOIN batches b ON b.id = t.batch_id`

// GetTask resolves one task by ID.
func (d *DB) GetTask(id string) (Task, error) {
	rows, err := d.Query(taskSelect+` WHERE t.id = ?`, id)
	if err != nil {
		return Task{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Task{}, err
		}
		return Task{}, fmt.Errorf("no task with id %q", id)
	}
	return scanTask(rows)
}

// scanTask deserializes a task row from the database.
func scanTask(rows *sql.Rows) (Task, error) {
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
