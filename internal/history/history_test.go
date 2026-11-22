package history_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/history"
	"github.com/inknironapps/bask-media-server/internal/nowplaying"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// book is the database, without one.
type book struct {
	mu      sync.Mutex
	next    int64
	rows    map[int64]*row
	order   []int64
	failing bool
}

type row struct {
	start store.NewWatch
	last  store.WatchProgress
	ends  int
}

func newBook() *book { return &book{rows: map[int64]*row{}} }

func (b *book) StartWatch(_ context.Context, in store.NewWatch) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failing {
		return 0, errors.New("the database is unavailable")
	}
	b.next++
	b.rows[b.next] = &row{start: in}
	b.order = append(b.order, b.next)
	return b.next, nil
}

func (b *book) TouchWatch(_ context.Context, id int64, in store.WatchProgress) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	kept, found := b.rows[id]
	if !found {
		return errors.New("no such row")
	}
	if kept.ends > 0 {
		// The store refuses to reopen a closed row, so this one does too.
		return nil
	}
	kept.last = in
	if in.Done {
		kept.ends++
	}
	return nil
}

func (b *book) row(id int64) row {
	b.mu.Lock()
	defer b.mu.Unlock()
	return *b.rows[id]
}

func (b *book) rowCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.rows)
}

// clock is a hand-wound one, because every number this package produces is a
// difference between two times.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) pass(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newRecorder(t *testing.T) (*history.Recorder, *book, *clock) {
	t.Helper()
	ledger := newBook()
	tick := &clock{now: time.Date(2026, 11, 21, 20, 0, 0, 0, time.UTC)}
	return history.New(history.Options{DB: ledger, Now: tick.read}), ledger, tick
}

func stream(key string, since time.Time) nowplaying.Stream {
	return nowplaying.Stream{
		Key: key, Surface: "jellyfin", UserID: 1, UserName: "Riley",
		ItemID: 7, Title: "Big Hill", Device: "Living Room TV", Mode: "direct",
		Duration: 95 * time.Minute, Since: since,
	}
}

// The whole of the pause accounting: an hour open, twenty minutes of it
// paused, forty minutes watched.
func TestAPausedFilmIsNotAFilmBeingWatched(t *testing.T) {
	recorder, ledger, tick := newRecorder(t)
	started := tick.read()

	recorder.Record(nowplaying.Change{Kind: nowplaying.Started, Stream: stream("tv|1", started)})
	if ledger.rowCount() != 1 {
		t.Fatalf("a start should open one row, got %d", ledger.rowCount())
	}

	tick.pass(20 * time.Minute)
	live := stream("tv|1", started)
	live.Paused, live.Position = true, 20*time.Minute
	recorder.Record(nowplaying.Change{Kind: nowplaying.Paused, Stream: live})

	tick.pass(20 * time.Minute)
	live.Paused = false
	recorder.Record(nowplaying.Change{Kind: nowplaying.Resumed, Stream: live})

	tick.pass(20 * time.Minute)
	live.Position = 40 * time.Minute
	recorder.Record(nowplaying.Change{Kind: nowplaying.Stopped, Stream: live})

	kept := ledger.row(1)
	if kept.ends != 1 {
		t.Errorf("the row should be closed once, closed %d times", kept.ends)
	}
	if kept.last.Paused != 20*time.Minute {
		t.Errorf("paused = %s, want 20m", kept.last.Paused)
	}
	open := kept.last.At.Sub(kept.start.Started)
	if open != time.Hour {
		t.Errorf("the stream was open for %s, want 1h", open)
	}
	if open-kept.last.Paused != 40*time.Minute {
		t.Errorf("watched = %s, want 40m", open-kept.last.Paused)
	}
	if recorder.Watching() != 0 {
		t.Errorf("the recorder is still holding %d rows", recorder.Watching())
	}
}

// A pause that is still running counts up to now. Otherwise a film left
// paused all evening reads as one watched all evening.
func TestAPauseStillRunningCountsUpToNow(t *testing.T) {
	recorder, ledger, tick := newRecorder(t)
	started := tick.read()

	recorder.Record(nowplaying.Change{Kind: nowplaying.Started, Stream: stream("tv|1", started)})

	tick.pass(10 * time.Minute)
	paused := stream("tv|1", started)
	paused.Paused, paused.Position = true, 10*time.Minute
	recorder.Record(nowplaying.Change{Kind: nowplaying.Paused, Stream: paused})

	// Two hours of nobody in the room, and a heartbeat in the middle of it.
	tick.pass(2 * time.Hour)
	recorder.Sweep([]nowplaying.Stream{paused})

	kept := ledger.row(1)
	if kept.last.Paused != 2*time.Hour {
		t.Errorf("paused = %s, want 2h", kept.last.Paused)
	}
	open := kept.last.At.Sub(kept.start.Started)
	if open-kept.last.Paused != 10*time.Minute {
		t.Errorf("watched = %s, want 10m", open-kept.last.Paused)
	}
}

// The heartbeat. Without it a row's length is whatever it was at the start,
// so a machine that lost power mid-film would record a film that lasted no
// time at all.
func TestTheHeartbeatKeepsAnOpenRowHonest(t *testing.T) {
	recorder, ledger, tick := newRecorder(t)
	started := tick.read()
	live := stream("tv|1", started)

	recorder.Record(nowplaying.Change{Kind: nowplaying.Started, Stream: live})
	for range 3 {
		tick.pass(time.Minute)
		live.Position += time.Minute
		recorder.Sweep([]nowplaying.Stream{live})
	}

	kept := ledger.row(1)
	if kept.last.At.Sub(kept.start.Started) != 3*time.Minute {
		t.Errorf("the row covers %s, want 3m", kept.last.At.Sub(kept.start.Started))
	}
	if kept.last.Position != 3*time.Minute {
		t.Errorf("position = %s, want 3m", kept.last.Position)
	}
	if kept.last.Done {
		t.Error("a heartbeat should not close the row")
	}
}

// The recorder recovers. A stream whose row could not be written, or one
// already playing when the recorder was built, is picked up by the next
// sweep rather than going unrecorded for the rest of the evening.
func TestASweepPicksUpAStreamItHasNoRowFor(t *testing.T) {
	recorder, ledger, tick := newRecorder(t)
	started := tick.read()
	live := stream("tv|1", started)

	ledger.failing = true
	recorder.Record(nowplaying.Change{Kind: nowplaying.Started, Stream: live})
	if ledger.rowCount() != 0 || recorder.Watching() != 0 {
		t.Fatalf("the failed write should have left nothing: %d rows", ledger.rowCount())
	}

	ledger.failing = false
	tick.pass(time.Minute)
	recorder.Sweep([]nowplaying.Stream{live})

	if ledger.rowCount() != 1 || recorder.Watching() != 1 {
		t.Fatalf("the sweep should have opened a row: %d", ledger.rowCount())
	}
	if ledger.row(1).start.Title != "Big Hill" {
		t.Errorf("the recovered row is about %q", ledger.row(1).start.Title)
	}
}

// A row for a stream the register no longer has. The stop was missed, and
// leaving it open would hold it for the life of the process.
func TestASweepClosesARowWhoseStreamHasGone(t *testing.T) {
	recorder, ledger, tick := newRecorder(t)
	live := stream("tv|1", tick.read())

	recorder.Record(nowplaying.Change{Kind: nowplaying.Started, Stream: live})
	tick.pass(5 * time.Minute)
	recorder.Sweep(nil)

	if recorder.Watching() != 0 {
		t.Errorf("the recorder is still holding %d rows", recorder.Watching())
	}
	if kept := ledger.row(1); kept.ends != 1 {
		t.Errorf("the orphaned row should have been closed, ends = %d", kept.ends)
	}
}

// Shutdown is not a stream ending. The rows are left incomplete on purpose:
// "the server never saw this stop" is a true thing to be able to say later.
func TestShutdownLeavesTheRowIncomplete(t *testing.T) {
	recorder, ledger, tick := newRecorder(t)
	live := stream("tv|1", tick.read())

	recorder.Record(nowplaying.Change{Kind: nowplaying.Started, Stream: live})
	tick.pass(30 * time.Minute)
	live.Position = 30 * time.Minute
	recorder.Flush([]nowplaying.Stream{live})

	kept := ledger.row(1)
	if kept.last.Done {
		t.Error("a flush should not claim the stream ended")
	}
	if kept.last.At.Sub(kept.start.Started) != 30*time.Minute {
		t.Errorf("the row covers %s, want 30m", kept.last.At.Sub(kept.start.Started))
	}
}

// Some clients reuse one play session across an episode boundary: the
// register reports a stop and a start, and the log should be two rows.
func TestTheNextEpisodeIsASecondRow(t *testing.T) {
	recorder, ledger, tick := newRecorder(t)
	first := stream("tv|1", tick.read())
	first.Title, first.Subtitle = "Big Hill", "One"

	recorder.Record(nowplaying.Change{Kind: nowplaying.Started, Stream: first})
	tick.pass(42 * time.Minute)
	recorder.Record(nowplaying.Change{Kind: nowplaying.Stopped, Stream: first})

	second := stream("tv|1", tick.read())
	second.ItemID, second.Title, second.Subtitle = 8, "Big Hill", "Two"
	recorder.Record(nowplaying.Change{Kind: nowplaying.Started, Stream: second})
	tick.pass(40 * time.Minute)
	recorder.Record(nowplaying.Change{Kind: nowplaying.Stopped, Stream: second})

	if ledger.rowCount() != 2 {
		t.Fatalf("two episodes are two rows, got %d", ledger.rowCount())
	}
	if ledger.row(1).start.Subtitle != "One" || ledger.row(2).start.Subtitle != "Two" {
		t.Errorf("the rows are about %q and %q",
			ledger.row(1).start.Subtitle, ledger.row(2).start.Subtitle)
	}
	if ledger.row(1).last.Done != true || ledger.row(2).last.Done != true {
		t.Error("both rows should be closed")
	}
}

// A stream somebody stopped records why, because that is the one thing a log
// can say that the register cannot once the stream is gone.
func TestATerminatedStreamRecordsWhy(t *testing.T) {
	recorder, ledger, tick := newRecorder(t)
	live := stream("tv|1", tick.read())

	recorder.Record(nowplaying.Change{Kind: nowplaying.Started, Stream: live})
	tick.pass(10 * time.Minute)
	live.Reason = "An administrator stopped this stream."
	recorder.Record(nowplaying.Change{Kind: nowplaying.Stopped, Stream: live})

	if kept := ledger.row(1); kept.last.Reason != live.Reason {
		t.Errorf("the reason was not recorded: %q", kept.last.Reason)
	}
}

// A recorder nobody built is every caller's normal case on a server with no
// log, and none of them should have to check.
func TestANilRecorderIsHarmless(t *testing.T) {
	var recorder *history.Recorder
	recorder.Record(nowplaying.Change{Kind: nowplaying.Started, Stream: stream("tv|1", time.Now())})
	recorder.Sweep([]nowplaying.Stream{stream("tv|1", time.Now())})
	recorder.Flush(nil)
	if recorder.Watching() != 0 {
		t.Error("a recorder that does not exist is watching nothing")
	}
}

// Nothing is recorded for a change with no stream key, which is what a
// malformed report looks like by the time it reaches here.
func TestAChangeWithNoKeyIsIgnored(t *testing.T) {
	recorder, ledger, _ := newRecorder(t)
	recorder.Record(nowplaying.Change{Kind: nowplaying.Started})
	if ledger.rowCount() != 0 {
		t.Errorf("a keyless change wrote %d rows", ledger.rowCount())
	}
}
