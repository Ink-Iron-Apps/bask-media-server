package admin_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/webhook"
)

// receiver stands in for whatever is at the far end: Home Assistant, ntfy, a
// script of somebody's own.
type receiver struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []string
	status int
}

func newReceiver(t *testing.T, status int) *receiver {
	t.Helper()
	r := &receiver{status: status}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.bodies = append(r.bodies, string(body))
		code := r.status
		r.mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *receiver) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.bodies...)
}

// sending gives the interface a notifier, which the real server always has.
func sending(t *testing.T, h *harness) {
	t.Helper()
	notifier := webhook.New(webhook.Options{DB: h.db, Name: func() string { return "Bask Media Server" }})
	t.Cleanup(notifier.Close)
	h.panel.Notifier = notifier
}

// The whole flow: nothing, an address, a test that arrives, a switch, a
// removal.
func TestAWebhookIsAddedTestedAndRemoved(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	sending(t, h)
	far := newReceiver(t, http.StatusNoContent)

	_, before := h.call(http.MethodGet, "/admin/api/webhooks", nil)
	if hooks, _ := before["webhooks"].([]any); len(hooks) != 0 {
		t.Fatalf("a fresh server tells nobody anything, it listed %v", hooks)
	}
	if kinds, _ := before["kinds"].([]any); len(kinds) == 0 {
		t.Fatal("the page needs to be told which events there are")
	}

	res, made := h.call(http.MethodPost, "/admin/api/webhooks", map[string]any{
		"url": far.URL, "format": "json",
	})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("adding a webhook answered %s", res.Status)
	}
	id := int64(made["id"].(float64))
	if made["enabled"] != true {
		t.Error("a new webhook should be on, or adding it did nothing")
	}
	if events, _ := made["events"].([]any); len(events) != 0 {
		t.Errorf("a new webhook hears about everything, it asked for %v", events)
	}

	res, sent := h.call(http.MethodPost, "/admin/api/webhooks/"+itoa(id)+"/test", map[string]any{})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the test button answered %s", res.Status)
	}
	if sent["delivered"] != true {
		t.Fatalf("the test should have been delivered: %+v", sent)
	}
	if bodies := far.got(); len(bodies) != 1 {
		t.Fatalf("the receiver should have been sent one thing, got %d", len(bodies))
	}

	// The delivery is remembered, so a destination that stops working can be
	// seen to have stopped.
	_, after := h.call(http.MethodGet, "/admin/api/webhooks", nil)
	hooks, _ := after["webhooks"].([]any)
	if len(hooks) != 1 {
		t.Fatalf("expected one webhook, got %v", hooks)
	}
	hook := hooks[0].(map[string]any)
	if hook["lastStatus"] != float64(http.StatusNoContent) || hook["lastAt"] == nil {
		t.Errorf("the last delivery should be on the page: %+v", hook)
	}

	// Switched off rather than deleted: the address is worth keeping.
	h.call(http.MethodPatch, "/admin/api/webhooks/"+itoa(id), map[string]any{"enabled": false})
	_, off := h.call(http.MethodGet, "/admin/api/webhooks", nil)
	hooks, _ = off["webhooks"].([]any)
	if hooks[0].(map[string]any)["enabled"] != false {
		t.Error("switching it off did nothing")
	}

	res, _ = h.call(http.MethodDelete, "/admin/api/webhooks/"+itoa(id), nil)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("removing it answered %s", res.Status)
	}
	_, gone := h.call(http.MethodGet, "/admin/api/webhooks", nil)
	if hooks, _ := gone["webhooks"].([]any); len(hooks) != 0 {
		t.Errorf("it should be gone: %v", hooks)
	}
}

// The rule this page is held to: a credential set here is never read back
// out, however convenient that would be for the form.
func TestTheInterfaceNeverShowsATokenOrASecret(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)

	_, made := h.call(http.MethodPost, "/admin/api/webhooks", map[string]any{
		"url": "https://example.test/hook", "token": "tk_secret", "secret": "shhh",
	})
	if made["hasToken"] != true || made["hasSecret"] != true {
		t.Fatalf("the page should know they are set: %+v", made)
	}

	raw := wire(t, h, "/admin/api/webhooks")
	for _, leaked := range []string{"tk_secret", "shhh"} {
		if strings.Contains(raw, leaked) {
			t.Fatalf("the page was sent a credential: %s", raw)
		}
	}
}

// wire is exactly what the page receives, because a test that re-reads a
// parsed structure cannot see a field that should not be in the bytes.
func wire(t *testing.T, h *harness, path string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-BMS-Admin", "1")
	res, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// A destination that refuses is not this interface's failure. The page says
// what happened rather than claiming the server is broken.
func TestADestinationThatRefusesIsReportedNotHidden(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	sending(t, h)
	far := newReceiver(t, http.StatusForbidden)

	_, made := h.call(http.MethodPost, "/admin/api/webhooks", map[string]any{"url": far.URL})
	id := int64(made["id"].(float64))

	res, sent := h.call(http.MethodPost, "/admin/api/webhooks/"+itoa(id)+"/test", map[string]any{})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the request worked, so it should answer 200, not %s", res.Status)
	}
	if sent["delivered"] != false {
		t.Errorf("it was refused, so it was not delivered: %+v", sent)
	}
	if sent["error"] == nil || sent["status"] != float64(http.StatusForbidden) {
		t.Errorf("the page should be told what the receiver said: %+v", sent)
	}
}

// An event this server does not send is a typo, and a subscription that
// matches nothing is the worst outcome: a destination that looks right and
// receives nothing for ever.
func TestAnUnknownEventIsRefused(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)

	res, body := h.call(http.MethodPost, "/admin/api/webhooks", map[string]any{
		"url": "https://example.test/hook", "events": []string{"playback.started"},
	})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a typed event kind should be refused, got %s", res.Status)
	}
	if body["error"] == nil {
		t.Error("the refusal should say what was wrong")
	}

	res, _ = h.call(http.MethodPost, "/admin/api/webhooks", map[string]any{
		"url": "https://example.test/hook", "events": []string{"playback.*", "job.done"},
	})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("a family and a kind are both valid, got %s", res.Status)
	}
}

func TestAnUnknownFormatIsRefused(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)

	res, _ := h.call(http.MethodPost, "/admin/api/webhooks", map[string]any{
		"url": "https://example.test/hook", "format": "xml",
	})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("this server sends json or ntfy, got %s", res.Status)
	}
}

// A server with no notifier can still be configured, and says so rather than
// pretending the test button worked.
func TestWithoutANotifierTheTestButtonSaysSo(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)

	_, made := h.call(http.MethodPost, "/admin/api/webhooks", map[string]any{"url": "https://example.test/hook"})
	id := int64(made["id"].(float64))

	res, _ := h.call(http.MethodPost, "/admin/api/webhooks/"+itoa(id)+"/test", map[string]any{})
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected the page to be told nothing can be sent, got %s", res.Status)
	}

	_, listed := h.call(http.MethodGet, "/admin/api/webhooks", nil)
	if listed["sending"] != false {
		t.Error("the page should know nothing is being sent")
	}
}

// Nobody but an administrator configures where this server sends what the
// household is watching. A viewer's token cannot sign in at all, so these
// routes are reached by nobody.
func TestOnlyAnAdministratorMayConfigureWebhooks(t *testing.T) {
	h := newHarness(t)
	if res := h.signIn(h.viewerToken); res.StatusCode != http.StatusForbidden {
		t.Fatalf("a viewer signed in: %s", res.Status)
	}

	res, _ := h.call(http.MethodGet, "/admin/api/webhooks", nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a viewer reading the destinations got %s", res.Status)
	}
	res, _ = h.call(http.MethodPost, "/admin/api/webhooks", map[string]any{"url": "https://example.test/hook"})
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a viewer adding a destination got %s", res.Status)
	}
}
