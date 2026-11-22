package nowplaying_test

import (
	"sync"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/nowplaying"
)

// watcher collects what the register reported.
type watcher struct {
	mu      sync.Mutex
	changes []nowplaying.Change
}

func (w *watcher) note(change nowplaying.Change) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.changes = append(w.changes, change)
}

func (w *watcher) kinds() []nowplaying.ChangeKind {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]nowplaying.ChangeKind, 0, len(w.changes))
	for _, change := range w.changes {
		out = append(out, change.Kind)
	}
	return out
}

func same(t *testing.T, got []nowplaying.ChangeKind, want ...nowplaying.ChangeKind) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, got)
		}
	}
}

// The register is told a stream's state every few seconds. Only a change in
// it is news: a webhook that fired on every report would be a notification
// every ten seconds for as long as somebody left a film paused.
func TestOnlyChangesAreReported(t *testing.T) {
	seen := &watcher{}
	registry := nowplaying.New(nowplaying.Options{Changes: seen.note})

	playing := nowplaying.Stream{Key: "tv|1", UserID: 1, ItemID: 1, Title: "Big Hill", PausedKnown: true}
	registry.Report(playing)
	registry.Report(playing)
	registry.Report(playing)
	same(t, seen.kinds(), nowplaying.Started)

	paused := playing
	paused.Paused = true
	registry.Report(paused)
	registry.Report(paused)
	same(t, seen.kinds(), nowplaying.Started, nowplaying.Paused)

	registry.Report(playing)
	same(t, seen.kinds(), nowplaying.Started, nowplaying.Paused, nowplaying.Resumed)

	registry.Stopped(playing.Key)
	same(t, seen.kinds(),
		nowplaying.Started, nowplaying.Paused, nowplaying.Resumed, nowplaying.Stopped)
}

// A player goes on buffering while it is paused, so the file request that
// does so must not cancel out the progress report that said it was paused.
// Otherwise a film left paused is reported paused and resumed every ten
// seconds until somebody comes back to it.
func TestARequestForBytesDoesNotUnpauseAStream(t *testing.T) {
	seen := &watcher{}
	registry := nowplaying.New(nowplaying.Options{Changes: seen.note})

	progress := nowplaying.Stream{Key: "tv|1", ItemID: 1, Title: "Big Hill", PausedKnown: true}
	registry.Report(progress)

	paused := progress
	paused.Paused = true
	registry.Report(paused)
	same(t, seen.kinds(), nowplaying.Started, nowplaying.Paused)

	// The file request: a key, an item, and nothing it is in a position to
	// say about pausing.
	registry.Report(nowplaying.Stream{Key: "tv|1", ItemID: 1})
	registry.Report(nowplaying.Stream{Key: "tv|1", ItemID: 1})
	same(t, seen.kinds(), nowplaying.Started, nowplaying.Paused)

	if live := registry.List(); len(live) != 1 || !live[0].Paused {
		t.Errorf("the stream should still be shown as paused: %+v", live)
	}

	// And the next real report still resumes it.
	registry.Report(progress)
	same(t, seen.kinds(), nowplaying.Started, nowplaying.Paused, nowplaying.Resumed)
}

// Some clients reuse one play session across an episode boundary. That is two
// streams as far as anybody listening is concerned, and the stop is owed
// before the next start.
func TestTheNextEpisodeIsAStopAndAStart(t *testing.T) {
	seen := &watcher{}
	registry := nowplaying.New(nowplaying.Options{Changes: seen.note})

	registry.Report(nowplaying.Stream{Key: "tv|1", ItemID: 1, Title: "One"})
	registry.Report(nowplaying.Stream{Key: "tv|1", ItemID: 2, Title: "Two"})

	same(t, seen.kinds(), nowplaying.Started, nowplaying.Stopped, nowplaying.Started)

	seen.mu.Lock()
	defer seen.mu.Unlock()
	if seen.changes[1].Stream.Title != "One" || seen.changes[2].Stream.Title != "Two" {
		t.Errorf("the stop is the old episode and the start the new one: %+v", seen.changes)
	}
}

// There is no event for a television being switched off at the wall. The
// deadline is what turns that into a stop.
func TestATelevisionSwitchedOffStops(t *testing.T) {
	seen := &watcher{}
	now := time.Now()
	clock := func() time.Time { return now }
	registry := nowplaying.New(nowplaying.Options{
		Idle: time.Minute, Now: clock, Changes: seen.note,
	})

	registry.Report(nowplaying.Stream{Key: "tv|1", ItemID: 1, Title: "Big Hill"})
	same(t, seen.kinds(), nowplaying.Started)

	now = now.Add(2 * time.Minute)
	if playing := registry.List(); len(playing) != 0 {
		t.Fatalf("the stream should have expired: %+v", playing)
	}
	same(t, seen.kinds(), nowplaying.Started, nowplaying.Stopped)
}

// A terminated stream is reported stopped once, when the latch is armed, and
// not again when the expired row is swept up.
func TestATerminatedStreamIsReportedOnce(t *testing.T) {
	seen := &watcher{}
	now := time.Now()
	clock := func() time.Time { return now }
	registry := nowplaying.New(nowplaying.Options{
		Idle: time.Minute, Now: clock, Changes: seen.note,
	})

	registry.Report(nowplaying.Stream{Key: "tv|1", ItemID: 1, Title: "Big Hill"})
	if _, found := registry.Terminate("tv|1", "An administrator stopped this stream."); !found {
		t.Fatal("the stream should have been there to stop")
	}
	same(t, seen.kinds(), nowplaying.Started, nowplaying.Stopped)

	now = now.Add(10 * time.Minute)
	registry.List()
	same(t, seen.kinds(), nowplaying.Started, nowplaying.Stopped)

	seen.mu.Lock()
	defer seen.mu.Unlock()
	if seen.changes[1].Stream.Reason == "" {
		t.Error("whoever is listening should be told why it stopped")
	}
}

// Nothing is called with the register's lock held. A listener that asks the
// register a question — which a webhook mapping reasonably might — would
// otherwise deadlock the whole server.
func TestAListenerMayAskTheRegisterAQuestion(t *testing.T) {
	var registry *nowplaying.Registry
	done := make(chan int, 4)
	registry = nowplaying.New(nowplaying.Options{
		Changes: func(nowplaying.Change) { done <- registry.Count() },
	})

	registry.Report(nowplaying.Stream{Key: "tv|1", ItemID: 1, Title: "Big Hill"})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a listener that asked the register a question deadlocked")
	}
}

// A register with nobody listening behaves exactly as it did before, which is
// what every existing surface relies on.
func TestNobodyListeningIsFine(t *testing.T) {
	registry := nowplaying.New(nowplaying.Options{})
	registry.Report(nowplaying.Stream{Key: "tv|1", ItemID: 1, Title: "Big Hill"})
	if registry.Count() != 1 {
		t.Error("the register should still work with no listener")
	}
	registry.Stopped("tv|1")
	if registry.Count() != 0 {
		t.Error("the register should still work with no listener")
	}
}
