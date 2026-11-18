// Package markers works out which stretches of a file a viewer wants to skip.
//
// Two detectors, cheapest first:
//
//   - Chapters. A file whose chapters are titled "Intro" or "End Credits" has
//     already been labelled by a person, and reading that costs nothing beyond
//     the probe a scan already runs. It is also rare: most rips have either no
//     chapters or evenly spaced ones named "Chapter 3".
//   - Audio. An episode's intro is the stretch of audio it shares with the
//     other episodes of its season, which is a real measurement rather than a
//     guess about titles, and the only thing that works on an ordinary
//     library. That lives in fingerprint.go, because it costs a decode.
//
// Nothing here talks to a database or a client: a detector takes what was
// measured and returns markers. See [FromChapters].
package markers

import (
	"strings"
	"time"
	"unicode"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// MaxChapterIntro is the longest chapter that may become an intro marker.
//
// A detected intro runs well under two minutes. A movie's first chapter named
// "Opening Credits" or "Introduction" is five to ten minutes of actual film,
// and a Skip Intro button over that skips the opening of the film. The cap
// applies only to markers minted from a title, never to credits, which really
// are that long.
const MaxChapterIntro = 3 * time.Minute

// A chapter title is classified by its words, not by a substring match.
//
// Bask does this client-side with a word-boundary regex, which also matches
// "The Opening Ceremony Heist" and leans on the three-minute cap to keep the
// damage down. A server's answer is worse placed to be wrong: it is stored,
// served to every client, and nothing downstream re-examines it. So a title
// counts only when every word in it belongs to the vocabulary of real chapter
// labels — "Opening Credits" yes, a film's own chapter names no.
var (
	introCore    = words("intro", "introduction", "opening", "op", "openings")
	introAllowed = words("credits", "credit", "titles", "title", "theme", "song", "sequence", "the", "and")

	creditsCore    = words("credits", "credit", "outro", "ending", "closing", "ed", "endcredits")
	creditsAllowed = words("end", "final", "closing", "titles", "title", "roll", "rolling", "sequence", "the", "and", "song", "theme")
)

func words(list ...string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, w := range list {
		out[w] = true
	}
	return out
}

// tokenize splits a title into lowercase words, dropping punctuation and
// keeping numbers, which are the one thing that decorates a real label
// ("OP 2", "Credits 1").
func tokenize(title string) []string {
	var (
		out     []string
		current strings.Builder
	)
	flush := func() {
		if current.Len() > 0 {
			out = append(out, current.String())
			current.Reset()
		}
	}
	for _, r := range strings.ToLower(title) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			current.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// matches reports whether every word is part of this label's vocabulary and at
// least one of them is the word that names it.
func matches(tokens []string, core, allowed map[string]bool) bool {
	var hasCore bool
	for _, token := range tokens {
		switch {
		case core[token]:
			hasCore = true
		case allowed[token]:
		case isNumber(token):
		default:
			return false
		}
	}
	return hasCore
}

func isNumber(token string) bool {
	for _, r := range token {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return token != ""
}

// FromChapters reads markers out of chapter titles.
//
// A library whose chapters are labelled gets Skip Intro out of a plain scan,
// with no decoding and no second pass — and because the server stores the
// answer, every client gets it without recomputing anything per playback.
//
// Chapters with no usable bounds are skipped rather than guessed at: a marker
// with the wrong end is a button that skips into the episode.
func FromChapters(chapters []domain.Chapter) []domain.Marker {
	var out []domain.Marker
	for _, ch := range chapters {
		kind, ok := classify(ch.Title)
		if !ok {
			continue
		}
		if ch.End <= ch.Start {
			continue
		}
		if kind == domain.MarkerIntro && ch.End-ch.Start > MaxChapterIntro {
			continue
		}
		out = append(out, domain.Marker{
			ItemID: ch.ItemID,
			Type:   kind,
			Start:  ch.Start,
			End:    ch.End,
			Source: domain.SourceChapter,
		})
	}
	return out
}

func classify(title string) (domain.MarkerType, bool) {
	tokens := tokenize(title)
	if len(tokens) == 0 {
		return "", false
	}
	// Intro first, so "Opening Credits" is an opening rather than the end:
	// both vocabularies accept the word "credits", and only one of them
	// accepts "opening".
	switch {
	case matches(tokens, introCore, introAllowed):
		return domain.MarkerIntro, true
	case matches(tokens, creditsCore, creditsAllowed):
		return domain.MarkerCredits, true
	default:
		return "", false
	}
}
