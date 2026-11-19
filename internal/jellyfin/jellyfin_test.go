package jellyfin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/inknironapps/bask-media-server/internal/artwork"
	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/ffprobe"
	"github.com/inknironapps/bask-media-server/internal/jellyfin"
	"github.com/inknironapps/bask-media-server/internal/scan"
	"github.com/inknironapps/bask-media-server/internal/store"
	"github.com/inknironapps/bask-media-server/internal/subtitles"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("modernc.org/sqlite.(*conn).interruptOnDone.func1"),
	)
}

// prober stands in for ffprobe: a duration for everything and tags for the
// music, which is what a scan reads.
type prober struct{}

func (prober) Probe(_ context.Context, path string) (ffprobe.Info, error) {
	info := ffprobe.Info{Duration: 30 * time.Minute}
	switch filepath.Base(path) {
	case "01 - So What.flac":
		info.Tags = ffprobe.Tags{
			Title: "So What", Artist: "Miles Davis", AlbumArtist: "Miles Davis",
			Album: "Kind of Blue", Date: "1959", Track: 1,
		}
	case "02 - Blue in Green.flac":
		info.Tags = ffprobe.Tags{
			Title: "Blue in Green", Artist: "Bill Evans", AlbumArtist: "Miles Davis",
			Album: "Kind of Blue", Date: "1959", Track: 2,
		}
	}
	return info, nil
}

// library stands up one of each kind of section over real files, scans it,
// and returns a server plus a working token.
func library(t *testing.T) (*jellyfin.Server, string, *store.DB) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()

	write := func(name string, body []byte) string {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	write("Films/Arrival (2016)/Arrival (2016).mkv", []byte("a film"))
	write("Films/Arrival (2016)/Arrival (2016).en.srt", []byte("1\n00:00:01,000 --> 00:00:04,000\nHello.\n"))
	write("Shows/Hornblower (1998)/Season 01/S01E01 - The Duel.mkv", []byte("an episode"))
	write("Shows/Hornblower (1998)/Season 01/S01E02 - The Fire Ship.mkv", []byte("another"))
	write("Music/Miles Davis/Kind of Blue (1959)/01 - So What.flac", []byte("audio"))
	write("Music/Miles Davis/Kind of Blue (1959)/02 - Blue in Green.flac", []byte("audio"))
	write("Music/Miles Davis/Kind of Blue (1959)/cover.jpg", smallJPEG(t, 300, 300))
	write("Pictures/2016/Cornwall/IMG_0042.jpg", smallJPEG(t, 120, 80))

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bms.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	sections := []struct {
		title string
		kind  domain.SectionKind
		dir   string
	}{
		{"Films", domain.SectionMovies, "Films"},
		{"TV Shows", domain.SectionShows, "Shows"},
		{"Music", domain.SectionMusic, "Music"},
		{"Photos", domain.SectionPhotos, "Pictures"},
	}
	images, err := artwork.New(filepath.Join(t.TempDir(), "art"), 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	scanner := &scan.Scanner{DB: db, Workers: 2, Prober: prober{}, Artwork: images}
	for _, want := range sections {
		section, err := db.CreateSection(ctx, want.title, want.kind, []string{filepath.Join(root, want.dir)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := scanner.Section(ctx, *section); err != nil {
			t.Fatalf("scan %s: %v", want.title, err)
		}
	}

	user, err := db.CreateUser(ctx, "owner", true)
	if err != nil {
		t.Fatal(err)
	}
	token, err := db.IssueToken(ctx, user.ID, "tablet")
	if err != nil {
		t.Fatal(err)
	}

	return &jellyfin.Server{
		DB: db, Name: "BMS test", MachineID: "0123456789abcdef0123456789abcdef0123", Version: "test",
		Artwork: images, Subtitles: &subtitles.Converter{},
	}, token, db
}

func smallJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := jpeg.Encode(&out, image.NewRGBA(image.Rect(0, 0, width, height)), nil); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

type call struct {
	t      *testing.T
	server *jellyfin.Server
	token  string
}

func (c call) do(method, path string, body any) (*http.Response, []byte) {
	c.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if c.token != "" {
		req.Header.Set("X-Emby-Token", c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	c.server.Handler().ServeHTTP(rec, req)
	return rec.Result(), rec.Body.Bytes()
}

func (c call) json(path string) map[string]any {
	c.t.Helper()
	res, body := c.do(http.MethodGet, path, nil)
	if res.StatusCode != http.StatusOK {
		c.t.Fatalf("GET %s: %d %s", path, res.StatusCode, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		c.t.Fatalf("GET %s: %v (%s)", path, err, body)
	}
	return out
}

func (c call) items(path string) []map[string]any {
	c.t.Helper()
	reply := c.json(path)
	raw, _ := reply["Items"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		if row, ok := entry.(map[string]any); ok {
			out = append(out, row)
		}
	}
	return out
}

func (c call) me() string {
	c.t.Helper()
	return c.json("/Users/Me")["Id"].(string)
}

func (c call) views() map[string]map[string]any {
	c.t.Helper()
	out := map[string]map[string]any{}
	for _, view := range c.items("/Users/" + c.me() + "/Views") {
		out[view["Name"].(string)] = view
	}
	return out
}

// A client asks this before anybody has a credential: it is how an address
// becomes a server in the list.
func TestDiscoveryNeedsNoToken(t *testing.T) {
	server, _, _ := library(t)
	c := call{t: t, server: server}

	res, body := c.do(http.MethodGet, "/System/Info/Public", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	var info map[string]any
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatal(err)
	}
	if info["ServerName"] != "BMS test" || info["ProductName"] != "Bask Media Server" {
		t.Errorf("info = %v", info)
	}
	if id, _ := info["Id"].(string); len(id) != 32 {
		t.Errorf("Id = %q, want the 32 characters a GUID column holds", id)
	}
}

func TestEverythingElseNeedsAToken(t *testing.T) {
	server, _, _ := library(t)
	c := call{t: t, server: server}

	res, _ := c.do(http.MethodGet, "/Users/Me", nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
}

// The password field carries the token BMS issued. No second secret for the
// same account, and a token stays revocable on its own.
func TestSignInTakesTheTokenAsThePassword(t *testing.T) {
	server, token, _ := library(t)
	c := call{t: t, server: server}

	res, body := c.do(http.MethodPost, "/Users/AuthenticateByName",
		map[string]string{"Username": "owner", "Pw": token})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out["AccessToken"] != token {
		t.Errorf("AccessToken = %v", out["AccessToken"])
	}
	user, _ := out["User"].(map[string]any)
	if user["Name"] != "owner" {
		t.Errorf("User = %v", user)
	}
	policy, _ := user["Policy"].(map[string]any)
	if policy["IsAdministrator"] != true {
		t.Errorf("an admin should be told so: %v", policy)
	}
}

func TestAWrongUsernameIsRefusedEvenWithARealToken(t *testing.T) {
	server, token, _ := library(t)
	c := call{t: t, server: server}

	// The token decides which account this is; the name has to agree, or a
	// token would sign in as whoever was typed above it.
	res, _ := c.do(http.MethodPost, "/Users/AuthenticateByName",
		map[string]string{"Username": "somebody-else", "Pw": token})
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
}

func TestTheLibrariesAreViews(t *testing.T) {
	server, token, _ := library(t)
	c := call{t: t, server: server, token: token}

	views := c.views()
	want := map[string]string{
		"Films": "movies", "TV Shows": "tvshows", "Music": "music", "Photos": "homevideos",
	}
	for name, collection := range want {
		view, found := views[name]
		if !found {
			t.Fatalf("no view %q in %v", name, views)
		}
		if view["CollectionType"] != collection {
			t.Errorf("%s is a %v library, want %s", name, view["CollectionType"], collection)
		}
		if view["IsFolder"] != true {
			t.Errorf("%s should be a folder", name)
		}
	}
}

// A library and a row in it are different things, and their row numbers
// collide: library 1 and film 1 both exist. Without a marker in the id,
// whichever the server looked up first would shadow the other — which is
// exactly what it did.
func TestALibraryAndAnItemWithTheSameRowNumberAreDifferentThings(t *testing.T) {
	server, token, _ := library(t)
	c := call{t: t, server: server, token: token}

	films := c.views()["Films"]
	libraryID := films["Id"].(string)
	items := c.items("/Users/" + c.me() + "/Items?ParentId=" + libraryID)
	if len(items) != 1 {
		t.Fatalf("got %d films", len(items))
	}
	filmID := items[0]["Id"].(string)

	if libraryID == filmID {
		t.Fatal("a library and a film answer to the same id")
	}
	asLibrary := c.json("/Users/" + c.me() + "/Items/" + libraryID)
	asFilm := c.json("/Users/" + c.me() + "/Items/" + filmID)
	if asLibrary["Type"] != "CollectionFolder" {
		t.Errorf("the library id is a %v", asLibrary["Type"])
	}
	if asFilm["Type"] != "Movie" || asFilm["Name"] != "Arrival" {
		t.Errorf("the film id is %v %v", asFilm["Type"], asFilm["Name"])
	}
}

func TestAShowItsSeasonsAndItsEpisodes(t *testing.T) {
	server, token, _ := library(t)
	c := call{t: t, server: server, token: token}

	shows := c.items("/Users/" + c.me() + "/Items?ParentId=" + c.views()["TV Shows"]["Id"].(string))
	if len(shows) != 1 || shows[0]["Type"] != "Series" {
		t.Fatalf("shows = %v", shows)
	}
	series := shows[0]
	if series["ChildCount"] != float64(1) {
		t.Errorf("ChildCount = %v, want one season", series["ChildCount"])
	}

	seasons := c.items("/Shows/" + series["Id"].(string) + "/Seasons")
	if len(seasons) != 1 || seasons[0]["Type"] != "Season" {
		t.Fatalf("seasons = %v", seasons)
	}
	if seasons[0]["SeriesName"] != "Hornblower" {
		t.Errorf("SeriesName = %v", seasons[0]["SeriesName"])
	}

	episodes := c.items("/Shows/" + series["Id"].(string) + "/Episodes")
	if len(episodes) != 2 {
		t.Fatalf("got %d episodes", len(episodes))
	}
	first := episodes[0]
	// A client draws a list of episodes with the show and the season on each
	// row; asking again per row is what this avoids.
	if first["SeriesName"] != "Hornblower" || first["SeasonName"] != "Season 1" {
		t.Errorf("row context = %v / %v", first["SeriesName"], first["SeasonName"])
	}
	if first["IndexNumber"] != float64(1) || first["ParentIndexNumber"] != float64(1) {
		t.Errorf("numbering = %v / %v", first["IndexNumber"], first["ParentIndexNumber"])
	}
	if first["RunTimeTicks"] != float64(30*time.Minute/100) {
		t.Errorf("RunTimeTicks = %v", first["RunTimeTicks"])
	}
}

func TestMusicIsArtistsAlbumsAndTracks(t *testing.T) {
	server, token, _ := library(t)
	c := call{t: t, server: server, token: token}

	artists := c.items("/Users/" + c.me() + "/Items?ParentId=" + c.views()["Music"]["Id"].(string))
	if len(artists) != 1 || artists[0]["Type"] != "MusicArtist" {
		t.Fatalf("artists = %v", artists)
	}
	albums := c.items("/Users/" + c.me() + "/Items?ParentId=" + artists[0]["Id"].(string))
	if len(albums) != 1 || albums[0]["Type"] != "MusicAlbum" {
		t.Fatalf("albums = %v", albums)
	}
	if albums[0]["ProductionYear"] != float64(1959) {
		t.Errorf("year = %v", albums[0]["ProductionYear"])
	}
	if tags, _ := albums[0]["ImageTags"].(map[string]any); tags["Primary"] == nil {
		t.Error("the cover beside the album was not offered")
	}

	tracks := c.items("/Users/" + c.me() + "/Items?ParentId=" + albums[0]["Id"].(string))
	if len(tracks) != 2 {
		t.Fatalf("got %d tracks", len(tracks))
	}
	if tracks[0]["Type"] != "Audio" || tracks[0]["MediaType"] != "Audio" {
		t.Errorf("a track is %v/%v", tracks[0]["Type"], tracks[0]["MediaType"])
	}
	if tracks[0]["Album"] != "Kind of Blue" || tracks[0]["AlbumArtist"] != "Miles Davis" {
		t.Errorf("row context = %v", tracks[0])
	}
	// The second track is by somebody else, which is the only thing that
	// makes a compilation readable.
	artistsOf, _ := tracks[1]["Artists"].([]any)
	if len(artistsOf) != 1 || artistsOf[0] != "Bill Evans" {
		t.Errorf("performer = %v", tracks[1]["Artists"])
	}
	// A track with no cover of its own shows the album's.
	if tags, _ := tracks[0]["ImageTags"].(map[string]any); tags["Primary"] == nil {
		t.Error("a track should carry its album's cover")
	}
}

func TestPhotographsCarryTheirShapeAndTheirDate(t *testing.T) {
	server, token, _ := library(t)
	c := call{t: t, server: server, token: token}

	years := c.items("/Users/" + c.me() + "/Items?ParentId=" + c.views()["Photos"]["Id"].(string))
	if len(years) != 1 || years[0]["Type"] != "PhotoAlbum" {
		t.Fatalf("albums = %v", years)
	}
	inner := c.items("/Users/" + c.me() + "/Items?ParentId=" + years[0]["Id"].(string))
	if len(inner) != 1 || inner[0]["Name"] != "Cornwall" {
		t.Fatalf("nested albums = %v", inner)
	}
	photos := c.items("/Users/" + c.me() + "/Items?ParentId=" + inner[0]["Id"].(string))
	if len(photos) != 1 || photos[0]["Type"] != "Photo" {
		t.Fatalf("photos = %v", photos)
	}
	if photos[0]["Width"] != float64(120) || photos[0]["Height"] != float64(80) {
		t.Errorf("a photograph's dimensions are its content: %v x %v", photos[0]["Width"], photos[0]["Height"])
	}
	if photos[0]["PremiereDate"] == nil {
		t.Error("a photograph should say when it was taken")
	}

	res, body := c.do(http.MethodGet, "/Items/"+photos[0]["Id"].(string)+"/Images/Primary?maxWidth=64", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("thumbnail: %d", res.StatusCode)
	}
	if len(body) < 2 || body[0] != 0xFF || body[1] != 0xD8 {
		t.Errorf("that is not a JPEG: % x", body[:min(8, len(body))])
	}
}

func TestSubtitlesAreOfferedAndServed(t *testing.T) {
	server, token, _ := library(t)
	c := call{t: t, server: server, token: token}

	films := c.items("/Users/" + c.me() + "/Items?ParentId=" + c.views()["Films"]["Id"].(string))
	detail := c.json("/Users/" + c.me() + "/Items/" + films[0]["Id"].(string))

	streams, _ := detail["MediaStreams"].([]any)
	if len(streams) != 1 {
		t.Fatalf("streams = %v", streams)
	}
	sub, _ := streams[0].(map[string]any)
	if sub["Type"] != "Subtitle" || sub["Language"] != "eng" {
		t.Errorf("subtitle = %v", sub)
	}
	if sub["IsExternal"] != true || sub["IsTextSubtitleStream"] != true {
		t.Errorf("a sidecar is an external text subtitle: %v", sub)
	}
	// Numbered by position: a sidecar has no index inside the container, and
	// a client switching tracks sends back what it was given.
	if sub["Index"] != float64(0) {
		t.Errorf("Index = %v", sub["Index"])
	}

	url, _ := sub["DeliveryUrl"].(string)
	if url == "" {
		t.Fatal("no delivery url")
	}
	res, body := c.do(http.MethodGet, url, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("subtitle: %d %s", res.StatusCode, body)
	}
	if !strings.HasPrefix(string(body), "WEBVTT") {
		t.Errorf("not WebVTT: %q", string(body[:min(32, len(body))]))
	}
}

// The file itself, with ranges, which is what lets a client seek.
func TestTheFileIsServedWithRanges(t *testing.T) {
	server, token, _ := library(t)
	c := call{t: t, server: server, token: token}

	films := c.items("/Users/" + c.me() + "/Items?ParentId=" + c.views()["Films"]["Id"].(string))
	id := films[0]["Id"].(string)

	req := httptest.NewRequest(http.MethodGet, "/Videos/"+id+"/stream", nil)
	req.Header.Set("X-Emby-Token", token)
	req.Header.Set("Range", "bytes=0-2")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	if rec.Body.String() != "a f" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// Playback info is where a client asks how it may play something, and the
// answer is the file: whether this client can decode it is a question only
// the client can answer.
func TestPlaybackInfoOffersTheFileItself(t *testing.T) {
	server, token, _ := library(t)
	c := call{t: t, server: server, token: token}

	films := c.items("/Users/" + c.me() + "/Items?ParentId=" + c.views()["Films"]["Id"].(string))
	info := c.json("/Items/" + films[0]["Id"].(string) + "/PlaybackInfo")

	sources, _ := info["MediaSources"].([]any)
	if len(sources) != 1 {
		t.Fatalf("sources = %v", sources)
	}
	source, _ := sources[0].(map[string]any)
	if source["SupportsDirectPlay"] != true || source["Container"] != "mkv" {
		t.Errorf("source = %v", source)
	}
	if info["PlaySessionId"] == nil {
		t.Error("a client needs a play session id to report against")
	}
}

// Where a viewer is, written by one client and read by the next.
func TestProgressGoesHomeAndComesBack(t *testing.T) {
	server, token, _ := library(t)
	c := call{t: t, server: server, token: token}

	episodes := c.items("/Shows/" + c.items("/Users/" + c.me() + "/Items?ParentId=" + c.views()["TV Shows"]["Id"].(string))[0]["Id"].(string) + "/Episodes")
	id := episodes[0]["Id"].(string)
	quarter := int64(30*time.Minute/100) / 4

	res, body := c.do(http.MethodPost, "/Sessions/Playing/Progress",
		map[string]any{"ItemId": id, "PositionTicks": quarter})
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("progress: %d %s", res.StatusCode, body)
	}

	again := c.json("/Users/" + c.me() + "/Items/" + id)
	data, _ := again["UserData"].(map[string]any)
	if data["PlaybackPositionTicks"] != float64(quarter) {
		t.Errorf("position = %v, want %d", data["PlaybackPositionTicks"], quarter)
	}
	if pct, _ := data["PlayedPercentage"].(float64); pct < 24 || pct > 26 {
		t.Errorf("percentage = %v", pct)
	}

	resume := c.items("/Users/" + c.me() + "/Items/Resume")
	if len(resume) != 1 || resume[0]["Id"] != id {
		t.Errorf("resume = %v", resume)
	}

	// Stopping near the end is what makes something watched: a position
	// alone cannot say it.
	if res, _ := c.do(http.MethodPost, "/Sessions/Playing/Stopped",
		map[string]any{"ItemId": id, "PositionTicks": int64(30*time.Minute/100) * 97 / 100}); res.StatusCode != http.StatusNoContent {
		t.Fatalf("stopped: %d", res.StatusCode)
	}
	done := c.json("/Users/" + c.me() + "/Items/" + id)
	if data, _ := done["UserData"].(map[string]any); data["Played"] != true {
		t.Errorf("not marked played: %v", done["UserData"])
	}

	// And the next episode is what a television asks for next.
	next := c.items("/Shows/NextUp")
	if len(next) != 1 || next[0]["Name"] != "The Fire Ship" {
		t.Errorf("next up = %v", next)
	}
}

// Clients are not consistent about the case of the fixed segments, and a mux
// matches a path exactly.
func TestThePathIsReadWhicheverWayItIsSpelled(t *testing.T) {
	server, token, _ := library(t)
	c := call{t: t, server: server, token: token}
	id := c.me()

	for _, path := range []string{"/Users/" + id + "/Views", "/users/" + id + "/views"} {
		res, body := c.do(http.MethodGet, path, nil)
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s: %d %s", path, res.StatusCode, body)
		}
	}
}
