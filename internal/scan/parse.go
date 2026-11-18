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
