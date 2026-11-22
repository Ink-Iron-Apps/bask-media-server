package jellyfin_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/jellyfin"
	"github.com/inknironapps/bask-media-server/internal/nowplaying"
)

// watching is a client's request with the Authorization header a real one
// sends, because the device in that header is what a stream is keyed on —
// and it is on every request, including the one that fetches the file.
func watching(t *testing.T, server *jellyfin.Server, token, device, method, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Authorization",
		`MediaBrowser Client="Bask", Device="`+device+`", DeviceId="`+device+`", Version="2.55.7", Token="`+token+`"`)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

// A client reporting where it is appears on the status page, and stopping it
// is refused bytes rather than a message: there is nothing to push to a
// client from here.
func TestAJellyfinStreamIsSeenAndStopped(t *testing.T) {
	server, token, _ := library(t)
	register := nowplaying.New(nowplaying.Options{})
	server.Playing = register
	c := call{t: t, server: server, token: token}

	films := c.items("/Users/" + c.me() + "/Items?ParentId=" + c.views()["Films"]["Id"].(string))
	id := films[0]["Id"].(string)

	report := `{"ItemId":"` + id + `","PositionTicks":600000000,"PlaySessionId":"play-1"}`
	if rec := watching(t, server, token, "television", http.MethodPost, "/Sessions/Playing/Progress", report); rec.Code != http.StatusNoContent {
		t.Fatalf("reporting progress = %d", rec.Code)
	}

	live := register.List()
	if len(live) != 1 {
		t.Fatalf("expected one stream, got %+v", live)
	}
	if live[0].Surface != "jellyfin" || live[0].Device != "television" || live[0].Client != "Bask" {
		t.Fatalf("the stream did not say who was watching on what: %+v", live[0])
	}
	if live[0].Position != 60_000_000_000 {
		t.Fatalf("position = %s", live[0].Position)
	}

	// Serving the file while it is live, refusing it afterwards. The same
	// request either way: that is the whole of the enforcement.
	if rec := watching(t, server, token, "television", http.MethodGet, "/Videos/"+id+"/stream", ""); rec.Code != http.StatusOK {
		t.Fatalf("the file was not served while the stream was live: %d", rec.Code)
	}
	if _, ok := register.Terminate(live[0].Key, "Bedtime."); !ok {
		t.Fatal("the stream could not be terminated")
	}
	rec := watching(t, server, token, "television", http.MethodGet, "/Videos/"+id+"/stream", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("the file was still served to a stopped stream: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Bedtime.") {
		t.Fatalf("the refusal did not say why: %q", rec.Body.String())
	}

	// And it cannot restart itself by asking for a conversion instead.
	rec = watching(t, server, token, "television", http.MethodGet, "/Videos/"+id+"/master.m3u8?PlaySessionId=play-1", "")
	if rec.Code == http.StatusOK {
		t.Fatal("a stopped stream negotiated its way back in")
	}
}

// The client saying it has stopped takes it off the page immediately.
func TestAJellyfinClientThatStopsLeavesThePage(t *testing.T) {
	server, token, _ := library(t)
	register := nowplaying.New(nowplaying.Options{})
	server.Playing = register
	c := call{t: t, server: server, token: token}

	films := c.items("/Users/" + c.me() + "/Items?ParentId=" + c.views()["Films"]["Id"].(string))
	id := films[0]["Id"].(string)

	body := `{"ItemId":"` + id + `","PositionTicks":600000000,"PlaySessionId":"play-1"}`
	watching(t, server, token, "television", http.MethodPost, "/Sessions/Playing", body)
	if register.Count() != 1 {
		t.Fatal("the stream was not registered")
	}
	watching(t, server, token, "television", http.MethodPost, "/Sessions/Playing/Stopped", body)
	if register.Count() != 0 {
		t.Fatalf("still playing after the client stopped: %+v", register.List())
	}
}

// A server with no register plays everything exactly as it did before: the
// page is an addition, not a gate.
func TestWithNoRegisterNothingChanges(t *testing.T) {
	server, token, _ := library(t)
	c := call{t: t, server: server, token: token}

	films := c.items("/Users/" + c.me() + "/Items?ParentId=" + c.views()["Films"]["Id"].(string))
	id := films[0]["Id"].(string)

	if rec := watching(t, server, token, "television", http.MethodGet, "/Videos/"+id+"/stream", ""); rec.Code != http.StatusOK {
		t.Fatalf("the file was not served: %d", rec.Code)
	}
	report := `{"ItemId":"` + id + `","PositionTicks":600000000,"PlaySessionId":"play-1"}`
	if rec := watching(t, server, token, "television", http.MethodPost, "/Sessions/Playing/Progress", report); rec.Code != http.StatusNoContent {
		t.Fatalf("reporting progress = %d", rec.Code)
	}
}
