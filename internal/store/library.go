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

// Page is a window onto a list query.
//
// Every list in this package takes one. Plex clients already page with
// X-Plex-Container-Start and X-Plex-Container-Size, so the API edge has the
// numbers to hand, and making the window mandatory means no code path can
// quietly load a 30,000-item section into memory.
type Page struct {
	Start int
	Size  int
}

// Window returns the page clamped to something sane: a non-negative start and
// a size between 1 and max.
func (p Page) Window(max int) (offset, limit int) {
	offset = p.Start
	if offset < 0 {
		offset = 0
	}
	limit = p.Size
	if limit <= 0 || limit > max {
		limit = max
	}
	return offset, limit
}

// CreateSection adds a library.
func (d *DB) CreateSection(ctx context.Context, title string, kind domain.SectionKind, roots []string) (*domain.Section, error) {
	now := time.Now().UTC()
	res, err := d.sql.ExecContext(ctx,
		`INSERT INTO sections (title, kind, roots, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		title, string(kind), strings.Join(roots, "\n"), millis(now), millis(now))
	if err != nil {
		return nil, fmt.Errorf("create section: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("create section: %w", err)
	}
	return &domain.Section{ID: id, Title: title, Kind: kind, Roots: roots, CreatedAt: now, UpdatedAt: now}, nil
}

// Sections lists every library, oldest first so a client's section order is
// stable across restarts.
func (d *DB) Sections(ctx context.Context) ([]domain.Section, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT id, title, kind, roots, scanned_at, created_at, updated_at FROM sections ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list sections: %w", err)
	}
	defer rows.Close()

	var out []domain.Section
	for rows.Next() {
		var (
			s                               domain.Section
			kind, roots                     string
			scannedAt, createdAt, updatedAt int64
		)
		if err := rows.Scan(&s.ID, &s.Title, &kind, &roots, &scannedAt, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan section: %w", err)
		}
		s.Kind = domain.SectionKind(kind)
		if roots != "" {
			s.Roots = strings.Split(roots, "\n")
		}
		s.ScannedAt, s.CreatedAt, s.UpdatedAt = fromMillis(scannedAt), fromMillis(createdAt), fromMillis(updatedAt)
		out = append(out, s)
	}
	return out, rows.Err()
}

// Section reads one library.
func (d *DB) Section(ctx context.Context, id int64) (*domain.Section, error) {
	sections, err := d.Sections(ctx)
	if err != nil {
		return nil, err
	}
	for i := range sections {
		if sections[i].ID == id {
			return &sections[i], nil
		}
	}
	return nil, ErrNotFound
}

// MarkSectionScanned records when a scan of this section finished.
func (d *DB) MarkSectionScanned(ctx context.Context, id int64, at time.Time) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE sections SET scanned_at = ?, updated_at = ? WHERE id = ?`, millis(at), millis(at), id)
	if err != nil {
		return fmt.Errorf("mark scanned: %w", err)
	}
	return nil
}

const itemColumns = `id, section_id, kind, parent_id, title, sort_title, year, idx, summary, duration_ms, added_at, updated_at, provider_id, poster_key, art_key, metadata_at`

func scanItem(rows interface{ Scan(...any) error }) (domain.Item, error) {
	var (
		it                             domain.Item
		kind                           string
		parent                         sql.NullInt64
		durationMS                     int64
		addedAt, updatedAt, metadataAt int64
	)
	if err := rows.Scan(&it.ID, &it.SectionID, &kind, &parent, &it.Title, &it.SortTitle,
		&it.Year, &it.Index, &it.Summary, &durationMS, &addedAt, &updatedAt,
		&it.ProviderID, &it.PosterKey, &it.ArtKey, &metadataAt); err != nil {
		return domain.Item{}, err
	}
	it.MetadataAt = fromMillis(metadataAt)
	it.Kind = domain.Kind(kind)
	if parent.Valid {
		id := parent.Int64
		it.ParentID = &id
	}
	it.Duration = time.Duration(durationMS) * time.Millisecond
	it.AddedAt, it.UpdatedAt = fromMillis(addedAt), fromMillis(updatedAt)
	return it, nil
}

// SectionItems lists a section's top level: its movies, or its shows.
//
// Returns the page and the total, because a client that is paging needs to
// know how far it can page and Plex's MediaContainer carries both.
func (d *DB) SectionItems(ctx context.Context, sectionID int64, kind domain.Kind, page Page) ([]domain.Item, int, error) {
	var total int
	if err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM items WHERE section_id = ? AND kind = ?`, sectionID, string(kind)).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count section items: %w", err)
	}

	offset, limit := page.Window(500)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items WHERE section_id = ? AND kind = ?
		 ORDER BY sort_title, year LIMIT ? OFFSET ?`,
		sectionID, string(kind), limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list section items: %w", err)
	}
	defer rows.Close()

	items, err := d.collect(ctx, rows)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// Children lists a parent's children in order: a show's seasons, a season's
// episodes.
func (d *DB) Children(ctx context.Context, parentID int64, page Page) ([]domain.Item, int, error) {
	var total int
	if err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM items WHERE parent_id = ?`, parentID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count children: %w", err)
	}

	offset, limit := page.Window(500)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items WHERE parent_id = ? ORDER BY idx, sort_title LIMIT ? OFFSET ?`,
		parentID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list children: %w", err)
	}
	defer rows.Close()

	items, err := d.collect(ctx, rows)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// Item reads one item, with its parts when it is playable.
func (d *DB) Item(ctx context.Context, id int64) (*domain.Item, error) {
	row := d.sql.QueryRowContext(ctx, `SELECT `+itemColumns+` FROM items WHERE id = ?`, id)
	it, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read item: %w", err)
	}
	if it.Playable() {
		parts, err := d.Parts(ctx, it.ID)
		if err != nil {
			return nil, err
		}
		it.Parts = parts
	}
	return &it, nil
}

// Search matches titles across every section, for the client's search bar.
//
// LIKE rather than FTS5 while the library is one household's: the index would
// be another table to keep in step with the scanner, and a prefix match over
// a few tens of thousands of rows is already instant. Worth revisiting if this
// ever serves a library large enough to notice.
func (d *DB) Search(ctx context.Context, query string, page Page) ([]domain.Item, int, error) {
	pattern := "%" + strings.ToLower(query) + "%"
	var total int
	if err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM items WHERE kind IN ('movie','show','episode') AND LOWER(title) LIKE ?`,
		pattern).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count search: %w", err)
	}

	offset, limit := page.Window(100)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items WHERE kind IN ('movie','show','episode') AND LOWER(title) LIKE ?
		 ORDER BY CASE kind WHEN 'movie' THEN 0 WHEN 'show' THEN 1 ELSE 2 END, sort_title LIMIT ? OFFSET ?`,
		pattern, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	items, err := d.collect(ctx, rows)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// collect drains a rows iterator into items, attaching parts to the playable
// ones in a single follow-up query rather than one per item.
func (d *DB) collect(ctx context.Context, rows *sql.Rows) ([]domain.Item, error) {
	var (
		items    []domain.Item
		playable []int64
	)
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, fmt.Errorf("scan item: %w", err)
		}
		if it.Playable() {
			playable = append(playable, it.ID)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(playable) == 0 {
		return items, nil
	}

	byItem, err := d.partsFor(ctx, playable)
	if err != nil {
		return nil, err
	}
	for i := range items {
		if parts := byItem[items[i].ID]; len(parts) > 0 {
			items[i].Parts = parts
		}
	}
	return items, nil
}

// Parts lists the files behind one item.
func (d *DB) Parts(ctx context.Context, itemID int64) ([]domain.Part, error) {
	byItem, err := d.partsFor(ctx, []int64{itemID})
	if err != nil {
		return nil, err
	}
	return byItem[itemID], nil
}

// Part reads one file row, which is what a stream request resolves.
func (d *DB) Part(ctx context.Context, id int64) (*domain.Part, error) {
	var (
		p          domain.Part
		modifiedAt int64
	)
	err := d.sql.QueryRowContext(ctx,
		`SELECT id, item_id, path, size, container, modified_at FROM parts WHERE id = ?`, id).
		Scan(&p.ID, &p.ItemID, &p.Path, &p.Size, &p.Container, &modifiedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read part: %w", err)
	}
	p.ModifiedAt = fromMillis(modifiedAt)
	return &p, nil
}

func (d *DB) partsFor(ctx context.Context, itemIDs []int64) (map[int64][]domain.Part, error) {
	if len(itemIDs) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(itemIDs)), ",")
	args := make([]any, 0, len(itemIDs))
	for _, id := range itemIDs {
		args = append(args, id)
	}

	rows, err := d.sql.QueryContext(ctx,
		`SELECT id, item_id, path, size, container, modified_at FROM parts WHERE item_id IN (`+placeholders+`) ORDER BY id`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("list parts: %w", err)
	}
	defer rows.Close()

	out := make(map[int64][]domain.Part, len(itemIDs))
	for rows.Next() {
		var (
			p          domain.Part
			modifiedAt int64
		)
		if err := rows.Scan(&p.ID, &p.ItemID, &p.Path, &p.Size, &p.Container, &modifiedAt); err != nil {
			return nil, fmt.Errorf("scan part: %w", err)
		}
		p.ModifiedAt = fromMillis(modifiedAt)
		out[p.ItemID] = append(out[p.ItemID], p)
	}
	return out, rows.Err()
}
