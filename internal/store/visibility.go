package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// Visible returns a handle onto the same database that can see only the named
// libraries.
//
// A handle, rather than a parameter on every query, because the alternative
// was threading a set of section ids through forty call sites and trusting
// each one to pass it. A query that forgets the filter is a library that is
// hidden from a list and still plays; the filter lives beside the SQL here, in
// one package, where every statement that returns an item is in sight of it.
//
// The underlying *sql.DB is shared, so this costs no connection and no
// allocation beyond the copy: one per authenticated request is nothing.
//
// An empty set sees nothing. That is deliberate and it is not the same as the
// unrestricted handle, whose visible field is nil: "allowed nowhere" and "not
// restricted" are different answers and conflating them is how an empty
// grant list becomes full access. The policy that no grants means every
// library belongs at the edge that reads the grants, not here.
func (d *DB) Visible(sections []int64) *DB {
	scoped := *d
	ids := make([]int64, len(sections))
	copy(ids, sections)
	scoped.visible = &visibility{sections: ids}
	return &scoped
}

// visibility is the set of libraries a handle may see. A nil *visibility is
// an unrestricted handle; a non-nil one with no sections sees nothing.
type visibility struct{ sections []int64 }

// restricted reports whether this handle has a library set at all.
func (d *DB) restricted() bool { return d.visible != nil }

// canSee reports whether a library is within this handle's set. True for every
// library on an unrestricted handle.
func (d *DB) canSee(sectionID int64) bool {
	if !d.restricted() {
		return true
	}
	for _, id := range d.visible.sections {
		if id == sectionID {
			return true
		}
	}
	return false
}

// only builds the predicate that restricts a query to this handle's libraries,
// as a fragment to append to a WHERE clause and the arguments it needs.
//
// Empty fragment on an unrestricted handle, so an unscoped query is byte for
// byte the statement it always was. `AND 0` when the set is empty, because a
// handle that may see nothing must return nothing rather than everything — an
// `IN ()` is a syntax error and a dropped predicate is a leak.
//
// column is the expression holding the section, qualified where the query
// joins: `section_id` in a plain query, `episode.section_id` in a join.
func (d *DB) only(column string) (string, []any) {
	if !d.restricted() {
		return "", nil
	}
	if len(d.visible.sections) == 0 {
		return " AND 0", nil
	}
	args := make([]any, 0, len(d.visible.sections))
	for _, id := range d.visible.sections {
		args = append(args, id)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	return " AND " + column + " IN (" + placeholders + ")", args
}

// TokenSections lists the libraries a token is restricted to. An empty result
// means the token is not restricted: see [DB.Credential].
func (d *DB) TokenSections(ctx context.Context, tokenID int64) ([]int64, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT section_id FROM token_sections WHERE token_id = ? ORDER BY section_id`, tokenID)
	if err != nil {
		return nil, fmt.Errorf("list token sections: %w", err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan token section: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetTokenSections replaces the libraries a token may see.
//
// An empty list removes the restriction rather than imposing a total one. The
// admin interface has no way to express "this device may see nothing", because
// a token that can reach no library is a revoked token with extra steps, and
// the button for that already exists.
func (d *DB) SetTokenSections(ctx context.Context, tokenID int64, sections []int64) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set token sections: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM token_sections WHERE token_id = ?`, tokenID); err != nil {
		return fmt.Errorf("clear token sections: %w", err)
	}
	for _, id := range sections {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO token_sections (token_id, section_id) VALUES (?, ?)`,
			tokenID, id); err != nil {
			return fmt.Errorf("add token section: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set token sections: %w", err)
	}
	return nil
}

// TokenExists reports whether a credential row is there, so the admin
// interface can answer "no such token" rather than failing a foreign key.
func (d *DB) TokenExists(ctx context.Context, tokenID int64) (bool, error) {
	var n int
	if err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tokens WHERE id = ?`, tokenID).Scan(&n); err != nil {
		return false, fmt.Errorf("read token: %w", err)
	}
	return n > 0, nil
}

// NoteTokenDevice records what called itself in with this token, so the token
// list reads as a list of devices.
//
// Diagnostics, never authorisation. The name arrives in a header the client
// writes, so it can say anything at all; it is worth showing beside a label a
// human typed and worth nothing as a gate. Best effort for the same reason
// last_seen_at is: a failed write here must not refuse a valid request.
func (d *DB) NoteTokenDevice(ctx context.Context, tokenID int64, device string) {
	device = strings.TrimSpace(device)
	if tokenID == 0 || device == "" {
		return
	}
	if len(device) > 64 {
		device = device[:64]
	}
	_, _ = d.sql.ExecContext(ctx,
		`UPDATE tokens SET device = ? WHERE id = ? AND device <> ?`, device, tokenID, device)
}

// scopedFor turns a credential's library set into the handle its requests
// should run on: the unrestricted handle when nothing was granted, because no
// grant means no restriction, and a scoped one otherwise.
//
// The one place that policy is written down. Every surface that authenticates
// a credential calls this rather than deciding for itself, so there is no
// second opinion about what an empty grant list means.
func (d *DB) scopedFor(cred *domain.Credential) *DB {
	if cred == nil || len(cred.Sections) == 0 {
		return d
	}
	return d.Visible(cred.Sections)
}

// Scoped is scopedFor under a name a handler can read: the database as this
// credential may see it.
func (d *DB) Scoped(cred *domain.Credential) *DB { return d.scopedFor(cred) }
