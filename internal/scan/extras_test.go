package scan_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/scan"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// Both layouts the ecosystem uses, read the same way.
func TestAnExtraIsRecognisedByItsFolderOrItsSuffix(t *testing.T) {
	root := filepath.FromSlash("/library/Films")
	cases := []struct {
		path   string
		kind   domain.ExtraKind
		title  string
		parent string
	}{
		{"Big Hill (2026)/Trailers/Teaser.mkv", domain.ExtraTrailer, "Teaser", "Big Hill (2026)"},
		{"Big Hill (2026)/Behind The Scenes/Making Of.mkv", domain.ExtraBehindTheScenes, "Making Of", "Big Hill (2026)"},
		{"Big Hill (2026)/Deleted Scenes/The Ending.mkv", domain.ExtraDeleted, "The Ending", "Big Hill (2026)"},
		{"Big Hill (2026)/Featurettes/Stunts.mkv", domain.ExtraFeaturette, "Stunts", "Big Hill (2026)"},
		{"Big Hill (2026)/Interviews/The Director.mkv", domain.ExtraInterview, "The Director", "Big Hill (2026)"},
		{"Big Hill (2026)/extras/Odds and Ends.mkv", domain.ExtraOther, "Odds and Ends", "Big Hill (2026)"},
		// The suffix form, which is what Plex's own naming guide asks for.
		{"Big Hill (2026)/Big Hill (2026)-trailer.mkv", domain.ExtraTrailer, "Trailer", "Big Hill (2026)/Big Hill (2026).mkv"},
		{"Big Hill (2026)-behindthescenes.mkv", domain.ExtraBehindTheScenes, "Behind the Scenes", "Big Hill (2026).mkv"},
		{"Big Hill (2026)/Big Hill (2026)-deleted2.mkv", domain.ExtraDeleted, "Deleted Scene", "Big Hill (2026)/Big Hill (2026).mkv"},
	}
	for _, c := range cases {
		path := filepath.Join(root, filepath.FromSlash(c.path))
		extra, ok := scan.ParseExtra(root, path)
		if !ok {
			t.Errorf("%s was not read as an extra", c.path)
			continue
		}
		if extra.Kind != c.kind {
			t.Errorf("%s is a %q", c.path, extra.Kind)
		}
		if extra.Title != c.title {
			t.Errorf("%s is called %q", c.path, extra.Title)
		}
		if want := filepath.Join(root, filepath.FromSlash(c.parent)); extra.Parent != want {
			t.Errorf("%s belongs to %q, want %q", c.path, extra.Parent, want)
		}
	}
}

// An ordinary film is not an extra, however its title reads. "The Interview"
// is a film; a folder called "Extras" at the top of a library holds films
// somebody filed oddly, not extras of nothing.
func TestOrdinaryFilmsAreNotExtras(t *testing.T) {
	root := filepath.FromSlash("/library/Films")
	for _, name := range []string{
		"The Interview (2014)/The Interview (2014).mkv",
		"Scene (2019).mkv",
		"Trailer Park Boys (2006)/Trailer Park Boys (2006).mkv",
		"Extras/Big Hill (2026).mkv",
	} {
		if extra, ok := scan.ParseExtra(root, filepath.Join(root, filepath.FromSlash(name))); ok {
			t.Errorf("%s was read as a %q called %q", name, extra.Kind, extra.Title)
		}
	}
}

// The whole point, end to end: a library with a trailer in it has one film,
// not two, and the trailer is reachable as an extra of that film.
func TestATrailerIsAnExtraRatherThanAFilm(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	write := func(parts ...string) {
		t.Helper()
		path := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, 512), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Big Hill (2026)", "Big Hill (2026).mkv")
	write("Big Hill (2026)", "Trailers", "Teaser.mkv")
	write("Big Hill (2026)", "Deleted Scenes", "The Ending.mkv")

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bms.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	section, err := db.CreateSection(ctx, "Films", domain.SectionMovies, []string{root})
	if err != nil {
		t.Fatal(err)
	}

	scanner := &scan.Scanner{DB: db, Workers: 2}
	if _, err := scanner.Section(ctx, *section); err != nil {
		t.Fatal(err)
	}

	films, total, err := db.SectionItems(ctx, section.ID, domain.KindMovie, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(films) != 1 {
		t.Fatalf("a film with two extras scanned as %d films: %v", total, films)
	}
	film := films[0]
	if film.Title != "Big Hill" || film.Year != 2026 {
		t.Fatalf("the film is %q (%d)", film.Title, film.Year)
	}
	if len(film.Parts) != 1 {
		t.Fatalf("the film has %d files: an extra was filed as a version of it", len(film.Parts))
	}

	extras, err := db.Extras(ctx, film.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(extras) != 2 {
		t.Fatalf("extras = %v", extras)
	}
	// The trailer first, because that is what anybody is looking for.
	if extras[0].Extra != domain.ExtraTrailer || extras[0].Title != "Teaser" {
		t.Fatalf("first extra = %+v", extras[0])
	}
	if extras[1].Extra != domain.ExtraDeleted {
		t.Fatalf("second extra = %+v", extras[1])
	}
	// Playable, with a file behind it: an extra nobody can play is a row in
	// a list and nothing else.
	if !extras[0].Playable() || len(extras[0].Parts) != 1 {
		t.Fatalf("the trailer cannot be played: %+v", extras[0])
	}

	// And a rescan changes nothing, which is the property every scan rule
	// here has to have.
	if _, err := scanner.Section(ctx, *section); err != nil {
		t.Fatal(err)
	}
	if _, total, err := db.SectionItems(ctx, section.ID, domain.KindMovie, store.Page{Size: 10}); err != nil || total != 1 {
		t.Fatalf("a rescan made %d films: %v", total, err)
	}
	again, err := db.Extras(ctx, film.ID)
	if err != nil || len(again) != 2 {
		t.Fatalf("a rescan made %d extras: %v", len(again), err)
	}
}

// A series' extras hang off the series, and an extra named after an episode
// hangs off that episode.
func TestASeriesAndItsEpisodesBothHaveExtras(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	write := func(parts ...string) {
		t.Helper()
		path := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, 512), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Dinotopia (2002)", "Season 01", "S01E01 - Part 1.mkv")
	write("Dinotopia (2002)", "Season 01", "S01E01 - Part 1-deleted.mkv")
	write("Dinotopia (2002)", "Extras", "The Making Of.mkv")

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bms.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	section, err := db.CreateSection(ctx, "TV", domain.SectionShows, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	scanner := &scan.Scanner{DB: db, Workers: 1}
	if _, err := scanner.Section(ctx, *section); err != nil {
		t.Fatal(err)
	}

	shows, _, err := db.SectionItems(ctx, section.ID, domain.KindShow, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(shows) != 1 {
		t.Fatalf("shows = %v", shows)
	}
	seriesExtras, err := db.Extras(ctx, shows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(seriesExtras) != 1 || seriesExtras[0].Title != "The Making Of" {
		t.Fatalf("the series' extras = %v", seriesExtras)
	}

	// The season's children are its episodes and nothing else: a deleted
	// scene is not an episode, and a client asking for episodes must not be
	// handed one.
	seasons, _, err := db.Children(ctx, shows[0].ID, store.Page{Size: 10})
	if err != nil || len(seasons) != 1 {
		t.Fatalf("seasons = %v, %v", seasons, err)
	}
	episodes, total, err := db.Children(ctx, seasons[0].ID, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(episodes) != 1 {
		t.Fatalf("the season has %d children: %v", total, episodes)
	}
	episodeExtras, err := db.Extras(ctx, episodes[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodeExtras) != 1 || episodeExtras[0].Extra != domain.ExtraDeleted {
		t.Fatalf("the episode's extras = %v", episodeExtras)
	}
}
