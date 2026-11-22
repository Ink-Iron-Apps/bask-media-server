package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// This file is what the admin interface needs and nothing else does: browser
// sessions, the write side of accounts and libraries, and the counts a status
// page shows.
//
// Sessions are deliberately not tokens. A token is a long-lived credential a
// media client keeps forever; a browser session expires, is revoked by signing
// out, and is recorded separately so that revoking one does not log a
// television out of the library.

// Settings the server keeps for itself, in the server table.
//
// Named here rather than where they are read, because more than one package
// reads them — the Plex surface shows the name, a background pass needs the
// key, the admin interface writes both — and a key spelled two ways is a
// setting that silently does nothing.
const (
	// KeyFriendlyName is the name clients show for this server.
	KeyFriendlyName = "friendly_name"
	// KeyTMDB is the metadata provider's key, stored so it survives a restart
	// and can be set without one.
	KeyTMDB = "tmdb_key"
	// KeyOpenSubtitles is the subtitle service's key. Empty means subtitle
	// searching is off, which is the default: nothing about a library
	// reaches a third party until somebody puts a key here.
	KeyOpenSubtitles = "opensubtitles_key"
)

// SessionTTL is how long a browser stays signed in without asking again.
//
// Long enough that administering a server does not mean signing in every time,
// short enough that a forgotten session on a shared machine stops working.
const SessionTTL = 14 * 24 * time.Hour

// CreateSession starts a browser session for a user and returns its secret
// once. Only the hash is stored, for the same reason tokens are hashed.
//
// tokenID is the credential the session was signed in with, and it is kept so
// the session can see exactly what that credential may see. Zero where there
// was none, which is unrestricted.
func (d *DB) CreateSession(ctx context.Context, userID, tokenID int64, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = SessionTTL
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate session: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(secret))

	now := time.Now().UTC()
	if _, err := d.sql.ExecContext(ctx,
		`INSERT INTO sessions (user_id, token_id, hash, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		userID, tokenID, sum[:], millis(now), millis(now.Add(ttl))); err != nil {
		return "", fmt.Errorf("store session: %w", err)
	}
	return secret, nil
}

// SessionCredential resolves a session secret to its account and the libraries
// the token it was signed in with may reach, or ErrNotFound when the session
// is unknown or has expired.
//
// A browser inherits its token's restriction rather than having one of its
// own. Handing a device a narrowed token and then letting the same token open
// the whole library through a web page would be a restriction with a door
// beside it.
func (d *DB) SessionCredential(ctx context.Context, secret string) (*domain.Credential, error) {
	if secret == "" {
		return nil, ErrNotFound
	}
	sum := sha256.Sum256([]byte(secret))

	var (
		u                    domain.User
		sessionID, tokenID   int64
		createdAt, expiresAt int64
	)
	err := d.sql.QueryRowContext(ctx,
		`SELECT s.id, s.token_id, s.expires_at, u.id, u.username, u.admin, u.created_at
		   FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.hash = ?`, sum[:]).
		Scan(&sessionID, &tokenID, &expiresAt, &u.ID, &u.Username, &u.Admin, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("resolve session: %w", err)
	}
	if time.Now().UTC().After(fromMillis(expiresAt)) {
		// Expired sessions are deleted on sight rather than swept on a timer:
		// the row is already in hand, and nothing else has to remember to run.
		_, _ = d.sql.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, sessionID)
		return nil, ErrNotFound
	}
	u.CreatedAt = fromMillis(createdAt)

	open := d.unscoped()
	var granted []int64
	if tokenID != 0 {
		granted, err = open.TokenSections(ctx, tokenID)
		if err != nil {
			return nil, err
		}
	}
	account, err := open.UserSections(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	sections, restricted := narrow(account, granted)
	cred := &domain.Credential{
		User: u, TokenID: tokenID, Restricted: restricted, Sections: sections,
	}

	_, _ = d.sql.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE id = ?`,
		millis(time.Now().UTC()), sessionID)
	return cred, nil
}

// SessionUser resolves a session secret to its user, for the callers that only
// need to know who it is.
func (d *DB) SessionUser(ctx context.Context, secret string) (*domain.User, error) {
	cred, err := d.SessionCredential(ctx, secret)
	if err != nil {
		return nil, err
	}
	return &cred.User, nil
}

// DeleteSession signs one browser out.
func (d *DB) DeleteSession(ctx context.Context, secret string) error {
	if secret == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(secret))
	if _, err := d.sql.ExecContext(ctx, `DELETE FROM sessions WHERE hash = ?`, sum[:]); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// SweepSessions removes expired sessions, which keeps the table bounded by the
// number of people who sign in rather than by how often they do.
func (d *DB) SweepSessions(ctx context.Context) (int, error) {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, millis(time.Now().UTC()))
	if err != nil {
		return 0, fmt.Errorf("sweep sessions: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// Users lists every account, oldest first.
func (d *DB) Users(ctx context.Context) ([]domain.User, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT id, username, admin, created_at FROM users ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var out []domain.User
	for rows.Next() {
		var (
			u         domain.User
			createdAt int64
		)
		if err := rows.Scan(&u.ID, &u.Username, &u.Admin, &createdAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		u.CreatedAt = fromMillis(createdAt)
		out = append(out, u)
	}
	return out, rows.Err()
}

// User reads one account.
func (d *DB) User(ctx context.Context, id int64) (*domain.User, error) {
	var (
		u         domain.User
		createdAt int64
	)
	err := d.sql.QueryRowContext(ctx,
		`SELECT id, username, admin, created_at FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Username, &u.Admin, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read user: %w", err)
	}
	u.CreatedAt = fromMillis(createdAt)
	return &u, nil
}

// SetUserAdmin grants or withdraws administration.
func (d *DB) SetUserAdmin(ctx context.Context, id int64, admin bool) error {
	res, err := d.sql.ExecContext(ctx, `UPDATE users SET admin = ? WHERE id = ?`, admin, id)
	if err != nil {
		return fmt.Errorf("set admin: %w", err)
	}
	return affected(res, "user")
}

// DeleteUser removes an account along with its tokens, sessions and viewing
// history, which the foreign keys do: see the pragmas in [dsn] for why that
// cascade is actually switched on.
func (d *DB) DeleteUser(ctx context.Context, id int64) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	return affected(res, "user")
}

// CountAdmins reports how many accounts can administer the server, which is
// what stops the last one being deleted or demoted.
func (d *DB) CountAdmins(ctx context.Context) (int, error) {
	var n int
	if err := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE admin = 1`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count admins: %w", err)
	}
	return n, nil
}

// Tokens lists one user's credentials. The hash comes back because it is the
// row's identity in the database; nothing outside this package should present
// it, and the admin API does not.
func (d *DB) Tokens(ctx context.Context, userID int64) ([]domain.Token, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT id, user_id, label, hash, created_at, last_seen_at, device
		   FROM tokens WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()

	var out []domain.Token
	for rows.Next() {
		var (
			t                     domain.Token
			createdAt, lastSeenAt int64
		)
		if err := rows.Scan(&t.ID, &t.UserID, &t.Label, &t.Hash, &createdAt, &lastSeenAt, &t.Device); err != nil {
			return nil, fmt.Errorf("scan token: %w", err)
		}
		t.CreatedAt, t.LastSeenAt = fromMillis(createdAt), fromMillis(lastSeenAt)
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken deletes one credential. The client holding it is signed out the
// next time it asks for anything.
func (d *DB) RevokeToken(ctx context.Context, id int64) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM tokens WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	return affected(res, "token")
}

// UpdateSection renames a library or changes where it looks.
//
// Changing the roots does not move anything: the next scan finds what is there
// now and prunes what is not, which is the same path a disk that was unplugged
// takes.
func (d *DB) UpdateSection(ctx context.Context, id int64, title string, roots []string) error {
	now := time.Now().UTC()
	res, err := d.sql.ExecContext(ctx,
		`UPDATE sections SET title = ?, roots = ?, updated_at = ? WHERE id = ?`,
		title, strings.Join(roots, "\n"), millis(now), id)
	if err != nil {
		return fmt.Errorf("update section: %w", err)
	}
	return affected(res, "section")
}

// DeleteSection removes a library and everything scanned into it. No file on
// disk is touched: this forgets a library, it does not delete media.
func (d *DB) DeleteSection(ctx context.Context, id int64) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM sections WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete section: %w", err)
	}
	return affected(res, "section")
}

func affected(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Stats is what the server knows about itself, in one query apiece.
//
// Counts rather than rows: a status page that listed the library would be the
// one request in the server that reads it all into memory.
type Stats struct {
	Movies   int `json:"movies"`
	Shows    int `json:"shows"`
	Seasons  int `json:"seasons"`
	Episodes int `json:"episodes"`

	Artists int `json:"artists"`
	Albums  int `json:"albums"`
	Tracks  int `json:"tracks"`

	PhotoAlbums int `json:"photoAlbums"`
	Photos      int `json:"photos"`

	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`

	Markers       int `json:"markers"`
	Chapters      int `json:"chapters"`
	MarkedItems   int `json:"markedItems"`
	Users         int `json:"users"`
	Tokens        int `json:"tokens"`
	Sessions      int `json:"sessions"`
	WithoutArt    int `json:"withoutArt"`
	NeedsMetadata int `json:"needsMetadata"`
	NeedsMarkers  int `json:"needsMarkers"`
}

// Stats collects the counts a status page shows.
func (d *DB) Stats(ctx context.Context) (Stats, error) {
	var s Stats

	byKind := map[domain.Kind]*int{
		domain.KindMovie:   &s.Movies,
		domain.KindShow:    &s.Shows,
		domain.KindSeason:  &s.Seasons,
		domain.KindEpisode: &s.Episodes,

		domain.KindArtist: &s.Artists,
		domain.KindAlbum:  &s.Albums,
		domain.KindTrack:  &s.Tracks,

		domain.KindPhotoAlbum: &s.PhotoAlbums,
		domain.KindPhoto:      &s.Photos,
	}
	rows, err := d.sql.QueryContext(ctx, `SELECT kind, COUNT(*) FROM items GROUP BY kind`)
	if err != nil {
		return s, fmt.Errorf("count items: %w", err)
	}
	for rows.Next() {
		var (
			kind string
			n    int
		)
		if err := rows.Scan(&kind, &n); err != nil {
			rows.Close()
			return s, fmt.Errorf("count items: %w", err)
		}
		if into, ok := byKind[domain.Kind(kind)]; ok {
			*into = n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return s, err
	}

	singles := []struct {
		query string
		into  any
	}{
		{`SELECT COUNT(*), IFNULL(SUM(size), 0) FROM parts`, nil}, // handled below
		{`SELECT COUNT(*) FROM markers`, &s.Markers},
		{`SELECT COUNT(DISTINCT item_id) FROM markers`, &s.MarkedItems},
		{`SELECT COUNT(*) FROM chapters`, &s.Chapters},
		{`SELECT COUNT(*) FROM users`, &s.Users},
		{`SELECT COUNT(*) FROM tokens`, &s.Tokens},
		{`SELECT COUNT(*) FROM sessions`, &s.Sessions},
		{`SELECT COUNT(*) FROM items WHERE poster_key = '' AND kind IN ('movie', 'show', 'album')`, &s.WithoutArt},
		{`SELECT COUNT(*) FROM items WHERE metadata_at = 0 AND kind IN ('movie', 'show', 'episode')`, &s.NeedsMetadata},
		{`SELECT COUNT(*) FROM items WHERE kind = 'season' AND markers_at = 0`, &s.NeedsMarkers},
	}
	if err := d.sql.QueryRowContext(ctx, singles[0].query).Scan(&s.Files, &s.Bytes); err != nil {
		return s, fmt.Errorf("count parts: %w", err)
	}
	for _, q := range singles[1:] {
		if err := d.sql.QueryRowContext(ctx, q.query).Scan(q.into); err != nil {
			return s, fmt.Errorf("count: %w", err)
		}
	}
	return s, nil
}

// SectionStats is one library's size, for the row that describes it.
type SectionStats struct {
	// Items is what the library's top level holds: films, shows, artists or
	// albums of photographs.
	Items int `json:"items"`

	// Leaves is what is under it and playable: episodes, tracks or
	// photographs. One field for all three because the row reads the same
	// either way — "12 shows, 240 episodes", "40 artists, 900 tracks".
	Leaves   int   `json:"leaves"`
	Episodes int   `json:"episodes"` // kept for a client reading the older name
	Files    int   `json:"files"`
	Bytes    int64 `json:"bytes"`
}

// SectionStats counts each library in one pass, keyed by section id.
func (d *DB) SectionStats(ctx context.Context) (map[int64]SectionStats, error) {
	out := map[int64]SectionStats{}

	rows, err := d.sql.QueryContext(ctx, `SELECT section_id, kind, COUNT(*) FROM items GROUP BY section_id, kind`)
	if err != nil {
		return nil, fmt.Errorf("count section items: %w", err)
	}
	for rows.Next() {
		var (
			id   int64
			kind string
			n    int
		)
		if err := rows.Scan(&id, &kind, &n); err != nil {
			rows.Close()
			return nil, fmt.Errorf("count section items: %w", err)
		}
		stats := out[id]
		switch domain.Kind(kind) {
		case domain.KindMovie, domain.KindShow, domain.KindArtist:
			stats.Items += n
		case domain.KindPhotoAlbum:
			// Every level of a photo tree is an album, and the count a person
			// wants is how many albums there are, not how deep they nest.
			stats.Items += n
		case domain.KindEpisode, domain.KindTrack, domain.KindPhoto:
			stats.Leaves += n
		}
		if domain.Kind(kind) == domain.KindEpisode {
			stats.Episodes += n
		}
		out[id] = stats
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = d.sql.QueryContext(ctx,
		`SELECT i.section_id, COUNT(*), IFNULL(SUM(p.size), 0)
		   FROM parts p JOIN items i ON i.id = p.item_id GROUP BY i.section_id`)
	if err != nil {
		return nil, fmt.Errorf("count section files: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id    int64
			files int
			bytes int64
		)
		if err := rows.Scan(&id, &files, &bytes); err != nil {
			return nil, fmt.Errorf("count section files: %w", err)
		}
		stats := out[id]
		stats.Files, stats.Bytes = files, bytes
		out[id] = stats
	}
	return out, rows.Err()
}
