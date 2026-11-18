package markers_test

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/fingerprint"
	"github.com/inknironapps/bask-media-server/internal/markers"
)

// frames is how many fingerprint frames a duration covers, so a test can talk
// in seconds and leave the frame rate to the fingerprint package.
func frames(d time.Duration) int { return int(d / fingerprint.FrameDuration) }

// noise is audio nothing else shares: distinct hashes, deterministically so a
// failure is reproducible.
func noise(seed uint64, n int) []uint32 {
	r := rand.New(rand.NewPCG(seed, 0x9e3779b9))
	out := make([]uint32, n)
	for i := range out {
		out[i] = r.Uint32()
	}
	return out
}

// episode is an intro followed by content of its own, which is the shape the
// detector exists for.
func episode(itemID int64, intro []uint32, contentSeed uint64, content int) fingerprint.Print {
	hashes := make([]uint32, 0, len(intro)+content)
	hashes = append(hashes, intro...)
	hashes = append(hashes, noise(contentSeed, content)...)
	return fingerprint.Print{ItemID: itemID, Hashes: hashes}
}

func TestFromAudioFindsWhatEpisodesShare(t *testing.T) {
	intro := noise(1, frames(80*time.Second))
	prints := []fingerprint.Print{
		episode(11, intro, 100, frames(5*time.Minute)),
		episode(12, intro, 200, frames(5*time.Minute)),
		episode(13, intro, 300, frames(5*time.Minute)),
	}

	spans := markers.FromAudio(prints, markers.IntroLimits)

	if len(spans) != 3 {
		t.Fatalf("got %d spans, want one per episode: %+v", len(spans), spans)
	}
	for id, span := range spans {
		if span.Start > 2*fingerprint.FrameDuration {
			t.Errorf("episode %d: intro starts at %v, want the beginning", id, span.Start)
		}
		if diff := span.Length() - 80*time.Second; diff < -2*time.Second || diff > 2*time.Second {
			t.Errorf("episode %d: intro is %v, want about 80s", id, span.Length())
		}
	}
}

// Two episodes are routinely not encoded identically, so the same intro comes
// back with a few bits wrong per frame. A detector that needed exact equality
// would find nothing on a real library.
func TestFromAudioToleratesAnImperfectMatch(t *testing.T) {
	intro := noise(2, frames(90*time.Second))
	reencoded := make([]uint32, len(intro))
	r := rand.New(rand.NewPCG(7, 7))
	for i, h := range intro {
		// Up to three flipped bits out of 32, which is what a different
		// encoder of the same audio looks like.
		for range r.IntN(4) {
			h ^= 1 << uint(r.IntN(32))
		}
		reencoded[i] = h
	}

	prints := []fingerprint.Print{
		episode(21, intro, 100, frames(4*time.Minute)),
		episode(22, reencoded, 200, frames(4*time.Minute)),
	}

	spans := markers.FromAudio(prints, markers.IntroLimits)
	if len(spans) != 2 {
		t.Fatalf("a re-encode should still match: %+v", spans)
	}
	for id, span := range spans {
		if span.Length() < 60*time.Second {
			t.Errorf("episode %d: matched only %v of a 90s intro", id, span.Length())
		}
	}
}

// Nothing in common means no marker. A button that skips the first ninety
// seconds of the episode is worse than no button.
func TestFromAudioClaimsNothingFromUnrelatedEpisodes(t *testing.T) {
	prints := []fingerprint.Print{
		{ItemID: 31, Hashes: noise(11, frames(5*time.Minute))},
		{ItemID: 32, Hashes: noise(12, frames(5*time.Minute))},
		{ItemID: 33, Hashes: noise(13, frames(5*time.Minute))},
	}

	if spans := markers.FromAudio(prints, markers.IntroLimits); len(spans) != 0 {
		t.Fatalf("unrelated audio produced markers: %+v", spans)
	}
}

// A show whose episodes share a two-second production logo has no intro, and
// the limits are what keep that from becoming one.
func TestFromAudioRejectsSomethingTooShortToBeAnIntro(t *testing.T) {
	logo := noise(3, frames(3*time.Second))
	prints := []fingerprint.Print{
		episode(41, logo, 100, frames(5*time.Minute)),
		episode(42, logo, 200, frames(5*time.Minute)),
	}

	if spans := markers.FromAudio(prints, markers.IntroLimits); len(spans) != 0 {
		t.Fatalf("a three-second match is not an intro: %+v", spans)
	}
}

// A single file has nothing to agree with, and an intro claimed from one file
// is an intro claimed from nothing.
func TestFromAudioNeedsMoreThanOneEpisode(t *testing.T) {
	intro := noise(4, frames(90*time.Second))
	prints := []fingerprint.Print{episode(51, intro, 100, frames(5*time.Minute))}

	if spans := markers.FromAudio(prints, markers.IntroLimits); len(spans) != 0 {
		t.Fatalf("one episode cannot corroborate itself: %+v", spans)
	}
}

// Credits are measured from the end of the file, so the fingerprint carries
// the offset it started at and the marker has to be reported in file time.
func TestFromAudioReportsPositionsInFileTime(t *testing.T) {
	outro := noise(5, frames(60*time.Second))
	content := frames(5 * time.Minute)
	offset := 14 * time.Minute

	tail := func(id int64, seed uint64) fingerprint.Print {
		hashes := append(noise(seed, content), outro...)
		return fingerprint.Print{ItemID: id, Offset: offset, Hashes: hashes}
	}
	spans := markers.FromAudio([]fingerprint.Print{tail(61, 100), tail(62, 200)}, markers.CreditsLimits)

	if len(spans) != 2 {
		t.Fatalf("got %d spans: %+v", len(spans), spans)
	}
	for id, span := range spans {
		want := offset + time.Duration(content)*fingerprint.FrameDuration
		if diff := span.Start - want; diff < -2*time.Second || diff > 2*time.Second {
			t.Errorf("episode %d: credits start reported at %v, want about %v", id, span.Start, want)
		}
	}
}

func TestMarkersCarryTheirSource(t *testing.T) {
	spans := map[int64]markers.Span{7: {Start: 0, End: 90 * time.Second}}

	out := markers.Markers(spans, domain.MarkerIntro)

	if len(out) != 1 {
		t.Fatalf("got %d markers", len(out))
	}
	if out[0].Source != domain.SourceAudio || out[0].Type != domain.MarkerIntro {
		t.Errorf("marker = %+v", out[0])
	}
}
