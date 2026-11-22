package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// scanned puts one film in one library the way a scan does, and reports the
// section and the item.
func scanned(t *testing.T, db *store.DB, ctx context.Context, title string, year int) (sectionID, itemID int64) {
	t.Helper()
	section, err := db.CreateSection(ctx, "Films", domain.SectionMovies, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := rescan(t, db, ctx, section.ID, title, year); err != nil {
		t.Fatal(err)
	}
	items, _, err := db.SectionItems(ctx, section.ID, domain.KindMovie, store.Page{Size: 10})
	if err != nil || len(items) != 1 {
		t.Fatalf("seeding failed: %v %v", items, err)
	}
	return section.ID, items[0].ID
}

// rescan is the scanner finding the same file again, which is the thing an
// edit must survive.
func rescan(t *testing.T, db *store.DB, ctx context.Context, sectionID int64, title string, year int) error {
	t.Helper()
	writer := db.NewWriter(1)
	if err := writer.Add(ctx, store.Found{
		SectionID: sectionID, Kind: domain.KindMovie, Title: title, Year: year,
		Path: filepath.Join("/library", title+".mkv"), Container: "mkv", Size: 1,
	}); err != nil {
		return err
	}
	return writer.Commit(ctx)
}

// The whole reason the scanned title is kept separately: a title corrected by
// hand has to survive the next scan.
//
// Without it the scan would look for the item by its displayed title, fail to
// find the row it had written, insert a second one beside it, move the file to
// the new row and leave the correction attached to nothing.
func TestACorrectedTitleSurvivesARescan(t *testing.T) {
	db, ctx := open(t)
	section, id := scanned(t, db, ctx, "blue.2026", 2026)

	corrected := "Blue"
	if err := db.EditItem(ctx, id, store.Edit{Title: &corrected}); err != nil {
		t.Fatal(err)
	}

	// The scanner finds the same file, named the same way it always was.
	if err := rescan(t, db, ctx, section, "blue.2026", 2026); err != nil {
		t.Fatal(err)
	}

	items, total, err := db.SectionItems(ctx, section, domain.KindMovie, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("the rescan made %d films out of one: %v", total, items)
	}
	if items[0].ID != id {
		t.Fatalf("the rescan replaced the item: %d became %d", id, items[0].ID)
	}
	if items[0].Title != "Blue" {
		t.Fatalf("the correction was undone: %q", items[0].Title)
	}
	if len(items[0].Parts) != 1 {
		t.Fatalf("the file is no longer attached: %v", items[0].Parts)
	}
}

// An edit also fixes where the item files, because a film renamed by hand
// should sort where somebody would look for it.
func TestAnEditRecomputesTheSortTitle(t *testing.T) {
	db, ctx := open(t)
	_, id := scanned(t, db, ctx, "italian.job", 1969)

	corrected := "The Italian Job"
	if err := db.EditItem(ctx, id, store.Edit{Title: &corrected}); err != nil {
		t.Fatal(err)
	}
	item, err := db.Item(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if item.SortTitle != "italian job" {
		t.Fatalf("sort title = %q", item.SortTitle)
	}
}

// Editing locks the item, and a locked item is left alone by the enrichment
// pass: a provider that overwrites a correction every night is worse than no
// provider.
func TestACorrectedItemIsLeftAloneByEnrichment(t *testing.T) {
	db, ctx := open(t)
	_, id := scanned(t, db, ctx, "blue.2026", 2026)

	waiting, err := db.NeedsMetadata(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting) != 1 {
		t.Fatalf("an unmatched film is not waiting for metadata: %v", waiting)
	}

	corrected := "Blue"
	if err := db.EditItem(ctx, id, store.Edit{Title: &corrected}); err != nil {
		t.Fatal(err)
	}
	waiting, err = db.NeedsMetadata(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting) != 0 {
		t.Fatalf("a corrected film is still offered to the provider: %v", waiting)
	}

	// And unlocking puts it back in the queue, which is what makes the lock a
	// decision rather than a one-way door.
	unlocked := false
	if err := db.EditItem(ctx, id, store.Edit{Locked: &unlocked}); err != nil {
		t.Fatal(err)
	}
	waiting, err = db.NeedsMetadata(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting) != 1 {
		t.Fatalf("unlocking did not put it back: %v", waiting)
	}
}

// Reverting puts the filename's own title back, which is the escape hatch
// from a correction that was itself wrong.
func TestTheScannedTitleIsStillThereToRevertTo(t *testing.T) {
	db, ctx := open(t)
	_, id := scanned(t, db, ctx, "blue.2026", 2026)

	corrected, wrongYear := "Bleu", 1999
	if err := db.EditItem(ctx, id, store.Edit{Title: &corrected, Year: &wrongYear}); err != nil {
		t.Fatal(err)
	}

	title, year, err := db.ScannedTitle(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if title != "blue.2026" || year != 2026 {
		t.Fatalf("the scanned title was not kept: %q %d", title, year)
	}
}

// An empty title is refused: a row with no name is unreachable from every
// interface that lists it.
func TestAnEmptyTitleIsRefused(t *testing.T) {
	db, ctx := open(t)
	_, id := scanned(t, db, ctx, "blue.2026", 2026)

	empty := "   "
	if err := db.EditItem(ctx, id, store.Edit{Title: &empty}); err == nil {
		t.Fatal("an item was renamed to nothing")
	}
	item, err := db.Item(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if item.Title != "blue.2026" {
		t.Fatalf("the refused edit was partly applied: %q", item.Title)
	}
}

// Rematching clears what the old match left: a wrong match that is half
// replaced is worse than one that is obviously empty.
func TestRematchingForgetsTheOldMatch(t *testing.T) {
	db, ctx := open(t)
	_, id := scanned(t, db, ctx, "blue.2026", 2026)

	if err := db.SaveMetadata(ctx, id, "111", "The wrong film entirely.", "poster-a", "art-a"); err != nil {
		t.Fatal(err)
	}
	if err := db.Rematch(ctx, id, "222"); err != nil {
		t.Fatal(err)
	}

	item, err := db.Item(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if item.ProviderID != "222" {
		t.Fatalf("provider id = %q", item.ProviderID)
	}
	if item.Summary != "" || item.PosterKey != "" || item.ArtKey != "" {
		t.Fatalf("the old match is still partly there: %+v", item)
	}
	// Unmatched again, so an interrupted correction is picked up by the next
	// ordinary pass rather than lost.
	waiting, err := db.NeedsMetadata(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting) != 1 || waiting[0].ID != id {
		t.Fatalf("a rematched item is not waiting for its new metadata: %v", waiting)
	}
}

// A show matched to the wrong series had every episode matched to the wrong
// series, because an episode is addressed through its show's provider id.
// Correcting the show has to unmatch them.
func TestCorrectingAShowUnmatchesItsEpisodes(t *testing.T) {
	db, ctx := open(t)
	section, err := db.CreateSection(ctx, "TV", domain.SectionShows, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	writer := db.NewWriter(1)
	if err := writer.Add(ctx, store.Found{
		SectionID: section.ID, Kind: domain.KindEpisode,
		Show: "Wrong Show", SeasonNum: 1, EpisodeNum: 1, Title: "Pilot",
		Path: filepath.Join("/library", "s01e01.mkv"), Container: "mkv", Size: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	shows, _, err := db.SectionItems(ctx, section.ID, domain.KindShow, store.Page{Size: 10})
	if err != nil || len(shows) != 1 {
		t.Fatalf("seeding failed: %v %v", shows, err)
	}
	show := shows[0]

	// Everything matched to the wrong series.
	waiting, err := db.NeedsMetadata(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range waiting {
		if err := db.SaveMetadata(ctx, item.ID, "999", "The wrong series.", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if left, err := db.NeedsMetadata(ctx, 10); err != nil || len(left) != 0 {
		t.Fatalf("not everything matched: %v %v", left, err)
	}

	if err := db.Rematch(ctx, show.ID, "123"); err != nil {
		t.Fatal(err)
	}
	if err := db.ForgetDescendants(ctx, show.ID); err != nil {
		t.Fatal(err)
	}

	again, err := db.NeedsMetadata(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	// The show, its season and its episode: all three have to be asked
	// about again, in that order.
	if len(again) != 3 {
		t.Fatalf("expected the show, the season and the episode, got %v", again)
	}
	for _, item := range again {
		if item.Kind != domain.KindShow && item.ProviderID != "" {
			t.Fatalf("%s still points at the old series: %q", item.Kind, item.ProviderID)
		}
	}
}
