package jobs_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/jobs"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// scheduled opens a server with one library of one film, so a scheduled scan
// has something real to walk.
func scheduled(t *testing.T) (*store.DB, *jobs.Runner, int64, context.Context) {
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

	runner := &jobs.Runner{DB: db}
	t.Cleanup(runner.Shutdown)
	return db, runner, section.ID, ctx
}

func TestUpcomingListsOnlyTheLibrariesThatAskedForASchedule(t *testing.T) {
	db, runner, section, ctx := scheduled(t)

	upcoming, err := runner.Upcoming(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(upcoming) != 0 {
		t.Fatalf("upcoming = %v with no schedule set", upcoming)
	}

	if err := db.SetSectionSettings(ctx, section, domain.SectionSettings{ScanEvery: 360}); err != nil {
		t.Fatal(err)
	}
	upcoming, err = runner.Upcoming(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(upcoming) != 1 {
		t.Fatalf("upcoming = %v", upcoming)
	}
	if upcoming[0].Every != 6*time.Hour {
		t.Fatalf("every = %v", upcoming[0].Every)
	}
	// Never swept, so due now rather than in 1970: the page says "now" and
	// the scheduler agrees with it.
	if upcoming[0].Next.After(time.Now().UTC().Add(time.Minute)) {
		t.Fatalf("next = %v, want now", upcoming[0].Next)
	}
}
