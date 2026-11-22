package nowplaying_test

import (
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/nowplaying"
)

// The count behind the cap on how many things one account may play at once.
func TestStreamsAreCountedPerAccount(t *testing.T) {
	reg, _, _ := registry(t)

	reg.Report(nowplaying.Stream{Key: "a", UserID: 1, ItemID: 1})
	reg.Report(nowplaying.Stream{Key: "b", UserID: 1, ItemID: 2})
	reg.Report(nowplaying.Stream{Key: "c", UserID: 2, ItemID: 3})

	if n := reg.CountFor(1, ""); n != 2 {
		t.Fatalf("the first account has %d streams", n)
	}
	if n := reg.CountFor(2, ""); n != 1 {
		t.Fatalf("the second account has %d streams", n)
	}
	if n := reg.CountFor(3, ""); n != 0 {
		t.Fatalf("an account playing nothing has %d streams", n)
	}

	// A client renegotiating its own stream is not a second stream, or a cap
	// of one could never play anything at all.
	if n := reg.CountFor(1, "a"); n != 1 {
		t.Fatalf("counting its own stream again: %d", n)
	}

	// Stopped and terminated streams are not playing.
	reg.Stopped("b")
	if n := reg.CountFor(1, ""); n != 1 {
		t.Fatalf("after one stopped: %d", n)
	}
	reg.Terminate("a", "")
	if n := reg.CountFor(1, ""); n != 0 {
		t.Fatalf("after the other was stopped by an administrator: %d", n)
	}
}

// And a stream nobody has reported inside the deadline is not counted, so a
// cap cannot be used up by a television somebody switched off.
func TestACapIsNotUsedUpByAStreamThatEnded(t *testing.T) {
	reg, clk, _ := registry(t)
	reg.Report(nowplaying.Stream{Key: "a", UserID: 1, ItemID: 1})

	clk.pass(2 * time.Minute)
	if n := reg.CountFor(1, ""); n != 0 {
		t.Fatalf("a stream nobody has reported for two minutes counts: %d", n)
	}
}
