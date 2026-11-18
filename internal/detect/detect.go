// Package detect runs marker detection over a library.
//
// One season at a time, because a season is the set of files that share an
// intro: the pass fingerprints each episode's first and last ten minutes, asks
// the matcher what they have in common, and stores the answer. A season it has
// already looked at is skipped, so the expensive part happens once per file
// rather than once per pass — and once per playback is what the client used to
// do instead.
//
// What this costs, which is why it is a separate pass and not part of a scan:
// one ffmpeg decode of twenty minutes of audio per episode, at a few seconds
// each. A library of five thousand episodes is therefore hours of work the
// first time and nothing on every pass after.
package detect

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/fingerprint"
	"github.com/inknironapps/bask-media-server/internal/markers"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// Printer is what fingerprints a file. An interface so a test can run the
// whole pass without ffmpeg and without inventing audio.
type Printer interface {
	Head(ctx context.Context, path string, window time.Duration) (fingerprint.Print, error)
	Tail(ctx context.Context, path string, duration, window time.Duration) (fingerprint.Print, error)
}

// Detector finds markers by comparing episodes of a season.
type Detector struct {
	DB      *store.DB
	Printer Printer
	Log     *slog.Logger

	// HeadWindow and TailWindow are how much of each end is examined. Ten
	// minutes each by default: an intro is inside the first ten minutes of
	// anything, and credits inside the last ten of all but a feature film.
	HeadWindow time.Duration
	TailWindow time.Duration

	// Workers fingerprint files in parallel. Zero means half the CPUs,
	// minimum one: each worker is an ffmpeg process, so this is the knob that
	// decides whether a detection pass leaves the machine usable.
	Workers int

	// MaxEpisodes bounds one season's working set. Fingerprints are about
	// 19 KiB per end, so sixty episodes is a couple of megabytes — but a
	// misparsed library that put four hundred files in one season must not
	// turn into a hundred megabytes of held fingerprints.
	MaxEpisodes int
}

// Result is what one pass did.
type Result struct {
	Seasons  int
	Episodes int
	Intros   int
	Credits  int
	Elapsed  time.Duration
}

const (
	defaultWindow      = 10 * time.Minute
	defaultMaxEpisodes = 60
	seasonBatch        = 50
)

// Run detects markers for every season that has not been looked at.
//
// Resumable by construction: each season is recorded as detected as it
// finishes, so a pass that is interrupted — or a server that is stopped —
// resumes where it left off rather than starting the library again.
func (d *Detector) Run(ctx context.Context) (Result, error) {
	started := time.Now()
	var result Result

	for {
		seasons, err := d.DB.SeasonsNeedingMarkers(ctx, seasonBatch)
		if err != nil {
			return result, err
		}
		if len(seasons) == 0 {
			break
		}
		for _, season := range seasons {
			if err := ctx.Err(); err != nil {
				result.Elapsed = time.Since(started)
				return result, err
			}
			intros, credits, episodes, err := d.Season(ctx, season)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					result.Elapsed = time.Since(started)
					return result, err
				}
				// One unreadable season must not abandon a library.
				d.logger().Warn("could not detect markers", "season", season.ID, "error", err)
				continue
			}
			result.Seasons++
			result.Episodes += episodes
			result.Intros += intros
			result.Credits += credits
		}
	}

	result.Elapsed = time.Since(started)
	d.logger().Info("detected markers",
		"seasons", result.Seasons, "episodes", result.Episodes,
		"intros", result.Intros, "credits", result.Credits,
		"elapsed", result.Elapsed.Round(time.Second))
	return result, nil
}

// Season detects markers for one season and records that it has been seen.
func (d *Detector) Season(ctx context.Context, season domain.Item) (intros, credits, episodes int, err error) {
	list, err := d.DB.EpisodesWithParts(ctx, season.ID)
	if err != nil {
		return 0, 0, 0, err
	}
	if maxEpisodes := d.maxEpisodes(); len(list) > maxEpisodes {
		d.logger().Warn("season is larger than one working set; detecting from the first episodes only",
			"season", season.ID, "episodes", len(list), "using", maxEpisodes)
		list = list[:maxEpisodes]
	}

	heads, tails := d.fingerprintAll(ctx, list)
	if err := ctx.Err(); err != nil {
		return 0, 0, 0, err
	}

	introSpans := markers.FromAudio(heads, markers.IntroLimits)
	creditSpans := markers.FromAudio(tails, markers.CreditsLimits)

	for _, episode := range list {
		var found []domain.Marker
		if span, ok := introSpans[episode.ID]; ok && plausibleIntro(span, episode.Duration) {
			found = append(found, domain.Marker{
				ItemID: episode.ID,
				Type:   domain.MarkerIntro,
				Start:  span.Start,
				End:    span.End,
				Source: domain.SourceAudio,
			})
			intros++
		}
		if span, ok := creditSpans[episode.ID]; ok && plausibleCredits(span, episode.Duration) {
			found = append(found, domain.Marker{
				ItemID: episode.ID,
				Type:   domain.MarkerCredits,
				Start:  span.Start,
				End:    span.End,
				Source: domain.SourceAudio,
			})
			credits++
		}
		// Written in one call per episode, including the empty case: both
		// kinds share the audio source, so two calls would have the second
		// delete the first, and an episode that no longer matches has to lose
		// the marker it used to have.
		if err := d.DB.ReplaceMarkers(ctx, episode.ID, domain.SourceAudio, found); err != nil {
			return intros, credits, len(list), err
		}
	}

	if err := d.DB.MarkSeasonDetected(ctx, season.ID, time.Now().UTC()); err != nil {
		return intros, credits, len(list), err
	}
	return intros, credits, len(list), nil
}

// fingerprintAll produces both ends of every episode, from the cache where it
// can and from ffmpeg where it cannot.
//
// Bounded workers, one ffmpeg each. The results are collected under a mutex
// rather than through a channel because the set is one season and the work per
// item is seconds: the lock is never the contended thing here.
func (d *Detector) fingerprintAll(ctx context.Context, episodes []domain.Item) (heads, tails []fingerprint.Print) {
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	work := make(chan domain.Item)

	for range d.workers() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for episode := range work {
				head, tail, ok := d.fingerprintOne(ctx, episode)
				mu.Lock()
				if ok.head {
					heads = append(heads, head)
				}
				if ok.tail {
					tails = append(tails, tail)
				}
				mu.Unlock()
			}
		}()
	}

	for _, episode := range episodes {
		select {
		case work <- episode:
		case <-ctx.Done():
			close(work)
			wg.Wait()
			return heads, tails
		}
	}
	close(work)
	wg.Wait()
	return heads, tails
}

type gotEnds struct{ head, tail bool }

func (d *Detector) fingerprintOne(ctx context.Context, episode domain.Item) (fingerprint.Print, fingerprint.Print, gotEnds) {
	var (
		head, tail fingerprint.Print
		got        gotEnds
	)
	part := episode.Parts[0]

	if print, err := d.endFor(ctx, episode, part, store.SegmentHead); err == nil {
		head, got.head = print, true
	} else if !errors.Is(err, context.Canceled) {
		d.logger().Debug("could not fingerprint the opening", "path", part.Path, "error", err)
	}

	// The tail is skipped when the duration is unknown, because the offset a
	// marker would carry is measured back from the end of the file: without a
	// duration it would be reported at the wrong place in the file, which is a
	// Skip button that jumps into the episode.
	if episode.Duration > 0 {
		if print, err := d.endFor(ctx, episode, part, store.SegmentTail); err == nil {
			tail, got.tail = print, true
		} else if !errors.Is(err, context.Canceled) {
			d.logger().Debug("could not fingerprint the credits", "path", part.Path, "error", err)
		}
	}
	return head, tail, got
}

// endFor returns one end's fingerprint, computing and storing it on a miss.
func (d *Detector) endFor(ctx context.Context, episode domain.Item, part domain.Part, segment store.Segment) (fingerprint.Print, error) {
	cached, err := d.DB.Fingerprint(ctx, part.ID, segment, part.Size, part.ModifiedAt)
	if err == nil {
		return fingerprint.Print{
			ItemID: episode.ID,
			Offset: cached.Offset,
			Hashes: fingerprint.Decode(cached.Data),
		}, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return fingerprint.Print{}, err
	}

	// Clamped to half the file so the two ends cannot overlap. Without this a
	// twenty-minute window over a ten-minute episode fingerprints the whole
	// thing twice, and the opening gets credited with the closing: the
	// detector then reports an intro that runs to the end of the episode.
	window := d.windowFor(segment, episode.Duration)

	var print fingerprint.Print
	if segment == store.SegmentHead {
		print, err = d.Printer.Head(ctx, part.Path, window)
	} else {
		print, err = d.Printer.Tail(ctx, part.Path, episode.Duration, window)
	}
	if err != nil {
		return fingerprint.Print{}, err
	}
	if len(print.Hashes) == 0 {
		return fingerprint.Print{}, errors.New("no audio was decoded")
	}
	print.ItemID = episode.ID

	if err := d.DB.SaveFingerprint(ctx, part.ID, segment, print.Offset,
		fingerprint.Encode(print.Hashes), part.Size, part.ModifiedAt); err != nil {
		// Worth having without being worth failing for: the pass can finish
		// on a fingerprint it could not cache, and the next pass recomputes.
		d.logger().Warn("could not cache a fingerprint", "path", part.Path, "error", err)
	}
	return print, nil
}

// plausibleIntro and plausibleCredits are where each kind of marker is allowed
// to be.
//
// The matcher only knows that two files share a stretch of audio. What makes
// that stretch an intro is also that it is near the start, and what makes one
// the credits is that the file ends shortly after it. Shows do put a shared
// "previously on" or a sponsor read in the middle of an episode, and without
// this a Skip Intro button appears over it.
const (
	introMustStartWithin = 5 * time.Minute
	creditsMustEndWithin = 5 * time.Minute
)

func plausibleIntro(span markers.Span, duration time.Duration) bool {
	if span.Start > introMustStartWithin {
		return false
	}
	// An "intro" that reaches the end of the episode is the whole episode.
	return duration <= 0 || span.End < duration
}

func plausibleCredits(span markers.Span, duration time.Duration) bool {
	if duration <= 0 {
		// No duration means nothing to measure the end against, and the
		// fingerprint's own offset was a guess: see Printer.Tail.
		return false
	}
	return duration-span.End <= creditsMustEndWithin && span.Start > 0
}

func (d *Detector) workers() int {
	if d.Workers > 0 {
		return d.Workers
	}
	return max(1, runtime.NumCPU()/2)
}

// windowFor is how much of one end to fingerprint, never more than half the
// file.
func (d *Detector) windowFor(segment store.Segment, duration time.Duration) time.Duration {
	window := d.headWindow()
	if segment == store.SegmentTail {
		window = d.tailWindow()
	}
	if duration > 0 && window > duration/2 {
		window = duration / 2
	}
	return window
}

func (d *Detector) headWindow() time.Duration {
	if d.HeadWindow > 0 {
		return d.HeadWindow
	}
	return defaultWindow
}

func (d *Detector) tailWindow() time.Duration {
	if d.TailWindow > 0 {
		return d.TailWindow
	}
	return defaultWindow
}

func (d *Detector) maxEpisodes() int {
	if d.MaxEpisodes > 0 {
		return d.MaxEpisodes
	}
	return defaultMaxEpisodes
}

func (d *Detector) logger() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}
