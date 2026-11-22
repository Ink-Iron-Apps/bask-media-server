package store_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// logged is a server with two accounts and two films, so a test about the log
// points at rows that exist.
//
// The identifiers matter: history holds a reference to the account and the
// item, so that a row can still be followed to what it was about while that
// exists. A test that invented ids would be testing a table this one is not.
type logged struct {
	db    *store.DB
	riley int64
	guest int64
	hill  int64
	heat  int64
}

func newLogged(t *testing.T) *logged {
	t.Helper()
	db, ctx := open(t)

	riley, err := db.CreateUser(ctx, "Riley", true)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := db.CreateUser(ctx, "Guest", false)
	if err != nil {
		t.Fatal(err)
	}

	section, err := db.CreateSection(ctx, "Films", domain.SectionMovies, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	writer := db.NewWriter(4)
	for _, title := range []string{"Big Hill", "Heat"} {
		if err := writer.Add(ctx, store.Found{
			SectionID: section.ID, Kind: domain.KindMovie, Title: title, Year: 2026,
			Path: filepath.Join("/library", title+".mkv"), Container: "mkv", Size: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	items, _, err := db.SectionItems(ctx, section.ID, domain.KindMovie, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	out := &logged{db: db, riley: riley.ID, guest: guest.ID}
	for _, item := range items {
		switch item.Title {
		case "Big Hill":
			out.hill = item.ID
		case "Heat":
			out.heat = item.ID
		}
	}
	if out.hill == 0 || out.heat == 0 {
		t.Fatalf("the fixture's films are missing: %+v", items)
	}
	return out
}

// watched is one finished watch, written the way the recorder writes one.
func watched(t *testing.T, db *store.DB, in store.NewWatch, length time.Duration, paused time.Duration) int64 {
	t.Helper()
	id, err := db.StartWatch(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.TouchWatch(t.Context(), id, store.WatchProgress{
		At: in.Started.Add(length), Paused: paused, Position: length - paused, Done: true,
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// The arithmetic the whole log turns on: watched is the time the stream was
// open, less the time it sat paused — and it is not the position.
func TestWhatWasWatchedIsNotWhereTheyGotTo(t *testing.T) {
	fixture := newLogged(t)
	db, ctx := fixture.db, t.Context()
	started := time.Now().UTC().Add(-2 * time.Hour)

	id, err := db.StartWatch(ctx, store.NewWatch{
		UserID: fixture.riley, Name: "Riley", ItemID: fixture.hill, Title: "Big Hill",
		Surface: "jellyfin", Device: "Living Room TV", Mode: "direct",
		Started: started, Length: 95 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Somebody skipped to the last ten minutes and paused for five.
	if err := db.TouchWatch(ctx, id, store.WatchProgress{
		At: started.Add(20 * time.Minute), Paused: 5 * time.Minute,
		Position: 85 * time.Minute, Length: 95 * time.Minute, Done: true,
	}); err != nil {
		t.Fatal(err)
	}

	watch, err := db.Watch(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if watch.Watched() != 15*time.Minute {
		t.Errorf("watched = %s, want 15m", watch.Watched())
	}
	if watch.Position != 85*time.Minute {
		t.Errorf("position = %s, want 1h25m", watch.Position)
	}
	if !watch.Complete {
		t.Error("the server saw this stop, so it is complete")
	}
	if watch.Device != "Living Room TV" || watch.Mode != "direct" || watch.Surface != "jellyfin" {
		t.Errorf("the row lost what it was watched on: %+v", watch)
	}
}

// A row is closed when the stream ends. A client that reports once more
// afterwards must not reopen it, or a stream somebody stopped would go on
// growing for as long as its player kept asking.
func TestAClosedWatchIsNotReopened(t *testing.T) {
	db, ctx := open(t)
	started := time.Now().UTC().Add(-time.Hour)

	id, err := db.StartWatch(ctx, store.NewWatch{Name: "Riley", Title: "Heat", Started: started})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.TouchWatch(ctx, id, store.WatchProgress{At: started.Add(10 * time.Minute), Done: true}); err != nil {
		t.Fatal(err)
	}
	if err := db.TouchWatch(ctx, id, store.WatchProgress{At: started.Add(90 * time.Minute)}); err != nil {
		t.Fatal(err)
	}

	watch, err := db.Watch(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if watch.Watched() != 10*time.Minute {
		t.Errorf("a closed row grew to %s", watch.Watched())
	}
}

// The reason the names are in the row: a log that read them through the
// library would become unreadable exactly when it is most wanted.
func TestHistorySurvivesTheFilmAndTheAccount(t *testing.T) {
	db, ctx := open(t)
	section, err := db.CreateSection(ctx, "Films", domain.SectionMovies, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	writer := db.NewWriter(4)
	if err := writer.Add(ctx, store.Found{
		SectionID: section.ID, Kind: domain.KindMovie, Title: "Big Hill", Year: 2026,
		Path: filepath.Join("/library", "big-hill.mkv"), Container: "mkv", Size: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	items, _, err := db.SectionItems(ctx, section.ID, domain.KindMovie, store.Page{Size: 5})
	if err != nil {
		t.Fatal(err)
	}
	film := items[0]

	user, err := db.CreateUser(ctx, "guest", false)
	if err != nil {
		t.Fatal(err)
	}

	watched(t, db, store.NewWatch{
		UserID: user.ID, Name: user.Username, ItemID: film.ID, Title: film.Title,
		Started: time.Now().UTC().Add(-time.Hour),
	}, 30*time.Minute, 0)

	// The account goes, and the library loses the film.
	if err := db.DeleteUser(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PrunePaths(ctx, section.ID, map[string]struct{}{}); err != nil {
		t.Fatal(err)
	}

	watches, total, err := db.History(ctx, store.HistoryFilter{}, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(watches) != 1 {
		t.Fatalf("the watch should have outlived both: got %d", total)
	}
	if watches[0].Name != "guest" || watches[0].Title != "Big Hill" {
		t.Errorf("the row lost what it was about: %+v", watches[0])
	}
	if watches[0].UserID != 0 || watches[0].ItemID != 0 {
		t.Errorf("the identifiers should have given way: %+v", watches[0])
	}
	if watches[0].Watched() != 30*time.Minute {
		t.Errorf("watched = %s", watches[0].Watched())
	}
}

func TestHistoryIsFilteredAndPaged(t *testing.T) {
	fixture := newLogged(t)
	db, ctx := fixture.db, t.Context()
	now := time.Now().UTC()

	for i := range 5 {
		watched(t, db, store.NewWatch{
			UserID: fixture.riley, Name: "Riley", ItemID: fixture.hill, Title: "Big Hill",
			Started: now.Add(-time.Duration(i) * time.Hour),
		}, 10*time.Minute, 0)
	}
	watched(t, db, store.NewWatch{
		UserID: fixture.guest, Name: "Guest", ItemID: fixture.heat, Title: "Heat",
		Started: now.Add(-100 * 24 * time.Hour),
	}, 20*time.Minute, 0)

	all, total, err := db.History(ctx, store.HistoryFilter{}, store.Page{Size: 3})
	if err != nil {
		t.Fatal(err)
	}
	if total != 6 || len(all) != 3 {
		t.Fatalf("got %d of %d, want 3 of 6", len(all), total)
	}
	// Newest first, which is the only order a log is ever read in.
	if !all[0].Started.After(all[1].Started) {
		t.Errorf("out of order: %s then %s", all[0].Started, all[1].Started)
	}

	second, _, err := db.History(ctx, store.HistoryFilter{}, store.Page{Start: 3, Size: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 3 || second[0].ID == all[0].ID {
		t.Errorf("the second page repeated the first: %+v", second)
	}

	mine, total, err := db.History(ctx, store.HistoryFilter{UserID: fixture.riley}, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 || len(mine) != 5 {
		t.Errorf("one viewer's history is %d rows", total)
	}

	thing, total, err := db.History(ctx, store.HistoryFilter{ItemID: fixture.heat}, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || thing[0].Title != "Heat" {
		t.Errorf("one thing's history is %+v", thing)
	}

	recent, total, err := db.History(ctx, store.HistoryFilter{Since: now.AddDate(0, 0, -7)}, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 || len(recent) != 5 {
		t.Errorf("the last week is %d rows, want 5", total)
	}
}

// The two questions a page of rows cannot answer.
func TestTheTotalsAnswerWhoAndWhat(t *testing.T) {
	fixture := newLogged(t)
	db, ctx := fixture.db, t.Context()
	now := time.Now().UTC()

	watched(t, db, store.NewWatch{UserID: fixture.riley, Name: "Riley", ItemID: fixture.hill, Title: "Big Hill", Started: now.Add(-3 * time.Hour)}, time.Hour, 0)
	watched(t, db, store.NewWatch{UserID: fixture.riley, Name: "Riley", ItemID: fixture.heat, Title: "Heat", Started: now.Add(-2 * time.Hour)}, 30*time.Minute, 0)
	watched(t, db, store.NewWatch{UserID: fixture.guest, Name: "Guest", ItemID: fixture.hill, Title: "Big Hill", Started: now.Add(-time.Hour)}, 20*time.Minute, 0)

	viewers, err := db.WatchersSince(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(viewers) != 2 {
		t.Fatalf("two viewers, got %+v", viewers)
	}
	if viewers[0].Name != "Riley" || viewers[0].Plays != 2 || viewers[0].Watched != 90*time.Minute {
		t.Errorf("the busiest viewer is %+v", viewers[0])
	}
	if viewers[1].Name != "Guest" || viewers[1].Watched != 20*time.Minute {
		t.Errorf("the other viewer is %+v", viewers[1])
	}

	titles, err := db.TitlesSince(ctx, now.Add(-24*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(titles) != 2 {
		t.Fatalf("two titles, got %+v", titles)
	}
	if titles[0].Title != "Big Hill" || titles[0].Plays != 2 || titles[0].Watched != 80*time.Minute {
		t.Errorf("the most watched title is %+v", titles[0])
	}

	// A window that excludes everything totals nothing rather than failing.
	empty, err := db.WatchersSince(ctx, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Errorf("nothing was watched in the future: %+v", empty)
	}
}

// Getting rid of it is a feature, not an admission.
func TestForgettingHistory(t *testing.T) {
	db, ctx := open(t)
	now := time.Now().UTC()

	watched(t, db, store.NewWatch{Name: "Riley", Title: "Old", Started: now.AddDate(0, 0, -200)}, time.Hour, 0)
	watched(t, db, store.NewWatch{Name: "Riley", Title: "Recent", Started: now.Add(-time.Hour)}, time.Hour, 0)

	removed, err := db.ForgetHistory(ctx, now.AddDate(0, 0, -90))
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("one old row should have gone, %d did", removed)
	}
	left, total, err := db.History(ctx, store.HistoryFilter{}, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || left[0].Title != "Recent" {
		t.Fatalf("the wrong row went: %+v", left)
	}

	if removed, err = db.ForgetHistory(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	} else if removed != 1 {
		t.Fatalf("the rest should have gone, %d did", removed)
	}
	if _, total, err = db.History(ctx, store.HistoryFilter{}, store.Page{Size: 10}); err != nil {
		t.Fatal(err)
	} else if total != 0 {
		t.Errorf("%d rows survived forgetting everything", total)
	}
}

func TestAMissingWatchIsNotFound(t *testing.T) {
	db, ctx := open(t)
	if _, err := db.Watch(ctx, 404); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}
