package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// Per-library settings, and the question the scheduler asks of them.
//
// The settings live in one JSON column for the same reasons a smart
// collection's rules do: they are only ever read and written whole, by the
// page that writes them, and a column per setting would be a migration for
// every new one. Unreadable JSON reads as the defaults rather than failing —
// a library whose settings were corrupted should still be browsable, and the
// page that rewrites them is where that gets noticed.

// maxSettingsBytes bounds what will be stored in the column. The settings are
// four fields; anything near this is not settings.
const maxSettingsBytes = 4 << 10

// scanFloor is the shortest interval a library may scan itself at.
//
// A scan of an unchanged library costs a stat per file, which is cheap but
// not free, and a library set to one minute would spend more time walking
// than waiting. Fifteen minutes is frequent enough that a film added before
// dinner is there after it.
const scanFloor = 15 * time.Minute

// parseSectionSettings reads the column, defaulting on anything it cannot
// make sense of.
func parseSectionSettings(stored string) domain.SectionSettings {
	if stored == "" {
		return domain.SectionSettings{}
	}
	var out domain.SectionSettings
	if err := json.Unmarshal([]byte(stored), &out); err != nil {
		return domain.SectionSettings{}
	}
	return out
}

func marshalSectionSettings(s domain.SectionSettings) (string, error) {
	// The zero value is the empty string, so a library set back to the
	// defaults is indistinguishable from one that never had settings.
	if s == (domain.SectionSettings{}) {
		return "", nil
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("store settings: %w", err)
	}
	if len(encoded) > maxSettingsBytes {
		return "", fmt.Errorf("store settings: too large")
	}
	return string(encoded), nil
}

// ValidateSectionSettings refuses a setting that cannot mean what it says.
//
// Refused at save time rather than clamped silently, because a scan interval
// of one minute is somebody misunderstanding the field and a message is more
// use to them than a number quietly becoming fifteen.
func ValidateSectionSettings(s domain.SectionSettings) error {
	if s.ScanEvery < 0 {
		return fmt.Errorf("a scan interval cannot be negative")
	}
	if s.ScanEvery > 0 && time.Duration(s.ScanEvery)*time.Minute < scanFloor {
		return fmt.Errorf("a library cannot scan itself more often than every %d minutes",
			int(scanFloor.Minutes()))
	}
	// A week. Past this somebody means "never", and the field for that is
	// zero, which is also what the page offers.
	if s.ScanEvery > 7*24*60 {
		return fmt.Errorf("a scan interval longer than a week means never — leave it empty for that")
	}
	return nil
}

// SetSectionSettings stores what one library wants done to it.
func (d *DB) SetSectionSettings(ctx context.Context, sectionID int64, s domain.SectionSettings) error {
	if err := ValidateSectionSettings(s); err != nil {
		return err
	}
	stored, err := marshalSectionSettings(s)
	if err != nil {
		return err
	}
	res, err := d.sql.ExecContext(ctx,
		`UPDATE sections SET settings = ?, updated_at = ? WHERE id = ?`,
		stored, millis(time.Now().UTC()), sectionID)
	if err != nil {
		return fmt.Errorf("set settings: %w", err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return ErrNotFound
	}
	return nil
}

// DueForScan is the libraries whose own interval says it is time, oldest
// visit first.
//
// Measured from when the scheduler last looked rather than from when a scan
// last finished, which is what keeps a library whose scan fails from being
// retried in a tight loop: the visit is recorded either way.
//
// Unrestricted on purpose. This is the scheduler asking, not a viewer: a
// library nobody can see still has to be scanned, or it would quietly stop
// updating for the one account that can.
func (d *DB) DueForScan(ctx context.Context, now time.Time) ([]domain.Section, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT id, title, kind, roots, scanned_at, created_at, updated_at, settings, swept_at
		   FROM sections WHERE settings <> '' ORDER BY swept_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list libraries to scan: %w", err)
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
			return nil, fmt.Errorf("scan library row: %w", err)
		}
		s.Kind = domain.SectionKind(kind)
		if roots != "" {
			s.Roots = strings.Split(roots, "\n")
		}
		s.Settings = parseSectionSettings(settings)
		s.ScannedAt, s.CreatedAt, s.UpdatedAt = fromMillis(scannedAt), fromMillis(createdAt), fromMillis(updatedAt)
		s.SweptAt = fromMillis(sweptAt)

		every := s.Settings.ScanInterval()
		if every <= 0 {
			continue
		}
		// A library that has never been swept is due immediately, which is
		// what somebody who just turned this on expects to see.
		if s.SweptAt.IsZero() || !now.Before(s.SweptAt.Add(every)) {
			out = append(out, s)
		}
	}
	return out, rows.Err()
}

// MarkSwept records that the scheduler looked at this library, whatever came
// of it.
func (d *DB) MarkSwept(ctx context.Context, sectionID int64, when time.Time) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE sections SET swept_at = ? WHERE id = ?`, millis(when), sectionID)
	if err != nil {
		return fmt.Errorf("mark swept: %w", err)
	}
	return nil
}

// allowedSections is the ids of the libraries whose settings permit a pass.
//
// Read in Go rather than decided in SQL, because the settings are a JSON
// column and a query that picked them apart would be a second parser to keep
// agreeing with the first. The table has a handful of rows, so reading all of
// them to filter a batch query is not a cost worth optimising.
//
// Returns nil when every library is allowed, which lets the caller skip the
// clause entirely rather than build an IN list of everything.
func (d *DB) allowedSections(ctx context.Context, allow func(domain.Section) bool) (ids []int64, all bool, err error) {
	sections, err := d.Sections(ctx)
	if err != nil {
		return nil, false, err
	}
	allowed := make([]int64, 0, len(sections))
	for _, section := range sections {
		if allow(section) {
			allowed = append(allowed, section.ID)
		}
	}
	return allowed, len(allowed) == len(sections), nil
}

// sectionFilter is the SQL and arguments for "in one of these libraries".
//
// An empty list is not no filter: it is a filter nothing satisfies, which is
// the honest reading of "every library has this turned off". Returning no
// clause there would run the pass over the whole library instead, which is
// the opposite of what was asked.
func sectionFilter(column string, ids []int64, all bool) (string, []any) {
	if all {
		return "", nil
	}
	if len(ids) == 0 {
		return " AND 0 = 1", nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	return " AND " + column + " IN (" + questionMarks(len(ids)) + ")", args
}

// Sorting a library, and the letters its titles start with.
//
// Both exist because a client asks for them: Plex's clients read
// /library/sections/{id}/sorts to build a sort menu and /firstCharacter to
// draw the alphabet strip beside a long grid. BMS answered neither, so the
// menu offered the client's own guesses — sorts this server then ignored —
// and the strip had nothing to jump to.

// SortKey is how a listing may be ordered.
//
// Short, and every one of them backed by a column that is already there: a
// sort menu that offers an order the server then ignores is worse than a
// shorter menu, which is exactly the state this replaces.
type SortKey string

const (
	// SortTitleKey is the default, and the order every listing had before
	// sorting existed.
	SortTitleKey SortKey = "titleSort"
	SortAdded    SortKey = "addedAt"
	SortYear     SortKey = "year"
	SortDuration SortKey = "duration"
	SortRating   SortKey = "contentRating"
)

// sortColumns is the ORDER BY for each key.
//
// Every one ends in sort_title and then id. The title is the tie-break
// somebody expects — twenty films from 1999 in alphabetical order rather
// than in insertion order — and the id after it means a page boundary never
// falls inside a tie, which is the same rule the smart collections follow.
var sortColumns = map[SortKey]string{
	SortTitleKey: "sort_title, year",
	SortAdded:    "added_at",
	SortYear:     "year",
	SortDuration: "duration_ms",
	SortRating:   "rating_age",
}

// ParseSort reads a client's sort parameter: a key, optionally with ":desc".
//
// An unknown key is the default rather than an error. A client asking for an
// order this server does not have still wants its list, and the alternative
// is a grid that fails to load because a sort menu disagreed.
func ParseSort(raw string) (key SortKey, desc bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return SortTitleKey, false
	}
	if cut := strings.IndexByte(raw, ':'); cut >= 0 {
		desc = strings.EqualFold(strings.TrimSpace(raw[cut+1:]), "desc")
		raw = raw[:cut]
	}
	candidate := SortKey(strings.TrimSpace(raw))
	if _, known := sortColumns[candidate]; !known {
		return SortTitleKey, desc
	}
	return candidate, desc
}

// orderBy is the clause for a sort, with the tie-breaks.
func orderBy(key SortKey, desc bool) string {
	columns, known := sortColumns[key]
	if !known {
		columns = sortColumns[SortTitleKey]
	}
	direction := ""
	if desc {
		direction = " DESC"
	}
	clause := ""
	for _, column := range strings.Split(columns, ", ") {
		if clause != "" {
			clause += ", "
		}
		clause += column + direction
	}
	// The title, then the id: a tie-break somebody expects, and then one
	// that guarantees a page boundary never falls inside a tie.
	if key != SortTitleKey {
		clause += ", sort_title"
	}
	return clause + ", id"
}

// SortOption is one row of a client's sort menu.
type SortOption struct {
	Key     SortKey
	Title   string
	Descend bool // whether descending is the sensible default for it
}

// Sorts lists the orders a library can be listed in.
//
// Per kind, because the sensible menu differs: a photo album has no
// certificate and a music library's runtime is a track's, which nobody sorts
// an artist by.
func Sorts(kind domain.SectionKind) []SortOption {
	title := SortOption{Key: SortTitleKey, Title: "Title"}
	added := SortOption{Key: SortAdded, Title: "Date added", Descend: true}
	switch kind {
	case domain.SectionPhotos:
		// A photo library is a camera roll: newest first is what anybody
		// opening it wants, and there is nothing else worth offering.
		return []SortOption{added, title}
	case domain.SectionMusic:
		return []SortOption{title, added, {Key: SortYear, Title: "Year", Descend: true}}
	default:
		return []SortOption{
			title, added,
			{Key: SortYear, Title: "Year", Descend: true},
			{Key: SortDuration, Title: "Duration", Descend: true},
			{Key: SortRating, Title: "Rating", Descend: false},
		}
	}
}

// FirstLetter is one row of the alphabet strip: a letter and how many
// things in the library start with it.
type FirstLetter struct {
	Key   string
	Title string
	Held  int
}

// FirstLetters is the letters a library's titles start with, with counts.
//
// Read from sort_title rather than title, so "The Matrix" is filed under M
// exactly where the listing puts it — a strip that disagreed with the grid
// beside it would jump to the wrong place.
//
// Everything that does not start with a letter is grouped under "#", which
// is what Plex's own strip does and what a client's fallback expects.
func (d *DB) FirstLetters(ctx context.Context, sectionID int64, kind domain.Kind) ([]FirstLetter, error) {
	if !d.canSee(sectionID) {
		return nil, nil
	}
	rated, ceiling := d.onlyRated("section_id")
	args := append([]any{sectionID, string(kind)}, ceiling...)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT UPPER(SUBSTR(sort_title, 1, 1)) AS letter, COUNT(*) FROM items
		  WHERE section_id = ? AND kind = ? AND parent_id IS NULL AND extra = ''`+rated+`
		  GROUP BY letter ORDER BY letter`, args...)
	if err != nil {
		return nil, fmt.Errorf("list first letters: %w", err)
	}
	defer rows.Close()

	// Gathered in Go rather than grouped in SQL, because "not a letter"
	// cannot be expressed in SQLite's GROUP BY without a CASE that would
	// have to agree with the Go below anyway.
	counts := map[string]int{}
	for rows.Next() {
		var (
			letter string
			held   int
		)
		if err := rows.Scan(&letter, &held); err != nil {
			return nil, fmt.Errorf("scan first letter: %w", err)
		}
		key := "#"
		if len(letter) == 1 && letter[0] >= 'A' && letter[0] <= 'Z' {
			key = letter
		}
		counts[key] += held
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]FirstLetter, 0, len(counts))
	if held, found := counts["#"]; found {
		out = append(out, FirstLetter{Key: "#", Title: "#", Held: held})
	}
	for letter := 'A'; letter <= 'Z'; letter++ {
		key := string(letter)
		if held, found := counts[key]; found {
			out = append(out, FirstLetter{Key: key, Title: key, Held: held})
		}
	}
	return out, nil
}

// Minutes reads a setting that is an interval in minutes, and zero for one
// that is unset or unreadable.
//
// Zero for unreadable rather than an error, because every one of these means
// "never" at zero: a corrupted interval should stop a pass running by itself
// rather than stop the server starting.
func (d *DB) Minutes(ctx context.Context, key string) (int, error) {
	raw, err := d.ServerValue(ctx, key)
	if err != nil {
		return 0, err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	minutes, err := strconv.Atoi(raw)
	if err != nil || minutes < 0 {
		return 0, nil
	}
	return minutes, nil
}

// lastRunKey is where a pass's own clock is kept, beside its interval.
func lastRunKey(key string) string { return key + "_last" }

// LastRun is when a scheduled pass last started, and the zero time for one
// that never has.
func (d *DB) LastRun(ctx context.Context, key string) (time.Time, error) {
	raw, err := d.ServerValue(ctx, lastRunKey(key))
	if err != nil {
		return time.Time{}, err
	}
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, nil
	}
	millis, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return time.Time{}, nil
	}
	return fromMillis(millis), nil
}

// MarkRun records that a scheduled pass started.
//
// Written before the pass runs, for the reason a library's visit is: a pass
// that fails must be retried at its own interval rather than every minute.
func (d *DB) MarkRun(ctx context.Context, key string, when time.Time) error {
	return d.SetServerValue(ctx, lastRunKey(key), strconv.FormatInt(millis(when), 10))
}
