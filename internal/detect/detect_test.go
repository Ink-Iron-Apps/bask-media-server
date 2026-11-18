package detect_test

import (
	"context"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/inknironapps/bask-media-server/internal/detect"
	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/ffprobe"
	"github.com/inknironapps/bask-media-server/internal/fingerprint"
	"github.com/inknironapps/bask-media-server/internal/scan"
	"github.com/inknironapps/bask-media-server/internal/store"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("modernc.org/sqlite.(*conn).interruptOnDone.func1"),
	)
}

const episodeLength = 22 * time.Minute

// printer stands in for ffmpeg: every episode of the fixture show opens with
// the same intro and ends with the same credits, with content of its own in
// between, which is what a season actually looks like to a fingerprint.
type printer struct {
	mu    sync.Mutex
	calls map[string]int

	introFrames   int
	creditsFrames int
}

func newPrinter(intro, credits time.Duration) *printer {
	return &printer{
		calls:         map[string]int{},
		introFrames:   int(intro / fingerprint.FrameDuration),
		creditsFrames: int(credits / fingerprint.FrameDuration),
	}
}

func (p *printer) record(path string) {
	p.mu.Lock()
	p.calls[path]++
	p.mu.Unlock()
}

func (p *printer) total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	var sum int
	for _, n := range p.calls {
		sum += n
	}
	return sum
}

func noise(seed uint64, n int) []uint32 {
	r := rand.New(rand.NewPCG(seed, 0x9e3779b9))
	out := make([]uint32, n)
	for i := range out {
		out[i] = r.Uint32()
	}
	return out
}

// seedFor gives each file its own content, keyed by path so a re-run of the
// same file produces the same audio.
func seedFor(path string) uint64 {
	var h uint64 = 1469598103934665603
	for _, b := range []byte(path) {
		h = (h ^ uint64(b)) * 1099511628211
	}
	return h
}

func (p *printer) Head(_ context.Context, path string, window time.Duration) (fingerprint.Print, error) {
	p.record(path)
	frames := int(window / fingerprint.FrameDuration)
	hashes := make([]uint32, 0, frames)
	hashes = append(hashes, noise(1, p.introFrames)...)
	hashes = append(hashes, noise(seedFor(path), frames-p.introFrames)...)
	return fingerprint.Print{Offset: 0, Hashes: hashes}, nil
}

func (p *printer) Tail(_ context.Context, path string, duration, window time.Duration) (fingerprint.Print, error) {
	p.record(path)
	frames := int(window / fingerprint.FrameDuration)
	hashes := make([]uint32, 0, frames)
	hashes = append(hashes, noise(seedFor(path)+1, frames-p.creditsFrames)...)
	hashes = append(hashes, noise(2, p.creditsFrames)...)
	return fingerprint.Print{Offset: duration - window, Hashes: hashes}, nil
}

// season stands up a real database over a real directory tree and scans it.
func season(t *testing.T, episodes int) (*store.DB, domain.Item) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()

	for i := 1; i <= episodes; i++ {
		path := filepath.Join(root, "Dinotopia (2002)", "Season 01",
			"S01E0"+string(rune('0'+i))+" - Part "+string(rune('0'+i))+".mkv")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, 1024*i), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bms.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	section, err := db.CreateSection(ctx, "TV Shows", domain.SectionShows, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	scanner := &scan.Scanner{DB: db, Workers: 2, Prober: constantProber{}}
	if _, err := scanner.Section(ctx, *section); err != nil {
		t.Fatal(err)
	}

	seasons, err := db.SeasonsNeedingMarkers(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(seasons) != 1 {
		t.Fatalf("got %d seasons needing markers, want 1", len(seasons))
	}
	return db, seasons[0]
}

// constantProber gives every episode the same runtime, which is what the
// credits offsets are measured back from.
type constantProber struct{}

func (constantProber) Probe(context.Context, string) (ffprobe.Info, error) {
	return ffprobe.Info{Duration: episodeLength}, nil
}

func TestAPassMarksWhatASeasonShares(t *testing.T) {
	ctx := context.Background()
	db, s := season(t, 4)
	p := newPrinter(85*time.Second, 60*time.Second)

	detector := &detect.Detector{DB: db, Printer: p, Workers: 2}
	result, err := detector.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if result.Seasons != 1 || result.Episodes != 4 {
		t.Fatalf("result = %+v", result)
	}
	if result.Intros != 4 || result.Credits != 4 {
		t.Fatalf("want a marker of each kind per episode, got %+v", result)
	}

	for _, episode := range episodesOf(t, db, s.ID) {
		found, err := db.Markers(ctx, []int64{episode.ID})
		if err != nil {
			t.Fatal(err)
		}
		list := found[episode.ID]
		if len(list) != 2 {
			t.Fatalf("episode %d: got %d markers: %+v", episode.ID, len(list), list)
		}

		intro := list[0]
		if intro.Type != domain.MarkerIntro || intro.Source != domain.SourceAudio {
			t.Errorf("episode %d: first marker = %+v", episode.ID, intro)
		}
		if intro.Start > 2*time.Second {
			t.Errorf("episode %d: intro starts at %v, want the beginning", episode.ID, intro.Start)
		}
		if diff := intro.Duration() - 85*time.Second; diff < -2*time.Second || diff > 2*time.Second {
			t.Errorf("episode %d: intro is %v, want about 85s", episode.ID, intro.Duration())
		}

		credits := list[1]
		if credits.Type != domain.MarkerCredits {
			t.Errorf("episode %d: second marker = %+v", episode.ID, credits)
		}
		// Measured back from the end of the file, which is the whole reason
		// the fingerprint carries the offset it started at.
		if diff := episodeLength - credits.End; diff < -2*time.Second || diff > 2*time.Second {
			t.Errorf("episode %d: credits end at %v, want the end of a %v episode",
				episode.ID, credits.End, episodeLength)
		}
	}
}

// The expensive part has to happen once. A second pass over a library that has
// not changed must decode nothing at all.
func TestASecondPassDecodesNothing(t *testing.T) {
	ctx := context.Background()
	db, _ := season(t, 3)
	p := newPrinter(85*time.Second, 60*time.Second)
	detector := &detect.Detector{DB: db, Printer: p, Workers: 2}

	if _, err := detector.Run(ctx); err != nil {
		t.Fatal(err)
	}
	after := p.total()
	if after == 0 {
		t.Fatal("the first pass decoded nothing")
	}

	result, err := detector.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Seasons != 0 {
		t.Errorf("a second pass looked at %d seasons, want none", result.Seasons)
	}
	if p.total() != after {
		t.Errorf("a second pass decoded %d more files", p.total()-after)
	}
}

// A season that is looked at again — a new episode arrived — must not decode
// the episodes it already has.
func TestAReopenedSeasonReusesItsFingerprints(t *testing.T) {
	ctx := context.Background()
	db, s := season(t, 3)
	p := newPrinter(85*time.Second, 60*time.Second)
	detector := &detect.Detector{DB: db, Printer: p, Workers: 1}

	if _, err := detector.Run(ctx); err != nil {
		t.Fatal(err)
	}
	first := p.total()

	if err := db.ClearSeasonDetection(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := detector.Run(ctx); err != nil {
		t.Fatal(err)
	}

	if p.total() != first {
		t.Errorf("re-detecting decoded %d files again, want 0", p.total()-first)
	}
}

// Nothing in common is the common case on a library of films, and the answer
// has to be no marker rather than a guess.
func TestEpisodesWithNothingInCommonGetNoMarkers(t *testing.T) {
	ctx := context.Background()
	db, s := season(t, 3)
	p := newPrinter(0, 0) // no shared intro, no shared credits

	detector := &detect.Detector{DB: db, Printer: p, Workers: 2}
	result, err := detector.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if result.Intros != 0 || result.Credits != 0 {
		t.Fatalf("markers invented from nothing: %+v", result)
	}
	// Still recorded as looked at, or every pass would decode this show again
	// for the rest of the library's life.
	seasons, err := db.SeasonsNeedingMarkers(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(seasons) != 0 {
		t.Errorf("a season with no markers is still a season that was checked: %+v", seasons)
	}
	_ = s
}

// A scan that changes a file reopens its season, because the file's intro may
// be somewhere else now and a marker measured from the old file would skip
// into the episode.
func TestAChangedFileReopensItsSeason(t *testing.T) {
	ctx := context.Background()
	db, s := season(t, 3)
	p := newPrinter(85*time.Second, 60*time.Second)
	detector := &detect.Detector{DB: db, Printer: p, Workers: 2}
	if _, err := detector.Run(ctx); err != nil {
		t.Fatal(err)
	}

	episodes := episodesOf(t, db, s.ID)
	parts, err := db.Parts(ctx, episodes[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	// A better rip of the same episode: same path, different file.
	if err := os.WriteFile(parts[0].Path, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(parts[0].Path, later, later); err != nil {
		t.Fatal(err)
	}

	sections, err := db.Sections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scanner := &scan.Scanner{DB: db, Workers: 2, Prober: constantProber{}}
	if _, err := scanner.Section(ctx, sections[0]); err != nil {
		t.Fatal(err)
	}

	seasons, err := db.SeasonsNeedingMarkers(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(seasons) != 1 {
		t.Fatalf("a changed file should reopen its season, got %d", len(seasons))
	}

	before := p.total()
	if _, err := detector.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// Only the changed file is decoded again: its fingerprint no longer
	// describes what is on disk, and the other two still do.
	if decoded := p.total() - before; decoded != 2 {
		t.Errorf("decoded %d ends, want the two ends of the one changed file", decoded)
	}
}

func episodesOf(t *testing.T, db *store.DB, seasonID int64) []domain.Item {
	t.Helper()
	episodes, err := db.EpisodesWithParts(context.Background(), seasonID)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) == 0 {
		t.Fatal("the season has no episodes")
	}
	return episodes
}
