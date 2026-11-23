package store_test

import (
	"slices"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

func TestEveryKindOfWordIsKeptApart(t *testing.T) {
	s := newShelf(t)

	for _, kind := range store.TagKinds() {
		if err := s.db.SetTags(s.ctx, kind, s.filmA, []string{"Comedy"}); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
	}

	// "Comedy" the genre and "Comedy" the label are two rows, because one
	// came from a catalogue and the other from somebody here. Merging them
	// would mean a correction silently editing a provider's answer.
	for _, kind := range store.TagKinds() {
		words, err := s.db.Tags(s.ctx, kind, s.filmA)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(words, []string{"Comedy"}) {
			t.Fatalf("%s = %v", kind, words)
		}
	}

	// And setting one kind leaves the others alone.
	if err := s.db.SetTags(s.ctx, store.TagLabel, s.filmA, []string{"Rewatch"}); err != nil {
		t.Fatal(err)
	}
	genres, err := s.db.Genres(s.ctx, s.filmA)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(genres, []string{"Comedy"}) {
		t.Fatalf("setting labels changed the genres: %v", genres)
	}
	labels, err := s.db.Tags(s.ctx, store.TagLabel, s.filmA)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(labels, []string{"Rewatch"}) {
		t.Fatalf("labels = %v", labels)
	}
}

func TestAnItemReadWholeCarriesEveryKind(t *testing.T) {
	s := newShelf(t)
	if err := s.db.SetGenres(s.ctx, s.filmA, []string{"Drama"}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetTags(s.ctx, store.TagTag, s.filmA, []string{"Sunday"}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetTags(s.ctx, store.TagLabel, s.filmA, []string{"Keep"}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetTags(s.ctx, store.TagMood, s.filmA, []string{"Melancholy"}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetTags(s.ctx, store.TagStyle, s.filmA, []string{"Slow Cinema"}); err != nil {
		t.Fatal(err)
	}

	item, err := s.db.Item(s.ctx, s.filmA)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(item.Genres, []string{"Drama"}) ||
		!slices.Equal(item.Tags, []string{"Sunday"}) ||
		!slices.Equal(item.Labels, []string{"Keep"}) ||
		!slices.Equal(item.Moods, []string{"Melancholy"}) ||
		!slices.Equal(item.Styles, []string{"Slow Cinema"}) {
		t.Fatalf("item = %+v", item)
	}

	// A listing reads genres alone: a grid draws one row of chips, and four
	// more queries per page would buy nothing anybody can see.
	items, _, err := s.db.SectionItems(s.ctx, s.films, domain.KindMovie, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, listed := range items {
		if listed.ID != s.filmA {
			continue
		}
		if !slices.Equal(listed.Genres, []string{"Drama"}) {
			t.Fatalf("a listing lost the genres: %v", listed.Genres)
		}
		if len(listed.Labels) != 0 {
			t.Fatalf("a listing read the labels as well: %v", listed.Labels)
		}
	}
}

func TestNarrowingByTwoMenusMeansBoth(t *testing.T) {
	s := newShelf(t)
	if err := s.db.SetGenres(s.ctx, s.filmA, []string{"Drama"}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetGenres(s.ctx, s.filmB, []string{"Drama"}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetTags(s.ctx, store.TagLabel, s.filmA, []string{"Keep"}); err != nil {
		t.Fatal(err)
	}

	titles := func(page store.Page) []string {
		t.Helper()
		items, _, err := s.db.SectionItems(s.ctx, s.films, domain.KindMovie, page)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(items))
		for _, item := range items {
			out = append(out, item.Title)
		}
		return out
	}

	both := store.Page{
		Size:   50,
		Genres: []string{"Drama"},
		Words:  []store.WordFilter{{Kinds: []store.TagKind{store.TagLabel}, Values: []string{"Keep"}}},
	}
	if got := titles(both); !slices.Equal(got, []string{"Arrival"}) {
		t.Fatalf("drama and labelled Keep = %v", got)
	}
}

func TestOneFilterOverTwoKindsMatchesEither(t *testing.T) {
	s := newShelf(t)
	// One film carries the word as a label, the other as a tag. Jellyfin has
	// one field for both, so a client asking for the word means either.
	if err := s.db.SetTags(s.ctx, store.TagLabel, s.filmA, []string{"Sunday"}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetTags(s.ctx, store.TagTag, s.filmB, []string{"Sunday"}); err != nil {
		t.Fatal(err)
	}

	items, total, err := s.db.SectionItems(s.ctx, s.films, domain.KindMovie, store.Page{
		Size: 50,
		Words: []store.WordFilter{{
			Kinds:  []store.TagKind{store.TagLabel, store.TagTag},
			Values: []string{"Sunday"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("either kind matched %d of 2", total)
	}

	// And a film carrying it as only one kind still appears once, not twice.
	seen := map[int64]int{}
	for _, item := range items {
		seen[item.ID]++
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("item %d appeared %d times", id, count)
		}
	}
}

func TestSectionTagsCountEachKindSeparately(t *testing.T) {
	s := newShelf(t)
	if err := s.db.SetGenres(s.ctx, s.filmA, []string{"Comedy"}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetTags(s.ctx, store.TagLabel, s.filmA, []string{"Comedy"}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetTags(s.ctx, store.TagLabel, s.filmB, []string{"Comedy"}); err != nil {
		t.Fatal(err)
	}

	genres, err := s.db.SectionTags(s.ctx, s.films, string(domain.KindMovie), store.TagGenre)
	if err != nil {
		t.Fatal(err)
	}
	if len(genres) != 1 || genres[0].Held != 1 {
		t.Fatalf("genres = %+v", genres)
	}
	labels, err := s.db.SectionTags(s.ctx, s.films, string(domain.KindMovie), store.TagLabel)
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 1 || labels[0].Held != 2 {
		t.Fatalf("labels = %+v", labels)
	}
	// Two rows with the same name and different kinds, so different ids.
	if genres[0].ID == labels[0].ID {
		t.Fatal("the genre and the label are the same row")
	}
}

func TestAnUnknownKindIsRefused(t *testing.T) {
	s := newShelf(t)
	if err := s.db.SetTags(s.ctx, store.TagKind("director"), s.filmA, []string{"Villeneuve"}); err == nil {
		t.Fatal("a kind of word this server does not keep was accepted")
	}
}

func TestEditingAnItemSetsEachKindIndependently(t *testing.T) {
	s := newShelf(t)
	if err := s.db.SetGenres(s.ctx, s.filmA, []string{"Drama"}); err != nil {
		t.Fatal(err)
	}

	labels := []string{"Keep", "Rewatch"}
	if err := s.db.EditItem(s.ctx, s.filmA, store.Edit{Labels: &labels}); err != nil {
		t.Fatal(err)
	}
	item, err := s.db.Item(s.ctx, s.filmA)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(item.Labels, []string{"Keep", "Rewatch"}) {
		t.Fatalf("labels = %v", item.Labels)
	}
	// An edit that said nothing about genres left them alone.
	if !slices.Equal(item.Genres, []string{"Drama"}) {
		t.Fatalf("genres = %v", item.Genres)
	}
}
