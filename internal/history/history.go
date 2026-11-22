// Package history records what was watched.
//
// The register of live streams knows who is watching what right now and
// deliberately keeps nothing: a stream is true only while it is happening,
// and the register throws it away when it stops. That is the right shape for
// a status page and the wrong shape for the question a household actually
// asks, which is what happened last Tuesday. This is the part that remembers.
//
// Four decisions shape it:
//
//   - A row is opened when a stream starts, kept up to date while it runs,
//     and closed when it ends. Not written at the end: a server that loses
//     power in the middle of a film should still have the film, and what it
//     loses is the last minute of its length.
//   - The names are written into the row, not joined to it. A log that read
//     titles and accounts through the library would become unreadable exactly
//     when it is most wanted — after a film was deleted, an account removed,
//     a title corrected.
//   - Watched is not position. Somebody who skips to the last ten minutes of
//     a film has a position of two hours and watched ten minutes. Both are
//     recorded, and the time it sat paused is subtracted from one of them.
//   - A failure to record is never a failure to play. Every write here is on
//     the path of a request a viewer is waiting for, so a database error is
//     logged and the stream goes on.
package history

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/inknironapps/bask-media-server/internal/nowplaying"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// writeTimeout bounds one write. Generous for a local database and finite
// because this runs inside somebody's play request.
const writeTimeout = 5 * time.Second

// Store is what this package needs of the database.
type Store interface {
	StartWatch(ctx context.Context, in store.NewWatch) (int64, error)
	TouchWatch(ctx context.Context, id int64, in store.WatchProgress) error
}

// Recorder keeps the log.
type Recorder struct {
	db  Store
	log *slog.Logger
	now func() time.Time

	// mu guards open, which is the row this process has for each live
	// stream, plus what it knows about that stream's pauses.
	mu   sync.Mutex
	open map[string]*live
}

// live is one stream's row and its pause accounting.
type live struct {
	id int64

	// paused is how long this stream has been paused in total, and since
	// when it has been paused now. A pause that is still running is counted
	// up to the moment somebody asks, so a film left paused does not read as
	// one being watched.
	paused time.Duration
	since  time.Time
}

// Options configure a recorder.
type Options struct {
	DB  Store
	Log *slog.Logger

	// Now is the clock, for tests.
	Now func() time.Time
}

// New prepares a recorder.
func New(opts Options) *Recorder {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Recorder{db: opts.DB, log: log, now: now, open: map[string]*live{}}
}

// Record takes one change from the register of live streams.
//
// Safe on a nil recorder, so a server built without one needs no condition at
// the call site.
func (r *Recorder) Record(change nowplaying.Change) {
	if r == nil || change.Stream.Key == "" {
		return
	}
	switch change.Kind {
	case nowplaying.Started:
		r.start(change.Stream)
	case nowplaying.Paused:
		r.pause(change.Stream)
	case nowplaying.Resumed:
		r.resume(change.Stream)
	case nowplaying.Stopped:
		r.stop(change.Stream)
	}
}

func (r *Recorder) start(stream nowplaying.Stream) {
	// A key that is somehow already open belongs to a stream this process
	// never saw end. Close it before opening another, or the two would share
	// a row and the first would never be finished.
	r.stop(stream)

	started := stream.Since
	if started.IsZero() {
		started = r.now().UTC()
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()

	id, err := r.db.StartWatch(ctx, store.NewWatch{
		UserID: stream.UserID, Name: stream.UserName,
		ItemID: stream.ItemID, Title: stream.Title, Subtitle: stream.Subtitle,
		Surface: stream.Surface, Client: stream.Client, Device: stream.Device,
		Address: stream.Address, Mode: stream.Mode,
		Started: started, Position: stream.Position, Length: stream.Duration,
	})
	if err != nil {
		r.log.Error("could not record the start of a stream", "error", err)
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	entry := &live{id: id}
	if stream.Paused {
		entry.since = r.now().UTC()
	}
	r.open[stream.Key] = entry
}

func (r *Recorder) pause(stream nowplaying.Stream) {
	now := r.now().UTC()

	r.mu.Lock()
	entry, found := r.open[stream.Key]
	if !found {
		r.mu.Unlock()
		return
	}
	if entry.since.IsZero() {
		entry.since = now
	}
	id, progress := entry.id, r.progressLocked(entry, stream, now, false)
	r.mu.Unlock()

	r.touch(id, progress)
}

func (r *Recorder) resume(stream nowplaying.Stream) {
	now := r.now().UTC()

	r.mu.Lock()
	entry, found := r.open[stream.Key]
	if !found {
		r.mu.Unlock()
		return
	}
	if !entry.since.IsZero() {
		entry.paused += now.Sub(entry.since)
		entry.since = time.Time{}
	}
	id, progress := entry.id, r.progressLocked(entry, stream, now, false)
	r.mu.Unlock()

	r.touch(id, progress)
}

func (r *Recorder) stop(stream nowplaying.Stream) {
	now := r.now().UTC()

	r.mu.Lock()
	entry, found := r.open[stream.Key]
	if !found {
		r.mu.Unlock()
		return
	}
	delete(r.open, stream.Key)
	id, progress := entry.id, r.progressLocked(entry, stream, now, true)
	r.mu.Unlock()

	r.touch(id, progress)
}

// Sweep brings every open row up to date, and opens one for any live stream
// this recorder has no row for.
//
// The heartbeat. Without it a row's length would be fixed at the moment the
// stream started and only corrected when it ended, so a server that went down
// mid-film would record a film that lasted no time at all. With it, a crash
// costs however long it has been since the last sweep.
//
// It also recovers: a stream whose row failed to be written, or one that was
// already playing when this recorder was built, gets a row here rather than
// going unrecorded for the rest of the evening.
func (r *Recorder) Sweep(streams []nowplaying.Stream) {
	if r == nil {
		return
	}
	now := r.now().UTC()

	type pending struct {
		id       int64
		progress store.WatchProgress
	}
	var (
		writes  []pending
		missing []nowplaying.Stream
	)

	r.mu.Lock()
	seen := make(map[string]struct{}, len(streams))
	for _, stream := range streams {
		seen[stream.Key] = struct{}{}
		entry, found := r.open[stream.Key]
		if !found {
			missing = append(missing, stream)
			continue
		}
		writes = append(writes, pending{id: entry.id, progress: r.progressLocked(entry, stream, now, false)})
	}
	// A row this process holds for a stream the register no longer has. The
	// stop was missed — which should not happen, and would otherwise leave
	// the row open for the life of the process.
	var orphans []int64
	for key, entry := range r.open {
		if _, live := seen[key]; !live {
			orphans = append(orphans, entry.id)
			delete(r.open, key)
		}
	}
	r.mu.Unlock()

	for _, write := range writes {
		r.touch(write.id, write.progress)
	}
	for _, id := range orphans {
		r.touch(id, store.WatchProgress{At: now, Done: true})
	}
	for _, stream := range missing {
		r.start(stream)
	}
}

// Flush brings every open row up to date without closing it.
//
// For shutdown. The streams did not end, the server did, so the rows are
// left incomplete on purpose: "the server never saw this stop" is a true
// thing to be able to say later, and pretending every stream ended neatly at
// the moment of a restart would be a lie in the log.
func (r *Recorder) Flush(streams []nowplaying.Stream) {
	if r == nil {
		return
	}
	now := r.now().UTC()

	r.mu.Lock()
	type pending struct {
		id       int64
		progress store.WatchProgress
	}
	writes := make([]pending, 0, len(streams))
	for _, stream := range streams {
		if entry, found := r.open[stream.Key]; found {
			writes = append(writes, pending{id: entry.id, progress: r.progressLocked(entry, stream, now, false)})
		}
	}
	r.mu.Unlock()

	for _, write := range writes {
		r.touch(write.id, write.progress)
	}
}

// Watching reports how many streams this recorder has rows for, for a test
// and for the status page.
func (r *Recorder) Watching() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.open)
}

// progressLocked is what to write about a stream now. The caller holds mu,
// because the pause accounting is read from the entry.
func (r *Recorder) progressLocked(entry *live, stream nowplaying.Stream, now time.Time, done bool) store.WatchProgress {
	paused := time.Duration(0)
	if entry != nil {
		paused = entry.paused
		if !entry.since.IsZero() {
			// A pause that is still running counts up to now, so a film left
			// paused does not read as one being watched.
			paused += now.Sub(entry.since)
		}
	}
	return store.WatchProgress{
		At: now, Position: stream.Position, Length: stream.Duration,
		Paused: paused, Mode: stream.Mode, Reason: stream.Reason, Done: done,
	}
}

func (r *Recorder) touch(id int64, progress store.WatchProgress) {
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	if err := r.db.TouchWatch(ctx, id, progress); err != nil {
		r.log.Error("could not record where a stream had got to", "error", err)
	}
}
