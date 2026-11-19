package scan_test

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/scan"
	"github.com/inknironapps/bask-media-server/internal/store"
)

func TestAPhotographsPlaceIsItsFolders(t *testing.T) {
	photo, ok := scan.ParsePhoto("/pictures", "/pictures/2016/Cornwall/IMG_0042.jpg")
	if !ok {
		t.Fatal("not recognised")
	}
	if len(photo.Albums) != 2 || photo.Albums[0] != "2016" || photo.Albums[1] != "Cornwall" {
		t.Errorf("albums = %v", photo.Albums)
	}
	// The filename is the title, as written: "IMG 0042" is not an improvement
	// on "IMG_0042", it is a different name for the same file.
	if photo.Title != "IMG_0042" {
		t.Errorf("title = %q", photo.Title)
	}
}

// A file at the top of the library still has to be in an album, because a
// library's top level is a list of albums.
func TestALoosePhotographGetsTheLibrarysOwnName(t *testing.T) {
	photo, ok := scan.ParsePhoto("/mnt/Pictures", "/mnt/Pictures/scan001.png")
	if !ok {
		t.Fatal("not recognised")
	}
	if len(photo.Albums) != 1 || photo.Albums[0] != "Pictures" {
		t.Errorf("albums = %v", photo.Albums)
	}
}

// cover.jpg is an album's cover in a music library and somebody's photograph
// in a photo library. The same filename, two answers, and only the library it
// is in can say which.
func TestACoverNameIsArtworkInMusicAndAPhotographInPhotos(t *testing.T) {
	if !scan.IsArtwork("/music/Band/Album/cover.jpg") {
		t.Error("cover.jpg beside music is the album's cover")
	}
	if !scan.IsArtwork("/music/Band/Album/folder-1.png") {
		t.Error("folder-1.png is artwork too")
	}
	if scan.IsArtwork("/music/Band/Album/IMG_0042.jpg") {
		t.Error("a photograph in a music folder is not the cover")
	}
	// ParsePhoto does not consult IsArtwork at all, which is the point.
	if _, ok := scan.ParsePhoto("/pictures", "/pictures/Album covers/cover.jpg"); !ok {
		t.Error("in a photo library, cover.jpg is a photograph")
	}
}

// jpegOf writes a real JPEG of a given size, because the scanner reads the
// format's own header for the dimensions.
func jpegOf(t *testing.T, width, height int) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := jpeg.Encode(&out, image.NewRGBA(image.Rect(0, 0, width, height)), nil); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestAPhotoScanBuildsTheFolderTree(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "Pictures")

	files := map[string][]byte{
		"2016/Cornwall/IMG_0042.jpg": jpegOf(t, 60, 40),
		"2016/Cornwall/IMG_0043.jpg": jpegOf(t, 40, 60),
		"2016/Birthday/cake.png":     jpegOf(t, 20, 20), // a PNG name over JPEG bytes is still a picture
		"loose.jpg":                  jpegOf(t, 10, 10),
		".thumbnails/IMG_0042.jpg":   jpegOf(t, 8, 8), // a cache, not a library
		"2016/Cornwall/notes.txt":    []byte("not a picture"),
	}
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Taken in a known order, so the album's order can be checked.
	older := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, "2016", "Cornwall", "IMG_0043.jpg"), older, older); err != nil {
		t.Fatal(err)
	}

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bms.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	section, err := db.CreateSection(ctx, "Photos", domain.SectionPhotos, []string{root})
	if err != nil {
		t.Fatal(err)
	}

	// No prober: a photo library needs no ffprobe at all, which is worth
	// pinning — nothing here should depend on a subprocess.
	scanner := &scan.Scanner{DB: db, Workers: 2}
	result, err := scanner.Section(ctx, *section)
	if err != nil {
		t.Fatal(err)
	}
	if result.Files != 4 {
		t.Errorf("wrote %d files, want the four pictures outside the thumbnail cache", result.Files)
	}

	albums, _, err := db.SectionItems(ctx, section.ID, domain.KindPhotoAlbum, store.Page{Size: 50})
	if err != nil {
		t.Fatal(err)
	}
	top := map[string]domain.Item{}
	for _, album := range albums {
		if album.ParentID == nil {
			top[album.Title] = album
		}
	}
	if len(top) != 2 {
		t.Fatalf("top-level albums = %v", top)
	}
	if _, found := top["Pictures"]; !found {
		t.Error("the loose photograph should be in an album named after the library")
	}

	year, found := top["2016"]
	if !found {
		t.Fatal("no album for the year folder")
	}
	inside, _, err := db.Children(ctx, year.ID, store.Page{Size: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(inside) != 2 {
		t.Fatalf("got %d albums under 2016: %+v", len(inside), inside)
	}

	var cornwall domain.Item
	for _, album := range inside {
		if album.Title == "Cornwall" {
			cornwall = album
		}
	}
	pictures, _, err := db.Children(ctx, cornwall.ID, store.Page{Size: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(pictures) != 2 {
		t.Fatalf("got %d photographs", len(pictures))
	}
	// Oldest first: an album of photographs is in the order they happened,
	// not the order of their names.
	if pictures[0].Title != "IMG_0043" {
		t.Errorf("first photograph = %q, want the one taken earlier", pictures[0].Title)
	}
	if pictures[0].TakenAt.IsZero() {
		t.Error("a photograph with no EXIF still has the file's own time")
	}
	if len(pictures[0].Parts) != 1 {
		t.Fatalf("a photograph is a file: %+v", pictures[0].Parts)
	}
	if pictures[0].Parts[0].Width != 40 || pictures[0].Parts[0].Height != 60 {
		t.Errorf("dimensions = %dx%d, want 40x60", pictures[0].Parts[0].Width, pictures[0].Parts[0].Height)
	}

	// The cover of an album is the earliest picture anywhere inside it, which
	// is the only cover a folder of photographs has.
	cover, err := db.AlbumCover(ctx, year.ID)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(cover.Path) != "IMG_0043.jpg" {
		t.Errorf("cover = %s", cover.Path)
	}
}

// A folder that is emptied is a folder that goes, however deep it was.
func TestAnEmptiedPhotoFolderIsPruned(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "Pictures")
	deep := filepath.Join(root, "2016", "Cornwall", "Day one")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "one.jpg"), jpegOf(t, 10, 10), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "keep.jpg"), jpegOf(t, 10, 10), 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bms.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	section, err := db.CreateSection(ctx, "Photos", domain.SectionPhotos, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	scanner := &scan.Scanner{DB: db, Workers: 1}
	if _, err := scanner.Section(ctx, *section); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(deep, "one.jpg")); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Section(ctx, *section); err != nil {
		t.Fatal(err)
	}

	albums, _, err := db.SectionItems(ctx, section.ID, domain.KindPhotoAlbum, store.Page{Size: 50})
	if err != nil {
		t.Fatal(err)
	}
	// Three levels of empty folders went, and the one with a picture in it
	// stayed.
	if len(albums) != 1 || albums[0].Title != "Pictures" {
		t.Errorf("albums after the prune = %+v", albums)
	}
}
