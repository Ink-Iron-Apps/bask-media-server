package jobs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// These are in the package rather than beside it because what is under test
// is one sweep's decision, and the loop that calls it is a ticker. Driving
// the loop from outside would mean sleeping for a tick and asserting on a
// timer rather than on the decision.

func sweepable(t *testing.T) (*store.DB, *Runner, int64, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bms.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Big Hill (2026).mkv"), make([]byte, 512), 0o644); err != nil {
		t.Fatal(err)
	}
	section, err := db.CreateSection(ctx, "Films", domain.SectionMovies, []string{root})
	if err != nil {
		t.Fatal(err)
	}

	runner := &Runner{DB: db}
	t.Cleanup(runner.Shutdown)
	return db, runner, section.ID, ctx
}

func TestASweepScansALibraryThatIsDue(t *testing.T) {
	db, runner, section, ctx := sweepable(t)
	if err := db.SetSectionSettings(ctx, section, domain.SectionSettings{ScanEvery: 15}); err != nil {
		t.Fatal(err)
	}

	before, err := db.Section(ctx, section)
	if err != nil {
		t.Fatal(err)
	}
	runner.sweep(ctx, runner.logger())

	after, err := db.Section(ctx, section)
	if err != nil {
		t.Fatal(err)
	}
	// The visit is recorded before the scan starts, so a library whose scan
	// fails is retried at its own pace rather than every minute.
	if !after.SweptAt.After(before.SweptAt) {
		t.Fatalf("the visit was not recorded: %v then %v", before.SweptAt, after.SweptAt)
	}

	var scans int
	for _, job := range runner.Jobs() {
		if job.Kind == KindScan {
			scans++
		}
	}
	if scans != 1 {
		t.Fatalf("%d scans were started, want one", scans)
	}

	// And it is no longer due, so the next sweep leaves it alone.
	due, err := db.DueForScan(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("still due straight after a sweep: %v", due)
	}
}

func TestASweepLeavesALibraryWithNoScheduleAlone(t *testing.T) {
	db, runner, section, ctx := sweepable(t)

	before, err := db.Section(ctx, section)
	if err != nil {
		t.Fatal(err)
	}
	runner.sweep(ctx, runner.logger())

	after, err := db.Section(ctx, section)
	if err != nil {
		t.Fatal(err)
	}
	if !after.SweptAt.Equal(before.SweptAt) {
		t.Fatal("a library with no schedule was marked as visited")
	}
	if len(runner.Jobs()) != 0 {
		t.Fatalf("a library with no schedule had %d jobs started", len(runner.Jobs()))
	}
}

func TestASweepStopsWhenTheServerIsGoingDown(t *testing.T) {
	db, runner, section, ctx := sweepable(t)
	if err := db.SetSectionSettings(ctx, section, domain.SectionSettings{ScanEvery: 15}); err != nil {
		t.Fatal(err)
	}

	// A cancelled context is a server on its way down: it starts nothing and
	// records nothing, so the library is still due when the next one comes
	// up.
	dead, cancel := context.WithCancel(ctx)
	cancel()
	runner.sweep(dead, runner.logger())

	if len(runner.Jobs()) != 0 {
		t.Fatal("a sweep started work while the server was shutting down")
	}
	due, err := db.DueForScan(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("the library was not still due: %v", due)
	}
}
