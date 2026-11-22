package scan

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// Trailers, featurettes, deleted scenes: the files that belong to something
// rather than being it.
//
// This is a correction as much as a feature. Until now a trailer in
// "Big Hill (2026)/Trailers/teaser.mkv" was scanned as a film in its own
// right, because the parser reads a title out of whatever path it is handed —
// so a library with extras in it grew a row called "Teaser" with no year, no
// poster and no way to tell it apart from a real film. Recognising extras
// takes those rows out of the library and puts them where a client can offer
// them beside the thing they belong to.
//
// Two layouts, because the ecosystem has two and both are in the wild:
//
//   - A folder: "Big Hill (2026)/Trailers/teaser.mkv". The folder names the
//     sort, and the thing above it is what the extra belongs to.
//   - A suffix: "Big Hill (2026)-trailer.mkv", which is what Plex's own
//     naming guide asks for. The suffix names the sort, and the rest of the
//     filename is the thing it belongs to — which means the parent is read by
//     handing the stripped name back to the ordinary parser.

// Extra is a file that belongs to something else.
type Extra struct {
	// Kind is which sort of extra it is.
	Kind domain.ExtraKind

	// Title is what to call it: the filename, cleaned, or the sort's own name
	// where the filename was only the suffix.
	Title string

	// Parent is the path the parent item is read from. For the folder layout
	// that is the folder above the extras folder; for the suffix layout it is
	// the file's own path with the suffix taken off. Either way it is handed
	// to the ordinary parsers, so an extra is placed by exactly the same
	// reading of a name as the thing it belongs to.
	Parent string
}

// extraDirs maps a folder name to the sort of extra inside it.
//
// The spellings are the ones Plex and Jellyfin document plus the ones people
// actually type: the comparison strips spaces, hyphens and underscores, so
// "Behind The Scenes", "behind-the-scenes" and "behindthescenes" are one
// entry rather than three.
var extraDirs = map[string]domain.ExtraKind{
	"trailer":          domain.ExtraTrailer,
	"trailers":         domain.ExtraTrailer,
	"behindthescenes":  domain.ExtraBehindTheScenes,
	"deletedscenes":    domain.ExtraDeleted,
	"deleted":          domain.ExtraDeleted,
	"featurettes":      domain.ExtraFeaturette,
	"featurette":       domain.ExtraFeaturette,
	"interviews":       domain.ExtraInterview,
	"interview":        domain.ExtraInterview,
	"scenes":           domain.ExtraScene,
	"shorts":           domain.ExtraShort,
	"short":            domain.ExtraShort,
	"extras":           domain.ExtraOther,
	"other":            domain.ExtraOther,
	"specialfeatures":  domain.ExtraOther,
	"bonus":            domain.ExtraOther,
	"bonusdisc":        domain.ExtraOther,
	"bonuscontent":     domain.ExtraOther,
	"behindthescene":   domain.ExtraBehindTheScenes,
	"makingof":         domain.ExtraBehindTheScenes,
	"deletedscene":     domain.ExtraDeleted,
	"interviewsextras": domain.ExtraInterview,
}

// extraSuffix is the filename form: anything ending in "-trailer",
// "-behindthescenes", "-deleted" and so on.
//
// Anchored at the end and requiring something before the hyphen, so a film
// actually called "Trailer" is not mistaken for one: a bare "trailer.mkv" is
// handled by the folder rule or not at all.
var extraSuffix = regexp.MustCompile(`(?i)^(.*[^ ._-])[ ._-](trailer|behindthescenes|deleted(?:scene)?|featurette|interview|scene|short|other)(?:[ ._-]?\d{1,2})?$`)

// ParseExtra reads whether this file is an extra, and of what.
//
// Folder first, because a file inside "Trailers" is a trailer whatever it is
// called, and because the suffix rule would otherwise claim a file named
// "the-interview.mkv" that happens to be a film.
func ParseExtra(root, path string) (Extra, bool) {
	if found, ok := extraFromFolder(root, path); ok {
		return found, true
	}
	return extraFromSuffix(path)
}

// extraFromFolder walks up from the file looking for a folder that names a
// sort of extra, stopping at the library root: a library whose own folder is
// called "Extras" holds films, not extras.
func extraFromFolder(root, path string) (Extra, bool) {
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	dir := filepath.Dir(path)
	for dir != root && dir != filepath.Dir(dir) {
		name := filepath.Base(dir)
		if kind, found := extraDirs[normaliseFolder(name)]; found {
			parent := filepath.Dir(dir)
			if parent == root || parent == "" {
				// An extras folder at the top of a library has nothing above
				// it to belong to, so there is no sensible parent and this is
				// not an extra. Scanned as whatever the name reads as, which
				// is what happened before any of this existed.
				return Extra{}, false
			}
			title := cleanTitle(withoutEdition(stem))
			if title == "" {
				title = kind.Label()
			}
			return Extra{Kind: kind, Title: title, Parent: parent}, true
		}
		dir = filepath.Dir(dir)
	}
	return Extra{}, false
}

// extraFromSuffix reads the "-trailer" form.
func extraFromSuffix(path string) (Extra, bool) {
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(filepath.Base(path), ext)
	match := extraSuffix.FindStringSubmatch(stem)
	if match == nil {
		return Extra{}, false
	}
	kind, found := extraDirs[normaliseFolder(match[2])]
	if !found {
		return Extra{}, false
	}
	// The parent is the same path with the suffix removed, so the ordinary
	// parser reads it exactly as it would read the film's own file — which
	// matters because that parser prefers the folder name, and a film in its
	// own folder is the common layout.
	parent := filepath.Join(filepath.Dir(path), match[1]+ext)
	return Extra{Kind: kind, Title: kind.Label(), Parent: parent}, true
}

// normaliseFolder is a folder or suffix name with the separators people
// disagree about taken out.
func normaliseFolder(name string) string {
	var out strings.Builder
	for _, r := range strings.ToLower(name) {
		switch r {
		case ' ', '-', '_', '.':
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}

// ShowOf is the series a path is under: the first folder below the library
// root.
//
// Used to place an extra that belongs to a series rather than to one episode.
// The first folder rather than the nearest, because the nearest is the extras
// folder itself and the one above it may be a season — and a trailer for a
// series belongs to the series however deep it was filed.
func ShowOf(root, path string) (title string, year int, ok bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", 0, false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 2 {
		// A file directly in the library root belongs to no series.
		return "", 0, false
	}
	name := parts[0]
	if m, found := movieFrom(name); found {
		return m.Title, m.Year, m.Title != ""
	}
	cleaned := cleanTitle(name)
	return cleaned, 0, cleaned != ""
}
