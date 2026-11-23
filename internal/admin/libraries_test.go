package admin_test

import (
	"context"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
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

// An account's libraries are set the same way a token's are, and listed
// beside it so the page can draw the boxes already ticked.
func TestAnAccountsLibrariesAreSetAndListed(t *testing.T) {
	h := newHarness(t)
	films, _ := twoLibraries(t, h)
	h.signIn(h.adminToken)
	ctx := context.Background()

	users, err := h.db.Users(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var viewer int64
	for _, user := range users {
		if user.Username == "kitchen-tv" {
			viewer = user.ID
		}
	}

	if res, body := h.call(http.MethodPatch, "/admin/api/users/"+strconv.FormatInt(viewer, 10),
		map[string]any{"sections": []int64{films}}); res.StatusCode != http.StatusNoContent {
		t.Fatalf("patch = %d: %v", res.StatusCode, body)
	}

	for _, row := range h.list("/admin/api/users") {
		if row["username"] != "kitchen-tv" {
			continue
		}
		granted, _ := row["sections"].([]any)
		if len(granted) != 1 || int64(granted[0].(float64)) != films {
			t.Fatalf("expected the films library, got %v", row["sections"])
		}
		return
	}
	t.Fatal("the account was not listed")
}

// Changing only the libraries leaves the role alone, and changing only the
// role leaves the libraries alone: one endpoint, two independent settings.
func TestRoleAndLibrariesAreChangedIndependently(t *testing.T) {
	h := newHarness(t)
	films, _ := twoLibraries(t, h)
	h.signIn(h.adminToken)
	ctx := context.Background()

	users, err := h.db.Users(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var viewer int64
	for _, user := range users {
		if user.Username == "kitchen-tv" {
			viewer = user.ID
		}
	}
	path := "/admin/api/users/" + strconv.FormatInt(viewer, 10)

	if res, _ := h.call(http.MethodPatch, path, map[string]any{"sections": []int64{films}}); res.StatusCode != http.StatusNoContent {
		t.Fatalf("setting libraries = %d", res.StatusCode)
	}
	if res, _ := h.call(http.MethodPatch, path, map[string]any{"admin": true}); res.StatusCode != http.StatusNoContent {
		t.Fatalf("setting the role = %d", res.StatusCode)
	}

	stored, err := h.db.UserSections(ctx, viewer)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("changing the role lost the libraries: %v", stored)
	}
	user, err := h.db.User(ctx, viewer)
	if err != nil {
		t.Fatal(err)
	}
	if !user.Admin {
		t.Fatal("the role did not change")
	}
}

// Collections are made in the administration interface, because they are the
// library's grouping rather than a viewer's.
func TestACollectionIsMadeFilledAndRead(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	ctx := context.Background()

	films, err := h.db.CreateSection(ctx, "Films", domain.SectionMovies, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	writer := h.db.NewWriter(4)
	for _, title := range []string{"Blue", "White", "Red"} {
		if err := writer.Add(ctx, store.Found{
			SectionID: films.ID, Kind: domain.KindMovie, Title: title, Year: 1994,
			Path: filepath.Join(t.TempDir(), title+".mkv"), Container: "mkv", Size: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	res, body := h.call(http.MethodPost, "/admin/api/collections",
		map[string]any{"title": "Three Colours", "summary": "A trilogy"})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d: %v", res.StatusCode, body)
	}
	id := int64(body["id"].(float64))

	// The search behind the picker finds the films and says which library
	// each came from.
	found := h.list("/admin/api/items?q=blue")
	if len(found) != 1 || found[0]["library"] != "Films" {
		t.Fatalf("search = %v", found)
	}

	all, _, err := h.db.SectionItems(ctx, films.ID, domain.KindMovie, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	ordered := []int64{}
	for _, item := range all {
		ordered = append(ordered, item.ID)
	}
	path := "/admin/api/collections/" + strconv.FormatInt(id, 10)
	if res, body := h.call(http.MethodPatch, path, map[string]any{"items": ordered}); res.StatusCode != http.StatusNoContent {
		t.Fatalf("set items = %d: %v", res.StatusCode, body)
	}

	_, read := h.call(http.MethodGet, path, nil)
	if int(read["held"].(float64)) != 3 {
		t.Fatalf("held = %v", read["held"])
	}
	items, _ := read["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("the editor was given %d items", len(items))
	}

	listed := h.list("/admin/api/collections")
	if len(listed) != 1 || listed[0]["title"] != "Three Colours" {
		t.Fatalf("collections = %v", listed)
	}
}

// Deleting the grouping must not delete the films, which is the one mistake
// that would be unrecoverable.
func TestDeletingACollectionKeepsTheFilms(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	ctx := context.Background()

	films, err := h.db.CreateSection(ctx, "Films", domain.SectionMovies, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	writer := h.db.NewWriter(2)
	if err := writer.Add(ctx, store.Found{
		SectionID: films.ID, Kind: domain.KindMovie, Title: "Blue", Year: 1993,
		Path: filepath.Join(t.TempDir(), "Blue.mkv"), Container: "mkv", Size: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	all, _, err := h.db.SectionItems(ctx, films.ID, domain.KindMovie, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}

	made, err := h.db.CreateCollection(ctx, "Going", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetCollectionItems(ctx, made.ID, []int64{all[0].ID}); err != nil {
		t.Fatal(err)
	}

	res, _ := h.call(http.MethodDelete, "/admin/api/collections/"+strconv.FormatInt(made.ID, 10), nil)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d", res.StatusCode)
	}
	if _, err := h.db.Item(ctx, all[0].ID); err != nil {
		t.Fatalf("the film went with the collection: %v", err)
	}
}

func TestALibrarysSettingsAreSavedAndReportedBack(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	section := films(t, h)
	key := "/admin/api/sections/" + strconv.FormatInt(section, 10)

	// A fresh library reports the defaults rather than nothing, so the page
	// can draw its controls in the state they are in.
	listed := h.list("/admin/api/sections")
	first := listed[0]
	if first["settings"] == nil {
		t.Fatal("a library reported no settings at all")
	}
	if first["nextScan"] != nil {
		t.Fatalf("a library with no schedule reported a next scan: %v", first["nextScan"])
	}

	res, body := h.call(http.MethodPatch, key, map[string]any{
		"title": "Films",
		"roots": first["roots"],
		"settings": map[string]any{
			"scanEvery": 360,
			"enrich":    false,
		},
	})
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("save = %d: %v", res.StatusCode, body)
	}

	listed = h.list("/admin/api/sections")
	after := listed[0]
	settings, _ := after["settings"].(map[string]any)
	if int(settings["scanEvery"].(float64)) != 360 {
		t.Fatalf("scanEvery = %v", settings["scanEvery"])
	}
	if settings["enrich"] != false {
		t.Fatalf("enrich = %v", settings["enrich"])
	}
	// Now it has a schedule, so the listing says when it is next due — "every
	// 6 hours" being only half of what somebody wants to know.
	if after["nextScan"] == nil {
		t.Fatal("a library with a schedule reported no next scan")
	}
}

func TestAnIntervalTheServerCannotHonourIsRefusedWithItsReason(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	section := films(t, h)
	key := "/admin/api/sections/" + strconv.FormatInt(section, 10)

	res, body := h.call(http.MethodPatch, key, map[string]any{
		"title":    "Films",
		"roots":    []string{t.TempDir()},
		"settings": map[string]any{"scanEvery": 2},
	})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("two minutes = %d: %v", res.StatusCode, body)
	}
	// The message says what the floor is, rather than the number quietly
	// becoming fifteen.
	if message, _ := body["error"].(string); !strings.Contains(message, "15") {
		t.Fatalf("error = %q", message)
	}
}

func TestSettingsLeftOutOfAPatchAreLeftAlone(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	section := films(t, h)
	key := "/admin/api/sections/" + strconv.FormatInt(section, 10)
	root := t.TempDir()

	if res, body := h.call(http.MethodPatch, key, map[string]any{
		"title": "Films", "roots": []string{root},
		"settings": map[string]any{"scanEvery": 60},
	}); res.StatusCode != http.StatusNoContent {
		t.Fatalf("save = %d: %v", res.StatusCode, body)
	}

	// A rename says nothing about settings, so it must not clear them: a
	// patch that could not say "leave alone" would make the page resend
	// every setting on every rename.
	if res, body := h.call(http.MethodPatch, key, map[string]any{
		"title": "The Films", "roots": []string{root},
	}); res.StatusCode != http.StatusNoContent {
		t.Fatalf("rename = %d: %v", res.StatusCode, body)
	}
	listed := h.list("/admin/api/sections")
	after := listed[0]
	settings, _ := after["settings"].(map[string]any)
	if settings == nil || int(settings["scanEvery"].(float64)) != 60 {
		t.Fatalf("a rename changed the settings: %v", after["settings"])
	}

	// And an explicit null puts them back, which is how somebody turns a
	// library's own arrangements off without knowing what every field was.
	if res, body := h.call(http.MethodPatch, key, map[string]any{
		"title": "The Films", "roots": []string{root}, "settings": nil,
	}); res.StatusCode != http.StatusNoContent {
		t.Fatalf("clear = %d: %v", res.StatusCode, body)
	}
	listed = h.list("/admin/api/sections")
	cleared := listed[0]
	clearedSettings, _ := cleared["settings"].(map[string]any)
	if len(clearedSettings) != 0 {
		t.Fatalf("settings after clearing = %v", cleared["settings"])
	}
}

func TestThePassSchedulesAreSavedAndRefused(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)

	res, before := h.call(http.MethodGet, "/admin/api/settings", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("settings = %d", res.StatusCode)
	}
	// Never, which is what every server did until these existed.
	if int(before["enrichEvery"].(float64)) != 0 || int(before["markersEvery"].(float64)) != 0 {
		t.Fatalf("intervals = %v / %v", before["enrichEvery"], before["markersEvery"])
	}

	res, body := h.call(http.MethodPatch, "/admin/api/settings",
		map[string]any{"enrichEvery": 360, "markersEvery": 1440})
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("save = %d: %v", res.StatusCode, body)
	}
	res, after := h.call(http.MethodGet, "/admin/api/settings", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("settings = %d", res.StatusCode)
	}
	if int(after["enrichEvery"].(float64)) != 360 || int(after["markersEvery"].(float64)) != 1440 {
		t.Fatalf("intervals = %v / %v", after["enrichEvery"], after["markersEvery"])
	}

	// The same floor a library's scan has, refused with its reason rather
	// than clamped.
	for _, bad := range []map[string]any{
		{"enrichEvery": 5},
		{"markersEvery": -1},
		{"enrichEvery": 8 * 24 * 60},
	} {
		res, answer := h.call(http.MethodPatch, "/admin/api/settings", bad)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("%v = %d: %v", bad, res.StatusCode, answer)
		}
	}

	// And zero is never, which is how somebody turns one off.
	if res, body := h.call(http.MethodPatch, "/admin/api/settings",
		map[string]any{"enrichEvery": 0}); res.StatusCode != http.StatusNoContent {
		t.Fatalf("clear = %d: %v", res.StatusCode, body)
	}
	res, cleared := h.call(http.MethodGet, "/admin/api/settings", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("settings = %d", res.StatusCode)
	}
	if int(cleared["enrichEvery"].(float64)) != 0 {
		t.Fatalf("enrichEvery = %v after clearing", cleared["enrichEvery"])
	}
}
