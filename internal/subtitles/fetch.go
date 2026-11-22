package subtitles

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/opensubtitles"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// Finding a subtitle for a film that has none.
//
// The shape of this follows one rule: the scan stays the authority on what is
// on disk. So a fetched subtitle is written as a sidecar file beside the
// media, named the way the scanner reads sidecars, and only then recorded —
// which means the next scan finds the same file and leaves the row alone,
// rather than deleting a row whose file it cannot see.
//
// Off until somebody saves a key. Nothing about anybody's library reaches a
// third party before that, and when it does it is a title and a hash.

// ErrOff means no key is configured, so this server does not search.
var ErrOff = errors.New("subtitle searching is off: save an OpenSubtitles key in the admin interface")

// ErrNotWritable means the folder holding the media cannot be written to, so
// there is nowhere to put a sidecar the next scan will also find.
var ErrNotWritable = errors.New("the folder holding this file cannot be written to")

// Fetcher searches for and downloads subtitles.
type Fetcher struct {
	DB *store.DB

	// Key resolves the service's key when it is needed rather than holding
	// it, because it can be saved from the admin interface on a server that
	// is already running.
	Key func(ctx context.Context) (string, error)

	Log *slog.Logger
}

// Candidate is one subtitle on offer, with everything a person needs to
// choose between them.
type Candidate struct {
	// Handle is what a client sends back to ask for this one. Opaque on
	// purpose: it names a provider and that provider's own id.
	Handle string

	Language string
	Release  string
	Name     string

	// Exact means the subtitle was authored against this very file, which is
	// the only thing here that guarantees it stays in time.
	Exact bool

	HearingImpaired bool
	Forced          bool

	Downloads int
	Rating    float64
}

// Want narrows a search.
type Want struct {
	// Language is the two-letter code, defaulting to English.
	Language string

	// HearingImpaired and Forced ask for those kinds specifically.
	HearingImpaired bool
	Forced          bool
}

// Search offers what the service has for this file.
func (f *Fetcher) Search(ctx context.Context, item domain.Item, part domain.Part, want Want) ([]Candidate, error) {
	client, err := f.client(ctx)
	if err != nil {
		return nil, err
	}

	query := opensubtitles.Query{
		Title:           item.Title,
		Year:            item.Year,
		HearingImpaired: want.HearingImpaired,
		Forced:          want.Forced,
	}
	if language := strings.ToLower(strings.TrimSpace(want.Language)); language != "" {
		query.Languages = []string{language}
	}
	// An episode is searched for by the series and its numbering, which is
	// how the service files them: its own title is "Part 1" and matches
	// everything.
	if item.Kind == domain.KindEpisode {
		series, season := f.lineage(ctx, item)
		if series != "" {
			query.Title, query.Year = series, 0
		}
		query.Season, query.Episode = season, item.Index
	}
	// The hash is what makes an exact match possible, and it costs 128 KiB
	// of reading whatever the file's size. A file that cannot be hashed — a
	// missing file, a tiny one — is still searched for by title.
	if hash, size, err := opensubtitles.Hash(part.Path); err == nil {
		query.Hash, query.Size = hash, size
	} else {
		f.logger().Debug("could not hash a file for a subtitle search", "path", part.Path, "error", err)
	}

	found, err := client.Search(ctx, query)
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(found))
	for _, candidate := range found {
		out = append(out, Candidate{
			Handle:          handleFor(candidate.FileID),
			Language:        candidate.Language,
			Release:         candidate.Release,
			Name:            candidate.Name,
			Exact:           candidate.Exact,
			HearingImpaired: candidate.HearingImpaired,
			Forced:          candidate.Forced,
			Downloads:       candidate.Downloads,
			Rating:          candidate.Rating,
		})
	}
	return out, nil
}

// Fetch downloads one and records it.
//
// The language, and whether it is forced or for the hard of hearing, are
// taken from what the client asked for rather than guessed from the file's
// name: the client chose a row that said so, and the name of a subtitle file
// on the service is whatever its author typed.
func (f *Fetcher) Fetch(ctx context.Context, item domain.Item, part domain.Part, handle string, want Want) (*domain.Subtitle, error) {
	client, err := f.client(ctx)
	if err != nil {
		return nil, err
	}
	fileID, err := fileIDOf(handle)
	if err != nil {
		return nil, err
	}

	// Checked before the download, because a download is spent whether or not
	// anything can be done with what comes back: the service counts it
	// against a daily allowance.
	folder := filepath.Dir(part.Path)
	if err := writable(folder); err != nil {
		return nil, err
	}

	text, name, err := client.Download(ctx, fileID)
	if err != nil {
		return nil, err
	}
	if len(text) == 0 {
		return nil, errors.New("the service sent an empty subtitle")
	}

	path, codec := sidecarPath(part.Path, name, want)
	if err := os.WriteFile(path, text, 0o644); err != nil {
		return nil, fmt.Errorf("write the subtitle beside the film: %w", err)
	}

	language := strings.ToLower(strings.TrimSpace(want.Language))
	if language == "" {
		language = "en"
	}
	saved, err := f.DB.AddSubtitle(ctx, item.ID, part.ID, domain.Subtitle{
		External:    true,
		StreamIndex: -1,
		Path:        path,
		Codec:       codec,
		Language:    language,
		Forced:      want.Forced,
		Hearing:     want.HearingImpaired,
	})
	if err != nil {
		// The file is written and the next scan will find it, so this is not
		// worth deleting the download over.
		return nil, err
	}
	f.logger().Info("fetched a subtitle",
		"item", item.ID, "language", language, "path", filepath.Base(path))
	return saved, nil
}

// sidecarPath is where a fetched subtitle goes and what it is.
//
// Named the way the scanner reads a sidecar — the media's own name, then the
// language and what kind it is — so the next scan finds this file, matches it
// to this film, and records the same thing rather than a second row.
func sidecarPath(mediaPath, name string, want Want) (path, codec string) {
	extension := strings.ToLower(filepath.Ext(name))
	switch extension {
	case ".srt", ".ass", ".ssa", ".vtt", ".sub":
	default:
		// Whatever the service called it, SubRip is what it almost always
		// is, and a name this server does not recognise would be a sidecar
		// the scanner ignores.
		extension = ".srt"
	}
	codec = strings.TrimPrefix(extension, ".")
	if codec == "srt" {
		codec = "subrip"
	}

	language := strings.ToLower(strings.TrimSpace(want.Language))
	if language == "" {
		language = "en"
	}
	stem := strings.TrimSuffix(mediaPath, filepath.Ext(mediaPath))
	suffix := "." + language
	if want.Forced {
		suffix += ".forced"
	}
	if want.HearingImpaired {
		suffix += ".sdh"
	}

	path = stem + suffix + extension
	// A second subtitle of the same language and kind gets a number, rather
	// than overwriting the one somebody is already using.
	for n := 2; n < 20; n++ {
		if _, err := os.Stat(path); err != nil {
			break
		}
		path = stem + suffix + "." + strconv.Itoa(n) + extension
	}
	return path, codec
}

// writable reports whether a sidecar can be written into this folder.
//
// By trying, because the permission bits on a network share are a story and
// the only question that matters is whether a file can be created.
func writable(folder string) error {
	probe, err := os.CreateTemp(folder, ".bms-subtitle-*")
	if err != nil {
		return fmt.Errorf("%w: %s", ErrNotWritable, folder)
	}
	name := probe.Name()
	probe.Close()
	_ = os.Remove(name)
	return nil
}

// lineage is the series and season above an episode.
func (f *Fetcher) lineage(ctx context.Context, item domain.Item) (series string, season int) {
	if item.ParentID == nil {
		return "", 0
	}
	parent, err := f.DB.Item(ctx, *item.ParentID)
	if err != nil {
		return "", 0
	}
	season = parent.Index
	if parent.ParentID == nil {
		return parent.Title, season
	}
	if show, err := f.DB.Item(ctx, *parent.ParentID); err == nil {
		return show.Title, season
	}
	return parent.Title, season
}

// client is the service, with the key resolved now.
func (f *Fetcher) client(ctx context.Context) (*opensubtitles.Client, error) {
	if f == nil || f.DB == nil || f.Key == nil {
		return nil, ErrOff
	}
	key, err := f.Key(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(key) == "" {
		return nil, ErrOff
	}
	return &opensubtitles.Client{Key: key}, nil
}

// On reports whether this server can search at all, which is what an
// interface asks before offering a button.
func (f *Fetcher) On(ctx context.Context) bool {
	_, err := f.client(ctx)
	return err == nil
}

func (f *Fetcher) logger() *slog.Logger {
	if f != nil && f.Log != nil {
		return f.Log
	}
	return slog.Default()
}

// handleFor and fileIDOf are the opaque id a client passes back.
//
// Prefixed with the provider, because a second provider would otherwise be
// indistinguishable from this one and the handle is a thing clients store.
func handleFor(fileID int64) string { return "opensubtitles:" + strconv.FormatInt(fileID, 10) }

func fileIDOf(handle string) (int64, error) {
	provider, id, found := strings.Cut(handle, ":")
	if !found || provider != "opensubtitles" {
		return 0, fmt.Errorf("that is not a subtitle this server offered: %q", handle)
	}
	fileID, err := strconv.ParseInt(id, 10, 64)
	if err != nil || fileID <= 0 {
		return 0, fmt.Errorf("that is not a subtitle this server offered: %q", handle)
	}
	return fileID, nil
}
