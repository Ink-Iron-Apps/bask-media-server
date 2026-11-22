package jellyfin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/jellyfin"
	"github.com/inknironapps/bask-media-server/internal/scan"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// bonusLibrary is one film with a trailer and a deleted scene, scanned.
//
// Its own small library rather than the shared harness, whose counts several
// other tests assert on.
func bonusLibrary(t *testing.T) (*jellyfin.Server, string, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()

	write := func(parts ...string) {
		t.Helper()
		path := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, 512), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Arrival (2016)", "Arrival (2016).mkv")
	write("Arrival (2016)", "Trailers", "Teaser.mkv")
	write("Arrival (2016)", "Deleted Scenes", "The Ending.mkv")

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bms.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	section, err := db.CreateSection(ctx, "Films", domain.SectionMovies, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&scan.Scanner{DB: db, Workers: 2}).Section(ctx, *section); err != nil {
		t.Fatal(err)
	}
	films, _, err := db.SectionItems(ctx, section.ID, domain.KindMovie, store.Page{Size: 10})
	if err != nil || len(films) != 1 {
		t.Fatalf("expected one film, got %v (%v)", films, err)
	}

	user, err := db.CreateUser(ctx, "owner", true)
	if err != nil {
		t.Fatal(err)
	}
	token, err := db.IssueToken(ctx, user.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	server := &jellyfin.Server{
		Store: db, Name: "BMS test", MachineID: "0123456789abcdef0123456789abcdef0123", Version: "test",
	}
	return server, token, jellyfinID(films[0].ID)
}

// jellyfinID is the GUID-shaped spelling of a row number this surface hands
// out, written here rather than imported because it is unexported.
func jellyfinID(id int64) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 32)
	for i := 31; i >= 0; i-- {
		out[i] = hex[id&0xf]
		id >>= 4
	}
	return string(out)
}

func listOf(t *testing.T, server *jellyfin.Server, token, path string) []map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Emby-Token", token)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
	}
	var out []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET %s: %v, body %s", path, err, rec.Body.String())
	}
	return out
}

// Both routes a Jellyfin client uses: everything that belongs to the film,
// and the trailers among them.
func TestSpecialFeaturesAndLocalTrailers(t *testing.T) {
	server, token, film := bonusLibrary(t)

	features := listOf(t, server, token, "/Items/"+film+"/SpecialFeatures")
	if len(features) != 2 {
		t.Fatalf("special features = %v", features)
	}
	if features[0]["ExtraType"] != "Trailer" || features[0]["Name"] != "Teaser" {
		t.Fatalf("first feature = %v", features[0])
	}
	if features[0]["MediaType"] != "Video" {
		t.Fatalf("an extra a client cannot play: %v", features[0])
	}
	if features[1]["ExtraType"] != "DeletedScene" {
		t.Fatalf("second feature = %v", features[1])
	}

	trailers := listOf(t, server, token, "/Items/"+film+"/LocalTrailers")
	if len(trailers) != 1 || trailers[0]["Name"] != "Teaser" {
		t.Fatalf("local trailers = %v", trailers)
	}

	// The same answers under the per-user spelling, which is what the older
	// clients ask.
	me := listOf(t, server, token, "/Users/"+film+"/Items/"+film+"/SpecialFeatures")
	if len(me) != 2 {
		t.Fatalf("the per-user route answered %v", me)
	}
}

// The counts are on the item, which is how a client knows to draw the row
// before it asks for the rows.
func TestTheItemSaysHowManyExtrasItHas(t *testing.T) {
	server, token, film := bonusLibrary(t)

	req := httptest.NewRequest(http.MethodGet, "/Items/"+film+"/PlaybackInfo", nil)
	req.Header.Set("X-Emby-Token", token)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("playback info = %d", rec.Code)
	}

	// The item itself, where the counts live.
	detail := map[string]any{}
	req = httptest.NewRequest(http.MethodGet, "/Users/"+film+"/Items/"+film, nil)
	req.Header.Set("X-Emby-Token", token)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("item = %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail["LocalTrailerCount"] != float64(1) {
		t.Errorf("trailer count = %v", detail["LocalTrailerCount"])
	}
	if detail["SpecialFeatureCount"] != float64(1) {
		t.Errorf("special feature count = %v", detail["SpecialFeatureCount"])
	}
}

// And a library of one film with extras in it is a library of one film.
func TestExtrasAreNotItemsInTheLibrary(t *testing.T) {
	server, token, _ := bonusLibrary(t)

	req := httptest.NewRequest(http.MethodGet, "/Items?IncludeItemTypes=Movie&Recursive=true", nil)
	req.Header.Set("X-Emby-Token", token)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("items = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items            []map[string]any `json:"Items"`
		TotalRecordCount int              `json:"TotalRecordCount"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.TotalRecordCount != 1 {
		t.Fatalf("the library lists %d films: %v", body.TotalRecordCount, body.Items)
	}
}
