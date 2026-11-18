package scan

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/ffprobe"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// Result is what one scan did.
type Result struct {
	Files   int
	Skipped int
	Pruned  int
	Elapsed time.Duration
}

// Prober measures a file. Kept as an interface so a scan can run without
// ffprobe in tests and on a machine that has not installed it: a library with
// no durations is still browsable and playable, and the duration is filled in
// on the next scan once ffprobe is there.
type Prober interface {
	Probe(ctx context.Context, path string) (ffprobe.Info, error)
}

// Scanner walks a section's roots and writes what it finds.
//
// Memory shape, which is the reason this is not six lines of WalkDir:
//
//   - filepath.WalkDir streams directory entries rather than reading a whole
//     tree into a slice, so a 40,000-file folder costs one entry at a time.
//   - Probing is the slow part and runs on a bounded worker pool, sized to the
//     machine rather than to the library.
//   - Database writes happen on one goroutine through store.Writer, because
//     SQLite has exactly one writer and funnelling through a channel is both
//     faster and simpler than contending for it.
//   - Nothing accumulates per file except the set of paths seen, which is what
//     the prune step needs. That set is the scan's one unavoidable O(files)
//     structure, at roughly 60 bytes a path.
type Scanner struct {
	DB     *store.DB
	Prober Prober
	Log    *slog.Logger

	// Workers probing files. Zero means half the CPUs, minimum two: probing is
	// IO-bound enough that one per core wins nothing and costs memory.
	Workers int

	// Batch is how many items a write transaction covers. Zero means the
	// store's default.
	Batch int
}

type found struct {
	path  string
	size  int64
	mod   time.Time
	movie *Movie
	ep    *Episode
}

// Section scans one library and prunes anything that has vanished from disk.
func (s *Scanner) Section(ctx context.Context, section domain.Section) (Result, error) {
	started := time.Now()
	workers := s.Workers
	if workers <= 0 {
		workers = max(2, runtime.NumCPU()/2)
	}
	log := s.Log
	if log == nil {
		log = slog.Default()
	}

	// Bounded both ways: the walker blocks when the probers are behind, the
	// writer blocks when nothing has been probed. A library cannot outrun the
	// pipeline into memory.
	paths := make(chan found, workers*4)
	probed := make(chan store.Found, workers*4)

	var walkErr error
	go func() {
		defer close(paths)
		walkErr = s.walk(ctx, section, paths)
	}()

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range paths {
				out, ok := s.toFound(ctx, section, f)
				if !ok {
					continue
				}
				select {
				case probed <- out:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(probed)
	}()

	writer := s.DB.NewWriter(s.Batch)
	defer writer.Rollback()

	result := Result{}
	for f := range probed {
		if err := writer.Add(ctx, f); err != nil {
			return result, err
		}
		result.Files++
	}
	if err := writer.Commit(ctx); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if walkErr != nil {
		return result, walkErr
	}

	pruned, err := s.DB.PrunePaths(ctx, section.ID, writer.Seen())
	if err != nil {
		return result, err
	}
	result.Pruned = pruned

	if err := s.DB.MarkSectionScanned(ctx, section.ID, time.Now().UTC()); err != nil {
		return result, err
	}
	result.Elapsed = time.Since(started)
	log.Info("scanned section",
		"section", section.Title, "files", result.Files, "pruned", result.Pruned,
		"skipped", result.Skipped, "elapsed", result.Elapsed.Round(time.Millisecond))
	return result, nil
}

func (s *Scanner) walk(ctx context.Context, section domain.Section, out chan<- found) error {
	for _, root := range section.Roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				// A directory that cannot be read is logged and skipped, not
				// fatal: one unreadable folder must not abandon a library.
				if s.Log != nil {
					s.Log.Warn("skipping unreadable path", "path", path, "error", err)
				}
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if d.IsDir() {
				return nil
			}
			if !IsMedia(path) {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}

			f := found{path: path, size: info.Size(), mod: info.ModTime().UTC()}
			switch section.Kind {
			case domain.SectionMovies:
				m := ParseMovie(path)
				if m.Title == "" {
					return nil
				}
				f.movie = &m
			case domain.SectionShows:
				ep, ok := ParseEpisode(root, path)
				if !ok {
					return nil
				}
				f.ep = &ep
			default:
				return nil
			}

			select {
			case out <- f:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	return nil
}

func (s *Scanner) toFound(ctx context.Context, section domain.Section, f found) (store.Found, bool) {
	var (
		duration time.Duration
		chapters []domain.Chapter
	)
	if s.Prober != nil {
		if info, err := s.Prober.Probe(ctx, f.path); err == nil {
			duration, chapters = info.Duration, info.Chapters
		} else if s.Log != nil {
			s.Log.Debug("could not probe", "path", f.path, "error", err)
		}
	}

	out := store.Found{
		SectionID:  section.ID,
		Path:       f.path,
		Size:       f.size,
		Container:  containerOf(f.path),
		ModifiedAt: f.mod,
		Duration:   duration,
		Chapters:   chapters,
	}
	switch {
	case f.movie != nil:
		out.Kind = domain.KindMovie
		out.Title, out.Year = f.movie.Title, f.movie.Year
	case f.ep != nil:
		out.Kind = domain.KindEpisode
		out.Show, out.ShowYear = f.ep.Show, f.ep.ShowYear
		out.SeasonNum, out.EpisodeNum = f.ep.Season, f.ep.Number
		out.Title = f.ep.Title
	default:
		return store.Found{}, false
	}
	return out, true
}

func containerOf(path string) string {
	ext := filepath.Ext(path)
	if len(ext) > 1 {
		return ext[1:]
	}
	return ""
}
