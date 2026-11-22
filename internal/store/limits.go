package store

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// What an account may watch, and how much of the server it may use.
//
// All of it on the account rather than on the device, because every one of
// these is about a person: a child is a child on the television and on the
// tablet, and a household that has given somebody a rating ceiling has not
// said anything about which screen they are allowed to break it on.

// Limits are an account's restrictions.
type Limits struct {
	// Ceiling is the highest age rating this account may see, or
	// [domain.UnknownAge] for no limit.
	Ceiling int

	// AllowUnrated says whether the things nobody has rated are allowed
	// through. True keeps a half-enriched library watchable; false is what
	// somebody who wants a child's account to be predictable asks for.
	AllowUnrated bool

	// MaxStreams is how many things this account may play at once, zero for
	// no limit.
	MaxStreams int

	// MaxBitRate is the most this account may be sent, in bits per second,
	// zero for no limit. A file above it is converted down rather than
	// refused.
	MaxBitRate int64

	// HasPIN says whether a PIN is set, which is how a ceiling is lifted for
	// an evening without the administrator being in the room. The PIN itself
	// is never read back.
	HasPIN bool
}

// UserLimits reads an account's restrictions.
func (d *DB) UserLimits(ctx context.Context, userID int64) (Limits, error) {
	var (
		out     Limits
		pinHash []byte
	)
	err := d.sql.QueryRowContext(ctx,
		`SELECT max_rating_age, allow_unrated, max_streams, max_bitrate, pin_hash
		   FROM users WHERE id = ?`, userID).
		Scan(&out.Ceiling, &out.AllowUnrated, &out.MaxStreams, &out.MaxBitRate, &pinHash)
	if errors.Is(err, sql.ErrNoRows) {
		return Limits{}, ErrNotFound
	}
	if err != nil {
		return Limits{}, fmt.Errorf("read limits: %w", err)
	}
	out.HasPIN = len(pinHash) > 0
	return out, nil
}

// SetUserLimits writes an account's restrictions, leaving the PIN alone.
func (d *DB) SetUserLimits(ctx context.Context, userID int64, limits Limits) error {
	if limits.Ceiling < domain.UnknownAge {
		limits.Ceiling = domain.UnknownAge
	}
	if limits.MaxStreams < 0 {
		limits.MaxStreams = 0
	}
	if limits.MaxBitRate < 0 {
		limits.MaxBitRate = 0
	}
	res, err := d.sql.ExecContext(ctx,
		`UPDATE users SET max_rating_age = ?, allow_unrated = ?, max_streams = ?, max_bitrate = ?
		 WHERE id = ?`,
		limits.Ceiling, limits.AllowUnrated, limits.MaxStreams, limits.MaxBitRate, userID)
	if err != nil {
		return fmt.Errorf("set limits: %w", err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return ErrNotFound
	}
	return nil
}

// pinIterations is how much work one PIN check costs.
//
// A PIN is four digits, which is ten thousand guesses — so the stretching is
// the only thing standing between a copy of the database and the number. It
// is also checked by hand, by somebody standing at a television, so a tenth
// of a second is invisible where it matters and expensive where it does not.
const pinIterations = 200_000

// SetUserPIN sets or clears the PIN that lifts an account's rating ceiling.
//
// Four digits or more, because a one-digit PIN is a formality. An empty PIN
// clears it, which turns the unlock off rather than allowing an empty one.
func (d *DB) SetUserPIN(ctx context.Context, userID int64, pin string) error {
	pin = strings.TrimSpace(pin)
	if pin == "" {
		if _, err := d.sql.ExecContext(ctx, `UPDATE users SET pin_hash = NULL WHERE id = ?`, userID); err != nil {
			return fmt.Errorf("clear pin: %w", err)
		}
		return nil
	}
	if len(pin) < 4 || !digitsOnly(pin) {
		return errors.New("a PIN is four digits or more")
	}

	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("set pin: %w", err)
	}
	derived, err := pbkdf2.Key(sha256.New, pin, salt, pinIterations, 32)
	if err != nil {
		return fmt.Errorf("set pin: %w", err)
	}
	stored := append(salt, derived...)
	if _, err := d.sql.ExecContext(ctx, `UPDATE users SET pin_hash = ? WHERE id = ?`, stored, userID); err != nil {
		return fmt.Errorf("set pin: %w", err)
	}
	return nil
}

// CheckUserPIN reports whether this is the account's PIN.
//
// False for an account with no PIN, rather than true: an account that never
// set one has not agreed to anything being unlocked.
func (d *DB) CheckUserPIN(ctx context.Context, userID int64, pin string) (bool, error) {
	var stored []byte
	err := d.sql.QueryRowContext(ctx, `SELECT pin_hash FROM users WHERE id = ?`, userID).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("read pin: %w", err)
	}
	if len(stored) != 48 {
		return false, nil
	}
	salt, want := stored[:16], stored[16:]
	got, err := pbkdf2.Key(sha256.New, strings.TrimSpace(pin), salt, pinIterations, 32)
	if err != nil {
		return false, fmt.Errorf("check pin: %w", err)
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func digitsOnly(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value != ""
}

// SetRating records what a provider said an item is rated.
//
// Both the label and the age, because the label is what a client shows and
// the age is what a restriction compares against — and working the age out
// once, here, means the comparison is an indexed integer rather than a table
// of national rating systems inside a WHERE clause.
func (d *DB) SetRating(ctx context.Context, itemID int64, label string) error {
	label = strings.TrimSpace(label)
	_, err := d.sql.ExecContext(ctx,
		`UPDATE items SET rating = ?, rating_age = ?, updated_at = ? WHERE id = ?`,
		label, domain.AgeOfRating(label), millis(time.Now().UTC()), itemID)
	if err != nil {
		return fmt.Errorf("set rating: %w", err)
	}
	return nil
}

// InheritRatings gives a season and an episode the rating of the show above
// them, where they have none of their own.
//
// A provider rates a series, not every episode of it, so without this a
// ceiling would hide a show and then show every episode inside it — which is
// the whole restriction defeated by opening a folder.
func (d *DB) InheritRatings(ctx context.Context) error {
	// Twice, because a show is two levels above an episode and each pass
	// moves the rating down one: show to season, then season to episode.
	for range 2 {
		if _, err := d.sql.ExecContext(ctx,
			`UPDATE items SET
			   rating = COALESCE((SELECT parent.rating FROM items AS parent WHERE parent.id = items.parent_id), ''),
			   rating_age = COALESCE((SELECT parent.rating_age FROM items AS parent WHERE parent.id = items.parent_id), -1)
			 WHERE rating_age < 0 AND parent_id IS NOT NULL
			   AND kind IN ('season','episode')
			   AND EXISTS (SELECT 1 FROM items AS parent
			                WHERE parent.id = items.parent_id AND parent.rating_age >= 0)`); err != nil {
			return fmt.Errorf("inherit ratings: %w", err)
		}
	}
	return nil
}
