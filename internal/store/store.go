// Package store implements a SQLite-backed task queue shared between
// the CLI, runner, and MCP server.
package store

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// schema creates the database tables.
const schema = `
CREATE TABLE IF NOT EXISTS batches (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL,
	workdir    TEXT NOT NULL DEFAULT '',
	env        TEXT NOT NULL DEFAULT '[]'
);

CREATE TABLE IF NOT EXISTS tasks (
	seq              INTEGER PRIMARY KEY AUTOINCREMENT,
	id               TEXT NOT NULL UNIQUE,
	batch_id         TEXT NOT NULL REFERENCES batches(id) ON DELETE CASCADE,
	kind             TEXT NOT NULL DEFAULT 'exec',
	argv             TEXT NOT NULL,
	payload          TEXT,
	result           TEXT,
	status           TEXT NOT NULL DEFAULT 'pending',
	runner           TEXT,
	claimed_at       TEXT,
	heartbeat_at     TEXT,
	started_at       TEXT,
	finished_at      TEXT,
	exit_code        INTEGER,
	error            TEXT,
	log_path         TEXT,
	cancel_requested INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);
CREATE INDEX IF NOT EXISTS idx_tasks_batch  ON tasks(batch_id);
`

// DB wraps the sql.DB with the data directory for deriving task paths.
type DB struct {
	*sql.DB
	dir string // forebay home directory
}

// Dir returns the forebay home directory, respecting FOREBAY_HOME.
func Dir() (string, error) {
	if d := os.Getenv("FOREBAY_HOME"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".forebay"), nil
}

// Open initializes the database and applies migrations.
func Open() (*DB, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	dbPath := filepath.Join(dir, "forebay.db")
	dsn := "file:" + url.PathEscape(dbPath) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dbPath, err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	d := &DB{DB: db, dir: dir}
	if err := d.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return d, nil
}

// migrate applies schema changes for databases created by older versions.
func (d *DB) migrate() error {
	rows, err := d.Query(`PRAGMA table_info(tasks)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return err
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for col, def := range map[string]string{
		"kind":    `TEXT NOT NULL DEFAULT 'exec'`,
		"payload": `TEXT`,
		"result":  `TEXT`,
	} {
		if !have[col] {
			if _, err := d.Exec(`ALTER TABLE tasks ADD COLUMN ` + col + ` ` + def); err != nil {
				return err
			}
		}
	}
	return nil
}

// HomeDir returns the forebay home directory backing this database.
func (d *DB) HomeDir() string {
	return d.dir
}

// LogsDir returns the directory task logs are written under.
func (d *DB) LogsDir() string {
	return filepath.Join(d.dir, "logs")
}
