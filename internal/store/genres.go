package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// This file is everything about what a thing is about.
//
// Three decisions worth stating, because the rest of the file follows from
// them.
//
// A genre is a row, not a word in a column. The two questions asked of
// genres are "what is this film" and "what does this library have", and the
// second one is a client's filter menu: it has to be answerable without
// reading every item in the library and splitting strings.
//
// Case is not part of a genre's identity. "Science Fiction" from a provider
// and "science fiction" from an ID3 tag are the same shelf. The column is
// COLLATE NOCASE, so the first spelling to arrive is the one shown and later
// ones land on it rather than beside it.
//
// Genres belong to the thing that has them and are not inherited downwards.
// A provider gives a series its genres and says nothing about episode four;
// filing the episode under the series' genres would be inventing a fact, and
// nothing BMS does with genres asks about an episode.

// maxGenresPerItem bounds what one item can carry.
//
// A film has three or four genres. A file whose tags carry two hundred is a
// mistake somewhere upstream, and accepting it would put two hundred rows in
// the joining table and two hundred chips on a page.
const maxGenresPerItem = 24

// maxGenreLength bounds one name. Long enough for "Science Fiction & Fantasy"
// and short enough that a tag holding a whole sentence is refused.
const maxGenreLength = 60

// cleanGenres is what a list of names becomes before it is stored: trimmed,
// emptied of blanks, deduplicated without regard to case, and capped.
//
// Order is kept as given rather than sorted, because the order a provider
// lists genres in is the order of how much it thinks each one applies —
// though nothing downstream relies on it, since reads come back sorted.
func cleanGenres(names []string) []string {
	out := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.Join(strings.Fields(name), " ")
		if name == "" {
			continue
		}
		if len(name) > maxGenreLength {
			continue
		}
		key := strings.ToLower(name)
		if _, found := seen[key]; found {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, name)
		if len(out) == maxGenresPerItem {
			break
		}
	}
	return out
}

// genresFor reads the genres of a page of items in one query.
//
// A map rather than a slice, because the caller already has the items in the
// order it wants them and needs to attach genres to the rows it has. One
// query for the page is the point: a poster wall of forty films would
// otherwise be forty round trips to draw forty lines of chips.
func (d *DB) genresFor(ctx context.Context, itemIDs []int64) (map[int64][]string, error) {
	if len(itemIDs) == 0 {
		return nil, nil
	}
	query := `SELECT ig.item_id, g.name FROM item_genres ig
	            JOIN genres g ON g.id = ig.genre_id
	           WHERE ig.item_id IN (` + questionMarks(len(itemIDs)) + `)
	           ORDER BY ig.item_id, g.name`
	args := make([]any, 0, len(itemIDs))
	for _, id := range itemIDs {
		args = append(args, id)
	}
	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read genres: %w", err)
	}
	defer rows.Close()

	out := make(map[int64][]string, len(itemIDs))
	for rows.Next() {
		var (
			itemID int64
			name   string
		)
		if err := rows.Scan(&itemID, &name); err != nil {
			return nil, fmt.Errorf("scan genre: %w", err)
		}
		out[itemID] = append(out[itemID], name)
	}
	return out, rows.Err()
}

// Genres reads one item's genres.
func (d *DB) Genres(ctx context.Context, itemID int64) ([]string, error) {
	byItem, err := d.genresFor(ctx, []int64{itemID})
	if err != nil {
		return nil, err
	}
	return byItem[itemID], nil
}

// SetGenres makes an item's genres exactly these.
//
// Exactly, rather than adding to what is there: the caller knows the whole
// answer — a provider's list, a file's tags, somebody's correction — and a
// genre that was removed upstream should go here too. A caller with nothing
// to say must not call this; see the note on the empty case in the
// enrichment pass.
func (d *DB) SetGenres(ctx context.Context, itemID int64, names []string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set genres: %w", err)
	}
	defer tx.Rollback()
	if err := replaceGenresTx(ctx, tx, itemID, names); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set genres: %w", err)
	}
	return nil
}

// execer is the part of a transaction the genre writes need, so the same code
// serves a scan's batch and a one-item correction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// replaceGenresTx points an item at exactly these genres, inside a
// transaction somebody else owns.
//
// The names table is only ever added to. A genre nothing carries any more is
// left in it: the rows are a handful of bytes each, and deleting one would
// mean counting its users on every write to find out whether it is the last.
// The listing reads what is in use, so an orphan is invisible rather than
// wrong.
func replaceGenresTx(ctx context.Context, tx execer, itemID int64, names []string) error {
	wanted := cleanGenres(names)

	ids := make([]int64, 0, len(wanted))
	for _, name := range wanted {
		id, err := genreID(ctx, tx, name)
		if err != nil {
			return err
		}
		ids = append(ids, id)
	}

	if len(ids) == 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM item_genres WHERE item_id = ?`, itemID); err != nil {
			return fmt.Errorf("clear genres: %w", err)
		}
		return nil
	}

	args := append(make([]any, 0, len(ids)+1), itemID)
	for _, id := range ids {
		args = append(args, id)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM item_genres WHERE item_id = ? AND genre_id NOT IN (`+questionMarks(len(ids))+`)`,
		args...); err != nil {
		return fmt.Errorf("drop genres: %w", err)
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO item_genres (item_id, genre_id) VALUES (?, ?)
			 ON CONFLICT(item_id, genre_id) DO NOTHING`, itemID, id); err != nil {
			return fmt.Errorf("add genre: %w", err)
		}
	}
	return nil
}

// addGenresTx gives an item genres only if it has none.
//
// This is how a container above a file gets them: an album takes the genre
// off the first track that declares one, and the sixty tracks after it cost
// one query each and change nothing. Doing it the other way — every track
// replacing the album's genres — would leave the album filed under whichever
// track the walk happened to reach last.
func addGenresTx(ctx context.Context, tx execer, itemID int64, names []string) error {
	if len(cleanGenres(names)) == 0 {
		return nil
	}
	var held int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM item_genres WHERE item_id = ?`, itemID).Scan(&held); err != nil {
		return fmt.Errorf("count genres: %w", err)
	}
	if held > 0 {
		return nil
	}
	return replaceGenresTx(ctx, tx, itemID, names)
}

// genreID is the row for a name, created if this is the first thing to carry
// it.
//
// The insert comes first and the read second, because the name is unique
// without regard to case: an insert that conflicts tells us the genre is
// already there under somebody else's spelling, and the select then finds it
// whichever way it was spelled.
func genreID(ctx context.Context, tx execer, name string) (int64, error) {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO genres (name) VALUES (?) ON CONFLICT(name) DO NOTHING`, name); err != nil {
		return 0, fmt.Errorf("record genre: %w", err)
	}
	var id int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM genres WHERE name = ?`, name).Scan(&id); err != nil {
		return 0, fmt.Errorf("read genre: %w", err)
	}
	return id, nil
}

// GenreCount is one genre and how many things in a library carry it.
type GenreCount struct {
	// ID is the row, which is what a Plex client is handed as a filter's key
	// and hands back on the next request.
	ID int64

	Name string

	// Held is how many items of the library's own top kind carry it, so a
	// filter menu can say "Comedy (14)" and leave out a genre that nothing
	// visible to this viewer has.
	Held int
}

// SectionGenres lists the genres in use in one library, with counts.
//
// Only the library's top kind — its films, its series, its artists — because
// that is the list a client filters. Counted through the same visibility and
// rating ceiling as every other read, so a genre that only a hidden film
// carries does not appear in the menu: an empty shelf is worse than a
// missing one.
func (d *DB) SectionGenres(ctx context.Context, sectionID int64, kind string) ([]GenreCount, error) {
	if !d.canSee(sectionID) {
		return nil, nil
	}
	rated, ceiling := d.onlyRated("i.section_id")
	args := append([]any{sectionID, kind}, ceiling...)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT g.id, g.name, COUNT(*) FROM genres g
		   JOIN item_genres ig ON ig.genre_id = g.id
		   JOIN items i ON i.id = ig.item_id
		  WHERE i.section_id = ? AND i.kind = ? AND i.extra = ''`+rated+`
		  GROUP BY g.id, g.name
		  ORDER BY g.name`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("list section genres: %w", err)
	}
	defer rows.Close()

	var out []GenreCount
	for rows.Next() {
		var row GenreCount
		if err := rows.Scan(&row.ID, &row.Name, &row.Held); err != nil {
			return nil, fmt.Errorf("scan section genre: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// GenreNames lists every genre anything visible carries, across every
// library.
//
// What the smart-collection editor offers: a rule about genre is only
// writable if the words in use can be shown, and nobody should have to spell
// "Science Fiction & Fantasy" from memory to narrow a shelf.
func (d *DB) GenreNames(ctx context.Context) ([]string, error) {
	rated, ceiling := d.onlyRated("i.section_id")
	only, scope := d.only("i.section_id")
	args := append([]any{}, ceiling...)
	args = append(args, scope...)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT DISTINCT g.name FROM genres g
		   JOIN item_genres ig ON ig.genre_id = g.id
		   JOIN items i ON i.id = ig.item_id
		  WHERE i.extra = ''`+rated+only+`
		  ORDER BY g.name`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("list genres: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan genre name: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// genreNamesFor resolves what a client sent into genre names.
//
// A client hands back whatever the filter menu gave it as a key, and Plex's
// own menus give numbers while Jellyfin's give words. Both are accepted:
// anything that reads as the id of a known genre becomes that genre's name,
// and everything else is taken as a name already. A value that matches
// nothing is left as it is, so the listing comes back empty rather than
// unfiltered — a filter nobody can satisfy must not quietly show everything.
func (d *DB) genreNamesFor(ctx context.Context, values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if id, err := strconv.ParseInt(value, 10, 64); err == nil {
			var name string
			if err := d.sql.QueryRowContext(ctx, `SELECT name FROM genres WHERE id = ?`, id).Scan(&name); err == nil {
				out = append(out, name)
				continue
			}
		}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// questionMarks is a bind list of the given length, for the IN clauses that
// read a page of items at once.
func questionMarks(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// genreClause is the SQL and the values for "carrying any one of these".
//
// Returns an empty fragment when nothing was asked for, so a listing with no
// filter is the query it always was rather than one with a predicate that is
// always true.
//
// The subquery is correlated on items.id rather than joined, because a join
// would return one row per matching genre and a film in both Comedy and
// Drama would appear twice in a list filtered by both.
func (d *DB) genreClause(ctx context.Context, values []string) (string, []any) {
	names := d.genreNamesFor(ctx, values)
	if len(names) == 0 {
		return "", nil
	}
	args := make([]any, 0, len(names))
	for _, name := range names {
		args = append(args, name)
	}
	return ` AND EXISTS (SELECT 1 FROM item_genres ig JOIN genres g ON g.id = ig.genre_id
	            WHERE ig.item_id = items.id AND g.name IN (` + questionMarks(len(names)) + `))`, args
}

// Facets are the other values a client's filter sheet offers beside genre:
// the content ratings in use and the years things came out.
//
// Here rather than in their own file because they are the same question
// asked of two more columns, they are only ever asked together with the
// genres, and a client that cannot fill its filter sheet in one request asks
// three times.
type Facets struct {
	Ratings []string
	Years   []int
}

// SectionFacets lists the ratings and years in use in one library.
//
// Only the library's top kind, and only what this viewer can see, for the
// same reason [DB.SectionGenres] is: a filter that leads to an empty shelf
// is worse than one that was never offered.
func (d *DB) SectionFacets(ctx context.Context, sectionID int64, kind string) (Facets, error) {
	if !d.canSee(sectionID) {
		return Facets{}, nil
	}
	rated, ceiling := d.onlyRated("section_id")
	args := append([]any{sectionID, kind}, ceiling...)

	var out Facets
	ratings, err := d.sql.QueryContext(ctx,
		`SELECT DISTINCT rating FROM items
		  WHERE section_id = ? AND kind = ? AND extra = '' AND rating <> ''`+rated+`
		  ORDER BY rating`, args...)
	if err != nil {
		return Facets{}, fmt.Errorf("list ratings: %w", err)
	}
	defer ratings.Close()
	for ratings.Next() {
		var rating string
		if err := ratings.Scan(&rating); err != nil {
			return Facets{}, fmt.Errorf("scan rating: %w", err)
		}
		out.Ratings = append(out.Ratings, rating)
	}
	if err := ratings.Err(); err != nil {
		return Facets{}, err
	}

	// Newest first, because that is the order a year filter is read in.
	years, err := d.sql.QueryContext(ctx,
		`SELECT DISTINCT year FROM items
		  WHERE section_id = ? AND kind = ? AND extra = '' AND year > 0`+rated+`
		  ORDER BY year DESC`, args...)
	if err != nil {
		return Facets{}, fmt.Errorf("list years: %w", err)
	}
	defer years.Close()
	for years.Next() {
		var year int
		if err := years.Scan(&year); err != nil {
			return Facets{}, fmt.Errorf("scan year: %w", err)
		}
		out.Years = append(out.Years, year)
	}
	return out, years.Err()
}
