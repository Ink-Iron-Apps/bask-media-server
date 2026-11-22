package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// A restriction set in the interface is what a media client then sees, which
// is the only test of this that proves anything: the page writes a row and
// the Plex surface reads the library through it.
func TestARatingCeilingSetHereIsWhatAClientSees(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	ctx := context.Background()
	item, _ := playable(t, h, "Big Hill")
	if err := h.db.SetRating(ctx, item, "18"); err != nil {
		t.Fatal(err)
	}

	// The viewer account the harness holds a token for sees it to begin
	// with.
	if !h.plexSees(t, "Big Hill") {
		t.Fatal("the film was not visible before anything was restricted")
	}

	// Find that account and give it a ceiling.
	viewer := h.accountNamed(t, "kitchen-tv")
	res, body := h.call(http.MethodPatch, "/admin/api/users/"+strconv.FormatInt(viewer, 10),
		map[string]any{"ceiling": 12, "allowUnrated": true})
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("set limits = %d: %v", res.StatusCode, body)
	}

	if h.plexSees(t, "Big Hill") {
		t.Fatal("an 18 film is still visible to a twelve-year-old's token")
	}

	// And the page reads back what it set, so somebody can see the state
	// they put the account in.
	for _, account := range h.list("/admin/api/users") {
		if account["username"] != "kitchen-tv" {
			continue
		}
		if int(account["ceiling"].(float64)) != 12 {
			t.Fatalf("ceiling = %v", account["ceiling"])
		}
		if account["allowUnrated"] != true {
			t.Fatalf("allowUnrated = %v", account["allowUnrated"])
		}
	}
}

// The caps and the PIN go through the same route, and one of them being set
// does not clear the others.
func TestTheLimitsAreSetOneAtATimeWithoutClearingTheRest(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	viewer := h.accountNamed(t, "kitchen-tv")
	path := "/admin/api/users/" + strconv.FormatInt(viewer, 10)

	h.call(http.MethodPatch, path, map[string]any{"maxStreams": 2})
	h.call(http.MethodPatch, path, map[string]any{"maxBitRate": 4000000})
	h.call(http.MethodPatch, path, map[string]any{"ceiling": 15})
	if res, body := h.call(http.MethodPatch, path, map[string]any{"pin": "4812"}); res.StatusCode != http.StatusNoContent {
		t.Fatalf("set pin = %d: %v", res.StatusCode, body)
	}

	limits, err := h.db.UserLimits(context.Background(), viewer)
	if err != nil {
		t.Fatal(err)
	}
	if limits.MaxStreams != 2 || limits.MaxBitRate != 4000000 || limits.Ceiling != 15 || !limits.HasPIN {
		t.Fatalf("limits = %+v", limits)
	}

	// A PIN too short to be one is refused, and the one that was set stays.
	res, body := h.call(http.MethodPatch, path, map[string]any{"pin": "1"})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a one-digit PIN = %d, want 400", res.StatusCode)
	}
	if message, _ := body["error"].(string); !strings.Contains(message, "four digits") {
		t.Fatalf("error = %q", message)
	}
	if ok, err := h.db.CheckUserPIN(context.Background(), viewer, "4812"); err != nil || !ok {
		t.Fatalf("the PIN was changed by a refused request (%v)", err)
	}
}

// plexSees reports whether the viewer's token finds a film by name on the
// media surface.
func (h *harness) plexSees(t *testing.T, title string) bool {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.server.URL+"/search?query="+url.QueryEscape(title), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Plex-Token", h.viewerToken)
	req.Header.Set("Accept", "application/json")
	res, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("search = %d", res.StatusCode)
	}
	var body struct {
		MediaContainer struct {
			Metadata []struct {
				Title string `json:"title"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	for _, row := range body.MediaContainer.Metadata {
		if row.Title == title {
			return true
		}
	}
	return false
}

// accountNamed is the id of the account with this name.
func (h *harness) accountNamed(t *testing.T, name string) int64 {
	t.Helper()
	users, err := h.db.Users(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range users {
		if user.Username == name {
			return user.ID
		}
	}
	t.Fatalf("no account called %q", name)
	return 0
}
