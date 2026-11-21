package plexapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/plexapi"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// narrow restricts the token the harness issued to one library, which is what
// the admin interface does when somebody ticks boxes under a device.
func narrow(t *testing.T, db *store.DB, title string) {
	t.Helper()
	ctx := context.Background()

	sections, err := db.Sections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var wanted int64
	for _, section := range sections {
		if section.Title == title {
			wanted = section.ID
		}
	}
	if wanted == 0 {
		t.Fatalf("no library called %q", title)
	}

	users, err := db.Users(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := db.Tokens(ctx, users[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetTokenSections(ctx, tokens[0].ID, []int64{wanted}); err != nil {
		t.Fatal(err)
	}
}

func status(t *testing.T, srv *plexapi.Server, token, path string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("X-Plex-Token", token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec.Code
}

// The Plex surface carries the same restriction as the Jellyfin one, because
// it is the same library and the same credential.
func TestAPlexClientSeesOnlyTheLibrariesItsTokenWasGiven(t *testing.T) {
	srv, token, db := library(t)

	before := get(t, srv, token, "/library/sections")
	if n, _ := before["size"].(float64); int(n) != 2 {
		t.Fatalf("expected two libraries first, got %v", before["size"])
	}

	narrow(t, db, "Movies")

	after := get(t, srv, token, "/library/sections")
	if n, _ := after["size"].(float64); int(n) != 1 {
		t.Fatalf("expected one library, got %v", after["size"])
	}
	directories, _ := after["Directory"].([]any)
	if len(directories) != 1 {
		t.Fatalf("expected one directory, got %d", len(directories))
	}
	row, _ := directories[0].(map[string]any)
	if row["title"] != "Movies" {
		t.Fatalf("expected Movies, got %v", row["title"])
	}
}

// A Plex client streams a file by its part id, which never passes through the
// item it belongs to. That makes it the one route where hiding a library from
// a list would otherwise leave the files behind it playable.
func TestAPartInAHiddenLibraryWillNotStream(t *testing.T) {
	srv, token, db := library(t)
	ctx := context.Background()

	sections, err := db.Sections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var movies int64
	for _, section := range sections {
		if section.Title == "Movies" {
			movies = section.ID
		}
	}
	films, _, err := db.SectionItems(ctx, movies, "movie", store.Page{Size: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(films) == 0 || len(films[0].Parts) == 0 {
		t.Fatal("the fixture has no film with a file, so this proves nothing")
	}
	part := films[0].Parts[0].ID

	// Reachable while the library is visible…
	if code := status(t, srv, token, partPath(part)); code != http.StatusOK {
		t.Fatalf("the part should stream before narrowing: %d", code)
	}

	narrow(t, db, "TV Shows")

	// …and gone once it is not, rather than merely absent from a listing.
	if code := status(t, srv, token, partPath(part)); code != http.StatusNotFound {
		t.Fatalf("a part in a hidden library still streamed: %d", code)
	}
}

func partPath(id int64) string {
	return "/library/parts/" + strconv.FormatInt(id, 10)
}
