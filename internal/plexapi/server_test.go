package plexapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"go.uber.org/goleak"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/plexapi"
	"github.com/inknironapps/bask-media-server/internal/scan"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// A leaked goroutine is the memory leak this project is most likely to grow,
// so every package's tests fail on one rather than leaving it to be noticed on
// a machine nobody is watching. See docs/RESOURCE-BUDGET.md.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// The SQLite driver keeps a package-level worker alive for the life of
		// the process once a database has been opened.
		goleak.IgnoreTopFunction("modernc.org/sqlite.(*conn).interruptOnDone.func1"),
	)
}

// library stands up a real database over a real directory tree, scans it, and
// returns a server plus a working token. Real files because the scanner's job
// is reading a filesystem, and faking one would test the fake.
func library(t *testing.T) (*plexapi.Server, string, *store.DB) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()

	movies := filepath.Join(root, "Movies")
	shows := filepath.Join(root, "TV Shows")
	write := func(path string, size int) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(movies, "Arrival (2016)", "Arrival (2016).mkv"), 2048)
	write(filepath.Join(movies, "The Martian (2015)", "The Martian (2015).mp4"), 4096)
	write(filepath.Join(shows, "Dinotopia (2002)", "Season 01", "S01E01 - Part 1.mkv"), 1024)
	write(filepath.Join(shows, "Dinotopia (2002)", "Season 01", "S01E02 - Part 2.mkv"), 1024)
	// Not media: must not become an item.
	write(filepath.Join(movies, "Arrival (2016)", "Arrival (2016).srt"), 10)

	db, err := store.Open(ctx, filepath.Join(root, "bms.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	movieSection, err := db.CreateSection(ctx, "Movies", domain.SectionMovies, []string{movies})
	if err != nil {
		t.Fatal(err)
	}
	showSection, err := db.CreateSection(ctx, "TV Shows", domain.SectionShows, []string{shows})
	if err != nil {
		t.Fatal(err)
	}

	scanner := &scan.Scanner{DB: db, Workers: 2}
	for _, section := range []*domain.Section{movieSection, showSection} {
		if _, err := scanner.Section(ctx, *section); err != nil {
			t.Fatalf("scan %s: %v", section.Title, err)
		}
	}

	user, err := db.CreateUser(ctx, "owner", true)
	if err != nil {
		t.Fatal(err)
	}
	token, err := db.IssueToken(ctx, user.ID, "test")
	if err != nil {
		t.Fatal(err)
	}

	return &plexapi.Server{Store: db, Name: "BMS test", MachineID: "machine-1", Version: "test"}, token, db
}

func get(t *testing.T, srv *plexapi.Server, token, path string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("X-Plex-Token", token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d, body %s", path, rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET %s: %v, body %s", path, err, rec.Body.String())
	}
	mc, ok := body["MediaContainer"].(map[string]any)
	if !ok {
		t.Fatalf("GET %s: no MediaContainer in %s", path, rec.Body.String())
	}
	return mc
}

func TestIdentityNeedsNoToken(t *testing.T) {
	// Anchor: a client asks who a server is before it has any credential, so
	// this one route cannot be behind the token check.
	srv, _, _ := library(t)

	mc := get(t, srv, "", "/identity")

	if mc["machineIdentifier"] != "machine-1" {
		t.Fatalf("machineIdentifier = %v", mc["machineIdentifier"])
	}
}

func TestEverythingElseNeedsAToken(t *testing.T) {
	srv, _, _ := library(t)

	req := httptest.NewRequest(http.MethodGet, "/library/sections", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestSectionsAreListedWithTheirPaths(t *testing.T) {
	srv, token, _ := library(t)

	mc := get(t, srv, token, "/library/sections")

	dirs, _ := mc["Directory"].([]any)
	if len(dirs) != 2 {
		t.Fatalf("got %d sections, want 2", len(dirs))
	}
	first, _ := dirs[0].(map[string]any)
	if first["title"] != "Movies" || first["type"] != "movie" {
		t.Fatalf("first section = %v", first)
	}
	if loc, _ := first["Location"].([]any); len(loc) != 1 {
		t.Fatalf("section has no location: %v", first)
	}
}

func TestMoviesComeBackPlayable(t *testing.T) {
	srv, token, _ := library(t)

	mc := get(t, srv, token, "/library/sections/1/all")

	items, _ := mc["Metadata"].([]any)
	if len(items) != 2 {
		t.Fatalf("got %d movies, want 2 (a subtitle file must not become one)", len(items))
	}
	// Sorted by sort title, which drops a leading article: The Martian files
	// under M, so Arrival comes first.
	first, _ := items[0].(map[string]any)
	second, _ := items[1].(map[string]any)
	if first["title"] != "Arrival" || second["title"] != "The Martian" {
		t.Fatalf("order = %v, %v", first["title"], second["title"])
	}
	if first["year"].(float64) != 2016 {
		t.Fatalf("year = %v", first["year"])
	}

	media, _ := first["Media"].([]any)
	if len(media) != 1 {
		t.Fatalf("movie has no media: %v", first)
	}
	parts, _ := media[0].(map[string]any)["Part"].([]any)
	if len(parts) != 1 {
		t.Fatalf("media has no part: %v", media[0])
	}
	key, _ := parts[0].(map[string]any)["key"].(string)
	if key == "" {
		t.Fatalf("part has no key: %v", parts[0])
	}
}

func TestShowsDescendToEpisodes(t *testing.T) {
	srv, token, _ := library(t)

	shows := get(t, srv, token, "/library/sections/2/all")
	dirs, _ := shows["Directory"].([]any)
	if len(dirs) != 1 {
		t.Fatalf("got %d shows, want 1", len(dirs))
	}
	show, _ := dirs[0].(map[string]any)
	if show["title"] != "Dinotopia" || show["year"].(float64) != 2002 {
		t.Fatalf("show = %v", show)
	}

	seasons := get(t, srv, token, "/library/metadata/"+show["ratingKey"].(string)+"/children")
	seasonDirs, _ := seasons["Directory"].([]any)
	if len(seasonDirs) != 1 {
		t.Fatalf("got %d seasons, want 1", len(seasonDirs))
	}
	season, _ := seasonDirs[0].(map[string]any)

	episodes := get(t, srv, token, "/library/metadata/"+season["ratingKey"].(string)+"/children")
	eps, _ := episodes["Metadata"].([]any)
	if len(eps) != 2 {
		t.Fatalf("got %d episodes, want 2", len(eps))
	}
	firstEp, _ := eps[0].(map[string]any)
	if firstEp["title"] != "Part 1" || firstEp["index"].(float64) != 1 {
		t.Fatalf("first episode = %v", firstEp)
	}
}

func TestStreamingServesRangesFromTheFile(t *testing.T) {
	srv, token, db := library(t)
	ctx := context.Background()

	items, _, err := db.SectionItems(ctx, 1, domain.KindMovie, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	var partID int64
	for _, it := range items {
		if len(it.Parts) > 0 {
			partID = it.Parts[0].ID
			break
		}
	}
	if partID == 0 {
		t.Fatal("no part to stream")
	}

	req := httptest.NewRequest(http.MethodGet, "/library/parts/1?X-Plex-Token="+token, nil)
	req.Header.Set("Range", "bytes=0-99")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206 (a player seeks with ranges)", rec.Code)
	}
	if got := rec.Body.Len(); got != 100 {
		t.Fatalf("served %d bytes, want 100", got)
	}
	if rec.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatal("no Accept-Ranges header, so a client will not try to seek")
	}
}

func TestProgressRoundTripsAndWatchedSticks(t *testing.T) {
	srv, token, db := library(t)
	ctx := context.Background()

	items, _, err := db.SectionItems(ctx, 1, domain.KindMovie, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	id := items[0].ID

	// A position partway through comes back as a resume offset.
	play := httptest.NewRequest(http.MethodGet,
		"/:/timeline?ratingKey="+itoa(id)+"&time=60000&duration=600000&state=playing", nil)
	play.Header.Set("X-Plex-Token", token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, play)
	if rec.Code != http.StatusOK {
		t.Fatalf("timeline status = %d", rec.Code)
	}

	mc := get(t, srv, token, "/library/metadata/"+itoa(id))
	meta, _ := mc["Metadata"].([]any)
	first, _ := meta[0].(map[string]any)
	if first["viewOffset"].(float64) != 60000 {
		t.Fatalf("viewOffset = %v", first["viewOffset"])
	}

	// Past the 90% mark it counts as watched, so the offset stops being
	// offered as a resume point.
	done := httptest.NewRequest(http.MethodGet,
		"/:/timeline?ratingKey="+itoa(id)+"&time=595000&duration=600000&state=stopped", nil)
	done.Header.Set("X-Plex-Token", token)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, done)
	if rec.Code != http.StatusOK {
		t.Fatalf("timeline status = %d", rec.Code)
	}

	mc = get(t, srv, token, "/library/metadata/"+itoa(id))
	meta, _ = mc["Metadata"].([]any)
	first, _ = meta[0].(map[string]any)
	if _, resumable := first["viewOffset"]; resumable {
		t.Fatalf("a watched item still reports a resume offset: %v", first)
	}
	if first["viewCount"].(float64) != 1 {
		t.Fatalf("viewCount = %v", first["viewCount"])
	}
}

func TestSearchFindsAcrossSections(t *testing.T) {
	srv, token, _ := library(t)

	mc := get(t, srv, token, "/hubs/search?query=martian")

	items, _ := mc["Metadata"].([]any)
	if len(items) != 1 {
		t.Fatalf("got %d hits, want 1", len(items))
	}
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }
