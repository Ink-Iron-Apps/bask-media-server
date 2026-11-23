package tvdb_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/metadata"
	"github.com/inknironapps/bask-media-server/internal/metadata/tvdb"
)

// fake is a stand-in for TheTVDB: the login, the search and the two artwork
// routes, counting what was asked so the token can be shown to be reused.
type fake struct {
	logins   atomic.Int64
	searches atomic.Int64
	server   *httptest.Server

	// refuse makes the next data request answer 401, which is how a token
	// that went stale early is tested.
	refuse atomic.Bool
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("login body: %v", err)
		}
		if body["apikey"] != "a-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// An empty pin must never be sent: the real service refuses it.
		if pin, present := body["pin"]; present && pin == "" {
			t.Error("an empty pin was sent")
		}
		f.logins.Add(1)
		writeJSON(w, map[string]any{"data": map[string]string{"token": "a-token"}})
	})

	guard := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer a-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if f.refuse.Swap(false) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}

	mux.HandleFunc("GET /search", guard(func(w http.ResponseWriter, r *http.Request) {
		f.searches.Add(1)
		switch r.URL.Query().Get("query") {
		case "Hinterland":
			writeJSON(w, map[string]any{"data": []map[string]string{
				{"tvdb_id": "111", "type": "series", "name": "Hinterland", "year": "1999"},
				{"tvdb_id": "222", "type": "series", "name": "Hinterland", "year": "2020"},
			}})
		case "Epic":
			writeJSON(w, map[string]any{"data": []map[string]string{
				{"tvdb_id": "333", "type": "movie", "name": "Epic", "year": "2024"},
			}})
		default:
			writeJSON(w, map[string]any{"data": []map[string]string{}})
		}
	}))

	mux.HandleFunc("GET /series/{id}/episodes/default", guard(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("season") != "0" {
			t.Errorf("asked for season %q, want the specials", r.URL.Query().Get("season"))
		}
		writeJSON(w, map[string]any{"data": map[string]any{"episodes": []map[string]any{
			{"number": 1, "seasonNumber": 0, "airsAfterSeason": 1},
			{"number": 2, "seasonNumber": 0, "airsBeforeSeason": 2, "airsBeforeEpisode": 13},
			// Says nothing, which is most specials in most archives.
			{"number": 3, "seasonNumber": 0},
			// Half filled in: an episode with no season to count it in.
			{"number": 4, "seasonNumber": 0, "airsAfterSeason": 2, "airsBeforeEpisode": 4},
			// Not a special at all, so not a placement.
			{"number": 7, "seasonNumber": 2, "airsAfterSeason": 1},
		}}})
	}))

	mux.HandleFunc("GET /series/{id}/artworks", guard(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "222" {
			t.Errorf("asked for series %s, want the one matching the year", r.PathValue("id"))
		}
		writeJSON(w, map[string]any{"data": map[string]any{"artworks": []map[string]any{
			{"id": 1, "image": "https://art/eng-poster.jpg", "thumbnail": "https://art/t1.jpg",
				"language": "eng", "type": 2, "score": 9, "width": 680, "height": 1000},
			{"id": 2, "image": "https://art/plain-poster.jpg", "thumbnail": "https://art/t2.jpg",
				"language": "null", "type": 2, "score": 5, "width": 680, "height": 1000},
			{"id": 3, "image": "https://art/back.jpg", "thumbnail": "https://art/t3.jpg",
				"language": "", "type": 3, "score": 7, "width": 1920, "height": 1080},
			// A banner, which nothing here draws.
			{"id": 4, "image": "https://art/banner.jpg", "language": "eng", "type": 1},
		}}})
	}))

	mux.HandleFunc("GET /movies/{id}/extended", guard(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": map[string]any{"artworks": []map[string]any{
			{"id": 9, "image": "https://art/film.jpg", "language": "null", "type": 14,
				"score": 3, "width": 1000, "height": 1500},
		}}})
	}))

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)

	was := tvdb.BaseURL
	tvdb.BaseURL = f.server.URL
	t.Cleanup(func() { tvdb.BaseURL = was })
	return f
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func TestNewNeedsAKey(t *testing.T) {
	if _, err := tvdb.New("  ", ""); err == nil {
		t.Fatal("a client was built with no key")
	}
}

func TestArtListsASeriesPictures(t *testing.T) {
	newFake(t)
	client, err := tvdb.New("a-key", "")
	if err != nil {
		t.Fatal(err)
	}

	art, err := client.Art(context.Background(), metadata.ArtQuery{
		Kind: domain.KindShow, Title: "Hinterland", Year: 2020,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(art) != 3 {
		t.Fatalf("got %d pictures, want the 3 that are a poster or a backdrop: %+v", len(art), art)
	}

	// Posters first, and the textless one before the English one: a client
	// draws the title itself, so a poster with words on it reads as a
	// mistake once it is on the shelf.
	if art[0].URL != "https://art/plain-poster.jpg" {
		t.Fatalf("first = %+v", art[0])
	}
	if art[0].Language != "" {
		t.Fatalf("the textless poster reported a language: %q", art[0].Language)
	}
	if art[1].Language != "en" {
		t.Fatalf("eng was not read as en: %q", art[1].Language)
	}
	if art[2].Kind != metadata.ArtBackdrop {
		t.Fatalf("the backdrop did not come last: %+v", art[2])
	}
	if art[0].ThumbURL == art[0].URL {
		t.Fatal("the thumbnail was not used, so a grid would fetch full-size posters")
	}
	if art[0].Source != "tvdb" {
		t.Fatalf("source = %q", art[0].Source)
	}
}

func TestArtListsAFilmsPictures(t *testing.T) {
	newFake(t)
	client, _ := tvdb.New("a-key", "")

	art, err := client.Art(context.Background(), metadata.ArtQuery{
		Kind: domain.KindMovie, Title: "Epic", Year: 2024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(art) != 1 || art[0].Kind != metadata.ArtPoster {
		t.Fatalf("art = %+v", art)
	}
	// No thumbnail of its own, so the full image stands in rather than an
	// empty src.
	if art[0].ThumbURL != art[0].URL {
		t.Fatalf("thumb = %q", art[0].ThumbURL)
	}
}

func TestNothingFoundIsAnAnswer(t *testing.T) {
	newFake(t)
	client, _ := tvdb.New("a-key", "")

	art, err := client.Art(context.Background(), metadata.ArtQuery{
		Kind: domain.KindMovie, Title: "Nothing By This Name",
	})
	if err != nil {
		t.Fatalf("a search with no results was reported as a failure: %v", err)
	}
	if len(art) != 0 {
		t.Fatalf("art = %+v", art)
	}
}

func TestTheLoginIsReused(t *testing.T) {
	f := newFake(t)
	client, _ := tvdb.New("a-key", "")

	for range 3 {
		if _, err := client.Art(context.Background(), metadata.ArtQuery{
			Kind: domain.KindShow, Title: "Hinterland", Year: 2020,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.logins.Load(); got != 1 {
		t.Fatalf("logged in %d times, want once", got)
	}
	if got := f.searches.Load(); got != 3 {
		t.Fatalf("searched %d times, want one per ask", got)
	}
}

func TestAStaleTokenIsForgotten(t *testing.T) {
	f := newFake(t)
	client, _ := tvdb.New("a-key", "")

	if _, err := client.Art(context.Background(), metadata.ArtQuery{
		Kind: domain.KindShow, Title: "Hinterland", Year: 2020,
	}); err != nil {
		t.Fatal(err)
	}

	// The service decides the token is no longer good. The request fails,
	// and the next one logs in again rather than failing for the rest of the
	// day.
	f.refuse.Store(true)
	if _, err := client.Art(context.Background(), metadata.ArtQuery{
		Kind: domain.KindShow, Title: "Hinterland", Year: 2020,
	}); err == nil {
		t.Fatal("a rejected login was not reported")
	}
	if _, err := client.Art(context.Background(), metadata.ArtQuery{
		Kind: domain.KindShow, Title: "Hinterland", Year: 2020,
	}); err != nil {
		t.Fatalf("the client did not log in again: %v", err)
	}
	if got := f.logins.Load(); got != 2 {
		t.Fatalf("logged in %d times, want twice", got)
	}
}

func TestAWrongKeyIsSaidPlainly(t *testing.T) {
	newFake(t)
	client, _ := tvdb.New("not-the-key", "")

	_, err := client.Art(context.Background(), metadata.ArtQuery{
		Kind: domain.KindShow, Title: "Hinterland",
	})
	if err == nil {
		t.Fatal("a wrong key worked")
	}
	if err.Error() != "TheTVDB rejected the key" {
		t.Fatalf("error = %q", err)
	}
}

func TestASubscriberPinIsSentAndAnEmptyOneIsNot(t *testing.T) {
	newFake(t)
	// The fake fails the test if an empty pin is present in the body, so the
	// client with no pin passing is the assertion for the empty case.
	plain, _ := tvdb.New("a-key", "")
	if _, err := plain.Art(context.Background(), metadata.ArtQuery{Kind: domain.KindMovie, Title: "Epic"}); err != nil {
		t.Fatal(err)
	}
	withPin, _ := tvdb.New("a-key", "1234")
	if _, err := withPin.Art(context.Background(), metadata.ArtQuery{Kind: domain.KindMovie, Title: "Epic"}); err != nil {
		t.Fatal(err)
	}
}

func TestPlacementsReadWhereTheSpecialsBelong(t *testing.T) {
	newFake(t)
	client, _ := tvdb.New("a-key", "")

	placements, err := client.Placements(context.Background(), metadata.ArtQuery{
		Kind: domain.KindShow, Title: "Hinterland", Year: 2020,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The Christmas special: after series one, which is the case this whole
	// feature exists for.
	first, found := placements[1]
	if !found {
		t.Fatalf("placements = %+v", placements)
	}
	if first.AfterSeason != 1 || first.BeforeSeason != domain.NoPlacement {
		t.Fatalf("special 1 = %+v", first)
	}

	// One placed before a particular episode.
	second := placements[2]
	if second.BeforeSeason != 2 || second.BeforeEpisode != 13 {
		t.Fatalf("special 2 = %+v", second)
	}

	// A record that says nothing is not a placement: storing it would
	// overwrite nothing with nothing and cost a write per special.
	if _, found := placements[3]; found {
		t.Fatalf("an empty record became a placement: %+v", placements[3])
	}

	// "Before episode" with no season names nothing, so the episode is
	// dropped rather than acted on — TheTVDB's own docs say the two go
	// together, and a record with only one is half filled in.
	fourth, found := placements[4]
	if !found {
		t.Fatalf("placements = %+v", placements)
	}
	if fourth.BeforeEpisode != domain.NoPlacement {
		t.Fatalf("special 4 kept an episode with no season: %+v", fourth)
	}
}

func TestPlacementsAreOnlyAskedAboutASeries(t *testing.T) {
	f := newFake(t)
	client, _ := tvdb.New("a-key", "")

	before := f.searches.Load()
	placements, err := client.Placements(context.Background(), metadata.ArtQuery{
		Kind: domain.KindMovie, Title: "Epic",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(placements) != 0 {
		t.Fatalf("a film had specials placed: %+v", placements)
	}
	// Nothing was even asked: a film has no seasons to sit between.
	if f.searches.Load() != before {
		t.Fatal("a film was searched for")
	}
}
