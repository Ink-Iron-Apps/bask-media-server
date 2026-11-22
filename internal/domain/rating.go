package domain

import (
	"strconv"
	"strings"
)

// Turning a content rating into a number.
//
// A household's library carries ratings from whatever country each release
// came from: a US film is PG-13, the same film in Britain is a 15, a German
// release is FSK 16, and a television episode is TV-14. A ceiling that worked
// on labels would need every combination of them; a ceiling on an age works
// on all of them at once.
//
// So every rating is read as the age it stands for, and the comparison is one
// integer. The ages are the ones the boards themselves state, and where a
// board gives no age — "M" in Australia is advisory — the number is the age
// the board recommends.
//
// Unrated is not zero. An item nobody has rated is [UnknownAge], because
// "nobody has said" and "suitable for everyone" are different facts and
// treating them as one would either hide a children's film or show a child an
// unrated one.

// UnknownAge means no rating is recorded. Negative so it cannot be confused
// with a rating that means "everyone".
const UnknownAge = -1

// ratingAges maps a rating, normalised, to the age it stands for.
var ratingAges = map[string]int{
	// United States, film.
	"g": 0, "pg": 8, "pg13": 13, "r": 17, "nc17": 18, "m": 15, "gp": 8,
	// United States, television.
	"tvy": 0, "tvy7": 7, "tvy7fv": 7, "tvg": 0, "tvpg": 8, "tv14": 14, "tvma": 17,
	// Britain.
	"u": 0, "uc": 0, "12a": 12, "15": 15, "18": 18, "r18": 18,
	// Australia, which gives letters for the ones others give numbers.
	"ma15": 15, "ma15plus": 15, "r18plus": 18, "x18plus": 18, "ck": 0, "e": 0,
	// Canada.
	"14a": 14, "18a": 18, "a": 18,
	// Ireland, which writes its own as numbers with a letter.
	"12pg": 12, "16": 16,
	// Said in words rather than in a code, which happens in a hand-written
	// NFO and in some providers' answers.
	"all": 0, "everyone": 0, "kids": 0, "family": 0,
	"teen": 13, "mature": 17, "adult": 18,
}

// unrated are the spellings of "nobody has rated this".
var unrated = map[string]bool{
	"": true, "nr": true, "notrated": true, "unrated": true, "ur": true,
	"none": true, "unknown": true, "na": true, "n/a": true, "notyetrated": true,
}

// AgeOfRating is the age a content rating stands for, or [UnknownAge].
//
// Lenient about spelling because the labels arrive from a provider, a
// filename and occasionally somebody's own NFO: a country prefix is dropped,
// punctuation and spaces are ignored, and a bare number is itself. A label
// this does not recognise is unknown rather than a guess — a wrong number
// here is a child shown the wrong film.
func AgeOfRating(label string) int {
	// "Not rated" is checked before the country prefix is stripped, because
	// "N/A" with the prefix rule applied is "A", which is a real rating in
	// Canada and means eighteen.
	if unrated[squash(label)] {
		return UnknownAge
	}
	normalised := normaliseRating(label)
	if unrated[normalised] {
		return UnknownAge
	}
	if age, found := ratingAges[normalised]; found {
		return age
	}
	// A bare number is the age, which is how most of Europe writes it.
	if age, err := strconv.Atoi(normalised); err == nil && age >= 0 && age <= 21 {
		return age
	}
	// "FSK 16", "16+", "Rated 15": a number inside a label that is otherwise
	// not recognised still says what it says.
	if age, found := numberIn(normalised); found {
		return age
	}
	return UnknownAge
}

// normaliseRating strips the country prefix and everything that is only
// punctuation.
func normaliseRating(label string) string {
	label = strings.ToLower(strings.TrimSpace(label))
	// "us:PG-13", "gb/15", "de|16": a prefix naming where the rating came
	// from, which does not change what it means. Only where the prefix looks
	// like a country — two or three letters — because "N/A" is not a British
	// rating of "A".
	for _, separator := range []string{":", "/", "|"} {
		before, rest, found := strings.Cut(label, separator)
		if !found {
			continue
		}
		before = strings.TrimSpace(before)
		if len(before) >= 2 && len(before) <= 3 && onlyLetters(before) {
			label = strings.TrimSpace(rest)
		}
	}
	return squash(label)
}

// squash keeps the letters and digits, and spells a plus so "MA15+" and
// "MA15" are not the same key.
func squash(label string) string {
	var out strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(label)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out.WriteRune(r)
		case r == '+':
			out.WriteString("plus")
		}
	}
	return out.String()
}

func onlyLetters(value string) bool {
	for _, r := range value {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return value != ""
}

// numberIn is the first run of digits in a label, as an age.
func numberIn(label string) (int, bool) {
	start := -1
	for i, r := range label {
		if r >= '0' && r <= '9' {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			if age, err := strconv.Atoi(label[start:i]); err == nil && age >= 0 && age <= 21 {
				return age, true
			}
			start = -1
		}
	}
	if start >= 0 {
		if age, err := strconv.Atoi(label[start:]); err == nil && age >= 0 && age <= 21 {
			return age, true
		}
	}
	return 0, false
}
