package admin_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// playable is one film in one library, with a file behind it.
func playable(t *testing.T, h *harness, title string) (itemID, partID int64) {
	t.Helper()
	ctx := context.Background()

	films, err := h.db.CreateSection(ctx, "Films", domain.SectionMovies, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), title+".mkv")
	// A real file, because this test serves it: the point of the refusal is
	// that it replaces bytes that were otherwise going out.
	if err := os.WriteFile(path, []byte("film"), 0o600); err != nil {
		t.Fatal(err)
	}
	writer := h.db.NewWriter(1)
	if err := writer.Add(ctx, store.Found{
		SectionID: films.ID, Kind: domain.KindMovie, Title: title, Year: 2026,
		Path: path, Container: "mkv", Size: 4,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	items, _, err := h.db.SectionItems(ctx, films.ID, domain.KindMovie, store.Page{Size: 1})
	if err != nil || len(items) != 1 {
		t.Fatalf("seeding failed: %v %v", items, err)
	}
	parts, err := h.db.Parts(ctx, items[0].ID)
	if err != nil || len(parts) != 1 {
		t.Fatalf("no file behind the item: %v %v", parts, err)
	}
	return items[0].ID, parts[0].ID
}

// asPlexClient makes the request a Plex client would, with the headers one
// sends: the identifier is what the register keys a stream on.
func (h *harness) asPlexClient(method, path, device string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(method, h.server.URL+path, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("X-Plex-Token", h.viewerToken)
	req.Header.Set("X-Plex-Client-Identifier", device)
	req.Header.Set("X-Plex-Product", "Plex for Android (TV)")
	req.Header.Set("X-Plex-Device-Name", "Living room")
	res, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return res
}

// The whole feature in one test: a client plays something, the interface says
// who is watching what, the administrator stops it, and the client is refused
// the next time it asks for the file.
func TestAStreamIsSeenAndStopped(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	item, part := playable(t, h, "Big Hill")

	// Nothing is playing to begin with, and the page says so by listing
	// nothing rather than by failing.
	_, empty := h.call(http.MethodGet, "/admin/api/playing", nil)
	if streams, _ := empty["playing"].([]any); len(streams) != 0 {
		t.Fatalf("something was playing before anything played: %v", streams)
	}

	// A client reports where it is, which is the request it was going to make
	// anyway.
	timeline := "/:/timeline?ratingKey=" + strconv.FormatInt(item, 10) + "&time=61000&duration=5400000&state=playing"
	if res := h.asPlexClient(http.MethodGet, timeline, "device-1"); res.StatusCode != http.StatusOK {
		res.Body.Close()
		t.Fatalf("timeline = %d", res.StatusCode)
	} else {
		res.Body.Close()
	}

	_, body := h.call(http.MethodGet, "/admin/api/playing", nil)
	streams, _ := body["playing"].([]any)
	if len(streams) != 1 {
		t.Fatalf("expected one stream, got %v", streams)
	}
	stream, _ := streams[0].(map[string]any)
	if stream["title"] != "Big Hill" {
		t.Fatalf("the page did not name what was playing: %v", stream)
	}
	if stream["user"] != "kitchen-tv" {
		t.Fatalf("the page did not name who was watching: %v", stream)
	}
	if stream["mode"] != "direct" {
		t.Fatalf("mode = %v", stream["mode"])
	}
	if int64(stream["positionMs"].(float64)) != 61000 {
		t.Fatalf("position = %v", stream["positionMs"])
	}
	if stream["client"] != "Plex for Android (TV)" {
		t.Fatalf("client = %v", stream["client"])
	}

	// The file is being served while the stream is live.
	res := h.asPlexClient(http.MethodGet, "/library/parts/"+strconv.FormatInt(part, 10), "device-1")
	res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		t.Fatalf("the file was not served: %d", res.StatusCode)
	}

	key, _ := stream["key"].(string)
	if key == "" {
		t.Fatal("the page gave no way to stop the stream")
	}
	_, stopped := h.call(http.MethodPost, "/admin/api/playing/terminate",
		map[string]any{"key": key, "reason": "Dinner."})
	if stopped["stopped"] != true {
		t.Fatalf("stopping reported %v", stopped)
	}

	// Gone from the page.
	_, after := h.call(http.MethodGet, "/admin/api/playing", nil)
	if streams, _ := after["playing"].([]any); len(streams) != 0 {
		t.Fatalf("a stopped stream is still listed: %v", streams)
	}

	// And refused the next time it asks for bytes, which is the only
	// enforcement there can be: nothing is pushed to a client from here.
	res = h.asPlexClient(http.MethodGet, "/library/parts/"+strconv.FormatInt(part, 10), "device-1")
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("the file was still served to a stopped stream: %d", res.StatusCode)
	}
	said, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(said), "Dinner.") {
		t.Fatalf("the refusal did not say why: %q", said)
	}

	// Its next timeline report carries Plex's own termination, which is what
	// makes the client stop and show the sentence rather than stall.
	res = h.asPlexClient(http.MethodGet, timeline, "device-1")
	defer res.Body.Close()
	answer, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(answer), "terminationCode=\"2006\"") {
		t.Fatalf("the timeline did not terminate the stream: %q", answer)
	}
	if !strings.Contains(string(answer), "Dinner.") {
		t.Fatalf("the termination did not say why: %q", answer)
	}
}

// Another device is another stream, so stopping one leaves the other alone.
// A household with a television and a tablet is the normal case, not an edge.
func TestStoppingOneDeviceLeavesTheOther(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	item, part := playable(t, h, "Big Hill")

	timeline := "/:/timeline?ratingKey=" + strconv.FormatInt(item, 10) + "&time=1000&duration=5400000&state=playing"
	for _, device := range []string{"television", "tablet"} {
		h.asPlexClient(http.MethodGet, timeline, device).Body.Close()
	}

	_, body := h.call(http.MethodGet, "/admin/api/playing", nil)
	streams, _ := body["playing"].([]any)
	if len(streams) != 2 {
		t.Fatalf("expected two streams, got %v", streams)
	}

	var televisionKey string
	for _, raw := range streams {
		stream, _ := raw.(map[string]any)
		if strings.Contains(stream["key"].(string), "television") {
			televisionKey = stream["key"].(string)
		}
	}
	if televisionKey == "" {
		t.Fatalf("could not find the television: %v", streams)
	}
	h.call(http.MethodPost, "/admin/api/playing/terminate", map[string]any{"key": televisionKey})

	res := h.asPlexClient(http.MethodGet, "/library/parts/"+strconv.FormatInt(part, 10), "television")
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("the television was not stopped: %d", res.StatusCode)
	}
	res = h.asPlexClient(http.MethodGet, "/library/parts/"+strconv.FormatInt(part, 10), "tablet")
	res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		t.Fatalf("the tablet was stopped as well: %d", res.StatusCode)
	}
}

// A client that says it has stopped is taken at its word: the line goes
// immediately rather than at the idle deadline.
func TestAClientThatStopsLeavesThePage(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	item, _ := playable(t, h, "Big Hill")

	base := "/:/timeline?ratingKey=" + strconv.FormatInt(item, 10) + "&duration=5400000&time=1000&state="
	h.asPlexClient(http.MethodGet, base+"playing", "device-1").Body.Close()
	if h.playing.Count() != 1 {
		t.Fatal("the stream was not registered")
	}
	h.asPlexClient(http.MethodGet, base+"stopped", "device-1").Body.Close()
	if h.playing.Count() != 0 {
		t.Fatalf("still playing after the client stopped: %v", h.playing.List())
	}
}

// Stopping something that is not playing is not an error: it is the outcome
// the administrator asked for, reached before they pressed the button.
func TestStoppingSomethingThatIsNotPlaying(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)

	res, body := h.call(http.MethodPost, "/admin/api/playing/terminate", map[string]any{"key": "plex|ghost|1"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("terminate = %d: %v", res.StatusCode, body)
	}
	if body["stopped"] != false {
		t.Fatalf("claimed to stop a stream that was not playing: %v", body)
	}
}

// Nobody but an administrator can see who is watching, or stop them.
func TestOnlyAnAdministratorSeesWhoIsWatching(t *testing.T) {
	h := newHarness(t)

	for _, path := range []string{"/admin/api/playing"} {
		res, _ := h.call(http.MethodGet, path, nil)
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s without a session = %d, want 401", path, res.StatusCode)
		}
	}
	h.signIn(h.viewerToken)
	res, _ := h.call(http.MethodGet, "/admin/api/playing", nil)
	if res.StatusCode == http.StatusOK {
		t.Error("a viewer's token opened the administration interface")
	}
}
