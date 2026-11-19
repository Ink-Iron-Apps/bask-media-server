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

const itemColumns = `id, section_id, kind, parent_id, title, sort_title, year, idx, disc, summary, duration_ms, added_at, updated_at, provider_id, poster_key, art_key, metadata_at, taken_at, performer`

func scanItem(rows interface{ Scan(...any) error }) (domain.Item, error) {
	var (
		it                                      domain.Item
		kind                                    string
		parent                                  sql.NullInt64
		durationMS                              int64
		addedAt, updatedAt, metadataAt, takenAt int64
	)
	if err := rows.Scan(&it.ID, &it.SectionID, &kind, &parent, &it.Title, &it.SortTitle,
		&it.Year, &it.Index, &it.Disc, &it.Summary, &durationMS, &addedAt, &updatedAt,
		&it.ProviderID, &it.PosterKey, &it.ArtKey, &metadataAt, &takenAt, &it.Performer); err != nil {
		return domain.Item{}, err
	}
	it.MetadataAt, it.TakenAt = fromMillis(metadataAt), fromMillis(takenAt)
	it.Kind = domain.Kind(kind)
	if parent.Valid {
		id := parent.Int64
		it.ParentID = &id
	}
	it.Duration = time.Duration(durationMS) * time.Millisecond
	it.AddedAt, it.UpdatedAt = fromMillis(addedAt), fromMillis(updatedAt)
	return it, nil
}

// SectionItems lists a section's top level: its films, shows, artists or
// albums of photographs.
//
// Only the rows with nothing above them, which matters for photographs: every
// level of a folder tree is an album, and a library's top level is the
// outermost ones. Without that a library of a hundred nested folders opens on
// a hundred albums with no shape to them.
//
// Returns the page and the total, because a client that is paging needs to
// know how far it can page and Plex's MediaContainer carries both.
func (d *DB) SectionItems(ctx context.Context, sectionID int64, kind domain.Kind, page Page) ([]domain.Item, int, error) {
	var total int
	if err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM items WHERE section_id = ? AND kind = ? AND parent_id IS NULL`,
		sectionID, string(kind)).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count section items: %w", err)
	}

	offset, limit := page.Window(500)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items WHERE section_id = ? AND kind = ? AND parent_id IS NULL
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

// Children lists a parent's children in the order the thing above them puts
// them in: a show's seasons and a season's episodes by number, an artist's
// albums by year, an album's tracks by disc and then track number, and a
// photo album's contents by when the photographs were taken.
//
// One ORDER BY for all of them, because every level sets exactly the columns
// that order it and leaves the rest at zero: a season has an index and no
// disc, a track has both, a photograph has neither and a time instead. The
// alternative is a query per kind, which is five queries that have to be kept
// agreeing with each other.
func (d *DB) Children(ctx context.Context, parentID int64, page Page) ([]domain.Item, int, error) {
	var total int
	if err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM items WHERE parent_id = ?`, parentID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count children: %w", err)
	}

	offset, limit := page.Window(500)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items WHERE parent_id = ?
		 ORDER BY disc, idx, taken_at, year, sort_title LIMIT ? OFFSET ?`,
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
		`SELECT COUNT(*) FROM items WHERE kind IN `+searchableKinds+` AND LOWER(title) LIKE ?`,
		pattern).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count search: %w", err)
	}

	offset, limit := page.Window(100)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items WHERE kind IN `+searchableKinds+` AND LOWER(title) LIKE ?
		 ORDER BY CASE kind
		     WHEN 'movie' THEN 0 WHEN 'show' THEN 1 WHEN 'artist' THEN 2
		     WHEN 'album' THEN 3 WHEN 'photoalbum' THEN 4 WHEN 'episode' THEN 5 ELSE 6
		   END, sort_title LIMIT ? OFFSET ?`,
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

// searchableKinds is what a search bar looks through.
//
// Individual photographs are left out on purpose: their titles are camera
// filenames, so a library of them would answer every query with a thousand
// IMG_0042s and bury the album somebody was looking for.
const searchableKinds = `('movie','show','episode','artist','album','track','photoalbum')`

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

const partColumns = `id, item_id, path, size, container, modified_at, width, height, orientation`

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
		`SELECT `+partColumns+` FROM parts WHERE id = ?`, id).
		Scan(&p.ID, &p.ItemID, &p.Path, &p.Size, &p.Container, &modifiedAt,
			&p.Width, &p.Height, &p.Orientation)
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
		`SELECT `+partColumns+` FROM parts WHERE item_id IN (`+placeholders+`) ORDER BY id`,
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
		if err := rows.Scan(&p.ID, &p.ItemID, &p.Path, &p.Size, &p.Container, &modifiedAt,
			&p.Width, &p.Height, &p.Orientation); err != nil {
			return nil, fmt.Errorf("scan part: %w", err)
		}
		p.ModifiedAt = fromMillis(modifiedAt)
		out[p.ItemID] = append(out[p.ItemID], p)
	}
	return out, rows.Err()
}

// ItemsByIDs reads a set of items in one query, for a presenter that needs the
// containers above a page of rows.
//
// A client listing an album's tracks wants the artist and the album on every
// row, and a client listing episodes wants the show and the season: one query
// for the page's parents beats a point read per row, which on a sixty-track
// album is sixty round trips to learn two names.
func (d *DB) ItemsByIDs(ctx context.Context, ids []int64) (map[int64]domain.Item, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	// Deduplicated first: a page of tracks from one album asks for the same
	// parent on every row.
	wanted := make([]any, 0, len(ids))
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if _, already := seen[id]; already || id == 0 {
			continue
		}
		seen[id] = struct{}{}
		wanted = append(wanted, id)
	}
	if len(wanted) == 0 {
		return nil, nil
	}

	out := make(map[int64]domain.Item, len(wanted))
	const chunk = 200
	for start := 0; start < len(wanted); start += chunk {
		end := min(start+chunk, len(wanted))
		batch := wanted[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		rows, err := d.sql.QueryContext(ctx,
			`SELECT `+itemColumns+` FROM items WHERE id IN (`+placeholders+`)`, batch...)
		if err != nil {
			return nil, fmt.Errorf("read items: %w", err)
		}
		for rows.Next() {
			it, err := scanItem(rows)
			if err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan item: %w", err)
			}
			out[it.ID] = it
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ChildCounts is how many things sit directly under each of these items, for
// the "12 episodes" or "40 photographs" a client puts under a title.
func (d *DB) ChildCounts(ctx context.Context, ids []int64) (map[int64]int, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := d.sql.QueryContext(ctx,
		`SELECT parent_id, COUNT(*) FROM items WHERE parent_id IN (`+placeholders+`) GROUP BY parent_id`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("count children: %w", err)
	}
	defer rows.Close()

	out := make(map[int64]int, len(ids))
	for rows.Next() {
		var (
			parent int64
			n      int
		)
		if err := rows.Scan(&parent, &n); err != nil {
			return nil, fmt.Errorf("count children: %w", err)
		}
		out[parent] = n
	}
	return out, rows.Err()
}

// AlbumCover is the photograph that stands for a photo album: the earliest one
// anywhere inside it, however many levels down.
//
// Resolved on request rather than stored, because a photo album has no poster
// of its own — the cover is one of the files in it, and which file that is
// changes as photographs are added. A recursive walk of the album tree in
// SQLite, which is a handful of rows: the depth of a folder tree, not the size
// of a library.
func (d *DB) AlbumCover(ctx context.Context, itemID int64) (*domain.Part, error) {
	var (
		p          domain.Part
		modifiedAt int64
	)
	err := d.sql.QueryRowContext(ctx,
		`WITH RECURSIVE tree(id) AS (
		     SELECT ?
		     UNION
		     SELECT items.id FROM items JOIN tree ON items.parent_id = tree.id
		 )
		 SELECT `+prefixed("parts", partColumns)+`
		   FROM parts
		   JOIN items ON items.id = parts.item_id
		  WHERE items.kind = 'photo' AND items.id IN (SELECT id FROM tree)
		  ORDER BY items.taken_at, items.id
		  LIMIT 1`, itemID).
		Scan(&p.ID, &p.ItemID, &p.Path, &p.Size, &p.Container, &modifiedAt,
			&p.Width, &p.Height, &p.Orientation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read album cover: %w", err)
	}
	p.ModifiedAt = fromMillis(modifiedAt)
	return &p, nil
}

// SectionItemsOfKind lists everything of one kind in a library, however deep
// it sits: a show library's episodes, a music library's tracks.
//
// A client's "all episodes" row asks for exactly this, and walking the tree
// to build it would be a query per show and a query per season.
func (d *DB) SectionItemsOfKind(ctx context.Context, sectionID int64, kind domain.Kind, limit int) ([]domain.Item, error) {
	if limit <= 0 || limit > 2000 {
		limit = 1000
	}
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items WHERE section_id = ? AND kind = ?
		 ORDER BY sort_title, disc, idx, taken_at LIMIT ?`,
		sectionID, string(kind), limit)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", kind, err)
	}
	defer rows.Close()
	return d.collect(ctx, rows)
}

// RecentlyAdded is what arrived last, newest first, for the row every client
// puts at the top of a home screen.
//
// Only the things that are watched or listened to, never the containers: a
// show whose season folder was created today is not news, and the episode in
// it is.
func (d *DB) RecentlyAdded(ctx context.Context, limit int) ([]domain.Item, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items
		  WHERE kind IN ('movie','episode','album','photo')
		  ORDER BY added_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("recently added: %w", err)
	}
	defer rows.Close()
	return d.collect(ctx, rows)
}

// NextUp is the next unwatched episode of each show this viewer has started.
//
// The question a television asks on its home screen, and the one BMS already
// has the rows to answer: for every show with a watched episode, the lowest
// season-and-episode that has not been watched. One query rather than a walk
// per show, because a library of forty shows would otherwise be forty round
// trips before anything is drawn.
func (d *DB) NextUp(ctx context.Context, userID int64, limit int) ([]domain.Item, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := d.sql.QueryContext(ctx,
		`WITH watched AS (
		     SELECT season.parent_id AS show_id, MAX(p.updated_at) AS seen_at
		       FROM progress p
		       JOIN items episode ON episode.id = p.item_id AND episode.kind = 'episode'
		       JOIN items season ON season.id = episode.parent_id
		      WHERE p.user_id = ? AND (p.watched = 1 OR p.offset_ms > 0)
		      GROUP BY season.parent_id
		 ),
		 candidates AS (
		     SELECT `+prefixed("episode", itemColumns)+`, watched.seen_at AS seen_at,
		            ROW_NUMBER() OVER (
		              PARTITION BY season.parent_id ORDER BY season.idx, episode.idx
		            ) AS rank
		       FROM items episode
		       JOIN items season ON season.id = episode.parent_id
		       JOIN watched ON watched.show_id = season.parent_id
		       LEFT JOIN progress p ON p.item_id = episode.id AND p.user_id = ?
		      WHERE episode.kind = 'episode' AND IFNULL(p.watched, 0) = 0
		 )
		 SELECT `+itemColumns+` FROM candidates WHERE rank = 1
		  ORDER BY seen_at DESC LIMIT ?`,
		userID, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("next up: %w", err)
	}
	defer rows.Close()
	return d.collect(ctx, rows)
}
