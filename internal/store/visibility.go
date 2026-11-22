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
	scoped.visible = &visibility{sections: ids, sectioned: true, ceiling: NoCeiling, unrated: true}
	return &scoped
}

// NoCeiling means no age rating is too high for this handle, which is what
// every unrestricted account has.
const NoCeiling = -1

// Rated returns a handle that may see only what is rated for this age.
//
// ceiling is an age in years and [NoCeiling] means no limit. unrated decides
// what happens to the things nobody has rated: allowed, which is the honest
// default for a library whose metadata pass has not reached everything yet,
// or hidden, which is what somebody who wants a child's account to be
// predictable asks for.
//
// Chained onto [DB.Visible] rather than replacing it, because the two
// restrictions are independent: an account can be limited to one library, to
// a rating, to both, or to neither.
func (d *DB) Rated(ceiling int, unrated bool) *DB {
	scoped := *d
	if d.visible != nil {
		copied := *d.visible
		scoped.visible = &copied
	} else {
		scoped.visible = &visibility{ceiling: NoCeiling, unrated: true}
	}
	scoped.visible.ceiling, scoped.visible.unrated = ceiling, unrated
	return &scoped
}

// visibility is what a handle may see: a set of libraries, an age rating
// ceiling, or both. A nil *visibility is an unrestricted handle.
type visibility struct {
	// sections are the libraries, and sectioned says whether that list
	// applies at all: an empty list with sectioned set sees no library,
	// which is not the same as no library restriction.
	sections  []int64
	sectioned bool

	// ceiling is the highest age rating this handle may see, [NoCeiling] for
	// no limit, and unrated says whether the things with no rating are
	// allowed through.
	ceiling int
	unrated bool
}

// restricted reports whether this handle is narrowed at all.
func (d *DB) restricted() bool { return d.visible != nil }

// sectioned reports whether a library restriction applies.
func (d *DB) sectioned() bool { return d.visible != nil && d.visible.sectioned }

// rated reports whether an age rating ceiling applies.
func (d *DB) rated() bool { return d.visible != nil && d.visible.ceiling >= 0 }

// canSee reports whether a library is within this handle's set. True for every
// library on a handle with no library restriction.
func (d *DB) canSee(sectionID int64) bool {
	if !d.sectioned() {
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
	fragment, args := d.onlySections(column)
	rating, rated := d.onlyRated(column)
	return fragment + rating, append(args, rated...)
}

// onlySections is the library half of the restriction, for the few queries
// that are not about items at all: a library has no age rating, and asking
// for one would be a column that is not there.
func (d *DB) onlySections(column string) (string, []any) {
	if !d.sectioned() {
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

// onlyRated is the age rating half.
//
// The column is derived from the section column so the predicate lands on the
// same table in a join: `section_id` is the items table itself, and
// `episode.section_id` is the items table under that alias. Written this way
// because the alternative is a second argument at every call site, and a call
// site that passes the wrong one is a rating ceiling that silently does
// nothing.
func (d *DB) onlyRated(column string) (string, []any) {
	if !d.rated() {
		return "", nil
	}
	rating := "rating_age"
	if dot := strings.LastIndex(column, "."); dot >= 0 {
		rating = column[:dot+1] + "rating_age"
	}
	if d.visible.unrated {
		// An unrated item is allowed through: a library whose metadata pass
		// has not reached everything yet is still watchable.
		return " AND (" + rating + " <= ? OR " + rating + " < 0)", []any{d.visible.ceiling}
	}
	return " AND (" + rating + " >= 0 AND " + rating + " <= ?)", []any{d.visible.ceiling}
}

// UserSections lists the libraries an account is restricted to. An empty
// result means the account is not restricted: see [DB.Credential].
func (d *DB) UserSections(ctx context.Context, userID int64) ([]int64, error) {
	return d.sectionsFor(ctx, `SELECT section_id FROM user_sections WHERE user_id = ? ORDER BY section_id`, userID)
}

// SetUserSections replaces the libraries an account may see.
//
// Empty means every library, the same way it does for a token: an account
// that could reach nothing cannot sign in to anything useful, and deleting
// the account is the honest way to say that.
func (d *DB) SetUserSections(ctx context.Context, userID int64, sections []int64) error {
	return d.setSections(ctx, "user_sections", "user_id", userID, sections)
}

// TokenSections lists the libraries a token is restricted to. An empty result
// means the token is not restricted: see [DB.Credential].
func (d *DB) TokenSections(ctx context.Context, tokenID int64) ([]int64, error) {
	return d.sectionsFor(ctx, `SELECT section_id FROM token_sections WHERE token_id = ? ORDER BY section_id`, tokenID)
}

// sectionsFor reads one grant list.
func (d *DB) sectionsFor(ctx context.Context, query string, id int64) ([]int64, error) {
	rows, err := d.sql.QueryContext(ctx, query, id)
	if err != nil {
		return nil, fmt.Errorf("list granted sections: %w", err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var section int64
		if err := rows.Scan(&section); err != nil {
			return nil, fmt.Errorf("scan granted section: %w", err)
		}
		out = append(out, section)
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
	return d.setSections(ctx, "token_sections", "token_id", tokenID, sections)
}

// setSections replaces one grant list in a single transaction, so a half
// written set is never what a request reads.
func (d *DB) setSections(ctx context.Context, table, column string, id int64, sections []int64) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set %s: %w", table, err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM `+table+` WHERE `+column+` = ?`, id); err != nil {
		return fmt.Errorf("clear %s: %w", table, err)
	}
	for _, section := range sections {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO `+table+` (`+column+`, section_id) VALUES (?, ?)`,
			id, section); err != nil {
			return fmt.Errorf("add to %s: %w", table, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set %s: %w", table, err)
	}
	return nil
}

// narrow combines an account's grants with a credential's own.
//
// Either empty means that side imposes nothing, so the other side stands
// alone and two empties are no restriction at all. Where both name libraries
// the answer is what they agree on, which can be nothing: an account allowed
// the films and a television allowed the music are contradictory
// instructions, and the device reaches neither rather than both.
func narrow(user, own []int64) (sections []int64, restricted bool) {
	switch {
	case len(user) == 0 && len(own) == 0:
		return nil, false
	case len(user) == 0:
		return own, true
	case len(own) == 0:
		return user, true
	}
	allowed := make(map[int64]bool, len(user))
	for _, id := range user {
		allowed[id] = true
	}
	both := make([]int64, 0, len(own))
	for _, id := range own {
		if allowed[id] {
			both = append(both, id)
		}
	}
	return both, true
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

// scopedFor turns a credential into the handle its requests should run on: as
// narrow as the credential says, and carrying the account it belongs to so a
// query that needs somebody's opinion — watch state, at present — has one.
//
// The one place that policy is written down. Every surface that authenticates
// a credential calls this rather than deciding for itself, so there is no
// second opinion about what a restriction means — including the restriction
// that allows nothing, which [narrow] can produce and which must not be read
// as allowing everything.
func (d *DB) scopedFor(cred *domain.Credential) *DB {
	if cred == nil {
		return d
	}
	scoped := d.Viewer(cred.User.ID)
	if cred.Restricted {
		scoped = scoped.Visible(cred.Sections)
	}
	// A ceiling that has been unlocked with a PIN does not apply: that is
	// what the PIN is for, and the surface that took it is the only thing
	// that knows it was given.
	if cred.Ceiling >= 0 && !cred.Unlocked {
		scoped = scoped.Rated(cred.Ceiling, cred.AllowUnrated)
	}
	return scoped
}

// Scoped is scopedFor under a name a handler can read: the database as this
// credential may see it.
func (d *DB) Scoped(cred *domain.Credential) *DB { return d.scopedFor(cred) }

// Viewer returns a handle that reads watch state as this account sees it.
//
// Separate from [DB.Visible] and [DB.Rated] because it is not a restriction:
// it narrows nothing and hides nothing. It answers a different question —
// "watched by whom" — which only a smart collection's watch rules ask, and
// which has no answer at all on a handle nobody authenticated.
func (d *DB) Viewer(userID int64) *DB {
	scoped := *d
	scoped.viewer = userID
	return &scoped
}
