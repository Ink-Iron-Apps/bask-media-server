package nowplaying_test

import (
	"sync"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/nowplaying"
)

// clock is a time this test moves by hand, because every rule in this package
// is about a deadline and a test that waits for one is a test that is slow and
// occasionally wrong.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) pass(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

// stopper records what was asked to stop, standing in for the transcode
// manager.
type stopper struct {
	mu       sync.Mutex
	sessions []string
	keys     []string
}

func (s *stopper) Stop(id string) {
	s.mu.Lock()
	s.sessions = append(s.sessions, id)
	s.mu.Unlock()
}

func (s *stopper) StopKey(key string) {
	s.mu.Lock()
	s.keys = append(s.keys, key)
	s.mu.Unlock()
}

func registry(t *testing.T) (*nowplaying.Registry, *clock, *stopper) {
	t.Helper()
	c := &clock{at: time.Date(2026, 11, 21, 20, 0, 0, 0, time.UTC)}
	s := &stopper{}
	return nowplaying.New(nowplaying.Options{
		Idle:       time.Minute,
		Now:        c.now,
		Transcoder: s,
	}), c, s
}

// A progress report is all it takes to appear on the page, because that is a
// request the client was going to make anyway.
func TestAReportIsAStream(t *testing.T) {
	reg, _, _ := registry(t)

	reg.Report(nowplaying.Stream{
		Key: "jellyfin|abc", Surface: "jellyfin",
		UserID: 7, UserName: "riley",
		ItemID: 42, Title: "Big Hill", Client: "Bask",
		Position: 90 * time.Second, Duration: 100 * time.Minute,
	})

	playing := reg.List()
	if len(playing) != 1 {
		t.Fatalf("expected one stream, got %d", len(playing))
	}
	if playing[0].Title != "Big Hill" || playing[0].UserName != "riley" {
		t.Fatalf("the stream did not carry who was watching what: %+v", playing[0])
	}
	if reg.Count() != 1 {
		t.Fatalf("counted %d", reg.Count())
	}
}

// A later report moves the position without starting a second stream, and
// without forgetting when the first one began: the page says how long
// somebody has been watching, not how long ago they checked in.
func TestLaterReportsAreTheSameStream(t *testing.T) {
	reg, clk, _ := registry(t)
	began := clk.now()

	reg.Report(nowplaying.Stream{Key: "k", ItemID: 1, Title: "Big Hill", Position: 10 * time.Second})
	clk.pass(20 * time.Second)
	reg.Report(nowplaying.Stream{Key: "k", ItemID: 1, Position: 30 * time.Second})

	playing := reg.List()
	if len(playing) != 1 {
		t.Fatalf("expected one stream, got %d", len(playing))
	}
	if playing[0].Position != 30*time.Second {
		t.Fatalf("position is %s", playing[0].Position)
	}
	if !playing[0].Since.Equal(began) {
		t.Fatalf("the stream forgot when it started: %s", playing[0].Since)
	}
	// And the title it was given first is still there, because a progress
	// report does not carry one.
	if playing[0].Title != "Big Hill" {
		t.Fatalf("title became %q", playing[0].Title)
	}
}

// The negotiation knows what is being done to the file and the progress
// report knows where the viewer is. Neither erases the other.
func TestDetailsAndPositionComeFromDifferentRequests(t *testing.T) {
	reg, _, _ := registry(t)

	reg.Report(nowplaying.Stream{Key: "k", ItemID: 1, Mode: "transcode", Why: "the client cannot decode HEVC", Session: "sess-1"})
	reg.Report(nowplaying.Stream{Key: "k", ItemID: 1, Position: time.Minute})

	got := reg.List()[0]
	if got.Mode != "transcode" || got.Session != "sess-1" {
		t.Fatalf("the transcoding details were lost: %+v", got)
	}
	if got.Position != time.Minute {
		t.Fatalf("position is %s", got.Position)
	}
}

// Nobody reporting for long enough means nobody watching. There is no event
// for a television switched off at the wall.
func TestAStreamNobodyReportsIsOver(t *testing.T) {
	reg, clk, _ := registry(t)
	reg.Report(nowplaying.Stream{Key: "k", ItemID: 1})

	clk.pass(30 * time.Second)
	if reg.Count() != 1 {
		t.Fatal("a stream reported half a minute ago is still playing")
	}
	clk.pass(90 * time.Second)
	if reg.Count() != 0 {
		t.Fatalf("a stream nobody has reported for two minutes is still listed: %+v", reg.List())
	}
}

// A client saying it has stopped is taken at its word, immediately.
func TestStoppingForgetsIt(t *testing.T) {
	reg, _, _ := registry(t)
	reg.Report(nowplaying.Stream{Key: "k", ItemID: 1})
	reg.Stopped("k")
	if reg.Count() != 0 {
		t.Fatal("the stream outlived the client that said it had stopped")
	}
}

// Terminating kills the work and arms the latch: the stream is not playing
// any more, and the next request that names it is refused.
func TestTerminatingStopsTheWorkAndRefusesWhatComesNext(t *testing.T) {
	reg, _, stop := registry(t)
	reg.Report(nowplaying.Stream{Key: "k", ItemID: 1, Session: "sess-1"})

	if _, ok := reg.Terminate("k", "Family time."); !ok {
		t.Fatal("terminating a live stream reported nothing to terminate")
	}
	if reg.Count() != 0 {
		t.Fatal("a terminated stream is still listed as playing")
	}

	reason, stopping := reg.Stopping("k")
	if !stopping {
		t.Fatal("the next request for a terminated stream was not refused")
	}
	if reason != "Family time." {
		t.Fatalf("the reason given was %q", reason)
	}

	stop.mu.Lock()
	defer stop.mu.Unlock()
	if len(stop.sessions) != 1 || stop.sessions[0] != "sess-1" {
		t.Fatalf("the encoder was not killed: %v", stop.sessions)
	}
	if len(stop.keys) != 1 || stop.keys[0] != "k" {
		t.Fatalf("the key was not stopped: %v", stop.keys)
	}
}

// A client that keeps reporting does not come back from the dead, which is
// what would happen if a report cleared the latch.
func TestATerminatedStreamStaysTerminated(t *testing.T) {
	reg, clk, _ := registry(t)
	reg.Report(nowplaying.Stream{Key: "k", ItemID: 1})
	reg.Terminate("k", "")

	clk.pass(10 * time.Second)
	reg.Report(nowplaying.Stream{Key: "k", ItemID: 1, Position: time.Minute})

	if reg.Count() != 0 {
		t.Fatal("a terminated stream came back by reporting progress")
	}
	if _, stopping := reg.Stopping("k"); !stopping {
		t.Fatal("the latch was cleared by a progress report")
	}
}

// And a player that retries for a long time is refused for that whole time:
// each refusal keeps the latch alive, so it cannot expire underneath a client
// and quietly start working again.
func TestTheLatchOutlivesAClientThatKeepsAsking(t *testing.T) {
	reg, clk, _ := registry(t)
	reg.Report(nowplaying.Stream{Key: "k", ItemID: 1})
	reg.Terminate("k", "")

	for range 10 {
		clk.pass(50 * time.Second)
		if _, stopping := reg.Stopping("k"); !stopping {
			t.Fatal("the latch expired while the client was still asking")
		}
	}
}

// An unterminated stream, and an id nobody has ever used, are both served
// normally. A gate that refuses anything it does not recognise would be a
// gate that breaks playback.
func TestNothingElseIsRefused(t *testing.T) {
	reg, _, _ := registry(t)
	reg.Report(nowplaying.Stream{Key: "live", ItemID: 1})

	if _, stopping := reg.Stopping("live"); stopping {
		t.Fatal("a live stream was refused")
	}
	if _, stopping := reg.Stopping("never-heard-of-it"); stopping {
		t.Fatal("an unknown stream was refused")
	}
	if _, stopping := reg.Stopping(""); stopping {
		t.Fatal("a request with no stream id was refused")
	}
}

// Terminating something that is not playing says so rather than inventing a
// stream to stop.
func TestTerminatingNothingSaysSo(t *testing.T) {
	reg, _, stop := registry(t)
	if _, ok := reg.Terminate("nobody", ""); ok {
		t.Fatal("terminated a stream that was not playing")
	}
	stop.mu.Lock()
	defer stop.mu.Unlock()
	if len(stop.sessions)+len(stop.keys) != 0 {
		t.Fatal("killed work for a stream that was not playing")
	}
}

// A client that reuses its play session id for the next episode is watching
// something else, and the page says so.
func TestTheNextEpisodeIsANewStream(t *testing.T) {
	reg, clk, _ := registry(t)
	reg.Report(nowplaying.Stream{Key: "k", ItemID: 1, Title: "Episode 1", Position: 20 * time.Minute})
	clk.pass(30 * time.Second)
	reg.Report(nowplaying.Stream{Key: "k", ItemID: 2, Title: "Episode 2", Position: 5 * time.Second})

	got := reg.List()
	if len(got) != 1 {
		t.Fatalf("expected one stream, got %d", len(got))
	}
	if got[0].Title != "Episode 2" || got[0].Position != 5*time.Second {
		t.Fatalf("still showing the previous episode: %+v", got[0])
	}
	if !got[0].Since.Equal(clk.now()) {
		t.Fatal("the new episode inherited the old one's start time")
	}
}

// Nothing in here may be the thing that falls over when two clients report at
// once, which is the normal state of a household at eight in the evening.
func TestConcurrentReportsAreSafe(t *testing.T) {
	reg, _, _ := registry(t)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := range 50 {
				reg.Report(nowplaying.Stream{Key: string(rune('a' + n)), ItemID: int64(n), Position: time.Duration(j) * time.Second})
				reg.List()
				reg.Stopping(string(rune('a' + n)))
			}
		}(i)
	}
	wg.Wait()
	if reg.Count() != 8 {
		t.Fatalf("expected eight streams, got %d", reg.Count())
	}
}
