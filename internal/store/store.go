// Package store is BMS's only store of record.
//
// SQLite rather than a server database: the library is a single machine's
// worth of rows, the access pattern is one writer and a handful of readers,
// and a file nobody has to administer is worth more here than a process that
// has to be kept alive. The driver is pure Go (modernc.org/sqlite) so a
// Windows build needs no C toolchain, which is the whole reason BMS can be
// cross-compiled from this Linux box to the desktop that holds the media.
//
// Memory discipline lives here as much as anywhere: readers stream rows and
// every list query is paged, so no caller can accidentally ask for a whole
// library at once. See docs/RESOURCE-BUDGET.md.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned instead of sql.ErrNoRows so callers need not import
// database/sql to tell "no such item" from a real failure.
var ErrNotFound = errors.New("not found")

// DB is a handle on the library database.
type DB struct {
	sql *sql.DB
}

// Open opens or creates the database at path and brings the schema up to date.
//
// The pragmas are the interesting part:
//
//   - WAL, so a scan writing in the background does not block a client reading
//     in the foreground. This is the single biggest responsiveness win.
//   - busy_timeout, so the rare collision waits rather than failing a request.
//   - cache_size as a negative number, which SQLite reads as KiB rather than
//     pages: 8 MiB, deliberately small. The default would grow with use, and
//     the budget has no room for a page cache that decides its own size.
//   - foreign_keys, so a deleted section cannot leave orphaned items behind.
//   - synchronous NORMAL, which under WAL loses at most the last transaction on
//     a power cut. A rescan rebuilds anything lost; a slow scanner is forever.
func Open(ctx context.Context, path string) (*DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	// One writer and a small reader pool. SQLite serialises writes anyway, and
	// an unbounded pool would open a connection per request, each with its own
	// page cache — a memory leak with a respectable name.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)

	pragmas := []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA cache_size = -8000",
	}
	for _, p := range pragmas {
		if _, err := db.ExecContext(ctx, p); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}

	wrapped := &DB{sql: db}
	if err := wrapped.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return wrapped, nil
}

// Close releases the database.
func (d *DB) Close() error { return d.sql.Close() }

// SQL exposes the underlying handle for the few places that need a transaction
// of their own, such as the scanner's batched writes.
func (d *DB) SQL() *sql.DB { return d.sql }

// migrations are applied in order and recorded by index. Append only: an
// existing database is upgraded by running the ones it has not seen, so
// editing a shipped migration would leave two databases with the same version
// and different shapes.
var migrations = []string{
	`CREATE TABLE sections (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		title       TEXT NOT NULL,
		kind        TEXT NOT NULL,
		roots       TEXT NOT NULL,
		scanned_at  INTEGER NOT NULL DEFAULT 0,
		created_at  INTEGER NOT NULL,
		updated_at  INTEGER NOT NULL
	)`,

	`CREATE TABLE items (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		section_id  INTEGER NOT NULL REFERENCES sections(id) ON DELETE CASCADE,
		kind        TEXT NOT NULL,
		parent_id   INTEGER REFERENCES items(id) ON DELETE CASCADE,
		title       TEXT NOT NULL,
		sort_title  TEXT NOT NULL,
		year        INTEGER NOT NULL DEFAULT 0,
		idx         INTEGER NOT NULL DEFAULT 0,
		summary     TEXT NOT NULL DEFAULT '',
		duration_ms INTEGER NOT NULL DEFAULT 0,
		added_at    INTEGER NOT NULL,
		updated_at  INTEGER NOT NULL
	)`,

	// The three ways anything is ever looked up: a section's top level, a
	// parent's children, and the identity of a thing the scanner just found.
	`CREATE INDEX items_section_kind_sort ON items(section_id, kind, sort_title)`,
	`CREATE INDEX items_parent_idx ON items(parent_id, idx)`,
	`CREATE UNIQUE INDEX items_identity ON items(section_id, kind, IFNULL(parent_id, 0), title, year, idx)`,

	`CREATE TABLE parts (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		item_id     INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
		path        TEXT NOT NULL UNIQUE,
		size        INTEGER NOT NULL,
		container   TEXT NOT NULL,
		modified_at INTEGER NOT NULL
	)`,
	`CREATE INDEX parts_item ON parts(item_id)`,

	`CREATE TABLE users (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		username   TEXT NOT NULL UNIQUE,
		admin      INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL
	)`,

	`CREATE TABLE tokens (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		label        TEXT NOT NULL DEFAULT '',
		hash         BLOB NOT NULL UNIQUE,
		created_at   INTEGER NOT NULL,
		last_seen_at INTEGER NOT NULL DEFAULT 0
	)`,

	`CREATE TABLE progress (
		user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		item_id     INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
		offset_ms   INTEGER NOT NULL DEFAULT 0,
		duration_ms INTEGER NOT NULL DEFAULT 0,
		watched     INTEGER NOT NULL DEFAULT 0,
		view_count  INTEGER NOT NULL DEFAULT 0,
		updated_at  INTEGER NOT NULL,
		PRIMARY KEY (user_id, item_id)
	)`,
	`CREATE INDEX progress_recent ON progress(user_id, updated_at DESC)`,

	// The server's identity, which a Plex client expects to be stable forever:
	// change it and every client treats this as a different server and forgets
	// everything it knew.
	`CREATE TABLE server (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
}

func (d *DB) migrate(ctx context.Context) error {
	if _, err := d.sql.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("schema_version: %w", err)
	}

	var current int
	row := d.sql.QueryRowContext(ctx, `SELECT IFNULL(MAX(version), 0) FROM schema_version`)
	if err := row.Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	for i := current; i < len(migrations); i++ {
		tx, err := d.sql.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM schema_version`); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (?)`, i+1); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", i+1, err)
		}
	}
	return nil
}

// ServerValue reads a value from the server table, or "" when absent.
func (d *DB) ServerValue(ctx context.Context, key string) (string, error) {
	var value string
	err := d.sql.QueryRowContext(ctx, `SELECT value FROM server WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", key, err)
	}
	return value, nil
}

// SetServerValue stores a value in the server table.
func (d *DB) SetServerValue(ctx context.Context, key, value string) error {
	_, err := d.sql.ExecContext(ctx,
		`INSERT INTO server (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value)
	if err != nil {
		return fmt.Errorf("write %s: %w", key, err)
	}
	return nil
}

func millis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
