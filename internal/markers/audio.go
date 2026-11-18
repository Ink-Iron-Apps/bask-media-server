package markers

import (
	"sort"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/fingerprint"
)

// Limits are what counts as a believable marker.
//
// The detector finds the stretch of audio two episodes share. That is usually
// the intro, but it is also sometimes a shared "previously on", a silent gap,
// or a production logo, so the answer is only accepted inside bounds a real
// intro or credits sequence falls in.
type Limits struct {
	Min time.Duration
	Max time.Duration

	// MaxDistance is how many of a hash's 32 bits two frames may disagree on
	// and still count as the same audio. Same source through two encoders
	// lands in the low single digits; unrelated audio sits near sixteen.
	MaxDistance int
}

// IntroLimits and CreditsLimits are the two cases this runs in.
//
// An intro is short and an intro that claims to be four minutes has found
// something else. Credits are genuinely long — a film's can run twelve
// minutes — but they are also the easiest thing to over-claim, because what
// follows the credits of one episode and the credits of another is often the
// same silence, so they get a ceiling too.
var (
	IntroLimits   = Limits{Min: 10 * time.Second, Max: 3 * time.Minute, MaxDistance: 6}
	CreditsLimits = Limits{Min: 15 * time.Second, Max: 6 * time.Minute, MaxDistance: 6}
)

// Span is a stretch of one item.
type Span struct {
	Start time.Duration
	End   time.Duration
}

// Length is how long the span runs for.
func (s Span) Length() time.Duration { return s.End - s.Start }

// FromAudio finds the stretch each fingerprint shares with the others.
//
// Compared pairwise within one season, which is the set that shares an intro.
// A season is a handful of episodes, so the pairs are a handful too; the work
// per pair is bounded by the fingerprint rather than by the file, and no audio
// is held at all — see the fingerprint package.
//
// An episode with no agreement gets no marker. That is the right answer far
// more often than a guess: a Skip Intro button that skips the first ninety
// seconds of the episode is worse than no button.
func FromAudio(prints []fingerprint.Print, limits Limits) map[int64]Span {
	if len(prints) < 2 {
		// Nothing to agree with. A single-episode season is common enough —
		// a special, a pilot, a season still being downloaded — and claiming
		// an intro from one file would be claiming it from nothing.
		return nil
	}

	candidates := make(map[int64][]Span, len(prints))
	for i := range prints {
		for j := i + 1; j < len(prints); j++ {
			runA, runB, ok := sharedRun(prints[i], prints[j], limits)
			if !ok {
				continue
			}
			candidates[prints[i].ItemID] = append(candidates[prints[i].ItemID], runA)
			candidates[prints[j].ItemID] = append(candidates[prints[j].ItemID], runB)
		}
	}

	out := make(map[int64]Span, len(candidates))
	for itemID, spans := range candidates {
		if span, ok := consensus(spans, limits); ok {
			out[itemID] = span
		}
	}
	return out
}

// Markers turns detected spans into markers of a kind.
func Markers(spans map[int64]Span, kind domain.MarkerType) []domain.Marker {
	out := make([]domain.Marker, 0, len(spans))
	for itemID, span := range spans {
		out = append(out, domain.Marker{
			ItemID: itemID,
			Type:   kind,
			Start:  span.Start,
			End:    span.End,
			Source: domain.SourceAudio,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ItemID < out[j].ItemID })
	return out
}

// consensus picks the span the pairs agree on.
//
// The median of each edge rather than the longest or the first: one pair can
// match further than the intro really runs — two episodes that share a cold
// open as well — and the median of several pairs is not moved by one of them.
func consensus(spans []Span, limits Limits) (Span, bool) {
	if len(spans) == 0 {
		return Span{}, false
	}
	starts := make([]time.Duration, len(spans))
	ends := make([]time.Duration, len(spans))
	for i, s := range spans {
		starts[i], ends[i] = s.Start, s.End
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
	sort.Slice(ends, func(i, j int) bool { return ends[i] < ends[j] })

	span := Span{Start: starts[len(starts)/2], End: ends[len(ends)/2]}
	if span.Length() < limits.Min || span.Length() > limits.Max {
		return Span{}, false
	}
	return span, true
}

// sharedRun finds the longest stretch of frames two fingerprints have in
// common, and where it falls in each.
//
// Offsets are proposed by exact hash matches — the two recordings of one intro
// produce identical hashes for at least a few frames — and then each proposal
// is checked by walking both sequences at that alignment and measuring how far
// the agreement extends. The check is what makes the result tolerant: a frame
// may disagree on up to MaxDistance bits, and a handful of bad frames in a row
// ends the run rather than one.
func sharedRun(a, b fingerprint.Print, limits Limits) (Span, Span, bool) {
	if len(a.Hashes) == 0 || len(b.Hashes) == 0 {
		return Span{}, Span{}, false
	}

	index := make(map[uint32][]int32, len(a.Hashes))
	for i, h := range a.Hashes {
		if !fingerprint.Informative(h) {
			continue
		}
		// Frames whose hash occurs everywhere carry no information about
		// alignment — silence is the usual culprit — so a hash that repeats
		// beyond a handful of frames is left out of the index entirely.
		if len(index[h]) > 8 {
			continue
		}
		index[h] = append(index[h], int32(i))
	}

	votes := make(map[int]int, len(b.Hashes))
	for j, h := range b.Hashes {
		if !fingerprint.Informative(h) {
			continue
		}
		for _, i := range index[h] {
			votes[int(i)-j]++
		}
	}
	if len(votes) == 0 {
		return Span{}, Span{}, false
	}

	type offset struct {
		shift int
		count int
	}
	ordered := make([]offset, 0, len(votes))
	for shift, count := range votes {
		ordered = append(ordered, offset{shift, count})
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].count != ordered[j].count {
			return ordered[i].count > ordered[j].count
		}
		return ordered[i].shift < ordered[j].shift
	})

	// Only the best few alignments are checked. A real match is at or near the
	// top of this list; walking the rest would be most of the cost of the pass
	// for none of the answer.
	const checked = 8
	var (
		bestStartA, bestLength int
		bestStartB             int
	)
	for _, cand := range ordered[:min(checked, len(ordered))] {
		startA, startB, length := longestRun(a.Hashes, b.Hashes, cand.shift, limits.MaxDistance)
		if length > bestLength {
			bestStartA, bestStartB, bestLength = startA, startB, length
		}
	}
	if bestLength == 0 {
		return Span{}, Span{}, false
	}

	spanA := Span{Start: a.At(bestStartA), End: a.At(bestStartA + bestLength)}
	spanB := Span{Start: b.At(bestStartB), End: b.At(bestStartB + bestLength)}
	if spanA.Length() < limits.Min || spanA.Length() > limits.Max {
		return Span{}, Span{}, false
	}
	return spanA, spanB, true
}

// longestRun walks two fingerprints at one alignment and returns the longest
// stretch where they agree.
//
// A run survives a few disagreeing frames — a cut, a dropped frame, one
// encoder's transient — and ends when they pile up, which is where the shared
// audio actually stops.
func longestRun(a, b []uint32, shift, maxDistance int) (startA, startB, length int) {
	const tolerated = 4 // consecutive disagreeing frames before a run is over

	// A run may skip over audio that says nothing, but not over much of it:
	// a second of silence inside an intro is a cut, and a minute of it is two
	// unrelated stretches of a file either side of a quiet passage.
	maxQuiet := int(5 * time.Second / fingerprint.FrameDuration)

	var (
		runStart   = -1
		bad        int
		quiet      int
		informed   int
		bestLen    int
		bestAt     = -1
		bestInform int
	)
	for j := range b {
		i := j + shift
		if i < 0 {
			continue
		}
		if i >= len(a) {
			break
		}
		// Frames carrying no information are neither evidence nor
		// counter-evidence: they are stepped over, up to a limit. Counting
		// them as matches is what makes a detector report that every episode
		// of a show shares its every silent passage.
		if !fingerprint.Informative(a[i]) || !fingerprint.Informative(b[j]) {
			if runStart >= 0 {
				quiet++
				if quiet > maxQuiet {
					runStart, bad, quiet, informed = -1, 0, 0, 0
				}
			}
			continue
		}
		if fingerprint.Distance(a[i], b[j]) <= maxDistance {
			bad, quiet = 0, 0
			if runStart < 0 {
				runStart, informed = j, 0
			}
			informed++
			if current := j - runStart + 1; current > bestLen {
				bestLen, bestAt, bestInform = current, runStart, informed
			}
			continue
		}
		if runStart < 0 {
			continue
		}
		bad++
		if bad > tolerated {
			runStart, bad, quiet, informed = -1, 0, 0, 0
		}
	}
	if bestAt < 0 {
		return 0, 0, 0
	}
	// Most of what a run covers has to be audio that said something, or what
	// has been found is mostly a quiet passage that two files happen to share.
	if bestInform*2 < bestLen {
		return 0, 0, 0
	}
	return bestAt + shift, bestAt, bestLen
}
