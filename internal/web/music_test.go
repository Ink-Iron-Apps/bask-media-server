package web_test

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/artwork"
	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/scan"
	"github.com/inknironapps/bask-media-server/internal/store"
	"github.com/inknironapps/bask-media-server/internal/web"
)

// listening is a library of music and photographs, scanned from real files,
// with a browser session signed in against it.
type listening struct {
	*harness

	musicSection int64
	photoSection int64
	artistID     int64
	albumID      int64
	albumRoot    int64 // the outermost photo album
}

func newListening(t *testing.T) *listening {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bms.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	viewer, err := db.CreateUser(ctx, "kitchen-tv", false)
	if err != nil {
		t.Fatal(err)
	}
	token, err := db.IssueToken(ctx, viewer.ID, "kitchen")
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	music := filepath.Join(root, "Music")
	pictures := filepath.Join(root, "Pictures")
	write := func(path string, body []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	album := filepath.Join(music, "Miles Davis", "Kind of Blue (1959)")
	write(filepath.Join(album, "01 - So What.flac"), []byte("audio"))
	write(filepath.Join(album, "02 - Freddie Freeloader.flac"), []byte("audio"))
	write(filepath.Join(album, "cover.jpg"), aJPEG(t, 120, 120))
	write(filepath.Join(pictures, "2016", "IMG_0042.jpg"), aJPEG(t, 60, 40))
	write(filepath.Join(pictures, "2016", "IMG_0043.jpg"), aJPEG(t, 40, 60))

	musicSection, err := db.CreateSection(ctx, "Music", domain.SectionMusic, []string{music})
	if err != nil {
		t.Fatal(err)
	}
	photoSection, err := db.CreateSection(ctx, "Photos", domain.SectionPhotos, []string{pictures})
	if err != nil {
		t.Fatal(err)
	}

	images, err := artwork.New(filepath.Join(root, "artwork"), 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	scanner := &scan.Scanner{DB: db, Workers: 2, Artwork: images}
	for _, section := range []*domain.Section{musicSection, photoSection} {
		if _, err := scanner.Section(ctx, *section); err != nil {
			t.Fatal(err)
		}
	}

	viewing := &web.Server{Store: db, Name: "Bask Media Server", Artwork: images}
	server := httptest.NewServer(viewing.Handler())
	t.Cleanup(server.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}

	out := &listening{
		harness: &harness{
			t: t, db: db, token: token, userID: viewer.ID,
			server: server, client: &http.Client{Jar: jar},
		},
		musicSection: musicSection.ID,
		photoSection: photoSection.ID,
	}

	artists, _, err := db.SectionItems(ctx, musicSection.ID, domain.KindArtist, store.Page{})
	if err != nil || len(artists) != 1 {
		t.Fatalf("artists = %v (%v)", artists, err)
	}
	out.artistID = artists[0].ID
	albums, _, err := db.Children(ctx, out.artistID, store.Page{})
	if err != nil || len(albums) != 1 {
		t.Fatalf("albums = %v (%v)", albums, err)
	}
	out.albumID = albums[0].ID

	photoAlbums, _, err := db.SectionItems(ctx, photoSection.ID, domain.KindPhotoAlbum, store.Page{})
	if err != nil || len(photoAlbums) != 1 {
		t.Fatalf("photo albums = %v (%v)", photoAlbums, err)
	}
	out.albumRoot = photoAlbums[0].ID
	return out
}

func aJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := jpeg.Encode(&out, image.NewRGBA(image.Rect(0, 0, width, height)), nil); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// The page needs one request per screen: a library of artists, an artist's
// albums, an album's tracks. Anything more and opening an album is a round
// trip per row.
func TestTheInterfaceOpensAMusicLibrary(t *testing.T) {
	h := newListening(t)
	h.signIn()

	res, body := h.call(http.MethodGet, "/web/api/sections/"+strconv.FormatInt(h.musicSection, 10), nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %v", res.StatusCode, body)
	}
	if body["kind"] != "music" {
		t.Errorf("kind = %v", body["kind"])
	}
	artists := items(t, body, "items")
	if len(artists) != 1 || artists[0]["title"] != "Miles Davis" {
		t.Fatalf("artists = %v", artists)
	}

	res, album := h.call(http.MethodGet, "/web/api/items/"+strconv.FormatInt(h.albumID, 10), nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %v", res.StatusCode, album)
	}
	item, _ := album["item"].(map[string]any)
	if item["kind"] != "album" || item["title"] != "Kind of Blue" {
		t.Fatalf("album = %v", item)
	}
	// The artist, so the page can show it without a second request.
	if item["subtitle"] != "Miles Davis" {
		t.Errorf("subtitle = %v", item["subtitle"])
	}
	if item["poster"] != true {
		t.Error("the cover beside the album should be offered")
	}

	tracks := items(t, album, "children")
	if len(tracks) != 2 {
		t.Fatalf("tracks = %v", tracks)
	}
	if tracks[0]["kind"] != "track" || tracks[0]["index"] != float64(1) {
		t.Errorf("first track = %v", tracks[0])
	}
	if tracks[0]["title"] != "So What" {
		t.Errorf("title = %v", tracks[0]["title"])
	}
}

// A photo album is a wall of pictures and the folders among them, and every
// picture has to carry the shape the page will draw it at.
func TestTheInterfaceOpensAPhotoAlbum(t *testing.T) {
	h := newListening(t)
	h.signIn()

	res, body := h.call(http.MethodGet, "/web/api/items/"+strconv.FormatInt(h.albumRoot, 10), nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %v", res.StatusCode, body)
	}
	item, _ := body["item"].(map[string]any)
	if item["kind"] != "photoalbum" || item["title"] != "2016" {
		t.Fatalf("album = %v", item)
	}

	pictures := items(t, body, "children")
	if len(pictures) != 2 {
		t.Fatalf("pictures = %v", pictures)
	}
	first := pictures[0]
	if first["kind"] != "photo" {
		t.Errorf("kind = %v", first["kind"])
	}
	if first["takenAt"] == nil {
		t.Error("a photograph has to say when it was taken, or an album has no order")
	}
	if first["poster"] != true {
		t.Error("a photograph is its own thumbnail")
	}
	if first["width"] == nil || first["height"] == nil {
		t.Errorf("no dimensions on %v", first)
	}
}

// A thumbnail at the size the wall asked for, made from the file in the
// library: no copy of a photo library is kept anywhere.
func TestAPhotographIsServedAtTheSizeAsked(t *testing.T) {
	h := newListening(t)
	h.signIn()

	ctx := context.Background()
	pictures, _, err := h.db.Children(ctx, h.albumRoot, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(pictures[0].ID, 10)

	for _, path := range []string{
		"/web/image/" + id + "?w=200&h=200",
		"/web/image/" + id, // the original, which is what one picture full screen wants
	} {
		req, err := http.NewRequest(http.MethodGet, h.server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := h.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body := make([]byte, 4)
		n, _ := res.Body.Read(body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d", path, res.StatusCode)
		}
		// A JPEG, whichever way it was produced.
		if n < 2 || body[0] != 0xFF || body[1] != 0xD8 {
			t.Errorf("GET %s did not return an image: % x", path, body[:n])
		}
	}
}

// A track plays like anything else: the client says what it can decode, the
// server answers with a URL, and a browser that can read FLAC gets the file.
func TestATrackDirectPlaysWhereTheBrowserCanReadIt(t *testing.T) {
	h := newListening(t)
	h.signIn()

	ctx := context.Background()
	tracks, _, err := h.db.Children(ctx, h.albumID, store.Page{})
	if err != nil {
		t.Fatal(err)
	}

	res, body := h.call(http.MethodPost, "/web/api/play", map[string]any{
		"itemId": tracks[0].ID,
		"capabilities": map[string]any{
			"containers": []string{"flac", "mp3", "mp4"},
			"audio":      []string{"flac", "aac"},
		},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %v", res.StatusCode, body)
	}
	if body["mode"] != "direct" {
		t.Errorf("mode = %v (%v), want a direct play", body["mode"], body["why"])
	}
	// No part id was sent, because a track list does not carry one: the
	// server plays the file the item has.
	if body["url"] == nil {
		t.Error("no url to play")
	}
}

// What the library holds decides what the top level is, and a library whose
// photographs are all in folders must not list every folder at once.
func TestOnlyTheOutermostPhotoAlbumsAreTheTopLevel(t *testing.T) {
	h := newListening(t)
	h.signIn()

	res, body := h.call(http.MethodGet, "/web/api/sections/"+strconv.FormatInt(h.photoSection, 10), nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %v", res.StatusCode, body)
	}
	albums := items(t, body, "items")
	if len(albums) != 1 || albums[0]["title"] != "2016" {
		t.Fatalf("albums = %v", albums)
	}
	if body["total"] != float64(1) {
		t.Errorf("total = %v", body["total"])
	}
}

// Nothing about a music library involves a metadata provider: TMDB has never
// heard of an album, and asking it about one spends a request per row on
// every pass for ever.
func TestMusicAndPhotographsAreNotSentToTheFilmProvider(t *testing.T) {
	h := newListening(t)

	needs, err := h.db.NeedsMetadata(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range needs {
		switch item.Kind {
		case domain.KindArtist, domain.KindAlbum, domain.KindTrack,
			domain.KindPhotoAlbum, domain.KindPhoto:
			t.Errorf("%s %q was queued for the film provider", item.Kind, item.Title)
		}
	}
}
