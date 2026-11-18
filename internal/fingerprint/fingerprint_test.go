package fingerprint

import (
	"bytes"
	"encoding/binary"
	"math"
	"math/rand/v2"
	"testing"
)

// pcm renders signed 16-bit mono samples at [SampleRate], the format the
// decoder is asked for, so these tests exercise the same path a real file
// takes after ffmpeg.
func pcm(samples []float64) []byte {
	out := make([]byte, 2*len(samples))
	for i, s := range samples {
		clamped := math.Max(-1, math.Min(1, s))
		binary.LittleEndian.PutUint16(out[2*i:], uint16(int16(clamped*32767)))
	}
	return out
}

// music is a deterministic signal with structure across the bands the hash
// looks at: a few tones whose amplitudes drift, which is what gives each frame
// a different spectral shape to fingerprint.
func music(seed uint64, seconds float64) []float64 {
	r := rand.New(rand.NewPCG(seed, 0x5deece66))
	n := int(seconds * SampleRate)
	out := make([]float64, n)

	type partial struct{ hz, amp, drift float64 }
	partials := make([]partial, 6)
	for i := range partials {
		partials[i] = partial{
			hz:    300 + r.Float64()*2500,
			amp:   0.1 + r.Float64()*0.2,
			drift: 0.5 + r.Float64()*3,
		}
	}
	for i := range out {
		t := float64(i) / SampleRate
		var sum float64
		for _, p := range partials {
			sum += p.amp * (0.5 + 0.5*math.Sin(2*math.Pi*p.drift*t)) * math.Sin(2*math.Pi*p.hz*t)
		}
		out[i] = sum
	}
	return out
}

func hashOf(t *testing.T, samples []float64) []uint32 {
	t.Helper()
	hashes, err := hashStream(bytes.NewReader(pcm(samples)), maxFrames)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) == 0 {
		t.Fatal("no frames were hashed")
	}
	return hashes
}

func meanDistance(a, b []uint32) float64 {
	n := min(len(a), len(b))
	if n == 0 {
		return 32
	}
	var total int
	for i := range n {
		total += Distance(a[i], b[i])
	}
	return float64(total) / float64(n)
}

// The whole detector rests on this: the same audio has to hash the same way.
func TestTheSameAudioHashesTheSameWay(t *testing.T) {
	audio := music(1, 8)

	first := hashOf(t, audio)
	second := hashOf(t, audio)

	if got := meanDistance(first, second); got != 0 {
		t.Fatalf("mean distance between a signal and itself = %.2f, want 0", got)
	}
}

// Different audio has to hash differently, or every episode would match every
// other and the markers would be noise. Unrelated audio should sit near a coin
// toss per bit.
func TestDifferentAudioHashesDifferently(t *testing.T) {
	a := hashOf(t, music(1, 8))
	b := hashOf(t, music(2, 8))

	if got := meanDistance(a, b); got < 8 {
		t.Fatalf("mean distance between unrelated audio = %.2f, want well above the match threshold", got)
	}
}

// The reason the hash is a difference of differences rather than a level: one
// episode mastered louder than another is the same intro, and has to match.
func TestAVolumeChangeStillMatches(t *testing.T) {
	audio := music(3, 8)
	quieter := make([]float64, len(audio))
	for i, s := range audio {
		quieter[i] = s * 0.4
	}

	loud := hashOf(t, audio)
	soft := hashOf(t, quieter)

	if got := meanDistance(loud, soft); got > 2 {
		t.Fatalf("mean distance after a volume change = %.2f, want near zero", got)
	}
}

// A fingerprint is a fraction of the audio it describes, which is what makes
// holding a whole season's worth affordable. This pins the ratio rather than
// trusting the comment about it.
func TestAFingerprintIsSmall(t *testing.T) {
	const seconds = 30
	audio := music(4, seconds)

	hashes := hashOf(t, audio)

	bytesOfAudio := 2 * len(audio)
	bytesOfPrint := 4 * len(hashes)
	if ratio := float64(bytesOfAudio) / float64(bytesOfPrint); ratio < 100 {
		t.Fatalf("a fingerprint is 1/%.0f of the audio, want at least 1/100", ratio)
	}
	want := seconds * SampleRate / Hop
	if len(hashes) < want-4 || len(hashes) > want {
		t.Fatalf("got %d frames for %ds, want about %d", len(hashes), seconds, want)
	}
}

func TestEncodeRoundTrips(t *testing.T) {
	hashes := []uint32{0, 1, 0xdeadbeef, 0xffffffff}

	if got := Decode(Encode(hashes)); len(got) != len(hashes) {
		t.Fatalf("got %d hashes back, want %d", len(got), len(hashes))
	} else {
		for i := range hashes {
			if got[i] != hashes[i] {
				t.Fatalf("hash %d came back as %08x, want %08x", i, got[i], hashes[i])
			}
		}
	}
}

// Frames are only produced for whole windows, and a stream that stops mid-frame
// must not produce a hash from a half-filled buffer: that hash would be of
// silence as much as of audio, and would match anything else that ended the
// same way.
func TestATruncatedStreamProducesNoPartialFrame(t *testing.T) {
	samples := music(5, 1)
	truncated := pcm(samples)[:2*(FrameSize+Hop+37)]

	hashes, err := hashStream(bytes.NewReader(truncated), maxFrames)
	if err != nil {
		t.Fatal(err)
	}

	// FrameSize fills the first window (which only sets the baseline energy),
	// and each further Hop produces one hash.
	if len(hashes) != 1 {
		t.Fatalf("got %d hashes, want 1 whole frame past the baseline", len(hashes))
	}
}
