package store_test

import (
	"testing"

	"github.com/inknironapps/bask-media-server/internal/store"
)

// The watchlist is a playlist with a reserved name, which is the whole point:
// every surface already lists playlists, so this is on the television without
// a line of protocol being added to it.
func TestTheWatchlistIsAPlaylistEverySurfaceAlreadyShows(t *testing.T) {
	h := newHousehold(t)
	film := h.films["Paddington"]

	// Nothing is made by asking what is on it, so an account that never uses
	// it has no empty list cluttering its playlists.
	if on, err := h.db.OnWatchlist(h.ctx, h.user, film); err != nil || on {
		t.Fatalf("on = %v (%v)", on, err)
	}
	if lists, err := h.db.Playlists(h.ctx, h.user); err != nil || len(lists) != 0 {
		t.Fatalf("playlists = %v (%v)", lists, err)
	}

	added, err := h.db.AddToWatchlist(h.ctx, h.user, film)
	if err != nil {
		t.Fatal(err)
	}
	if !added {
		t.Fatal("nothing was added")
	}

	lists, err := h.db.Playlists(h.ctx, h.user)
	if err != nil {
		t.Fatal(err)
	}
	if len(lists) != 1 || lists[0].Title != store.WatchlistTitle || lists[0].Entries != 1 {
		t.Fatalf("playlists = %+v", lists)
	}
	if on, err := h.db.OnWatchlist(h.ctx, h.user, film); err != nil || !on {
		t.Fatalf("on = %v (%v)", on, err)
	}

	// Pressing the button twice is the same wish, not an error and not two
	// lines on the list.
	if added, err := h.db.AddToWatchlist(h.ctx, h.user, film); err != nil || added {
		t.Fatalf("added twice = %v (%v)", added, err)
	}
	if lists, _ := h.db.Playlists(h.ctx, h.user); lists[0].Entries != 1 {
		t.Fatalf("the film is on the list %d times", lists[0].Entries)
	}

	// Removed by the item rather than by the line it is on, because a button
	// that says "remove this film" has a film.
	if removed, err := h.db.RemoveFromWatchlist(h.ctx, h.user, film); err != nil || !removed {
		t.Fatalf("removed = %v (%v)", removed, err)
	}
	if on, _ := h.db.OnWatchlist(h.ctx, h.user, film); on {
		t.Fatal("still on the list")
	}
	// And removing what is not on it is not an error.
	if removed, err := h.db.RemoveFromWatchlist(h.ctx, h.user, film); err != nil || removed {
		t.Fatalf("removed again = %v (%v)", removed, err)
	}
}

// One query for a page of posters, because a grid draws a button per row.
func TestWhatIsOnTheWatchlistIsReadForAWholePage(t *testing.T) {
	h := newHousehold(t)
	wanted := []int64{h.films["Paddington"], h.films["Heat"]}
	for _, id := range wanted {
		if _, err := h.db.AddToWatchlist(h.ctx, h.user, id); err != nil {
			t.Fatal(err)
		}
	}

	everything := []int64{}
	for _, id := range h.films {
		everything = append(everything, id)
	}
	set, err := h.db.WatchlistSet(h.ctx, h.user, everything)
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != 2 || !set[wanted[0]] || !set[wanted[1]] {
		t.Fatalf("set = %v", set)
	}

	// An account with no watchlist gets an empty answer rather than one
	// being made for it.
	other, err := h.db.CreateUser(h.ctx, "somebody-else", false)
	if err != nil {
		t.Fatal(err)
	}
	if set, err := h.db.WatchlistSet(h.ctx, other.ID, everything); err != nil || len(set) != 0 {
		t.Fatalf("another account's set = %v (%v)", set, err)
	}
	if lists, _ := h.db.Playlists(h.ctx, other.ID); len(lists) != 0 {
		t.Fatalf("asking made %d playlists", len(lists))
	}
}
