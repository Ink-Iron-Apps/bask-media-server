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
