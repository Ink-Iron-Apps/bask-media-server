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
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned instead of sql.ErrNoRows so callers need not import
// database/sql to tell "no such item" from a real failure.
var ErrNotFound = errors.New("not found")

// DB is a handle on the library database.
type DB struct {
	sql  *sql.DB
	path string
}

// Open opens or creates the database at path and brings the schema up to date.
//
// The pragmas live in the connection string: see [dsn] for them and for why
// they cannot be statements run after opening.
func Open(ctx context.Context, path string) (*DB, error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	// One writer and a small reader pool. SQLite serialises writes anyway, and
	// an unbounded pool would open a connection per request, each with its own
	// page cache — a memory leak with a respectable name.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)

	wrapped := &DB{sql: db, path: path}
	if err := wrapped.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return wrapped, nil
}

// dsn builds the connection string, pragmas included.
//
// In the connection string and not as statements after opening, which is the
// correction of a real bug: a pragma is per-connection, and database/sql hands
// out whichever of its pooled connections is free. Running them once after
// Open configured exactly one connection and left the rest on the defaults —
// so a write that happened to land on another connection had no busy timeout
// and failed outright with SQLITE_BUSY the moment two goroutines wrote at
// once, and foreign keys were off there, which is the setting that stops a
// deleted section leaving orphaned rows behind.
//
// The pragmas themselves:
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
func dsn(path string) string {
	pragmas := []string{
		"_pragma=journal_mode(WAL)",
		"_pragma=busy_timeout(5000)",
		"_pragma=foreign_keys(on)",
		"_pragma=synchronous(normal)",
		"_pragma=cache_size(-8000)",
	}
	// A path is a filename, not a URL: anything that would be read as the
	// start of the query or a fragment has to be escaped, and nothing else
	// should be, because percent-escaping a whole path breaks a directory
	// named with a percent sign.
	escaped := strings.ReplaceAll(path, "%", "%25")
	escaped = strings.ReplaceAll(escaped, "?", "%3f")
	escaped = strings.ReplaceAll(escaped, "#", "%23")
	return "file:" + escaped + "?" + strings.Join(pragmas, "&")
}

// Close releases the database.
func (d *DB) Close() error { return d.sql.Close() }

// Path is where the database file lives, which is how a status page reports
// its size without being told separately where it was opened from.
func (d *DB) Path() string { return d.path }

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

	// Metadata a provider supplied, kept beside the scanned facts rather than
	// replacing them: the filename is what the file says it is, and the
	// provider is a second opinion that can be wrong or absent.
	`ALTER TABLE items ADD COLUMN provider_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE items ADD COLUMN poster_key TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE items ADD COLUMN art_key TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE items ADD COLUMN metadata_at INTEGER NOT NULL DEFAULT 0`,
	`CREATE INDEX items_unenriched ON items(metadata_at, kind)`,

	// The server's identity, which a Plex client expects to be stable forever:
	// change it and every client treats this as a different server and forgets
	// everything it knew.
	`CREATE TABLE server (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,

	// Chapters a file declares, and the markers worked out from them or from
	// the audio. Separate tables because they answer different questions: a
	// client draws chapters on the timeline, and offers a Skip button for a
	// marker.
	`CREATE TABLE chapters (
		id       INTEGER PRIMARY KEY AUTOINCREMENT,
		item_id  INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
		idx      INTEGER NOT NULL,
		title    TEXT NOT NULL DEFAULT '',
		start_ms INTEGER NOT NULL,
		end_ms   INTEGER NOT NULL
	)`,
	`CREATE UNIQUE INDEX chapters_item_idx ON chapters(item_id, idx)`,

	// Unique on where a marker starts rather than on its type, because a file
	// can legitimately carry two of a kind — a two-part opening, a mid-credits
	// scene — and because it makes a detector's write idempotent.
	`CREATE TABLE markers (
		id       INTEGER PRIMARY KEY AUTOINCREMENT,
		item_id  INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
		type     TEXT NOT NULL,
		source   TEXT NOT NULL,
		start_ms INTEGER NOT NULL,
		end_ms   INTEGER NOT NULL
	)`,
	`CREATE UNIQUE INDEX markers_item_type_start ON markers(item_id, type, source, start_ms)`,
	`CREATE INDEX markers_item ON markers(item_id)`,

	// Audio fingerprints, which are a cache: ~19 KiB per end of a file, and
	// derivable again from the file at the cost of a decode. Keyed on the
	// file's size and mtime as well as its id, so a replaced file is a miss
	// rather than a marker computed from a file that is no longer there.
	`CREATE TABLE fingerprints (
		part_id     INTEGER NOT NULL REFERENCES parts(id) ON DELETE CASCADE,
		segment     TEXT NOT NULL,
		offset_ms   INTEGER NOT NULL,
		data        BLOB NOT NULL,
		size        INTEGER NOT NULL,
		modified_at INTEGER NOT NULL,
		created_at  INTEGER NOT NULL,
		PRIMARY KEY (part_id, segment)
	)`,

	// When an audio pass last finished for a season, recorded even when it
	// found nothing so a show with no detectable intro is not decoded again
	// on every pass.
	`ALTER TABLE items ADD COLUMN markers_at INTEGER NOT NULL DEFAULT 0`,
	`CREATE INDEX items_undetected ON items(kind, markers_at)`,

	// Browser sessions for the admin interface. Separate from tokens on
	// purpose: a token is a credential a television keeps for years, and
	// signing out of a web page must not log the television out of the
	// library. Hashed like a token, and with an expiry a token does not have.
	`CREATE TABLE sessions (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		hash         BLOB NOT NULL UNIQUE,
		created_at   INTEGER NOT NULL,
		expires_at   INTEGER NOT NULL,
		last_seen_at INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX sessions_expiry ON sessions(expires_at)`,

	// Subtitles, whether they are streams in the container or files beside
	// it. One table for both because every client-facing question — what can
	// I show, in what language, is it forced — is the same question either
	// way, and only this server cares where the text comes from.
	`CREATE TABLE subtitles (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		item_id      INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
		part_id      INTEGER NOT NULL REFERENCES parts(id) ON DELETE CASCADE,
		external     INTEGER NOT NULL DEFAULT 0,
		stream_index INTEGER NOT NULL DEFAULT -1,
		path         TEXT NOT NULL DEFAULT '',
		codec        TEXT NOT NULL DEFAULT '',
		language     TEXT NOT NULL DEFAULT '',
		title        TEXT NOT NULL DEFAULT '',
		forced       INTEGER NOT NULL DEFAULT 0,
		is_default   INTEGER NOT NULL DEFAULT 0,
		hearing      INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX subtitles_item ON subtitles(item_id)`,
	// A scan rewrites a file's subtitles wholesale, and this is what makes
	// that idempotent: the same stream or the same sidecar is the same row.
	`CREATE UNIQUE INDEX subtitles_identity ON subtitles(part_id, external, stream_index, path)`,
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

// millisDuration reads a stored millisecond count as a duration.
func millisDuration(ms int64) time.Duration {
	return time.Duration(ms) * time.Millisecond
}

func fromMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
