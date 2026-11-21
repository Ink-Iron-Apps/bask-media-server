package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// ReplaceSubtitles rewrites what is known about one file's subtitles.
//
// Wholesale rather than merged, for the same reason the markers are: a scan
// is the authority on what is there now, and a sidecar somebody deleted has
// to stop being offered.
func (d *DB) ReplaceSubtitles(ctx context.Context, itemID, partID int64, found []domain.Subtitle) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace subtitles: %w", err)
	}
	defer tx.Rollback()

	if err := replaceSubtitlesTx(ctx, tx, itemID, partID, found); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("replace subtitles: %w", err)
	}
	return nil
}

func replaceSubtitlesTx(ctx context.Context, tx *sql.Tx, itemID, partID int64, found []domain.Subtitle) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM subtitles WHERE part_id = ?`, partID); err != nil {
		return fmt.Errorf("clear subtitles: %w", err)
	}
	for _, sub := range found {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO subtitles
			   (item_id, part_id, external, stream_index, path, codec, language, title, forced, is_default, hearing)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			itemID, partID, sub.External, sub.StreamIndex, sub.Path, sub.Codec,
			sub.Language, sub.Title, sub.Forced, sub.Default, sub.Hearing); err != nil {
			return fmt.Errorf("write subtitle: %w", err)
		}
	}
	return nil
}

const subtitleColumns = `id, item_id, part_id, external, stream_index, path, codec, language, title, forced, is_default, hearing`

func scanSubtitle(row interface{ Scan(...any) error }) (domain.Subtitle, error) {
	var sub domain.Subtitle
	err := row.Scan(&sub.ID, &sub.ItemID, &sub.PartID, &sub.External, &sub.StreamIndex,
		&sub.Path, &sub.Codec, &sub.Language, &sub.Title, &sub.Forced, &sub.Default, &sub.Hearing)
	return sub, err
}

// Subtitles reads the subtitles for a set of items in one query, which is
// what keeps drawing a season's episodes two queries rather than two per row.
//
// Ordered so a client that takes the first of a language gets the useful one:
// the default track, then forced, then whatever was found first.
func (d *DB) Subtitles(ctx context.Context, itemIDs []int64) (map[int64][]domain.Subtitle, error) {
	if len(itemIDs) == 0 {
		return nil, nil
	}
	clause, args := inClause(itemIDs)
	only, scope := d.only("items.section_id")
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+prefixed("subtitles", subtitleColumns)+`
		   FROM subtitles JOIN items ON items.id = subtitles.item_id
		  WHERE subtitles.item_id IN (`+clause+`)`+only+`
		  ORDER BY subtitles.item_id, subtitles.is_default DESC, subtitles.forced DESC,
		           subtitles.external, subtitles.id`, append(args, scope...)...)
	if err != nil {
		return nil, fmt.Errorf("read subtitles: %w", err)
	}
	defer rows.Close()

	out := make(map[int64][]domain.Subtitle, len(itemIDs))
	for rows.Next() {
		sub, err := scanSubtitle(rows)
		if err != nil {
			return nil, fmt.Errorf("scan subtitle: %w", err)
		}
		out[sub.ItemID] = append(out[sub.ItemID], sub)
	}
	return out, rows.Err()
}

// Subtitle reads one, which is what a request for the text itself needs.
func (d *DB) Subtitle(ctx context.Context, id int64) (*domain.Subtitle, error) {
	only, scope := d.only("items.section_id")
	sub, err := scanSubtitle(d.sql.QueryRowContext(ctx,
		`SELECT `+prefixed("subtitles", subtitleColumns)+`
		   FROM subtitles JOIN items ON items.id = subtitles.item_id
		  WHERE subtitles.id = ?`+only, append([]any{id}, scope...)...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read subtitle: %w", err)
	}
	return &sub, nil
}
