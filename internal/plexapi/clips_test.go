package plexapi_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/plexapi"
	"github.com/inknironapps/bask-media-server/internal/scan"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// withExtras is a library of one film with a trailer and a featurette in it,
// scanned the way a real one would be.
//
// Its own library rather than the shared one, because the shared one's counts
// are what several other tests assert on and an extra added to it would be a
// change to all of them.
func withExtras(t *testing.T) (*plexapi.Server, string, int64) {
	t.Helper()
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
	write("Arrival (2016)", "Arrival (2016).mkv")
	write("Arrival (2016)", "Trailers", "Teaser.mkv")
	write("Arrival (2016)", "Featurettes", "The Language.mkv")

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bms.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	section, err := db.CreateSection(ctx, "Movies", domain.SectionMovies, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	scanner := &scan.Scanner{DB: db, Workers: 2}
	if _, err := scanner.Section(ctx, *section); err != nil {
		t.Fatal(err)
	}
	films, _, err := db.SectionItems(ctx, section.ID, domain.KindMovie, store.Page{Size: 10})
	if err != nil || len(films) != 1 {
		t.Fatalf("expected one film, got %v (%v)", films, err)
	}

	user, err := db.CreateUser(ctx, "owner", true)
	if err != nil {
		t.Fatal(err)
	}
	token, err := db.IssueToken(ctx, user.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	return &plexapi.Server{Store: db, Name: "BMS test", MachineID: "machine-1", Version: "test"}, token, films[0].ID
}

// A Plex client asks for a film's extras and gets clips, each saying which
// sort it is: the client draws the trailer behind the play button off that
// subtype.
func TestExtrasComeBackAsClips(t *testing.T) {
	server, token, film := withExtras(t)

	out := get(t, server, token, "/library/metadata/"+itoa(film)+"/extras")
	rows, _ := out["Metadata"].([]any)
	if len(rows) != 2 {
		t.Fatalf("extras = %v", rows)
	}

	first, _ := rows[0].(map[string]any)
	if first["type"] != "clip" {
		t.Errorf("type = %v", first["type"])
	}
	if first["subtype"] != "trailer" {
		t.Errorf("the trailer is a %v", first["subtype"])
	}
	if first["title"] != "Teaser" {
		t.Errorf("title = %v", first["title"])
	}
	// Playable, which means a Media and a Part: an extra a client cannot
	// play is a row and nothing else.
	media, _ := first["Media"].([]any)
	if len(media) != 1 {
		t.Fatalf("the trailer has no file: %v", first)
	}

	second, _ := rows[1].(map[string]any)
	if second["subtype"] != "featurette" {
		t.Errorf("the featurette is a %v", second["subtype"])
	}
}

// And the library itself still holds one film: the whole reason for this is
// that a trailer was becoming a row in the films list.
func TestExtrasAreNotInTheLibrary(t *testing.T) {
	server, token, _ := withExtras(t)

	out := get(t, server, token, "/library/sections/1/all")
	rows, _ := out["Metadata"].([]any)
	if len(rows) != 1 {
		t.Fatalf("the library lists %d films: %v", len(rows), rows)
	}
	film, _ := rows[0].(map[string]any)
	if film["title"] != "Arrival" {
		t.Fatalf("film = %v", film)
	}
	// One file behind it, not three: an extra must not arrive as another
	// version of the film.
	media, _ := film["Media"].([]any)
	if len(media) != 1 {
		t.Fatalf("the film has %d versions", len(media))
	}
}
