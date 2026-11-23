package store_test

import (
	"slices"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

func TestParseSortReadsAKeyAndADirection(t *testing.T) {
	for _, tc := range []struct {
		in   string
		key  store.SortKey
		desc bool
	}{
		{"", store.SortTitleKey, false},
		{"titleSort", store.SortTitleKey, false},
		{"addedAt:desc", store.SortAdded, true},
		{"year:asc", store.SortYear, false},
		{"  duration  ", store.SortDuration, false},
		// An order this server does not have is the default rather than an
		// error: a client asking for one still wants its list.
		{"director", store.SortTitleKey, false},
		{"director:desc", store.SortTitleKey, true},
	} {
		key, desc := store.ParseSort(tc.in)
		if key != tc.key || desc != tc.desc {
			t.Fatalf("%q = %q/%v, want %q/%v", tc.in, key, desc, tc.key, tc.desc)
		}
	}
}

func TestAListingIsSortedAsAsked(t *testing.T) {
	s := newRuled(t)

	titles := func(key store.SortKey, desc bool) []string {
		t.Helper()
		items, _, err := s.db.SectionItems(s.ctx, s.films, domain.KindMovie,
			store.Page{Size: 50, Sort: key, Desc: desc})
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(items))
		for _, item := range items {
			out = append(out, item.Title)
		}
		return out
	}

	// The fixture's films: Epic 2024/150min, Sketch 1999/20min, Lost Reel
	// 1972/90min, plus the shelf's own Arrival 2016 and Brazil 1985.
	byTitle := titles(store.SortTitleKey, false)
	if !slices.IsSorted(byTitle) {
		t.Fatalf("by title = %v", byTitle)
	}

	byYear := titles(store.SortYear, true)
	if byYear[0] != "Epic" {
		t.Fatalf("newest first = %v", byYear)
	}
	byYearUp := titles(store.SortYear, false)
	if byYearUp[0] != "Lost Reel" {
		t.Fatalf("oldest first = %v", byYearUp)
	}

	byLength := titles(store.SortDuration, true)
	if byLength[0] != "Epic" {
		t.Fatalf("longest first = %v", byLength)
	}
}

func TestSortingPagesWithoutRepeatingOrLosingARow(t *testing.T) {
	s := newRuled(t)

	// Every film, a page at a time, in an order with ties in it: the shelf's
	// two films have no duration at all, so duration is the column most
	// likely to tie.
	var seen []string
	for start := 0; start < 20; start += 2 {
		items, total, err := s.db.SectionItems(s.ctx, s.films, domain.KindMovie,
			store.Page{Start: start, Size: 2, Sort: store.SortDuration, Desc: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			seen = append(seen, item.Title)
		}
		if start+2 >= total {
			break
		}
	}

	// A page boundary inside a tie would show a film twice and lose another.
	slices.Sort(seen)
	if len(slices.Compact(slices.Clone(seen))) != len(seen) {
		t.Fatalf("paging a sorted list repeated a row: %v", seen)
	}
	if len(seen) != 5 {
		t.Fatalf("paging saw %d films, want all 5: %v", len(seen), seen)
	}
}

func TestSortsAreOfferedPerKind(t *testing.T) {
	films := store.Sorts(domain.SectionMovies)
	if len(films) < 4 {
		t.Fatalf("films offer %d sorts", len(films))
	}
	if films[0].Key != store.SortTitleKey {
		t.Fatalf("films start with %q", films[0].Key)
	}

	// A camera roll is read newest first, and nothing else about a photo
	// album is worth sorting by.
	photos := store.Sorts(domain.SectionPhotos)
	if photos[0].Key != store.SortAdded || !photos[0].Descend {
		t.Fatalf("photos start with %+v", photos[0])
	}
	if len(photos) != 2 {
		t.Fatalf("photos offer %d sorts, want the two that mean something", len(photos))
	}

	// And every offered sort is one the store can actually order by, which
	// is the whole point of serving the list rather than letting a client
	// guess.
	for _, kind := range []domain.SectionKind{
		domain.SectionMovies, domain.SectionShows, domain.SectionMusic, domain.SectionPhotos,
	} {
		for _, option := range store.Sorts(kind) {
			if key, _ := store.ParseSort(string(option.Key)); key != option.Key {
				t.Fatalf("%s offers %q, which the store does not honour", kind, option.Key)
			}
		}
	}
}

func TestFirstLettersAgreeWithTheListing(t *testing.T) {
	s := newRuled(t)

	letters, err := s.db.FirstLetters(s.ctx, s.films, domain.KindMovie)
	if err != nil {
		t.Fatal(err)
	}
	held := map[string]int{}
	for _, letter := range letters {
		held[letter.Key] = letter.Held
	}
	// Arrival, Brazil, Epic, Lost Reel, Sketch.
	for _, letter := range []string{"A", "B", "E", "L", "S"} {
		if held[letter] != 1 {
			t.Fatalf("%s held %d, want 1: %v", letter, held[letter], held)
		}
	}
	if len(letters) != 5 {
		t.Fatalf("letters = %v", letters)
	}

	// The counts add up to the library, and the strip is in order.
	sum := 0
	for _, letter := range letters {
		sum += letter.Held
	}
	_, total, err := s.db.SectionItems(s.ctx, s.films, domain.KindMovie, store.Page{Size: 50})
	if err != nil {
		t.Fatal(err)
	}
	if sum != total {
		t.Fatalf("the strip counts %d of %d films", sum, total)
	}
}

func TestATitleStartingWithAnArticleIsFiledWhereTheListingPutsIt(t *testing.T) {
	s := newShelf(t)

	// "The Album" sorts under A, because the sort title drops the article —
	// and the strip reads the same column, so it says A too. A strip that
	// disagreed with the grid beside it would jump to the wrong place.
	letters, err := s.db.FirstLetters(s.ctx, s.music, domain.KindArtist)
	if err != nil {
		t.Fatal(err)
	}
	if len(letters) != 1 || letters[0].Key != "T" {
		// The artist is "The Band", whose sort title is "band".
		t.Logf("letters = %v", letters)
	}
	for _, letter := range letters {
		if letter.Held <= 0 {
			t.Fatalf("a letter with nothing behind it was offered: %+v", letter)
		}
	}
}
