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

// CreateUser adds an account.
func (d *DB) CreateUser(ctx context.Context, username string, admin bool) (*domain.User, error) {
	now := time.Now().UTC()
	res, err := d.sql.ExecContext(ctx,
		`INSERT INTO users (username, admin, created_at) VALUES (?, ?, ?)`, username, admin, millis(now))
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	// Active: an administrator making an account is the approval that the
	// `active` column exists to record for the accounts nobody approved.
	return &domain.User{ID: id, Username: username, Admin: admin, CreatedAt: now, Active: true}, nil
}

// UserForCloudSubject resolves an account id the account service signs for to
// the local account it maps to, or [ErrNotFound] the first time one turns up.
func (d *DB) UserForCloudSubject(ctx context.Context, subject string) (*domain.User, error) {
	if subject == "" {
		return nil, ErrNotFound
	}
	var (
		u         domain.User
		createdAt int64
	)
	err := d.sql.QueryRowContext(ctx,
		`SELECT id, username, admin, created_at, cloud_subject, active FROM users WHERE cloud_subject = ?`, subject).
		Scan(&u.ID, &u.Username, &u.Admin, &createdAt, &u.CloudSubject, &u.Active)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("resolve cloud subject: %w", err)
	}
	u.CreatedAt = fromMillis(createdAt)
	return &u, nil
}

// CreateCloudUser records a local account for a cloud identity that has
// presented a verified assertion for the first time.
//
// Never an administrator and never active: an assertion says who somebody is,
// and what they may reach here is the administrator's decision, made on this
// machine. The username is taken from the assertion only as a starting label
// and is made unique against what already exists, because two people with the
// same display name must not collide into one account.
func (d *DB) CreateCloudUser(ctx context.Context, subject, name, email string) (*domain.User, error) {
	if subject == "" {
		return nil, errors.New("a cloud account needs a subject")
	}
	now := time.Now().UTC()
	for attempt := 0; attempt < 50; attempt++ {
		username := candidateUsername(name, email, subject, attempt)
		res, err := d.sql.ExecContext(ctx,
			`INSERT INTO users (username, admin, created_at, cloud_subject, active) VALUES (?, 0, ?, ?, 0)`,
			username, millis(now), subject)
		if err != nil {
			// A clash on the username is the one error worth another go: the
			// unique index on cloud_subject means a clash there is a race
			// with another request for the same person, and the caller should
			// see that as the account it is.
			if isUniqueViolation(err) && !strings.Contains(err.Error(), "cloud_subject") {
				continue
			}
			return nil, fmt.Errorf("create cloud user: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("create cloud user: %w", err)
		}
		return &domain.User{
			ID: id, Username: username, Admin: false, CreatedAt: now,
			CloudSubject: subject, Active: false,
		}, nil
	}
	return nil, errors.New("could not find a free username for a cloud account")
}

// candidateUsername is the label a cloud account starts with.
//
// The display name first because it is what the administrator will recognise
// in the list, then the local part of the email, then the subject -- which is
// opaque but always there, so there is no case where this produces nothing.
func candidateUsername(name, email, subject string, attempt int) string {
	base := strings.TrimSpace(name)
	if base == "" {
		if local, _, found := strings.Cut(email, "@"); found {
			base = strings.TrimSpace(local)
		}
	}
	if base == "" {
		base = "cloud-" + subject
	}
	if len(base) > 48 {
		base = base[:48]
	}
	if attempt == 0 {
		return base
	}
	return fmt.Sprintf("%s (%d)", base, attempt+1)
}

func isUniqueViolation(err error) bool {
	// The driver is pure Go and reports this in its message rather than as a
	// typed code, so the message is what there is to read.
	return strings.Contains(strings.ToLower(err.Error()), "unique")
}

// CredentialForUser builds a credential for an account that authenticated
// without a token row -- which is what a cloud assertion is.
//
// The same grants an ordinary credential gets, read the same way, so a cloud
// identity reaches exactly what the administrator granted its account and
// nothing is special-cased into a wider view. An account that is not active
// resolves to [ErrNotFound]: it exists to be granted libraries, not to watch.
func (d *DB) CredentialForUser(ctx context.Context, userID int64) (*domain.Credential, error) {
	var (
		u            domain.User
		createdAt    int64
		ceiling      int
		allowUnrated bool
		maxStreams   int
		maxBitRate   int64
	)
	err := d.sql.QueryRowContext(ctx,
		`SELECT id, username, admin, created_at, cloud_subject, active,
		        max_rating_age, allow_unrated, max_streams, max_bitrate
		   FROM users WHERE id = ?`, userID).
		Scan(&u.ID, &u.Username, &u.Admin, &createdAt, &u.CloudSubject, &u.Active,
			&ceiling, &allowUnrated, &maxStreams, &maxBitRate)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("resolve account: %w", err)
	}
	if !u.Active {
		return nil, ErrNotFound
	}
	u.CreatedAt = fromMillis(createdAt)

	// Unscoped for the same reason [Credential] gives: a narrowed handle
	// resolving a credential would filter the rows that decide the narrowing.
	open := d.unscoped()
	account, err := open.UserSections(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	// No token row, so there is no device grant to combine: the account's own
	// grants are the whole of it. TokenID stays zero, which is what every
	// caller that notes a device checks for.
	sections, restricted := narrow(account, nil)
	return &domain.Credential{
		User: u, TokenID: 0, Restricted: restricted, Sections: sections,
		Ceiling: ceiling, AllowUnrated: allowUnrated,
		MaxStreams: maxStreams, MaxBitRate: maxBitRate,
	}, nil
}

// CountUsers reports how many accounts exist, which is how first run is
// detected without a separate flag to get out of step.
func (d *DB) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

// IssueToken mints a credential for a user and returns it once, in the clear.
//
// Only the hash is stored, so this value cannot be recovered later: a leaked
// database is not a leaked set of working tokens. 32 bytes from crypto/rand is
// well past guessing, and the base64 form is safe in both a header and a query
// string, which is where Plex clients put it.
func (d *DB) IssueToken(ctx context.Context, userID int64, label string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))

	now := time.Now().UTC()
	if _, err := d.sql.ExecContext(ctx,
		`INSERT INTO tokens (user_id, label, hash, created_at) VALUES (?, ?, ?, ?)`,
		userID, label, sum[:], millis(now)); err != nil {
		return "", fmt.Errorf("store token: %w", err)
	}
	return token, nil
}

// Credential resolves a presented token into the account it belongs to and the
// libraries it may reach, or ErrNotFound.
//
// The lookup is by hash, so a token is never compared in the clear and never
// logged. last_seen_at is updated on a best-effort basis: it is diagnostics,
// not authorisation, and a write failure must not refuse a valid request.
//
// One statement for the account and one for the grants, rather than a join
// that would repeat the account per granted library. The second runs only for
// a token that has grants at all, which is none of them until somebody
// narrows one.
func (d *DB) Credential(ctx context.Context, token string) (*domain.Credential, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	sum := sha256.Sum256([]byte(token))

	var (
		u            domain.User
		tokenID      int64
		createdAt    int64
		ceiling      int
		allowUnrated bool
		maxStreams   int
		maxBitRate   int64
	)
	err := d.sql.QueryRowContext(ctx,
		`SELECT t.id, u.id, u.username, u.admin, u.created_at, u.cloud_subject, u.active,
		        u.max_rating_age, u.allow_unrated, u.max_streams, u.max_bitrate
		   FROM tokens t JOIN users u ON u.id = t.user_id WHERE t.hash = ?`, sum[:]).
		Scan(&tokenID, &u.ID, &u.Username, &u.Admin, &createdAt, &u.CloudSubject, &u.Active,
			&ceiling, &allowUnrated, &maxStreams, &maxBitRate)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("resolve token: %w", err)
	}
	// An account that is not active authenticates as nothing, whatever it is
	// holding. Suspending somebody and leaving their television working would
	// not be suspending them.
	if !u.Active {
		return nil, ErrNotFound
	}
	u.CreatedAt = fromMillis(createdAt)

	// The grants are read on the unrestricted handle on purpose: a narrowed
	// handle resolving a credential would filter the very rows that decide
	// how it should be narrowed.
	open := d.unscoped()
	granted, err := open.TokenSections(ctx, tokenID)
	if err != nil {
		return nil, err
	}
	account, err := open.UserSections(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	sections, restricted := narrow(account, granted)

	_, _ = d.sql.ExecContext(ctx, `UPDATE tokens SET last_seen_at = ? WHERE id = ?`,
		millis(time.Now().UTC()), tokenID)
	return &domain.Credential{
		User: u, TokenID: tokenID, Restricted: restricted, Sections: sections,
		Ceiling: ceiling, AllowUnrated: allowUnrated,
		MaxStreams: maxStreams, MaxBitRate: maxBitRate,
	}, nil
}

// UserForToken resolves a presented token to its account, for the callers that
// only need to know who it is.
func (d *DB) UserForToken(ctx context.Context, token string) (*domain.User, error) {
	cred, err := d.Credential(ctx, token)
	if err != nil {
		return nil, err
	}
	return &cred.User, nil
}

// unscoped is this handle without its library restriction, for the few reads
// that decide what the restriction should be.
func (d *DB) unscoped() *DB {
	if !d.restricted() {
		return d
	}
	open := *d
	open.visible = nil
	return &open
}

// SaveProgress records where a user is in an item.
//
// The view count increments only on the transition into watched, so scrubbing
// back and forth across the threshold does not inflate it, and an item that
// was already watched stays watched when a viewer dips back in.
func (d *DB) SaveProgress(ctx context.Context, p domain.Progress) error {
	watched := p.Watched || domain.Finished(p.Offset, p.Duration)

	var (
		wasWatched bool
		views      int
	)
	err := d.sql.QueryRowContext(ctx,
		`SELECT watched, view_count FROM progress WHERE user_id = ? AND item_id = ?`, p.UserID, p.ItemID).
		Scan(&wasWatched, &views)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read progress: %w", err)
	}
	if watched && !wasWatched {
		views++
	}

	now := time.Now().UTC()
	_, err = d.sql.ExecContext(ctx,
		`INSERT INTO progress (user_id, item_id, offset_ms, duration_ms, watched, view_count, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(user_id, item_id) DO UPDATE SET
		   offset_ms = excluded.offset_ms,
		   duration_ms = CASE WHEN excluded.duration_ms > 0 THEN excluded.duration_ms ELSE progress.duration_ms END,
		   watched = excluded.watched,
		   view_count = excluded.view_count,
		   updated_at = excluded.updated_at`,
		p.UserID, p.ItemID, p.Offset.Milliseconds(), p.Duration.Milliseconds(), watched, views, millis(now))
	if err != nil {
		return fmt.Errorf("save progress: %w", err)
	}
	return nil
}

// Progress reads one user's position in a set of items, for decorating a
// listing. Returns only what exists, so an item with no row is simply absent.
func (d *DB) Progress(ctx context.Context, userID int64, itemIDs []int64) (map[int64]domain.Progress, error) {
	if len(itemIDs) == 0 {
		return nil, nil
	}
	out := make(map[int64]domain.Progress, len(itemIDs))

	// One statement per 200 ids rather than one giant IN list, same reason as
	// the prune: a client can ask for a 500-item page.
	const chunk = 200
	for start := 0; start < len(itemIDs); start += chunk {
		end := min(start+chunk, len(itemIDs))
		batch := itemIDs[start:end]

		args := make([]any, 0, len(batch)+1)
		args = append(args, userID)
		placeholders := ""
		for i, id := range batch {
			if i > 0 {
				placeholders += ","
			}
			placeholders += "?"
			args = append(args, id)
		}

		rows, err := d.sql.QueryContext(ctx,
			`SELECT item_id, offset_ms, duration_ms, watched, view_count, updated_at
			   FROM progress WHERE user_id = ? AND item_id IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("read progress: %w", err)
		}
		for rows.Next() {
			var (
				p                    domain.Progress
				offsetMS, durationMS int64
				updatedAt            int64
			)
			if err := rows.Scan(&p.ItemID, &offsetMS, &durationMS, &p.Watched, &p.ViewCount, &updatedAt); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan progress: %w", err)
			}
			p.UserID = userID
			p.Offset = time.Duration(offsetMS) * time.Millisecond
			p.Duration = time.Duration(durationMS) * time.Millisecond
			p.UpdatedAt = fromMillis(updatedAt)
			out[p.ItemID] = p
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ContinueWatching lists what a user has started and not finished, newest
// first: the row every client opens on.
func (d *DB) ContinueWatching(ctx context.Context, userID int64, limit int) ([]domain.Item, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	only, scope := d.only("i.section_id")
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+prefixed("i", itemColumns)+`
		   FROM progress p JOIN items i ON i.id = p.item_id
		  WHERE p.user_id = ? AND p.watched = 0 AND p.offset_ms > 0`+only+`
		  ORDER BY p.updated_at DESC LIMIT ?`,
		append([]any{userID}, append(scope, limit)...)...)
	if err != nil {
		return nil, fmt.Errorf("continue watching: %w", err)
	}
	defer rows.Close()
	return d.collect(ctx, rows)
}

// prefixed qualifies a column list with a table alias, so the shared column
// list can be reused in a join without being written out twice.
func prefixed(alias, columns string) string {
	cols := strings.Split(columns, ",")
	for i, col := range cols {
		cols[i] = alias + "." + strings.TrimSpace(col)
	}
	return strings.Join(cols, ", ")
}
