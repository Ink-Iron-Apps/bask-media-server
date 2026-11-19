package artwork_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/artwork"
)

// A thumbnail of a photograph is made from the file where it already is: a
// photo library is not copied into the cache, because that would double what
// a library costs on disk for a picture that is already there.
func TestAThumbnailIsMadeWithoutCopyingTheOriginal(t *testing.T) {
	root := t.TempDir()
	cache, err := artwork.New(filepath.Join(root, "cache"), 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	source := writeJPEG(t, filepath.Join(root, "IMG_0042.jpg"), 400, 200)

	file, err := cache.OpenFrom(context.Background(), source, 100, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	config, _, err := image.DecodeConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	// Fitted inside the box rather than stretched to it.
	if config.Width != 100 || config.Height != 50 {
		t.Errorf("thumbnail = %dx%d, want 100x50", config.Width, config.Height)
	}

	originals, err := os.ReadDir(filepath.Join(root, "cache", "originals"))
	if err != nil {
		t.Fatal(err)
	}
	if len(originals) != 0 {
		t.Errorf("%d originals were copied into the cache", len(originals))
	}
}

// Orientation 6 is a phone held upright. The pixels are landscape and the
// photograph is portrait, and a thumbnail that ignores that is every holiday
// picture lying on its side.
func TestAThumbnailIsTurnedTheWayTheFileAsks(t *testing.T) {
	root := t.TempDir()
	cache, err := artwork.New(filepath.Join(root, "cache"), 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	source := writeJPEG(t, filepath.Join(root, "upright.jpg"), 400, 200)

	file, err := cache.OpenFrom(context.Background(), source, 100, 100, 6)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	config, _, err := image.DecodeConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	if config.Width != 50 || config.Height != 100 {
		t.Errorf("thumbnail = %dx%d, want 50x100 — a quarter turn", config.Width, config.Height)
	}
}

// The same picture at the same size is rendered once, and the second request
// is a file that is already there.
func TestASecondRequestForTheSameThumbnailRendersNothing(t *testing.T) {
	root := t.TempDir()
	cache, err := artwork.New(filepath.Join(root, "cache"), 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	source := writeJPEG(t, filepath.Join(root, "IMG_0042.jpg"), 400, 200)

	for range 2 {
		file, err := cache.OpenFrom(context.Background(), source, 100, 100, 0)
		if err != nil {
			t.Fatal(err)
		}
		file.Close()
	}

	renditions, err := os.ReadDir(filepath.Join(root, "cache", "renditions"))
	if err != nil {
		t.Fatal(err)
	}
	if len(renditions) != 1 {
		t.Errorf("%d renditions for one picture at one size", len(renditions))
	}
}

// A cover that is already on this machine is copied in, because a key is a
// promise that the cache has the image: the folder it came from may be
// renamed or unplugged.
func TestAFolderImageIsAdoptedIntoTheCache(t *testing.T) {
	root := t.TempDir()
	cache, err := artwork.New(filepath.Join(root, "cache"), 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	source := writeJPEG(t, filepath.Join(root, "cover.jpg"), 300, 300)

	key, err := cache.Adopt(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}

	file, err := cache.Open(context.Background(), key, 0, 0)
	if err != nil {
		t.Fatalf("the cover did not outlive the folder it came from: %v", err)
	}
	file.Close()
}

// A cover inside a music file is stored by its bytes, so an album whose every
// track carries the same picture stores it once.
func TestAnEmbeddedCoverIsStoredOncePerPicture(t *testing.T) {
	cache, err := artwork.New(filepath.Join(t.TempDir(), "cache"), 0, 1)
	if err != nil {
		t.Fatal(err)
	}

	var picture bytes.Buffer
	if err := jpeg.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 60, 60)), nil); err != nil {
		t.Fatal(err)
	}

	first, err := cache.Store(picture.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	second, err := cache.Store(picture.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("the same picture stored twice got two keys: %s and %s", first, second)
	}
}

func writeJPEG(t *testing.T, path string, width, height int) string {
	t.Helper()
	picture := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			picture.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 40, A: 255})
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, picture, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
