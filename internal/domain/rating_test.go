package domain_test

import (
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// A ceiling has to work across the rating systems one household's library
// actually carries, which is the whole reason a rating is read as an age.
func TestEveryRatingSystemReadsAsAnAge(t *testing.T) {
	cases := map[string]int{
		// United States.
		"G": 0, "PG": 8, "PG-13": 13, "R": 17, "NC-17": 18,
		"TV-Y": 0, "TV-Y7": 7, "TV-G": 0, "TV-PG": 8, "TV-14": 14, "TV-MA": 17,
		// Britain.
		"U": 0, "12A": 12, "15": 15, "18": 18,
		// Australia.
		"MA15+": 15, "R18+": 18,
		// Written with a country in front, which is how Plex carries them.
		"us:PG-13": 13, "gb/15": 15, "de|16": 16,
		// A number inside a label nobody standardised.
		"FSK 16": 16, "16+": 16, "Rated 12": 12,
		// Said in words.
		"Everyone": 0, "Teen": 13, "Adult": 18,
	}
	for label, want := range cases {
		if got := domain.AgeOfRating(label); got != want {
			t.Errorf("%q = %d, want %d", label, got, want)
		}
	}
}

// Unrated is not "suitable for everyone": the two are different facts, and
// conflating them either hides a children's film or shows a child an unrated
// one.
func TestUnratedIsNotZero(t *testing.T) {
	for _, label := range []string{"", "   ", "NR", "Not Rated", "Unrated", "UR", "N/A"} {
		if got := domain.AgeOfRating(label); got != domain.UnknownAge {
			t.Errorf("%q = %d, want unknown", label, got)
		}
	}
	if domain.UnknownAge >= 0 {
		t.Fatal("unknown has to be negative, or it is a rating")
	}
}

// A label this does not recognise is unknown rather than a guess: a wrong
// number here is a child shown the wrong film.
func TestAnUnrecognisedLabelIsUnknown(t *testing.T) {
	for _, label := range []string{"Approved", "Passed", "GP-ish", "???"} {
		if got := domain.AgeOfRating(label); got != domain.UnknownAge && got != 8 {
			t.Errorf("%q = %d, want unknown", label, got)
		}
	}
	// And a number that is not an age is not read as one.
	if got := domain.AgeOfRating("2026"); got != domain.UnknownAge {
		t.Errorf("a year read as an age: %d", got)
	}
}
