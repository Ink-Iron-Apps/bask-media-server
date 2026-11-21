package plexapi_test

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/artwork"
	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/ffprobe"
	"github.com/inknironapps/bask-media-server/internal/plexapi"
	"github.com/inknironapps/bask-media-server/internal/scan"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// tagProber answers with the tags a music file would carry.
type tagProber struct{}

func (tagProber) Probe(_ context.Context, path string) (ffprobe.Info, error) {
	name := filepath.Base(path)
	info := ffprobe.Info{
		Duration: 5 * time.Minute,
		Streams:  []ffprobe.Stream{{Index: 0, Type: "audio", Codec: "flac", Channels: 2}},
	}
	switch name {
	case "01 - So What.flac":
		info.Tags = ffprobe.Tags{
			Title: "So What", Artist: "Miles Davis", AlbumArtist: "Miles Davis",
			Album: "Kind of Blue", Date: "1959", Track: 1,
		}
	case "02 - Freddie Freeloader.flac":
		info.Tags = ffprobe.Tags{
			Title: "Freddie Freeloader", Artist: "The Sextet", AlbumArtist: "Miles Davis",
			Album: "Kind of Blue", Date: "1959", Track: 2,
		}
	}
	return info, nil
}

// mixedLibrary is a server with a music library and a photo library, scanned
// from real files.
func mixedLibrary(t *testing.T) (*plexapi.Server, string, *store.DB) {
	t.Helper()
	ctx := context.Background()
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
	write(filepath.Join(album, "01 - So What.flac"), make([]byte, 9_000_000))
	write(filepath.Join(album, "02 - Freddie Freeloader.flac"), make([]byte, 9_000_000))
	write(filepath.Join(album, "cover.jpg"), smallJPEG(t, 300, 300))

	write(filepath.Join(pictures, "2016", "Cornwall", "IMG_0042.jpg"), smallJPEG(t, 60, 40))
	write(filepath.Join(pictures, "2016", "Cornwall", "IMG_0043.jpg"), smallJPEG(t, 40, 60))

	db, err := store.Open(ctx, filepath.Join(root, "bms.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

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
	scanner := &scan.Scanner{DB: db, Workers: 2, Prober: tagProber{}, Artwork: images}
	for _, section := range []*domain.Section{musicSection, photoSection} {
		if _, err := scanner.Section(ctx, *section); err != nil {
			t.Fatalf("scan %s: %v", section.Title, err)
		}
	}

	user, err := db.CreateUser(ctx, "owner", true)
	if err != nil {
		t.Fatal(err)
	}
	token, err := db.IssueToken(ctx, user.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	return &plexapi.Server{
		Store: db, Name: "BMS test", MachineID: "machine-1", Version: "test", Artwork: images,
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

func sectionIDFor(t *testing.T, srv *plexapi.Server, token, title string) string {
	t.Helper()
	mc := get(t, srv, token, "/library/sections")
	dirs, _ := mc["Directory"].([]any)
	for _, raw := range dirs {
		dir, _ := raw.(map[string]any)
		if dir["title"] == title {
			return dir["key"].(string)
		}
	}
	t.Fatalf("no section %q", title)
	return ""
}

// A client decides which interface to show from the section's type, so these
// strings are the wire and not an internal choice.
func TestAMusicLibraryIsAnArtistLibraryAndPhotosArePhotos(t *testing.T) {
	srv, token, _ := mixedLibrary(t)

	mc := get(t, srv, token, "/library/sections")

	dirs, _ := mc["Directory"].([]any)
	kinds := map[string]string{}
	for _, raw := range dirs {
		dir, _ := raw.(map[string]any)
		kinds[dir["title"].(string)] = dir["type"].(string)
	}
	if kinds["Music"] != "artist" {
		t.Errorf("music library type = %q, want artist", kinds["Music"])
	}
	if kinds["Photos"] != "photo" {
		t.Errorf("photo library type = %q, want photo", kinds["Photos"])
	}
}

func TestAnArtistsAlbumsAndItsTracks(t *testing.T) {
	srv, token, _ := mixedLibrary(t)
	id := sectionIDFor(t, srv, token, "Music")

	top := get(t, srv, token, "/library/sections/"+id+"/all")
	artists, _ := top["Directory"].([]any)
	if len(artists) != 1 {
		t.Fatalf("got %d artists: %v", len(artists), top)
	}
	artist, _ := artists[0].(map[string]any)
	if artist["title"] != "Miles Davis" || artist["type"] != "artist" {
		t.Fatalf("artist = %v", artist)
	}
	// A client descends into an artist, so the key has to be its children.
	if artist["key"] != "/library/metadata/"+artist["ratingKey"].(string)+"/children" {
		t.Errorf("key = %v", artist["key"])
	}
	if artist["childCount"] != float64(1) {
		t.Errorf("childCount = %v, want 1 album", artist["childCount"])
	}

	albums := get(t, srv, token, "/library/metadata/"+artist["ratingKey"].(string)+"/children")
	if albums["viewGroup"] != "album" {
		t.Errorf("viewGroup = %v", albums["viewGroup"])
	}
	list, _ := albums["Directory"].([]any)
	if len(list) != 1 {
		t.Fatalf("got %d albums", len(list))
	}
	album, _ := list[0].(map[string]any)
	if album["type"] != "album" || album["title"] != "Kind of Blue" {
		t.Fatalf("album = %v", album)
	}
	if album["year"] != float64(1959) {
		t.Errorf("year = %v", album["year"])
	}
	if album["parentTitle"] != "Miles Davis" {
		t.Errorf("parentTitle = %v, want the artist", album["parentTitle"])
	}
	if album["thumb"] == nil {
		t.Error("the cover beside the album was not offered")
	}

	tracks := get(t, srv, token, "/library/metadata/"+album["ratingKey"].(string)+"/children")
	if tracks["viewGroup"] != "track" {
		t.Errorf("viewGroup = %v", tracks["viewGroup"])
	}
	rows, _ := tracks["Metadata"].([]any)
	if len(rows) != 2 {
		t.Fatalf("got %d tracks: %v", len(rows), tracks)
	}
	first, _ := rows[0].(map[string]any)
	if first["type"] != "track" {
		t.Errorf("type = %v, want track", first["type"])
	}
	// A music client puts the album and the artist on every row, and reads
	// them from here rather than asking twice.
	if first["parentTitle"] != "Kind of Blue" || first["grandparentTitle"] != "Miles Davis" {
		t.Errorf("row context = %v / %v", first["parentTitle"], first["grandparentTitle"])
	}
	if first["index"] != float64(1) || first["title"] != "So What" {
		t.Errorf("first track = %v", first)
	}
	if first["thumb"] == nil {
		t.Error("a track with no cover of its own should show the album's")
	}

	media, _ := first["Media"].([]any)
	if len(media) != 1 {
		t.Fatalf("no media on a track: %v", first)
	}
	m, _ := media[0].(map[string]any)
	if m["audioCodec"] != "flac" {
		t.Errorf("audioCodec = %v", m["audioCodec"])
	}
	if m["bitrate"] == nil || m["bitrate"].(float64) <= 0 {
		t.Errorf("bitrate = %v", m["bitrate"])
	}

	// The second track is by somebody else, which is the only thing that
	// makes a compilation readable.
	second, _ := rows[1].(map[string]any)
	if second["originalTitle"] != "The Sextet" {
		t.Errorf("originalTitle = %v, want the performer", second["originalTitle"])
	}
}

func TestAnAlbumOfPhotographs(t *testing.T) {
	srv, token, _ := mixedLibrary(t)
	id := sectionIDFor(t, srv, token, "Photos")

	top := get(t, srv, token, "/library/sections/"+id+"/all")
	albums, _ := top["Directory"].([]any)
	if len(albums) != 1 {
		t.Fatalf("got %d top-level albums: %v", len(albums), top)
	}
	year, _ := albums[0].(map[string]any)
	if year["type"] != "photoalbum" || year["title"] != "2016" {
		t.Fatalf("album = %v", year)
	}
	// An album of photographs has no artwork of its own: its cover is one of
	// the pictures inside it.
	if year["thumb"] == nil {
		t.Error("an album should offer a thumbnail")
	}

	inside := get(t, srv, token, "/library/metadata/"+year["ratingKey"].(string)+"/children")
	nested, _ := inside["Directory"].([]any)
	if len(nested) != 1 {
		t.Fatalf("got %d nested albums", len(nested))
	}
	cornwall, _ := nested[0].(map[string]any)

	photos := get(t, srv, token, "/library/metadata/"+cornwall["ratingKey"].(string)+"/children")
	if photos["viewGroup"] != "photo" {
		t.Errorf("viewGroup = %v", photos["viewGroup"])
	}
	rows, _ := photos["Metadata"].([]any)
	if len(rows) != 2 {
		t.Fatalf("got %d photographs: %v", len(rows), photos)
	}
	photo, _ := rows[0].(map[string]any)
	if photo["type"] != "photo" {
		t.Errorf("type = %v", photo["type"])
	}
	if photo["originallyAvailableAt"] == nil {
		t.Error("a photograph should say when it was taken")
	}
	media, _ := photo["Media"].([]any)
	m, _ := media[0].(map[string]any)
	if m["width"] == nil || m["height"] == nil {
		t.Errorf("a photograph's dimensions are its content: %v", m)
	}
	parts, _ := m["Part"].([]any)
	part, _ := parts[0].(map[string]any)
	if part["key"] == nil {
		t.Error("a photograph has to be fetchable")
	}
}

// A photograph is served from the library, where it already is: there is no
// copy of it in the artwork cache, and only the thumbnail is kept.
func TestAPhotographIsItsOwnThumbnail(t *testing.T) {
	srv, token, db := mixedLibrary(t)
	ctx := context.Background()

	sections, err := db.Sections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var photoSection domain.Section
	for _, section := range sections {
		if section.Kind == domain.SectionPhotos {
			photoSection = section
		}
	}
	albums, _, err := db.SectionItems(ctx, photoSection.ID, domain.KindPhotoAlbum, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	// Down through the album tree to the first photograph, because a photo
	// library is as deep as somebody's folders go.
	var photo domain.Item
	queue := albums
	for len(queue) > 0 && photo.ID == 0 {
		var next []domain.Item
		for _, album := range queue {
			children, _, err := db.Children(ctx, album.ID, store.Page{Size: 50})
			if err != nil {
				t.Fatal(err)
			}
			for _, child := range children {
				if child.Kind == domain.KindPhoto {
					photo = child
					break
				}
				next = append(next, child)
			}
		}
		queue = next
	}
	if photo.ID == 0 {
		t.Fatal("no photograph in the library")
	}

	for _, path := range []string{
		"/library/metadata/" + strconv.FormatInt(photo.ID, 10) + "/thumb?width=200",
		"/photo/:/transcode?width=200&url=" + "/library/metadata/" + strconv.FormatInt(photo.ID, 10) + "/thumb",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Plex-Token", token)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d", path, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("GET %s: no image", path)
		}
	}
}
