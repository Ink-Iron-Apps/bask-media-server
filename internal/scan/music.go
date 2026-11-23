package scan

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// AudioExtensions are the audio containers a scan will pick up.
//
// The lossless and the lossy together, because a household's music is both,
// and including a format this server cannot convert is still right: whether a
// file can be played is a question about the client, and most of these play
// untouched in a browser.
var AudioExtensions = map[string]bool{
	".mp3": true, ".flac": true, ".m4a": true, ".m4b": true, ".aac": true,
	".ogg": true, ".oga": true, ".opus": true, ".wav": true, ".wma": true,
	".aiff": true, ".aif": true, ".alac": true, ".ape": true, ".wv": true,
	".mka": true, ".dsf": true, ".mpc": true,
}

// IsAudio reports whether a path looks like something to listen to.
func IsAudio(path string) bool {
	return AudioExtensions[strings.ToLower(filepath.Ext(path))]
}

// Tags is what a file says about itself, as the container's own metadata.
//
// Preferred over the filename wherever it is present, which is the opposite
// of the rule for films: a film's name is written in its path by the tool
// that fetched it, while a music file carries tags written by whoever
// published it, and those are better than any parse of "01 - track.mp3".
type Tags struct {
	Title       string
	Artist      string
	AlbumArtist string
	Album       string
	Date        string
	Genre       string
	Track       int
	Disc        int
}

// Track is what BMS files one music file as.
type Track struct {
	// Artist is the album's artist — the level a client browses — which is
	// album_artist where the file has one, because that is what keeps a
	// compilation together instead of scattering it across twenty artists.
	Artist string

	// Performer is who plays on this track, kept only when it differs from
	// the album's artist: on a compilation it is the useful half of the row.
	Performer string

	Album  string
	Year   int
	Disc   int
	Number int
	Title  string

	// Genres are what the file's own tags say it is. A tag routinely holds
	// several, separated by a semicolon, a slash or a comma, and some hold
	// one name that contains a slash — "Rock/Pop" is two genres, "R&B" is
	// one — so the split is deliberate and documented at [splitGenres].
	Genres []string
}

var (
	// "01 - Title", "01. Title", "01 Title", "1_Title".
	leadingNumber = regexp.MustCompile(`^(\d{1,3})\s*[-._) ]\s*(.+)$`)

	// "CD1", "Disc 2", "disk-3" as a directory name, which is how a multi-disc
	// album is routinely laid out.
	discDir = regexp.MustCompile(`(?i)^(?:cd|disc|disk)[ ._-]*(\d{1,2})$`)

	// A leading disc-and-track number, "1-01 Title", which some rippers write
	// instead of a folder per disc.
	discTrack = regexp.MustCompile(`^(\d{1,2})[-_](\d{1,3})\s*[-._) ]*\s*(.*)$`)
)

// ParseTrack reads artist, album, disc, number and title for one file.
//
// Tags first, path second, and a name for what is still missing: a file with
// no tags in a flat folder is filed under an obvious placeholder rather than
// dropped, because a track nobody can find is worse than a track filed under
// "Unknown Artist".
func ParseTrack(root, path string, tags Tags) Track {
	dir := filepath.Dir(path)
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))

	out := Track{
		Artist:    strings.TrimSpace(tags.AlbumArtist),
		Performer: strings.TrimSpace(tags.Artist),
		Album:     strings.TrimSpace(tags.Album),
		Disc:      tags.Disc,
		Number:    tags.Track,
		Title:     strings.TrimSpace(tags.Title),
		Year:      yearOf(tags.Date),
		Genres:    splitGenres(tags.Genre),
	}
	if out.Artist == "" {
		out.Artist = out.Performer
	}

	// The album's folder, which is the one above the file unless the file is
	// in a per-disc folder.
	albumDir := dir
	if m := discDir.FindStringSubmatch(filepath.Base(dir)); m != nil {
		if out.Disc == 0 {
			out.Disc, _ = strconv.Atoi(m[1])
		}
		albumDir = filepath.Dir(dir)
	}

	if out.Album == "" {
		if name := albumName(root, albumDir); name != "" {
			out.Album, out.Year = name, yearFromDir(albumDir, out.Year)
		}
	} else if out.Year == 0 {
		out.Year = yearFromDir(albumDir, 0)
	}
	if out.Artist == "" {
		// <Artist>/<Album>/track, the layout every tagger and ripper writes.
		if parent := filepath.Dir(albumDir); withinRoot(root, parent) {
			out.Artist = cleanTitle(filepath.Base(parent))
		}
	}

	if out.Title == "" || out.Number == 0 {
		number, title := numberAndTitle(base)
		if out.Number == 0 {
			out.Number = number
		}
		if out.Title == "" {
			out.Title = title
		}
	}
	// A leading "1-01" names the disc as well as the track, and only where
	// nothing better already did.
	if m := discTrack.FindStringSubmatch(base); m != nil && tags.Disc == 0 && out.Disc == 0 {
		disc, _ := strconv.Atoi(m[1])
		if disc > 0 && disc < 50 {
			out.Disc = disc
		}
	}

	if out.Artist == "" {
		out.Artist = UnknownArtist
	}
	if out.Album == "" {
		out.Album = UnknownAlbum
	}
	if out.Title == "" {
		out.Title = cleanTitle(base)
	}
	if strings.EqualFold(out.Performer, out.Artist) {
		out.Performer = ""
	}
	return out
}

// UnknownArtist and UnknownAlbum are where a file with nothing to go on is
// filed. Spelled as words rather than left empty so the row reads as an
// answer in a client's list instead of a gap.
const (
	UnknownArtist = "Unknown Artist"
	UnknownAlbum  = "Unknown Album"
)

// numberAndTitle splits a filename into its track number and its title.
func numberAndTitle(base string) (int, string) {
	if m := discTrack.FindStringSubmatch(base); m != nil {
		number, _ := strconv.Atoi(m[2])
		return number, cleanTitle(m[3])
	}
	if m := leadingNumber.FindStringSubmatch(base); m != nil {
		number, _ := strconv.Atoi(m[1])
		return number, cleanTitle(m[2])
	}
	return 0, cleanTitle(base)
}

// albumName is the album's folder name, cleaned, unless that folder is the
// section root itself — a loose file at the top of a library belongs to no
// album the folder tree knows about.
func albumName(root, albumDir string) string {
	if !withinRoot(root, albumDir) {
		return ""
	}
	name := filepath.Base(albumDir)
	if m, ok := movieFrom(name); ok {
		// "Album (2001)", the same shape a film folder has.
		return m.Title
	}
	return cleanTitle(name)
}

// yearFromDir reads a year out of an album folder's name.
func yearFromDir(albumDir string, fallback int) int {
	if m, ok := movieFrom(filepath.Base(albumDir)); ok {
		return m.Year
	}
	return fallback
}

// yearOf takes the year out of a tag that may be a year, a date, or a
// timestamp: "2001", "2001-05-14", "2001-05-14T00:00:00Z".
func yearOf(date string) int {
	date = strings.TrimSpace(date)
	if len(date) < 4 {
		return 0
	}
	year, err := strconv.Atoi(date[:4])
	if err != nil || year < 1000 || year > 3000 {
		return 0
	}
	return year
}

// withinRoot reports whether dir is under root rather than being it or above
// it, which is what stops a parse walking out of the library it was given.
func withinRoot(root, dir string) bool {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return false
	}
	return rel != "." && !strings.HasPrefix(rel, "..")
}

// splitGenres reads one genre tag as the list it usually is.
//
// A genre tag is free text and the people who write them disagree: a single
// name, several separated by a semicolon, several separated by a slash, or
// several separated by a comma. All three separators are honoured, because a
// file tagged "Rock; Pop" and one tagged "Rock/Pop" mean the same thing and
// filing one of them under a genre called "Rock/Pop" would put it on a shelf
// of its own.
//
// An ampersand is left alone: "R&B" and "Drum & Bass" are one genre each,
// and splitting on it would invent two that nobody means.
func splitGenres(tag string) []string {
	if strings.TrimSpace(tag) == "" {
		return nil
	}
	parts := strings.FieldsFunc(tag, func(r rune) bool {
		return r == ';' || r == '/' || r == ',' || r == '|'
	})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.Join(strings.Fields(part), " "); part != "" {
			out = append(out, part)
		}
	}
	return out
}
