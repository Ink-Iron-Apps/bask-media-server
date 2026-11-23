package store_test

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// A library with enough variety for rules to bite on: three films that differ
// in year, length, certificate, edition and pixels, and a two-episode series
// to have something with children in it.
type ruled struct {
	*shelf
	shows   int64
	long    int64 // Epic, 2024, 150 minutes, 15, 4K, Director's Cut
	short   int64 // Sketch, 1999, 20 minutes, PG, 480 lines
	unrated int64 // Lost Reel, 1972, 90 minutes, no certificate
	show    int64
	first   int64
	second  int64
	viewer  int64
}

func newRuled(t *testing.T) *ruled {
	t.Helper()
	s := newShelf(t)

	shows, err := s.db.CreateSection(s.ctx, "Series", domain.SectionShows, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	writer := s.db.NewWriter(10)
	film := func(title string, year, minutes, height int, edition string) {
		t.Helper()
		if err := writer.Add(s.ctx, store.Found{
			SectionID: s.films, Kind: domain.KindMovie, Title: title, Year: year,
			Duration: time.Duration(minutes) * time.Minute,
			Height:   height, Edition: edition,
			Path: t.TempDir() + "/" + title + ".mkv", Container: "mkv", Size: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	film("Epic", 2024, 150, 2160, "Director's Cut")
	film("Sketch", 1999, 20, 480, "")
	film("Lost Reel", 1972, 90, 1080, "")

	episode := func(number int, title string) {
		t.Helper()
		if err := writer.Add(s.ctx, store.Found{
			SectionID: shows.ID, Kind: domain.KindEpisode,
			Show: "Hinterland", ShowYear: 2020, SeasonNum: 1, EpisodeNum: number,
			Title: title, Duration: 45 * time.Minute,
			Path: t.TempDir() + "/" + title + ".mkv", Container: "mkv", Size: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	episode(1, "Arrivals")
	episode(2, "Departures")
	if err := writer.Commit(s.ctx); err != nil {
		t.Fatal(err)
	}

	r := &ruled{shelf: s, shows: shows.ID}
	r.long = s.find(t, s.films, domain.KindMovie, "Epic")
	r.short = s.find(t, s.films, domain.KindMovie, "Sketch")
	r.unrated = s.find(t, s.films, domain.KindMovie, "Lost Reel")
	r.show = s.find(t, shows.ID, domain.KindShow, "Hinterland")
	r.first = s.findDeep(t, shows.ID, domain.KindEpisode, "Arrivals")
	r.second = s.findDeep(t, shows.ID, domain.KindEpisode, "Departures")

	if err := s.db.SetRating(s.ctx, r.long, "15"); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetRating(s.ctx, r.short, "PG"); err != nil {
		t.Fatal(err)
	}

	viewer, err := s.db.CreateUser(s.ctx, "sam", false)
	if err != nil {
		t.Fatal(err)
	}
	r.viewer = viewer.ID
	return r
}

// smart makes a collection filled by these rules and returns its id.
func (r *ruled) smart(t *testing.T, title string, rules domain.Smart) int64 {
	t.Helper()
	made, err := r.db.CreateCollection(r.ctx, title, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.db.SetCollectionRules(r.ctx, made.ID, &rules); err != nil {
		t.Fatal(err)
	}
	return made.ID
}

// titles is what a collection holds, by name, as this handle sees it.
func titles(t *testing.T, db *store.DB, r *ruled, id int64) []string {
	t.Helper()
	items, _, err := db.CollectionItems(r.ctx, id, store.Page{Size: 100})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.Title)
	}
	return out
}

func same(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func one(field domain.SmartField, op domain.SmartOp, value string) domain.Smart {
	return domain.Smart{Rules: []domain.SmartRule{{Field: field, Op: op, Value: value}}}
}

/* ---------- the rules themselves ---------- */

func TestEachRuleAsksWhatItSays(t *testing.T) {
	r := newRuled(t)

	for _, c := range []struct {
		name  string
		rules domain.Smart
		want  []string
	}{
		{"a library", one(domain.SmartLibrary, domain.SmartIs, itoa(r.films)),
			[]string{"Arrival", "Brazil", "Epic", "Lost Reel", "Sketch"}},
		{"not a library", one(domain.SmartLibrary, domain.SmartIsNot, itoa(r.films)),
			[]string{"The Album", "The Band", "Hinterland"}},
		{"a kind", one(domain.SmartKind, domain.SmartIs, string(domain.KindShow)),
			[]string{"Hinterland"}},
		{"a title containing", one(domain.SmartTitle, domain.SmartContains, "ske"),
			[]string{"Sketch"}},
		{"a title starting", one(domain.SmartTitle, domain.SmartStarts, "Lost"),
			[]string{"Lost Reel"}},
		{"before a year", one(domain.SmartYear, domain.SmartBefore, "2000"),
			[]string{"Brazil", "Lost Reel", "Sketch"}},
		{"after a year", one(domain.SmartYear, domain.SmartAfter, "2000"),
			[]string{"Arrival", "Epic", "Hinterland"}},
		{"a certificate", one(domain.SmartCertificate, domain.SmartIs, "PG"),
			[]string{"Sketch"}},
		{"no certificate at all", one(domain.SmartCertificate, domain.SmartIs, ""),
			[]string{"The Album", "Arrival", "The Band", "Brazil", "Hinterland", "Lost Reel"}},
		{"an age rating", one(domain.SmartAge, domain.SmartAtMost, "8"),
			[]string{"Sketch"}},
		{"added recently", one(domain.SmartAdded, domain.SmartWithin, "1"),
			[]string{"The Album", "Arrival", "The Band", "Brazil", "Epic", "Hinterland", "Lost Reel", "Sketch"}},
		{"added long ago", one(domain.SmartAdded, domain.SmartBefore, "1"), nil},
		{"at least so long", one(domain.SmartDuration, domain.SmartAtLeast, "100"),
			[]string{"Epic"}},
		{"at most so long", one(domain.SmartDuration, domain.SmartAtMost, "30"),
			[]string{"Sketch"}},
		{"enough pixels", one(domain.SmartResolution, domain.SmartAtLeast, "2000"),
			[]string{"Epic"}},
		{"a container", one(domain.SmartContainer, domain.SmartIs, "MKV"),
			[]string{"Arrival", "Brazil", "Epic", "Hinterland", "Lost Reel", "Sketch"}},
		{"an edition", one(domain.SmartEdition, domain.SmartContains, "director"),
			[]string{"Epic"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Sorted by title, which is the default, so the expectation can
			// be written down.
			id := r.smart(t, c.name, c.rules)
			got := titles(t, r.db, r, id)
			if !same(got, c.want) {
				t.Fatalf("%s matched %v, wanted %v", c.name, got, c.want)
			}
		})
	}
}

// A rule about pixels or a container is a rule about files, and a series has
// none of its own: its episodes do. Asking the question of a show has to look
// inside it, or every series falls out of every rule about how a thing was
// encoded.
func TestARuleAboutFilesLooksInsideASeries(t *testing.T) {
	r := newRuled(t)

	id := r.smart(t, "series in mkv", domain.Smart{Rules: []domain.SmartRule{
		{Field: domain.SmartKind, Op: domain.SmartIs, Value: string(domain.KindShow)},
		{Field: domain.SmartContainer, Op: domain.SmartIs, Value: "mkv"},
	}})
	if got := titles(t, r.db, r, id); !same(got, []string{"Hinterland"}) {
		t.Fatalf("a series whose episodes are mkv was not found: %v", got)
	}
}

// Matching any rule rather than every rule, which is the difference between
// "unwatched science fiction" and "either".
func TestMatchingAnyRuleWidensRatherThanNarrows(t *testing.T) {
	r := newRuled(t)

	rules := domain.Smart{Match: domain.SmartAny, Rules: []domain.SmartRule{
		{Field: domain.SmartTitle, Op: domain.SmartIs, Value: "Epic"},
		{Field: domain.SmartTitle, Op: domain.SmartIs, Value: "Sketch"},
	}}
	if got := titles(t, r.db, r, r.smart(t, "either", rules)); !same(got, []string{"Epic", "Sketch"}) {
		t.Fatalf("any = %v", got)
	}

	rules.Match = domain.SmartAll
	if got := titles(t, r.db, r, r.smart(t, "both", rules)); len(got) != 0 {
		t.Fatalf("nothing is called two things, got %v", got)
	}
}

// A year nobody filled in must not be read as the year nought: "made before
// 1980" is a question about films we know the year of.
func TestAnUnknownYearIsNotTheYearNought(t *testing.T) {
	r := newRuled(t)

	id := r.smart(t, "old", one(domain.SmartYear, domain.SmartBefore, "1980"))
	for _, title := range titles(t, r.db, r, id) {
		if title == "The Album" || title == "The Band" {
			t.Fatalf("something with no year was filed as ancient: %v", titles(t, r.db, r, id))
		}
	}
}

// The same trap on the age rating, where unknown is stored as -1 and would
// slip under every ceiling.
func TestAnUnratedFilmIsNotRatedZero(t *testing.T) {
	r := newRuled(t)

	id := r.smart(t, "for children", one(domain.SmartAge, domain.SmartAtMost, "12"))
	for _, title := range titles(t, r.db, r, id) {
		if title == "Lost Reel" {
			t.Fatal("a film nobody rated was offered as suitable for a twelve year old")
		}
	}
}

/* ---------- watch state ---------- */

// Watched is the asking account's, and two accounts genuinely get two
// different collections out of one rule set. That is the question, not a bug
// in the answer.
func TestWatchedIsAnsweredForWhoeverAsks(t *testing.T) {
	r := newRuled(t)

	if err := r.db.SaveProgress(r.ctx, domain.Progress{
		UserID: r.user, ItemID: r.long, Watched: true,
		Offset: 150 * time.Minute, Duration: 150 * time.Minute,
	}); err != nil {
		t.Fatal(err)
	}

	id := r.smart(t, "not seen", domain.Smart{Rules: []domain.SmartRule{
		{Field: domain.SmartKind, Op: domain.SmartIs, Value: string(domain.KindMovie)},
		{Field: domain.SmartWatched, Op: domain.SmartNo},
	}})

	seen := titles(t, r.db.Viewer(r.user), r, id)
	if !same(seen, []string{"Arrival", "Brazil", "Lost Reel", "Sketch"}) {
		t.Fatalf("the account that watched Epic still sees it: %v", seen)
	}
	other := titles(t, r.db.Viewer(r.viewer), r, id)
	if !same(other, []string{"Arrival", "Brazil", "Epic", "Lost Reel", "Sketch"}) {
		t.Fatalf("an account that watched nothing is missing something: %v", other)
	}
}

// A series is watched when its episodes are, and part way through when some
// of them are. The state lives on the episodes and the question is asked of
// the series, which is the whole difficulty.
func TestASeriesIsWatchedWhenItsEpisodesAre(t *testing.T) {
	r := newRuled(t)
	me := r.db.Viewer(r.user)

	partly := r.smart(t, "started", domain.Smart{Rules: []domain.SmartRule{
		{Field: domain.SmartKind, Op: domain.SmartIs, Value: string(domain.KindShow)},
		{Field: domain.SmartWatched, Op: domain.SmartPartly},
	}})
	through := r.smart(t, "finished", domain.Smart{Rules: []domain.SmartRule{
		{Field: domain.SmartKind, Op: domain.SmartIs, Value: string(domain.KindShow)},
		{Field: domain.SmartWatched, Op: domain.SmartYes},
	}})

	if got := titles(t, me, r, partly); len(got) != 0 {
		t.Fatalf("nothing has been watched yet, got %v", got)
	}

	watch := func(id int64) {
		t.Helper()
		if err := r.db.SaveProgress(r.ctx, domain.Progress{
			UserID: r.user, ItemID: id, Watched: true,
			Offset: 45 * time.Minute, Duration: 45 * time.Minute,
		}); err != nil {
			t.Fatal(err)
		}
	}

	watch(r.first)
	if got := titles(t, me, r, partly); !same(got, []string{"Hinterland"}) {
		t.Fatalf("one episode of two watched is part way through, got %v", got)
	}
	if got := titles(t, me, r, through); len(got) != 0 {
		t.Fatalf("one episode of two is not the whole series, got %v", got)
	}

	watch(r.second)
	if got := titles(t, me, r, through); !same(got, []string{"Hinterland"}) {
		t.Fatalf("both episodes watched is a watched series, got %v", got)
	}
	if got := titles(t, me, r, partly); len(got) != 0 {
		t.Fatalf("a finished series is not part way through, got %v", got)
	}
}

// A handle with nobody behind it has watched nothing, rather than failing or
// matching everything: a surface that lists collections without an account
// still has to list them.
func TestAHandleWithNoAccountHasWatchedNothing(t *testing.T) {
	r := newRuled(t)
	if err := r.db.SaveProgress(r.ctx, domain.Progress{
		UserID: r.user, ItemID: r.long, Watched: true,
		Offset: 150 * time.Minute, Duration: 150 * time.Minute,
	}); err != nil {
		t.Fatal(err)
	}

	id := r.smart(t, "unseen films", domain.Smart{Rules: []domain.SmartRule{
		{Field: domain.SmartKind, Op: domain.SmartIs, Value: string(domain.KindMovie)},
		{Field: domain.SmartWatched, Op: domain.SmartNo},
	}})
	if got := titles(t, r.db, r, id); !same(got, []string{"Arrival", "Brazil", "Epic", "Lost Reel", "Sketch"}) {
		t.Fatalf("an anonymous handle = %v", got)
	}
}

/* ---------- order, limit and paging ---------- */

func TestTheOrderAndTheLimitAreTheCollections(t *testing.T) {
	r := newRuled(t)

	id := r.smart(t, "two newest films", domain.Smart{
		Rules: []domain.SmartRule{{Field: domain.SmartKind, Op: domain.SmartIs, Value: string(domain.KindMovie)}},
		Sort:  domain.SmartByYear, Desc: true, Limit: 2,
	})

	got := titles(t, r.db, r, id)
	if !same(got, []string{"Epic", "Arrival"}) {
		t.Fatalf("newest two = %v", got)
	}

	// The limit is the collection's, not the request's: a client asking for
	// five hundred gets two, and the count agrees with what it was sent.
	items, total, err := r.db.CollectionItems(r.ctx, id, store.Page{Size: 500})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("held %d, sent %d", total, len(items))
	}

	// And a page beyond the limit is empty rather than carrying on into the
	// rest of the library.
	beyond, total, err := r.db.CollectionItems(r.ctx, id, store.Page{Start: 2, Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(beyond) != 0 || total != 2 {
		t.Fatalf("page past the limit held %d of %d", len(beyond), total)
	}
}

func TestPagingASmartCollectionNeitherRepeatsNorSkips(t *testing.T) {
	r := newRuled(t)

	// Five films: the three with rules to bite on and the shelf's own two.
	id := r.smart(t, "films", one(domain.SmartKind, domain.SmartIs, string(domain.KindMovie)))
	seen := map[string]bool{}
	for start := 0; start < 5; start++ {
		items, total, err := r.db.CollectionItems(r.ctx, id, store.Page{Start: start, Size: 1})
		if err != nil {
			t.Fatal(err)
		}
		if total != 5 {
			t.Fatalf("total = %d", total)
		}
		if len(items) != 1 {
			t.Fatalf("page %d held %d", start, len(items))
		}
		if seen[items[0].Title] {
			t.Fatalf("%q came back twice", items[0].Title)
		}
		seen[items[0].Title] = true
	}
	if len(seen) != 5 {
		t.Fatalf("saw %v", seen)
	}
}

/* ---------- what a viewer may see ---------- */

// The library restriction applies to a smart collection exactly as it does to
// a hand-picked one, and a collection whose every answer is in a hidden
// library is hidden rather than empty: an empty shelf tells a viewer
// something exists.
func TestASmartCollectionObeysTheLibraryRestriction(t *testing.T) {
	r := newRuled(t)

	id := r.smart(t, "every series", one(domain.SmartKind, domain.SmartIs, string(domain.KindShow)))

	if got := titles(t, r.db, r, id); !same(got, []string{"Hinterland"}) {
		t.Fatalf("unrestricted = %v", got)
	}
	if got := titles(t, r.onlyFilms(), r, id); len(got) != 0 {
		t.Fatalf("a films-only device saw a series: %v", got)
	}

	if _, err := r.onlyFilms().Collection(r.ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a collection it can see nothing in was offered: %v", err)
	}
	for _, c := range mustCollections(t, r, r.onlyFilms()) {
		if c.ID == id {
			t.Fatal("an empty-for-this-device collection was listed")
		}
	}
	// The administration interface wants the opposite, and asks by name.
	if _, err := r.db.AnyCollection(r.ctx, id); err != nil {
		t.Fatalf("the editor cannot read it: %v", err)
	}
}

func mustCollections(t *testing.T, r *ruled, db *store.DB) []domain.Collection {
	t.Helper()
	all, err := db.Collections(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return all
}

// A smart collection holding nothing at all is hidden from a viewer the same
// way an empty hand-picked one is — and it has no rows in collection_items to
// be dropped by, so the listing has to ask its rules before deciding.
func TestASmartCollectionMatchingNothingIsHidden(t *testing.T) {
	r := newRuled(t)

	id := r.smart(t, "nothing", one(domain.SmartTitle, domain.SmartIs, "A Film Nobody Owns"))
	for _, c := range mustCollections(t, r, r.db) {
		if c.ID == id {
			t.Fatal("a collection matching nothing was offered to a viewer")
		}
	}
	all, err := r.db.EveryCollection(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range all {
		if c.ID == id {
			found = true
			if c.Smart == nil {
				t.Fatal("the editor was handed a smart collection with no rules")
			}
		}
	}
	if !found {
		t.Fatal("the editor cannot see the collection it just made")
	}
}

// What a listing says it holds has to be what it sends, or a client pages
// towards rows it will never be given.
func TestTheCountMatchesWhatComesBack(t *testing.T) {
	r := newRuled(t)

	id := r.smart(t, "films again", one(domain.SmartKind, domain.SmartIs, string(domain.KindMovie)))
	for _, db := range []*store.DB{r.db, r.onlyFilms()} {
		var held int
		for _, c := range mustCollections(t, r, db) {
			if c.ID == id {
				held = c.Held
			}
		}
		items, total, err := db.CollectionItems(r.ctx, id, store.Page{Size: 100})
		if err != nil {
			t.Fatal(err)
		}
		if held != total || total != len(items) {
			t.Fatalf("listed %d, counted %d, sent %d", held, total, len(items))
		}
	}
}

/* ---------- the two kinds of collection do not mix ---------- */

func TestRulesAndAHandPickedListAreNeverBothInForce(t *testing.T) {
	r := newRuled(t)

	made, err := r.db.CreateCollection(r.ctx, "Mixed", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.db.SetCollectionItems(r.ctx, made.ID, []int64{r.long}); err != nil {
		t.Fatal(err)
	}

	// Giving it rules drops what was picked by hand, rather than leaving a
	// list nothing reads.
	if err := r.db.SetCollectionRules(r.ctx, made.ID,
		&domain.Smart{Rules: []domain.SmartRule{{Field: domain.SmartTitle, Op: domain.SmartIs, Value: "Sketch"}}}); err != nil {
		t.Fatal(err)
	}
	if got := titles(t, r.db, r, made.ID); !same(got, []string{"Sketch"}) {
		t.Fatalf("the rules did not take over: %v", got)
	}
	if err := r.db.SetCollectionItems(r.ctx, made.ID, []int64{r.long}); !errors.Is(err, store.ErrSmartCollection) {
		t.Fatalf("filling a smart collection by hand = %v", err)
	}

	// And taking the rules away leaves an ordinary empty collection, because
	// the rules were its contents.
	if err := r.db.SetCollectionRules(r.ctx, made.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got := titles(t, r.db, r, made.ID); len(got) != 0 {
		t.Fatalf("a collection whose rules were removed still holds %v", got)
	}
	if err := r.db.SetCollectionItems(r.ctx, made.ID, []int64{r.long}); err != nil {
		t.Fatal(err)
	}
	if got := titles(t, r.db, r, made.ID); !same(got, []string{"Epic"}) {
		t.Fatalf("hand-picked again = %v", got)
	}
}

// An item says which collections it is part of, and a smart one counts: the
// membership is not stored anywhere to join against, so it is asked.
func TestAnItemKnowsWhichSmartCollectionsHoldIt(t *testing.T) {
	r := newRuled(t)

	id := r.smart(t, "Long films", one(domain.SmartDuration, domain.SmartAtLeast, "100"))
	parts, err := r.db.CollectionsOf(r.ctx, []int64{r.long, r.short})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts[r.long]) != 1 || parts[r.long][0].ID != id {
		t.Fatalf("the long film is in %v", parts[r.long])
	}
	if len(parts[r.short]) != 0 {
		t.Fatalf("the short film is in %v", parts[r.short])
	}
}

/* ---------- rules that will not do ---------- */

func TestARuleSetTheServerCannotAnswerIsRefused(t *testing.T) {
	r := newRuled(t)
	made, err := r.db.CreateCollection(r.ctx, "Refused", "")
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name  string
		rules domain.Smart
	}{
		// Composer is a real credit on a real film and a field this server
		// has no rule for, which is the case worth refusing: a rule nobody
		// can answer must not be stored as one that matches nothing.
		{"a field nobody has", one("composer", domain.SmartIs, "Jóhannsson")},
		{"a comparison the field cannot take", one(domain.SmartYear, domain.SmartContains, "198")},
		{"a year that is not a number", one(domain.SmartYear, domain.SmartIs, "nineteen")},
		{"a library that is not a library", one(domain.SmartLibrary, domain.SmartIs, "films")},
		{"a kind nobody has", one(domain.SmartKind, domain.SmartIs, "podcast")},
		{"nothing to compare against", one(domain.SmartTitle, domain.SmartContains, "")},
		{"an order nobody can sort by", domain.Smart{Sort: "loudness"}},
		{"a way of reading nobody has", domain.Smart{Match: "some"}},
		{"a negative limit", domain.Smart{Limit: -1}},
	} {
		if err := r.db.SetCollectionRules(r.ctx, made.ID, &c.rules); err == nil {
			t.Fatalf("%s was accepted", c.name)
		}
	}

	// And none of it was stored: the collection is still the hand-picked one
	// it was made as.
	found, err := r.db.AnyCollection(r.ctx, made.ID)
	if err != nil {
		t.Fatal(err)
	}
	if found.Smart != nil {
		t.Fatalf("a refused rule set was stored anyway: %+v", found.Smart)
	}
}

// A per-cent sign in a title is a title, not a wildcard.
func TestAWildcardInAValueIsNotAWildcard(t *testing.T) {
	r := newRuled(t)

	id := r.smart(t, "odd", one(domain.SmartTitle, domain.SmartContains, "%"))
	if got := titles(t, r.db, r, id); len(got) != 0 {
		t.Fatalf("a per-cent sign matched everything: %v", got)
	}
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }
