package scan_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/ffprobe"
	"github.com/inknironapps/bask-media-server/internal/scan"
	"github.com/inknironapps/bask-media-server/internal/store"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("modernc.org/sqlite.(*conn).interruptOnDone.func1"),
	)
}

// prober stands in for ffprobe: the scanner's contract with it is one call per
// file that reports a duration and whatever chapters the file declares.
type prober struct {
	info  ffprobe.Info
	calls int
}

func (p *prober) Probe(context.Context, string) (ffprobe.Info, error) {
	p.calls++
	return p.info, nil
}

// A labelled file is the cheapest Skip Intro there is: no decoding, no second
// pass, and the answer is stored once rather than worked out per playback.
func TestAScanTurnsLabelledChaptersIntoMarkers(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	episode := filepath.Join(root, "Dinotopia (2002)", "Season 01", "S01E01 - Part 1.mkv")
	if err := os.MkdirAll(filepath.Dir(episode), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(episode, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bms.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	section, err := db.CreateSection(ctx, "TV Shows", domain.SectionShows, []string{root})
	if err != nil {
		t.Fatal(err)
	}

	p := &prober{info: ffprobe.Info{
		Duration: 22 * time.Minute,
		Chapters: []domain.Chapter{
			{Index: 1, Title: "Intro", Start: 0, End: 85 * time.Second},
			{Index: 2, Title: "Chapter 2", Start: 85 * time.Second, End: 21 * time.Minute},
			{Index: 3, Title: "End Credits", Start: 21 * time.Minute, End: 22 * time.Minute},
		},
	}}
	scanner := &scan.Scanner{DB: db, Prober: p, Workers: 2}
	if _, err := scanner.Section(ctx, *section); err != nil {
		t.Fatal(err)
	}

	item := onlyEpisode(t, db, section.ID)
	if item.Duration != 22*time.Minute {
		t.Errorf("duration = %v", item.Duration)
	}

	found, err := db.Markers(ctx, []int64{item.ID})
	if err != nil {
		t.Fatal(err)
	}
	list := found[item.ID]
	if len(list) != 2 {
		t.Fatalf("got %d markers, want an intro and a credits: %+v", len(list), list)
	}
	if list[0].Type != domain.MarkerIntro || list[0].End != 85*time.Second {
		t.Errorf("intro = %+v", list[0])
	}
	if list[1].Type != domain.MarkerCredits || list[1].Start != 21*time.Minute {
		t.Errorf("credits = %+v", list[1])
	}
	if list[0].Source != domain.SourceChapter {
		t.Errorf("markers from a title should say so: %+v", list[0])
	}

	chapters, err := db.Chapters(ctx, []int64{item.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(chapters[item.ID]) != 3 {
		t.Errorf("every chapter is kept for the timeline, got %d", len(chapters[item.ID]))
	}

	// A rescan of an unchanged library must leave one set of each, not two.
	if _, err := scanner.Section(ctx, *section); err != nil {
		t.Fatal(err)
	}
	found, err = db.Markers(ctx, []int64{item.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(found[item.ID]) != 2 {
		t.Errorf("a rescan should not accumulate markers, got %d", len(found[item.ID]))
	}
}

// A machine with no ffprobe still has a browsable, playable library — and a
// scan without one must not delete what an earlier scan with one recorded.
func TestAScanWithoutAProbeKeepsWhatWasAlreadyKnown(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	movie := filepath.Join(root, "Arrival (2016)", "Arrival (2016).mkv")
	if err := os.MkdirAll(filepath.Dir(movie), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(movie, make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bms.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	section, err := db.CreateSection(ctx, "Movies", domain.SectionMovies, []string{root})
	if err != nil {
		t.Fatal(err)
	}

	withProbe := &scan.Scanner{DB: db, Workers: 2, Prober: &prober{info: ffprobe.Info{
		Duration: 116 * time.Minute,
		Chapters: []domain.Chapter{{Index: 1, Title: "Credits", Start: 114 * time.Minute, End: 116 * time.Minute}},
	}}}
	if _, err := withProbe.Section(ctx, *section); err != nil {
		t.Fatal(err)
	}

	items, _, err := db.SectionItems(ctx, section.ID, domain.KindMovie, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d movies", len(items))
	}

	bare := &scan.Scanner{DB: db, Workers: 2}
	if _, err := bare.Section(ctx, *section); err != nil {
		t.Fatal(err)
	}

	found, err := db.Markers(ctx, []int64{items[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(found[items[0].ID]) != 1 {
		t.Errorf("a probe-less rescan erased what was known, got %+v", found[items[0].ID])
	}
}

func onlyEpisode(t *testing.T, db *store.DB, sectionID int64) domain.Item {
	t.Helper()
	ctx := context.Background()
	shows, _, err := db.SectionItems(ctx, sectionID, domain.KindShow, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(shows) != 1 {
		t.Fatalf("got %d shows", len(shows))
	}
	seasons, _, err := db.Children(ctx, shows[0].ID, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(seasons) != 1 {
		t.Fatalf("got %d seasons", len(seasons))
	}
	episodes, _, err := db.Children(ctx, seasons[0].ID, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 1 {
		t.Fatalf("got %d episodes", len(episodes))
	}
	return episodes[0]
}
