package enrich_test

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/inknironapps/bask-media-server/internal/artwork"
	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/enrich"
	"github.com/inknironapps/bask-media-server/internal/metadata"
	"github.com/inknironapps/bask-media-server/internal/metadata/tmdb"
	"github.com/inknironapps/bask-media-server/internal/scan"
	"github.com/inknironapps/bask-media-server/internal/store"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("modernc.org/sqlite.(*conn).interruptOnDone.func1"),
	)
}

// fakeTMDB stands in for the real service, so the suite runs with no key, no
// network and no rate limit, and so the awkward cases — a title that matches
// nothing, an episode the show never had — can actually be exercised.
type fakeTMDB struct {
	server   *httptest.Server
	searches int
	images   int
}

func newFakeTMDB(t *testing.T) *fakeTMDB {
	t.Helper()
	fake := &fakeTMDB{}

	mux := http.NewServeMux()
	mux.HandleFunc("/search/movie", func(w http.ResponseWriter, r *http.Request) {
		fake.searches++
		query := r.URL.Query().Get("query")
		if strings.Contains(strings.ToLower(query), "unknown") {
			fmt.Fprint(w, `{"results":[]}`)
			return
		}
		fmt.Fprintf(w, `{"results":[{"id":1234,"title":%q,"overview":"A film.","release_date":"2016-11-11","poster_path":"/p.jpg","backdrop_path":"/b.jpg"}]}`, query)
	})
	mux.HandleFunc("/search/tv", func(w http.ResponseWriter, r *http.Request) {
		fake.searches++
		query := r.URL.Query().Get("query")
		fmt.Fprintf(w, `{"results":[{"id":77,"name":%q,"overview":"A series.","first_air_date":"2002-05-12","poster_path":"/show.jpg","backdrop_path":"/showart.jpg"}]}`, query)
	})
	mux.HandleFunc("/tv/77/season/1/episode/1", func(w http.ResponseWriter, r *http.Request) {
		fake.searches++
		fmt.Fprint(w, `{"id":9001,"name":"Part 1","overview":"The first part.","air_date":"2002-05-12","still_path":"/still.jpg"}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Everything else is a miss, which is how a provider says an episode
		// does not exist.
		http.NotFound(w, r)
	})

	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)

	previous := tmdb.BaseURL
	tmdb.BaseURL = fake.server.URL
	t.Cleanup(func() { tmdb.BaseURL = previous })
	return fake
}

// imageServer serves a real JPEG, so the artwork cache is exercised end to end
// rather than against a byte string that cannot be decoded.
func newImageServer(t *testing.T, fake *fakeTMDB) *httptest.Server {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 200, 300))
	for y := range 300 {
		for x := range 200 {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 120, A: 255})
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.images++
		w.Header().Set("Content-Type", "image/jpeg")
		if err := jpeg.Encode(w, img, nil); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func library(t *testing.T) (*store.DB, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()

	movies := filepath.Join(root, "Movies")
	shows := filepath.Join(root, "TV Shows")
	write := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, 16), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(movies, "Arrival (2016)", "Arrival (2016).mkv"))
	write(filepath.Join(movies, "Unknown Film (1999)", "Unknown Film (1999).mkv"))
	write(filepath.Join(shows, "Dinotopia (2002)", "Season 01", "S01E01 - Part 1.mkv"))
	write(filepath.Join(shows, "Dinotopia (2002)", "Season 01", "S01E02 - Part 2.mkv"))

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
			t.Fatal(err)
		}
	}
	return db, root
}

func TestEnrichFillsSummariesAndArtwork(t *testing.T) {
	ctx := context.Background()
	db, root := library(t)
	fake := newFakeTMDB(t)
	images := newImageServer(t, fake)

	provider, err := tmdb.New("test-key", 2)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := artwork.New(filepath.Join(root, "artwork"), 32<<20, 2)
	if err != nil {
		t.Fatal(err)
	}

	// Point the provider's image paths at the local image server by rewriting
	// what the fake returns: the client builds absolute URLs, so the test
	// swaps the host rather than the provider's code.
	enricher := &enrich.Enricher{DB: db, Provider: rewriting{provider, images.URL}, Artwork: cache}

	result, err := enricher.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Arrival, Dinotopia the show, and episode one matched. The unknown film
	// and episode two did not, and the season is answered by inheritance.
	if result.Matched != 3 {
		t.Fatalf("matched %d, want 3", result.Matched)
	}
	if result.Unmatched != 3 {
		t.Fatalf("unmatched %d, want 3 (unknown film, missing episode, season)", result.Unmatched)
	}

	movies, _, err := db.SectionItems(ctx, 1, domain.KindMovie, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	var arrival domain.Item
	for _, m := range movies {
		if m.Title == "Arrival" {
			arrival = m
		}
	}
	if arrival.Summary != "A film." {
		t.Fatalf("summary = %q", arrival.Summary)
	}
	if arrival.PosterKey == "" || arrival.ArtKey == "" {
		t.Fatalf("artwork keys missing: %+v", arrival)
	}
	if arrival.MetadataAt.IsZero() {
		t.Fatal("metadata timestamp not recorded, so the next pass would ask again")
	}

	// A title nothing matches is marked attempted, so a second pass does not
	// ask about it forever.
	searchesAfterFirst := fake.searches
	if _, err := enricher.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.searches != searchesAfterFirst {
		t.Fatalf("second pass made %d more searches, want none", fake.searches-searchesAfterFirst)
	}
}

func TestSeasonsInheritTheShowsPoster(t *testing.T) {
	ctx := context.Background()
	db, root := library(t)
	fake := newFakeTMDB(t)
	images := newImageServer(t, fake)

	provider, err := tmdb.New("test-key", 2)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := artwork.New(filepath.Join(root, "artwork"), 32<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	enricher := &enrich.Enricher{DB: db, Provider: rewriting{provider, images.URL}, Artwork: cache}
	if _, err := enricher.Run(ctx); err != nil {
		t.Fatal(err)
	}

	shows, _, err := db.SectionItems(ctx, 2, domain.KindShow, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(shows) != 1 {
		t.Fatalf("got %d shows", len(shows))
	}
	seasons, _, err := db.Children(ctx, shows[0].ID, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(seasons) != 1 {
		t.Fatalf("got %d seasons", len(seasons))
	}
	if seasons[0].PosterKey != shows[0].PosterKey || seasons[0].PosterKey == "" {
		t.Fatalf("season poster = %q, show poster = %q", seasons[0].PosterKey, shows[0].PosterKey)
	}
}

func TestArtworkIsFetchedOnceAndResizedOnDemand(t *testing.T) {
	ctx := context.Background()
	fake := &fakeTMDB{}
	images := newImageServer(t, fake)
	cache, err := artwork.New(filepath.Join(t.TempDir(), "artwork"), 32<<20, 2)
	if err != nil {
		t.Fatal(err)
	}

	key, err := cache.Fetch(ctx, images.URL+"/poster.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Fetch(ctx, images.URL+"/poster.jpg"); err != nil {
		t.Fatal(err)
	}
	if fake.images != 1 {
		t.Fatalf("fetched %d times, want 1 (the second must come from disk)", fake.images)
	}

	// Asking for a size produces a rendition; asking again reuses it.
	small, err := cache.Open(ctx, key, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, err := image.Decode(small)
	small.Close()
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bounds().Dx() != 100 {
		t.Fatalf("width = %d, want 100", decoded.Bounds().Dx())
	}
	// Aspect kept: a 200x300 original at width 100 is 150 tall.
	if decoded.Bounds().Dy() != 150 {
		t.Fatalf("height = %d, want 150", decoded.Bounds().Dy())
	}

	// Never enlarged past the original, because upscaling a poster server-side
	// wastes both disk and the viewer's bandwidth.
	big, err := cache.Open(ctx, key, 4000, 0)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, err = image.Decode(big)
	big.Close()
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bounds().Dx() != 200 {
		t.Fatalf("width = %d, want the original 200", decoded.Bounds().Dx())
	}
}

func TestSweepEvictsToTheCap(t *testing.T) {
	ctx := context.Background()
	fake := &fakeTMDB{}
	images := newImageServer(t, fake)

	// A cap of a few kilobytes against images of a few kilobytes each, so the
	// sweep has to evict rather than merely run.
	cache, err := artwork.New(filepath.Join(t.TempDir(), "artwork"), 4<<10, 1)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		if _, err := cache.Fetch(ctx, fmt.Sprintf("%s/poster-%d.jpg", images.URL, i)); err != nil {
			t.Fatal(err)
		}
	}

	freed, err := cache.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if freed == 0 {
		t.Fatal("swept nothing, so the cache has no cap in practice")
	}

	var remaining int64
	err = filepath.WalkDir(cache.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		remaining += info.Size()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if remaining > cache.MaxBytes {
		t.Fatalf("%d bytes left, cap is %d", remaining, cache.MaxBytes)
	}
}

// rewriting points a provider's image URLs at a local server, which is how the
// artwork path is tested without reaching image.tmdb.org.
type rewriting struct {
	inner *tmdb.Client
	host  string
}

func (r rewriting) Name() string { return r.inner.Name() }

func (r rewriting) Movie(ctx context.Context, title string, year int) (metadata.Match, error) {
	match, err := r.inner.Movie(ctx, title, year)
	return r.swap(match), err
}

func (r rewriting) Show(ctx context.Context, title string, year int) (metadata.Match, error) {
	match, err := r.inner.Show(ctx, title, year)
	return r.swap(match), err
}

func (r rewriting) Episode(ctx context.Context, showID string, season, number int) (metadata.Match, error) {
	match, err := r.inner.Episode(ctx, showID, season, number)
	return r.swap(match), err
}

func (r rewriting) swap(match metadata.Match) metadata.Match {
	if match.PosterURL != "" {
		match.PosterURL = r.host + "/poster.jpg"
	}
	if match.BackdropURL != "" {
		match.BackdropURL = r.host + "/backdrop.jpg"
	}
	return match
}
