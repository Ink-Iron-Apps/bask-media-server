package admin

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
	"github.com/inknironapps/bask-media-server/internal/webhook"
)

// Webhooks, in the interface.
//
// The page never shows a token or a secret, only whether one is set. They are
// credentials for somebody else's service, and the admin page is the one place
// in this server where a person is looking at a screen that might be shared,
// screenshotted, or screen-recorded during a support conversation.

// webhookView is one destination as the page sees it.
type webhookView struct {
	ID        int64    `json:"id"`
	URL       string   `json:"url"`
	Format    string   `json:"format"`
	Events    []string `json:"events"`
	Enabled   bool     `json:"enabled"`
	HasToken  bool     `json:"hasToken"`
	HasSecret bool     `json:"hasSecret"`
	Created   string   `json:"created"`

	// How the last delivery went, so a destination that stopped working can
	// be seen to have stopped.
	LastAt     string `json:"lastAt,omitempty"`
	LastStatus int    `json:"lastStatus,omitempty"`
	LastError  string `json:"lastError,omitempty"`
	Failures   int    `json:"failures,omitempty"`
}

func viewWebhook(hook domain.Webhook) webhookView {
	view := webhookView{
		ID: hook.ID, URL: hook.URL, Format: string(hook.Format),
		Events: hook.Events, Enabled: hook.Enabled,
		HasToken: hook.HasToken, HasSecret: hook.HasSecret,
		LastStatus: hook.LastStatus, LastError: hook.LastError, Failures: hook.Failures,
	}
	if view.Events == nil {
		view.Events = []string{}
	}
	if !hook.CreatedAt.IsZero() {
		view.Created = hook.CreatedAt.Format(time.RFC3339)
	}
	if !hook.LastAt.IsZero() {
		view.LastAt = hook.LastAt.Format(time.RFC3339)
	}
	return view
}

// listWebhooks is every destination, with the event kinds there are to
// subscribe to so the page does not keep its own copy of that list.
func (s *Server) listWebhooks(w http.ResponseWriter, r *http.Request) {
	hooks, err := s.DB.Webhooks(r.Context())
	if err != nil {
		s.fail(w, "list webhooks", err)
		return
	}
	out := make([]webhookView, 0, len(hooks))
	for _, hook := range hooks {
		out = append(out, viewWebhook(hook))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"webhooks": out,
		"kinds":    webhook.Kinds,
		"families": webhook.Families,
		"sending":  s.Notifier != nil,
	})
}

// webhookBody is what the page sends. Pointers so that leaving a field out
// and clearing it are different requests, which is what a form that cannot
// show the current token needs.
type webhookBody struct {
	URL     *string   `json:"url"`
	Format  *string   `json:"format"`
	Events  *[]string `json:"events"`
	Token   *string   `json:"token"`
	Secret  *string   `json:"secret"`
	Enabled *bool     `json:"enabled"`
}

// createWebhook adds a destination.
func (s *Server) createWebhook(w http.ResponseWriter, r *http.Request) {
	var body webhookBody
	if !decode(w, r, &body) {
		return
	}
	if body.URL == nil || strings.TrimSpace(*body.URL) == "" {
		writeError(w, http.StatusBadRequest, "a webhook needs an address")
		return
	}
	format := domain.WebhookJSON
	if body.Format != nil {
		format = domain.WebhookFormat(strings.TrimSpace(*body.Format))
	}
	if !format.Valid() {
		writeError(w, http.StatusBadRequest, "a webhook is sent either as json or as ntfy")
		return
	}
	events, ok := cleanEvents(w, body.Events)
	if !ok {
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}

	hook, err := s.DB.AddWebhook(r.Context(), store.NewWebhook{
		URL: *body.URL, Format: format, Events: events,
		Token: value(body.Token), Secret: value(body.Secret), Enabled: enabled,
	})
	if err != nil {
		s.fail(w, "add webhook", err)
		return
	}
	writeJSON(w, http.StatusCreated, viewWebhook(*hook))
}

// updateWebhook changes one.
func (s *Server) updateWebhook(w http.ResponseWriter, r *http.Request) {
	id, ok := webhookID(w, r)
	if !ok {
		return
	}
	var body webhookBody
	if !decode(w, r, &body) {
		return
	}

	edit := store.WebhookEdit{URL: body.URL, Token: body.Token, Secret: body.Secret, Enabled: body.Enabled}
	if body.Format != nil {
		format := domain.WebhookFormat(strings.TrimSpace(*body.Format))
		if !format.Valid() {
			writeError(w, http.StatusBadRequest, "a webhook is sent either as json or as ntfy")
			return
		}
		edit.Format = &format
	}
	if body.Events != nil {
		events, ok := cleanEvents(w, body.Events)
		if !ok {
			return
		}
		edit.Events = &events
	}

	hook, err := s.DB.EditWebhook(r.Context(), id, edit)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such webhook")
		return
	}
	if err != nil {
		s.fail(w, "edit webhook", err)
		return
	}
	writeJSON(w, http.StatusOK, viewWebhook(*hook))
}

// deleteWebhook forgets one.
func (s *Server) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	id, ok := webhookID(w, r)
	if !ok {
		return
	}
	err := s.DB.DeleteWebhook(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such webhook")
		return
	}
	if err != nil {
		s.fail(w, "delete webhook", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// testWebhook sends one event now and says what came back.
//
// Delivered in the request rather than queued, because the whole point is the
// answer: somebody pasting a URL wants to know on this page whether it works,
// not to go and read a log.
func (s *Server) testWebhook(w http.ResponseWriter, r *http.Request) {
	id, ok := webhookID(w, r)
	if !ok {
		return
	}
	if s.Notifier == nil {
		writeError(w, http.StatusServiceUnavailable, "this server is not set up to send webhooks")
		return
	}
	dest, err := s.DB.Destination(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such webhook")
		return
	}
	if err != nil {
		s.fail(w, "read webhook", err)
		return
	}

	status, err := s.Notifier.Deliver(r.Context(), *dest, webhook.Test(s.friendlyName(r)))
	out := map[string]any{"status": status, "delivered": err == nil}
	if err != nil {
		// Not a failure of this request: the request worked and the answer is
		// that the destination did not. A 500 here would be the page telling
		// somebody that the admin interface is broken.
		out["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

// cleanEvents keeps only kinds and families this server actually sends.
//
// A typed subscription that matches nothing is the worst outcome here: the
// page would show a destination, the destination would be enabled, and
// nothing would ever arrive. Refusing the typo is how somebody finds out.
func cleanEvents(w http.ResponseWriter, in *[]string) ([]string, bool) {
	if in == nil {
		return nil, true
	}
	out := make([]string, 0, len(*in))
	for _, raw := range *in {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if !knownEvent(entry) {
			writeError(w, http.StatusBadRequest, "this server does not send an event called "+entry)
			return nil, false
		}
		out = append(out, entry)
	}
	return out, true
}

func knownEvent(entry string) bool {
	if entry == "*" {
		return true
	}
	for _, kind := range webhook.Kinds {
		if entry == kind {
			return true
		}
	}
	for _, family := range webhook.Families {
		if entry == family+".*" {
			return true
		}
	}
	return false
}

func webhookID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "that is not a webhook id")
		return 0, false
	}
	return id, true
}

// value is a pointer's string, or empty where there was none.
func value(in *string) string {
	if in == nil {
		return ""
	}
	return *in
}
