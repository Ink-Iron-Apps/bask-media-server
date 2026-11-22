package web_test

import (
	"net/http"
	"strconv"
	"testing"
)

// The watchlist from the interface: a button, a list, and a flag on every row
// so the button is drawn in the state it is in.
func TestTheWatchlistIsPressedAndRead(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	film := strconv.FormatInt(h.filmID, 10)

	_, empty := h.call(http.MethodGet, "/web/api/watchlist", nil)
	if list := items(t, empty, "items"); len(list) != 0 {
		t.Fatalf("a new account's watchlist = %v", list)
	}

	res, body := h.call(http.MethodPost, "/web/api/watchlist/"+film, nil)
	if res.StatusCode != http.StatusOK || body["watchlist"] != true {
		t.Fatalf("add = %d: %v", res.StatusCode, body)
	}

	_, listed := h.call(http.MethodGet, "/web/api/watchlist", nil)
	list := items(t, listed, "items")
	if len(list) != 1 || list[0]["title"] != "Dinotopia" {
		t.Fatalf("watchlist = %v", list)
	}

	// The flag is on the item page, which is what draws the button pressed.
	_, detail := h.call(http.MethodGet, "/web/api/items/"+film, nil)
	item, _ := detail["item"].(map[string]any)
	if item["watchlist"] != true {
		t.Fatalf("the item does not say it is on the watchlist: %v", item)
	}

	// And it appears as a playlist, because that is what it is: every
	// surface lists playlists, so this is on the television too.
	_, lists := h.call(http.MethodGet, "/web/api/playlists", nil)
	playlists, _ := lists["playlists"].([]any)
	if len(playlists) != 1 {
		t.Fatalf("playlists = %v", playlists)
	}
	if first, _ := playlists[0].(map[string]any); first["title"] != "Watchlist" {
		t.Fatalf("the playlist is called %v", playlists[0])
	}

	res, body = h.call(http.MethodDelete, "/web/api/watchlist/"+film, nil)
	if res.StatusCode != http.StatusOK || body["watchlist"] != false {
		t.Fatalf("remove = %d: %v", res.StatusCode, body)
	}
	if _, after := h.call(http.MethodGet, "/web/api/watchlist", nil); len(items(t, after, "items")) != 0 {
		t.Fatal("still on the list")
	}
}

// An item this account cannot see cannot be put on a list by naming it.
func TestAnItemBehindARestrictionCannotBeWatchlisted(t *testing.T) {
	h := newHarness(t)
	restrict(t, h, 12, true)
	h.signIn()

	res, _ := h.call(http.MethodPost, "/web/api/watchlist/"+strconv.FormatInt(h.filmID, 10), nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("adding a film above the ceiling = %d, want 404", res.StatusCode)
	}
}
