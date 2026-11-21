package web_test

import (
	"context"
	"net/http"
	"strconv"
	"testing"
)

// The web interface signs in with a token, so it inherits that token's
// restriction. Otherwise handing a television a narrowed token and then
// opening a browser on the same television would walk straight around it.
func TestABrowserSessionInheritsItsTokensLibraries(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	sections, err := h.db.Sections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 2 {
		t.Fatalf("expected two libraries in the fixture, got %d", len(sections))
	}
	var shows int64
	for _, section := range sections {
		if section.Kind == "shows" {
			shows = section.ID
		}
	}
	if shows == 0 {
		t.Fatal("no shows library in the fixture")
	}

	tokens, err := h.db.Tokens(ctx, h.userID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetTokenSections(ctx, tokens[0].ID, []int64{shows}); err != nil {
		t.Fatal(err)
	}

	h.signIn()

	res, body := h.call(http.MethodGet, "/web/api/home", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("home = %d: %v", res.StatusCode, body)
	}
	list := items(t, body, "sections")
	if len(list) != 1 {
		t.Fatalf("expected one library, got %d: %v", len(list), list)
	}
	if list[0]["kind"] != "shows" {
		t.Fatalf("expected the shows library, got %v", list[0])
	}

	// The film is not merely missing from the list: nothing about it answers.
	for _, path := range []string{
		"/web/api/items/" + strconv.FormatInt(h.filmID, 10),
		"/web/image/" + strconv.FormatInt(h.filmID, 10),
		"/web/stream/" + strconv.FormatInt(h.filmPart, 10),
	} {
		res, _ := h.call(http.MethodGet, path, nil)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: want 404, got %d", path, res.StatusCode)
		}
	}
}

// A session opened before the restriction existed is narrowed too: the grants
// are read when the session is used, not copied when it was created.
func TestNarrowingATokenNarrowsSessionsAlreadyOpen(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.signIn()
	if res, body := h.call(http.MethodGet, "/web/api/home", nil); len(items(t, body, "sections")) != 2 {
		t.Fatalf("expected two libraries first: %d %v", res.StatusCode, body)
	}

	sections, err := h.db.Sections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var movies int64
	for _, section := range sections {
		if section.Kind == "movies" {
			movies = section.ID
		}
	}
	tokens, err := h.db.Tokens(ctx, h.userID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetTokenSections(ctx, tokens[0].ID, []int64{movies}); err != nil {
		t.Fatal(err)
	}

	_, body := h.call(http.MethodGet, "/web/api/home", nil)
	list := items(t, body, "sections")
	if len(list) != 1 || list[0]["kind"] != "movies" {
		t.Fatalf("the open session was not narrowed: %v", list)
	}
}

// The star, a playlist and a collection, through the interface people watch
// in rather than through the store.
func TestTheWebInterfaceStarsAndLists(t *testing.T) {
	h := newHarness(t)
	h.signIn()

	film := strconv.FormatInt(h.filmID, 10)

	// The star sticks and comes back on the item.
	if res, body := h.call(http.MethodPost, "/web/api/favourites/"+film, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("star = %d: %v", res.StatusCode, body)
	}
	_, detail := h.call(http.MethodGet, "/web/api/items/"+film, nil)
	item, _ := detail["item"].(map[string]any)
	if item["favourite"] != true {
		t.Fatalf("the star did not stick: %v", item)
	}
	_, shelf := h.call(http.MethodGet, "/web/api/favourites", nil)
	if len(items(t, shelf, "items")) != 1 {
		t.Fatalf("favourites shelf = %v", shelf)
	}

	// Unpressing it empties the shelf.
	if res, _ := h.call(http.MethodDelete, "/web/api/favourites/"+film, nil); res.StatusCode != http.StatusOK {
		t.Fatal("could not unstar")
	}
	_, empty := h.call(http.MethodGet, "/web/api/favourites", nil)
	if len(items(t, empty, "items")) != 0 {
		t.Fatalf("still starred: %v", empty)
	}

	// A playlist, with its line carrying its own id so it can be removed.
	res, made := h.call(http.MethodPost, "/web/api/playlists",
		map[string]any{"title": "Tonight", "items": []int64{h.filmID}})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create playlist = %d: %v", res.StatusCode, made)
	}
	id := strconv.FormatInt(int64(made["id"].(float64)), 10)

	_, read := h.call(http.MethodGet, "/web/api/playlists/"+id, nil)
	lines, _ := read["lines"].([]any)
	if len(lines) != 1 {
		t.Fatalf("playlist lines = %v", read)
	}
	line, _ := lines[0].(map[string]any)
	entry := int64(line["entry"].(float64))
	if entry == 0 {
		t.Fatalf("no entry id on %v", line)
	}

	if res, _ := h.call(http.MethodDelete,
		"/web/api/playlists/"+id+"/items/"+strconv.FormatInt(entry, 10), nil); res.StatusCode != http.StatusNoContent {
		t.Fatalf("remove = %d", res.StatusCode)
	}
	_, after := h.call(http.MethodGet, "/web/api/playlists/"+id, nil)
	if remaining, _ := after["lines"].([]any); len(remaining) != 0 {
		t.Fatalf("after removing the line: %v", after)
	}
}

// A collection reaches the viewing interface, and an empty one does not.
func TestTheWebInterfaceShowsCollections(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	ctx := context.Background()

	made, err := h.db.CreateCollection(ctx, "An evening in", "")
	if err != nil {
		t.Fatal(err)
	}

	// Empty: a viewer is not shown a shelf with nothing on it.
	_, none := h.call(http.MethodGet, "/web/api/collections", nil)
	if len(items(t, none, "collections")) != 0 {
		t.Fatalf("an empty collection reached a viewer: %v", none)
	}

	if err := h.db.SetCollectionItems(ctx, made.ID, []int64{h.filmID}); err != nil {
		t.Fatal(err)
	}
	_, listed := h.call(http.MethodGet, "/web/api/collections", nil)
	all := items(t, listed, "collections")
	if len(all) != 1 || all[0]["title"] != "An evening in" {
		t.Fatalf("collections = %v", listed)
	}

	_, inside := h.call(http.MethodGet,
		"/web/api/collections/"+strconv.FormatInt(made.ID, 10), nil)
	if len(items(t, inside, "items")) != 1 {
		t.Fatalf("inside the collection: %v", inside)
	}
}
