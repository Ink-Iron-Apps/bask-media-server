package tmdb_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/metadata"
	"github.com/inknironapps/bask-media-server/internal/metadata/tmdb"
)

// fakeTMDB answers the two routes the artwork chooser uses, plus the search
// one it falls back to when nothing has matched the item yet.
type fakeTMDB struct {
	searches atomic.Int64
	images   atomic.Int64
}

func newFakeTMDB(t *testing.T) *fakeTMDB {
	t.Helper()
	f := &fakeTMDB{}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /search/movie", func(w http.ResponseWriter, r *http.Request) {
		f.searches.Add(1)
		if r.URL.Query().Get("query") != "Epic" {
			writeJSON(w, map[string]any{"results": []any{}})
			return
		}
		writeJSON(w, map[string]any{"results": []map[string]any{
			{"id": 77, "title": "Epic", "release_date": "2024-05-01"},
		}})
	})
	mux.HandleFunc("GET /movie/{id}/release_dates", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"results": []any{}})
	})
	mux.HandleFunc("GET /genre/movie/list", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"genres": []any{}})
	})

	mux.HandleFunc("GET /movie/{id}/images", func(w http.ResponseWriter, r *http.Request) {
		f.images.Add(1)
		if r.PathValue("id") != "77" {
			t.Errorf("asked for images of %s, want 77", r.PathValue("id"))
		}
		writeJSON(w, map[string]any{
			"posters": []map[string]any{
				{"file_path": "/english.jpg", "iso_639_1": "en", "vote_average": 9, "width": 500, "height": 750},
				{"file_path": "/plain.jpg", "iso_639_1": nil, "vote_average": 4, "width": 2000, "height": 3000},
			},
			"backdrops": []map[string]any{
				{"file_path": "/wide.jpg", "iso_639_1": nil, "vote_average": 6, "width": 3840, "height": 2160},
			},
		})
	})
	mux.HandleFunc("GET /tv/{id}/images", func(w http.ResponseWriter, r *http.Request) {
		f.images.Add(1)
		writeJSON(w, map[string]any{"posters": []any{}, "backdrops": []any{}})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	was := tmdb.BaseURL
	tmdb.BaseURL = server.URL
	t.Cleanup(func() { tmdb.BaseURL = was })
	return f
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func TestArtUsesTheIdWhenItIsKnown(t *testing.T) {
	f := newFakeTMDB(t)
	client, err := tmdb.New("a-key", 2)
	if err != nil {
		t.Fatal(err)
	}

	art, err := client.Art(context.Background(), metadata.ArtQuery{
		Kind: domain.KindMovie, Title: "Epic", Year: 2024, ProviderID: "77",
	})
	if err != nil {
		t.Fatal(err)
	}
	// No search: the item already carried TMDB's own id, and searching again
	// could rank differently and offer another film's pictures.
	if got := f.searches.Load(); got != 0 {
		t.Fatalf("searched %d times with an id in hand", got)
	}
	if len(art) != 3 {
		t.Fatalf("art = %+v", art)
	}

	// Posters first, textless before English, and the thumbnail a smaller
	// address than the full image.
	if art[0].Language != "" || !strings.HasSuffix(art[0].URL, "/plain.jpg") {
		t.Fatalf("first = %+v", art[0])
	}
	if art[1].Language != "en" {
		t.Fatalf("second = %+v", art[1])
	}
	if art[2].Kind != metadata.ArtBackdrop {
		t.Fatalf("third = %+v", art[2])
	}
	if !strings.Contains(art[0].ThumbURL, "/w185") {
		t.Fatalf("the chooser would fetch full-size posters: %q", art[0].ThumbURL)
	}
	if !strings.Contains(art[0].URL, "/w500") {
		t.Fatalf("the full poster is not the full size: %q", art[0].URL)
	}
	if !strings.Contains(art[2].ThumbURL, "/w300") {
		t.Fatalf("backdrop thumb = %q", art[2].ThumbURL)
	}
	if art[0].Source != "tmdb" {
		t.Fatalf("source = %q", art[0].Source)
	}
}

func TestArtSearchesWhenNothingHasMatchedYet(t *testing.T) {
	f := newFakeTMDB(t)
	client, _ := tmdb.New("a-key", 2)

	art, err := client.Art(context.Background(), metadata.ArtQuery{
		Kind: domain.KindMovie, Title: "Epic", Year: 2024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.searches.Load(); got != 1 {
		t.Fatalf("searched %d times, want once", got)
	}
	if len(art) != 3 {
		t.Fatalf("art = %+v", art)
	}
}

func TestAFilmNothingMatchesHasNoPicturesRatherThanAnError(t *testing.T) {
	newFakeTMDB(t)
	client, _ := tmdb.New("a-key", 2)

	art, err := client.Art(context.Background(), metadata.ArtQuery{
		Kind: domain.KindMovie, Title: "Nothing By This Name",
	})
	if err != nil {
		t.Fatalf("no match was reported as a failure: %v", err)
	}
	if len(art) != 0 {
		t.Fatalf("art = %+v", art)
	}
}

func TestASeriesWithNoPicturesIsAnEmptyAnswer(t *testing.T) {
	newFakeTMDB(t)
	client, _ := tmdb.New("a-key", 2)

	art, err := client.Art(context.Background(), metadata.ArtQuery{
		Kind: domain.KindShow, Title: "Hinterland", ProviderID: "5",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(art) != 0 {
		t.Fatalf("art = %+v", art)
	}
}
