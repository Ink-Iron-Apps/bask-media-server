package store

import (
	"context"
	"fmt"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// NeedsMetadata lists items a provider has never answered for.
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
		  WHERE metadata_at = 0 AND kind IN ('movie','show','season','episode')
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
