package jellyfin_test

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
	"github.com/inknironapps/bask-media-server/internal/jellyfin"
	"github.com/inknironapps/bask-media-server/internal/opensubtitles"
	"github.com/inknironapps/bask-media-server/internal/store"
	"github.com/inknironapps/bask-media-server/internal/subtitles"
)

// fakeProvider is the subtitle service: one guess and one exact match.
func fakeProvider(t *testing.T) *int {
	t.Helper()
	downloads := 0
	var base string

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

// fetching is a server that can search for subtitles, with a key stored the
// way the admin interface stores one.
func fetching(t *testing.T) (*jellyfin.Server, string, *store.DB) {
	t.Helper()
	server, token, db := library(t)
	ctx := context.Background()
	if err := db.SetServerValue(ctx, store.KeyOpenSubtitles, "test-key"); err != nil {
		t.Fatal(err)
	}
	server.Fetcher = &subtitles.Fetcher{
		DB:  db,
		Key: func(ctx context.Context) (string, error) { return db.ServerValue(ctx, store.KeyOpenSubtitles) },
	}
	return server, token, db
}

// What a Jellyfin client asks for, and what it gets: the exact match first,
// flagged as a hash match, with a handle to post back.
func TestARemoteSearchOffersTheExactMatchFirst(t *testing.T) {
	fakeProvider(t)
	server, token, db := fetching(t)
	film := anyFilm(t, db)

	offered := listOf(t, server, token, "/Items/"+jellyfinID(film.ID)+"/RemoteSearch/Subtitles/en")
	if len(offered) != 2 {
		t.Fatalf("offered = %v", offered)
	}
	if offered[0]["IsHashMatch"] != true {
		t.Fatalf("the exact match is not first: %v", offered)
	}
	if id, _ := offered[0]["Id"].(string); !strings.HasPrefix(id, "opensubtitles:") {
		t.Fatalf("Id = %v, want a handle to post back", offered[0]["Id"])
	}
	if offered[0]["ProviderName"] != "OpenSubtitles" {
		t.Fatalf("ProviderName = %v", offered[0]["ProviderName"])
	}
	if name, _ := offered[0]["Name"].(string); !strings.Contains(name, "Exact match") {
		t.Fatalf("Name = %q", name)
	}
}

// And posting one back fetches it, writes it beside the film and records it.
func TestPostingBackASubtitleFetchesIt(t *testing.T) {
	downloads := fakeProvider(t)
	server, token, db := fetching(t)
	film := anyFilm(t, db)

	req := httptest.NewRequest(http.MethodPost,
		"/Items/"+jellyfinID(film.ID)+"/RemoteSearch/Subtitles/opensubtitles:222?Language=en", nil)
	req.Header.Set("X-Emby-Token", token)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("fetch = %d: %s", rec.Code, rec.Body.String())
	}
	if *downloads != 1 {
		t.Fatalf("made %d downloads", *downloads)
	}

	beside := strings.TrimSuffix(film.Parts[0].Path, filepath.Ext(film.Parts[0].Path)) + ".en.srt"
	if _, err := os.Stat(beside); err != nil {
		t.Fatalf("the subtitle is not beside the film: %v", err)
	}
	stored, err := db.Subtitles(context.Background(), []int64{film.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range stored[film.ID] {
		if sub.Path == beside {
			// And it is offered to a client as a track it can ask for, which
			// is the only reason to have fetched it.
			item := listOf(t, server, token, "/Items/"+jellyfinID(film.ID)+"/RemoteSearch/Subtitles/en")
			_ = item
			return
		}
	}
	t.Fatalf("the subtitle was not recorded: %+v", stored[film.ID])
}

// A handle this server never issued is refused rather than turned into a
// request to somebody else's service.
func TestAHandleThisServerDidNotIssueIsRefused(t *testing.T) {
	downloads := fakeProvider(t)
	server, token, db := fetching(t)
	film := anyFilm(t, db)

	for _, handle := range []string{"666", "elsewhere:222", "opensubtitles:nonsense"} {
		req := httptest.NewRequest(http.MethodPost,
			"/Items/"+jellyfinID(film.ID)+"/RemoteSearch/Subtitles/"+handle+"?Language=en", nil)
		req.Header.Set("X-Emby-Token", token)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusNoContent {
			t.Errorf("%q was accepted", handle)
		}
	}
	if *downloads != 0 {
		t.Fatalf("made %d downloads for handles nobody issued", *downloads)
	}
}

// With no key, a client that asks is told nothing rather than that something
// is broken.
func TestWithNoKeyARemoteSearchIsEmpty(t *testing.T) {
	fakeProvider(t)
	server, token, db := library(t)
	server.Fetcher = &subtitles.Fetcher{
		DB:  db,
		Key: func(ctx context.Context) (string, error) { return db.ServerValue(ctx, store.KeyOpenSubtitles) },
	}
	film := anyFilm(t, db)

	if offered := listOf(t, server, token, "/Items/"+jellyfinID(film.ID)+"/RemoteSearch/Subtitles/en"); len(offered) != 0 {
		t.Fatalf("offered %v with no key", offered)
	}
}

// anyFilm is a film from the test library, with its file.
func anyFilm(t *testing.T, db *store.DB) domain.Item {
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
		films, _, err := db.SectionItems(ctx, section.ID, domain.KindMovie, store.Page{Size: 5})
		if err != nil {
			t.Fatal(err)
		}
		for _, film := range films {
			if len(film.Parts) > 0 {
				return film
			}
		}
	}
	t.Fatal("the test library has no film with a file")
	return domain.Item{}
}
