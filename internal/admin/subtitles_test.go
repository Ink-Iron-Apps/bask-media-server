package admin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/opensubtitles"
	"github.com/inknironapps/bask-media-server/internal/store"
	"github.com/inknironapps/bask-media-server/internal/subtitles"
)

// fakeProvider is the subtitle service: one guess, one exact match.
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

// searching fits the interface with a fetcher and a stored key.
func searching(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()
	if err := h.db.SetServerValue(ctx, store.KeyOpenSubtitles, "test-key"); err != nil {
		t.Fatal(err)
	}
	h.panel.Fetcher = &subtitles.Fetcher{
		DB:  h.db,
		Key: func(ctx context.Context) (string, error) { return h.db.ServerValue(ctx, store.KeyOpenSubtitles) },
	}
}

// The whole thing from the interface: what is on offer, and the one somebody
// picks written beside the film.
func TestASubtitleIsFoundAndSavedBesideTheFilm(t *testing.T) {
	downloads := fakeProvider(t)
	h := newHarness(t)
	h.signIn(h.adminToken)
	searching(t, h)
	id := film(t, h, "blue.1993", 1993)
	path := "/admin/api/items/" + strconv.FormatInt(id, 10) + "/subtitles"

	_, offered := h.call(http.MethodGet, path+"?language=en", nil)
	list, _ := offered["subtitles"].([]any)
	if len(list) != 2 {
		t.Fatalf("offered = %v", list)
	}
	first, _ := list[0].(map[string]any)
	if first["exact"] != true {
		t.Fatalf("the exact match is not first: %v", list)
	}
	handle, _ := first["handle"].(string)

	res, saved := h.call(http.MethodPost, path, map[string]any{"handle": handle, "language": "en"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("fetch = %d: %v", res.StatusCode, saved)
	}
	if *downloads != 1 {
		t.Fatalf("made %d downloads", *downloads)
	}

	written, _ := saved["path"].(string)
	if !strings.HasSuffix(written, ".en.srt") {
		t.Fatalf("saved as %q, want a name the scanner reads", written)
	}
	body, err := os.ReadFile(written)
	if err != nil {
		t.Fatalf("the subtitle is not on disk: %v", err)
	}
	if !strings.Contains(string(body), "It begins.") {
		t.Fatalf("the file holds %q", body)
	}
	// Beside the film, which is what makes the next scan keep it.
	item, err := h.db.Item(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(written) != filepath.Dir(item.Parts[0].Path) {
		t.Fatalf("written to %s, the film is in %s", filepath.Dir(written), filepath.Dir(item.Parts[0].Path))
	}
	// And recorded, so a client is offered it.
	stored, err := h.db.Subtitles(context.Background(), []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	if len(stored[id]) != 1 || stored[id][0].Path != written {
		t.Fatalf("recorded %+v", stored[id])
	}
}

// With no key the page is told to go and save one, rather than being shown an
// empty list it cannot explain.
func TestWithNoKeyTheInterfaceSaysWhatToDo(t *testing.T) {
	fakeProvider(t)
	h := newHarness(t)
	h.signIn(h.adminToken)
	h.panel.Fetcher = &subtitles.Fetcher{
		DB:  h.db,
		Key: func(ctx context.Context) (string, error) { return h.db.ServerValue(ctx, store.KeyOpenSubtitles) },
	}
	id := film(t, h, "blue.1993", 1993)

	res, body := h.call(http.MethodGet, "/admin/api/items/"+strconv.FormatInt(id, 10)+"/subtitles", nil)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("search = %d, want 409", res.StatusCode)
	}
	if message, _ := body["error"].(string); !strings.Contains(message, "key") {
		t.Fatalf("error = %q, want it to say what is missing", message)
	}
}

// Nobody without a session can make this server fetch anything from a third
// party, which is a request it pays for out of a daily allowance.
func TestFindingSubtitlesNeedsAnAdministrator(t *testing.T) {
	fakeProvider(t)
	h := newHarness(t)
	searching(t, h)

	for _, call := range [][2]string{
		{http.MethodGet, "/admin/api/items/1/subtitles"},
		{http.MethodPost, "/admin/api/items/1/subtitles"},
	} {
		res, _ := h.call(call[0], call[1], nil)
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without a session = %d", call[0], call[1], res.StatusCode)
		}
	}
}
