package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// shelf is a small library to arrange: two films and two tracks, in separate
// sections so the visibility rules have something to bite on.
type shelf struct {
	db      *store.DB
	ctx     context.Context
	films   int64
	music   int64
	user    int64
	filmA   int64
	filmB   int64
	trackA  int64
	trackB  int64
	allItem []int64
}

func newShelf(t *testing.T) *shelf {
	t.Helper()
	db, ctx := open(t)

	films, err := db.CreateSection(ctx, "Films", domain.SectionMovies, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	music, err := db.CreateSection(ctx, "Music", domain.SectionMusic, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	user, err := db.CreateUser(ctx, "rae", false)
	if err != nil {
		t.Fatal(err)
	}

	writer := db.NewWriter(10)
	add := func(section int64, kind domain.Kind, title string, year int) {
		t.Helper()
		found := store.Found{SectionID: section, Kind: kind, Title: title, Year: year}
		switch kind {
		case domain.KindMovie:
			found.Path = t.TempDir() + "/" + title + ".mkv"
			found.Container = "mkv"
			found.Size = 1
		case domain.KindTrack:
			found.Artist = "The Band"
			found.Album = "The Album"
			found.Path = t.TempDir() + "/" + title + ".flac"
			found.Container = "flac"
			found.Size = 1
		}
		if err := writer.Add(ctx, found); err != nil {
			t.Fatal(err)
		}
	}
	add(films.ID, domain.KindMovie, "Arrival", 2016)
	add(films.ID, domain.KindMovie, "Brazil", 1985)
	add(music.ID, domain.KindTrack, "Anthem", 0)
	add(music.ID, domain.KindTrack, "Benediction", 0)
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	s := &shelf{db: db, ctx: ctx, films: films.ID, music: music.ID, user: user.ID}
	s.filmA = s.find(t, films.ID, domain.KindMovie, "Arrival")
	s.filmB = s.find(t, films.ID, domain.KindMovie, "Brazil")
	s.trackA = s.findDeep(t, music.ID, domain.KindTrack, "Anthem")
	s.trackB = s.findDeep(t, music.ID, domain.KindTrack, "Benediction")
	s.allItem = []int64{s.filmA, s.filmB, s.trackA, s.trackB}
	return s
}

func (s *shelf) find(t *testing.T, section int64, kind domain.Kind, title string) int64 {
	t.Helper()
	items, _, err := s.db.SectionItems(s.ctx, section, kind, store.Page{Size: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Title == title {
			return item.ID
		}
	}
	t.Fatalf("no %s called %q", kind, title)
	return 0
}

func (s *shelf) findDeep(t *testing.T, section int64, kind domain.Kind, title string) int64 {
	t.Helper()
	items, err := s.db.SectionItemsOfKind(s.ctx, section, kind, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Title == title {
			return item.ID
		}
	}
	t.Fatalf("no %s called %q", kind, title)
	return 0
}

// onlyFilms is the handle a device restricted to the films library gets.
func (s *shelf) onlyFilms() *store.DB { return s.db.Visible([]int64{s.films}) }

/* ---------- favourites ---------- */

func TestAFavouriteIsMarkedAndUnmarked(t *testing.T) {
	s := newShelf(t)

	if err := s.db.SetFavourite(s.ctx, s.user, s.filmA, true); err != nil {
		t.Fatal(err)
	}
	marked, _, err := s.db.Favourites(s.ctx, s.user, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(marked) != 1 || marked[0].ID != s.filmA {
		t.Fatalf("favourites = %v", marked)
	}

	if err := s.db.SetFavourite(s.ctx, s.user, s.filmA, false); err != nil {
		t.Fatal(err)
	}
	marked, _, err = s.db.Favourites(s.ctx, s.user, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(marked) != 0 {
		t.Fatalf("still favourite after unmarking: %v", marked)
	}
}

// A favourite is an opinion, so two people in a household hold their own.
func TestFavouritesBelongToOneAccount(t *testing.T) {
	s := newShelf(t)
	other, err := s.db.CreateUser(s.ctx, "sam", false)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.db.SetFavourite(s.ctx, s.user, s.filmA, true); err != nil {
		t.Fatal(err)
	}
	theirs, _, err := s.db.Favourites(s.ctx, other.ID, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(theirs) != 0 {
		t.Fatalf("one account's favourite showed up in another's: %v", theirs)
	}
}

func TestAFavouriteInAHiddenLibraryIsNotListed(t *testing.T) {
	s := newShelf(t)
	if err := s.db.SetFavourite(s.ctx, s.user, s.filmA, true); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetFavourite(s.ctx, s.user, s.trackA, true); err != nil {
		t.Fatal(err)
	}

	marked, total, err := s.onlyFilms().Favourites(s.ctx, s.user, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(marked) != 1 || marked[0].ID != s.filmA {
		t.Fatalf("a films-only device saw %v", marked)
	}
	if total != 1 {
		t.Fatalf("total counted things the device cannot see: %d", total)
	}
}

// Marking something it was never shown is a client guessing, and the honest
// answer to a guess is nothing at all rather than a confirmation.
func TestAHiddenItemCannotBeFavourited(t *testing.T) {
	s := newShelf(t)
	if err := s.onlyFilms().SetFavourite(s.ctx, s.user, s.trackA, true); err != nil {
		t.Fatal(err)
	}
	marked, _, err := s.db.Favourites(s.ctx, s.user, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(marked) != 0 {
		t.Fatalf("a hidden item was favourited: %v", marked)
	}
}

func TestTheFavouriteSetDecoratesAListing(t *testing.T) {
	s := newShelf(t)
	if err := s.db.SetFavourite(s.ctx, s.user, s.filmB, true); err != nil {
		t.Fatal(err)
	}
	set, err := s.db.FavouriteSet(s.ctx, s.user, s.allItem)
	if err != nil {
		t.Fatal(err)
	}
	if !set[s.filmB] || set[s.filmA] || len(set) != 1 {
		t.Fatalf("favourite set = %v", set)
	}
}

/* ---------- playlists ---------- */

func TestAPlaylistKeepsTheOrderThingsWerePutIn(t *testing.T) {
	s := newShelf(t)
	list, err := s.db.CreatePlaylist(s.ctx, s.user, "Evening")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.AddToPlaylist(s.ctx, s.user, list.ID, []int64{s.trackB, s.trackA}); err != nil {
		t.Fatal(err)
	}

	entries, total, err := s.db.PlaylistEntries(s.ctx, s.user, list.ID, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(entries) != 2 {
		t.Fatalf("entries = %d, total = %d", len(entries), total)
	}
	if entries[0].Item.ID != s.trackB || entries[1].Item.ID != s.trackA {
		t.Fatalf("order was not kept: %v", entries)
	}
	// The files come with them, so a client can play the list without a
	// request per track.
	if len(entries[0].Item.Parts) == 0 {
		t.Fatal("a playable entry came back with no file")
	}
}

// The same track twice is two entries, which is what a playlist is for.
func TestTheSameTrackCanAppearTwice(t *testing.T) {
	s := newShelf(t)
	list, err := s.db.CreatePlaylist(s.ctx, s.user, "On repeat")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.AddToPlaylist(s.ctx, s.user, list.ID, []int64{s.trackA, s.trackA}); err != nil {
		t.Fatal(err)
	}

	entries, _, err := s.db.PlaylistEntries(s.ctx, s.user, list.ID, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected two entries, got %d", len(entries))
	}
	if entries[0].ID == entries[1].ID {
		t.Fatal("two entries share an id, so neither can be removed on its own")
	}

	// And removing one leaves the other.
	removed, err := s.db.RemoveFromPlaylist(s.ctx, s.user, list.ID, []int64{entries[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed %d", removed)
	}
	left, _, err := s.db.PlaylistEntries(s.ctx, s.user, list.ID, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].ID != entries[1].ID {
		t.Fatalf("the wrong entry went: %v", left)
	}
}

func TestAnEntryMovesToWhereItWasPut(t *testing.T) {
	s := newShelf(t)
	list, err := s.db.CreatePlaylist(s.ctx, s.user, "Set")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.AddToPlaylist(s.ctx, s.user, list.ID,
		[]int64{s.trackA, s.trackB, s.filmA}); err != nil {
		t.Fatal(err)
	}
	entries, _, err := s.db.PlaylistEntries(s.ctx, s.user, list.ID, store.Page{})
	if err != nil {
		t.Fatal(err)
	}

	// Last to first.
	if err := s.db.MovePlaylistEntry(s.ctx, s.user, list.ID, entries[2].ID, 0); err != nil {
		t.Fatal(err)
	}
	after, _, err := s.db.PlaylistEntries(s.ctx, s.user, list.ID, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{entries[2].ID, entries[0].ID, entries[1].ID}
	for i, id := range want {
		if after[i].ID != id {
			t.Fatalf("after moving, order is %v, want %v", ids(after), want)
		}
	}
}

// A playlist is not shared, so somebody else's is not merely forbidden: it
// does not exist, because the other answer says that it does.
func TestAPlaylistIsNotVisibleToAnotherAccount(t *testing.T) {
	s := newShelf(t)
	other, err := s.db.CreateUser(s.ctx, "sam", false)
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.db.CreatePlaylist(s.ctx, s.user, "Mine")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.db.Playlist(s.ctx, other.ID, list.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another account read the playlist: %v", err)
	}
	if _, err := s.db.AddToPlaylist(s.ctx, other.ID, list.ID, []int64{s.trackA}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another account added to the playlist: %v", err)
	}
	if err := s.db.DeletePlaylist(s.ctx, other.ID, list.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another account deleted the playlist: %v", err)
	}
}

// The playlist survives; the entry it cannot see does not.
func TestAPlaylistHidesTheEntriesADeviceMayNotSee(t *testing.T) {
	s := newShelf(t)
	list, err := s.db.CreatePlaylist(s.ctx, s.user, "Both kinds")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.AddToPlaylist(s.ctx, s.user, list.ID, []int64{s.filmA, s.trackA}); err != nil {
		t.Fatal(err)
	}

	entries, total, err := s.onlyFilms().PlaylistEntries(s.ctx, s.user, list.ID, store.Page{})
	if err != nil {
		t.Fatalf("the playlist itself vanished: %v", err)
	}
	if len(entries) != 1 || entries[0].Item.ID != s.filmA {
		t.Fatalf("a films-only device saw %v", ids(entries))
	}
	if total != 1 {
		t.Fatalf("total counted an entry the device cannot see: %d", total)
	}
}

func TestAddingSkipsWhatTheDeviceCannotSee(t *testing.T) {
	s := newShelf(t)
	list, err := s.db.CreatePlaylist(s.ctx, s.user, "Tried")
	if err != nil {
		t.Fatal(err)
	}
	added, err := s.onlyFilms().AddToPlaylist(s.ctx, s.user, list.ID, []int64{s.filmA, s.trackA})
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 {
		t.Fatalf("added %d, want 1", added)
	}
	entries, _, err := s.db.PlaylistEntries(s.ctx, s.user, list.ID, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Item.ID != s.filmA {
		t.Fatalf("a hidden item got in: %v", ids(entries))
	}
}

// A music client asking for audio playlists should not be offered films.
func TestAPlaylistKnowsWhatItHolds(t *testing.T) {
	s := newShelf(t)
	list, err := s.db.CreatePlaylist(s.ctx, s.user, "Sounds")
	if err != nil {
		t.Fatal(err)
	}

	kindNow := func() domain.PlaylistKind {
		t.Helper()
		read, err := s.db.Playlist(s.ctx, s.user, list.ID)
		if err != nil {
			t.Fatal(err)
		}
		return read.Kind
	}

	if _, err := s.db.AddToPlaylist(s.ctx, s.user, list.ID, []int64{s.trackA, s.trackB}); err != nil {
		t.Fatal(err)
	}
	if kindNow() != domain.PlaylistAudio {
		t.Fatalf("tracks only, kind = %q", kindNow())
	}

	if _, err := s.db.AddToPlaylist(s.ctx, s.user, list.ID, []int64{s.filmA}); err != nil {
		t.Fatal(err)
	}
	if kindNow() != domain.PlaylistMixed {
		t.Fatalf("after a film went in, kind = %q", kindNow())
	}
}

func TestDeletingAPlaylistLeavesTheLibraryAlone(t *testing.T) {
	s := newShelf(t)
	list, err := s.db.CreatePlaylist(s.ctx, s.user, "Temporary")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.AddToPlaylist(s.ctx, s.user, list.ID, []int64{s.filmA}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.DeletePlaylist(s.ctx, s.user, list.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := s.db.Item(s.ctx, s.filmA); err != nil {
		t.Fatalf("deleting a playlist took the film with it: %v", err)
	}
	lists, err := s.db.Playlists(s.ctx, s.user)
	if err != nil {
		t.Fatal(err)
	}
	if len(lists) != 0 {
		t.Fatalf("playlists = %v", lists)
	}
}

/* ---------- collections ---------- */

func TestACollectionGroupsItemsForEverybody(t *testing.T) {
	s := newShelf(t)
	made, err := s.db.CreateCollection(s.ctx, "Science fiction", "Two of them")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetCollectionItems(s.ctx, made.ID, []int64{s.filmB, s.filmA}); err != nil {
		t.Fatal(err)
	}

	all, err := s.db.Collections(s.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Title != "Science fiction" || all[0].Held != 2 {
		t.Fatalf("collections = %+v", all)
	}

	items, total, err := s.db.CollectionItems(s.ctx, made.ID, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || items[0].ID != s.filmB {
		t.Fatalf("collection order was not kept: %v", items)
	}
}

// Saving the same list twice is the administrator pressing the button twice,
// not an instruction to hold everything in duplicate.
func TestSettingACollectionTwiceDoesNotDuplicateIt(t *testing.T) {
	s := newShelf(t)
	made, err := s.db.CreateCollection(s.ctx, "Repeat", "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.db.SetCollectionItems(s.ctx, made.ID, []int64{s.filmA, s.filmB}); err != nil {
			t.Fatal(err)
		}
	}
	_, total, err := s.db.CollectionItems(s.ctx, made.ID, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("total = %d after saving twice", total)
	}
}

// A group with nothing in it still tells a viewer that something exists,
// which is the thing a hidden library is meant to stop.
func TestACollectionOfHiddenThingsIsNotListedAtAll(t *testing.T) {
	s := newShelf(t)
	made, err := s.db.CreateCollection(s.ctx, "Music only", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetCollectionItems(s.ctx, made.ID, []int64{s.trackA, s.trackB}); err != nil {
		t.Fatal(err)
	}

	films := s.onlyFilms()
	listed, err := films.Collections(s.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("a films-only device was shown %+v", listed)
	}
	if _, err := films.Collection(s.ctx, made.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("reading it directly worked: %v", err)
	}
}

// Partly visible is different from invisible: the group is real, and what it
// counts is what the asker may see.
func TestAPartlyHiddenCollectionShowsOnlyWhatIsVisible(t *testing.T) {
	s := newShelf(t)
	made, err := s.db.CreateCollection(s.ctx, "A bit of both", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetCollectionItems(s.ctx, made.ID, []int64{s.filmA, s.trackA}); err != nil {
		t.Fatal(err)
	}

	films := s.onlyFilms()
	listed, err := films.Collections(s.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Held != 1 {
		t.Fatalf("a films-only device was told %+v", listed)
	}
	items, total, err := films.CollectionItems(s.ctx, made.ID, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(items) != 1 || items[0].ID != s.filmA {
		t.Fatalf("contents = %v (total %d)", items, total)
	}
}

func TestAnItemKnowsWhichCollectionsItIsIn(t *testing.T) {
	s := newShelf(t)
	first, err := s.db.CreateCollection(s.ctx, "Alpha", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.db.CreateCollection(s.ctx, "Beta", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetCollectionItems(s.ctx, first.ID, []int64{s.filmA}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetCollectionItems(s.ctx, second.ID, []int64{s.filmA, s.filmB}); err != nil {
		t.Fatal(err)
	}

	of, err := s.db.CollectionsOf(s.ctx, []int64{s.filmA, s.trackA})
	if err != nil {
		t.Fatal(err)
	}
	if len(of[s.filmA]) != 2 {
		t.Fatalf("the film is in %d collections", len(of[s.filmA]))
	}
	if len(of[s.trackA]) != 0 {
		t.Fatalf("the track is in %d collections", len(of[s.trackA]))
	}
}

func TestDeletingACollectionLeavesTheFilmsAlone(t *testing.T) {
	s := newShelf(t)
	made, err := s.db.CreateCollection(s.ctx, "Going", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetCollectionItems(s.ctx, made.ID, []int64{s.filmA, s.filmB}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.DeleteCollection(s.ctx, made.ID); err != nil {
		t.Fatal(err)
	}

	for _, id := range []int64{s.filmA, s.filmB} {
		if _, err := s.db.Item(s.ctx, id); err != nil {
			t.Fatalf("deleting a collection took a film with it: %v", err)
		}
	}
}

// Removing a film from the library takes it out of every arrangement of it,
// which the foreign keys do — see the pragmas for why that cascade is on.
func TestRemovingALibraryEmptiesWhatPointedAtIt(t *testing.T) {
	s := newShelf(t)
	made, err := s.db.CreateCollection(s.ctx, "Films", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetCollectionItems(s.ctx, made.ID, []int64{s.filmA, s.filmB}); err != nil {
		t.Fatal(err)
	}
	list, err := s.db.CreatePlaylist(s.ctx, s.user, "Films")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.AddToPlaylist(s.ctx, s.user, list.ID, []int64{s.filmA}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetFavourite(s.ctx, s.user, s.filmA, true); err != nil {
		t.Fatal(err)
	}

	if err := s.db.DeleteSection(s.ctx, s.films); err != nil {
		t.Fatal(err)
	}

	if _, err := s.db.Collection(s.ctx, made.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the collection outlived its only library: %v", err)
	}
	entries, _, err := s.db.PlaylistEntries(s.ctx, s.user, list.ID, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("playlist entries outlived their files: %v", ids(entries))
	}
	marked, _, err := s.db.Favourites(s.ctx, s.user, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(marked) != 0 {
		t.Fatalf("a favourite outlived its film: %v", marked)
	}
}

func ids(entries []domain.PlaylistEntry) []int64 {
	out := make([]int64, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.ID)
	}
	return out
}

// The two emptinesses, and why the browsing reader hides both. A collection
// whose library was deleted would otherwise linger in front of somebody
// trying to watch something, as a shelf with nothing on it.
func TestAnEmptyCollectionIsForTheAdministratorAndNobodyElse(t *testing.T) {
	s := newShelf(t)
	made, err := s.db.CreateCollection(s.ctx, "Not filled yet", "")
	if err != nil {
		t.Fatal(err)
	}

	// The interface that just made it can see it, which is the whole point:
	// you cannot fill a collection you are told does not exist.
	every, err := s.db.EveryCollection(s.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(every) != 1 {
		t.Fatalf("the administrator was shown %d collections", len(every))
	}
	if _, err := s.db.AnyCollection(s.ctx, made.ID); err != nil {
		t.Fatalf("the administrator could not read it: %v", err)
	}

	// A viewer is shown nothing, on the unrestricted handle as much as a
	// narrowed one: this is not a library rule, it is an empty shelf rule.
	browsing, err := s.db.Collections(s.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(browsing) != 0 {
		t.Fatalf("a viewer was shown an empty collection: %+v", browsing)
	}
	if _, err := s.db.Collection(s.ctx, made.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a viewer read an empty collection: %v", err)
	}

	// Filling it puts it in front of a viewer.
	if err := s.db.SetCollectionItems(s.ctx, made.ID, []int64{s.filmA}); err != nil {
		t.Fatal(err)
	}
	browsing, err = s.db.Collections(s.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(browsing) != 1 || browsing[0].Held != 1 {
		t.Fatalf("after filling it, a viewer sees %+v", browsing)
	}
}
