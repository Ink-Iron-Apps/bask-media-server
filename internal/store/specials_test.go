package store_test

import (
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// doctor builds the case this feature exists for: two series with a
// Christmas special between them that advances the story.
type doctor struct {
	*shelf
	shows int64
	show  int64
}

func newDoctor(t *testing.T) *doctor {
	t.Helper()
	s := newShelf(t)
	section, err := s.db.CreateSection(s.ctx, "Series", domain.SectionShows, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	writer := s.db.NewWriter(20)
	add := func(season, number int, title string) {
		t.Helper()
		if err := writer.Add(s.ctx, store.Found{
			SectionID: section.ID, Kind: domain.KindEpisode,
			Show: "Doctor Who", ShowYear: 2005,
			SeasonNum: season, EpisodeNum: number, Title: title,
			Path: t.TempDir() + "/" + title + ".mkv", Container: "mkv", Size: 1,
			Duration: 45 * time.Minute,
		}); err != nil {
			t.Fatal(err)
		}
	}
	add(1, 1, "Rose")
	add(1, 13, "The Parting of the Ways")
	add(2, 1, "New Earth")
	add(2, 13, "Doomsday")
	add(3, 1, "Smith and Jones")
	// The specials, in season zero, numbered in the order the archive put
	// them rather than the order they belong in.
	add(0, 1, "The Christmas Invasion")
	add(0, 2, "The Runaway Bride")
	if err := writer.Commit(s.ctx); err != nil {
		t.Fatal(err)
	}

	d := &doctor{shelf: s, shows: section.ID}
	d.show = s.find(t, section.ID, domain.KindShow, "Doctor Who")
	return d
}

// titles is the show's episodes in the order they should be watched.
func (d *doctor) titles(t *testing.T) []string {
	t.Helper()
	episodes, err := d.db.EpisodeOrder(d.ctx, d.show)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(episodes))
	for _, episode := range episodes {
		out = append(out, episode.Title)
	}
	return out
}

func (d *doctor) episode(t *testing.T, season, number int) int64 {
	t.Helper()
	episodes, err := d.db.EpisodeOrder(d.ctx, d.show)
	if err != nil {
		t.Fatal(err)
	}
	for _, episode := range episodes {
		parent, err := d.db.Item(d.ctx, *episode.ParentID)
		if err != nil {
			t.Fatal(err)
		}
		if parent.Index == season && episode.Index == number {
			return episode.ID
		}
	}
	t.Fatalf("no S%02dE%02d", season, number)
	return 0
}

func TestWithNothingKnownASpecialGoesLastRatherThanFirst(t *testing.T) {
	d := newDoctor(t)

	// Nothing has been enriched, so nothing is known about where the
	// specials belong. The end of the list is the one place that cannot be
	// mistaken for a deliberate answer — and it is emphatically not before
	// the pilot, which is where season-then-episode puts them.
	got := d.titles(t)
	if got[0] != "Rose" {
		t.Fatalf("the list starts with %q, not the pilot: %v", got[0], got)
	}
	if got[len(got)-1] != "The Runaway Bride" {
		t.Fatalf("the list ends with %q: %v", got[len(got)-1], got)
	}
	if len(got) != 7 {
		t.Fatalf("got %d episodes: %v", len(got), got)
	}
}

func TestAnAirDatePutsAChristmasSpecialBetweenTheSeries(t *testing.T) {
	d := newDoctor(t)

	// The real dates. Series one ended in June 2005, series two began in
	// April 2006, and The Christmas Invasion went out between them.
	dates := map[string]string{
		"Rose":                    "2005-03-26",
		"The Parting of the Ways": "2005-06-18",
		"The Christmas Invasion":  "2005-12-25",
		"New Earth":               "2006-04-15",
		"Doomsday":                "2006-07-08",
		"The Runaway Bride":       "2006-12-25",
		"Smith and Jones":         "2007-03-31",
	}
	for _, episode := range mustOrder(t, d) {
		when, err := time.Parse("2006-01-02", dates[episode.Title])
		if err != nil {
			t.Fatal(err)
		}
		if err := d.db.SetAired(d.ctx, episode.ID, when); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{
		"Rose", "The Parting of the Ways",
		"The Christmas Invasion",
		"New Earth", "Doomsday",
		"The Runaway Bride",
		"Smith and Jones",
	}
	got := d.titles(t)
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestTheTVDBPlacementBeatsTheAirDate(t *testing.T) {
	d := newDoctor(t)

	// A special whose date is misleading: broadcast long after the series it
	// belongs between, which is what a retrospective or a delayed release
	// looks like.
	for _, episode := range mustOrder(t, d) {
		switch episode.Title {
		case "Rose":
			setAired(t, d, episode.ID, "2005-03-26")
		case "The Parting of the Ways":
			setAired(t, d, episode.ID, "2005-06-18")
		case "New Earth":
			setAired(t, d, episode.ID, "2006-04-15")
		case "Doomsday":
			setAired(t, d, episode.ID, "2006-07-08")
		case "Smith and Jones":
			setAired(t, d, episode.ID, "2007-03-31")
		case "The Christmas Invasion":
			// Says 2010, which by date would land it at the very end.
			setAired(t, d, episode.ID, "2010-12-25")
			if err := d.db.SetPlacement(d.ctx, episode.ID, domain.Placement{
				AfterSeason:   1,
				BeforeSeason:  domain.NoPlacement,
				BeforeEpisode: domain.NoPlacement,
			}, false); err != nil {
				t.Fatal(err)
			}
		}
	}

	got := d.titles(t)
	// After series one, where the placement says, not at the end where the
	// date says.
	if got[2] != "The Christmas Invasion" {
		t.Fatalf("order = %v", got)
	}
}

func TestBeforeAParticularEpisode(t *testing.T) {
	d := newDoctor(t)

	for _, episode := range mustOrder(t, d) {
		if episode.Title != "The Runaway Bride" {
			continue
		}
		if err := d.db.SetPlacement(d.ctx, episode.ID, domain.Placement{
			AfterSeason:   domain.NoPlacement,
			BeforeSeason:  2,
			BeforeEpisode: 13,
		}, false); err != nil {
			t.Fatal(err)
		}
	}

	got := d.titles(t)
	// Immediately before Doomsday, which is S02E13.
	for i, title := range got {
		if title == "The Runaway Bride" {
			if i+1 >= len(got) || got[i+1] != "Doomsday" {
				t.Fatalf("order = %v", got)
			}
			return
		}
	}
	t.Fatalf("the special vanished: %v", got)
}

func TestAHandPlacementSurvivesAMetadataPass(t *testing.T) {
	d := newDoctor(t)
	var special int64
	for _, episode := range mustOrder(t, d) {
		if episode.Title == "The Christmas Invasion" {
			special = episode.ID
		}
	}

	mine := domain.Placement{AfterSeason: 2, BeforeSeason: domain.NoPlacement, BeforeEpisode: domain.NoPlacement}
	if err := d.db.SetPlacement(d.ctx, special, mine, true); err != nil {
		t.Fatal(err)
	}

	// A pass then says something else. It must not win: placing one special
	// by hand should not mean locking the item's title and artwork too, so
	// the placement carries its own flag.
	theirs := domain.Placement{AfterSeason: 1, BeforeSeason: domain.NoPlacement, BeforeEpisode: domain.NoPlacement}
	if err := d.db.SetPlacement(d.ctx, special, theirs, false); err != nil {
		t.Fatal(err)
	}

	item, err := d.db.Item(d.ctx, special)
	if err != nil {
		t.Fatal(err)
	}
	if item.Placement.AfterSeason != 2 {
		t.Fatalf("a pass overwrote a hand placement: %+v", item.Placement)
	}
	if !item.Placement.ByHand {
		t.Fatal("the hand placement lost its flag")
	}
	// And the item is not locked: its title and artwork are still a pass's
	// to fetch.
	if item.Locked {
		t.Fatal("placing a special locked the whole item")
	}
}

func TestPlacingSomethingThatIsNotThereIsRefused(t *testing.T) {
	d := newDoctor(t)
	if err := d.db.SetPlacement(d.ctx, 999999, domain.NewPlacement(), true); err == nil {
		t.Fatal("a placement was accepted for an item that does not exist")
	}
}

func setAired(t *testing.T, d *doctor, id int64, date string) {
	t.Helper()
	when, err := time.Parse("2006-01-02", date)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.db.SetAired(d.ctx, id, when); err != nil {
		t.Fatal(err)
	}
}

func mustOrder(t *testing.T, d *doctor) []domain.Item {
	t.Helper()
	episodes, err := d.db.EpisodeOrder(d.ctx, d.show)
	if err != nil {
		t.Fatal(err)
	}
	return episodes
}

func TestNextUpSkipsPastASpecialThatIsNotNext(t *testing.T) {
	d := newDoctor(t)

	// Real dates, so the Christmas special sits between the series.
	for _, episode := range mustOrder(t, d) {
		switch episode.Title {
		case "Rose":
			setAired(t, d, episode.ID, "2005-03-26")
		case "The Parting of the Ways":
			setAired(t, d, episode.ID, "2005-06-18")
		case "The Christmas Invasion":
			setAired(t, d, episode.ID, "2005-12-25")
		case "New Earth":
			setAired(t, d, episode.ID, "2006-04-15")
		case "Doomsday":
			setAired(t, d, episode.ID, "2006-07-08")
		case "The Runaway Bride":
			setAired(t, d, episode.ID, "2006-12-25")
		case "Smith and Jones":
			setAired(t, d, episode.ID, "2007-03-31")
		}
	}

	// Watched the first series through. What comes next is the Christmas
	// special, not New Earth — the whole point, since it is where the story
	// goes.
	for _, title := range []string{"Rose", "The Parting of the Ways"} {
		if err := d.db.SaveProgress(d.ctx, domain.Progress{
			UserID: d.user, ItemID: titleID(t, d, title), Watched: true,
			Offset: 45 * time.Minute, Duration: 45 * time.Minute,
		}); err != nil {
			t.Fatal(err)
		}
	}
	next := nextTitle(t, d)
	if next != "The Christmas Invasion" {
		t.Fatalf("next up = %q, want the Christmas special", next)
	}

	// Watch that too, and the next is the series it leads into.
	if err := d.db.SaveProgress(d.ctx, domain.Progress{
		UserID: d.user, ItemID: titleID(t, d, "The Christmas Invasion"), Watched: true,
		Offset: 60 * time.Minute, Duration: 60 * time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	if next := nextTitle(t, d); next != "New Earth" {
		t.Fatalf("next up = %q, want New Earth", next)
	}
}

func TestNextUpIsUnchangedForAShowWithNoSpecials(t *testing.T) {
	s := newShelf(t)
	section, err := s.db.CreateSection(s.ctx, "Series", domain.SectionShows, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	writer := s.db.NewWriter(10)
	for number, title := range map[int]string{1: "Arrivals", 2: "Departures"} {
		if err := writer.Add(s.ctx, store.Found{
			SectionID: section.ID, Kind: domain.KindEpisode,
			Show: "Hinterland", ShowYear: 2013, SeasonNum: 1, EpisodeNum: number, Title: title,
			Path: t.TempDir() + "/" + title + ".mkv", Container: "mkv", Size: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Commit(s.ctx); err != nil {
		t.Fatal(err)
	}

	first := s.findDeep(t, section.ID, domain.KindEpisode, "Arrivals")
	if err := s.db.SaveProgress(s.ctx, domain.Progress{
		UserID: s.user, ItemID: first, Watched: true,
		Offset: 45 * time.Minute, Duration: 45 * time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	up, err := s.db.NextUp(s.ctx, s.user, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(up) != 1 || up[0].Title != "Departures" {
		t.Fatalf("next up = %v", up)
	}
}

func titleID(t *testing.T, d *doctor, title string) int64 {
	t.Helper()
	for _, episode := range mustOrder(t, d) {
		if episode.Title == title {
			return episode.ID
		}
	}
	t.Fatalf("no episode called %q", title)
	return 0
}

func nextTitle(t *testing.T, d *doctor) string {
	t.Helper()
	up, err := d.db.NextUp(d.ctx, d.user, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(up) == 0 {
		t.Fatal("nothing was next up")
	}
	return up[0].Title
}

// tardisode adds a one-minute webisode that went out three days before New
// Earth, which is the case the length floor exists for.
func (d *doctor) tardisode(t *testing.T, number int, title string, length time.Duration) {
	t.Helper()
	writer := d.db.NewWriter(5)
	if err := writer.Add(d.ctx, store.Found{
		SectionID: d.shows, Kind: domain.KindEpisode,
		Show: "Doctor Who", ShowYear: 2005, SeasonNum: 0, EpisodeNum: number,
		Title: title, Duration: length,
		Path: t.TempDir() + "/" + title + ".mkv", Container: "mkv", Size: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(d.ctx); err != nil {
		t.Fatal(err)
	}
}

// realDates gives the series and the Christmas specials the dates they
// actually went out on.
func (d *doctor) realDates(t *testing.T) {
	t.Helper()
	dates := map[string]string{
		"Rose":                    "2005-03-26",
		"The Parting of the Ways": "2005-06-18",
		"The Christmas Invasion":  "2005-12-25",
		"New Earth":               "2006-04-15",
		"Doomsday":                "2006-07-08",
		"The Runaway Bride":       "2006-12-25",
		"Smith and Jones":         "2007-03-31",
		"Tardisode 1":             "2006-04-12",
		"Night of the Doctor":     "2013-11-14",
	}
	for _, episode := range mustOrder(t, d) {
		if date, found := dates[episode.Title]; found {
			setAired(t, d, episode.ID, date)
		}
	}
}

func TestAMinisodeDoesNotInterruptASeries(t *testing.T) {
	d := newDoctor(t)
	// One minute, against episodes of forty-five.
	d.tardisode(t, 20, "Tardisode 1", time.Minute)
	d.realDates(t)

	got := d.titles(t)
	want := []string{
		"Rose", "The Parting of the Ways",
		"The Christmas Invasion",
		"New Earth", "Doomsday",
		"The Runaway Bride",
		"Smith and Jones",
		// At the end, where it can still be played deliberately but is not
		// in the way of the series it is not part of.
		"Tardisode 1",
	}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	// And the hour-long Christmas special is still woven in, which is the
	// whole point of judging by length rather than by season.
	if got[2] != "The Christmas Invasion" {
		t.Fatalf("the Christmas special moved: %v", got)
	}
}

func TestAHandPlacementBeatsTheLengthFloor(t *testing.T) {
	d := newDoctor(t)
	// Seven minutes: a real story episode by any reading, and far too short
	// for the floor to let through on its own.
	d.tardisode(t, 30, "Night of the Doctor", 7*time.Minute)
	d.realDates(t)

	var short int64
	for _, episode := range mustOrder(t, d) {
		if episode.Title == "Night of the Doctor" {
			short = episode.ID
		}
	}
	if err := d.db.SetPlacement(d.ctx, short, domain.Placement{
		AfterSeason: 2, BeforeSeason: domain.NoPlacement, BeforeEpisode: domain.NoPlacement,
	}, true); err != nil {
		t.Fatal(err)
	}

	got := d.titles(t)
	// Somebody said it goes after series two, so it does — the floor is a
	// guess and this is not.
	for i, title := range got {
		if title == "Night of the Doctor" {
			if i == len(got)-1 {
				t.Fatalf("a hand placement lost to the length floor: %v", got)
			}
			if got[i-1] != "Doomsday" {
				t.Fatalf("placed after %q, want after Doomsday: %v", got[i-1], got)
			}
			return
		}
	}
	t.Fatalf("the special vanished: %v", got)
}

func TestWithNoDurationsNothingIsJudgedByLength(t *testing.T) {
	d := newDoctor(t)
	// A library that has not been probed: no episode has a duration, so
	// nothing can be judged by length and leaving every special out would
	// be worse than leaving them in.
	writer := d.db.NewWriter(5)
	if err := writer.Add(d.ctx, store.Found{
		SectionID: d.shows, Kind: domain.KindEpisode,
		Show: "Hinterland", ShowYear: 2013, SeasonNum: 1, EpisodeNum: 1, Title: "Arrivals",
		Path: t.TempDir() + "/arrivals.mkv", Container: "mkv", Size: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Add(d.ctx, store.Found{
		SectionID: d.shows, Kind: domain.KindEpisode,
		Show: "Hinterland", ShowYear: 2013, SeasonNum: 0, EpisodeNum: 1, Title: "A Short Thing",
		Path: t.TempDir() + "/short.mkv", Container: "mkv", Size: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(d.ctx); err != nil {
		t.Fatal(err)
	}

	show := d.find(t, d.shows, domain.KindShow, "Hinterland")
	episodes, err := d.db.EpisodeOrder(d.ctx, show)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 2 {
		t.Fatalf("episodes = %v", episodes)
	}
}
