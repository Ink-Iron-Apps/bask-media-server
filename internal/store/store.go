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

// ErrSmartCollection is what a caller gets for trying to put an item into a
// collection whose contents come from its rules. Not a refusal of permission:
// the request has nowhere to go, because a smart collection stores no
// membership to add to.
var ErrSmartCollection = errors.New("a smart collection's contents come from its rules")

// DB is a handle on the library database.
//
// A handle can be narrowed to a subset of the libraries with [DB.Visible],
// which returns a copy sharing this one's connection pool. Every query that
// returns a library, an item or a file consults that set, so a request holding
// a narrowed handle cannot reach past it.
type DB struct {
	sql     *sql.DB
	path    string
	visible *visibility

	// viewer is whose opinion this handle reads where a query needs one —
	// at present only a smart collection's watch-state rules, which mean
	// nothing without somebody to have watched. Zero is nobody, which reads
	// as having watched nothing rather than as an error: a surface with no
	// account behind it still has to be able to list a collection.
	viewer int64
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

	// Music and photographs, which need two facts the other kinds do not.
	//
	// disc is part of a track's identity, because the two discs of an album
	// both have a track 1 and nothing else about them differs; taken_at is
	// what a photo album is ordered by, since a folder of pictures belongs in
	// the order they happened rather than the order of their filenames.
	`ALTER TABLE items ADD COLUMN disc INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE items ADD COLUMN taken_at INTEGER NOT NULL DEFAULT 0`,
	`DROP INDEX items_identity`,
	`CREATE UNIQUE INDEX items_identity ON items(section_id, kind, IFNULL(parent_id, 0), title, year, idx, disc)`,
	`CREATE INDEX items_parent_taken ON items(parent_id, taken_at)`,

	// A photograph's dimensions and the rotation its EXIF asks for. On the
	// part rather than the item because they describe the file: a photograph
	// with two versions on disk has two sizes, and this is the row that is
	// one file.
	`ALTER TABLE parts ADD COLUMN width INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE parts ADD COLUMN height INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE parts ADD COLUMN orientation INTEGER NOT NULL DEFAULT 0`,

	// Who plays on a track, where that is not the album's artist. Its own
	// column because it is per track while the artist is per album, and
	// because a compilation is unreadable without it.
	`ALTER TABLE items ADD COLUMN performer TEXT NOT NULL DEFAULT ''`,

	// Which libraries a token may see.
	//
	// A token is BMS's per-device credential, so a row here is "this device
	// may see that library" without inventing a second notion of a device:
	// the television has its own token already, because that is what makes it
	// revocable on its own.
	//
	// No row for a token means no restriction, which is what keeps this table
	// empty on every database that does not want the feature and makes the
	// migration a no-op in behaviour as well as in shape.
	`CREATE TABLE token_sections (
		token_id   INTEGER NOT NULL REFERENCES tokens(id) ON DELETE CASCADE,
		section_id INTEGER NOT NULL REFERENCES sections(id) ON DELETE CASCADE,
		PRIMARY KEY (token_id, section_id)
	)`,

	// What last signed in with a token, as the client described itself. Shown
	// beside the label a human typed so a list of credentials reads as a list
	// of devices; never consulted to decide anything, because a client writes
	// this field itself.
	`ALTER TABLE tokens ADD COLUMN device TEXT NOT NULL DEFAULT ''`,

	// The token a browser session was born from, so the web interface sees
	// exactly what the credential it was handed may see. Zero for the
	// sessions that predate this column, which are therefore unrestricted —
	// the same answer they were already getting.
	`ALTER TABLE sessions ADD COLUMN token_id INTEGER NOT NULL DEFAULT 0`,

	// Which libraries an account may see, whatever it signs in from.
	//
	// The companion to token_sections and the same rule: no row means no
	// restriction. The two are combined rather than ranked — a device reaches
	// a library only if both its account and its own grants allow it — so an
	// account narrowed here cannot be widened by handing it a token with
	// different grants.
	`CREATE TABLE user_sections (
		user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		section_id INTEGER NOT NULL REFERENCES sections(id) ON DELETE CASCADE,
		PRIMARY KEY (user_id, section_id)
	)`,

	// What somebody marked. Per account rather than per server, because
	// whether a film is a favourite is an opinion and two people in a
	// household are allowed different ones.
	`CREATE TABLE favourites (
		user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		item_id    INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
		created_at INTEGER NOT NULL,
		PRIMARY KEY (user_id, item_id)
	)`,
	`CREATE INDEX favourites_recent ON favourites(user_id, created_at DESC)`,

	// A playlist belongs to one account and is ordered by hand.
	//
	// kind is what the client's Playlists tab filters on — a music client
	// asking for audio playlists should not be shown a list of films. It is
	// decided by what goes in rather than declared, and a list holding both
	// is "mixed", which is a real answer rather than a failure to choose.
	`CREATE TABLE playlists (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		title      TEXT NOT NULL,
		kind       TEXT NOT NULL DEFAULT 'mixed',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`,
	`CREATE INDEX playlists_owner ON playlists(user_id, title)`,

	// An entry rather than a membership: the same track twice in one playlist
	// is two entries, which is what a playlist is for. position is a sparse
	// ordering so a move rewrites one row rather than all of them.
	`CREATE TABLE playlist_entries (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		playlist_id INTEGER NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
		item_id     INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
		position    INTEGER NOT NULL
	)`,
	`CREATE INDEX playlist_entries_order ON playlist_entries(playlist_id, position, id)`,

	// A collection is the library's, not a person's: everybody who can see
	// the films in it sees the same grouping, which is what makes it a
	// property of the library rather than an opinion about it.
	`CREATE TABLE collections (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		title      TEXT NOT NULL,
		sort_title TEXT NOT NULL,
		summary    TEXT NOT NULL DEFAULT '',
		poster_key TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`,
	`CREATE UNIQUE INDEX collections_title ON collections(sort_title)`,

	`CREATE TABLE collection_items (
		collection_id INTEGER NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
		item_id       INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
		position      INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (collection_id, item_id)
	)`,
	`CREATE INDEX collection_items_item ON collection_items(item_id)`,

	// Which cut of a film this file is: the Director's Cut, the Theatrical,
	// the Extended. On the part rather than the item because that is what it
	// describes — two editions are one film with two files, which is the
	// whole point of reading it.
	`ALTER TABLE parts ADD COLUMN edition TEXT NOT NULL DEFAULT ''`,

	// What the scan found, kept beside what is displayed, so a title can be
	// corrected by hand without the next scan undoing it.
	//
	// An item's identity is the title and year the filename gave, which is
	// what makes a rescan idempotent — so editing the title in place would
	// make the next scan fail to recognise the row and insert a second one
	// beside it, with the file moving to the new row and the correction
	// orphaned. Holding the scanned values separately costs two columns and
	// a rebuilt index, and nothing in the read path changes: title is still
	// title.
	`ALTER TABLE items ADD COLUMN scanned_title TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE items ADD COLUMN scanned_year INTEGER NOT NULL DEFAULT 0`,
	`UPDATE items SET scanned_title = title, scanned_year = year`,
	`DROP INDEX items_identity`,
	`CREATE UNIQUE INDEX items_identity ON items(section_id, kind, IFNULL(parent_id, 0), scanned_title, scanned_year, idx, disc)`,

	// Which sort of extra an item is: a trailer, a deleted scene, a
	// featurette. Empty for everything that is not one.
	//
	// On the item rather than the part because an extra is a thing with its
	// own title, duration and position beside what it belongs to — and
	// because every listing has to leave extras out, which is a predicate on
	// a column and not a join.
	`ALTER TABLE items ADD COLUMN extra TEXT NOT NULL DEFAULT ''`,
	`CREATE INDEX items_extras ON items(parent_id, extra)`,

	// Whether somebody has set this item's details by hand.
	//
	// A locked item is left alone by the enrichment pass, because a provider
	// that overwrites a correction every night is worse than no provider.
	// One flag for the item rather than one per field: the thing a person
	// does here is fix an item, and a half-locked row is a rule nobody can
	// predict from the page.
	`ALTER TABLE items ADD COLUMN locked INTEGER NOT NULL DEFAULT 0`,

	// What an item is rated, and the age that rating stands for.
	//
	// Two columns because they answer two questions: the label is what a
	// client shows ("PG-13", "15"), and the age is what a restriction
	// compares against — one number, so a ceiling works across the half
	// dozen national systems a mixed library carries. -1 is "nobody has
	// said", which is every item until a metadata pass reaches it.
	`ALTER TABLE items ADD COLUMN rating TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE items ADD COLUMN rating_age INTEGER NOT NULL DEFAULT -1`,
	`CREATE INDEX items_rating ON items(rating_age)`,

	// What an account may watch, and how much of the server it may use.
	//
	// max_rating_age is a ceiling in years and -1 means none, which is what
	// every account has until somebody says otherwise. allow_unrated
	// decides what happens to the things nobody has rated: allowed, which
	// keeps a half-enriched library watchable, or hidden, which is what
	// somebody who wants a child's account to be predictable asks for.
	//
	// pin_hash is how a restriction is lifted for an evening without the
	// administrator being in the room. max_streams and max_bitrate are what
	// one account may take of a machine the whole household shares; zero
	// means no limit.
	`ALTER TABLE users ADD COLUMN max_rating_age INTEGER NOT NULL DEFAULT -1`,
	`ALTER TABLE users ADD COLUMN allow_unrated INTEGER NOT NULL DEFAULT 1`,
	`ALTER TABLE users ADD COLUMN pin_hash BLOB`,
	`ALTER TABLE users ADD COLUMN max_streams INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE users ADD COLUMN max_bitrate INTEGER NOT NULL DEFAULT 0`,

	// When a rating ceiling was lifted with a PIN, and until when.
	//
	// On the session rather than in memory so it survives a page reload, and
	// with an expiry rather than a flag so an evening's unlock does not
	// become a permanent one: whoever typed the PIN was unlocking tonight.
	`ALTER TABLE sessions ADD COLUMN unlocked_until INTEGER NOT NULL DEFAULT 0`,

	// Where to tell when something happens.
	//
	// The server cannot push anything to a client, so anything that wants to
	// react to a play or a new episode has to be told over HTTP. One row is
	// one destination: an address, the shape to send it in, and which events
	// it cares about.
	//
	// token and secret are stored as they were given, because they have to be
	// sent: a bearer token the receiver checks, and a secret the signature is
	// computed with. They are credentials for somebody else's service, so
	// nothing reads them back out for display.
	//
	// last_status, last_error and failures are the only history kept. A log
	// of every delivery would grow without bound for no one's benefit; what
	// an operator needs is whether this destination is working now.
	`CREATE TABLE webhooks (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		url         TEXT NOT NULL,
		format      TEXT NOT NULL DEFAULT 'json',
		events      TEXT NOT NULL DEFAULT '',
		token       TEXT NOT NULL DEFAULT '',
		secret      TEXT NOT NULL DEFAULT '',
		enabled     INTEGER NOT NULL DEFAULT 1,
		created_at  INTEGER NOT NULL,
		last_at     INTEGER NOT NULL DEFAULT 0,
		last_status INTEGER NOT NULL DEFAULT 0,
		last_error  TEXT NOT NULL DEFAULT '',
		failures    INTEGER NOT NULL DEFAULT 0
	)`,
	// What was watched, by whom, on what, for how long.
	//
	// Every column that names something is written here as well as pointed
	// at: the title, the viewer's name, the device. A log that joined the
	// library would become unreadable exactly when it is most wanted —
	// after a film was removed, an account deleted, a title corrected — and
	// "something, by someone, in March" is not an answer. The ids are kept
	// beside the names so a row can still be followed to what it was about
	// while that still exists, and give way rather than take the row with
	// them when it does not.
	//
	// ended_at is always set, and is the last moment the stream was known to
	// be playing rather than the moment it stopped. complete says which of
	// those it is: a stream the server watched end, or one it lost to a
	// restart. That way a crash costs a minute of one row and never leaves
	// an open one to be swept up later.
	`CREATE TABLE history (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id     INTEGER REFERENCES users(id) ON DELETE SET NULL,
		user_name   TEXT NOT NULL DEFAULT '',
		item_id     INTEGER REFERENCES items(id) ON DELETE SET NULL,
		title       TEXT NOT NULL DEFAULT '',
		subtitle    TEXT NOT NULL DEFAULT '',
		kind        TEXT NOT NULL DEFAULT '',
		surface     TEXT NOT NULL DEFAULT '',
		client      TEXT NOT NULL DEFAULT '',
		device      TEXT NOT NULL DEFAULT '',
		address     TEXT NOT NULL DEFAULT '',
		mode        TEXT NOT NULL DEFAULT '',
		reason      TEXT NOT NULL DEFAULT '',
		started_at  INTEGER NOT NULL,
		ended_at    INTEGER NOT NULL,
		paused_ms   INTEGER NOT NULL DEFAULT 0,
		position_ms INTEGER NOT NULL DEFAULT 0,
		duration_ms INTEGER NOT NULL DEFAULT 0,
		complete    INTEGER NOT NULL DEFAULT 0
	)`,
	// The three ways history is ever read: newest first, one viewer's, and
	// one thing's.
	`CREATE INDEX history_started ON history(started_at DESC)`,
	`CREATE INDEX history_user ON history(user_id, started_at DESC)`,
	`CREATE INDEX history_item ON history(item_id, started_at DESC)`,

	// What a smart collection asks of the library, as JSON, and empty for a
	// collection somebody filled by hand.
	//
	// One column rather than a flag and a rule table. A flag can disagree
	// with the rules it describes, and a rule table buys nothing: the rules
	// are only ever read and written whole, by the editor that wrote them.
	// Empty or not is the whole of the distinction between the two kinds of
	// collection, and a smart one has no rows in collection_items at all.
	`ALTER TABLE collections ADD COLUMN rules TEXT NOT NULL DEFAULT ''`,

	// What a thing is about, as the words a provider or a file's own tags use.
	//
	// A table of names and a joining table rather than a column of text,
	// because a genre is asked two questions a delimited string cannot answer
	// cheaply: which things are this genre, and what genres does this library
	// have. Both are an index lookup here and a scan of every row there.
	//
	// The name is unique without regard to case, because the same genre
	// arrives spelled differently from different places — a provider's
	// "Science Fiction" and a file tag's "science fiction" are one shelf, and
	// two rows would make it two.
	`CREATE TABLE genres (
		id   INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL COLLATE NOCASE UNIQUE
	)`,
	`CREATE TABLE item_genres (
		item_id  INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
		genre_id INTEGER NOT NULL REFERENCES genres(id) ON DELETE CASCADE,
		PRIMARY KEY (item_id, genre_id)
	)`,
	// The other direction: everything of one genre, which is what a client's
	// filter asks and what the primary key above cannot answer.
	`CREATE INDEX item_genres_genre ON item_genres(genre_id)`,

	// What one library wants done to it, as JSON, and empty for the defaults.
	//
	// One column for the same reasons a smart collection's rules are one: the
	// settings are only ever read and written whole, by the page that writes
	// them, and a column per setting would be a migration for every new one.
	// Empty means every default, so a library that existed before this
	// column behaves exactly as it did.
	`ALTER TABLE sections ADD COLUMN settings TEXT NOT NULL DEFAULT ''`,

	// When a library was last looked at by the scheduler, as opposed to
	// scanned_at, which is when a scan last finished.
	//
	// Separate because they answer different questions and a scan that found
	// nothing still counts as a visit: without this, a library whose scan
	// fails every time would be retried in a tight loop, and one whose scan
	// is slower than its interval would be started again while it ran.
	`ALTER TABLE sections ADD COLUMN swept_at INTEGER NOT NULL DEFAULT 0`,

	// Genres turn out to have been one case of a general thing.
	//
	// A genre, a tag, a label, a mood and a style are all the same shape: a
	// word, a set of things carrying it, and a client that wants the list of
	// words in use. They differ only in who writes them and what a client
	// calls them. So there is one pair of tables with a kind on it, rather
	// than four more pairs that would each need their own batch read, their
	// own listing and their own filter.
	//
	// New tables and a copy rather than a rename, because the genres table
	// declared its name UNIQUE inline and SQLite cannot drop a constraint
	// that was written into a column definition. Identity has to become the
	// kind and the name together — otherwise "Comedy" the genre and
	// "Comedy" the label would be one row, and a correction would silently
	// edit a provider's answer. The old tables are copied across and
	// dropped, child first so nothing references a table that is going.
	`CREATE TABLE tags (
		id   INTEGER PRIMARY KEY AUTOINCREMENT,
		kind TEXT NOT NULL,
		name TEXT NOT NULL COLLATE NOCASE
	)`,
	`CREATE UNIQUE INDEX tags_identity ON tags(kind, name)`,
	`CREATE TABLE item_tags (
		item_id INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
		tag_id  INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
		PRIMARY KEY (item_id, tag_id)
	)`,
	// The other direction: everything carrying one word, which is what a
	// client's filter asks and what the primary key above cannot answer.
	`CREATE INDEX item_tags_tag ON item_tags(tag_id)`,

	// The ids are carried over, so the joining rows copy across unchanged
	// and a client holding a genre's id from before still resolves it.
	`INSERT INTO tags (id, kind, name) SELECT id, 'genre', name FROM genres`,
	`INSERT INTO item_tags (item_id, tag_id) SELECT item_id, genre_id FROM item_genres`,
	`DROP TABLE item_genres`,
	`DROP TABLE genres`,

	// Who made a thing and who is in it.
	//
	// Not the tags table, although the shape rhymes. A person has a part as
	// well as a name — "Amy Adams as Louise Banks" — and a part belongs to
	// the pairing rather than to either side, so the joining row carries
	// it. A tag has nothing of its own to say.
	//
	// One row per person per name, shared across the library, so an actor in
	// forty films is one row and a client asking "what else are they in" is
	// an index lookup. The name is unique without regard to case for the
	// same reason a genre's is: two spellings of one person is two people
	// everywhere it matters.
	`CREATE TABLE people (
		id        INTEGER PRIMARY KEY AUTOINCREMENT,
		name      TEXT NOT NULL COLLATE NOCASE UNIQUE,
		thumb_key TEXT NOT NULL DEFAULT ''
	)`,

	// role is what they did: a part, or the job they are credited with.
	// position is where they came in the provider's own ordering, which is
	// billing order for a cast and is the only thing that makes the first
	// four names the ones worth showing.
	`CREATE TABLE item_people (
		item_id   INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
		person_id INTEGER NOT NULL REFERENCES people(id) ON DELETE CASCADE,
		role      TEXT NOT NULL,
		part      TEXT NOT NULL DEFAULT '',
		position  INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (item_id, person_id, role, part)
	)`,
	// The other direction: everything one person worked on.
	`CREATE INDEX item_people_person ON item_people(person_id, role)`,
	// And the order a credit list is read in.
	`CREATE INDEX item_people_item ON item_people(item_id, role, position)`,

	// A studio is not a person: it has no part and no billing order, and
	// Plex carries it as an attribute rather than a tag element. So it is a
	// column, and a film has one.
	`ALTER TABLE items ADD COLUMN studio TEXT NOT NULL DEFAULT ''`,

	// When an episode first went out, which is the only fact that can place
	// a special among the seasons without anybody being asked.
	//
	// A provider sends this on every episode and BMS was reading the year
	// off it and discarding the rest. Zero means nobody has said.
	`ALTER TABLE items ADD COLUMN aired INTEGER NOT NULL DEFAULT 0`,

	// Where a special belongs, as opposed to when it aired.
	//
	// The three fields TheTVDB records on a special and every client reads:
	// after which season, before which season, and before which episode of
	// it. Minus one for "nobody said", because zero is a real answer —
	// before season zero is meaningless but before episode zero is not, and
	// a sentinel that collides with a real value is the bug this file
	// already has one of.
	//
	// Columns rather than JSON because they are read to sort by.
	`ALTER TABLE items ADD COLUMN airs_after_season INTEGER NOT NULL DEFAULT -1`,
	`ALTER TABLE items ADD COLUMN airs_before_season INTEGER NOT NULL DEFAULT -1`,
	`ALTER TABLE items ADD COLUMN airs_before_episode INTEGER NOT NULL DEFAULT -1`,

	// Whether somebody placed this special by hand.
	//
	// Separate from the item lock, which stops a metadata pass touching
	// anything: placing one special should not freeze its title and its
	// artwork too. A pass writes placement only where this is zero.
	`ALTER TABLE items ADD COLUMN placed_by_hand INTEGER NOT NULL DEFAULT 0`,

	// The account a cloud identity maps to, and whether an account may be
	// used at all. See docs/CLOUD-ACCOUNTS.md.
	//
	// A cloud identity never arrives as an administrator and never arrives
	// able to watch anything: the first assertion from a new subject creates
	// an account here, inactive, and an administrator decides what it may
	// reach. Active defaults to 1 so every account that already exists is
	// unaffected -- they were created by an administrator, which is the
	// approval this column records.
	`ALTER TABLE users ADD COLUMN cloud_subject TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE users ADD COLUMN active INTEGER NOT NULL DEFAULT 1`,
	// One local account per cloud identity. Partial, because the empty string
	// is every ordinary account and they must not collide with each other.
	`CREATE UNIQUE INDEX users_cloud_subject ON users(cloud_subject) WHERE cloud_subject <> ''`,
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
