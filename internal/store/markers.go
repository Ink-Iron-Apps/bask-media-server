package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// ReplaceChapters records what a file's chapters are now.
//
// Replace rather than merge: a file whose chapters changed was re-encoded or
// replaced, and keeping the old ones would leave a timeline marked up for a
// file nobody has any more.
func (d *DB) ReplaceChapters(ctx context.Context, itemID int64, chapters []domain.Chapter) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace chapters: %w", err)
	}
	defer tx.Rollback()

	if err := replaceChaptersTx(ctx, tx, itemID, chapters); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("replace chapters: %w", err)
	}
	return nil
}

func replaceChaptersTx(ctx context.Context, tx *sql.Tx, itemID int64, chapters []domain.Chapter) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM chapters WHERE item_id = ?`, itemID); err != nil {
		return fmt.Errorf("clear chapters: %w", err)
	}
	for _, ch := range chapters {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO chapters (item_id, idx, title, start_ms, end_ms) VALUES (?, ?, ?, ?, ?)`,
			itemID, ch.Index, ch.Title, ch.Start.Milliseconds(), ch.End.Milliseconds()); err != nil {
			return fmt.Errorf("insert chapter: %w", err)
		}
	}
	return nil
}

// ReplaceMarkers records one detector's findings for one item.
//
// Scoped to a source so the two detectors cannot erase each other: an audio
// pass that finds nothing in an episode whose chapters were labelled leaves
// those markers alone, and re-running either pass is idempotent.
func (d *DB) ReplaceMarkers(ctx context.Context, itemID int64, source domain.MarkerSource, found []domain.Marker) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace markers: %w", err)
	}
	defer tx.Rollback()

	if err := replaceMarkersTx(ctx, tx, itemID, source, found); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("replace markers: %w", err)
	}
	return nil
}

func replaceMarkersTx(ctx context.Context, tx *sql.Tx, itemID int64, source domain.MarkerSource, found []domain.Marker) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM markers WHERE item_id = ? AND source = ?`, itemID, string(source)); err != nil {
		return fmt.Errorf("clear markers: %w", err)
	}
	for _, m := range found {
		if m.End <= m.Start {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO markers (item_id, type, source, start_ms, end_ms) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(item_id, type, source, start_ms) DO UPDATE SET end_ms = excluded.end_ms`,
			itemID, string(m.Type), string(source), m.Start.Milliseconds(), m.End.Milliseconds()); err != nil {
			return fmt.Errorf("insert marker: %w", err)
		}
	}
	return nil
}

// Markers reads the markers for a set of items in one query.
//
// One query rather than one per item, because the metadata endpoint for a
// season's worth of episodes would otherwise be a query per row. Where both
// detectors found the same kind of marker on one item, the chapter is kept and
// the guess dropped: a labelled file beats a measurement of one.
func (d *DB) Markers(ctx context.Context, itemIDs []int64) (map[int64][]domain.Marker, error) {
	if len(itemIDs) == 0 {
		return nil, nil
	}
	placeholders, args := inClause(itemIDs)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT id, item_id, type, source, start_ms, end_ms FROM markers
		   WHERE item_id IN (`+placeholders+`) ORDER BY item_id, start_ms`, args...)
	if err != nil {
		return nil, fmt.Errorf("read markers: %w", err)
	}
	defer rows.Close()

	byItem := make(map[int64][]domain.Marker, len(itemIDs))
	labelled := make(map[[2]any]bool)
	for rows.Next() {
		var (
			m              domain.Marker
			kind, source   string
			startMS, endMS int64
		)
		if err := rows.Scan(&m.ID, &m.ItemID, &kind, &source, &startMS, &endMS); err != nil {
			return nil, fmt.Errorf("scan marker: %w", err)
		}
		m.Type, m.Source = domain.MarkerType(kind), domain.MarkerSource(source)
		m.Start = millisDuration(startMS)
		m.End = millisDuration(endMS)
		byItem[m.ItemID] = append(byItem[m.ItemID], m)
		if m.Source == domain.SourceChapter {
			labelled[[2]any{m.ItemID, kind}] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for id, list := range byItem {
		kept := list[:0]
		for _, m := range list {
			if m.Source == domain.SourceAudio && labelled[[2]any{id, string(m.Type)}] {
				continue
			}
			kept = append(kept, m)
		}
		byItem[id] = kept
	}
	return byItem, nil
}

// Chapters reads the chapters for a set of items in one query.
func (d *DB) Chapters(ctx context.Context, itemIDs []int64) (map[int64][]domain.Chapter, error) {
	if len(itemIDs) == 0 {
		return nil, nil
	}
	placeholders, args := inClause(itemIDs)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT id, item_id, idx, title, start_ms, end_ms FROM chapters
		   WHERE item_id IN (`+placeholders+`) ORDER BY item_id, idx`, args...)
	if err != nil {
		return nil, fmt.Errorf("read chapters: %w", err)
	}
	defer rows.Close()

	byItem := make(map[int64][]domain.Chapter, len(itemIDs))
	for rows.Next() {
		var (
			ch             domain.Chapter
			startMS, endMS int64
		)
		if err := rows.Scan(&ch.ID, &ch.ItemID, &ch.Index, &ch.Title, &startMS, &endMS); err != nil {
			return nil, fmt.Errorf("scan chapter: %w", err)
		}
		ch.Start, ch.End = millisDuration(startMS), millisDuration(endMS)
		byItem[ch.ItemID] = append(byItem[ch.ItemID], ch)
	}
	return byItem, rows.Err()
}

// inClause builds a placeholder list for a bounded set of ids.
func inClause(ids []int64) (string, []any) {
	var sb strings.Builder
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteByte('?')
		args = append(args, id)
	}
	return sb.String(), args
}
