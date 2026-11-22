package jobs_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/jobs"
)

// collector is whoever is listening to the passes.
type collector struct {
	mu      sync.Mutex
	notices []jobs.Notice
}

func (c *collector) note(notice jobs.Notice) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notices = append(c.notices, notice)
}

// announced waits for the finished notice, which a pass sends after naming
// everything it added.
//
// A pass is marked done before the notices go out — the page should show a
// finished job the moment it is finished, not once a notification has been
// delivered — so waiting for the job to settle is not waiting for the
// notices. This is.
func (c *collector) announced(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.of(jobs.NoticeDone))+len(c.of(jobs.NoticeFailed)) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the pass never said it had finished")
}

func (c *collector) of(kind jobs.NoticeKind) []jobs.Notice {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []jobs.Notice
	for _, notice := range c.notices {
		if notice.Kind == kind {
			out = append(out, notice)
		}
	}
	return out
}

// A scan that found three films says so, and names them.
func TestAScanSaysWhatItAddedAndThatItFinished(t *testing.T) {
	ctx := context.Background()
	db, section := library(t, 3)
	heard := &collector{}
	runner := &jobs.Runner{DB: db, Prober: prober{}, Notify: heard.note}
	t.Cleanup(runner.Shutdown)

	started, err := runner.Start(ctx, jobs.KindScan, section.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job := settled(t, runner, started.ID); job.State != jobs.StateDone {
		t.Fatalf("the scan did not finish: %+v", job)
	}
	heard.announced(t)

	added := heard.of(jobs.NoticeAdded)
	if len(added) != 3 {
		t.Fatalf("three new films should be announced, got %d", len(added))
	}
	for _, notice := range added {
		if notice.Item == nil || notice.Item.Title == "" {
			t.Fatalf("an added notice with nothing in it: %+v", notice)
		}
		if notice.Section != "Films" {
			t.Errorf("a new item should name its library, got %q", notice.Section)
		}
	}

	done := heard.of(jobs.NoticeDone)
	if len(done) != 1 {
		t.Fatalf("one finished pass, got %d", len(done))
	}
	if done[0].Job.Note == "" || done[0].Job.Elapsed() <= 0 {
		t.Errorf("the finished notice should carry the pass: %+v", done[0].Job)
	}

	// Nothing is announced twice. A second scan of an unchanged library
	// found nothing new, and a notification per file per scan is how
	// somebody turns webhooks off again.
	heard.mu.Lock()
	heard.notices = nil
	heard.mu.Unlock()

	again, err := runner.Start(ctx, jobs.KindScan, section.ID)
	if err != nil {
		t.Fatal(err)
	}
	settled(t, runner, again.ID)
	heard.announced(t)
	if added := heard.of(jobs.NoticeAdded); len(added) != 0 {
		t.Errorf("a rescan of the same files announced %d items", len(added))
	}
	if done := heard.of(jobs.NoticeDone); len(done) != 1 {
		t.Errorf("the second pass should still report finishing, got %d", len(done))
	}
}

// A pass somebody cancelled is not news: they know, they pressed the button.
func TestACancelledPassIsNotAnnounced(t *testing.T) {
	ctx := context.Background()
	db, section := library(t, 2)
	heard := &collector{}
	held := make(chan struct{})
	runner := &jobs.Runner{DB: db, Prober: prober{held: held}, Notify: heard.note}
	t.Cleanup(runner.Shutdown)

	started, err := runner.Start(ctx, jobs.KindScan, section.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Cancel(started.ID); err != nil {
		t.Fatal(err)
	}
	close(held)

	job := settled(t, runner, started.ID)
	if job.State != jobs.StateCanceled {
		t.Fatalf("expected a cancelled pass, got %+v", job)
	}
	// Give the notice path the chance to be wrong.
	time.Sleep(50 * time.Millisecond)
	heard.mu.Lock()
	defer heard.mu.Unlock()
	if len(heard.notices) != 0 {
		t.Errorf("a cancelled pass announced %+v", heard.notices)
	}
}

// A runner with nobody listening is the default, and must not care.
func TestAPassWithNobodyListeningIsFine(t *testing.T) {
	ctx := context.Background()
	db, section := library(t, 1)
	runner := &jobs.Runner{DB: db, Prober: prober{}}
	t.Cleanup(runner.Shutdown)

	started, err := runner.Start(ctx, jobs.KindScan, section.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job := settled(t, runner, started.ID); job.State != jobs.StateDone {
		t.Fatalf("the scan did not finish: %+v", job)
	}
}
