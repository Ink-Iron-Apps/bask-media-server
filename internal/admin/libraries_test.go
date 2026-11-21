package admin_test

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// twoLibraries gives the fixture something to tick, and returns their ids in
// the order the interface lists them.
func twoLibraries(t *testing.T, h *harness) (films, shows int64) {
	t.Helper()
	ctx := context.Background()

	film, err := h.db.CreateSection(ctx, "Films", domain.SectionMovies, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	show, err := h.db.CreateSection(ctx, "TV Shows", domain.SectionShows, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return film.ID, show.ID
}

// viewerToken is the credential the fixture gave the kitchen television.
func viewerTokenID(t *testing.T, h *harness) int64 {
	t.Helper()
	ctx := context.Background()

	users, err := h.db.Users(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range users {
		if user.Username != "kitchen-tv" {
			continue
		}
		tokens, err := h.db.Tokens(ctx, user.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(tokens) != 1 {
			t.Fatalf("expected one token for the television, got %d", len(tokens))
		}
		return tokens[0].ID
	}
	t.Fatal("no kitchen television in the fixture")
	return 0
}

// A token's libraries are listed beside it, so the page can draw the boxes
// already ticked.
func TestATokensLibrariesAreListedWithIt(t *testing.T) {
	h := newHarness(t)
	films, _ := twoLibraries(t, h)
	h.signIn(h.adminToken)

	token := viewerTokenID(t, h)
	if res, body := h.call(http.MethodPatch, "/admin/api/tokens/"+strconv.FormatInt(token, 10),
		map[string]any{"sections": []int64{films}}); res.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d: %v", res.StatusCode, body)
	}

	users := h.list("/admin/api/users")
	var viewer string
	for _, user := range users {
		if user["username"] == "kitchen-tv" {
			viewer = strconv.FormatInt(int64(user["id"].(float64)), 10)
		}
	}
	tokens := h.list("/admin/api/users/" + viewer + "/tokens")
	if len(tokens) != 1 {
		t.Fatalf("expected one token, got %d", len(tokens))
	}
	granted, _ := tokens[0]["sections"].([]any)
	if len(granted) != 1 || int64(granted[0].(float64)) != films {
		t.Fatalf("expected the films library, got %v", tokens[0]["sections"])
	}
}

// Ticking every box is stored as no restriction at all, so a library added
// next month is visible to that device instead of quietly missing from it.
func TestTickingEveryLibraryStoresNoRestriction(t *testing.T) {
	h := newHarness(t)
	films, shows := twoLibraries(t, h)
	h.signIn(h.adminToken)
	ctx := context.Background()

	token := viewerTokenID(t, h)
	if res, body := h.call(http.MethodPatch, "/admin/api/tokens/"+strconv.FormatInt(token, 10),
		map[string]any{"sections": []int64{films, shows}}); res.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d: %v", res.StatusCode, body)
	}

	stored, err := h.db.TokenSections(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Fatalf("expected no rows for an unrestricted token, got %v", stored)
	}

	// And a library added afterwards is reachable, which is the point.
	third, err := h.db.CreateSection(ctx, "Music", domain.SectionMusic, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cred, err := h.db.Credential(ctx, h.viewerToken)
	if err != nil {
		t.Fatal(err)
	}
	sections, err := h.db.Scoped(cred).Sections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, section := range sections {
		if section.ID == third.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("a library added after the grant was hidden from an unrestricted token")
	}
}

// A library id that does not exist is refused rather than stored, because a
// stale row would silently shrink what a device can see.
func TestAnUnknownLibraryIsRefused(t *testing.T) {
	h := newHarness(t)
	twoLibraries(t, h)
	h.signIn(h.adminToken)

	token := viewerTokenID(t, h)
	res, _ := h.call(http.MethodPatch, "/admin/api/tokens/"+strconv.FormatInt(token, 10),
		map[string]any{"sections": []int64{9999}})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for an unknown library, got %d", res.StatusCode)
	}
}

// And a token that does not exist is a 404, not a foreign key error.
func TestGrantingToAnUnknownTokenIsNotFound(t *testing.T) {
	h := newHarness(t)
	films, _ := twoLibraries(t, h)
	h.signIn(h.adminToken)

	res, _ := h.call(http.MethodPatch, "/admin/api/tokens/4242",
		map[string]any{"sections": []int64{films}})
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for an unknown token, got %d", res.StatusCode)
	}
}

// Revoking a token takes its grants with it, so a reissued id cannot inherit
// somebody else's restriction.
func TestRevokingATokenRemovesItsGrants(t *testing.T) {
	h := newHarness(t)
	films, _ := twoLibraries(t, h)
	h.signIn(h.adminToken)
	ctx := context.Background()

	token := viewerTokenID(t, h)
	if err := h.db.SetTokenSections(ctx, token, []int64{films}); err != nil {
		t.Fatal(err)
	}
	if res, _ := h.call(http.MethodDelete, "/admin/api/tokens/"+strconv.FormatInt(token, 10), nil); res.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke = %d", res.StatusCode)
	}

	stored, err := h.db.TokenSections(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Fatalf("grants outlived the token: %v", stored)
	}
}
