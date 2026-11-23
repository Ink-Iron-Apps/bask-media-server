package store_test

import (
	"slices"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

func TestGenresAreStoredAndReadBack(t *testing.T) {
	s := newShelf(t)

	if err := s.db.SetGenres(s.ctx, s.filmA, []string{"Science Fiction", "Drama"}); err != nil {
		t.Fatal(err)
	}

	genres, err := s.db.Genres(s.ctx, s.filmA)
	if err != nil {
		t.Fatal(err)
	}
	// Sorted, not in the order they were given: a page draws them as a row
	// and the order a provider happened to list them in is not a fact.
	if want := []string{"Drama", "Science Fiction"}; !slices.Equal(genres, want) {
		t.Fatalf("genres = %v, want %v", genres, want)
	}

	// And they arrive with the item, not as a second request.
	item, err := s.db.Item(s.ctx, s.filmA)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(item.Genres, []string{"Drama", "Science Fiction"}) {
		t.Fatalf("item genres = %v", item.Genres)
	}
}

func TestGenresComeWithAListing(t *testing.T) {
	s := newShelf(t)
	if err := s.db.SetGenres(s.ctx, s.filmB, []string{"Satire"}); err != nil {
		t.Fatal(err)
	}

	items, _, err := s.db.SectionItems(s.ctx, s.films, domain.KindMovie, store.Page{Size: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		switch item.ID {
		case s.filmB:
			if !slices.Equal(item.Genres, []string{"Satire"}) {
				t.Fatalf("Brazil's genres = %v", item.Genres)
			}
		default:
			if len(item.Genres) != 0 {
				t.Fatalf("%s has genres it was never given: %v", item.Title, item.Genres)
			}
		}
	}
}

func TestGenreNamesIgnoreCaseAndRepeats(t *testing.T) {
	s := newShelf(t)

	if err := s.db.SetGenres(s.ctx, s.filmA, []string{"Science Fiction", "  science   fiction  ", "Drama"}); err != nil {
		t.Fatal(err)
	}
	// The second spelling is the same genre and the whitespace is not part of
	// it, so one film is in two genres, not three.
	genres, err := s.db.Genres(s.ctx, s.filmA)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Drama", "Science Fiction"}; !slices.Equal(genres, want) {
		t.Fatalf("genres = %v, want %v", genres, want)
	}

	// And a second film spelled differently lands on the same shelf rather
	// than making one of its own.
	if err := s.db.SetGenres(s.ctx, s.filmB, []string{"SCIENCE FICTION"}); err != nil {
		t.Fatal(err)
	}
	names, err := s.db.GenreNames(s.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Drama", "Science Fiction"}; !slices.Equal(names, want) {
		t.Fatalf("library genres = %v, want %v", names, want)
	}
}

func TestSettingGenresReplacesThemWhole(t *testing.T) {
	s := newShelf(t)
	if err := s.db.SetGenres(s.ctx, s.filmA, []string{"Drama", "Thriller"}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetGenres(s.ctx, s.filmA, []string{"Thriller"}); err != nil {
		t.Fatal(err)
	}
	genres, err := s.db.Genres(s.ctx, s.filmA)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(genres, []string{"Thriller"}) {
		t.Fatalf("genres = %v, want just Thriller", genres)
	}

	// An empty list is a thing somebody can mean, and clears them.
	if err := s.db.SetGenres(s.ctx, s.filmA, nil); err != nil {
		t.Fatal(err)
	}
	genres, err = s.db.Genres(s.ctx, s.filmA)
	if err != nil {
		t.Fatal(err)
	}
	if len(genres) != 0 {
		t.Fatalf("genres = %v, want none", genres)
	}
}

func TestGenresAreCapped(t *testing.T) {
	s := newShelf(t)

	many := make([]string, 0, 40)
	for i := range 40 {
		many = append(many, "Genre "+string(rune('A'+i%26))+string(rune('a'+i/26)))
	}
	// And one that is simply too long to be the name of a shelf.
	many = append(many, string(make([]byte, 200)))
	if err := s.db.SetGenres(s.ctx, s.filmA, many); err != nil {
		t.Fatal(err)
	}
	genres, err := s.db.Genres(s.ctx, s.filmA)
	if err != nil {
		t.Fatal(err)
	}
	if len(genres) != 24 {
		t.Fatalf("kept %d genres, want the cap of 24", len(genres))
	}
}

func TestSectionGenresCountWhatIsThere(t *testing.T) {
	s := newShelf(t)
	if err := s.db.SetGenres(s.ctx, s.filmA, []string{"Science Fiction", "Drama"}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetGenres(s.ctx, s.filmB, []string{"Science Fiction"}); err != nil {
		t.Fatal(err)
	}

	genres, err := s.db.SectionGenres(s.ctx, s.films, string(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}
	if len(genres) != 2 {
		t.Fatalf("genres = %v", genres)
	}
	if genres[0].Name != "Drama" || genres[0].Held != 1 {
		t.Fatalf("first = %+v", genres[0])
	}
	if genres[1].Name != "Science Fiction" || genres[1].Held != 2 {
		t.Fatalf("second = %+v", genres[1])
	}
	if genres[1].ID == 0 {
		t.Fatal("a genre with no id cannot be handed to a client as a filter key")
	}

	// A genre on a track is not a genre of the films library.
	if err := s.db.SetGenres(s.ctx, s.trackA, []string{"Shoegaze"}); err != nil {
		t.Fatal(err)
	}
	genres, err = s.db.SectionGenres(s.ctx, s.films, string(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}
	for _, genre := range genres {
		if genre.Name == "Shoegaze" {
			t.Fatal("a track's genre reached the films library")
		}
	}
}

func TestListingNarrowsToAGenre(t *testing.T) {
	s := newShelf(t)
	if err := s.db.SetGenres(s.ctx, s.filmA, []string{"Science Fiction", "Drama"}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetGenres(s.ctx, s.filmB, []string{"Satire"}); err != nil {
		t.Fatal(err)
	}

	titles := func(page store.Page) []string {
		t.Helper()
		items, total, err := s.db.SectionItems(s.ctx, s.films, domain.KindMovie, page)
		if err != nil {
			t.Fatal(err)
		}
		if total != len(items) {
			t.Fatalf("total %d does not match the %d rows it came with", total, len(items))
		}
		out := make([]string, 0, len(items))
		for _, item := range items {
			out = append(out, item.Title)
		}
		return out
	}

	if got := titles(store.Page{Size: 50, Genres: []string{"Science Fiction"}}); !slices.Equal(got, []string{"Arrival"}) {
		t.Fatalf("science fiction = %v", got)
	}
	// Two genres means either of them, which is what ticking two boxes means.
	if got := titles(store.Page{Size: 50, Genres: []string{"Satire", "Drama"}}); !slices.Equal(got, []string{"Arrival", "Brazil"}) {
		t.Fatalf("satire or drama = %v", got)
	}
	// A film in both of the asked-for genres appears once, not twice.
	if got := titles(store.Page{Size: 50, Genres: []string{"Drama", "Science Fiction"}}); !slices.Equal(got, []string{"Arrival"}) {
		t.Fatalf("drama or science fiction = %v", got)
	}
	// A genre nothing carries narrows to nothing, rather than quietly
	// showing the whole library.
	if got := titles(store.Page{Size: 50, Genres: []string{"Western"}}); len(got) != 0 {
		t.Fatalf("western = %v, want nothing", got)
	}
	// And a filter asked for by the genre's own row id works the same way,
	// because that is what a Plex client hands back.
	genres, err := s.db.SectionGenres(s.ctx, s.films, string(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}
	var satire int64
	for _, genre := range genres {
		if genre.Name == "Satire" {
			satire = genre.ID
		}
	}
	if satire == 0 {
		t.Fatal("no id for satire")
	}
	if got := titles(store.Page{Size: 50, Genres: []string{itoa(satire)}}); !slices.Equal(got, []string{"Brazil"}) {
		t.Fatalf("by id = %v", got)
	}
}

func TestGenreFilterPagesWithinItself(t *testing.T) {
	s := newShelf(t)
	for _, id := range []int64{s.filmA, s.filmB} {
		if err := s.db.SetGenres(s.ctx, id, []string{"Drama"}); err != nil {
			t.Fatal(err)
		}
	}

	items, total, err := s.db.SectionItems(s.ctx, s.films, domain.KindMovie,
		store.Page{Size: 1, Genres: []string{"Drama"}})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("total = %d, want the 2 that match rather than the whole library", total)
	}
	if len(items) != 1 || items[0].Title != "Arrival" {
		t.Fatalf("first page = %v", items)
	}
}

func TestATrackGivesItsAlbumAndArtistTheirGenre(t *testing.T) {
	s := newShelf(t)

	writer := s.db.NewWriter(10)
	add := func(title, genre string) {
		t.Helper()
		if err := writer.Add(s.ctx, store.Found{
			SectionID: s.music, Kind: domain.KindTrack, Title: title,
			Artist: "Slowdive Ish", Album: "Wash",
			Genres: []string{genre},
			Path:   t.TempDir() + "/" + title + ".flac", Container: "flac", Size: 1,
			Duration: 4 * time.Minute,
		}); err != nil {
			t.Fatal(err)
		}
	}
	add("Catch", "Shoegaze")
	// A later track disagreeing must not re-file the album: the first word
	// wins, and the track keeps its own.
	add("Blur", "Dream Pop")
	if err := writer.Commit(s.ctx); err != nil {
		t.Fatal(err)
	}

	track := s.findDeep(t, s.music, domain.KindTrack, "Blur")
	item, err := s.db.Item(s.ctx, track)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(item.Genres, []string{"Dream Pop"}) {
		t.Fatalf("the track's own genres = %v", item.Genres)
	}

	album := s.findDeep(t, s.music, domain.KindAlbum, "Wash")
	albumItem, err := s.db.Item(s.ctx, album)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(albumItem.Genres, []string{"Shoegaze"}) {
		t.Fatalf("the album's genres = %v, want the first track's", albumItem.Genres)
	}

	artist := s.find(t, s.music, domain.KindArtist, "Slowdive Ish")
	artistItem, err := s.db.Item(s.ctx, artist)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(artistItem.Genres, []string{"Shoegaze"}) {
		t.Fatalf("the artist's genres = %v", artistItem.Genres)
	}
}

func TestAScanWithNoTagsLeavesGenresAlone(t *testing.T) {
	s := newShelf(t)
	if err := s.db.SetGenres(s.ctx, s.trackA, []string{"Shoegaze"}); err != nil {
		t.Fatal(err)
	}

	// The same file again, found by a scan that could not probe it: no tags,
	// so nothing to say, so nothing erased.
	writer := s.db.NewWriter(10)
	if err := writer.Add(s.ctx, store.Found{
		SectionID: s.music, Kind: domain.KindTrack, Title: "Anthem",
		Artist: "The Band", Album: "The Album",
		Path: t.TempDir() + "/Anthem.flac", Container: "flac", Size: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(s.ctx); err != nil {
		t.Fatal(err)
	}

	genres, err := s.db.Genres(s.ctx, s.trackA)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(genres, []string{"Shoegaze"}) {
		t.Fatalf("genres = %v, want them still there", genres)
	}
}

func TestEditingAnItemSetsItsGenres(t *testing.T) {
	s := newShelf(t)
	if err := s.db.SetGenres(s.ctx, s.filmA, []string{"Drama"}); err != nil {
		t.Fatal(err)
	}

	wanted := []string{"Science Fiction", "Mystery"}
	if err := s.db.EditItem(s.ctx, s.filmA, store.Edit{Genres: &wanted}); err != nil {
		t.Fatal(err)
	}
	genres, err := s.db.Genres(s.ctx, s.filmA)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Mystery", "Science Fiction"}; !slices.Equal(genres, want) {
		t.Fatalf("genres = %v, want %v", genres, want)
	}

	// An edit that says nothing about genres leaves them alone.
	title := "Arrival"
	if err := s.db.EditItem(s.ctx, s.filmA, store.Edit{Title: &title}); err != nil {
		t.Fatal(err)
	}
	genres, err = s.db.Genres(s.ctx, s.filmA)
	if err != nil {
		t.Fatal(err)
	}
	if len(genres) != 2 {
		t.Fatalf("genres = %v, want the two that were there", genres)
	}
}

func TestSectionFacetsListRatingsAndYears(t *testing.T) {
	s := newShelf(t)
	if err := s.db.SetRating(s.ctx, s.filmA, "12A"); err != nil {
		t.Fatal(err)
	}

	facets, err := s.db.SectionFacets(s.ctx, s.films, string(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(facets.Ratings, []string{"12A"}) {
		t.Fatalf("ratings = %v", facets.Ratings)
	}
	// Newest first, which is the order a year filter reads in.
	if !slices.Equal(facets.Years, []int{2016, 1985}) {
		t.Fatalf("years = %v", facets.Years)
	}
}

func TestASmartCollectionCanAskAboutGenre(t *testing.T) {
	s := newShelf(t)
	if err := s.db.SetGenres(s.ctx, s.filmA, []string{"Science Fiction", "Drama"}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetGenres(s.ctx, s.filmB, []string{"Satire"}); err != nil {
		t.Fatal(err)
	}

	titles := func(rules domain.Smart) []string {
		t.Helper()
		items, held, err := s.db.SmartPreview(s.ctx, rules, store.Page{Size: 50})
		if err != nil {
			t.Fatal(err)
		}
		if held != len(items) {
			t.Fatalf("held %d but %d came back", held, len(items))
		}
		out := make([]string, 0, len(items))
		for _, item := range items {
			out = append(out, item.Title)
		}
		return out
	}

	is := domain.Smart{Rules: []domain.SmartRule{
		{Field: domain.SmartLibrary, Op: domain.SmartIs, Value: itoa(s.films)},
		{Field: domain.SmartGenre, Op: domain.SmartIs, Value: "Science Fiction"},
	}}
	if got := titles(is); !slices.Equal(got, []string{"Arrival"}) {
		t.Fatalf("science fiction = %v", got)
	}

	// Case does not matter, because it does not matter anywhere else either.
	lower := domain.Smart{Rules: []domain.SmartRule{
		{Field: domain.SmartLibrary, Op: domain.SmartIs, Value: itoa(s.films)},
		{Field: domain.SmartGenre, Op: domain.SmartIs, Value: "science fiction"},
	}}
	if got := titles(lower); !slices.Equal(got, []string{"Arrival"}) {
		t.Fatalf("lower case = %v", got)
	}

	isNot := domain.Smart{Rules: []domain.SmartRule{
		{Field: domain.SmartLibrary, Op: domain.SmartIs, Value: itoa(s.films)},
		{Field: domain.SmartGenre, Op: domain.SmartIsNot, Value: "Science Fiction"},
	}}
	if got := titles(isNot); !slices.Equal(got, []string{"Brazil"}) {
		t.Fatalf("not science fiction = %v", got)
	}

	// Either of two genres, which is what "any" means.
	either := domain.Smart{Match: domain.SmartAny, Rules: []domain.SmartRule{
		{Field: domain.SmartGenre, Op: domain.SmartIs, Value: "Satire"},
		{Field: domain.SmartGenre, Op: domain.SmartIs, Value: "Drama"},
	}}
	if got := titles(either); !slices.Equal(got, []string{"Arrival", "Brazil"}) {
		t.Fatalf("satire or drama = %v", got)
	}

	// A film in both appears once.
	both := domain.Smart{Rules: []domain.SmartRule{
		{Field: domain.SmartGenre, Op: domain.SmartIs, Value: "Drama"},
		{Field: domain.SmartGenre, Op: domain.SmartIs, Value: "Science Fiction"},
	}}
	if got := titles(both); !slices.Equal(got, []string{"Arrival"}) {
		t.Fatalf("drama and science fiction = %v", got)
	}
}

func TestAGenreRuleSurvivesAnEmptyLibrary(t *testing.T) {
	s := newShelf(t)

	// A rule about a genre nothing carries is a shelf that is empty today —
	// saved, not refused, because the point of a collection that fills
	// itself is that it fills itself later.
	rules := domain.Smart{Rules: []domain.SmartRule{
		{Field: domain.SmartGenre, Op: domain.SmartIs, Value: "Western"},
	}}
	if err := store.ValidateSmart(rules); err != nil {
		t.Fatalf("refused a genre nothing carries yet: %v", err)
	}
	items, held, err := s.db.SmartPreview(s.ctx, rules, store.Page{Size: 50})
	if err != nil {
		t.Fatal(err)
	}
	if held != 0 || len(items) != 0 {
		t.Fatalf("held %d, items %v", held, items)
	}

	// And it fills when something arrives.
	if err := s.db.SetGenres(s.ctx, s.filmB, []string{"Western"}); err != nil {
		t.Fatal(err)
	}
	items, held, err = s.db.SmartPreview(s.ctx, rules, store.Page{Size: 50})
	if err != nil {
		t.Fatal(err)
	}
	if held != 1 || len(items) != 1 || items[0].Title != "Brazil" {
		t.Fatalf("held %d, items %v", held, items)
	}
}

func TestAGenreRuleNeedsAGenre(t *testing.T) {
	for _, rules := range []domain.Smart{
		{Rules: []domain.SmartRule{{Field: domain.SmartGenre, Op: domain.SmartIs}}},
		{Rules: []domain.SmartRule{{Field: domain.SmartGenre, Op: domain.SmartContains, Value: "Dra"}}},
	} {
		if err := store.ValidateSmart(rules); err == nil {
			t.Fatalf("accepted %+v", rules.Rules[0])
		}
	}
}
