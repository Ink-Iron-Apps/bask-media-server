package jobs_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/ffprobe"
	"github.com/inknironapps/bask-media-server/internal/jobs"
	"github.com/inknironapps/bask-media-server/internal/store"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("modernc.org/sqlite.(*conn).interruptOnDone.func1"),
	)
}

// prober stands in for ffprobe. held blocks every probe until it is closed,
// which is how a test gets a pass that is reliably still running.
type prober struct{ held chan struct{} }

func (p prober) Probe(ctx context.Context, _ string) (ffprobe.Info, error) {
	if p.held != nil {
		select {
		case <-p.held:
		case <-ctx.Done():
			return ffprobe.Info{}, ctx.Err()
		}
	}
	return ffprobe.Info{Duration: 42 * time.Minute}, nil
}

// library writes a small film collection and a section pointing at it.
func library(t *testing.T, files int) (*store.DB, domain.Section) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	for i := 1; i <= files; i++ {
		name := filepath.Join(root, "Film "+string(rune('A'+i-1))+" (200"+string(rune('0'+i))+").mkv")
		if err := os.WriteFile(name, make([]byte, 512), 0o644); err != nil {
			t.Fatal(err)
		}
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
	return db, *section
}

// settled waits for a job to stop running, so a test asserts on an outcome
// rather than on a sleep.
func settled(t *testing.T, runner *jobs.Runner, id int64) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := runner.Job(id)
		if !ok {
			t.Fatalf("job %d disappeared", id)
		}
		if !job.Running() {
			return job
		}
		time.Sleep(5 * time.Millisecond)
	}
	job, _ := runner.Job(id)
	t.Fatalf("job %d is still running: %+v", id, job)
	return job
}

func TestAScanJobWritesTheLibraryAndSaysWhatItDid(t *testing.T) {
	ctx := context.Background()
	db, section := library(t, 3)
	runner := &jobs.Runner{DB: db, Prober: prober{}}
	t.Cleanup(runner.Shutdown)

	started, err := runner.Start(ctx, jobs.KindScan, section.ID)
	if err != nil {
		t.Fatal(err)
	}
	if started.State != jobs.StateRunning || started.Target != "Films" {
		t.Fatalf("started = %+v", started)
	}

	job := settled(t, runner, started.ID)
	if job.State != jobs.StateDone {
		t.Fatalf("job = %+v", job)
	}
	if job.Done != 3 {
		t.Errorf("job reported %d files, want 3: %+v", job.Done, job)
	}
	// The note is the sentence somebody reads on the page, so it is worth
	// pinning that it is a sentence.
	if job.Note != "3 files in 1 library" {
		t.Errorf("job note = %q", job.Note)
	}

	// The job is only worth anything if the library is now in the database.
	items, total, err := db.SectionItems(ctx, section.ID, domain.KindMovie, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(items) != 3 {
		t.Errorf("got %d of %d items, want 3", len(items), total)
	}
}

// Two scans at once would fight over SQLite's single writer and find the same
// files twice. The answer is to refuse, not to queue.
func TestASecondPassOfTheSameKindIsRefused(t *testing.T) {
	ctx := context.Background()
	db, section := library(t, 2)
	held := make(chan struct{})
	runner := &jobs.Runner{DB: db, Prober: prober{held: held}}
	t.Cleanup(runner.Shutdown)

	first, err := runner.Start(ctx, jobs.KindScan, section.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Start(ctx, jobs.KindScan, section.ID); !errors.Is(err, jobs.ErrBusy) {
		t.Fatalf("second scan returned %v, want ErrBusy", err)
	}

	close(held)
	if job := settled(t, runner, first.ID); job.State != jobs.StateDone {
		t.Fatalf("first job = %+v", job)
	}
	// Refusing the second must not have left a job behind to explain.
	if list := runner.Jobs(); len(list) != 1 {
		t.Errorf("got %d jobs, want only the one that ran: %+v", len(list), list)
	}
}

// A pass runs for hours on a cold library, so stopping one has to work, and
// what it has already written has to stay written.
func TestStoppingAPassLeavesWhatItWrote(t *testing.T) {
	ctx := context.Background()
	db, section := library(t, 2)
	held := make(chan struct{})
	runner := &jobs.Runner{DB: db, Prober: prober{held: held}}
	t.Cleanup(runner.Shutdown)

	started, err := runner.Start(ctx, jobs.KindScan, section.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Cancel(started.ID); err != nil {
		t.Fatal(err)
	}

	job := settled(t, runner, started.ID)
	if job.State != jobs.StateCanceled {
		t.Fatalf("job = %+v", job)
	}
	close(held)

	// The section is still there and still readable: a cancelled scan is a
	// library that has fewer files in it than it will later, not a broken one.
	if _, err := db.Section(ctx, section.ID); err != nil {
		t.Fatalf("section after a cancelled scan: %v", err)
	}
}

// A job that cannot possibly run must be refused before it exists. A row in
// the history that failed instantly tells somebody nothing.
func TestAPassWithNoToolIsRefusedBeforeItStarts(t *testing.T) {
	ctx := context.Background()
	db, _ := library(t, 1)
	runner := &jobs.Runner{DB: db} // no ffmpeg, no provider
	t.Cleanup(runner.Shutdown)

	for _, kind := range []jobs.Kind{jobs.KindMarkers, jobs.KindEnrich} {
		_, err := runner.Start(ctx, kind, 0)
		if !errors.Is(err, jobs.ErrUnavailable) {
			t.Errorf("%s returned %v, want ErrUnavailable", kind, err)
		}
		if err := runner.Can(ctx, kind); !errors.Is(err, jobs.ErrUnavailable) {
			t.Errorf("Can(%s) = %v, want ErrUnavailable", kind, err)
		}
	}
	if list := runner.Jobs(); len(list) != 0 {
		t.Errorf("a refusal created %d jobs: %+v", len(list), list)
	}
}

// The history is in memory and this process runs for months, so it has to be
// bounded: rule 3 of the resource budget.
func TestTheHistoryIsBounded(t *testing.T) {
	ctx := context.Background()
	db, section := library(t, 1)
	runner := &jobs.Runner{DB: db, Prober: prober{}, History: 2}
	t.Cleanup(runner.Shutdown)

	for range 5 {
		started, err := runner.Start(ctx, jobs.KindScan, section.ID)
		if err != nil {
			t.Fatal(err)
		}
		settled(t, runner, started.ID)
	}
	if list := runner.Jobs(); len(list) > 3 {
		t.Errorf("history holds %d jobs, want the last two plus at most one still finishing", len(list))
	}
}
