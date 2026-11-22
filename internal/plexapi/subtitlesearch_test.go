package plexapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/opensubtitles"
	"github.com/inknironapps/bask-media-server/internal/store"
	"github.com/inknironapps/bask-media-server/internal/subtitles"
)

// fakeProvider is the subtitle service, small enough to say what this
// surface does with what it answers.
func fakeProvider(t *testing.T) *int {
	t.Helper()
	downloads := 0

	mux := http.NewServeMux()
	mux.HandleFunc("/subtitles", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Api-Key") == "" {
			http.Error(w, "no key", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"data":[
		  {"id":"1","attributes":{"language":"en","download_count":40,"release":"A Guess",
		    "files":[{"file_id":111,"file_name":"guess.srt"}]}},
		  {"id":"2","attributes":{"language":"en","moviehash_match":true,"release":"This File",
		    "files":[{"file_id":222,"file_name":"exact.srt"}]}}
		]}`)
	})
	var base string
	mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		downloads++
		var body struct {
			FileID int64 `json:"file_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.FileID != 222 {
			http.Error(w, "no such file", http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"link":%q,"file_name":"exact.srt"}`, base+"/files/exact.srt")
	})
	mux.HandleFunc("/files/exact.srt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "1\n00:00:01,000 --> 00:00:02,000\nIt begins.\n")
	})

	server := httptest.NewServer(mux)
	base = server.URL
	t.Cleanup(server.Close)

	previous := opensubtitles.BaseURL
	opensubtitles.BaseURL = server.URL
	t.Cleanup(func() { opensubtitles.BaseURL = previous })
	return &downloads
}

// A client asks what is out there and gets the exact match first, marked as
// one: that is the row Bask's player takes and the only one that cannot drift.
func TestSubtitlesAreOfferedWithTheExactMatchFirst(t *testing.T) {
	fakeProvider(t)
	server, token, db := library(t)
	ctx := context.Background()
	if err := db.SetServerValue(ctx, store.KeyOpenSubtitles, "test-key"); err != nil {
		t.Fatal(err)
	}
	server.Fetcher = &subtitles.Fetcher{
		DB:  db,
		Key: func(ctx context.Context) (string, error) { return db.ServerValue(ctx, store.KeyOpenSubtitles) },
	}

	film := firstFilm(t, db)
	out := get(t, server, token, "/library/metadata/"+itoa(film.ID)+"/subtitles?language=en")
	offered, _ := out["Stream"].([]any)
	if len(offered) != 2 {
		t.Fatalf("offered = %v", offered)
	}
	first, _ := offered[0].(map[string]any)
	if first["perfectMatch"] != true {
		t.Fatalf("the exact match is not first: %v", offered)
	}
	if key, _ := first["key"].(string); !strings.HasPrefix(key, "opensubtitles:") {
		t.Fatalf("key = %v, want a handle to send back", first["key"])
	}
	if title, _ := first["displayTitle"].(string); !strings.Contains(title, "Exact match") {
		t.Fatalf("displayTitle = %q", title)
	}
	if first["providerTitle"] != "OpenSubtitles" {
		t.Fatalf("providerTitle = %v", first["providerTitle"])
	}
}

// Fetching one writes it beside the film, named the way the scanner reads a
// sidecar, and records it — so the next scan finds the same file and leaves
// the row alone rather than deleting it.
func TestFetchingASubtitleWritesItBesideTheFilm(t *testing.T) {
	downloads := fakeProvider(t)
	server, token, db := library(t)
	ctx := context.Background()
	if err := db.SetServerValue(ctx, store.KeyOpenSubtitles, "test-key"); err != nil {
		t.Fatal(err)
	}
	server.Fetcher = &subtitles.Fetcher{
		DB:  db,
		Key: func(ctx context.Context) (string, error) { return db.ServerValue(ctx, store.KeyOpenSubtitles) },
	}

	film := firstFilm(t, db)
	path := "/library/metadata/" + itoa(film.ID) + "/subtitles?language=en&key=opensubtitles:222"
	req := httptest.NewRequest(http.MethodPut, path, nil)
	req.Header.Set("X-Plex-Token", token)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fetch = %d: %s", rec.Code, rec.Body.String())
	}
	if *downloads != 1 {
		t.Fatalf("made %d downloads", *downloads)
	}

	// Beside the film, named so a scan reads the language off it.
	beside := strings.TrimSuffix(film.Parts[0].Path, filepath.Ext(film.Parts[0].Path)) + ".en.srt"
	body, err := os.ReadFile(beside)
	if err != nil {
		t.Fatalf("the subtitle is not beside the film: %v", err)
	}
	if !strings.Contains(string(body), "It begins.") {
		t.Fatalf("the file holds %q", body)
	}

	// And it is a track the client can now ask for.
	stored, err := db.Subtitles(ctx, []int64{film.ID})
	if err != nil {
		t.Fatal(err)
	}
	list := stored[film.ID]
	found := false
	for _, sub := range list {
		if sub.Path == beside && sub.External && sub.Language == "en" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the subtitle was not recorded: %+v", list)
	}
}

// With no key the answer is an empty list rather than an error: searching is
// off, and a client asking is not doing anything wrong.
func TestWithNoKeyNothingIsOffered(t *testing.T) {
	fakeProvider(t)
	server, token, db := library(t)
	server.Fetcher = &subtitles.Fetcher{
		DB:  db,
		Key: func(ctx context.Context) (string, error) { return db.ServerValue(ctx, store.KeyOpenSubtitles) },
	}

	film := firstFilm(t, db)
	out := get(t, server, token, "/library/metadata/"+itoa(film.ID)+"/subtitles?language=en")
	if offered, _ := out["Stream"].([]any); len(offered) != 0 {
		t.Fatalf("offered %v with no key", offered)
	}
}

// firstFilm is the film the test library holds, with its file.
func firstFilm(t *testing.T, db *store.DB) domain.Item {
	t.Helper()
	ctx := context.Background()
	sections, err := db.Sections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range sections {
		if section.Kind != domain.SectionMovies {
			continue
		}
		films, _, err := db.SectionItems(ctx, section.ID, domain.KindMovie, store.Page{Size: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(films) > 0 && len(films[0].Parts) > 0 {
			return films[0]
		}
	}
	t.Fatal("the test library has no film with a file")
	return domain.Item{}
}
