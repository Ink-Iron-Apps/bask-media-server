package plexapi_test

import (
	"context"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// Markers are the one thing in this API a client acts on without being asked:
// Bask reads the Marker list and puts a Skip Intro button over the picture. It
// reads four attributes and drops any marker missing one of them, so the shape
// here is not cosmetic.
func TestMarkersAreServedInPlexsShape(t *testing.T) {
	srv, token, db := library(t)
	ctx := context.Background()

	episode := findEpisode(t, db)
	if err := db.ReplaceMarkers(ctx, episode.ID, domain.SourceChapter, []domain.Marker{
		{Type: domain.MarkerIntro, Start: 5 * time.Second, End: 95 * time.Second},
		{Type: domain.MarkerCredits, Start: 20 * time.Minute, End: 21 * time.Minute},
	}); err != nil {
		t.Fatal(err)
	}

	mc := get(t, srv, token, "/library/metadata/"+itoa(episode.ID))
	item := firstMetadata(t, mc)

	list, _ := item["Marker"].([]any)
	if len(list) != 2 {
		t.Fatalf("got %d markers, want 2: %v", len(list), item)
	}
	intro, _ := list[0].(map[string]any)
	if intro["type"] != "intro" {
		t.Errorf("first marker type = %v", intro["type"])
	}
	if intro["startTimeOffset"] != float64(5000) || intro["endTimeOffset"] != float64(95000) {
		t.Errorf("offsets are not milliseconds: %v", intro)
	}
	if id, ok := intro["id"].(float64); !ok || id == 0 {
		t.Errorf("a marker needs an id, got %v", intro["id"])
	}
}

// A client only asks for markers where it needs them. Serving them anyway
// would make every poster wall two more queries, which is the kind of cost
// that is invisible until a library is large.
func TestListsOnlyCarryMarkersWhenAsked(t *testing.T) {
	srv, token, db := library(t)
	ctx := context.Background()

	episode := findEpisode(t, db)
	if err := db.ReplaceMarkers(ctx, episode.ID, domain.SourceChapter, []domain.Marker{
		{Type: domain.MarkerIntro, Start: 0, End: 90 * time.Second},
	}); err != nil {
		t.Fatal(err)
	}
	season := *episode.ParentID

	quiet := firstMetadata(t, get(t, srv, token, "/library/metadata/"+itoa(season)+"/children"))
	if _, ok := quiet["Marker"]; ok {
		t.Errorf("an unasked-for list should carry no markers: %v", quiet)
	}

	asked := firstMetadata(t, get(t, srv, token, "/library/metadata/"+itoa(season)+"/children?includeMarkers=1"))
	if list, _ := asked["Marker"].([]any); len(list) != 1 {
		t.Errorf("includeMarkers=1 should carry them: %v", asked)
	}
}

// Both detectors can find the same kind of marker on one item. The file's own
// chapter is the one a person labelled, so it wins — and the guess has to
// disappear rather than sit beside it, or the client shows two buttons.
func TestALabelledChapterBeatsAGuess(t *testing.T) {
	srv, token, db := library(t)
	ctx := context.Background()

	episode := findEpisode(t, db)
	if err := db.ReplaceMarkers(ctx, episode.ID, domain.SourceAudio, []domain.Marker{
		{Type: domain.MarkerIntro, Start: 12 * time.Second, End: 100 * time.Second},
		{Type: domain.MarkerCredits, Start: 19 * time.Minute, End: 20 * time.Minute},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceMarkers(ctx, episode.ID, domain.SourceChapter, []domain.Marker{
		{Type: domain.MarkerIntro, Start: 0, End: 90 * time.Second},
	}); err != nil {
		t.Fatal(err)
	}

	item := firstMetadata(t, get(t, srv, token, "/library/metadata/"+itoa(episode.ID)))
	list, _ := item["Marker"].([]any)
	if len(list) != 2 {
		t.Fatalf("want the labelled intro and the measured credits, got %d: %v", len(list), list)
	}

	var intros int
	for _, raw := range list {
		m, _ := raw.(map[string]any)
		if m["type"] != "intro" {
			continue
		}
		intros++
		if m["startTimeOffset"] != float64(0) {
			t.Errorf("the surviving intro is the guess, not the label: %v", m)
		}
	}
	if intros != 1 {
		t.Errorf("got %d intro markers, want 1", intros)
	}
}

// Re-running a detector must not accumulate rows: a nightly pass over a
// library that has not changed should leave the database exactly as it was.
func TestRerunningADetectorIsIdempotent(t *testing.T) {
	srv, token, db := library(t)
	ctx := context.Background()

	episode := findEpisode(t, db)
	found := []domain.Marker{{Type: domain.MarkerIntro, Start: 0, End: 90 * time.Second}}
	for range 3 {
		if err := db.ReplaceMarkers(ctx, episode.ID, domain.SourceChapter, found); err != nil {
			t.Fatal(err)
		}
	}

	item := firstMetadata(t, get(t, srv, token, "/library/metadata/"+itoa(episode.ID)))
	if list, _ := item["Marker"].([]any); len(list) != 1 {
		t.Fatalf("got %d markers after three passes, want 1", len(list))
	}
}

// Chapters are a different answer to a different question — a client draws
// them on the timeline — so they are served separately and on their own flag.
func TestChaptersAreServedWhenAsked(t *testing.T) {
	srv, token, db := library(t)
	ctx := context.Background()

	episode := findEpisode(t, db)
	if err := db.ReplaceChapters(ctx, episode.ID, []domain.Chapter{
		{Index: 1, Title: "Intro", Start: 0, End: 90 * time.Second},
		{Index: 2, Title: "Chapter 2", Start: 90 * time.Second, End: 10 * time.Minute},
	}); err != nil {
		t.Fatal(err)
	}

	item := firstMetadata(t, get(t, srv, token, "/library/metadata/"+itoa(episode.ID)))
	list, _ := item["Chapter"].([]any)
	if len(list) != 2 {
		t.Fatalf("got %d chapters, want 2: %v", len(list), item)
	}
	first, _ := list[0].(map[string]any)
	if first["tag"] != "Intro" || first["endTimeOffset"] != float64(90000) {
		t.Errorf("first chapter = %v", first)
	}
}

// findEpisode returns an episode from the scanned fixture library, which is
// where markers belong: a show's intro is the case this feature exists for.
func findEpisode(t *testing.T, db *store.DB) domain.Item {
	t.Helper()
	ctx := context.Background()
	sections, err := db.Sections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range sections {
		if section.Kind != domain.SectionShows {
			continue
		}
		shows, _, err := db.SectionItems(ctx, section.ID, domain.KindShow, store.Page{})
		if err != nil {
			t.Fatal(err)
		}
		for _, show := range shows {
			seasons, _, err := db.Children(ctx, show.ID, store.Page{})
			if err != nil {
				t.Fatal(err)
			}
			for _, season := range seasons {
				episodes, _, err := db.Children(ctx, season.ID, store.Page{})
				if err != nil {
					t.Fatal(err)
				}
				if len(episodes) > 0 {
					return episodes[0]
				}
			}
		}
	}
	t.Fatal("the fixture library has no episodes")
	return domain.Item{}
}

// firstMetadata returns the first playable entry in a container, which is
// where an episode or a movie lands.
func firstMetadata(t *testing.T, mc map[string]any) map[string]any {
	t.Helper()
	list, _ := mc["Metadata"].([]any)
	if len(list) == 0 {
		t.Fatalf("container carries no Metadata: %v", mc)
	}
	item, _ := list[0].(map[string]any)
	return item
}
