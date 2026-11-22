package webhook_test

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
	"github.com/inknironapps/bask-media-server/internal/webhook"
)

// fakeStore is the database the notifier reads, without one.
type fakeStore struct {
	mu           sync.Mutex
	destinations []store.Destination
	recorded     []record
}

type record struct {
	id      int64
	status  int
	failure string
}

func (f *fakeStore) Destinations(context.Context) ([]store.Destination, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.destinations, nil
}

func (f *fakeStore) RecordDelivery(_ context.Context, id int64, status int, failure string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded = append(f.recorded, record{id: id, status: status, failure: failure})
	return nil
}

func (f *fakeStore) results() []record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]record(nil), f.recorded...)
}

// catcher is a receiver, and what it was sent.
type catcher struct {
	*httptest.Server
	mu       sync.Mutex
	requests []caught
	status   int
}

type caught struct {
	headers http.Header
	body    []byte
}

func newCatcher(t *testing.T, status int) *catcher {
	t.Helper()
	c := &catcher{status: status}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.requests = append(c.requests, caught{headers: r.Header.Clone(), body: body})
		code := c.status
		c.mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *catcher) caught() []caught {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]caught(nil), c.requests...)
}

// A JSON destination gets a parsable object, signed where a secret is set,
// and the signature is the one a receiver computes for itself.
func TestAJSONDestinationGetsASignedObject(t *testing.T) {
	receiver := newCatcher(t, http.StatusNoContent)
	db := &fakeStore{destinations: []store.Destination{{
		Webhook: domain.Webhook{ID: 1, URL: receiver.URL, Format: domain.WebhookJSON, Enabled: true},
		Secret:  "shared",
	}}}

	notifier := webhook.New(webhook.Options{DB: db, Name: func() string { return "Bask" }})
	notifier.Send(webhook.Event{
		Kind: webhook.KindPlaybackStart, Title: "Big Hill",
		Text: "Riley started playing on the TV.", Data: map[string]any{"itemId": 7},
	})
	notifier.Close()

	got := receiver.caught()
	if len(got) != 1 {
		t.Fatalf("expected one delivery, got %d", len(got))
	}
	if event := got[0].headers.Get("X-Bask-Event"); event != webhook.KindPlaybackStart {
		t.Errorf("the kind should be in a header too, got %q", event)
	}
	if !webhook.Verify("shared", got[0].headers.Get("X-Bask-Signature"), got[0].body) {
		t.Error("the signature a receiver computes should match the one sent")
	}

	var body struct {
		Event   string         `json:"event"`
		Server  string         `json:"server"`
		Title   string         `json:"title"`
		Message string         `json:"message"`
		At      string         `json:"at"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(got[0].body, &body); err != nil {
		t.Fatalf("the body should be JSON: %v", err)
	}
	if body.Event != webhook.KindPlaybackStart || body.Server != "Bask" || body.Title != "Big Hill" {
		t.Errorf("wrong body: %+v", body)
	}
	if body.Message == "" || body.At == "" {
		t.Errorf("a person and a program both need something: %+v", body)
	}
	if body.Data["itemId"] != float64(7) {
		t.Errorf("the structured part should survive: %+v", body.Data)
	}

	if results := db.results(); len(results) != 1 || results[0].status != http.StatusNoContent || results[0].failure != "" {
		t.Errorf("the delivery should be recorded as a success: %+v", results)
	}
}

// An ntfy destination gets one line of text, because that is what the service
// turns into a notification.
func TestAnNtfyDestinationGetsOneLineOfText(t *testing.T) {
	receiver := newCatcher(t, http.StatusOK)
	db := &fakeStore{destinations: []store.Destination{{
		Webhook: domain.Webhook{ID: 2, URL: receiver.URL, Format: domain.WebhookNtfy, Enabled: true},
		Token:   "tk_topic",
	}}}

	notifier := webhook.New(webhook.Options{DB: db, Name: func() string { return "Bask" }})
	notifier.Send(webhook.Event{
		Kind: webhook.KindJobFailed, Title: "Failed: backup",
		Text: "The backup failed.", Urgent: true,
	})
	notifier.Close()

	got := receiver.caught()
	if len(got) != 1 {
		t.Fatalf("expected one delivery, got %d", len(got))
	}
	if string(got[0].body) != "The backup failed." {
		t.Errorf("the body should be the sentence, got %q", got[0].body)
	}
	// An ASCII title goes over the wire as it is, so a header somebody reads
	// in a log is readable.
	if title := got[0].headers.Get("X-Title"); title != "Failed: backup - Bask" {
		t.Errorf("the title should name the server, got %q", title)
	}
	if priority := got[0].headers.Get("X-Priority"); priority != "4" {
		t.Errorf("an urgent event should be sent as urgent, got %q", priority)
	}
	if tags := got[0].headers.Get("X-Tags"); tags == "" {
		t.Error("a notification with no tag is a grey line among grey lines")
	}
	if auth := got[0].headers.Get("Authorization"); auth != "Bearer tk_topic" {
		t.Errorf("the token should be sent, got %q", auth)
	}
	if ct := got[0].headers.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("ntfy is sent text, got %q", ct)
	}
}

// An HTTP header is Latin-1 by the specification and a library's titles are
// not. An encoded word is the one form that survives both the specification
// and ntfy, which decodes it; the alternative is a notification full of
// question marks.
func TestATitleWithAnAccentSurvivesTheWire(t *testing.T) {
	receiver := newCatcher(t, http.StatusOK)
	db := &fakeStore{destinations: []store.Destination{{
		Webhook: domain.Webhook{ID: 1, URL: receiver.URL, Format: domain.WebhookNtfy, Enabled: true},
	}}}

	notifier := webhook.New(webhook.Options{DB: db, Name: func() string { return "Bask" }})
	notifier.Send(webhook.Event{
		Kind: webhook.KindPlaybackStart, Title: "Amélie — Chapitre un", Text: "Riley started playing.",
	})
	notifier.Close()

	got := receiver.caught()
	if len(got) != 1 {
		t.Fatalf("expected one delivery, got %d", len(got))
	}
	sent := got[0].headers.Get("X-Title")
	if !strings.HasPrefix(sent, "=?UTF-8?B?") {
		t.Fatalf("a non-ASCII title should be an encoded word, got %q", sent)
	}
	decoded, err := new(mime.WordDecoder).DecodeHeader(sent)
	if err != nil {
		t.Fatalf("a receiver could not decode it: %v", err)
	}
	if decoded != "Amélie — Chapitre un - Bask" {
		t.Errorf("what a receiver reads is %q", decoded)
	}
	// The body is not a header, so it needs no encoding at all.
	if string(got[0].body) != "Riley started playing." {
		t.Errorf("the body should be the sentence, got %q", got[0].body)
	}
}

// A refusal is an answer: asking again changes nothing, and the second
// attempt is spent on a receiver that might still come good.
func TestARefusalIsNotRetriedAndAFaultIs(t *testing.T) {
	refusing := newCatcher(t, http.StatusBadRequest)
	broken := newCatcher(t, http.StatusInternalServerError)

	db := &fakeStore{destinations: []store.Destination{
		{Webhook: domain.Webhook{ID: 1, URL: refusing.URL, Format: domain.WebhookJSON, Enabled: true}},
		{Webhook: domain.Webhook{ID: 2, URL: broken.URL, Format: domain.WebhookJSON, Enabled: true}},
	}}

	notifier := webhook.New(webhook.Options{DB: db})
	notifier.Send(webhook.Event{Kind: webhook.KindTest, Text: "hello"})
	notifier.Close()

	if got := refusing.caught(); len(got) != 1 {
		t.Errorf("a 400 should be asked once, got %d attempts", len(got))
	}
	if got := broken.caught(); len(got) != 2 {
		t.Errorf("a 500 should be retried once, got %d attempts", len(got))
	}
	for _, result := range db.results() {
		if result.failure == "" {
			t.Errorf("both destinations failed and both should be recorded as failures: %+v", result)
		}
	}
}

// A destination is told what it asked for and nothing else.
func TestADestinationOnlyHearsWhatItSubscribedTo(t *testing.T) {
	playback := newCatcher(t, http.StatusOK)
	everything := newCatcher(t, http.StatusOK)

	db := &fakeStore{destinations: []store.Destination{
		{Webhook: domain.Webhook{
			ID: 1, URL: playback.URL, Format: domain.WebhookJSON,
			Events: []string{"playback.*"}, Enabled: true,
		}},
		{Webhook: domain.Webhook{ID: 2, URL: everything.URL, Format: domain.WebhookJSON, Enabled: true}},
	}}

	notifier := webhook.New(webhook.Options{DB: db})
	notifier.Send(webhook.Event{Kind: webhook.KindPlaybackStart, Text: "started"})
	notifier.Send(webhook.Event{Kind: webhook.KindJobFailed, Text: "failed"})
	notifier.Close()

	if got := playback.caught(); len(got) != 1 {
		t.Errorf("a playback subscriber should get one of the two events, got %d", len(got))
	}
	if got := everything.caught(); len(got) != 2 {
		t.Errorf("an empty subscription means everything, got %d of 2", len(got))
	}
}

// Events arrive in the order they happened. An automation that turns a light
// on at a start and off at a stop must never be handed the pair backwards.
func TestEventsArriveInOrder(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []string
	)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("X-Bask-Event"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	db := &fakeStore{destinations: []store.Destination{
		{Webhook: domain.Webhook{ID: 1, URL: receiver.URL, Format: domain.WebhookJSON, Enabled: true}},
	}}
	notifier := webhook.New(webhook.Options{DB: db})
	for _, kind := range []string{
		webhook.KindPlaybackStart, webhook.KindPlaybackPause,
		webhook.KindPlaybackResume, webhook.KindPlaybackStop,
	} {
		notifier.Send(webhook.Event{Kind: kind, Text: kind})
	}
	notifier.Close()

	mu.Lock()
	defer mu.Unlock()
	want := []string{
		webhook.KindPlaybackStart, webhook.KindPlaybackPause,
		webhook.KindPlaybackResume, webhook.KindPlaybackStop,
	}
	if len(seen) != len(want) {
		t.Fatalf("expected %d deliveries, got %v", len(want), seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("out of order: %v", seen)
		}
	}
}

func TestAnAddressThatCannotBePostedToIsRefusedWithoutAsking(t *testing.T) {
	db := &fakeStore{destinations: []store.Destination{
		{Webhook: domain.Webhook{ID: 1, URL: "ftp://example.test/hook", Format: domain.WebhookJSON, Enabled: true}},
	}}
	notifier := webhook.New(webhook.Options{DB: db})
	status, err := notifier.Deliver(context.Background(), db.destinations[0], webhook.Test("Bask"))
	notifier.Close()

	if err == nil {
		t.Fatal("an address that is not http should be refused")
	}
	if status != 0 {
		t.Errorf("nothing was sent, so there is no status: %d", status)
	}
}

// Something can always report one more thing while the process is going
// down. A notification is never worth taking the server with it.
func TestAnEventOfferedDuringShutdownIsDroppedNotFatal(t *testing.T) {
	receiver := newCatcher(t, http.StatusOK)
	db := &fakeStore{destinations: []store.Destination{
		{Webhook: domain.Webhook{ID: 1, URL: receiver.URL, Format: domain.WebhookJSON, Enabled: true}},
	}}
	notifier := webhook.New(webhook.Options{DB: db})
	notifier.Close()
	notifier.Close() // twice, because a shutdown path may be reached twice

	notifier.Send(webhook.Event{Kind: webhook.KindPlaybackStop, Text: "too late"})
	if got := receiver.caught(); len(got) != 0 {
		t.Errorf("an event sent after shutdown should be dropped, got %d deliveries", len(got))
	}
}

// A nil notifier is a server built without one, and every caller is allowed
// to be unaware of that.
func TestANilNotifierIsHarmless(t *testing.T) {
	var notifier *webhook.Notifier
	notifier.Send(webhook.Event{Kind: webhook.KindTest, Text: "hello"})
	notifier.Close()
}

func TestWhatADestinationWants(t *testing.T) {
	cases := []struct {
		name       string
		subscribed []string
		kind       string
		want       bool
	}{
		{"nothing said means everything", nil, webhook.KindJobFailed, true},
		{"a star means everything", []string{"*"}, webhook.KindLibraryNew, true},
		{"an exact kind", []string{webhook.KindPlaybackStop}, webhook.KindPlaybackStop, true},
		{"an exact kind is not another", []string{webhook.KindPlaybackStop}, webhook.KindPlaybackStart, false},
		{"a family covers its kinds", []string{"playback.*"}, webhook.KindPlaybackResume, true},
		{"a family covers nothing else", []string{"playback.*"}, webhook.KindLibraryNew, false},
		{"one of several", []string{"job.*", "library.*"}, webhook.KindLibraryNew, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := webhook.Wants(c.subscribed, c.kind); got != c.want {
				t.Errorf("Wants(%v, %q) = %v", c.subscribed, c.kind, got)
			}
		})
	}
}

// Nothing is sent to a destination that was deleted while an event was in
// flight, and a notifier with no destinations is not an error.
func TestAnEventWithNowhereToGoIsHarmless(t *testing.T) {
	db := &fakeStore{}
	notifier := webhook.New(webhook.Options{DB: db})
	notifier.Send(webhook.Event{Kind: webhook.KindTest, Text: "hello"})
	notifier.Close()

	if results := db.results(); len(results) != 0 {
		t.Errorf("nothing was delivered, so nothing should be recorded: %+v", results)
	}
}
