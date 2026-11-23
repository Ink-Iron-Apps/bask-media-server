package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// NeedsMetadata lists items a provider has never answered for.
//
// Items somebody has corrected by hand are left out: a provider that
// overwrites a correction on the next pass is worse than no provider.
//
// Films and television only. The provider BMS ships with is TMDB, which knows
// nothing about an album or a photograph, and handing it one would spend a
// request to learn that "Unknown Album" is not a film — on every pass, for
// every row in a library of thousands.
//
// Ordered so containers come before their contents: a show has to be matched
// before its episodes can be looked up, because an episode is addressed by the
// show's provider id and a season number. Limited, because an enrichment pass
// is a loop over batches rather than one query that returns a library.
func (d *DB) NeedsMetadata(ctx context.Context, limit int) ([]domain.Item, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items
		  WHERE metadata_at = 0 AND locked = 0 AND kind IN ('movie','show','season','episode')
		  ORDER BY CASE kind WHEN 'show' THEN 0 WHEN 'movie' THEN 1 WHEN 'season' THEN 2 ELSE 3 END, id
		  LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list unenriched: %w", err)
	}
	defer rows.Close()
	return d.collect(ctx, rows)
}

// SaveMetadata records what a provider said about an item.
//
// The scanned title and year are left alone on purpose. They come from the
// file, which is the thing actually on disk, and the identity index is built
// on them — rewriting a title to a provider's spelling would both rename what
// the viewer put there and risk colliding with another row. Summary, images
// and the provider id are additive, so a wrong match is a cosmetic problem
// that a rescan can correct rather than a library that reorganises itself.
func (d *DB) SaveMetadata(ctx context.Context, itemID int64, providerID, summary, posterKey, artKey string) error {
	now := time.Now().UTC()
	_, err := d.sql.ExecContext(ctx,
		`UPDATE items SET
		   provider_id = CASE WHEN ? <> '' THEN ? ELSE provider_id END,
		   summary     = CASE WHEN ? <> '' THEN ? ELSE summary END,
		   poster_key  = CASE WHEN ? <> '' THEN ? ELSE poster_key END,
		   art_key     = CASE WHEN ? <> '' THEN ? ELSE art_key END,
		   metadata_at = ?,
		   updated_at  = ?
		 WHERE id = ?`,
		providerID, providerID, summary, summary, posterKey, posterKey, artKey, artKey,
		millis(now), millis(now), itemID)
	if err != nil {
		return fmt.Errorf("save metadata: %w", err)
	}
	return nil
}

// MarkMetadataAttempted records that a provider was asked and had nothing, so
// the next pass does not ask again about every unmatched title forever.
func (d *DB) MarkMetadataAttempted(ctx context.Context, itemID int64) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE items SET metadata_at = ? WHERE id = ?`, millis(time.Now().UTC()), itemID)
	if err != nil {
		return fmt.Errorf("mark attempted: %w", err)
	}
	return nil
}

// InheritArtwork gives a season or episode its show's poster when it has none
// of its own, which is what makes a show's seasons look right before anything
// season-specific has been fetched.
func (d *DB) InheritArtwork(ctx context.Context) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE items SET poster_key = (
		     SELECT parent.poster_key FROM items AS parent WHERE parent.id = items.parent_id
		   ), art_key = (
		     SELECT parent.art_key FROM items AS parent WHERE parent.id = items.parent_id
		   )
		 WHERE kind = 'season' AND poster_key = '' AND parent_id IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("inherit artwork: %w", err)
	}
	return nil
}

// Edit is what somebody changed about an item by hand.
//
// Pointers, because this is a patch: a field that was not sent is a field
// nobody touched, and the difference between "clear the summary" and "leave
// the summary alone" has to survive the journey from the page.
type Edit struct {
	Title   *string
	Year    *int
	Summary *string

	// Genres is the whole list, replaced as one: a correction says what this
	// is about, not which word to add. An empty slice therefore means "it is
	// about nothing", which is a thing somebody can genuinely mean, while a
	// nil pointer means they did not touch the genres at all.
	Genres *[]string

	// Locked says whether the enrichment pass should leave this item alone.
	// Editing anything locks it by default — the point of a correction is
	// that it stays corrected — and this is how it is unlocked again.
	Locked *bool
}

// EditItem applies a correction.
//
// The scanned title and year are not touched, which is the whole reason they
// exist: they are this item's identity to the scanner, so a rescan still
// recognises the row it is looking at and the correction survives. The sort
// title is recomputed from the new title, because a film renamed by hand
// should file where a person would look for it.
func (d *DB) EditItem(ctx context.Context, itemID int64, edit Edit) error {
	sets := []string{}
	args := []any{}
	if edit.Title != nil {
		title := strings.TrimSpace(*edit.Title)
		if title == "" {
			return fmt.Errorf("edit item: a title cannot be empty")
		}
		sets = append(sets, "title = ?", "sort_title = ?")
		args = append(args, title, SortTitle(title))
	}
	if edit.Year != nil {
		sets = append(sets, "year = ?")
		args = append(args, *edit.Year)
	}
	if edit.Summary != nil {
		sets = append(sets, "summary = ?")
		args = append(args, strings.TrimSpace(*edit.Summary))
	}
	locked := true
	if edit.Locked != nil {
		locked = *edit.Locked
	}
	sets = append(sets, "locked = ?", "updated_at = ?")
	args = append(args, boolToInt(locked), millis(time.Now().UTC()))
	args = append(args, itemID)

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("edit item: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE items SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return fmt.Errorf("edit item: %w", err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return ErrNotFound
	}
	// The genres go with the row they describe, in the same transaction: a
	// correction that half applied would leave an item whose title somebody
	// fixed and whose shelf they did not.
	if edit.Genres != nil {
		if err := replaceGenresTx(ctx, tx, itemID, *edit.Genres); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("edit item: %w", err)
	}
	return nil
}

// ScannedTitle is what the filename said, which is what the page shows beside
// a corrected title so somebody can see what they changed — and what a
// "revert" puts back.
func (d *DB) ScannedTitle(ctx context.Context, itemID int64) (title string, year int, err error) {
	err = d.sql.QueryRowContext(ctx,
		`SELECT scanned_title, scanned_year FROM items WHERE id = ?`, itemID).Scan(&title, &year)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, ErrNotFound
	}
	if err != nil {
		return "", 0, fmt.Errorf("read scanned title: %w", err)
	}
	return title, year, nil
}

// Rematch points an item at a different entry in the provider's namespace and
// forgets everything the old match left behind.
//
// The summary and the artwork go, rather than being overwritten in place: a
// wrong match that is half replaced is worse than one that is obviously
// empty, and the images are fetched again by the caller from what the new
// match says. metadata_at is cleared as well, so an interrupted re-match is
// picked up by the next ordinary pass instead of being lost.
func (d *DB) Rematch(ctx context.Context, itemID int64, providerID string) error {
	res, err := d.sql.ExecContext(ctx,
		`UPDATE items SET provider_id = ?, summary = '', poster_key = '', art_key = '',
		   metadata_at = 0, updated_at = ?
		 WHERE id = ?`,
		providerID, millis(time.Now().UTC()), itemID)
	if err != nil {
		return fmt.Errorf("rematch: %w", err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ForgetDescendants makes everything under an item unmatched again.
//
// A show matched to the wrong series has every episode matched to the wrong
// series, and they are addressed through the show's provider id: correcting
// the show is only half the repair. Two levels, because that is how deep a
// show goes.
func (d *DB) ForgetDescendants(ctx context.Context, itemID int64) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE items SET metadata_at = 0, summary = '', provider_id = ''
		 WHERE locked = 0 AND (parent_id = ? OR parent_id IN (SELECT id FROM items WHERE parent_id = ?))`,
		itemID, itemID)
	if err != nil {
		return fmt.Errorf("forget descendants: %w", err)
	}
	return nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
