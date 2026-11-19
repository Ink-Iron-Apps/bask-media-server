package scan

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
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

	// Progress is told how many files this section has written so far.
	//
	// Called from the single writer goroutine, so an implementation must not
	// block: the admin interface stores a counter and reads it from the HTTP
	// side. Optional, because a scan started from the command line reports at
	// the end and has nothing to tell anybody meanwhile.
	Progress func(files int)
}

type found struct {
	path  string
	size  int64
	mod   time.Time
	movie *Movie
	ep    *Episode

	// siblings are the subtitle files in the same folder, which is where
	// every tool in this space puts them. Collected per directory while
	// walking so that matching one to a film is a string comparison rather
	// than a second pass over the tree.
	siblings []string
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
		if s.Progress != nil {
			s.Progress(result.Files)
		}
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
	// Keyed by directory and held for the length of the walk. Bounded by the
	// number of subtitle files in the library, which is the same order as the
	// set of seen paths the prune already needs.
	subtitles := map[string][]string{}

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
				// A folder's subtitle files are read once, when the folder is
				// entered, and used by every media file in it. One extra
				// listing per directory, against one per media file.
				subtitles[path] = subtitlesIn(path)
				return nil
			}
			if !IsMedia(path) {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}

			f := found{
				path: path, size: info.Size(), mod: info.ModTime().UTC(),
				siblings: subtitles[filepath.Dir(path)],
			}
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
		duration  time.Duration
		chapters  []domain.Chapter
		subtitles []domain.Subtitle
		probed    bool
	)
	if s.Prober != nil {
		if info, err := s.Prober.Probe(ctx, f.path); err == nil {
			duration, chapters, probed = info.Duration, info.Chapters, true
			subtitles = embeddedSubtitles(info)
		} else if s.Log != nil {
			s.Log.Debug("could not probe", "path", f.path, "error", err)
		}
	}
	// Sidecars are found by looking, not by probing, so they are collected
	// whether or not there is an ffprobe on this machine.
	sidecars := sidecarSubtitles(f.path, f.siblings)
	for i := range subtitles {
		// An embedded subtitle's text is in the media file, which is what
		// reading it later needs to open.
		subtitles[i].Path = f.path
	}
	if len(sidecars) > 0 || probed {
		// nil means "nothing was looked for" and must not delete what is
		// already known; an empty slice means "looked, found none".
		if subtitles == nil {
			subtitles = []domain.Subtitle{}
		}
		subtitles = append(subtitles, sidecars...)
	}

	out := store.Found{
		SectionID:  section.ID,
		Path:       f.path,
		Size:       f.size,
		Container:  containerOf(f.path),
		ModifiedAt: f.mod,
		Duration:   duration,
		Chapters:   chapters,
		Subtitles:  subtitles,
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

// subtitlesIn lists the subtitle files in one directory.
func subtitlesIn(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		if entry.IsDir() || !IsSubtitle(entry.Name()) {
			continue
		}
		out = append(out, filepath.Join(dir, entry.Name()))
	}
	return out
}

// sidecarSubtitles picks the files in a folder that belong to this one.
func sidecarSubtitles(mediaPath string, siblings []string) []domain.Subtitle {
	var out []domain.Subtitle
	for _, path := range siblings {
		sidecar, ok := ParseSidecar(mediaPath, path)
		if !ok {
			continue
		}
		out = append(out, domain.Subtitle{
			External:    true,
			StreamIndex: -1,
			Path:        sidecar.Path,
			Codec:       sidecar.Codec,
			Language:    LanguageCode(sidecar.Language),
			Title:       sidecar.Title,
			Forced:      sidecar.Forced,
			Hearing:     sidecar.Hearing,
		})
	}
	return out
}

// embeddedSubtitles turns the container's own subtitle streams into rows.
func embeddedSubtitles(info ffprobe.Info) []domain.Subtitle {
	streams := info.Subtitles()
	out := make([]domain.Subtitle, 0, len(streams))
	for _, stream := range streams {
		out = append(out, domain.Subtitle{
			StreamIndex: stream.Index,
			Codec:       stream.Codec,
			Language:    LanguageCode(stream.Language),
			Title:       stream.Title,
			Forced:      stream.Forced,
			Default:     stream.Default,
			Hearing:     stream.Hearing,
		})
	}
	return out
}

func containerOf(path string) string {
	ext := filepath.Ext(path)
	if len(ext) > 1 {
		return ext[1:]
	}
	return ""
}
