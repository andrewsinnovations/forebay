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

// envHome names the environment variable that overrides the forebay home
// directory.
const envHome = "FOREBAY_HOME"

// defaultBusyTimeout is how long SQLite waits for a write lock before failing.
const defaultBusyTimeout = 5000

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

// DB wraps sql.DB with the data directory needed to derive task paths.
//
// A DB is safe for simultaneous use by multiple goroutines; so is each of its
// methods.
type DB struct {
	*sql.DB
	dir string // forebay home directory
}

// HomeDir returns the forebay home directory, which holds the database and
// task logs. It honors the FOREBAY_HOME environment variable.
func HomeDir() (string, error) {
	if d := os.Getenv(envHome); d != "" {
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
	dir, err := HomeDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	dbPath := filepath.Join(dir, "forebay.db")
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)",
		url.PathEscape(dbPath), defaultBusyTimeout)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dbPath, err)
	}
	if err := initSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{DB: db, dir: dir}, nil
}

// initSchema creates the tables and applies schema migrations for databases
// created by older versions.
func initSchema(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return migrate(db)
}

// columns lists the existing column names of the tasks table.
func columns(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query(`PRAGMA table_info(tasks)`)
	if err != nil {
		return nil, fmt.Errorf("read tasks table info: %w", err)
	}
	defer rows.Close()
	have := make(map[string]bool, 16)
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, fmt.Errorf("scan tasks table info: %w", err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tasks table info: %w", err)
	}
	return have, nil
}

// addedColumns maps a column missing from older schemas to its definition.
var addedColumns = map[string]string{
	"kind":    `TEXT NOT NULL DEFAULT 'exec'`,
	"payload": `TEXT`,
	"result":  `TEXT`,
}

// migrate adds columns introduced after the initial schema.
func migrate(db *sql.DB) error {
	have, err := columns(db)
	if err != nil {
		return err
	}
	for col, def := range addedColumns {
		if have[col] { // if column already present
			continue
		}
		if _, err := db.Exec("ALTER TABLE tasks ADD COLUMN " + col + " " + def); err != nil {
			return fmt.Errorf("add column %q: %w", col, err)
		}
	}
	return nil
}

// Home returns the forebay home directory backing this database.
func (d *DB) Home() string { return d.dir }

// LogsDir returns the directory task logs are written under.
func (d *DB) LogsDir() string { return filepath.Join(d.dir, "logs") }
