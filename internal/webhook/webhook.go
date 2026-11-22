// Package webhook tells somewhere else when something happens here.
//
// This server cannot push anything to a client — there is no websocket in it,
// by design — so everything it knows stays inside it. A household that wants
// the lights dimmed when a film starts, or a message on a phone when a new
// episode lands, has nowhere to hang that behaviour. This is that place: a
// destination, and an HTTP request to it when an event happens.
//
// Four decisions shape it:
//
//   - Events are rare on purpose. A start, a pause, a resume, a stop, a new
//     item, a finished pass. Not progress: a client reports its position every
//     ten seconds, and a webhook that fires on each of those is a denial of
//     service with the operator's own blessing.
//   - Delivery never blocks the request that caused it. A viewer pressing
//     play waits for this server, not for somebody's home automation, so an
//     event goes on a queue and the handler returns. A full queue drops the
//     event and says so in the log, because the alternative is holding up
//     playback for a notification.
//   - Order is kept across events, parallel across destinations. One worker
//     takes events in turn so a stop can never arrive before its start; the
//     destinations for one event are delivered to at the same time, so a LAN
//     box that is switched off does not delay a phone.
//   - Two shapes, because the two receivers are different. Something that
//     parses JSON wants a JSON object; a notification service wants one line
//     of text with a title. Translating in between was the one thing standing
//     between this server and the ntfy already running on the same machine.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// Event kinds. A kind is a family and a name, so a destination can subscribe
// to "playback.*" without having to list every kind that exists today and
// without being silently left out of one added tomorrow.
const (
	KindPlaybackStart  = "playback.start"
	KindPlaybackPause  = "playback.pause"
	KindPlaybackResume = "playback.resume"
	KindPlaybackStop   = "playback.stop"
	KindLibraryNew     = "library.new"
	KindJobDone        = "job.done"
	KindJobFailed      = "job.failed"

	// KindTest is what the button on the settings page sends. A real kind
	// rather than a special case, so what arrives is exactly the shape a
	// real event arrives in.
	KindTest = "test"
)

// Families is every event family, for an interface offering them.
var Families = []string{"playback", "library", "job"}

// Kinds is every kind this server sends, in the order an interface should
// list them.
var Kinds = []string{
	KindPlaybackStart, KindPlaybackPause, KindPlaybackResume, KindPlaybackStop,
	KindLibraryNew, KindJobDone, KindJobFailed,
}

// Event is something that happened.
type Event struct {
	Kind string
	At   time.Time

	// Title is the heading, and Text the sentence a person reads. Both are
	// written here rather than by the receiver, because a notification whose
	// text is assembled at the far end is a notification that reads like a
	// database row.
	Title string
	Text  string

	// Data is what a program wants: the identifiers and numbers behind the
	// sentence. Carried in the JSON body and left out of a text message.
	Data map[string]any

	// Urgent marks an event somebody should look at now rather than later. A
	// failed pass is urgent; a film starting is not.
	Urgent bool
}

// queueDepth is how many events wait before the next one is dropped.
//
// Generous enough to ride out a destination that is slow and an evening where
// four people start something at once; bounded because an unbounded queue in
// front of a dead receiver is how a long-running process runs out of memory.
const queueDepth = 256

// Delivery limits. Two attempts rather than five: a receiver that is up
// answers the first one, and the second covers a dropped connection. Beyond
// that the thing is down, and holding a worker for a minute to prove it costs
// more than the notification was worth.
const (
	attempts       = 2
	attemptTimeout = 8 * time.Second
	retryAfter     = time.Second
)

// Store is what the notifier needs of the database.
type Store interface {
	Destinations(ctx context.Context) ([]store.Destination, error)
	RecordDelivery(ctx context.Context, id int64, status int, failure string, at time.Time) error
}

// Notifier sends events to the destinations that asked for them.
type Notifier struct {
	db   Store
	http *http.Client
	log  *slog.Logger
	name func() string
	now  func() time.Time

	queue chan Event
	done  chan struct{}

	// mu guards closed, which is the difference between dropping an event
	// during shutdown and panicking on a closed channel. Something can
	// always report one more thing while the process is going down — a job
	// being cancelled, a stream expiring — and a notification is never worth
	// taking the server with it.
	mu     sync.RWMutex
	closed bool
}

// Options configure a notifier.
type Options struct {
	DB Store

	// Name is the name this server calls itself, so a message says which
	// machine it came from in a household with more than one.
	//
	// A function rather than a string because the name is a setting somebody
	// can change from the admin interface without restarting, and a copy
	// taken at startup would be the old one for as long as the process lived.
	Name func() string

	// HTTP is the client deliveries are made with. Nil means one with a
	// sensible timeout.
	HTTP *http.Client

	Log *slog.Logger

	// Now is the clock, for tests.
	Now func() time.Time
}

// New starts a notifier and its worker.
func New(opts Options) *Notifier {
	client := opts.HTTP
	if client == nil {
		client = &http.Client{Timeout: attemptTimeout}
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	name := opts.Name
	if name == nil {
		name = func() string { return "" }
	}
	n := &Notifier{
		db: opts.DB, http: client, log: log, name: name, now: now,
		queue: make(chan Event, queueDepth),
		done:  make(chan struct{}),
	}
	go n.work()
	return n
}

// Send queues an event. Never blocks, and never returns an error: there is
// nothing the caller — a playback handler, a finished job — could do about a
// notification that did not go out.
func (n *Notifier) Send(event Event) {
	if n == nil || event.Kind == "" {
		return
	}
	if event.At.IsZero() {
		event.At = n.now().UTC()
	}

	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.closed {
		return
	}
	select {
	case n.queue <- event:
	default:
		n.log.Warn("webhook queue is full, dropping an event", "kind", event.Kind)
	}
}

// Close stops the worker once the queue has drained. Safe to call twice, and
// safe to race with a Send: an event offered after this returns is dropped.
func (n *Notifier) Close() {
	if n == nil {
		return
	}
	n.mu.Lock()
	if !n.closed {
		n.closed = true
		close(n.queue)
	}
	n.mu.Unlock()
	<-n.done
}

// work takes events in turn.
//
// One goroutine, so the order events were reported in is the order they are
// delivered in: an automation that turns a light on at playback.start and off
// at playback.stop must never be handed the pair the other way round.
func (n *Notifier) work() {
	defer close(n.done)
	for event := range n.queue {
		n.dispatch(context.Background(), event)
	}
}

// dispatch delivers one event to every destination that wants it, at once.
func (n *Notifier) dispatch(ctx context.Context, event Event) {
	destinations, err := n.db.Destinations(ctx)
	if err != nil {
		n.log.Error("could not read webhook destinations", "error", err)
		return
	}

	var wg sync.WaitGroup
	for _, dest := range destinations {
		if !Wants(dest.Events, event.Kind) {
			continue
		}
		wg.Add(1)
		go func(dest store.Destination) {
			defer wg.Done()
			status, err := n.deliver(ctx, dest, event)
			failure := ""
			if err != nil {
				failure = err.Error()
				n.log.Warn("webhook delivery failed",
					"url", dest.URL, "kind", event.Kind, "error", err)
			}
			if err := n.db.RecordDelivery(ctx, dest.ID, status, failure, n.now().UTC()); err != nil {
				n.log.Error("could not record a webhook delivery", "error", err)
			}
		}(dest)
	}
	wg.Wait()
}

// Deliver sends one event to one destination now, and reports what happened.
//
// Exported for the test button: an operator pressing it wants the answer on
// the page, not in a log file they would have to go and read.
func (n *Notifier) Deliver(ctx context.Context, dest store.Destination, event Event) (int, error) {
	if event.At.IsZero() {
		event.At = n.now().UTC()
	}
	status, err := n.deliver(ctx, dest, event)
	failure := ""
	if err != nil {
		failure = err.Error()
	}
	if recErr := n.db.RecordDelivery(ctx, dest.ID, status, failure, n.now().UTC()); recErr != nil {
		n.log.Error("could not record a webhook delivery", "error", recErr)
	}
	return status, err
}

// deliver makes the request, retrying once.
func (n *Notifier) deliver(ctx context.Context, dest store.Destination, event Event) (int, error) {
	body, headers, err := render(dest, event, n.name())
	if err != nil {
		return 0, err
	}

	var lastStatus int
	var lastErr error
	for attempt := range attempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return lastStatus, lastErr
			case <-time.After(retryAfter):
			}
		}
		status, err := n.attempt(ctx, dest, body, headers)
		if err == nil {
			return status, nil
		}
		lastStatus, lastErr = status, err

		// A refusal is an answer: the receiver is up and said no, and asking
		// it again changes nothing. Only a failure to reach it, or a fault
		// at its end, is worth a second try.
		if status >= 400 && status < 500 {
			break
		}
	}
	return lastStatus, lastErr
}

func (n *Notifier) attempt(ctx context.Context, dest store.Destination, body []byte, headers map[string]string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, attemptTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dest.URL, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("could not address %s: %w", dest.URL, err)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	if dest.Token != "" {
		req.Header.Set("Authorization", "Bearer "+dest.Token)
	}

	res, err := n.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("could not reach it: %w", err)
	}
	defer res.Body.Close()
	// Drained and discarded: a connection is only reusable once its body has
	// been read, and nothing here cares what the receiver had to say. Bounded,
	// because a receiver that answers with a gigabyte should not be able to
	// spend this server's memory on it.
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10))

	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return res.StatusCode, nil
	}
	return res.StatusCode, fmt.Errorf("it answered %s", res.Status)
}

// render builds the body and headers for one destination's format.
func render(dest store.Destination, event Event, server string) ([]byte, map[string]string, error) {
	if err := usable(dest.URL); err != nil {
		return nil, nil, err
	}
	switch dest.Format {
	case domain.WebhookNtfy:
		return renderNtfy(dest, event, server)
	case domain.WebhookJSON, "":
		return renderJSON(dest, event, server)
	default:
		return nil, nil, fmt.Errorf("%q is not a format this server can send", dest.Format)
	}
}

// usable refuses an address that cannot be posted to.
//
// Only the scheme and the host: a destination on the same machine or the same
// house is the normal case here — ntfy on loopback, Home Assistant on the LAN
// — so there is nothing to be gained by refusing a private address and a
// working setup to be broken by it.
func usable(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("that address cannot be read: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("a webhook address has to start with http:// or https://")
	}
	if parsed.Host == "" {
		return fmt.Errorf("that address has no host in it")
	}
	return nil
}

// payload is the JSON body. A flat object with the structured part nested,
// so a receiver can read the message without knowing any of the field names
// under it.
type payload struct {
	Event   string         `json:"event"`
	At      string         `json:"at"`
	Server  string         `json:"server,omitempty"`
	Title   string         `json:"title,omitempty"`
	Message string         `json:"message,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
}

func renderJSON(dest store.Destination, event Event, server string) ([]byte, map[string]string, error) {
	body, err := json.Marshal(payload{
		Event:   event.Kind,
		At:      event.At.UTC().Format(time.RFC3339),
		Server:  server,
		Title:   event.Title,
		Message: event.Text,
		Data:    event.Data,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("could not build the body: %w", err)
	}
	headers := map[string]string{
		"Content-Type":   "application/json; charset=utf-8",
		"User-Agent":     "bask-media-server",
		"X-Bask-Event":   event.Kind,
		"X-Bask-Deliver": deliveryID(),
	}
	if dest.Secret != "" {
		headers["X-Bask-Signature"] = sign(dest.Secret, body)
	}
	return body, headers, nil
}

// renderNtfy builds what a notification service expects: the message as the
// body, everything else in headers.
//
// The X- forms of the headers are used rather than the bare ones, because
// Title and Priority are plausible names for something else to have set on
// the way through, and ntfy understands both.
func renderNtfy(dest store.Destination, event Event, server string) ([]byte, map[string]string, error) {
	text := event.Text
	if text == "" {
		text = event.Title
	}
	headers := map[string]string{
		"Content-Type":   "text/plain; charset=utf-8",
		"User-Agent":     "bask-media-server",
		"X-Bask-Event":   event.Kind,
		"X-Bask-Deliver": deliveryID(),
		"X-Title":        headerText(title(event, server)),
		"X-Tags":         tagFor(event.Kind),
		"X-Priority":     priority(event),
	}
	if dest.Secret != "" {
		headers["X-Bask-Signature"] = sign(dest.Secret, []byte(text))
	}
	return []byte(text), headers, nil
}

// headerText makes a value safe to put in a header.
//
// An HTTP header is Latin-1 by the specification, and a title taken from a
// library is not: "Amélie", or any episode heading, carries an em dash. ntfy
// decodes RFC 2047 encoded words in X-Title, X-Message and X-Tags and leaves
// anything else alone, so an encoded word is the one form that survives both
// the specification and the service. Plain ASCII is left plain, because a
// header a person might read in a log should be readable.
func headerText(value string) string {
	ascii := true
	for i := 0; i < len(value); i++ {
		if value[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return value
	}
	return "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(value)) + "?="
}

func title(event Event, server string) string {
	if event.Title == "" {
		return server
	}
	if server == "" {
		return event.Title
	}
	// An ASCII separator on purpose: this string becomes a header, and a
	// middle dot would mean encoding every title that would otherwise have
	// gone over the wire readable.
	return event.Title + " - " + server
}

// tagFor is the emoji a notification carries, which is how a phone's
// notification shade becomes readable at a glance rather than a list of
// identical grey lines.
func tagFor(kind string) string {
	switch kind {
	case KindPlaybackStart, KindPlaybackResume:
		return "play_button"
	case KindPlaybackPause:
		return "pause_button"
	case KindPlaybackStop:
		return "stop_button"
	case KindLibraryNew:
		return "film_frames"
	case KindJobDone:
		return "white_check_mark"
	case KindJobFailed:
		return "rotating_light"
	default:
		return "bell"
	}
}

func priority(event Event) string {
	if event.Urgent {
		return "4"
	}
	return "3"
}

// sign is the proof that a body came from this server.
//
// A receiver on the open internet cannot otherwise tell a real event from
// anybody who learned the URL, and a URL travels: it goes in a config file, a
// screenshot, a support thread. The secret never does.
func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a signature the way a receiver should, and exists so the test
// for signing is a test of the thing a receiver will actually do.
func Verify(secret, signature string, body []byte) bool {
	return hmac.Equal([]byte(sign(secret, body)), []byte(signature))
}

func deliveryID() string {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(raw)
}

// Wants reports whether a destination subscribed to this kind.
//
// An empty subscription means everything, which is what a destination added
// without a thought gets: somebody adding a webhook wants to see it work, and
// a form they did not fill in should not produce a destination that silently
// receives nothing.
func Wants(subscribed []string, kind string) bool {
	if len(subscribed) == 0 {
		return true
	}
	for _, entry := range subscribed {
		entry = strings.TrimSpace(entry)
		switch {
		case entry == "" || entry == "*":
			return true
		case entry == kind:
			return true
		case strings.HasSuffix(entry, ".*") && strings.HasPrefix(kind, strings.TrimSuffix(entry, "*")):
			return true
		}
	}
	return false
}
