package jobs

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/inknironapps/bask-media-server/internal/store"
)

// Scanning a library without being asked.
//
// Periodic and not a filesystem watch, which is a decision rather than a
// shortcut. inotify and its equivalents report changes on a local
// filesystem; a library on a network share — which is where a household's
// media usually lives, and certainly where this one's does — produces no
// events at all when a file is added from the other machine. A watch would
// therefore work on exactly the setups that need it least and fail silently
// on the rest, which is worse than a schedule that works everywhere.
//
// It is affordable because a rescan of an unchanged library costs a stat per
// file and no writes: the scanner compares path, size and mtime and leaves
// the row alone. So the interval is a question of how soon a new film should
// appear, not of how much work the server is willing to do.

// sweepEvery is how often the scheduler looks for a library that is due.
//
// Finer than any interval a library can be set to, so a library due at a
// quarter past is scanned within a minute of a quarter past rather than at
// the next sweep. Coarse enough that the loop is invisible: one query
// against a table with a handful of rows.
const sweepEvery = time.Minute

// serverPasses are the passes with one schedule for the whole server, as
// opposed to the scan, which has one per library.
//
// Server-wide because that is what they are: each asks the whole database
// what is still unanswered and works through it, so an interval per library
// would be several schedules competing for one queue. Which libraries they
// touch is the per-library setting.
var serverPasses = []struct {
	kind Kind
	key  string
}{
	{KindEnrich, store.KeyEnrichEvery},
	{KindMarkers, store.KeyMarkersEvery},
}

// Schedule scans the libraries that asked to be scanned, until the context
// ends.
//
// One library at a time, because the runner already refuses a second scan
// while one is running and a scheduler that fought it would spend its sweeps
// collecting ErrBusy. A library that loses the race is simply due again next
// sweep.
func (r *Runner) Schedule(ctx context.Context) {
	log := r.logger()
	ticker := time.NewTicker(sweepEvery)
	defer ticker.Stop()

	// The first sweep happens immediately, so a server that has just started
	// picks up a library that was due while it was down. The alternative is
	// a restart quietly costing one interval.
	r.sweep(ctx, log)
	r.sweepPasses(ctx, log)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.sweep(ctx, log)
			r.sweepPasses(ctx, log)
		}
	}
}

// sweepPasses starts the server-wide passes that are due.
//
// A scan that finds a new film is only half of what somebody wants: without
// this, the film sits there with no poster and no summary until somebody
// presses the button, which is the same gap automatic scanning was meant to
// close.
//
// Each pass tracks when it last ran in the server table, written before the
// pass starts for the reason a library's visit is: a pass that fails must be
// retried at its own interval rather than every minute.
func (r *Runner) sweepPasses(ctx context.Context, log *slog.Logger) {
	if err := ctx.Err(); err != nil {
		return
	}
	now := time.Now().UTC()

	for _, pass := range serverPasses {
		every, err := r.DB.Minutes(ctx, pass.key)
		if err != nil {
			log.Warn("could not read a pass schedule", "pass", pass.kind, "error", err)
			continue
		}
		if every <= 0 {
			continue
		}
		last, err := r.DB.LastRun(ctx, pass.key)
		if err != nil {
			log.Warn("could not read when a pass last ran", "pass", pass.kind, "error", err)
			continue
		}
		if !last.IsZero() && now.Before(last.Add(time.Duration(every)*time.Minute)) {
			continue
		}

		// Nothing to do is not worth starting. A pass with an empty queue
		// would appear in Activity, finish immediately and reset its clock,
		// which is a log line every interval saying nothing happened.
		if r.Can(ctx, pass.kind) != nil {
			continue
		}
		waiting, err := r.waiting(ctx, pass.kind)
		if err != nil {
			log.Warn("could not count what a pass has to do", "pass", pass.kind, "error", err)
			continue
		}
		if waiting == 0 {
			continue
		}

		if err := r.DB.MarkRun(ctx, pass.key, now); err != nil {
			log.Warn("could not record a pass", "pass", pass.kind, "error", err)
			continue
		}
		job, err := r.Start(ctx, pass.kind, 0)
		switch {
		case errors.Is(err, ErrBusy):
			// Somebody pressed the button, or the last one is still going.
			continue
		case err != nil:
			log.Warn("could not start a scheduled pass", "pass", pass.kind, "error", err)
			continue
		}
		log.Info("running a pass on schedule",
			"pass", pass.kind, "job", job.ID, "waiting", waiting,
			"every", (time.Duration(every) * time.Minute).String())
	}
}

// waiting is how much a pass has to do, so one with an empty queue is not
// started at all.
func (r *Runner) waiting(ctx context.Context, kind Kind) (int, error) {
	stats, err := r.DB.Stats(ctx)
	if err != nil {
		return 0, err
	}
	switch kind {
	case KindEnrich:
		return stats.NeedsMetadata, nil
	case KindMarkers:
		return stats.NeedsMarkers, nil
	}
	return 0, nil
}

// sweep starts a scan for the first library that is due.
func (r *Runner) sweep(ctx context.Context, log *slog.Logger) {
	if err := ctx.Err(); err != nil {
		return
	}
	now := time.Now().UTC()
	due, err := r.DB.DueForScan(ctx, now)
	if err != nil {
		log.Warn("could not look for libraries to scan", "error", err)
		return
	}
	if len(due) == 0 {
		return
	}

	section := due[0]
	// Recorded before the scan is started, not after it finishes. A scan
	// that fails, or one that is still running when the next sweep comes
	// round, must not be retried immediately — the visit is what the
	// interval is measured from, so a broken library is tried again at its
	// own pace rather than every minute.
	if err := r.DB.MarkSwept(ctx, section.ID, now); err != nil {
		log.Warn("could not record a library visit", "library", section.Title, "error", err)
		return
	}

	job, err := r.Start(ctx, KindScan, section.ID)
	switch {
	case errors.Is(err, ErrBusy):
		// Somebody pressed the button, or the last scheduled scan is still
		// going. Nothing to report: it is due again next sweep.
		return
	case err != nil:
		log.Warn("could not start a scheduled scan", "library", section.Title, "error", err)
		return
	}
	log.Info("scanning on schedule",
		"library", section.Title, "job", job.ID,
		"every", section.Settings.ScanInterval().String())
}

// Scheduled reports what the scheduler would do, for the overview page: the
// libraries that scan themselves and when each is next due.
//
// A read, not a side effect. The page says "Films, every 6 hours, next in 41
// minutes" so somebody can tell a library that is set up from one that is
// merely quiet.
type Scheduled struct {
	SectionID int64
	Title     string
	Every     time.Duration
	Next      time.Time
}

// Upcoming lists the libraries that scan themselves, soonest first.
func (r *Runner) Upcoming(ctx context.Context) ([]Scheduled, error) {
	sections, err := r.DB.Sections(ctx)
	if err != nil {
		return nil, err
	}
	var out []Scheduled
	for _, section := range sections {
		every := section.Settings.ScanInterval()
		if every <= 0 {
			continue
		}
		next := section.SweptAt.Add(every)
		if section.SweptAt.IsZero() {
			// Never swept, so due now rather than in 1970.
			next = time.Now().UTC()
		}
		out = append(out, Scheduled{
			SectionID: section.ID, Title: section.Title, Every: every, Next: next,
		})
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Next.Before(out[j-1].Next); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}
