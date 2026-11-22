package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// household is a library with one film per rating and one account.
type household struct {
	db   *store.DB
	ctx  context.Context
	user int64
	// films, by the rating they were given.
	films map[string]int64
}

func newHousehold(t *testing.T) *household {
	t.Helper()
	db, ctx := open(t)
	section, err := db.CreateSection(ctx, "Films", domain.SectionMovies, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	ratings := map[string]string{
		"Paddington":    "U",
		"Spider-Man":    "PG-13",
		"Heat":          "15",
		"Casino":        "R",
		"Something Odd": "",
	}
	writer := db.NewWriter(10)
	for title := range ratings {
		if err := writer.Add(ctx, store.Found{
			SectionID: section.ID, Kind: domain.KindMovie, Title: title, Year: 2000,
			Path: filepath.Join("/library", title+".mkv"), Container: "mkv", Size: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	out := &household{db: db, ctx: ctx, films: map[string]int64{}}
	items, _, err := db.SectionItems(ctx, section.ID, domain.KindMovie, store.Page{Size: 20})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		rating := ratings[item.Title]
		if rating != "" {
			if err := db.SetRating(ctx, item.ID, rating); err != nil {
				t.Fatal(err)
			}
		}
		out.films[item.Title] = item.ID
	}

	user, err := db.CreateUser(ctx, "child", false)
	if err != nil {
		t.Fatal(err)
	}
	out.user = user.ID
	return out
}

func (h *household) titles(t *testing.T, db *store.DB) map[string]bool {
	t.Helper()
	sections, err := db.Sections(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, section := range sections {
		items, _, err := db.SectionItems(h.ctx, section.ID, domain.KindMovie, store.Page{Size: 50})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			seen[item.Title] = true
		}
	}
	return seen
}

// A ceiling is one number against every rating system a mixed library
// carries: a British 15 and an American R are both above a twelve-year-old's.
func TestARatingCeilingHidesWhatIsAboveIt(t *testing.T) {
	h := newHousehold(t)

	seen := h.titles(t, h.db.Rated(12, true))
	if !seen["Paddington"] {
		t.Error("a U film was hidden from a twelve-year-old")
	}
	for _, blocked := range []string{"Spider-Man", "Heat", "Casino"} {
		if seen[blocked] {
			t.Errorf("%q is above the ceiling and was listed", blocked)
		}
	}
	// Unrated is allowed through here, which is what keeps a half-enriched
	// library watchable.
	if !seen["Something Odd"] {
		t.Error("an unrated film was hidden when unrated was allowed")
	}

	// And hiding the unrated is the other half of the setting.
	strict := h.titles(t, h.db.Rated(12, false))
	if strict["Something Odd"] {
		t.Error("an unrated film was listed when unrated was not allowed")
	}
	if !strict["Paddington"] {
		t.Error("a U film was hidden by the strict setting")
	}
}

// No ceiling is no filter at all: the unrestricted handle is byte for byte
// the statement it always was.
func TestNoCeilingSeesEverything(t *testing.T) {
	h := newHousehold(t)
	seen := h.titles(t, h.db)
	if len(seen) != 5 {
		t.Fatalf("the unrestricted handle saw %v", seen)
	}
	// And so does a handle whose ceiling was set to no limit.
	if seen := h.titles(t, h.db.Rated(domain.UnknownAge, true)); len(seen) != 5 {
		t.Fatalf("a handle with no limit saw %v", seen)
	}
}

// The ceiling applies to every way into an item, not only to the listing: an
// item that cannot be listed must not be readable by its number either.
func TestACeilingAppliesToEveryWayIn(t *testing.T) {
	h := newHousehold(t)
	restricted := h.db.Rated(12, true)
	blocked := h.films["Casino"]

	if _, err := restricted.Item(h.ctx, blocked); err == nil {
		t.Fatal("a film above the ceiling was readable by its id")
	}
	if found, _, err := restricted.Search(h.ctx, "casino", store.Page{Size: 10}); err != nil || len(found) != 0 {
		t.Fatalf("search found %v (%v)", found, err)
	}
	// And the one below it still is, so the predicate is a filter rather
	// than a wall.
	if _, err := restricted.Item(h.ctx, h.films["Paddington"]); err != nil {
		t.Fatalf("a U film was not readable: %v", err)
	}
}

// A library restriction and a rating ceiling are independent: an account can
// have either, both or neither.
func TestTheTwoRestrictionsAreIndependent(t *testing.T) {
	h := newHousehold(t)
	sections, err := h.db.Sections(h.ctx)
	if err != nil {
		t.Fatal(err)
	}

	both := h.db.Visible([]int64{sections[0].ID}).Rated(12, true)
	seen := h.titles(t, both)
	if !seen["Paddington"] || seen["Casino"] {
		t.Fatalf("both restrictions together saw %v", seen)
	}
	// A rating ceiling alone does not hide the library itself.
	listed, err := h.db.Rated(0, false).Sections(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("a rating ceiling hid %d libraries", 1-len(listed))
	}
}

// A season and an episode take the series' rating, because a provider rates
// the series: without that a ceiling would hide a show and show every episode
// in it.
func TestAnEpisodeInheritsTheSeriesRating(t *testing.T) {
	db, ctx := open(t)
	section, err := db.CreateSection(ctx, "TV", domain.SectionShows, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	writer := db.NewWriter(1)
	if err := writer.Add(ctx, store.Found{
		SectionID: section.ID, Kind: domain.KindEpisode,
		Show: "Oz", SeasonNum: 1, EpisodeNum: 1, Title: "The Routine",
		Path: filepath.Join("/library", "s01e01.mkv"), Container: "mkv", Size: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	shows, _, err := db.SectionItems(ctx, section.ID, domain.KindShow, store.Page{Size: 5})
	if err != nil || len(shows) != 1 {
		t.Fatalf("shows = %v (%v)", shows, err)
	}
	if err := db.SetRating(ctx, shows[0].ID, "TV-MA"); err != nil {
		t.Fatal(err)
	}
	if err := db.InheritRatings(ctx); err != nil {
		t.Fatal(err)
	}

	// Nothing under an 18 series is visible to a twelve-year-old.
	restricted := db.Rated(12, true)
	seasons, _, err := restricted.Children(ctx, shows[0].ID, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(seasons) != 0 {
		t.Fatalf("a season of an adult series was listed: %v", seasons)
	}
	// Reached from the unrestricted handle, the episode carries the rating.
	allSeasons, _, err := db.Children(ctx, shows[0].ID, store.Page{Size: 10})
	if err != nil || len(allSeasons) != 1 {
		t.Fatalf("seasons = %v (%v)", allSeasons, err)
	}
	episodes, _, err := db.Children(ctx, allSeasons[0].ID, store.Page{Size: 10})
	if err != nil || len(episodes) != 1 {
		t.Fatalf("episodes = %v (%v)", episodes, err)
	}
	if episodes[0].RatingAge != 17 {
		t.Fatalf("the episode is rated %d (%q)", episodes[0].RatingAge, episodes[0].Rating)
	}
}

// The limits an account carries, written and read back.
func TestAccountLimitsRoundTrip(t *testing.T) {
	h := newHousehold(t)

	limits, err := h.db.UserLimits(h.ctx, h.user)
	if err != nil {
		t.Fatal(err)
	}
	if limits.Ceiling != domain.UnknownAge || !limits.AllowUnrated || limits.MaxStreams != 0 || limits.HasPIN {
		t.Fatalf("a new account starts restricted: %+v", limits)
	}

	if err := h.db.SetUserLimits(h.ctx, h.user, store.Limits{
		Ceiling: 12, AllowUnrated: false, MaxStreams: 2, MaxBitRate: 4_000_000,
	}); err != nil {
		t.Fatal(err)
	}
	limits, err = h.db.UserLimits(h.ctx, h.user)
	if err != nil {
		t.Fatal(err)
	}
	if limits.Ceiling != 12 || limits.AllowUnrated || limits.MaxStreams != 2 || limits.MaxBitRate != 4_000_000 {
		t.Fatalf("limits = %+v", limits)
	}
}

// The PIN that lifts a ceiling: stretched, never read back, and wrong by
// default rather than right.
func TestThePINIsCheckedAndNeverReadBack(t *testing.T) {
	h := newHousehold(t)

	// An account with no PIN says no to everything, including an empty
	// guess: it has not agreed to anything being unlocked.
	for _, guess := range []string{"", "0000", "1234"} {
		if ok, err := h.db.CheckUserPIN(h.ctx, h.user, guess); err != nil || ok {
			t.Fatalf("an account with no PIN accepted %q (%v)", guess, err)
		}
	}

	if err := h.db.SetUserPIN(h.ctx, h.user, "4812"); err != nil {
		t.Fatal(err)
	}
	if ok, err := h.db.CheckUserPIN(h.ctx, h.user, "4812"); err != nil || !ok {
		t.Fatalf("the right PIN was refused (%v)", err)
	}
	if ok, _ := h.db.CheckUserPIN(h.ctx, h.user, "4813"); ok {
		t.Fatal("a wrong PIN was accepted")
	}
	limits, err := h.db.UserLimits(h.ctx, h.user)
	if err != nil {
		t.Fatal(err)
	}
	if !limits.HasPIN {
		t.Fatal("the limits do not say a PIN is set")
	}

	// Too short is refused rather than stored, and an empty one clears it.
	if err := h.db.SetUserPIN(h.ctx, h.user, "12"); err == nil {
		t.Fatal("a two-digit PIN was accepted")
	}
	if err := h.db.SetUserPIN(h.ctx, h.user, "letters"); err == nil {
		t.Fatal("a PIN that is not digits was accepted")
	}
	if err := h.db.SetUserPIN(h.ctx, h.user, ""); err != nil {
		t.Fatal(err)
	}
	if ok, _ := h.db.CheckUserPIN(h.ctx, h.user, "4812"); ok {
		t.Fatal("the cleared PIN still works")
	}
}
