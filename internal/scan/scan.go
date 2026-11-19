package scan

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/inknironapps/bask-media-server/internal/artwork"
	"github.com/inknironapps/bask-media-server/internal/coverart"
	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/ffprobe"
	"github.com/inknironapps/bask-media-server/internal/photos"
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

	// Artwork is where a cover found in the library is kept: a folder image
	// beside an album, or the picture inside the music files themselves.
	// Optional — without it a music library scans and plays, with no covers.
	//
	// Photographs never go in here. They are served from where they already
	// are, and only their thumbnails are cached: see artwork.Cache.OpenFrom.
	Artwork *artwork.Cache

	// Workers probing files. Zero means half the CPUs, minimum two: probing is
	// IO-bound enough that one per core wins nothing and costs memory.
	Workers int

	// Batch is how many items a write transaction covers. Zero means the
	// store's default.
	Batch int

	// mu guards covers, which is the per-scan memory of artwork already
	// looked at. On the Scanner rather than passed through the pipeline
	// because the probing workers share it.
	mu     sync.Mutex
	covers map[string]string

	// Progress is told how many files this section has written so far.
	//
	// Called from the single writer goroutine, so an implementation must not
	// block: the admin interface stores a counter and reads it from the HTTP
	// side. Optional, because a scan started from the command line reports at
	// the end and has nothing to tell anybody meanwhile.
	Progress func(files int)
}

type found struct {
	path string
	size int64
	mod  time.Time

	// kind is what this file is, decided by the library it was found in.
	kind domain.Kind

	// root is the library root this file was found under, which is what the
	// parsers measure a path against.
	root string

	movie *Movie
	ep    *Episode

	// siblings are the subtitle files in the same folder, which is where
	// every tool in this space puts them. Collected per directory while
	// walking so that matching one to a film is a string comparison rather
	// than a second pass over the tree.
	siblings []string

	// cover and artistCover are folder images found beside a music file and
	// one level above it. Paths, not pictures: whether they are worth keeping
	// is decided once per album rather than once per track.
	cover       string
	artistCover string
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
	// The same for folder images, which is how an album's cover is found.
	covers := map[string]string{}

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
				if path != root && skipDir(d.Name()) {
					return fs.SkipDir
				}
				// A folder's sidecars and its cover are read once, when the
				// folder is entered, and used by every file in it: one extra
				// listing per directory against one per media file. Only
				// where they mean something — a photo library has no
				// subtitles, and in one of those cover.jpg is a photograph.
				switch section.Kind {
				case domain.SectionMovies, domain.SectionShows:
					subtitles[path] = subtitlesIn(path)
				case domain.SectionMusic:
					covers[path] = coverIn(path)
				}
				return nil
			}
			if !s.wanted(section.Kind, path) {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}

			f := found{
				path: path, size: info.Size(), mod: info.ModTime().UTC(), root: root,
			}
			switch section.Kind {
			case domain.SectionMovies:
				m := ParseMovie(path)
				if m.Title == "" {
					return nil
				}
				f.kind, f.movie = domain.KindMovie, &m
				f.siblings = subtitles[filepath.Dir(path)]
			case domain.SectionShows:
				ep, ok := ParseEpisode(root, path)
				if !ok {
					return nil
				}
				f.kind, f.ep = domain.KindEpisode, &ep
				f.siblings = subtitles[filepath.Dir(path)]
			case domain.SectionMusic:
				// The album's folder is the one above the file unless the file
				// is in a per-disc folder, which is also where the cover is.
				f.kind = domain.KindTrack
				album := albumDirOf(filepath.Dir(path))
				f.cover = covers[album]
				f.artistCover = covers[filepath.Dir(album)]
			case domain.SectionPhotos:
				f.kind = domain.KindPhoto
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

// wanted reports whether a file belongs in this kind of library.
func (s *Scanner) wanted(kind domain.SectionKind, path string) bool {
	switch kind {
	case domain.SectionMovies, domain.SectionShows:
		return IsMedia(path)
	case domain.SectionMusic:
		return IsAudio(path)
	case domain.SectionPhotos:
		return IsImage(path)
	default:
		return false
	}
}

// skipDir names the directories that are never part of a library: the hidden
// ones, and the thumbnail caches that network drives and photo tools scatter
// through a tree. Without this a photo library gains a second copy of itself
// at a quarter of the size.
func skipDir(name string) bool {
	switch strings.ToLower(name) {
	case "@eadir", "#recycle", "$recycle.bin", "lost+found",
		".thumbnails", ".picasaoriginals", "@__thumb":
		return true
	}
	return strings.HasPrefix(name, ".")
}

// albumDirOf is the folder that names an album: the file's own, unless that
// is a per-disc folder, in which case it is the one above.
func albumDirOf(dir string) string {
	if discDir.MatchString(filepath.Base(dir)) {
		return filepath.Dir(dir)
	}
	return dir
}

func (s *Scanner) toFound(ctx context.Context, section domain.Section, f found) (store.Found, bool) {
	switch f.kind {
	case domain.KindTrack:
		return s.trackFound(ctx, section, f)
	case domain.KindPhoto:
		return s.photoFound(section, f)
	}

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

// trackFound turns one music file into a row.
//
// The file's own tags come first and the path second, which is the opposite of
// the rule for films: a music file carries metadata written by whoever
// published it, and that beats any reading of "01 - track.mp3". The path is
// what fills the gaps, and a file with neither is still filed rather than
// dropped.
func (s *Scanner) trackFound(ctx context.Context, section domain.Section, f found) (store.Found, bool) {
	var (
		duration time.Duration
		tags     Tags
	)
	if s.Prober != nil {
		if info, err := s.Prober.Probe(ctx, f.path); err == nil {
			duration = info.Duration
			tags = Tags{
				Title:       info.Tags.Title,
				Artist:      info.Tags.Artist,
				AlbumArtist: info.Tags.AlbumArtist,
				Album:       info.Tags.Album,
				Date:        info.Tags.Date,
				Genre:       info.Tags.Genre,
				Track:       info.Tags.Track,
				Disc:        info.Tags.Disc,
			}
		} else if s.Log != nil {
			s.Log.Debug("could not probe", "path", f.path, "error", err)
		}
	}

	track := ParseTrack(f.root, f.path, tags)
	return store.Found{
		SectionID:    section.ID,
		Kind:         domain.KindTrack,
		Path:         f.path,
		Size:         f.size,
		Container:    containerOf(f.path),
		ModifiedAt:   f.mod,
		Duration:     duration,
		Title:        track.Title,
		Year:         track.Year,
		Artist:       track.Artist,
		Performer:    track.Performer,
		Album:        track.Album,
		Disc:         track.Disc,
		TrackNum:     track.Number,
		AlbumPoster:  s.cover(f.cover, f.path),
		ArtistPoster: s.adopt(f.artistCover),
	}, true
}

// photoFound turns one picture into a row.
//
// The file is read, never decoded: the dimensions come out of the format's
// header and the time and rotation out of its EXIF. A format this program
// cannot read — a phone's HEIC — is still a row, with no dimensions and the
// file's own mtime for when it was taken.
func (s *Scanner) photoFound(section domain.Section, f found) (store.Found, bool) {
	photo, ok := ParsePhoto(f.root, f.path)
	if !ok {
		return store.Found{}, false
	}

	taken := f.mod
	info, err := photos.Read(f.path)
	switch {
	case err == nil, errors.Is(err, photos.ErrNoPixels):
		if !info.TakenAt.IsZero() {
			taken = info.TakenAt
		}
	default:
		if s.Log != nil {
			s.Log.Debug("could not read a picture", "path", f.path, "error", err)
		}
	}

	return store.Found{
		SectionID:   section.ID,
		Kind:        domain.KindPhoto,
		Path:        f.path,
		Size:        f.size,
		Container:   containerOf(f.path),
		ModifiedAt:  f.mod,
		Albums:      photo.Albums,
		Title:       photo.Title,
		TakenAt:     taken,
		Width:       info.Width,
		Height:      info.Height,
		Orientation: info.Orientation,
	}, true
}

// cover is an album's artwork: the folder image beside it where there is one,
// and otherwise the picture inside the music file itself.
//
// Resolved once per album folder and remembered, because every track in a
// folder would otherwise ask the same question — and in the embedded case
// that question means reading the front of a file and hashing a few hundred
// kilobytes.
func (s *Scanner) cover(folderImage, trackPath string) string {
	if s.Artwork == nil {
		return ""
	}
	if key := s.adopt(folderImage); key != "" {
		return key
	}
	return s.embedded(trackPath)
}

// adopt copies a folder image into the artwork cache and returns its key.
func (s *Scanner) adopt(path string) string {
	if s.Artwork == nil || path == "" {
		return ""
	}
	if key, found := s.remembered(path); found {
		return key
	}
	key, err := s.Artwork.Adopt(path)
	if err != nil {
		if s.Log != nil {
			s.Log.Debug("could not keep a folder image", "path", path, "error", err)
		}
		key = ""
	}
	s.remember(path, key)
	return key
}

// embedded pulls the cover out of a music file, once per folder.
func (s *Scanner) embedded(trackPath string) string {
	folder := albumDirOf(filepath.Dir(trackPath))
	if key, found := s.remembered("embedded:" + folder); found {
		return key
	}

	var key string
	picture, err := coverart.Read(trackPath)
	switch {
	case err == nil:
		if stored, err := s.Artwork.Store(picture.Data); err == nil {
			key = stored
			if s.Log != nil {
				s.Log.Debug("kept a cover from inside a file",
					"path", trackPath, "picture", picture.Describe())
			}
		} else if s.Log != nil {
			s.Log.Debug("could not keep an embedded cover", "path", trackPath, "error", err)
		}
	case errors.Is(err, coverart.ErrNoPicture):
		// The common case for a library with folder images, and not a fault.
	default:
		if s.Log != nil {
			s.Log.Debug("could not read an embedded cover", "path", trackPath, "error", err)
		}
	}
	s.remember("embedded:"+folder, key)
	return key
}

// remembered and remember are the per-scan memory of artwork already dealt
// with, keyed by what was looked at. Guarded because the probing workers run
// in parallel, and bounded by the number of album folders in the library —
// the same order as the shows in the writer's parent cache.
func (s *Scanner) remembered(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, found := s.covers[key]
	return value, found
}

func (s *Scanner) remember(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.covers == nil {
		s.covers = map[string]string{}
	}
	s.covers[key] = value
}

// coverIn finds the folder image in one directory: the cover of the album
// whose tracks are in it, or the picture of the artist whose albums are.
func coverIn(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var fallback string
	for _, entry := range entries {
		if entry.IsDir() || !IsImage(entry.Name()) {
			continue
		}
		if IsArtwork(entry.Name()) {
			return filepath.Join(dir, entry.Name())
		}
		if fallback == "" {
			fallback = filepath.Join(dir, entry.Name())
		}
	}
	// A folder with one picture in it and a name nobody standardised is still
	// that album's cover: it is the only image in a folder of music.
	return fallback
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
