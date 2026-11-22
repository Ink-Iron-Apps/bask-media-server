// Package jobs runs the library's catch-up passes in the background.
//
// The passes themselves — scan, enrich, detect markers — already existed as
// things the command line could do once and then exit. The admin interface
// needs the same work started from a web page, watched while it runs, and
// stopped halfway, so this is where a pass becomes a job with a state, a
// progress line and a cancel function.
//
// Deliberately in memory and deliberately not a queue. One process runs these,
// they are all resumable by construction, and a job that is lost to a restart
// costs a button press rather than a lost library: a durable queue here would
// be a schema and a reaper and a lock in exchange for nothing.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/inknironapps/bask-media-server/internal/artwork"
	"github.com/inknironapps/bask-media-server/internal/detect"
	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/enrich"
	"github.com/inknironapps/bask-media-server/internal/metadata"
	"github.com/inknironapps/bask-media-server/internal/metadata/tmdb"
	"github.com/inknironapps/bask-media-server/internal/scan"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// TMDBProvider is the provider a server uses, resolved at the moment a pass
// starts rather than when the server came up.
//
// Later, because the key can be saved from the admin interface on a server
// that is already running, and a provider built at startup would be one built
// before there was a key. The stored key wins over the fallback from the
// command line or the environment: the interface is where somebody last said
// what it should be.
func TMDBProvider(db *store.DB, fallback string, concurrency int) func(context.Context) (metadata.Provider, error) {
	return func(ctx context.Context) (metadata.Provider, error) {
		key := fallback
		stored, err := db.ServerValue(ctx, store.KeyTMDB)
		if err != nil {
			return nil, err
		}
		if stored != "" {
			key = stored
		}
		if key == "" {
			return nil, errors.New("no TMDB key: save one in the admin interface, or pass -tmdb-key")
		}
		return tmdb.New(key, concurrency)
	}
}

// Kind is which pass a job runs.
type Kind string

const (
	// KindScan walks a library's folders and writes what it finds.
	KindScan Kind = "scan"
	// KindEnrich asks the metadata provider about anything unanswered.
	KindEnrich Kind = "enrich"
	// KindMarkers finds intros and credits by comparing a season's audio.
	KindMarkers Kind = "markers"
	// KindBackup writes a complete copy of the database beside it.
	KindBackup Kind = "backup"
)

// State is where a job got to.
type State string

const (
	StateRunning  State = "running"
	StateDone     State = "done"
	StateFailed   State = "failed"
	StateCanceled State = "canceled"
)

// ErrBusy is returned when a pass of that kind is already running. Two scans
// of the same library at once would fight over the one SQLite writer and find
// the same files twice, so the answer is to say no rather than to queue.
var ErrBusy = errors.New("a job of that kind is already running")

// ErrUnavailable is returned when the tool a pass needs is missing — no
// ffprobe, no ffmpeg, no metadata key. The message says which.
var ErrUnavailable = errors.New("that pass cannot run on this server")

// Job is one run, as a reader sees it.
type Job struct {
	ID       int64     `json:"id"`
	Kind     Kind      `json:"kind"`
	Target   string    `json:"target,omitempty"`
	State    State     `json:"state"`
	Note     string    `json:"note"`
	Done     int       `json:"done"`
	Total    int       `json:"total"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// Running reports whether this job is still going.
func (j Job) Running() bool { return j.State == StateRunning }

// Elapsed is how long the job ran, or has been running.
func (j Job) Elapsed() time.Duration {
	if j.Finished.IsZero() {
		return time.Since(j.Started)
	}
	return j.Finished.Sub(j.Started)
}

type entry struct {
	job    Job
	cancel context.CancelFunc
}

// Runner owns the background passes.
type Runner struct {
	DB  *store.DB
	Log *slog.Logger

	// Prober measures durations and reads chapters during a scan. Nil means
	// no ffprobe was found: a scan still runs and the library is still
	// playable, it simply has no durations yet.
	Prober scan.Prober

	// Printer fingerprints audio for marker detection. Nil means no ffmpeg,
	// and a marker job refuses rather than pretending to run.
	Printer detect.Printer

	// BackupDir is where database copies are written. Empty means a folder
	// beside the database.
	BackupDir string

	// KeepBackups is how many copies to keep. Zero means seven.
	KeepBackups int

	// Provider is resolved when an enrichment job starts rather than held,
	// because the key can be set from the admin interface after the server
	// came up. Nil, or an error, means an enrichment job refuses.
	Provider func(ctx context.Context) (metadata.Provider, error)

	Artwork *artwork.Cache

	// Workers is how many files marker detection fingerprints at once. Zero
	// leaves the detector's own default, which is half the CPUs.
	Workers int

	// History is how many finished jobs are remembered. Zero means 20.
	History int

	// Notify is told when a pass ends and when a scan found something new.
	// Nil means nobody is listening.
	//
	// A callback rather than this package building webhook events itself,
	// because the notifier reads the same database these passes write and one
	// importing the other would be a cycle the first time either grew.
	// Called from the goroutine that ran the pass, and expected not to block.
	Notify func(Notice)

	mu      sync.Mutex
	next    int64
	entries []*entry // newest first
	wg      sync.WaitGroup
}

// Start begins a pass and returns the job it created.
//
// sectionID scopes a scan to one library; zero scans every library. It is
// ignored by the other kinds, which are library-wide by nature.
func (r *Runner) Start(ctx context.Context, kind Kind, sectionID int64) (Job, error) {
	work, target, err := r.prepare(ctx, kind, sectionID)
	if err != nil {
		return Job{}, err
	}

	r.mu.Lock()
	for _, e := range r.entries {
		if e.job.Kind == kind && e.job.Running() {
			r.mu.Unlock()
			return Job{}, ErrBusy
		}
	}
	r.next++
	// Detached from the request that started it: a job outlives the HTTP call,
	// and cancelling it is a button, not a closed browser tab.
	jobCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	e := &entry{
		job: Job{
			ID:      r.next,
			Kind:    kind,
			Target:  target,
			State:   StateRunning,
			Started: time.Now().UTC(),
			Note:    "starting",
		},
		cancel: cancel,
	}
	r.entries = append([]*entry{e}, r.entries...)
	r.trim()
	started := e.job
	r.mu.Unlock()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer cancel()
		err := work(jobCtx, func(note string, done, total int) {
			r.update(e, note, done, total)
		})
		r.finish(e, err)
	}()
	return started, nil
}

// Can reports whether a pass could run now, and says why not when it could
// not. The admin interface asks so it can explain a greyed-out button rather
// than letting somebody press it and read a refusal.
func (r *Runner) Can(ctx context.Context, kind Kind) error {
	_, _, err := r.prepare(ctx, kind, 0)
	return err
}

// NoticeKind is what a notice is about.
type NoticeKind string

const (
	// NoticeDone and NoticeFailed are a pass that has ended.
	NoticeDone   NoticeKind = "done"
	NoticeFailed NoticeKind = "failed"

	// NoticeAdded is one thing a scan found that was not in the library
	// before.
	NoticeAdded NoticeKind = "added"
)

// Notice is something worth telling somebody about.
type Notice struct {
	Kind NoticeKind

	// Job is the pass this is about, finished for a done or failed notice
	// and still running for an added one.
	Job Job

	// Err is why a pass failed, and nil otherwise.
	Err error

	// Item and Section are what a scan found and which library it landed in,
	// set only on an added notice.
	Item    *domain.Item
	Section string
}

// announceLimit is how many new items a scan names one by one.
//
// A scan that found four new episodes should say which four. A first scan of a
// ten-thousand-film library should not send ten thousand notifications to
// somebody's phone, and the number in the finished-pass notice tells them what
// happened anyway.
const announceLimit = 25

// report is how a pass tells the runner where it is.
type report func(note string, done, total int)

// prepare validates what a job needs and returns the work to do, so a refusal
// happens before a job exists rather than as a job that immediately fails.
func (r *Runner) prepare(ctx context.Context, kind Kind, sectionID int64) (func(context.Context, report) error, string, error) {
	switch kind {
	case KindScan:
		sections, err := r.DB.Sections(ctx)
		if err != nil {
			return nil, "", err
		}
		if sectionID != 0 {
			for _, s := range sections {
				if s.ID == sectionID {
					return func(ctx context.Context, rep report) error {
						return r.scan(ctx, rep, []domain.Section{s})
					}, s.Title, nil
				}
			}
			return nil, "", store.ErrNotFound
		}
		if len(sections) == 0 {
			return nil, "", fmt.Errorf("%w: there are no libraries to scan", ErrUnavailable)
		}
		return func(ctx context.Context, rep report) error {
			return r.scan(ctx, rep, sections)
		}, "every library", nil

	case KindEnrich:
		if r.Provider == nil {
			return nil, "", fmt.Errorf("%w: no metadata provider is configured", ErrUnavailable)
		}
		provider, err := r.Provider(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("%w: %s", ErrUnavailable, err)
		}
		return func(ctx context.Context, rep report) error {
			return r.enrich(ctx, rep, provider)
		}, provider.Name(), nil

	case KindMarkers:
		if r.Printer == nil {
			return nil, "", fmt.Errorf("%w: ffmpeg was not found, so audio cannot be read", ErrUnavailable)
		}
		return r.markers, "", nil

	case KindBackup:
		return r.backup, filepath.Base(r.backupDir()), nil
	}
	return nil, "", fmt.Errorf("unknown job kind %q", kind)
}

func (r *Runner) scan(ctx context.Context, rep report, sections []domain.Section) error {
	scanner := &scan.Scanner{DB: r.DB, Prober: r.Prober, Log: r.logger(), Artwork: r.Artwork}
	var files int
	for i, section := range sections {
		// Each section's own count, added to what earlier sections found, so
		// the number a reader watches only ever goes up.
		before := files
		scanner.Progress = func(n int) {
			rep(fmt.Sprintf("%s: %d files", section.Title, n), before+n, 0)
		}
		result, err := scanner.Section(ctx, section)
		files = before + result.Files
		if err != nil {
			return err
		}
		rep(fmt.Sprintf("%s: %d files, %d removed", section.Title, result.Files, result.Pruned), files, 0)
		if i == len(sections)-1 {
			rep(fmt.Sprintf("%d files in %s", files, libraries(len(sections))), files, files)
		}
	}
	return nil
}

func (r *Runner) enrich(ctx context.Context, rep report, provider metadata.Provider) error {
	stats, err := r.DB.Stats(ctx)
	if err != nil {
		return err
	}
	total := stats.NeedsMetadata

	enricher := &enrich.Enricher{DB: r.DB, Provider: provider, Artwork: r.Artwork, Log: r.logger()}
	enricher.Progress = func(result enrich.Result) {
		done := result.Matched + result.Unmatched
		rep(fmt.Sprintf("%d matched, %d not found, %d images", result.Matched, result.Unmatched, result.Images),
			done, max(total, done))
	}
	result, err := enricher.Run(ctx)
	if err != nil {
		return err
	}
	rep(fmt.Sprintf("%d matched, %d not found, %d images", result.Matched, result.Unmatched, result.Images),
		result.Matched+result.Unmatched, max(total, result.Matched+result.Unmatched))
	return nil
}

// backup writes a copy of the database and prunes the old ones.
//
// A job rather than a handler that does the work itself, so it shows up in
// Activity with everything else, can be cancelled, and cannot be started
// twice at once — a second VACUUM INTO while the first is still writing is
// two copies of a large file at the same time on a machine that may not have
// room for one.
func (r *Runner) backup(ctx context.Context, rep report) error {
	dir := r.backupDir()
	path := filepath.Join(dir, store.BackupName(time.Now()))
	rep("writing "+filepath.Base(path), 0, 1)

	size, err := r.DB.Backup(ctx, path)
	if err != nil {
		return err
	}
	note := fmt.Sprintf("%s · %d MB", filepath.Base(path), size>>20)

	if removed, err := store.PruneBackups(dir, r.keepBackups()); err != nil {
		// The copy is written and good; failing the job over the tidying
		// would say otherwise.
		r.logger().Warn("could not remove old backups", "dir", dir, "error", err)
	} else if removed > 0 {
		note += fmt.Sprintf(" · %d older removed", removed)
	}

	rep(note, 1, 1)
	return nil
}

// backupDir is where copies go: what the operator configured, or beside the
// database.
func (r *Runner) backupDir() string {
	if r.BackupDir != "" {
		return r.BackupDir
	}
	return store.DefaultBackupDir(r.DB.Path())
}

// keepBackups is how many copies are kept. Zero means the default of seven,
// which is a week of nightly copies.
func (r *Runner) keepBackups() int {
	if r.KeepBackups > 0 {
		return r.KeepBackups
	}
	return 7
}

func (r *Runner) markers(ctx context.Context, rep report) error {
	stats, err := r.DB.Stats(ctx)
	if err != nil {
		return err
	}
	total := stats.NeedsMarkers

	detector := &detect.Detector{DB: r.DB, Printer: r.Printer, Log: r.logger(), Workers: r.Workers}
	detector.Progress = func(result detect.Result) {
		rep(markerNote(result), result.Seasons, max(total, result.Seasons))
	}
	result, err := detector.Run(ctx)
	if err != nil {
		return err
	}
	rep(markerNote(result), result.Seasons, max(total, result.Seasons))

	// The sweep belongs to this pass: it drops the cached fingerprints of
	// files that have changed, which is only ever discoverable by having just
	// looked at them.
	if swept, err := r.DB.SweepFingerprints(ctx); err != nil {
		r.logger().Warn("could not sweep stale fingerprints", "error", err)
	} else if swept > 0 {
		r.logger().Info("dropped fingerprints for files that have changed", "count", swept)
	}
	return nil
}

func markerNote(result detect.Result) string {
	return fmt.Sprintf("%d season%s, %d intros, %d credits",
		result.Seasons, plural(result.Seasons), result.Intros, result.Credits)
}

// libraries reads as English rather than as a format string with a plural
// suffix bolted on, which is what "1 librar" was.
func libraries(n int) string {
	if n == 1 {
		return "1 library"
	}
	return fmt.Sprintf("%d libraries", n)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// Cancel stops a running job. A cancelled pass keeps what it has already
// written: every one of them commits as it goes.
func (r *Runner) Cancel(id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.entries {
		if e.job.ID == id {
			if !e.job.Running() {
				return nil
			}
			e.cancel()
			return nil
		}
	}
	return store.ErrNotFound
}

// Jobs lists what has run, newest first.
func (r *Runner) Jobs() []Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Job, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e.job)
	}
	return out
}

// Job reads one.
func (r *Runner) Job(id int64) (Job, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.entries {
		if e.job.ID == id {
			return e.job, true
		}
	}
	return Job{}, false
}

// Shutdown cancels every running job and waits for them to stop.
//
// Called on the way down so no pass outlives the process that owns it, which
// is rule 4 of the resource budget: every goroutine has an owner.
func (r *Runner) Shutdown() {
	r.mu.Lock()
	for _, e := range r.entries {
		if e.job.Running() {
			e.cancel()
		}
	}
	r.mu.Unlock()
	r.wg.Wait()
}

func (r *Runner) update(e *entry, note string, done, total int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e.job.Note = note
	e.job.Done = done
	e.job.Total = total
}

func (r *Runner) finish(e *entry, err error) {
	r.mu.Lock()
	e.job.Finished = time.Now().UTC()
	switch {
	case err == nil:
		e.job.State = StateDone
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		e.job.State = StateCanceled
		e.job.Note = "stopped: " + e.job.Note
	default:
		e.job.State = StateFailed
		e.job.Error = err.Error()
		r.logger().Error("job failed", "kind", string(e.job.Kind), "error", err)
	}
	finished := e.job
	r.mu.Unlock()

	// Outside the lock, and not for a pass somebody cancelled: they know
	// they cancelled it.
	switch finished.State {
	case StateDone:
		if finished.Kind == KindScan {
			r.announce(finished)
		}
		r.notify(Notice{Kind: NoticeDone, Job: finished})
	case StateFailed:
		r.notify(Notice{Kind: NoticeFailed, Job: finished, Err: err})
	}
}

// announce names what a finished scan added.
//
// Here rather than inside the scan itself for two reasons: this is where the
// job and its start time are both in hand, and the question "what is new" is
// one query against added_at rather than a count threaded through the
// scanner's worker pool and its write batches.
//
// Its own context, because the job's was cancelled the moment the pass
// returned. Short, because a library that cannot answer this in five seconds
// has a bigger problem than a missed notification.
func (r *Runner) announce(job Job) {
	if r.Notify == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	added, err := r.DB.AddedSince(ctx, job.Started, announceLimit+1)
	if err != nil {
		r.logger().Error("could not read what a scan added", "error", err)
		return
	}
	if len(added) == 0 || len(added) > announceLimit {
		return
	}

	names := map[int64]string{}
	if sections, err := r.DB.Sections(ctx); err == nil {
		for _, section := range sections {
			names[section.ID] = section.Title
		}
	}
	for _, item := range added {
		r.notify(Notice{
			Kind: NoticeAdded, Job: job,
			Item: &item, Section: names[item.SectionID],
		})
	}
}

// notify hands a notice on, if anybody is listening.
func (r *Runner) notify(notice Notice) {
	if r.Notify == nil {
		return
	}
	r.Notify(notice)
}

// trim keeps the history bounded, which is rule 3: a list that only ever grows
// is the standard way a long-running process leaks.
//
// A running job is kept whatever its age. Dropping one would leave a pass with
// no way to be cancelled and nothing to report it, which is worse than holding
// one more row.
func (r *Runner) trim() {
	keep := r.History
	if keep <= 0 {
		keep = 20
	}
	kept := make([]*entry, 0, len(r.entries))
	var finished int
	for _, e := range r.entries {
		if e.job.Running() {
			kept = append(kept, e)
			continue
		}
		if finished < keep {
			finished++
			kept = append(kept, e)
		}
	}
	r.entries = kept
}

func (r *Runner) logger() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}
