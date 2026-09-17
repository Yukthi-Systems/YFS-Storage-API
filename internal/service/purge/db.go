package purge

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/Yukthi-Systems/YFS-Storage-API/internal/utils"
)

// schema holds the single table backing the queue: one row per path that
// has been accepted for deletion but not yet confirmed gone. A
// successful delete removes its row outright — this is a work queue, not
// an audit log, so it never grows beyond however large the current
// backlog is.
const schema = `
CREATE TABLE IF NOT EXISTS pending_deletes (
	path            TEXT PRIMARY KEY,
	enqueued_at     INTEGER NOT NULL,
	attempts        INTEGER NOT NULL DEFAULT 0,
	last_error      TEXT,
	last_attempt_at INTEGER
) WITHOUT ROWID;
`

// OpenDB opens (creating if necessary) the SQLite database at dbPath and
// prepares it for use by a Queue.
//
// The connection pool is pinned to exactly one connection: SQLite only
// ever allows one writer at a time regardless, and database/sql already
// serializes every Exec/Query onto that single connection for us, so
// concurrent Enqueue calls from multiple HTTP requests and the worker
// pool's own writes can never race or corrupt the file — no hand-rolled
// mutex needed on top.
//
// WAL journaling plus synchronous=FULL means every write this package
// makes is fsynced to disk before the call returns: a crash the instant
// after Enqueue (or a completed delete) returns can never lose it.
func OpenDB(dbPath string) (*sql.DB, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("purge: database path must not be empty")
	}
	if dir := filepath.Dir(dbPath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("purge: creating directory for %q: %w", dbPath, err)
		}
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("purge: opening %q: %w", dbPath, err)
	}
	db.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("purge: setting %q: %w", pragma, err)
		}
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("purge: creating schema: %w", err)
	}

	return db, nil
}

// DefaultDBPath returns "purge.db" next to the currently running binary,
// so a default deployment needs no extra configuration to get a durable,
// discoverable queue file. Callers that want it elsewhere (e.g. alongside
// other application state) should set PURGE_DB_PATH instead of relying on
// this.
func DefaultDBPath() string {
	return utils.PathNextToBinary("purge.db")
}
