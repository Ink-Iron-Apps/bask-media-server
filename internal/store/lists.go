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

// The three things a library needs besides the files: what somebody marked,
// what somebody arranged, and how the library groups itself.
//
// Favourites and playlists belong to an account — whether a film is a
// favourite is an opinion. A collection belongs to the library, because
// "these four films are one story" is a fact about the films.
//
// All three obey the handle's library restriction, and that is the part worth
// reading twice: a playlist can hold a track from a library this device
// cannot see, and the entry has to vanish from the listing without the
// playlist itself disappearing. A collection whose every film is hidden is
// hidden too, because an empty group tells a viewer something exists.

/* ---------- favourites ---------- */

// SetFavourite marks or unmarks one item for one account.
//
// Silently does nothing for an item this handle cannot see, rather than
// reporting a failure: a client that can ask about an item it was never shown
// is a client guessing, and the honest answer to a guess is nothing at all.
func (d *DB) SetFavourite(ctx context.Context, userID, itemID int64, favourite bool) error {
	visible, err := d.sees(ctx, itemID)
	if err != nil || !visible {
		return err
	}
	if !favourite {
		_, err := d.sql.ExecContext(ctx,
			`DELETE FROM favourites WHERE user_id = ? AND item_id = ?`, userID, itemID)
		if err != nil {
			return fmt.Errorf("unfavourite: %w", err)
		}
		return nil
	}
	_, err = d.sql.ExecContext(ctx,
		`INSERT INTO favourites (user_id, item_id, created_at) VALUES (?, ?, ?)
		 ON CONFLICT(user_id, item_id) DO NOTHING`,
		userID, itemID, millis(time.Now().UTC()))
	if err != nil {
		return fmt.Errorf("favourite: %w", err)
	}
	return nil
}

// Favourites lists what an account marked, newest first.
func (d *DB) Favourites(ctx context.Context, userID int64, page Page) ([]domain.Item, int, error) {
	only, scope := d.only("i.section_id")

	var total int
	if err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM favourites f JOIN items i ON i.id = f.item_id
		  WHERE f.user_id = ?`+only,
		append([]any{userID}, scope...)...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count favourites: %w", err)
	}

	offset, limit := page.Window(500)
	args := append([]any{userID}, scope...)
	args = append(args, limit, offset)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+prefixed("i", itemColumns)+`
		   FROM favourites f JOIN items i ON i.id = f.item_id
		  WHERE f.user_id = ?`+only+`
		  ORDER BY f.created_at DESC, i.id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list favourites: %w", err)
	}
	defer rows.Close()

	items, err := d.collect(ctx, rows)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// FavouriteSet reports which of these items an account has marked, for
// decorating a listing in one query rather than one per row.
func (d *DB) FavouriteSet(ctx context.Context, userID int64, itemIDs []int64) (map[int64]bool, error) {
	if len(itemIDs) == 0 {
		return nil, nil
	}
	out := make(map[int64]bool, len(itemIDs))
	// Chunked for the same reason every other fan-out here is: a client can
	// ask for a 500-item page.
	const chunk = 200
	for start := 0; start < len(itemIDs); start += chunk {
		end := min(start+chunk, len(itemIDs))
		placeholders, args := inClause(itemIDs[start:end])
		rows, err := d.sql.QueryContext(ctx,
			`SELECT item_id FROM favourites WHERE user_id = ? AND item_id IN (`+placeholders+`)`,
			append([]any{userID}, args...)...)
		if err != nil {
			return nil, fmt.Errorf("read favourites: %w", err)
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan favourite: %w", err)
			}
			out[id] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

/* ---------- playlists ---------- */

// CreatePlaylist starts an empty list for one account.
func (d *DB) CreatePlaylist(ctx context.Context, userID int64, title string) (*domain.Playlist, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "Playlist"
	}
	now := time.Now().UTC()
	res, err := d.sql.ExecContext(ctx,
		`INSERT INTO playlists (user_id, title, kind, created_at, updated_at) VALUES (?, ?, 'mixed', ?, ?)`,
		userID, title, millis(now), millis(now))
	if err != nil {
		return nil, fmt.Errorf("create playlist: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("create playlist: %w", err)
	}
	return &domain.Playlist{
		ID: id, UserID: userID, Title: title, Kind: domain.PlaylistMixed,
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

// Playlists lists one account's, by name.
func (d *DB) Playlists(ctx context.Context, userID int64) ([]domain.Playlist, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT p.id, p.user_id, p.title, p.kind, p.created_at, p.updated_at,
		        (SELECT COUNT(*) FROM playlist_entries e WHERE e.playlist_id = p.id) AS entries
		   FROM playlists p WHERE p.user_id = ? ORDER BY p.title, p.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list playlists: %w", err)
	}
	defer rows.Close()

	var out []domain.Playlist
	for rows.Next() {
		p, err := scanPlaylist(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Playlist reads one, which must belong to this account.
//
// A playlist is not shared, so another account asking for it is told there is
// no such playlist rather than that it may not have this one: the second
// answer tells them it exists.
func (d *DB) Playlist(ctx context.Context, userID, id int64) (*domain.Playlist, error) {
	row := d.sql.QueryRowContext(ctx,
		`SELECT p.id, p.user_id, p.title, p.kind, p.created_at, p.updated_at,
		        (SELECT COUNT(*) FROM playlist_entries e WHERE e.playlist_id = p.id) AS entries
		   FROM playlists p WHERE p.id = ? AND p.user_id = ?`, id, userID)
	p, err := scanPlaylist(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func scanPlaylist(row interface{ Scan(...any) error }) (domain.Playlist, error) {
	var (
		p                    domain.Playlist
		kind                 string
		createdAt, updatedAt int64
	)
	if err := row.Scan(&p.ID, &p.UserID, &p.Title, &kind, &createdAt, &updatedAt, &p.Entries); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Playlist{}, err
		}
		return domain.Playlist{}, fmt.Errorf("scan playlist: %w", err)
	}
	p.Kind = domain.PlaylistKind(kind)
	p.CreatedAt, p.UpdatedAt = fromMillis(createdAt), fromMillis(updatedAt)
	return p, nil
}

// RenamePlaylist changes the title.
func (d *DB) RenamePlaylist(ctx context.Context, userID, id int64, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return errors.New("a playlist needs a name")
	}
	res, err := d.sql.ExecContext(ctx,
		`UPDATE playlists SET title = ?, updated_at = ? WHERE id = ? AND user_id = ?`,
		title, millis(time.Now().UTC()), id, userID)
	if err != nil {
		return fmt.Errorf("rename playlist: %w", err)
	}
	return affected(res, "playlist")
}

// DeletePlaylist removes it and its entries. The items are untouched: a
// playlist is an arrangement of the library, never a part of it.
func (d *DB) DeletePlaylist(ctx context.Context, userID, id int64) error {
	res, err := d.sql.ExecContext(ctx,
		`DELETE FROM playlists WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("delete playlist: %w", err)
	}
	return affected(res, "playlist")
}

// AddToPlaylist appends items, in the order given, and returns how many went
// in.
//
// Items this handle cannot see are skipped rather than refused: the caller
// asked for a list of things and the answer is the ones it may have. Adding
// the same item twice is allowed and makes two entries, because that is what
// a playlist is for.
func (d *DB) AddToPlaylist(ctx context.Context, userID, id int64, itemIDs []int64) (int, error) {
	if _, err := d.Playlist(ctx, userID, id); err != nil {
		return 0, err
	}
	allowed, err := d.visibleOf(ctx, itemIDs)
	if err != nil {
		return 0, err
	}
	if len(allowed) == 0 {
		return 0, nil
	}

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("add to playlist: %w", err)
	}
	defer tx.Rollback()

	var next int64
	if err := tx.QueryRowContext(ctx,
		`SELECT IFNULL(MAX(position), 0) FROM playlist_entries WHERE playlist_id = ?`, id).
		Scan(&next); err != nil {
		return 0, fmt.Errorf("add to playlist: %w", err)
	}
	for _, itemID := range allowed {
		next += positionStep
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO playlist_entries (playlist_id, item_id, position) VALUES (?, ?, ?)`,
			id, itemID, next); err != nil {
			return 0, fmt.Errorf("add to playlist: %w", err)
		}
	}
	if err := touchPlaylist(ctx, tx, id); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("add to playlist: %w", err)
	}
	return len(allowed), nil
}

// positionStep leaves room between entries so moving one rewrites one row
// rather than renumbering the list. A playlist long enough to exhaust the gap
// is restacked by MovePlaylistEntry.
const positionStep = 1024

// RemoveFromPlaylist deletes entries by their own ids, not by item: the same
// track twice is two entries and removing "the track" would be ambiguous.
func (d *DB) RemoveFromPlaylist(ctx context.Context, userID, id int64, entryIDs []int64) (int, error) {
	if _, err := d.Playlist(ctx, userID, id); err != nil {
		return 0, err
	}
	if len(entryIDs) == 0 {
		return 0, nil
	}
	placeholders, args := inClause(entryIDs)
	res, err := d.sql.ExecContext(ctx,
		`DELETE FROM playlist_entries WHERE playlist_id = ? AND id IN (`+placeholders+`)`,
		append([]any{id}, args...)...)
	if err != nil {
		return 0, fmt.Errorf("remove from playlist: %w", err)
	}
	removed, _ := res.RowsAffected()
	if removed > 0 {
		if err := touchPlaylist(ctx, d.sql, id); err != nil {
			return int(removed), err
		}
	}
	return int(removed), nil
}

// MovePlaylistEntry puts one entry at a position in the list, counted from
// zero over the whole playlist.
//
// The whole playlist, not the visible part of it: moving an entry while a
// library is hidden would otherwise reorder things the mover cannot see.
func (d *DB) MovePlaylistEntry(ctx context.Context, userID, id, entryID int64, to int) error {
	if _, err := d.Playlist(ctx, userID, id); err != nil {
		return err
	}

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("move entry: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM playlist_entries WHERE playlist_id = ? ORDER BY position, id`, id)
	if err != nil {
		return fmt.Errorf("move entry: %w", err)
	}
	var order []int64
	for rows.Next() {
		var entry int64
		if err := rows.Scan(&entry); err != nil {
			rows.Close()
			return fmt.Errorf("move entry: %w", err)
		}
		order = append(order, entry)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	from := -1
	for i, entry := range order {
		if entry == entryID {
			from = i
		}
	}
	if from < 0 {
		return ErrNotFound
	}
	if to < 0 {
		to = 0
	}
	if to >= len(order) {
		to = len(order) - 1
	}
	if to == from {
		return tx.Commit()
	}

	order = append(order[:from], order[from+1:]...)
	moved := append([]int64{}, order[:to]...)
	moved = append(moved, entryID)
	moved = append(moved, order[to:]...)

	// Restacked rather than nudged. A playlist is a few hundred rows at the
	// outside, one statement each is nothing, and the alternative is the
	// arithmetic that eventually runs out of room between two positions.
	for i, entry := range moved {
		if _, err := tx.ExecContext(ctx,
			`UPDATE playlist_entries SET position = ? WHERE id = ?`,
			int64(i+1)*positionStep, entry); err != nil {
			return fmt.Errorf("move entry: %w", err)
		}
	}
	if err := touchPlaylist(ctx, tx, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("move entry: %w", err)
	}
	return nil
}

// PlaylistEntries lists what is in a playlist, in order, with each entry's own
// id so a client can remove or move exactly the row it is looking at.
//
// Entries whose item is in a library this handle cannot see are left out, and
// the total counts what came back rather than what is stored: a client paging
// a list it is only allowed part of should not be told to keep asking for
// rows it will never receive.
func (d *DB) PlaylistEntries(ctx context.Context, userID, id int64, page Page) ([]domain.PlaylistEntry, int, error) {
	if _, err := d.Playlist(ctx, userID, id); err != nil {
		return nil, 0, err
	}
	only, scope := d.only("i.section_id")

	var total int
	if err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM playlist_entries e JOIN items i ON i.id = e.item_id
		  WHERE e.playlist_id = ?`+only,
		append([]any{id}, scope...)...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count playlist: %w", err)
	}

	offset, limit := page.Window(500)
	args := append([]any{id}, scope...)
	args = append(args, limit, offset)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT e.id, `+prefixed("i", itemColumns)+`
		   FROM playlist_entries e JOIN items i ON i.id = e.item_id
		  WHERE e.playlist_id = ?`+only+`
		  ORDER BY e.position, e.id LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list playlist: %w", err)
	}
	defer rows.Close()

	var (
		entries []domain.PlaylistEntry
		items   []domain.Item
		ids     []int64
	)
	for rows.Next() {
		var entry domain.PlaylistEntry
		scanned, err := scanItemWithPrefix(rows, &entry.ID)
		if err != nil {
			return nil, 0, err
		}
		entry.Item = scanned
		entries = append(entries, entry)
		items = append(items, scanned)
		ids = append(ids, scanned.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	// The parts come in one follow-up query, the same way every other listing
	// does, so a playlist of sixty tracks is two statements rather than
	// sixty-one.
	if err := d.attachParts(ctx, items); err != nil {
		return nil, 0, err
	}
	for i := range entries {
		entries[i].Item = items[i]
	}
	return entries, total, nil
}

func touchPlaylist(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, id int64) error {
	// The kind is derived rather than declared: it is whatever went in, and a
	// list holding both is mixed. A client's Playlists tab filters on it, so
	// it has to be right after every change rather than at creation.
	if _, err := db.ExecContext(ctx,
		`UPDATE playlists SET updated_at = ?, kind = (
		   SELECT CASE
		     WHEN COUNT(*) = 0 THEN 'mixed'
		     WHEN SUM(CASE WHEN i.kind IN ('track','album','artist') THEN 1 ELSE 0 END) = COUNT(*) THEN 'audio'
		     WHEN SUM(CASE WHEN i.kind IN ('movie','episode') THEN 1 ELSE 0 END) = COUNT(*) THEN 'video'
		     ELSE 'mixed' END
		   FROM playlist_entries e JOIN items i ON i.id = e.item_id WHERE e.playlist_id = ?
		 ) WHERE id = ?`,
		millis(time.Now().UTC()), id, id); err != nil {
		return fmt.Errorf("update playlist: %w", err)
	}
	return nil
}

/* ---------- collections ---------- */

// CreateCollection groups items under a title.
func (d *DB) CreateCollection(ctx context.Context, title, summary string) (*domain.Collection, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, errors.New("a collection needs a name")
	}
	now := time.Now().UTC()
	res, err := d.sql.ExecContext(ctx,
		`INSERT INTO collections (title, sort_title, summary, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		title, SortTitle(title), strings.TrimSpace(summary), millis(now), millis(now))
	if err != nil {
		return nil, fmt.Errorf("create collection: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("create collection: %w", err)
	}
	return &domain.Collection{
		ID: id, Title: title, SortTitle: SortTitle(title), Summary: strings.TrimSpace(summary),
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

// Collections lists what a viewer should be offered: the groupings holding
// something this handle can actually see.
//
// An empty one is left out rather than shown empty, and that covers two
// different emptinesses with one rule. A group whose every item is in a
// hidden library would otherwise announce that something exists, which is
// the thing a hidden library is meant to stop; and a group whose library was
// deleted would linger as a shelf with nothing on it. Neither belongs in
// front of somebody trying to watch something.
//
// The administration interface wants the opposite and asks for it by name —
// see [DB.EveryCollection], which is what you need the moment after making
// one and before filling it.
func (d *DB) Collections(ctx context.Context) ([]domain.Collection, error) {
	return d.listCollections(ctx, true)
}

// EveryCollection lists them all, including the empty ones. For the interface
// that makes and fills them, never for one that browses them.
func (d *DB) EveryCollection(ctx context.Context) ([]domain.Collection, error) {
	return d.listCollections(ctx, false)
}

func (d *DB) listCollections(ctx context.Context, occupied bool) ([]domain.Collection, error) {
	rows, err := d.sql.QueryContext(ctx, collectionQuery(d, "", occupied), d.collectionArgs()...)
	if err != nil {
		return nil, fmt.Errorf("list collections: %w", err)
	}

	var found []domain.Collection
	for rows.Next() {
		c, err := scanCollection(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		found = append(found, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// A smart collection's count is its rules asked of the library, which is
	// a query of its own and cannot be folded into the listing above. One
	// extra statement per smart collection, and a household has a handful.
	var out []domain.Collection
	for _, c := range found {
		if c.Smart != nil {
			held, err := d.smartCount(ctx, *c.Smart)
			if err != nil {
				return nil, err
			}
			c.Held = held
		}
		if occupied && c.Held == 0 {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// Collection reads one a viewer may have, or ErrNotFound when this handle may
// see nothing in it — the same answer as a collection that does not exist,
// for the same reason.
func (d *DB) Collection(ctx context.Context, id int64) (*domain.Collection, error) {
	return d.readCollection(ctx, id, true)
}

// AnyCollection reads one whether or not it holds anything, for the interface
// that administers them.
func (d *DB) AnyCollection(ctx context.Context, id int64) (*domain.Collection, error) {
	return d.readCollection(ctx, id, false)
}

func (d *DB) readCollection(ctx context.Context, id int64, occupied bool) (*domain.Collection, error) {
	args := append([]any{id}, d.collectionArgs()...)
	row := d.sql.QueryRowContext(ctx, collectionQuery(d, "WHERE c.id = ?", occupied), args...)
	c, err := scanCollection(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if occupied && c.Unreadable {
		// Rules nobody can read hold nothing, and a collection holding
		// nothing is not offered to a viewer. The editor asks with
		// [DB.AnyCollection] and is told, which is the only place the fault
		// can be repaired.
		return nil, ErrNotFound
	}
	if c.Smart != nil {
		held, err := d.smartCount(ctx, *c.Smart)
		if err != nil {
			return nil, err
		}
		c.Held = held
		// The HAVING cannot do this one: a smart collection's count is not
		// known until its rules have been asked, which is after the row came
		// back. A viewer must not be shown an empty shelf, and a shelf that
		// is empty only because the libraries behind it are hidden must not
		// announce that it exists.
		if occupied && held == 0 {
			return nil, ErrNotFound
		}
	}
	return &c, nil
}

// smartCount is how many items a rule set currently matches, as this handle
// may see them and capped by the rule set's own limit.
func (d *DB) smartCount(ctx context.Context, s domain.Smart) (int, error) {
	rules := compileSmart(s, d.viewer)
	only, scope := d.only("i.section_id")

	args := append([]any{}, scope...)
	args = append(args, rules.args...)
	var total int
	if err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) `+smartBase+only+rules.where, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("count smart collection: %w", err)
	}
	if rules.limit > 0 && total > rules.limit {
		total = rules.limit
	}
	return total, nil
}

// smartItems is the page a smart collection shows: the rules asked of the
// library, in the order the rules name.
func (d *DB) smartItems(ctx context.Context, s domain.Smart, page Page) ([]domain.Item, int, error) {
	total, err := d.smartCount(ctx, s)
	if err != nil {
		return nil, 0, err
	}
	rules := compileSmart(s, d.viewer)

	offset, limit := page.Window(500)
	// The limit is part of the collection, not of the request: a collection
	// of the ten newest films has ten, and a client asking for five hundred
	// gets ten. Clamping here rather than in the SQL keeps one statement
	// doing one thing.
	if rules.limit > 0 {
		if offset >= rules.limit {
			return nil, total, nil
		}
		if offset+limit > rules.limit {
			limit = rules.limit - offset
		}
	}

	only, scope := d.only("i.section_id")
	args := append([]any{}, scope...)
	args = append(args, rules.args...)
	args = append(args, limit, offset)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+prefixed("i", itemColumns)+` `+smartBase+only+rules.where+`
		 ORDER BY `+rules.order+` LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list smart collection: %w", err)
	}
	defer rows.Close()

	items, err := d.collect(ctx, rows)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// SetCollectionRules turns a collection into a smart one, or back.
//
// A nil rule set makes it hand-picked again and leaves it empty, because the
// rules were its contents and there is nothing else to fall back to. Turning
// one smart clears whatever was picked by hand for the same reason in reverse:
// two sources of membership, one of which is ignored, is a collection that
// cannot be reasoned about.
func (d *DB) SetCollectionRules(ctx context.Context, id int64, s *domain.Smart) error {
	if s != nil {
		if err := ValidateSmart(*s); err != nil {
			return err
		}
	}
	stored, err := marshalSmart(s)
	if err != nil {
		return err
	}

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set rules: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE collections SET rules = ?, updated_at = ? WHERE id = ?`,
		stored, millis(time.Now().UTC()), id)
	if err != nil {
		return fmt.Errorf("set rules: %w", err)
	}
	if err := affected(res, "collection"); err != nil {
		return err
	}
	if stored != "" {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM collection_items WHERE collection_id = ?`, id); err != nil {
			return fmt.Errorf("set rules: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set rules: %w", err)
	}
	return nil
}

// collectionQuery builds the one statement every reader here uses.
//
// A LEFT JOIN so an empty collection has a row at all, with the library
// restriction in the ON clause rather than the WHERE — in a WHERE it would
// turn the outer join back into an inner one and lose the empty rows again,
// which is the mistake this comment exists to stop somebody repeating.
// HAVING then drops the empty ones when the caller asked for occupied.
func collectionQuery(d *DB, where string, occupied bool) string {
	only, _ := d.only("i.section_id")
	having := ""
	if occupied {
		// A smart collection has no rows in collection_items at all, so its
		// count here is always nought: it survives the HAVING on its rules
		// and is counted afterwards, by asking them. Dropping it here would
		// hide every smart collection from every viewer.
		having = " HAVING held > 0 OR c.rules <> ''"
	}
	return `SELECT c.id, c.title, c.sort_title, c.summary, c.poster_key, c.created_at, c.updated_at,
	               COUNT(i.id) AS held, c.rules
	          FROM collections c
	          LEFT JOIN collection_items ci ON ci.collection_id = c.id
	          LEFT JOIN items i ON i.id = ci.item_id` + only + `
	         ` + where + `
	         GROUP BY c.id` + having + `
	         ORDER BY c.sort_title, c.id`
}

func (d *DB) collectionArgs() []any {
	_, scope := d.only("i.section_id")
	return scope
}

func scanCollection(row interface{ Scan(...any) error }) (domain.Collection, error) {
	var (
		c                    domain.Collection
		createdAt, updatedAt int64
		rules                string
	)
	if err := row.Scan(&c.ID, &c.Title, &c.SortTitle, &c.Summary, &c.PosterKey,
		&createdAt, &updatedAt, &c.Held, &rules); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Collection{}, err
		}
		return domain.Collection{}, fmt.Errorf("scan collection: %w", err)
	}
	c.CreatedAt, c.UpdatedAt = fromMillis(createdAt), fromMillis(updatedAt)
	smart, readable := parseSmart(rules)
	c.Smart, c.Unreadable = smart, !readable
	return c, nil
}

// UpdateCollection changes the title and summary.
func (d *DB) UpdateCollection(ctx context.Context, id int64, title, summary string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return errors.New("a collection needs a name")
	}
	res, err := d.sql.ExecContext(ctx,
		`UPDATE collections SET title = ?, sort_title = ?, summary = ?, updated_at = ? WHERE id = ?`,
		title, SortTitle(title), strings.TrimSpace(summary), millis(time.Now().UTC()), id)
	if err != nil {
		return fmt.Errorf("update collection: %w", err)
	}
	return affected(res, "collection")
}

// DeleteCollection removes the grouping. The films stay where they were.
func (d *DB) DeleteCollection(ctx context.Context, id int64) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM collections WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete collection: %w", err)
	}
	return affected(res, "collection")
}

// SetCollectionItems replaces what is in a collection, in the order given.
//
// Replaces rather than appends because the administrator interface hands over
// the list it is showing, and an append would make saving twice a way to
// duplicate everything.
func (d *DB) SetCollectionItems(ctx context.Context, id int64, itemIDs []int64) error {
	smart, err := d.rulesOf(ctx, id)
	if err != nil {
		return err
	}
	if smart != nil {
		return ErrSmartCollection
	}

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set collection: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM collection_items WHERE collection_id = ?`, id); err != nil {
		return fmt.Errorf("clear collection: %w", err)
	}
	for position, itemID := range itemIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO collection_items (collection_id, item_id, position) VALUES (?, ?, ?)`,
			id, itemID, position); err != nil {
			return fmt.Errorf("add to collection: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE collections SET updated_at = ? WHERE id = ?`,
		millis(time.Now().UTC()), id); err != nil {
		return fmt.Errorf("set collection: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set collection: %w", err)
	}
	return nil
}

// CollectionItems lists what is in one: the order it was arranged in for a
// hand-picked collection, and what its rules currently match for a smart one.
//
// One function for both because every surface asks the same question of a
// collection — what is in it — and which kind it is is this package's business
// rather than three clients'.
func (d *DB) CollectionItems(ctx context.Context, id int64, page Page) ([]domain.Item, int, error) {
	smart, err := d.rulesOf(ctx, id)
	if err != nil {
		return nil, 0, err
	}
	if smart != nil {
		return d.smartItems(ctx, *smart, page)
	}

	only, scope := d.only("i.section_id")

	var total int
	if err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM collection_items ci JOIN items i ON i.id = ci.item_id
		  WHERE ci.collection_id = ?`+only,
		append([]any{id}, scope...)...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count collection: %w", err)
	}

	offset, limit := page.Window(500)
	args := append([]any{id}, scope...)
	args = append(args, limit, offset)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+prefixed("i", itemColumns)+`
		   FROM collection_items ci JOIN items i ON i.id = ci.item_id
		  WHERE ci.collection_id = ?`+only+`
		  ORDER BY ci.position, i.year, i.sort_title LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list collection: %w", err)
	}
	defer rows.Close()

	items, err := d.collect(ctx, rows)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// rulesOf reads one collection's rule set: nil for a hand-picked one, and nil
// for one whose rules cannot be read, which then holds nothing rather than
// everything. ErrNotFound when there is no such collection at all.
//
// Deliberately not the handle's restricted read: this answers "how is this
// collection filled", which is a property of the collection and the same for
// everybody. What a given viewer may see of it is decided by the query the
// answer leads to.
func (d *DB) rulesOf(ctx context.Context, id int64) (*domain.Smart, error) {
	var stored string
	err := d.sql.QueryRowContext(ctx,
		`SELECT rules FROM collections WHERE id = ?`, id).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read collection: %w", err)
	}
	smart, readable := parseSmart(stored)
	if !readable {
		return nil, nil
	}
	return smart, nil
}

// CollectionsOf reports which collections each of these items belongs to, so
// a film can say "part of this" without a query per row.
//
// Smart collections are asked separately and one at a time, because their
// membership is not stored anywhere to be joined against: each one's rules
// are asked of exactly these items. A household has a handful of them, which
// is a handful of statements for a page of sixty films.
func (d *DB) CollectionsOf(ctx context.Context, itemIDs []int64) (map[int64][]domain.Collection, error) {
	if len(itemIDs) == 0 {
		return nil, nil
	}
	placeholders, args := inClause(itemIDs)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT ci.item_id, c.id, c.title, c.sort_title, c.summary, c.poster_key,
		        c.created_at, c.updated_at, 0 AS held
		   FROM collection_items ci JOIN collections c ON c.id = ci.collection_id
		  WHERE ci.item_id IN (`+placeholders+`) ORDER BY c.sort_title`, args...)
	if err != nil {
		return nil, fmt.Errorf("read collections: %w", err)
	}
	defer rows.Close()

	out := make(map[int64][]domain.Collection, len(itemIDs))
	for rows.Next() {
		var (
			itemID               int64
			c                    domain.Collection
			createdAt, updatedAt int64
		)
		if err := rows.Scan(&itemID, &c.ID, &c.Title, &c.SortTitle, &c.Summary, &c.PosterKey,
			&createdAt, &updatedAt, &c.Held); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan collection: %w", err)
		}
		c.CreatedAt, c.UpdatedAt = fromMillis(createdAt), fromMillis(updatedAt)
		out[itemID] = append(out[itemID], c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if err := d.smartMembership(ctx, itemIDs, out); err != nil {
		return nil, err
	}
	return out, nil
}

// smartMembership adds the smart collections these items fall into.
func (d *DB) smartMembership(ctx context.Context, itemIDs []int64, out map[int64][]domain.Collection) error {
	smart, err := d.smartCollections(ctx)
	if err != nil {
		return err
	}
	placeholders, ids := inClause(itemIDs)
	for _, c := range smart {
		rules := compileSmart(*c.Smart, d.viewer)
		args := append([]any{}, ids...)
		args = append(args, rules.args...)
		rows, err := d.sql.QueryContext(ctx,
			`SELECT i.id `+smartBase+` AND i.id IN (`+placeholders+`)`+rules.where, args...)
		if err != nil {
			return fmt.Errorf("read smart collection: %w", err)
		}
		for rows.Next() {
			var itemID int64
			if err := rows.Scan(&itemID); err != nil {
				rows.Close()
				return fmt.Errorf("scan smart collection: %w", err)
			}
			out[itemID] = append(out[itemID], c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

/* ---------- shared ---------- */

// sees reports whether one item is within this handle's libraries.
func (d *DB) sees(ctx context.Context, itemID int64) (bool, error) {
	only, scope := d.only("section_id")
	var present int
	err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM items WHERE id = ?`+only,
		append([]any{itemID}, scope...)...).Scan(&present)
	if err != nil {
		return false, fmt.Errorf("read item: %w", err)
	}
	return present > 0, nil
}

// visibleOf keeps the ids this handle may see, in the order they were given.
func (d *DB) visibleOf(ctx context.Context, itemIDs []int64) ([]int64, error) {
	if len(itemIDs) == 0 {
		return nil, nil
	}
	only, scope := d.only("section_id")
	placeholders, args := inClause(itemIDs)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT id FROM items WHERE id IN (`+placeholders+`)`+only,
		append(args, scope...)...)
	if err != nil {
		return nil, fmt.Errorf("read items: %w", err)
	}
	defer rows.Close()

	allowed := make(map[int64]bool, len(itemIDs))
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan item: %w", err)
		}
		allowed[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The caller's order, which for a playlist is the order things go in.
	out := make([]int64, 0, len(itemIDs))
	for _, id := range itemIDs {
		if allowed[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

// scanItemWithPrefix reads a row that carries one extra leading column.
func scanItemWithPrefix(rows *sql.Rows, first *int64) (domain.Item, error) {
	item, err := scanItem(prepended{rows: rows, first: first})
	if err != nil {
		return domain.Item{}, fmt.Errorf("scan item: %w", err)
	}
	return item, nil
}

// prepended adapts a row whose first column is something other than an item
// into the shape scanItem reads.
//
// One scan function rather than two, because the second copy drifted the
// moment a column was added to itemColumns: the listings kept working and
// every playlist read failed with "expected 21 destination arguments, not
// 20", nowhere near the line that caused it.
type prepended struct {
	rows  interface{ Scan(...any) error }
	first any
}

func (p prepended) Scan(dest ...any) error {
	return p.rows.Scan(append([]any{p.first}, dest...)...)
}

// attachParts fills in the files for the playable items in a slice, in one
// query, the way collect does for the listings that go through it.
func (d *DB) attachParts(ctx context.Context, items []domain.Item) error {
	var playable []int64
	for _, item := range items {
		if item.Playable() {
			playable = append(playable, item.ID)
		}
	}
	if len(playable) == 0 {
		return nil
	}
	byItem, err := d.partsFor(ctx, playable)
	if err != nil {
		return err
	}
	for i := range items {
		if parts, ok := byItem[items[i].ID]; ok {
			items[i].Parts = parts
		}
	}
	return nil
}

// smartCollections reads the rule-driven collections and nothing else, with
// no count: the caller here is asking which of them an item falls into, and
// counting what else is in them would be a query per collection for an answer
// nobody asked for. Held is nought for the same reason it is in
// [DB.CollectionsOf], which is this function's only caller.
func (d *DB) smartCollections(ctx context.Context) ([]domain.Collection, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT c.id, c.title, c.sort_title, c.summary, c.poster_key,
		        c.created_at, c.updated_at, 0 AS held, c.rules
		   FROM collections c WHERE c.rules <> '' ORDER BY c.sort_title, c.id`)
	if err != nil {
		return nil, fmt.Errorf("list smart collections: %w", err)
	}
	defer rows.Close()

	var out []domain.Collection
	for rows.Next() {
		c, err := scanCollection(rows)
		if err != nil {
			return nil, err
		}
		if c.Smart == nil {
			continue
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
