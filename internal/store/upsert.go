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

// Found is one thing a scan discovered on disk, in the scanner's terms.
//
// Deliberately flat: the scanner knows a path, what it parsed out of it, and
// where it belongs in a hierarchy by name. Turning that into rows — creating
// the show before the season before the episode — is this package's job,
// because only this package knows the identity index that makes it idempotent.
type Found struct {
	SectionID int64
	Kind      domain.Kind

	// Show and Season place an episode. Empty for a movie.
	Show       string
	ShowYear   int
	SeasonNum  int
	EpisodeNum int

	Title    string
	Year     int
	Duration time.Duration

	Path       string
	Size       int64
	Container  string
	ModifiedAt time.Time
}

// Writer applies scan results in batches inside one transaction.
//
// A scan of a large library is tens of thousands of tiny writes, and one
// transaction per file would fsync per file: the difference is minutes versus
// hours. One transaction for the whole scan would instead hold a write lock
// for the duration and grow the WAL without bound, so the batch is the middle
// ground — commit every BatchSize items, keep the WAL small, let readers in
// between batches.
type Writer struct {
	db    *DB
	batch int

	tx      *sql.Tx
	pending int
	parents map[string]int64 // show/season cache, reset per batch — see note in Add
	now     time.Time
	seen    map[string]struct{}
}

// NewWriter starts a batched writer. batch of 0 means the default of 500,
// which keeps a batch's transaction well under a megabyte of WAL for typical
// rows.
func (d *DB) NewWriter(batch int) *Writer {
	if batch <= 0 {
		batch = 500
	}
	return &Writer{
		db:      d,
		batch:   batch,
		parents: make(map[string]int64),
		now:     time.Now().UTC(),
		seen:    make(map[string]struct{}),
	}
}

// Add records one discovery, opening a transaction if none is open and
// committing when the batch is full.
//
// The parent cache is keyed by section/show/season rather than by row id, and
// it is intentionally not cleared between batches: a library's shows are a
// small set — a few hundred at most — while its episodes are the many, so this
// is a bounded cache by the shape of the data, not by hope. A library with
// 50,000 distinct show names would be the exception that needs an LRU, and the
// metrics endpoint reports the occupancy so that case is visible rather than
// theoretical.
func (w *Writer) Add(ctx context.Context, f Found) error {
	if err := w.begin(ctx); err != nil {
		return err
	}

	var parentID *int64
	switch f.Kind {
	case domain.KindEpisode:
		showID, err := w.ensureContainer(ctx, f.SectionID, domain.KindShow, nil, f.Show, f.ShowYear, 0)
		if err != nil {
			return err
		}
		seasonTitle := fmt.Sprintf("Season %d", f.SeasonNum)
		seasonID, err := w.ensureContainer(ctx, f.SectionID, domain.KindSeason, &showID, seasonTitle, 0, f.SeasonNum)
		if err != nil {
			return err
		}
		parentID = &seasonID
	case domain.KindMovie:
		// No parent.
	default:
		return fmt.Errorf("scanner produced an unplayable kind: %s", f.Kind)
	}

	index := f.EpisodeNum
	itemID, err := w.upsertItem(ctx, f.SectionID, f.Kind, parentID, f.Title, f.Year, index, f.Duration)
	if err != nil {
		return err
	}
	if err := w.upsertPart(ctx, itemID, f); err != nil {
		return err
	}

	w.seen[f.Path] = struct{}{}
	w.pending++
	if w.pending >= w.batch {
		return w.Commit(ctx)
	}
	return nil
}

// Commit flushes the open batch, if any.
func (w *Writer) Commit(ctx context.Context) error {
	if w.tx == nil {
		return nil
	}
	tx := w.tx
	w.tx, w.pending = nil, 0
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit batch: %w", err)
	}
	return nil
}

// Rollback abandons the open batch. Safe to call after Commit.
func (w *Writer) Rollback() {
	if w.tx == nil {
		return
	}
	w.tx.Rollback()
	w.tx, w.pending = nil, 0
}

// Seen reports the paths this writer has accepted, which is what a sweep for
// vanished files compares against.
func (w *Writer) Seen() map[string]struct{} { return w.seen }

func (w *Writer) begin(ctx context.Context) error {
	if w.tx != nil {
		return nil
	}
	tx, err := w.db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin batch: %w", err)
	}
	w.tx = tx
	return nil
}

// ensureContainer finds or creates a show or season and returns its id.
func (w *Writer) ensureContainer(ctx context.Context, sectionID int64, kind domain.Kind, parentID *int64, title string, year, index int) (int64, error) {
	cacheKey := fmt.Sprintf("%d/%s/%v/%s/%d/%d", sectionID, kind, derefID(parentID), title, year, index)
	if id, ok := w.parents[cacheKey]; ok {
		return id, nil
	}
	id, err := w.upsertItem(ctx, sectionID, kind, parentID, title, year, index, 0)
	if err != nil {
		return 0, err
	}
	w.parents[cacheKey] = id
	return id, nil
}

// upsertItem is idempotent on the identity index, so a rescan of an unchanged
// library produces no new rows and no churn in updated_at.
func (w *Writer) upsertItem(ctx context.Context, sectionID int64, kind domain.Kind, parentID *int64, title string, year, index int, duration time.Duration) (int64, error) {
	var parent any
	if parentID != nil {
		parent = *parentID
	}

	var id int64
	err := w.tx.QueryRowContext(ctx,
		`SELECT id FROM items WHERE section_id = ? AND kind = ? AND IFNULL(parent_id, 0) = IFNULL(?, 0)
		   AND title = ? AND year = ? AND idx = ?`,
		sectionID, string(kind), parent, title, year, index).Scan(&id)
	switch {
	case err == nil:
		if duration > 0 {
			if _, err := w.tx.ExecContext(ctx,
				`UPDATE items SET duration_ms = ?, updated_at = ? WHERE id = ? AND duration_ms <> ?`,
				duration.Milliseconds(), millis(w.now), id, duration.Milliseconds()); err != nil {
				return 0, fmt.Errorf("update duration: %w", err)
			}
		}
		return id, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("look up item: %w", err)
	}

	res, err := w.tx.ExecContext(ctx,
		`INSERT INTO items (section_id, kind, parent_id, title, sort_title, year, idx, duration_ms, added_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sectionID, string(kind), parent, title, SortTitle(title), year, index,
		duration.Milliseconds(), millis(w.now), millis(w.now))
	if err != nil {
		return 0, fmt.Errorf("insert item: %w", err)
	}
	id, err = res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("insert item: %w", err)
	}
	return id, nil
}

// upsertPart records the file, and leaves the row untouched when nothing about
// it has changed. That is what makes a rescan cost a stat per file.
func (w *Writer) upsertPart(ctx context.Context, itemID int64, f Found) error {
	_, err := w.tx.ExecContext(ctx,
		`INSERT INTO parts (item_id, path, size, container, modified_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET
		   item_id = excluded.item_id,
		   size = excluded.size,
		   container = excluded.container,
		   modified_at = excluded.modified_at
		 WHERE parts.size <> excluded.size OR parts.modified_at <> excluded.modified_at OR parts.item_id <> excluded.item_id`,
		itemID, f.Path, f.Size, f.Container, millis(f.ModifiedAt))
	if err != nil {
		return fmt.Errorf("upsert part: %w", err)
	}
	return nil
}

// PrunePaths removes parts whose files are no longer on disk, and then any
// item left with no parts and no children.
//
// Deletes in bounded chunks: a library that lost a whole drive would otherwise
// build one statement with 40,000 placeholders.
func (d *DB) PrunePaths(ctx context.Context, sectionID int64, keep map[string]struct{}) (int, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT p.id, p.path FROM parts p JOIN items i ON i.id = p.item_id WHERE i.section_id = ?`, sectionID)
	if err != nil {
		return 0, fmt.Errorf("list paths: %w", err)
	}

	var stale []int64
	for rows.Next() {
		var (
			id   int64
			path string
		)
		if err := rows.Scan(&id, &path); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan path: %w", err)
		}
		if _, ok := keep[path]; !ok {
			stale = append(stale, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(stale) == 0 {
		return 0, nil
	}

	const chunk = 200
	for start := 0; start < len(stale); start += chunk {
		end := min(start+chunk, len(stale))
		batch := stale[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		args := make([]any, 0, len(batch))
		for _, id := range batch {
			args = append(args, id)
		}
		if _, err := d.sql.ExecContext(ctx, `DELETE FROM parts WHERE id IN (`+placeholders+`)`, args...); err != nil {
			return 0, fmt.Errorf("delete parts: %w", err)
		}
	}

	// Episodes and movies with no files left, then seasons and shows with
	// nothing underneath. Two passes, deepest first, so a show whose last
	// episode just went also goes.
	if _, err := d.sql.ExecContext(ctx,
		`DELETE FROM items WHERE section_id = ? AND kind IN ('movie','episode')
		   AND NOT EXISTS (SELECT 1 FROM parts WHERE parts.item_id = items.id)`, sectionID); err != nil {
		return 0, fmt.Errorf("prune leaves: %w", err)
	}
	for range 2 {
		if _, err := d.sql.ExecContext(ctx,
			`DELETE FROM items WHERE section_id = ? AND kind IN ('season','show')
			   AND NOT EXISTS (SELECT 1 FROM items AS child WHERE child.parent_id = items.id)`, sectionID); err != nil {
			return 0, fmt.Errorf("prune containers: %w", err)
		}
	}
	return len(stale), nil
}

// SortTitle is Title with a leading article dropped and case folded, so "The
// Martian" files under M where a viewer looks for it.
func SortTitle(title string) string {
	lower := strings.ToLower(strings.TrimSpace(title))
	for _, article := range []string{"the ", "a ", "an "} {
		if strings.HasPrefix(lower, article) {
			return strings.TrimSpace(lower[len(article):])
		}
	}
	return lower
}

func derefID(id *int64) int64 {
	if id == nil {
		return 0
	}
	return *id
}
