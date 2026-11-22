package admin_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/store"
)

// watch writes one finished watch the way the recorder writes one.
func watch(t *testing.T, h *harness, in store.NewWatch, open time.Duration, paused time.Duration) int64 {
	t.Helper()
	id, err := h.db.StartWatch(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.db.TouchWatch(t.Context(), id, store.WatchProgress{
		At: in.Started.Add(open), Paused: paused, Position: open - paused, Done: true,
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// The page: rows newest first, with the totals for the window being read.
func TestTheHistoryPageShowsWhatWasWatched(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	now := time.Now().UTC()

	watch(t, h, store.NewWatch{
		UserID: h.adminID, Name: "admin", Title: "Big Hill",
		Surface: "jellyfin", Device: "Living Room TV", Mode: "direct",
		Started: now.Add(-3 * time.Hour), Length: 95 * time.Minute,
	}, time.Hour, 10*time.Minute)
	watch(t, h, store.NewWatch{
		UserID: h.adminID, Name: "admin", Title: "Heat",
		Surface: "plex", Device: "Phone", Mode: "transcode",
		Started: now.Add(-time.Hour), Length: 170 * time.Minute,
	}, 30*time.Minute, 0)

	_, page := h.call(http.MethodGet, "/admin/api/history", nil)
	if page["total"] != float64(2) {
		t.Fatalf("expected two watches, got %v", page["total"])
	}
	watches, _ := page["watches"].([]any)
	if len(watches) != 2 {
		t.Fatalf("expected two rows, got %v", watches)
	}

	// Newest first: the log is only ever read that way.
	first := watches[0].(map[string]any)
	if first["title"] != "Heat" {
		t.Errorf("the newest row is %v", first["title"])
	}
	if first["watched"] != float64(1800) {
		t.Errorf("watched = %v seconds, want 1800", first["watched"])
	}

	second := watches[1].(map[string]any)
	if second["watched"] != float64(3000) || second["paused"] != float64(600) {
		t.Errorf("an hour with ten minutes paused came back as %v watched, %v paused",
			second["watched"], second["paused"])
	}
	if second["device"] != "Living Room TV" || second["mode"] != "direct" {
		t.Errorf("the row lost what it was watched on: %+v", second)
	}
	if second["complete"] != true {
		t.Errorf("the server saw this stop: %+v", second)
	}

	// The totals, which are the two questions a page of rows cannot answer.
	viewers, _ := page["viewers"].([]any)
	if len(viewers) != 1 {
		t.Fatalf("one viewer, got %v", viewers)
	}
	if viewer := viewers[0].(map[string]any); viewer["plays"] != float64(2) || viewer["watched"] != float64(4800) {
		t.Errorf("the viewer's total is %+v", viewer)
	}
	titles, _ := page["titles"].([]any)
	if len(titles) != 2 {
		t.Fatalf("two titles, got %v", titles)
	}
	if top := titles[0].(map[string]any); top["name"] != "Big Hill" {
		t.Errorf("the most watched title is %v", top["name"])
	}
}

// A window is a question about a period, and page two of it is not a
// different answer.
func TestTheHistoryWindowNarrowsBothRowsAndTotals(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	now := time.Now().UTC()

	watch(t, h, store.NewWatch{UserID: h.adminID, Name: "admin", Title: "Recent", Started: now.Add(-time.Hour)}, time.Hour, 0)
	watch(t, h, store.NewWatch{UserID: h.adminID, Name: "admin", Title: "Ancient", Started: now.AddDate(0, 0, -200)}, time.Hour, 0)

	_, everything := h.call(http.MethodGet, "/admin/api/history", nil)
	if everything["total"] != float64(2) {
		t.Fatalf("without a window the log is %v rows", everything["total"])
	}

	_, week := h.call(http.MethodGet, "/admin/api/history?days=7", nil)
	if week["total"] != float64(1) {
		t.Fatalf("last week is %v rows, want 1", week["total"])
	}
	titles, _ := week["titles"].([]any)
	if len(titles) != 1 || titles[0].(map[string]any)["name"] != "Recent" {
		t.Errorf("the totals should be narrowed too: %v", titles)
	}
	if week["days"] != float64(7) {
		t.Errorf("the page should know its window: %v", week["days"])
	}
}

func TestTheHistoryPageIsPagedAndFilterable(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	now := time.Now().UTC()

	for i := range 3 {
		watch(t, h, store.NewWatch{
			UserID: h.adminID, Name: "admin", Title: "Big Hill",
			Started: now.Add(-time.Duration(i+1) * time.Hour),
		}, 10*time.Minute, 0)
	}

	_, page := h.call(http.MethodGet, "/admin/api/history?start=2", nil)
	watches, _ := page["watches"].([]any)
	if page["total"] != float64(3) || len(watches) != 1 {
		t.Fatalf("the third row alone: %v of %v", len(watches), page["total"])
	}
	if page["start"] != float64(2) {
		t.Errorf("the page should know where it is: %v", page["start"])
	}

	// A filter nobody matches is an empty log, not a failure.
	_, none := h.call(http.MethodGet, "/admin/api/history?user=9999", nil)
	if none["total"] != float64(0) {
		t.Errorf("an unknown viewer has %v watches", none["total"])
	}

	// A number that is not a number narrows nothing rather than refusing:
	// these are all narrowings of a list.
	res, all := h.call(http.MethodGet, "/admin/api/history?user=banana", nil)
	if res.StatusCode != http.StatusOK || all["total"] != float64(3) {
		t.Errorf("a bad filter answered %s with %v rows", res.Status, all["total"])
	}
}

// Getting rid of it is a feature. Watched state and playlists are not part
// of the log and must survive forgetting it.
func TestForgettingTheLogLeavesTheLibraryAlone(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	now := time.Now().UTC()

	watch(t, h, store.NewWatch{UserID: h.adminID, Name: "admin", Title: "Recent", Started: now.Add(-time.Hour)}, time.Hour, 0)
	watch(t, h, store.NewWatch{UserID: h.adminID, Name: "admin", Title: "Ancient", Started: now.AddDate(0, 0, -200)}, time.Hour, 0)

	_, trimmed := h.call(http.MethodDelete, "/admin/api/history?olderThan=90", nil)
	if trimmed["removed"] != float64(1) {
		t.Fatalf("one old row should have gone, %v did", trimmed["removed"])
	}
	_, left := h.call(http.MethodGet, "/admin/api/history", nil)
	if left["total"] != float64(1) {
		t.Fatalf("%v rows are left, want 1", left["total"])
	}

	_, cleared := h.call(http.MethodDelete, "/admin/api/history", nil)
	if cleared["removed"] != float64(1) {
		t.Fatalf("the rest should have gone, %v did", cleared["removed"])
	}
	_, empty := h.call(http.MethodGet, "/admin/api/history", nil)
	if empty["total"] != float64(0) {
		t.Errorf("%v rows survived forgetting everything", empty["total"])
	}

	// The account is still there, which is the point of the sentence on the
	// button: forgetting the log is not forgetting the household.
	if users := h.list("/admin/api/users"); len(users) == 0 {
		t.Error("forgetting the log removed the accounts")
	}
}

// The log is the most personal thing this server holds: what somebody
// watched three Tuesdays ago is not something a housemate could have seen by
// walking into the room.
func TestOnlyAnAdministratorReadsTheLog(t *testing.T) {
	h := newHarness(t)
	if res := h.signIn(h.viewerToken); res.StatusCode != http.StatusForbidden {
		t.Fatalf("a viewer signed in: %s", res.Status)
	}

	res, _ := h.call(http.MethodGet, "/admin/api/history", nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("reading the log without signing in answered %s", res.Status)
	}
	res, _ = h.call(http.MethodDelete, "/admin/api/history", nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("forgetting the log without signing in answered %s", res.Status)
	}
}
