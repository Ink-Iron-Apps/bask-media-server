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

	// Genres narrows a listing to the items carrying any one of them, and
	// empty means every item. A filter rather than a window, carried here
	// because every listing already takes a Page and a client that is
	// filtering is paging through the filtered list rather than the whole
	// one: the two cannot be decided apart.
	//
	// Any one of them rather than all, because that is what a client's
	// filter menu means when somebody ticks two boxes.
	Genres []string

	// Words is the other kinds a client can narrow by: a label, a tag, a
	// mood, a style. One entry per menu somebody used.
	//
	// Values within an entry are OR — ticking two boxes in one menu — and
	// the entries themselves are AND, because ticking a box in two menus
	// means both. An entry naming several kinds matches a thing carrying the
	// word as any of them, which is what Jellyfin's single Tags parameter
	// means when BMS keeps labels and tags apart.
	Words []WordFilter

	// Sort is the order to list in, and the empty key means by title, which
	// is the order every listing had before clients could ask. Carried here
	// with the window for the same reason the filter is: a client paging
	// through a sorted list is paging through that order, and the two
	// cannot be decided apart.
	Sort SortKey
	Desc bool

	// People narrows to what somebody worked on. One entry per menu used:
	// the entries are AND and the names within one are OR, for the same
	// reason the words are. An entry naming several roles matches somebody
	// who did any of them, which is what a wire that does not say which
	// role it means asks for.
	People []CreditFilter

	// Studio narrows to who made it. One value, because a thing has one
	// studio and a menu row is a single choice.
	Studio string
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
	// Sections only: a library has no age rating of its own, and the items
	// inside it are filtered when they are asked for.
	only, args := d.onlySections("id")
	rows, err := d.sql.QueryContext(ctx,
		`SELECT id, title, kind, roots, scanned_at, created_at, updated_at, settings, swept_at
		   FROM sections WHERE 1 = 1`+only+` ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list sections: %w", err)
	}
	defer rows.Close()

	var out []domain.Section
	for rows.Next() {
		var (
			s                                        domain.Section
			kind, roots, settings                    string
			scannedAt, createdAt, updatedAt, sweptAt int64
		)
		if err := rows.Scan(&s.ID, &s.Title, &kind, &roots, &scannedAt, &createdAt, &updatedAt,
			&settings, &sweptAt); err != nil {
			return nil, fmt.Errorf("scan section: %w", err)
		}
		s.Kind = domain.SectionKind(kind)
		if roots != "" {
			s.Roots = strings.Split(roots, "\n")
		}
		// Settings that cannot be read are the defaults rather than an
		// error: a library whose settings column was corrupted should still
		// be browsable and playable, and the page that rewrites them is the
		// place to notice.
		s.Settings = parseSectionSettings(settings)
		s.ScannedAt, s.CreatedAt, s.UpdatedAt = fromMillis(scannedAt), fromMillis(createdAt), fromMillis(updatedAt)
		s.SweptAt = fromMillis(sweptAt)
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

const itemColumns = `id, section_id, kind, parent_id, title, sort_title, year, idx, disc, summary, duration_ms, added_at, updated_at, provider_id, poster_key, art_key, metadata_at, taken_at, performer, locked, extra, rating, rating_age, studio, aired, airs_after_season, airs_before_season, airs_before_episode, placed_by_hand`

func scanItem(rows interface{ Scan(...any) error }) (domain.Item, error) {
	var (
		it                                      domain.Item
		kind                                    string
		parent                                  sql.NullInt64
		durationMS                              int64
		addedAt, updatedAt, metadataAt, takenAt int64
		locked                                  int
		extra                                   string
		aired                                   int64
		placedByHand                            int
	)
	if err := rows.Scan(&it.ID, &it.SectionID, &kind, &parent, &it.Title, &it.SortTitle,
		&it.Year, &it.Index, &it.Disc, &it.Summary, &durationMS, &addedAt, &updatedAt,
		&it.ProviderID, &it.PosterKey, &it.ArtKey, &metadataAt, &takenAt, &it.Performer,
		&locked, &extra, &it.Rating, &it.RatingAge, &it.Studio,
		&aired, &it.Placement.AfterSeason, &it.Placement.BeforeSeason,
		&it.Placement.BeforeEpisode, &placedByHand); err != nil {
		return domain.Item{}, err
	}
	it.Locked = locked != 0
	it.Placement.ByHand = placedByHand != 0
	it.Aired = fromMillis(aired)
	it.Extra = domain.ExtraKind(extra)
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
	if !d.canSee(sectionID) {
		return nil, 0, nil
	}
	// The library is already known to be allowed; the rating still has to be
	// applied to what is inside it.
	rated, ceiling := d.onlyRated("section_id")
	genre, wanted := d.genreClause(ctx, page.Genres)
	// One predicate per menu used, so two menus narrow to the things that
	// satisfy both.
	for _, filter := range page.Words {
		clause, args := d.wordClause(ctx, filter)
		genre += clause
		wanted = append(wanted, args...)
	}
	for _, filter := range page.People {
		clause, args := peopleClause(filter, d.peopleNamesFor(ctx, filter.Names))
		genre += clause
		wanted = append(wanted, args...)
	}
	if page.Studio != "" {
		genre += " AND studio = ?"
		wanted = append(wanted, page.Studio)
	}

	var total int
	count := append([]any{sectionID, string(kind)}, ceiling...)
	count = append(count, wanted...)
	if err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM items WHERE section_id = ? AND kind = ? AND parent_id IS NULL`+rated+genre,
		count...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count section items: %w", err)
	}

	offset, limit := page.Window(500)
	args := append([]any{sectionID, string(kind)}, ceiling...)
	args = append(args, wanted...)
	args = append(args, limit, offset)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items WHERE section_id = ? AND kind = ? AND parent_id IS NULL`+rated+genre+`
		 ORDER BY `+orderBy(page.Sort, page.Desc)+` LIMIT ? OFFSET ?`,
		args...)
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
	only, scope := d.only("section_id")

	// Extras are left out: a season's episodes are its children and so is a
	// trailer, and a client asking for episodes must not be handed one. They
	// are read on their own, by [DB.Extras].
	var total int
	if err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM items WHERE parent_id = ? AND extra = ''`+only,
		append([]any{parentID}, scope...)...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count children: %w", err)
	}

	offset, limit := page.Window(500)
	args := append([]any{parentID}, scope...)
	args = append(args, limit, offset)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items WHERE parent_id = ? AND extra = ''`+only+`
		 ORDER BY disc, idx, taken_at, year, sort_title LIMIT ? OFFSET ?`,
		args...)
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
	only, scope := d.only("section_id")
	row := d.sql.QueryRowContext(ctx,
		`SELECT `+itemColumns+` FROM items WHERE id = ?`+only,
		append([]any{id}, scope...)...)
	it, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read item: %w", err)
	}
	// Every kind, because this is one item read whole: an item page draws
	// all of them, and five queries for one row is five queries.
	words, err := d.allTagsFor(ctx, []int64{it.ID})
	if err != nil {
		return nil, err
	}
	it.Genres = words[TagGenre][it.ID]
	it.Tags = words[TagTag][it.ID]
	it.Labels = words[TagLabel][it.ID]
	it.Moods = words[TagMood][it.ID]
	it.Styles = words[TagStyle][it.ID]

	// And the credits, for the same reason: this is one item read whole, and
	// a detail page draws all of them.
	credits, err := d.Credits(ctx, it.ID)
	if err != nil {
		return nil, err
	}
	for _, credit := range credits {
		it.People = append(it.People, credit.Credit)
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
	only, scope := d.only("section_id")

	var total int
	if err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM items WHERE kind IN `+searchableKinds+` AND LOWER(title) LIKE ?`+only,
		append([]any{pattern}, scope...)...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count search: %w", err)
	}

	offset, limit := page.Window(100)
	args := append([]any{pattern}, scope...)
	args = append(args, limit, offset)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items WHERE kind IN `+searchableKinds+` AND LOWER(title) LIKE ?`+only+`
		 ORDER BY CASE kind
		     WHEN 'movie' THEN 0 WHEN 'show' THEN 1 WHEN 'artist' THEN 2
		     WHEN 'album' THEN 3 WHEN 'photoalbum' THEN 4 WHEN 'episode' THEN 5 ELSE 6
		   END, sort_title LIMIT ? OFFSET ?`,
		args...)
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
	if len(items) == 0 {
		return items, nil
	}

	// Genres for the whole page in one query, for the same reason the parts
	// below are: a grid of forty films draws forty lines of chips, and a
	// query per row would make listing a library cost a round trip per
	// poster.
	all := make([]int64, 0, len(items))
	for _, it := range items {
		all = append(all, it.ID)
	}
	genres, err := d.genresFor(ctx, all)
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i].Genres = genres[items[i].ID]
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

const partColumns = `id, item_id, path, size, container, modified_at, width, height, orientation, edition`

// Parts lists the files behind one item.
func (d *DB) Parts(ctx context.Context, itemID int64) ([]domain.Part, error) {
	byItem, err := d.partsFor(ctx, []int64{itemID})
	if err != nil {
		return nil, err
	}
	return byItem[itemID], nil
}

// Part reads one file row, which is what a stream request resolves.
//
// Joined to its item rather than read on its own, because a part id is a
// number a client can hold from before a library was hidden from it and a
// Plex stream addresses the file directly. Without the join this is the one
// door left open in a locked house.
func (d *DB) Part(ctx context.Context, id int64) (*domain.Part, error) {
	only, scope := d.only("items.section_id")
	var (
		p          domain.Part
		modifiedAt int64
	)
	err := d.sql.QueryRowContext(ctx,
		`SELECT `+prefixed("parts", partColumns)+`
		   FROM parts JOIN items ON items.id = parts.item_id
		  WHERE parts.id = ?`+only,
		append([]any{id}, scope...)...).
		Scan(&p.ID, &p.ItemID, &p.Path, &p.Size, &p.Container, &modifiedAt,
			&p.Width, &p.Height, &p.Orientation, &p.Edition)
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
		// The plain file first, then the editions by name. A client plays
		// the first without asking, and the cut somebody did not label is
		// the one they meant by "the film".
		`SELECT `+partColumns+` FROM parts WHERE item_id IN (`+placeholders+`)
		  ORDER BY item_id, edition <> '' , edition, id`,
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
			&p.Width, &p.Height, &p.Orientation, &p.Edition); err != nil {
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
		only, scope := d.only("section_id")
		rows, err := d.sql.QueryContext(ctx,
			`SELECT `+itemColumns+` FROM items WHERE id IN (`+placeholders+`)`+only,
			append(append([]any{}, batch...), scope...)...)
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
	only, scope := d.only("section_id")
	args = append(args, scope...)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT parent_id, COUNT(*) FROM items WHERE parent_id IN (`+placeholders+`)`+only+`
		  GROUP BY parent_id`,
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
	only, scope := d.only("items.section_id")
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
		  WHERE items.kind = 'photo' AND items.id IN (SELECT id FROM tree)`+only+`
		  ORDER BY items.taken_at, items.id
		  LIMIT 1`, append([]any{itemID}, scope...)...).
		Scan(&p.ID, &p.ItemID, &p.Path, &p.Size, &p.Container, &modifiedAt,
			&p.Width, &p.Height, &p.Orientation, &p.Edition)
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
func (d *DB) SectionItemsOfKind(ctx context.Context, sectionID int64, kind domain.Kind, genres []string, limit int) ([]domain.Item, error) {
	if !d.canSee(sectionID) {
		return nil, nil
	}
	if limit <= 0 || limit > 2000 {
		limit = 1000
	}
	rated, ceiling := d.onlyRated("section_id")
	genre, wanted := d.genreClause(ctx, genres)
	args := append([]any{sectionID, string(kind)}, ceiling...)
	args = append(args, wanted...)
	args = append(args, limit)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items WHERE section_id = ? AND kind = ?`+rated+genre+`
		 ORDER BY sort_title, disc, idx, taken_at LIMIT ?`,
		args...)
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
	only, scope := d.only("section_id")
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items
		  WHERE kind IN ('movie','episode','album','photo')`+only+`
		  ORDER BY added_at DESC, id DESC LIMIT ?`,
		append(scope, limit)...)
	if err != nil {
		return nil, fmt.Errorf("recently added: %w", err)
	}
	defer rows.Close()
	return d.collect(ctx, rows)
}

// NextUp is the next unwatched episode of each show this viewer has started.
//
// The question a television asks on its home screen. One query finds the
// candidate per show — a library of forty shows must not be forty round
// trips before anything is drawn — and then only the shows whose candidate
// might be wrong are looked at again.
//
// It might be wrong because the query orders by season and then episode,
// which puts a show's specials before its pilot. A show with no specials is
// unaffected and most shows have none, so the second look is paid for only
// where it buys something: see [DB.nextWovenInto].
func (d *DB) NextUp(ctx context.Context, userID int64, limit int) ([]domain.Item, error) {
	only, scope := d.only("episode.section_id")
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
		      WHERE episode.kind = 'episode' AND IFNULL(p.watched, 0) = 0`+only+`
		 )
		 SELECT `+itemColumns+` FROM candidates WHERE rank = 1
		  ORDER BY seen_at DESC LIMIT ?`,
		append([]any{userID, userID}, append(scope, limit)...)...)
	if err != nil {
		return nil, fmt.Errorf("next up: %w", err)
	}
	defer rows.Close()

	candidates, err := d.collect(ctx, rows)
	if err != nil {
		return nil, err
	}
	return d.nextWovenInto(ctx, userID, candidates)
}

// nextWovenInto corrects the candidates for the shows that have specials.
//
// The query that found them orders by season and then episode, so for a show
// with specials it may have offered one of those rather than the episode
// that actually comes next. Rather than express the whole placement rule as
// a window function, the shows with specials are re-read in the order they
// should be watched and their candidate replaced.
//
// Only those shows. A show with no season zero is already correct, which is
// most of them, so this costs nothing on a library of ordinary series.
func (d *DB) nextWovenInto(ctx context.Context, userID int64, candidates []domain.Item) ([]domain.Item, error) {
	if len(candidates) == 0 {
		return candidates, nil
	}

	// Which show each candidate belongs to, and which of those shows have
	// specials at all.
	shows := make(map[int]int64, len(candidates))
	for i, candidate := range candidates {
		if candidate.ParentID == nil {
			continue
		}
		season, err := d.Item(ctx, *candidate.ParentID)
		if err != nil {
			continue
		}
		if season.ParentID == nil {
			continue
		}
		var specials int
		if err := d.sql.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM items
			  WHERE parent_id = ? AND kind = 'season' AND idx = 0`,
			*season.ParentID).Scan(&specials); err != nil {
			return nil, fmt.Errorf("next up: %w", err)
		}
		if specials > 0 {
			shows[i] = *season.ParentID
		}
	}
	if len(shows) == 0 {
		return candidates, nil
	}

	out := append([]domain.Item(nil), candidates...)
	for i, showID := range shows {
		episodes, err := d.EpisodeOrder(ctx, showID)
		if err != nil {
			return nil, err
		}
		ids := make([]int64, 0, len(episodes))
		for _, episode := range episodes {
			ids = append(ids, episode.ID)
		}
		seen, err := d.Progress(ctx, userID, ids)
		if err != nil {
			return nil, err
		}
		for _, episode := range episodes {
			if progress, found := seen[episode.ID]; found && progress.Watched {
				continue
			}
			out[i] = episode
			break
		}
	}
	return out, nil
}

// Extras are the trailers, featurettes and deleted scenes that belong to an
// item.
//
// Ordered by sort rather than by name, because that is how a client draws
// them: the trailer first, because that is what anybody is looking for, and
// the rest in a stable order after it. Read through the scoped handle like
// everything else, so an extra of a film in a library this device cannot see
// does not come back on its own.
func (d *DB) Extras(ctx context.Context, parentID int64) ([]domain.Item, error) {
	only, scope := d.only("section_id")
	args := append([]any{parentID}, scope...)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items
		  WHERE parent_id = ? AND extra <> ''`+only+`
		  ORDER BY CASE extra
		      WHEN 'trailer' THEN 0 WHEN 'scene' THEN 1 WHEN 'deletedScene' THEN 2
		      WHEN 'featurette' THEN 3 WHEN 'behindTheScenes' THEN 4
		      WHEN 'interview' THEN 5 WHEN 'short' THEN 6 ELSE 7
		    END, sort_title, id`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("list extras: %w", err)
	}
	defer rows.Close()
	return d.collect(ctx, rows)
}

// ExtraCounts is how many extras each of these items has, in one query, for a
// listing that wants to say so without reading them.
func (d *DB) ExtraCounts(ctx context.Context, ids []int64) (map[int64]int, error) {
	counts := map[int64]int{}
	if len(ids) == 0 {
		return counts, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := d.sql.QueryContext(ctx,
		`SELECT parent_id, COUNT(*) FROM items
		  WHERE extra <> '' AND parent_id IN (`+placeholders+`)
		  GROUP BY parent_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("count extras: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var parent sql.NullInt64
		var count int
		if err := rows.Scan(&parent, &count); err != nil {
			return nil, fmt.Errorf("count extras: %w", err)
		}
		if parent.Valid {
			counts[parent.Int64] = count
		}
	}
	return counts, rows.Err()
}
