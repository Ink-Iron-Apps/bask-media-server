package plexapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
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

// post is create, which Plex spells as the same path it lists with.
func post(t *testing.T, srv *plexapi.Server, token, path string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s: %d %s", path, rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("POST %s: %v (%s)", path, err, rec.Body.String())
	}
	mc, ok := body["MediaContainer"].(map[string]any)
	if !ok {
		t.Fatalf("POST %s: no MediaContainer in %s", path, rec.Body.String())
	}
	return mc
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

// Plex has no favourites, only a ten point rating, and its clients send a
// ten when somebody presses the heart. This is where one becomes the other.
func TestTheHeartBecomesAFavourite(t *testing.T) {
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
	if err != nil || len(films) == 0 {
		t.Fatalf("films = %v (%v)", films, err)
	}
	film := films[0].ID

	users, err := db.Users(ctx)
	if err != nil {
		t.Fatal(err)
	}

	rate := func(rating string) int {
		return status(t, srv, token,
			"/:/rate?key="+itoa(film)+"&rating="+rating+"&identifier=com.plexapp.plugins.library")
	}

	if code := rate("10"); code != http.StatusOK {
		t.Fatalf("the heart = %d", code)
	}
	marked, _, err := db.Favourites(ctx, users[0].ID, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(marked) != 1 || marked[0].ID != film {
		t.Fatalf("after the heart, favourites = %v", marked)
	}

	// And clearing it takes the favourite away.
	if code := rate("0"); code != http.StatusOK {
		t.Fatalf("clearing = %d", code)
	}
	marked, _, err = db.Favourites(ctx, users[0].ID, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(marked) != 0 {
		t.Fatalf("clearing left %v", marked)
	}
}

func TestAPlexPlaylistIsMadeAndRead(t *testing.T) {
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
	if err != nil || len(films) == 0 {
		t.Fatal("no films in the fixture")
	}

	// Plex passes what goes in as a URI whose last segment is the list, and
	// creates with a POST to the same path it lists with.
	made := post(t, srv, token,
		"/playlists?title=Tonight&type=video&uri=server://x/com.plexapp.plugins.library/library/metadata/"+itoa(films[0].ID))
	rows, _ := made["Metadata"].([]any)
	if len(rows) != 1 {
		t.Fatalf("create returned %v", made)
	}
	row, _ := rows[0].(map[string]any)
	id, _ := row["ratingKey"].(string)
	if id == "" {
		t.Fatalf("no ratingKey on %v", row)
	}

	listed := get(t, srv, token, "/playlists")
	if n, _ := listed["size"].(float64); int(n) != 1 {
		t.Fatalf("playlists = %v", listed)
	}

	inside := get(t, srv, token, "/playlists/"+id+"/items")
	items, _ := inside["Metadata"].([]any)
	if len(items) != 1 {
		t.Fatalf("playlist items = %v", inside)
	}
	entry, _ := items[0].(map[string]any)
	if entry["playlistItemID"] == nil || entry["playlistItemID"] == "" {
		t.Fatalf("no playlistItemID on %v", entry)
	}
}

// A collection appears under the library its items are in, and a library
// holding none of them is not offered it.
func TestAPlexCollectionAppearsUnderItsLibrary(t *testing.T) {
	srv, token, db := library(t)
	ctx := context.Background()

	sections, err := db.Sections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var movies, shows int64
	for _, section := range sections {
		switch section.Title {
		case "Movies":
			movies = section.ID
		case "TV Shows":
			shows = section.ID
		}
	}
	films, _, err := db.SectionItems(ctx, movies, "movie", store.Page{Size: 5})
	if err != nil || len(films) == 0 {
		t.Fatal("no films in the fixture")
	}

	made, err := db.CreateCollection(ctx, "Science fiction", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetCollectionItems(ctx, made.ID, []int64{films[0].ID}); err != nil {
		t.Fatal(err)
	}

	under := get(t, srv, token, "/library/sections/"+itoa(movies)+"/collections")
	if n, _ := under["size"].(float64); int(n) != 1 {
		t.Fatalf("under the films library: %v", under)
	}

	// The shows library holds none of it, so it is not offered there.
	elsewhere := get(t, srv, token, "/library/sections/"+itoa(shows)+"/collections")
	if n, _ := elsewhere["size"].(float64); int(n) != 0 {
		t.Fatalf("offered under a library holding none of it: %v", elsewhere)
	}

	inside := get(t, srv, token, "/library/collections/"+itoa(made.ID)+"/children")
	items, _ := inside["Metadata"].([]any)
	if len(items) != 1 {
		t.Fatalf("inside the collection: %v", inside)
	}
}

// A collection filled by rules says so, because Plex clients read that field
// to decide whether they may rearrange it — and this server would refuse the
// rearrangement.
func TestAPlexCollectionSaysWhetherItFillsItself(t *testing.T) {
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

	made, err := db.CreateCollection(ctx, "Every film", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetCollectionRules(ctx, made.ID, &domain.Smart{
		Rules: []domain.SmartRule{{Field: domain.SmartKind, Op: domain.SmartIs, Value: string(domain.KindMovie)}},
	}); err != nil {
		t.Fatal(err)
	}

	under := get(t, srv, token, "/library/sections/"+itoa(movies)+"/collections")
	rows, _ := under["Directory"].([]any)
	if len(rows) != 1 {
		t.Fatalf("collections under the films library: %v", under)
	}
	row, _ := rows[0].(map[string]any)
	if row["smart"] != true {
		t.Fatalf("a rule-driven collection reported smart = %v", row["smart"])
	}
	if n, _ := row["childCount"].(float64); int(n) == 0 {
		t.Fatalf("it holds nothing: %v", row)
	}

	inside := get(t, srv, token, "/library/collections/"+itoa(made.ID)+"/children")
	if items, _ := inside["Metadata"].([]any); len(items) == 0 {
		t.Fatalf("inside the collection: %v", inside)
	}
}
