package jellyfin_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/nowplaying"
	"github.com/inknironapps/bask-media-server/internal/store"
	"github.com/inknironapps/bask-media-server/internal/webhook"
)

// The whole chain, from the request a real client makes to the body somebody
// else's server receives: a client reports where it is, the register notices
// a stream it had not seen, the wording is built, and it is delivered.
//
// Here rather than in the webhook package because this is the only place the
// first link exists. Everything up to the register is tested by the surface's
// own tests and everything after it by the notifier's; this is the test that
// they are joined up.
func TestAClientPressingPlayReachesAWebhook(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies [][]byte
	)
	far := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer far.Close()

	server, token, db := library(t)
	if _, err := db.AddWebhook(t.Context(), store.NewWebhook{
		URL: far.URL, Format: domain.WebhookJSON, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	notifier := webhook.New(webhook.Options{DB: db, Name: func() string { return "Bask" }})
	server.Playing = nowplaying.New(nowplaying.Options{
		Changes: func(change nowplaying.Change) {
			if event, ok := webhook.Playback(change); ok {
				notifier.Send(event)
			}
		},
	})

	c := call{t: t, server: server, token: token}
	films := c.items("/Users/" + c.me() + "/Items?ParentId=" + c.views()["Films"]["Id"].(string))
	id := films[0]["Id"].(string)

	report := `{"ItemId":"` + id + `","PositionTicks":600000000,"PlaySessionId":"play-1"}`
	if rec := watching(t, server, token, "television", http.MethodPost, "/Sessions/Playing/Progress", report); rec.Code != http.StatusNoContent {
		t.Fatalf("reporting progress = %d", rec.Code)
	}
	// The same report again, which is what a client sends every ten seconds.
	// It is not a second event.
	watching(t, server, token, "television", http.MethodPost, "/Sessions/Playing/Progress", report)

	live := server.Playing.List()
	if len(live) != 1 {
		t.Fatalf("expected one stream, got %+v", live)
	}
	server.Playing.Stopped(live[0].Key)

	// Closing drains the queue, so there is nothing to poll for.
	notifier.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("expected a start and a stop, got %d deliveries", len(bodies))
	}

	var start, stop struct {
		Event   string         `json:"event"`
		Message string         `json:"message"`
		Title   string         `json:"title"`
		Server  string         `json:"server"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(bodies[0], &start); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(bodies[1], &stop); err != nil {
		t.Fatal(err)
	}
	if start.Event != webhook.KindPlaybackStart || stop.Event != webhook.KindPlaybackStop {
		t.Fatalf("expected a start then a stop, got %q then %q", start.Event, stop.Event)
	}
	if start.Server != "Bask" || start.Title == "" || start.Message == "" {
		t.Errorf("the start says nothing useful: %+v", start)
	}
	if start.Data["device"] != "television" || start.Data["surface"] != "jellyfin" {
		t.Errorf("the event should carry what the client said it was: %+v", start.Data)
	}
	if start.Data["itemId"] == nil || start.Data["itemId"] == float64(0) {
		t.Errorf("the event should name what was played: %+v", start.Data)
	}
}
