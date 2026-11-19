// Package scan turns a directory of files into library rows.
//
// Filenames are the only metadata source in v1. That is deliberate: the layout
// BMS reads is the layout Bask already writes for downloads and the layout the
// *arr stack already produces, so a library that exists today scans correctly
// today, before a single external metadata provider is wired in.
package scan

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// MediaExtensions are the containers a scan will pick up. A list rather than
// "every file", so subtitles, artwork, NFO sidecars and a stray .part are not
// mistaken for media.
var MediaExtensions = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true,
	".ts": true, ".m2ts": true, ".webm": true, ".mpg": true, ".mpeg": true,
	".wmv": true, ".flv": true, ".ogv": true,
}

// IsMedia reports whether a path looks like something playable.
func IsMedia(path string) bool {
	return MediaExtensions[strings.ToLower(filepath.Ext(path))]
}

var (
	// "Title (2016)" anywhere in a name, taking the last year so a title that
	// contains one of its own ("2012 (2009)") resolves the way a human reads it.
	yearPattern = regexp.MustCompile(`\((\d{4})\)`)

	// S01E02, s1e2, 1x02 — the three forms the tools in use actually emit.
	episodePattern = regexp.MustCompile(`(?i)\bs(\d{1,3})[ ._-]?e(\d{1,4})\b`)
	altEpisode     = regexp.MustCompile(`(?i)\b(\d{1,3})x(\d{1,4})\b`)

	// "Season 01", "season 1", "S01" as a directory name.
	seasonDir = regexp.MustCompile(`(?i)^(?:season[ ._-]*|s)(\d{1,3})$`)
)

// Movie is what a movie file's path says about it.
type Movie struct {
	Title string
	Year  int
}

// ParseMovie reads a title and year out of a movie's path.
//
// Prefers the containing directory over the filename, because the directory is
// where both Bask's downloader and the *arr tools put the clean name, while the
// filename often carries release junk. Falls back to the filename for a flat
// library with no per-movie folders.
func ParseMovie(path string) Movie {
	dir := filepath.Base(filepath.Dir(path))
	if m, ok := movieFrom(dir); ok {
		return m
	}
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if m, ok := movieFrom(base); ok {
		return m
	}
	return Movie{Title: cleanTitle(base)}
}

func movieFrom(name string) (Movie, bool) {
	matches := yearPattern.FindAllStringSubmatchIndex(name, -1)
	if len(matches) == 0 {
		return Movie{}, false
	}
	last := matches[len(matches)-1]
	year, err := strconv.Atoi(name[last[2]:last[3]])
	if err != nil {
		return Movie{}, false
	}
	title := cleanTitle(name[:last[0]])
	if title == "" {
		return Movie{}, false
	}
	return Movie{Title: title, Year: year}, true
}

// Episode is what an episode file's path says about it.
type Episode struct {
	Show     string
	ShowYear int
	Season   int
	Number   int
	Title    string
}

// ParseEpisode reads show, season, episode and title out of an episode's path,
// relative to the section root that contains it.
//
// The show and season come from the directory structure, which is where they
// are reliable; the numbers come from the filename, which is where they are
// written. A file whose name carries no recognisable episode marker returns
// ok=false rather than a guess — a wrong episode number is worse in a library
// than a file that did not import.
func ParseEpisode(root, path string) (Episode, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	base := strings.TrimSuffix(parts[len(parts)-1], filepath.Ext(parts[len(parts)-1]))

	var ep Episode
	if m := episodePattern.FindStringSubmatch(base); m != nil {
		ep.Season, _ = strconv.Atoi(m[1])
		ep.Number, _ = strconv.Atoi(m[2])
	} else if m := altEpisode.FindStringSubmatch(base); m != nil {
		ep.Season, _ = strconv.Atoi(m[1])
		ep.Number, _ = strconv.Atoi(m[2])
	} else {
		return Episode{}, false
	}

	// Walk up: the nearest directory that looks like a season names the season,
	// and the one above it names the show. Without a season directory the
	// immediate parent is the show, which is the flat layout some libraries use.
	showIdx := -1
	for i := len(parts) - 2; i >= 0; i-- {
		if m := seasonDir.FindStringSubmatch(parts[i]); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && ep.Season == 0 {
				ep.Season = n
			}
			showIdx = i - 1
			break
		}
		if showIdx == -1 {
			showIdx = i
		}
	}
	if showIdx >= 0 && showIdx < len(parts)-1 {
		show := parts[showIdx]
		if m, ok := movieFrom(show); ok {
			ep.Show, ep.ShowYear = m.Title, m.Year
		} else {
			ep.Show = cleanTitle(show)
		}
	}
	if ep.Show == "" {
		return Episode{}, false
	}

	ep.Title = episodeTitle(base)
	if ep.Title == "" {
		ep.Title = "Episode " + strconv.Itoa(ep.Number)
	}
	return ep, true
}

// episodeTitle takes what follows the episode marker, which is where both
// Bask's downloader ("S01E01 - Part 1") and the usual renamers put it.
func episodeTitle(base string) string {
	loc := episodePattern.FindStringIndex(base)
	if loc == nil {
		loc = altEpisode.FindStringIndex(base)
	}
	if loc == nil {
		return ""
	}
	return cleanTitle(base[loc[1]:])
}

var separators = strings.NewReplacer(".", " ", "_", " ")

// cleanTitle turns a filename fragment into something presentable: separators
// become spaces, leading punctuation goes, and runs of whitespace collapse.
func cleanTitle(s string) string {
	s = separators.Replace(s)
	s = strings.TrimSpace(s)
	s = strings.TrimLeft(s, "-–— ")
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimSpace(s)
}

// SubtitleExtensions are the sidecar formats a scan will pick up.
//
// Text formats only, plus the two bitmap ones that are routinely found beside
// a film: what can be done with each is decided later, and a row for a
// subtitle nobody can render is still worth having, because a direct play on
// a client with its own renderer can use it.
var SubtitleExtensions = map[string]string{
	".srt": "srt", ".vtt": "webvtt", ".ass": "ass", ".ssa": "ass",
	".sub": "subrip", ".sup": "pgs", ".idx": "vobsub",
}

// IsSubtitle reports whether a path looks like a subtitle file.
func IsSubtitle(path string) bool {
	_, found := SubtitleExtensions[strings.ToLower(filepath.Ext(path))]
	return found
}

// Sidecar is a subtitle file found beside a media file.
type Sidecar struct {
	Path     string
	Codec    string
	Language string // as written in the name: "en", "eng", "english", or empty
	Title    string
	Forced   bool
	Hearing  bool // marked SDH, HI or CC
}

// forcedWords and hearingWords are the tags the renamers in use actually
// write. Matched as whole words in the part of the name after the title, so a
// film called "Forced Entry" does not acquire a flag from its own title.
var (
	forcedWords  = map[string]bool{"forced": true, "foreign": true}
	hearingWords = map[string]bool{"sdh": true, "hi": true, "cc": true, "hearing": true}
)

// ParseSidecar decides whether a subtitle file belongs to a media file, and
// reads what its name says about it.
//
// The convention every tool in this space writes is the media file's name,
// then dot-separated tags: "Film (2002).en.forced.srt", "Film (2002).eng.sdh.srt",
// or just "Film (2002).srt". Anything that does not start with the media
// file's name belongs to another file in the same folder.
func ParseSidecar(mediaPath, subtitlePath string) (Sidecar, bool) {
	codec, known := SubtitleExtensions[strings.ToLower(filepath.Ext(subtitlePath))]
	if !known {
		return Sidecar{}, false
	}
	media := strings.TrimSuffix(filepath.Base(mediaPath), filepath.Ext(mediaPath))
	name := strings.TrimSuffix(filepath.Base(subtitlePath), filepath.Ext(subtitlePath))

	out := Sidecar{Path: subtitlePath, Codec: codec}
	if strings.EqualFold(name, media) {
		// "Film (2002).srt": the film's subtitle, saying nothing else.
		return out, true
	}
	if !strings.HasPrefix(strings.ToLower(name), strings.ToLower(media)) {
		return Sidecar{}, false
	}

	rest := name[len(media):]
	separator := rest[0]
	if !strings.ContainsRune(".-_ ", rune(separator)) {
		// "Film 2.srt" is not a subtitle for "Film": what follows the name
		// has to be separated from it.
		return Sidecar{}, false
	}

	tags := strings.FieldsFunc(rest, func(r rune) bool {
		return r == '.' || r == '-' || r == '_' || r == ' '
	})
	var titleParts []string
	for _, tag := range tags {
		lower := strings.ToLower(tag)
		switch {
		case forcedWords[lower]:
			out.Forced = true
		case hearingWords[lower]:
			out.Hearing = true
		case out.Language == "" && looksLikeLanguage(lower):
			out.Language = lower
		default:
			titleParts = append(titleParts, tag)
		}
	}

	// A dot is the convention and carries anything: "Film.Director commentary.srt"
	// is this film's. Any other separator has to be followed by tags that are
	// recognisably about a subtitle, because "Film (2002) extras.mkv" is a
	// different film and "Film (2002) extras.srt" is its subtitle, not this
	// one's.
	if separator != '.' && len(titleParts) > 0 {
		return Sidecar{}, false
	}
	out.Title = strings.Join(titleParts, " ")
	return out, true
}

// looksLikeLanguage accepts the two- and three-letter codes and the handful of
// full names that turn up in filenames. Deliberately not a complete table:
// what is not recognised becomes part of the title, which is visible and
// correctable, rather than a wrong language nobody can see.
func looksLikeLanguage(tag string) bool {
	if len(tag) == 2 || len(tag) == 3 {
		for _, r := range tag {
			if r < 'a' || r > 'z' {
				return false
			}
		}
		return true
	}
	return languageNames[tag] != ""
}

// languageNames maps the spelled-out names to their three-letter codes.
var languageNames = map[string]string{
	"english": "eng", "spanish": "spa", "french": "fre", "german": "ger",
	"italian": "ita", "portuguese": "por", "dutch": "dut", "swedish": "swe",
	"norwegian": "nor", "danish": "dan", "finnish": "fin", "polish": "pol",
	"russian": "rus", "japanese": "jpn", "korean": "kor", "chinese": "chi",
	"arabic": "ara", "hebrew": "heb", "hindi": "hin", "turkish": "tur",
	"czech": "cze", "greek": "gre", "hungarian": "hun", "romanian": "rum",
	"thai": "tha", "ukrainian": "ukr", "vietnamese": "vie",
}

// LanguageCode normalises what a filename or a container says into the
// three-letter code clients expect, leaving anything unrecognised as it is.
func LanguageCode(language string) string {
	lower := strings.ToLower(strings.TrimSpace(language))
	if code, found := languageNames[lower]; found {
		return code
	}
	if code, found := twoLetterCodes[lower]; found {
		return code
	}
	return lower
}

// twoLetterCodes covers the ISO 639-1 codes that turn up in filenames, mapped
// to the 639-2 codes containers and clients use.
var twoLetterCodes = map[string]string{
	"en": "eng", "es": "spa", "fr": "fre", "de": "ger", "it": "ita",
	"pt": "por", "nl": "dut", "sv": "swe", "no": "nor", "da": "dan",
	"fi": "fin", "pl": "pol", "ru": "rus", "ja": "jpn", "ko": "kor",
	"zh": "chi", "ar": "ara", "he": "heb", "hi": "hin", "tr": "tur",
	"cs": "cze", "el": "gre", "hu": "hun", "ro": "rum", "th": "tha",
	"uk": "ukr", "vi": "vie",
}

// LanguageName is what to show a person for a code, falling back to the code
// itself so an unknown language is still labelled with something.
func LanguageName(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	for name, mapped := range languageNames {
		if mapped == code {
			return strings.ToUpper(name[:1]) + name[1:]
		}
	}
	if code == "" || code == "und" {
		return "Unknown"
	}
	return strings.ToUpper(code)
}
