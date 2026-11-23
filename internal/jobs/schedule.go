package jobs

import (
	"context"
	"errors"
	"log/slog"
	"time"
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
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.sweep(ctx, log)
		}
	}
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
