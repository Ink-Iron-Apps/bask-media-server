package scan_test

import (
	"testing"

	"github.com/inknironapps/bask-media-server/internal/scan"
)

// Two cuts of a film are one film with two files. The title parser drops the
// edition so the rows collide, and the edition is kept on the file so a
// client can offer the choice rather than two identical-looking rows.
func TestTwoEditionsAreOneFilm(t *testing.T) {
	cases := []struct {
		path    string
		title   string
		year    int
		edition string
	}{
		{"/films/Blade Runner (1982)/Blade Runner (1982).mkv", "Blade Runner", 1982, ""},
		{"/films/Blade Runner (1982)/Blade Runner (1982) {edition-Final Cut}.mkv", "Blade Runner", 1982, "Final Cut"},
		{"/films/Blade Runner (1982)/Blade Runner (1982) [Director's Cut].mkv", "Blade Runner", 1982, "Director's Cut"},
		{"/films/Blade Runner (1982) [Theatrical].mkv", "Blade Runner", 1982, "Theatrical"},

		// Release junk in the same brackets is not a cut of the film. A
		// viewer offered "Blade Runner — x265" has been told nothing.
		{"/films/Blade Runner (1982) [1080p][x265].mkv", "Blade Runner", 1982, ""},
		{"/films/Arrival (2016) [WEB-DL][AAC].mkv", "Arrival", 2016, ""},
		{"/films/Arrival (2016) [YTS.MX].mkv", "Arrival", 2016, ""},
	}

	for _, want := range cases {
		m := scan.ParseMovie(want.path)
		if m.Title != want.title || m.Year != want.year {
			t.Errorf("%s: parsed %q (%d), want %q (%d)", want.path, m.Title, m.Year, want.title, want.year)
		}
		if got := scan.Edition(want.path); got != want.edition {
			t.Errorf("%s: edition %q, want %q", want.path, got, want.edition)
		}
	}
}

// An edition with junk beside it keeps the edition.
func TestAnEditionSurvivesTheJunkAroundIt(t *testing.T) {
	path := "/films/Dune (2021) {edition-Extended}[2160p][HDR].mkv"
	if m := scan.ParseMovie(path); m.Title != "Dune" || m.Year != 2021 {
		t.Fatalf("parsed %q (%d)", m.Title, m.Year)
	}
	if got := scan.Edition(path); got != "Extended" {
		t.Fatalf("edition %q", got)
	}
}
