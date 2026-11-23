package artwork_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/artwork"
)

// A cache over its cap, holding one original and a pile of renditions made
// from it.
func overflowing(t *testing.T, cap int64) (*artwork.Cache, string) {
	t.Helper()
	root := t.TempDir()
	cache, err := artwork.New(root, cap, 1)
	if err != nil {
		t.Fatal(err)
	}

	// The original is the oldest file in the cache, which is what a flat
	// least-recently-used sweep would reach for first.
	original := filepath.Join(root, "originals", "chosen.img")
	write(t, original, 400)
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(original, old, old); err != nil {
		t.Fatal(err)
	}

	for i, name := range []string{"a_100x150.jpg", "b_200x300.jpg", "c_300x450.jpg"} {
		path := filepath.Join(root, "renditions", name)
		write(t, path, 400)
		// Newer than the original, and newest last.
		when := time.Now().Add(-time.Duration(3-i) * time.Hour)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	return cache, original
}

func write(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSweepSpendsRenditionsBeforeOriginals(t *testing.T) {
	// 1600 bytes held, 900 allowed: two files have to go.
	cache, original := overflowing(t, 900)

	freed, err := cache.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if freed < 700 {
		t.Fatalf("freed %d, not enough to get under the cap", freed)
	}

	// The original is the only copy of that picture: an item stores a cache
	// key and not the address it came from, so evicting it is a poster
	// nobody can get back. A rendition costs a decode to remake.
	if _, err := os.Stat(original); err != nil {
		t.Fatalf("the original was evicted while renditions remained: %v", err)
	}

	left, err := os.ReadDir(filepath.Join(cache.Root, "renditions"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 {
		t.Fatalf("%d renditions left, want the newest one only", len(left))
	}
	// Least recently used first within the group, so the newest survives.
	if left[0].Name() != "c_300x450.jpg" {
		t.Fatalf("kept %q, want the most recently used", left[0].Name())
	}
}

func TestSweepStillHonoursTheCap(t *testing.T) {
	// Smaller than every rendition put together, so the original has to go
	// as well: a cache that refuses to honour its own cap is not a cache.
	cache, original := overflowing(t, 300)

	if _, err := cache.Sweep(); err != nil {
		t.Fatal(err)
	}
	bytes, _, err := cache.Usage()
	if err != nil {
		t.Fatal(err)
	}
	if bytes > 300 {
		t.Fatalf("still holding %d bytes against a cap of 300", bytes)
	}
	if _, err := os.Stat(original); err == nil {
		t.Fatal("the cap was not honoured: the original survived")
	}
}

func TestSweepLeavesACacheUnderItsCapAlone(t *testing.T) {
	cache, original := overflowing(t, 1<<20)

	freed, err := cache.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if freed != 0 {
		t.Fatalf("freed %d from a cache that was under its cap", freed)
	}
	if _, err := os.Stat(original); err != nil {
		t.Fatal("a cache under its cap lost a file")
	}
}
