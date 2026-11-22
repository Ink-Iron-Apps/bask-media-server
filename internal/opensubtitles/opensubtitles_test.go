package opensubtitles_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/opensubtitles"
)

// fakeService stands in for api.opensubtitles.com, so these tests run with no
// key, no network and no daily allowance to spend.
type fakeService struct {
	server *httptest.Server

	searches  int
	downloads int
	lastQuery string

	// quota makes the service answer the way it does when a key has had its
	// downloads for the day.
	quota bool
}

func newFakeService(t *testing.T) *fakeService {
	t.Helper()
	fake := &fakeService{}

	mux := http.NewServeMux()
	mux.HandleFunc("/subtitles", func(w http.ResponseWriter, r *http.Request) {
		fake.searches++
		fake.lastQuery = r.URL.RawQuery
		if r.Header.Get("Api-Key") == "" {
			http.Error(w, "no key", http.StatusUnauthorized)
			return
		}
		// Two answers, deliberately in the wrong order: the popular one
		// first and the hash match second, so the ordering is being tested
		// rather than passed through.
		fmt.Fprint(w, `{"data":[
		  {"id":"1","attributes":{"language":"en","download_count":9000,"ratings":8.5,
		    "release":"Some Other Release","moviehash_match":false,
		    "files":[{"file_id":111,"file_name":"other.srt"}]}},
		  {"id":"2","attributes":{"language":"en","download_count":12,"ratings":6,
		    "release":"The Very File","moviehash_match":true,"hearing_impaired":true,
		    "files":[{"file_id":222,"file_name":"exact.srt"}]}},
		  {"id":"3","attributes":{"language":"en","release":"No Files At All","files":[]}}
		]}`)
	})
	mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		fake.downloads++
		if fake.quota {
			// What the service says when the day's allowance is gone.
			http.Error(w, "quota", http.StatusTooManyRequests)
			return
		}
		var body struct {
			FileID int64 `json:"file_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if body.FileID != 222 {
			http.Error(w, "no such file", http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"link":%q,"file_name":"exact.srt","remaining":4}`,
			fake.server.URL+"/files/exact.srt")
	})
	mux.HandleFunc("/files/exact.srt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "1\n00:00:01,000 --> 00:00:02,000\nHello.\n")
	})

	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)

	previous := opensubtitles.BaseURL
	opensubtitles.BaseURL = fake.server.URL
	t.Cleanup(func() { opensubtitles.BaseURL = previous })
	return fake
}

// The hash match comes first, because it is the only row that says the
// subtitle was authored against this file and therefore cannot drift.
func TestAHashMatchIsOfferedFirst(t *testing.T) {
	newFakeService(t)
	client := &opensubtitles.Client{Key: "test-key"}

	found, err := client.Search(context.Background(), opensubtitles.Query{
		Title: "Big Hill", Year: 2026, Hash: "deadbeefdeadbeef", Size: 1 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 {
		t.Fatalf("offered %d subtitles, want the two with files: %+v", len(found), found)
	}
	if !found[0].Exact {
		t.Fatalf("the exact match is not first: %+v", found)
	}
	if found[0].FileID != 222 || !found[0].HearingImpaired {
		t.Fatalf("first = %+v", found[0])
	}
	// And the popular one is still there, behind it.
	if found[1].FileID != 111 || found[1].Downloads != 9000 {
		t.Fatalf("second = %+v", found[1])
	}
}

// The hash and the title both go out, because the hash is what makes an exact
// match possible and the title is what finds anything at all.
func TestTheSearchSendsTheHashAndTheTitle(t *testing.T) {
	fake := newFakeService(t)
	client := &opensubtitles.Client{Key: "test-key"}

	if _, err := client.Search(context.Background(), opensubtitles.Query{
		Title: "Big Hill", Year: 2026, Hash: "deadbeefdeadbeef",
		Season: 2, Episode: 4, Languages: []string{"fr", "en"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"moviehash=deadbeefdeadbeef", "query=Big+Hill", "year=2026",
		"season_number=2", "episode_number=4", "languages=fr%2Cen"} {
		if !strings.Contains(fake.lastQuery, want) {
			t.Errorf("the search did not carry %q: %s", want, fake.lastQuery)
		}
	}
}

// With no key nothing is sent at all, which is the whole of this being off by
// default.
func TestWithNoKeyNothingIsAsked(t *testing.T) {
	fake := newFakeService(t)
	client := &opensubtitles.Client{}

	if _, err := client.Search(context.Background(), opensubtitles.Query{Title: "Big Hill"}); !errors.Is(err, opensubtitles.ErrNoKey) {
		t.Fatalf("err = %v, want ErrNoKey", err)
	}
	if _, _, err := client.Download(context.Background(), 222); !errors.Is(err, opensubtitles.ErrNoKey) {
		t.Fatalf("err = %v, want ErrNoKey", err)
	}
	if fake.searches+fake.downloads != 0 {
		t.Fatalf("made %d requests with no key", fake.searches+fake.downloads)
	}
}

// Downloading is two requests — a link, then the file — and what comes back
// is the subtitle's text.
func TestDownloadingFetchesTheText(t *testing.T) {
	fake := newFakeService(t)
	client := &opensubtitles.Client{Key: "test-key"}

	text, name, err := client.Download(context.Background(), 222)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), "Hello.") {
		t.Fatalf("text = %q", text)
	}
	if name != "exact.srt" {
		t.Fatalf("name = %q", name)
	}
	if fake.downloads != 1 {
		t.Fatalf("made %d download requests", fake.downloads)
	}
}

// A spent allowance is its own answer, because the thing to do about it is
// wait rather than retry.
func TestNoDownloadsLeftIsItsOwnAnswer(t *testing.T) {
	fake := newFakeService(t)
	fake.quota = true
	client := &opensubtitles.Client{Key: "test-key"}

	if _, _, err := client.Download(context.Background(), 222); !errors.Is(err, opensubtitles.ErrQuota) {
		t.Fatalf("err = %v, want ErrQuota", err)
	}
}

// The hash is the file's size plus the 64-bit sum of its first and last 64
// KiB. Checked against a file whose bytes are known, because an algorithm
// nobody can verify is one that silently matches nothing.
func TestTheHashIsSizePlusBothEnds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "film.mkv")
	// 192 KiB: two 64 KiB chunks of known bytes and a middle that must not
	// be read at all.
	const chunk = 64 << 10
	body := make([]byte, 3*chunk)
	for i := range body[:chunk] {
		body[i] = 1
	}
	for i := range body[chunk : 2*chunk] {
		body[chunk+i] = 0xff // the middle, which the algorithm ignores
	}
	for i := range body[2*chunk:] {
		body[2*chunk+i] = 2
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	hash, size, err := opensubtitles.Hash(path)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(body)) {
		t.Fatalf("size = %d", size)
	}
	// Worked out the same way the algorithm says, rather than by running it:
	// 8192 words of 0x0101010101010101 from the head, the same of
	// 0x0202020202020202 from the tail, plus the size.
	var want uint64 = uint64(len(body))
	for range chunk / 8 {
		want += 0x0101010101010101
		want += 0x0202020202020202
	}
	if hash != fmt.Sprintf("%016x", want) {
		t.Fatalf("hash = %s, want %016x", hash, want)
	}

	// A file too small for the algorithm says so rather than returning a
	// hash of nothing.
	small := filepath.Join(t.TempDir(), "tiny.mkv")
	if err := os.WriteFile(small, []byte("not a film"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := opensubtitles.Hash(small); err == nil {
		t.Fatal("a ten-byte file was hashed")
	}
}
