package scan_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/ffprobe"
	"github.com/inknironapps/bask-media-server/internal/scan"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// A remux with a cover image in it: the film is the first video stream, and
// the attached picture is the second. A rule about pixels has to read the
// first, or every film with embedded artwork answers "320 lines".
type fourK struct{}

func (fourK) Probe(context.Context, string) (ffprobe.Info, error) {
	return ffprobe.Info{
		Duration: 120 * time.Minute,
		Streams: []ffprobe.Stream{
			{Index: 0, Type: "video", Codec: "hevc", Width: 3840, Height: 2160},
			{Index: 1, Type: "audio", Codec: "eac3", Channels: 6},
			{Index: 2, Type: "video", Codec: "mjpeg", Width: 600, Height: 900},
		},
	}, nil
}

// The pixels are recorded on the file, which is what a smart collection of 4K
// films is asked of. Before this they were read for photographs only, and
// every film in the library claimed to be nought by nought.
func TestAScanRecordsHowBigThePictureIs(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Big Hill (2026).mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bms.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	section, err := db.CreateSection(ctx, "Films", domain.SectionMovies, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	scanner := &scan.Scanner{DB: db, Workers: 1, Prober: fourK{}}
	if _, err := scanner.Section(ctx, *section); err != nil {
		t.Fatal(err)
	}

	items, _, err := db.SectionItems(ctx, section.ID, domain.KindMovie, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d films", len(items))
	}
	parts, err := db.Parts(ctx, items[0].ID)
	if err != nil || len(parts) != 1 {
		t.Fatalf("parts = %v (%v)", parts, err)
	}
	if parts[0].Width != 3840 || parts[0].Height != 2160 {
		t.Fatalf("the file measures %dx%d, want 3840x2160", parts[0].Width, parts[0].Height)
	}

	// And the rule that reads it answers.
	made, err := db.CreateCollection(ctx, "In 4K", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetCollectionRules(ctx, made.ID, &domain.Smart{
		Rules: []domain.SmartRule{{Field: domain.SmartResolution, Op: domain.SmartAtLeast, Value: "2000"}},
	}); err != nil {
		t.Fatal(err)
	}
	held, _, err := db.CollectionItems(ctx, made.ID, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || held[0].Title != "Big Hill" {
		t.Fatalf("a 4K collection holds %v", held)
	}
}
