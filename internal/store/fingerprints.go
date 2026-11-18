package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// Segment names which end of a file a fingerprint covers.
type Segment string

const (
	SegmentHead Segment = "head"
	SegmentTail Segment = "tail"
)

// Fingerprint is a stored fingerprint of one end of one file.
type Fingerprint struct {
	PartID int64
	ItemID int64
	Segment
	Offset time.Duration
	Data   []byte
}

// SaveFingerprint records a fingerprint against the exact file it was computed
// from.
//
// Keyed on the file's size and mtime as well as its id, which is what makes
// this cache correct rather than merely fast: a replaced file — a better rip,
// a different cut — has a different intro offset, and serving a marker
// computed from the file that used to be there would skip into the episode.
func (d *DB) SaveFingerprint(ctx context.Context, partID int64, segment Segment, offset time.Duration, data []byte, size int64, modifiedAt time.Time) error {
	_, err := d.sql.ExecContext(ctx,
		`INSERT INTO fingerprints (part_id, segment, offset_ms, data, size, modified_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(part_id, segment) DO UPDATE SET
		   offset_ms = excluded.offset_ms,
		   data = excluded.data,
		   size = excluded.size,
		   modified_at = excluded.modified_at,
		   created_at = excluded.created_at`,
		partID, string(segment), offset.Milliseconds(), data, size, millis(modifiedAt), millis(time.Now().UTC()))
	if err != nil {
		return fmt.Errorf("save fingerprint: %w", err)
	}
	return nil
}

// Fingerprint reads a stored fingerprint, or ErrNotFound when there is none
// for this file as it is now.
func (d *DB) Fingerprint(ctx context.Context, partID int64, segment Segment, size int64, modifiedAt time.Time) (*Fingerprint, error) {
	var (
		fp       Fingerprint
		offsetMS int64
	)
	err := d.sql.QueryRowContext(ctx,
		`SELECT part_id, segment, offset_ms, data FROM fingerprints
		   WHERE part_id = ? AND segment = ? AND size = ? AND modified_at = ?`,
		partID, string(segment), size, millis(modifiedAt)).
		Scan(&fp.PartID, &fp.Segment, &offsetMS, &fp.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read fingerprint: %w", err)
	}
	fp.Offset = millisDuration(offsetMS)
	return &fp, nil
}

// SweepFingerprints drops fingerprints that no longer describe the file they
// were computed from.
//
// A fingerprint is a cache, and this is its only eviction: ~19 KiB per end of
// per file is small, but a library that is re-encoded over the years would
// otherwise keep every version's fingerprint forever.
func (d *DB) SweepFingerprints(ctx context.Context) (int, error) {
	res, err := d.sql.ExecContext(ctx,
		`DELETE FROM fingerprints WHERE NOT EXISTS (
		   SELECT 1 FROM parts
		    WHERE parts.id = fingerprints.part_id
		      AND parts.size = fingerprints.size
		      AND parts.modified_at = fingerprints.modified_at)`)
	if err != nil {
		return 0, fmt.Errorf("sweep fingerprints: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return int(affected), nil
}

// SeasonsNeedingMarkers lists seasons no audio pass has finished for.
//
// A season at a time, because a season is the set that shares an intro, and
// limited because a pass is a loop over batches rather than one query that
// returns a library.
func (d *DB) SeasonsNeedingMarkers(ctx context.Context, limit int) ([]domain.Item, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items
		  WHERE kind = 'season' AND markers_at = 0
		  ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list seasons needing markers: %w", err)
	}
	defer rows.Close()
	return d.collect(ctx, rows)
}

// MarkSeasonDetected records that an audio pass has run for a season, whatever
// it found.
//
// Recorded even when it found nothing, so a library of shows with no detectable
// intro is not re-decoded on every pass. A changed file clears it again: see
// ClearSeasonDetection.
func (d *DB) MarkSeasonDetected(ctx context.Context, seasonID int64, at time.Time) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE items SET markers_at = ? WHERE id = ?`, millis(at), seasonID)
	if err != nil {
		return fmt.Errorf("mark season detected: %w", err)
	}
	return nil
}

// ClearSeasonDetection forgets that a season was ever detected, so the next
// pass looks again.
//
// Called where a season's files have changed: an added episode is new evidence
// about where the intro is, and the episode itself has no marker at all yet.
func (d *DB) ClearSeasonDetection(ctx context.Context, seasonID int64) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE items SET markers_at = 0 WHERE id = ?`, seasonID)
	if err != nil {
		return fmt.Errorf("clear season detection: %w", err)
	}
	return nil
}

// EpisodesWithParts lists a season's episodes together with their files, which
// is what a detector needs and what the generic item reader does not carry.
func (d *DB) EpisodesWithParts(ctx context.Context, seasonID int64) ([]domain.Item, error) {
	items, _, err := d.Children(ctx, seasonID, Page{Size: 500})
	if err != nil {
		return nil, err
	}
	out := make([]domain.Item, 0, len(items))
	for _, it := range items {
		if !it.Playable() {
			continue
		}
		parts, err := d.Parts(ctx, it.ID)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 {
			continue
		}
		it.Parts = parts
		out = append(out, it)
	}
	return out, nil
}
