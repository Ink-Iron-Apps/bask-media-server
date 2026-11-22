package web_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/nowplaying"
	"github.com/inknironapps/bask-media-server/internal/store"
	"github.com/inknironapps/bask-media-server/internal/web"
)

// restrict gives the harness's account a rating ceiling and rates the film
// above it.
func restrict(t *testing.T, h *harness, ceiling int, allowUnrated bool) {
	t.Helper()
	ctx := context.Background()
	if err := h.db.SetRating(ctx, h.filmID, "18"); err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetUserLimits(ctx, h.userID, store.Limits{
		Ceiling: ceiling, AllowUnrated: allowUnrated,
	}); err != nil {
		t.Fatal(err)
	}
}

// A restricted account does not see what it is restricted from — anywhere.
// The filter is on the handle every query runs through, so this is about the
// wiring rather than about the predicate.
func TestARestrictedAccountDoesNotSeeTheFilm(t *testing.T) {
	h := newHarness(t)
	restrict(t, h, 12, true)
	h.signIn()

	_, home := h.call(http.MethodGet, "/web/api/home", nil)
	for _, row := range items(t, home, "continue") {
		if row["title"] == "Dinotopia" {
			t.Fatal("an 18 film was on a twelve-year-old's home page")
		}
	}
	_, found := h.call(http.MethodGet, "/web/api/search?q=dino", nil)
	for _, row := range items(t, found, "items") {
		if row["title"] == "Dinotopia" {
			t.Fatal("an 18 film was in a twelve-year-old's search results")
		}
	}
	res, _ := h.call(http.MethodGet, "/web/api/items/"+strconv.FormatInt(h.filmID, 10), nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("the film answered %d to an account that may not see it", res.StatusCode)
	}
}

// The PIN lifts it for an evening, and only the right PIN.
func TestAPINLiftsTheCeiling(t *testing.T) {
	h := newHarness(t)
	restrict(t, h, 12, true)
	ctx := context.Background()
	if err := h.db.SetUserPIN(ctx, h.userID, "4812"); err != nil {
		t.Fatal(err)
	}
	h.signIn()

	// The page is told there is a ceiling and that it can be lifted, which
	// is what draws the control at all.
	_, me := h.call(http.MethodGet, "/web/api/session", nil)
	if me["ceiling"] == nil || me["canUnlock"] != true || me["unlocked"] != false {
		t.Fatalf("session = %v", me)
	}

	if res, _ := h.call(http.MethodPost, "/web/api/unlock", map[string]any{"pin": "0000"}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("a wrong PIN = %d, want 403", res.StatusCode)
	}
	res, body := h.call(http.MethodPost, "/web/api/unlock", map[string]any{"pin": "4812"})
	if res.StatusCode != http.StatusOK || body["unlocked"] != true {
		t.Fatalf("unlock = %d: %v", res.StatusCode, body)
	}

	// And now the film is there.
	if res, _ := h.call(http.MethodGet, "/web/api/items/"+strconv.FormatInt(h.filmID, 10), nil); res.StatusCode != http.StatusOK {
		t.Fatalf("after unlocking, the film answered %d", res.StatusCode)
	}

	// Locking again puts it back, for somebody who wants it back before the
	// unlock runs out.
	if res, _ := h.call(http.MethodDelete, "/web/api/unlock", nil); res.StatusCode != http.StatusNoContent {
		t.Fatalf("lock = %d", res.StatusCode)
	}
	if res, _ := h.call(http.MethodGet, "/web/api/items/"+strconv.FormatInt(h.filmID, 10), nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("after locking, the film answered %d", res.StatusCode)
	}
}

// An account with no PIN cannot be unlocked, and is not told whether the
// guess was close.
func TestWithNoPINNothingIsUnlocked(t *testing.T) {
	h := newHarness(t)
	restrict(t, h, 12, true)
	h.signIn()

	_, me := h.call(http.MethodGet, "/web/api/session", nil)
	if me["canUnlock"] == true {
		t.Fatal("the page was told it could unlock an account with no PIN")
	}
	res, _ := h.call(http.MethodPost, "/web/api/unlock", map[string]any{"pin": ""})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("unlocking with no PIN = %d, want 403", res.StatusCode)
	}
}

// A stream cap refuses, because there is no smaller version of "a second film
// at once".
func TestAStreamCapRefusesTheSecondStream(t *testing.T) {
	register := nowplaying.New(nowplaying.Options{})
	h := newHarnessWith(t, func(s *web.Server) {
		s.Prober = prober{contents("h264", "aac", 1080, 2)}
		s.Transcoder = fakeEncoder(t)
		s.Playing = register
	})
	ctx := context.Background()
	if err := h.db.SetUserLimits(ctx, h.userID, store.Limits{
		Ceiling: -1, AllowUnrated: true, MaxStreams: 1,
	}); err != nil {
		t.Fatal(err)
	}
	h.signIn()

	// The first plays.
	if res, body := h.call(http.MethodPost, "/web/api/play", map[string]any{
		"itemId": h.filmID, "capabilities": browser,
	}); res.StatusCode != http.StatusOK {
		t.Fatalf("the first stream = %d: %v", res.StatusCode, body)
	}
	if register.Count() != 1 {
		t.Fatalf("the first stream was not registered: %v", register.List())
	}

	// The second, of something else, is refused with a sentence.
	res, body := h.call(http.MethodPost, "/web/api/play", map[string]any{
		"itemId": h.firstEp, "capabilities": browser,
	})
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the second stream = %d, want 429", res.StatusCode)
	}
	if message, _ := body["error"].(string); !strings.Contains(message, "at once") {
		t.Fatalf("error = %q", message)
	}

	// And the same stream again is not a second stream, or a cap of one
	// could never play anything.
	if res, _ := h.call(http.MethodPost, "/web/api/play", map[string]any{
		"itemId": h.filmID, "capabilities": browser,
	}); res.StatusCode != http.StatusOK {
		t.Fatalf("playing the same thing again = %d", res.StatusCode)
	}
}

// A bandwidth cap converts the stream down rather than refusing it, which is
// what somebody on a slow line actually wants.
func TestABandwidthCapConvertsRatherThanRefuses(t *testing.T) {
	// A file that declares twenty megabits, which a browser could play as it
	// is and this account may not be sent.
	heavy := contents("h264", "aac", 2160, 2)
	heavy.BitRate = 20_000_000
	h := newHarnessWith(t, func(s *web.Server) {
		s.Prober = prober{heavy}
		s.Transcoder = fakeEncoder(t)
	})
	ctx := context.Background()
	if err := h.db.SetUserLimits(ctx, h.userID, store.Limits{
		Ceiling: -1, AllowUnrated: true, MaxBitRate: 2_000_000,
	}); err != nil {
		t.Fatal(err)
	}
	h.signIn()

	res, body := h.call(http.MethodPost, "/web/api/play", map[string]any{
		"itemId": h.filmID, "capabilities": browser,
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("play = %d: %v", res.StatusCode, body)
	}
	if body["mode"] == "direct" {
		t.Fatalf("a file above the cap was sent as it is: %v", body)
	}
	if url, _ := body["url"].(string); !strings.HasPrefix(url, "/web/hls/") {
		t.Fatalf("url = %v", body["url"])
	}
}
