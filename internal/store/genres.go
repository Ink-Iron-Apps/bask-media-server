package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// This file is everything about the words attached to a thing.
//
// A genre, a tag, a label, a mood and a style are the same shape: a word, a
// set of things carrying it, and a client that wants the list of words in
// use. They differ only in who writes them and what a client calls them —
// so there is one table with a kind on it, one batch read, one listing and
// one filter, rather than five of each.
//
// Four decisions worth stating, because the rest of the file follows.
//
// A word is a row, not a word in a column. The two questions asked are "what
// is this film" and "what does this library have", and the second one is a
// client's filter menu: it has to be answerable without reading every item
// in the library and splitting strings.
//
// Case is not part of identity. "Science Fiction" from a provider and
// "science fiction" from an ID3 tag are the same shelf. The column is
// COLLATE NOCASE, so the first spelling to arrive is the one shown and later
// ones land on it rather than beside it. The kind is part of identity,
// though: "Comedy" the genre and "Comedy" the label are two rows, because
// one came from a catalogue and the other from somebody here, and merging
// them would mean a correction silently editing a provider's answer.
//
// These belong to the thing that has them and are not inherited downwards.
// A provider gives a series its genres and says nothing about episode four;
// filing the episode under the series' genres would be inventing a fact.
//
// And every kind is written whole. A caller says what a thing's labels are,
// never which label to add, because the caller — a provider's answer, a
// file's tags, somebody's correction — knows the whole list and a word
// removed upstream should go here too.

// TagKind is which sort of word a row is.
//
// The names are the ones the clients use, because they are what arrives on
// the wire and what a filter menu is labelled with. Genre is the one a
// provider fills; the rest are a person's, except style and mood, which
// Plex's music agent writes and which a client shows beside an album.
type TagKind string

const (
	// TagGenre is what a thing is about: Comedy, Science Fiction, Shoegaze.
	// Written by a metadata provider and by a music file's own tags.
	TagGenre TagKind = "genre"

	// TagTag is a free word somebody attached. Plex's clients filter on
	// these and call them tags; nothing writes them but a person.
	TagTag TagKind = "tag"

	// TagLabel is Plex's own notion: a word used to organise rather than to
	// describe, and the thing its sharing rules are written against.
	TagLabel TagKind = "label"

	// TagMood and TagStyle are music's: how a record feels and what tradition
	// it sits in. Separate from genre because a music client shows them in
	// their own rows and because "melancholy" is not a genre.
	TagMood  TagKind = "mood"
	TagStyle TagKind = "style"
)

// TagKinds is every kind, in the order a page should offer them.
func TagKinds() []TagKind { return []TagKind{TagGenre, TagTag, TagLabel, TagMood, TagStyle} }

// Valid reports whether this is a kind of word BMS stores.
func (k TagKind) Valid() bool {
	for _, known := range TagKinds() {
		if k == known {
			return true
		}
	}
	return false
}

// Label is what a client's filter menu calls this kind.
func (k TagKind) Label() string {
	switch k {
	case TagTag:
		return "Tag"
	case TagLabel:
		return "Label"
	case TagMood:
		return "Mood"
	case TagStyle:
		return "Style"
	default:
		return "Genre"
	}
}

// maxGenresPerItem bounds what one item can carry of one kind.
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

// tagsFor reads one kind of word for a page of items in one query.
//
// A map rather than a slice, because the caller already has the items in the
// order it wants them. One query for the page is the point: a poster wall of
// forty films would otherwise be forty round trips to draw forty lines of
// chips.
func (d *DB) tagsFor(ctx context.Context, kind TagKind, itemIDs []int64) (map[int64][]string, error) {
	if len(itemIDs) == 0 {
		return nil, nil
	}
	query := `SELECT it.item_id, t.name FROM item_tags it
	            JOIN tags t ON t.id = it.tag_id
	           WHERE t.kind = ? AND it.item_id IN (` + questionMarks(len(itemIDs)) + `)
	           ORDER BY it.item_id, t.name`
	args := make([]any, 0, len(itemIDs)+1)
	args = append(args, string(kind))
	for _, id := range itemIDs {
		args = append(args, id)
	}
	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read %ss: %w", kind, err)
	}
	defer rows.Close()

	out := make(map[int64][]string, len(itemIDs))
	for rows.Next() {
		var (
			itemID int64
			name   string
		)
		if err := rows.Scan(&itemID, &name); err != nil {
			return nil, fmt.Errorf("scan %s: %w", kind, err)
		}
		out[itemID] = append(out[itemID], name)
	}
	return out, rows.Err()
}

// allTagsFor reads every kind for a page of items, in one query per kind.
//
// Five queries for a page rather than five per row, and only called where an
// item's whole detail is wanted: a listing reads genres alone, because that
// is the only kind a grid draws.
func (d *DB) allTagsFor(ctx context.Context, itemIDs []int64) (map[TagKind]map[int64][]string, error) {
	if len(itemIDs) == 0 {
		return nil, nil
	}
	out := make(map[TagKind]map[int64][]string, len(TagKinds()))
	for _, kind := range TagKinds() {
		byItem, err := d.tagsFor(ctx, kind, itemIDs)
		if err != nil {
			return nil, err
		}
		out[kind] = byItem
	}
	return out, nil
}

// genresFor is the one kind a listing reads, kept as its own name because it
// is read on every grid and the call sites say what they mean.
func (d *DB) genresFor(ctx context.Context, itemIDs []int64) (map[int64][]string, error) {
	return d.tagsFor(ctx, TagGenre, itemIDs)
}

// Tags reads one item's words of one kind.
func (d *DB) Tags(ctx context.Context, kind TagKind, itemID int64) ([]string, error) {
	byItem, err := d.tagsFor(ctx, kind, []int64{itemID})
	if err != nil {
		return nil, err
	}
	return byItem[itemID], nil
}

// Genres reads one item's genres.
func (d *DB) Genres(ctx context.Context, itemID int64) ([]string, error) {
	return d.Tags(ctx, TagGenre, itemID)
}

// SetTags makes an item's words of one kind exactly these.
//
// Exactly, rather than adding to what is there: the caller knows the whole
// answer — a provider's list, a file's tags, somebody's correction — and a
// word that was removed upstream should go here too. A caller with nothing
// to say must not call this; see the note where the enrichment pass writes
// genres.
func (d *DB) SetTags(ctx context.Context, kind TagKind, itemID int64, names []string) error {
	if !kind.Valid() {
		return fmt.Errorf("set tags: %q is not a kind of word this server keeps", kind)
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set %ss: %w", kind, err)
	}
	defer tx.Rollback()
	if err := replaceTagsTx(ctx, tx, kind, itemID, names); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set %ss: %w", kind, err)
	}
	return nil
}

// SetGenres makes an item's genres exactly these.
func (d *DB) SetGenres(ctx context.Context, itemID int64, names []string) error {
	return d.SetTags(ctx, TagGenre, itemID, names)
}

// execer is the part of a transaction these writes need, so the same code
// serves a scan's batch and a one-item correction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// replaceTagsTx points an item at exactly these words of one kind, inside a
// transaction somebody else owns.
//
// The names table is only ever added to. A word nothing carries any more is
// left in it: the rows are a handful of bytes each, and deleting one would
// mean counting its users on every write to find out whether it is the last.
// The listings read what is in use, so an orphan is invisible rather than
// wrong.
func replaceTagsTx(ctx context.Context, tx execer, kind TagKind, itemID int64, names []string) error {
	wanted := cleanGenres(names)

	ids := make([]int64, 0, len(wanted))
	for _, name := range wanted {
		id, err := tagID(ctx, tx, kind, name)
		if err != nil {
			return err
		}
		ids = append(ids, id)
	}

	// Only this kind's rows are touched. Setting an item's labels must not
	// clear the genres a provider gave it.
	mine := `DELETE FROM item_tags WHERE item_id = ?
	           AND tag_id IN (SELECT id FROM tags WHERE kind = ?)`
	if len(ids) == 0 {
		if _, err := tx.ExecContext(ctx, mine, itemID, string(kind)); err != nil {
			return fmt.Errorf("clear %ss: %w", kind, err)
		}
		return nil
	}

	args := append(make([]any, 0, len(ids)+2), itemID, string(kind))
	for _, id := range ids {
		args = append(args, id)
	}
	if _, err := tx.ExecContext(ctx,
		mine+` AND tag_id NOT IN (`+questionMarks(len(ids))+`)`, args...); err != nil {
		return fmt.Errorf("drop %ss: %w", kind, err)
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO item_tags (item_id, tag_id) VALUES (?, ?)
			 ON CONFLICT(item_id, tag_id) DO NOTHING`, itemID, id); err != nil {
			return fmt.Errorf("add %s: %w", kind, err)
		}
	}
	return nil
}

// replaceGenresTx is the genre case, which the scan and the item editor use.
func replaceGenresTx(ctx context.Context, tx execer, itemID int64, names []string) error {
	return replaceTagsTx(ctx, tx, TagGenre, itemID, names)
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
		`SELECT COUNT(*) FROM item_tags it JOIN tags t ON t.id = it.tag_id
		  WHERE it.item_id = ? AND t.kind = ?`, itemID, string(TagGenre)).Scan(&held); err != nil {
		return fmt.Errorf("count genres: %w", err)
	}
	if held > 0 {
		return nil
	}
	return replaceGenresTx(ctx, tx, itemID, names)
}

// tagID is the row for a kind and a name, created if this is the first thing
// to carry it.
//
// The insert comes first and the read second, because the name is unique
// within its kind without regard to case: an insert that conflicts tells us
// the word is already there under somebody else's spelling, and the select
// then finds it whichever way it was spelled.
func tagID(ctx context.Context, tx execer, kind TagKind, name string) (int64, error) {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tags (kind, name) VALUES (?, ?) ON CONFLICT(kind, name) DO NOTHING`,
		string(kind), name); err != nil {
		return 0, fmt.Errorf("record %s: %w", kind, err)
	}
	var id int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM tags WHERE kind = ? AND name = ?`, string(kind), name).Scan(&id); err != nil {
		return 0, fmt.Errorf("read %s: %w", kind, err)
	}
	return id, nil
}

// GenreCount is one word and how many things in a library carry it.
type GenreCount struct {
	// ID is the row, which is what a Plex client is handed as a filter's key
	// and hands back on the next request.
	ID int64

	Name string

	// Held is how many items of the library's own top kind carry it, so a
	// filter menu can say "Comedy (14)" and leave out a word that nothing
	// visible to this viewer has.
	Held int
}

// SectionTags lists one kind of word in use in one library, with counts.
//
// Only the library's top kind — its films, its series, its artists —
// because that is the list a client filters. Counted through the same
// visibility and rating ceiling as every other read, so a word that only a
// hidden film carries does not appear in the menu: an empty shelf is worse
// than a missing one.
func (d *DB) SectionTags(ctx context.Context, sectionID int64, kind string, tag TagKind) ([]GenreCount, error) {
	if !d.canSee(sectionID) {
		return nil, nil
	}
	rated, ceiling := d.onlyRated("i.section_id")
	args := append([]any{string(tag), sectionID, kind}, ceiling...)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT t.id, t.name, COUNT(*) FROM tags t
		   JOIN item_tags it ON it.tag_id = t.id
		   JOIN items i ON i.id = it.item_id
		  WHERE t.kind = ? AND i.section_id = ? AND i.kind = ? AND i.extra = ''`+rated+`
		  GROUP BY t.id, t.name
		  ORDER BY t.name`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("list section %ss: %w", tag, err)
	}
	defer rows.Close()

	var out []GenreCount
	for rows.Next() {
		var row GenreCount
		if err := rows.Scan(&row.ID, &row.Name, &row.Held); err != nil {
			return nil, fmt.Errorf("scan section %s: %w", tag, err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// SectionGenres lists the genres in use in one library, with counts.
func (d *DB) SectionGenres(ctx context.Context, sectionID int64, kind string) ([]GenreCount, error) {
	return d.SectionTags(ctx, sectionID, kind, TagGenre)
}

// TagNames lists every word of one kind that anything visible carries,
// across every library.
//
// What the smart-collection editor offers: a rule about a genre is only
// writable if the words in use can be shown, and nobody should have to spell
// "Science Fiction & Fantasy" from memory to narrow a shelf.
func (d *DB) TagNames(ctx context.Context, tag TagKind) ([]string, error) {
	rated, ceiling := d.onlyRated("i.section_id")
	only, scope := d.only("i.section_id")
	args := append([]any{string(tag)}, ceiling...)
	args = append(args, scope...)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT DISTINCT t.name FROM tags t
		   JOIN item_tags it ON it.tag_id = t.id
		   JOIN items i ON i.id = it.item_id
		  WHERE t.kind = ? AND i.extra = ''`+rated+only+`
		  ORDER BY t.name`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("list %ss: %w", tag, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan %s name: %w", tag, err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// GenreNames lists every genre anything visible carries.
func (d *DB) GenreNames(ctx context.Context) ([]string, error) {
	return d.TagNames(ctx, TagGenre)
}

// tagNamesFor resolves what a client sent into names of one kind.
//
// A client hands back whatever the filter menu gave it as a key, and Plex's
// own menus give numbers while Jellyfin's give words. Both are accepted:
// anything that reads as the id of a known row becomes that row's name, and
// everything else is taken as a name already. A value that matches nothing
// is left as it is, so the listing comes back empty rather than unfiltered —
// a filter nobody can satisfy must not quietly show everything.
func (d *DB) tagNamesFor(ctx context.Context, tag TagKind, values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if id, err := strconv.ParseInt(value, 10, 64); err == nil {
			var name string
			if err := d.sql.QueryRowContext(ctx,
				`SELECT name FROM tags WHERE id = ? AND kind = ?`, id, string(tag)).Scan(&name); err == nil {
				out = append(out, name)
				continue
			}
		}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func (d *DB) genreNamesFor(ctx context.Context, values []string) []string {
	return d.tagNamesFor(ctx, TagGenre, values)
}

// questionMarks is a bind list of the given length, for the IN clauses that
// read a page of items at once.
func questionMarks(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// genreClause is the genre case of [DB.tagClause], which is what a library
// listing filters by.
func (d *DB) genreClause(ctx context.Context, values []string) (string, []any) {
	return d.tagClause(ctx, TagGenre, values)
}

// WordFilter is one menu somebody narrowed a library with.
//
// Kinds is usually one. It is several where the wire has one field for what
// BMS keeps as two kinds — Jellyfin's Tags covers both a label and a tag —
// and a thing then matches if it carries the word as any of them. Expressing
// that as two filters would instead require it to carry the word twice.
type WordFilter struct {
	Kinds  []TagKind
	Values []string
}

// wordClause is the SQL and the values for one menu.
func (d *DB) wordClause(ctx context.Context, filter WordFilter) (string, []any) {
	if len(filter.Kinds) == 0 || len(filter.Values) == 0 {
		return "", nil
	}
	// Names are resolved against the first kind, because an id only ever
	// came from one menu and a name is a name whichever kind holds it.
	names := d.tagNamesFor(ctx, filter.Kinds[0], filter.Values)
	if len(names) == 0 {
		return "", nil
	}

	args := make([]any, 0, len(filter.Kinds)+len(names))
	for _, kind := range filter.Kinds {
		args = append(args, string(kind))
	}
	for _, name := range names {
		args = append(args, name)
	}
	return ` AND EXISTS (SELECT 1 FROM item_tags it JOIN tags t ON t.id = it.tag_id
	            WHERE it.item_id = items.id
	              AND t.kind IN (` + questionMarks(len(filter.Kinds)) + `)
	              AND t.name IN (` + questionMarks(len(names)) + `))`, args
}

// tagClause is the SQL and the values for "carrying any one of these words
// of this kind".
//
// Returns an empty fragment when nothing was asked for, so a listing with no
// filter is the query it always was rather than one with a predicate that is
// always true.
//
// The subquery is correlated on items.id rather than joined, because a join
// would return one row per matching word and a film in both Comedy and Drama
// would appear twice in a list filtered by both.
func (d *DB) tagClause(ctx context.Context, kind TagKind, values []string) (string, []any) {
	return d.wordClause(ctx, WordFilter{Kinds: []TagKind{kind}, Values: values})
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
