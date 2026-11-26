package plexapi_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/cloud"
	"github.com/inknironapps/bask-media-server/internal/cloudauth"
	"github.com/inknironapps/bask-media-server/internal/plexapi"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// An assertion is a second way in to somebody's library, so what it does and
// does not grant is worth as much care as the token path it sits beside.

type cloudHarness struct {
	server   *plexapi.Server
	db       *store.DB
	token    string
	private  ed25519.PrivateKey
	issuer   string
	serverID string
	now      time.Time
	jti      int
}

// linkedLibrary is the ordinary test library with an account service in front
// of it, claimed and with its key pinned.
func linkedLibrary(t *testing.T) *cloudHarness {
	t.Helper()
	srv, token, db := library(t)

	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/bask-keys.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []cloudauth.Key{{
			ID: "k1", Algorithm: "EdDSA", Key: base64.RawURLEncoding.EncodeToString(public),
		}}})
	}))
	t.Cleanup(service.Close)

	accounts := &cloud.Service{Values: db, HTTP: service.Client(), Now: func() time.Time { return now }}
	link := map[string]any{
		"issuer":   service.URL,
		"serverId": "server-1",
		"secret":   "server-secret",
		"name":     "The house",
		"keys": []cloudauth.Key{{
			ID: "k1", Algorithm: "EdDSA", Key: base64.RawURLEncoding.EncodeToString(public),
		}},
	}
	encoded, err := json.Marshal(link)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetServerValue(context.Background(), cloud.LinkKey, string(encoded)); err != nil {
		t.Fatal(err)
	}
	srv.Cloud = accounts

	return &cloudHarness{
		server: srv, db: db, token: token, private: private,
		issuer: service.URL, serverID: "server-1", now: now,
	}
}

// assertion signs one the way the service does. Each carries its own id, so a
// test that makes two requests is not accidentally testing replay.
func (h *cloudHarness) assertion(t *testing.T, overrides map[string]any) string {
	t.Helper()
	h.jti++
	claims := map[string]any{
		"iss":   h.issuer,
		"sub":   "account-1",
		"aud":   h.serverID,
		"email": "rae@example.com",
		"name":  "Rae",
		"iat":   h.now.Unix(),
		"exp":   h.now.Add(5 * time.Minute).Unix(),
		"jti":   fmt.Sprintf("assertion-%d-%d", time.Now().UnixNano(), h.jti),
	}
	for key, value := range overrides {
		claims[key] = value
	}
	encode := func(value any) string {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	body := encode(map[string]any{"alg": "EdDSA", "typ": "bask+jws", "kid": "k1"}) + "." + encode(claims)
	return body + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(h.private, []byte(body)))
}

// ask makes a request presenting whatever is given, and reports the status
// rather than failing on a refusal: the refusals are the point here.
func (h *cloudHarness) ask(path string, header, value string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept", "application/json")
	if value != "" {
		req.Header.Set(header, value)
	}
	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAFirstAssertionCreatesAnAccountThatCannotYetWatchAnything(t *testing.T) {
	h := linkedLibrary(t)

	got := h.ask("/library/sections", plexapi.AssertionHeader, h.assertion(t, nil))
	// 403, not 401: the identity is good and signing in again would not help.
	// A client told 401 would loop asking the service for another assertion.
	if got.Code != http.StatusForbidden {
		t.Fatalf("status %d, body %s", got.Code, got.Body.String())
	}

	users, err := h.db.Users(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, user := range users {
		if user.CloudSubject != "account-1" {
			continue
		}
		found = true
		// The account exists so the administrator has something to grant
		// libraries to, and it arrives with nothing: never an administrator,
		// never active.
		if user.Admin {
			t.Error("a cloud identity arrived as an administrator")
		}
		if user.Active {
			t.Error("a cloud identity arrived able to watch")
		}
		if user.Username != "Rae" {
			t.Errorf("username = %q, want the name off the assertion", user.Username)
		}
	}
	if !found {
		t.Fatal("no account was created for the subject")
	}
}

func TestAnApprovedCloudAccountReachesTheLibrary(t *testing.T) {
	h := linkedLibrary(t)
	if got := h.ask("/library/sections", plexapi.AssertionHeader, h.assertion(t, nil)); got.Code != http.StatusForbidden {
		t.Fatalf("first request: status %d", got.Code)
	}

	user, err := h.db.UserForCloudSubject(context.Background(), "account-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetUserActive(context.Background(), user.ID, true); err != nil {
		t.Fatal(err)
	}

	got := h.ask("/library/sections", plexapi.AssertionHeader, h.assertion(t, nil))
	if got.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", got.Code, got.Body.String())
	}
}

func TestAnAssertionIsAcceptedInTheTokenHeaderToo(t *testing.T) {
	h := linkedLibrary(t)
	if got := h.ask("/library/sections", plexapi.AssertionHeader, h.assertion(t, nil)); got.Code != http.StatusForbidden {
		t.Fatalf("priming request: status %d", got.Code)
	}
	user, err := h.db.UserForCloudSubject(context.Background(), "account-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetUserActive(context.Background(), user.ID, true); err != nil {
		t.Fatal(err)
	}

	// A client that speaks this dialect sends X-Plex-Token everywhere,
	// including in the query string of a stream URL, so an assertion has to
	// work there or half the surface is unreachable with one.
	got := h.ask("/library/sections", "X-Plex-Token", h.assertion(t, nil))
	if got.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", got.Code, got.Body.String())
	}
}

func TestAnOrdinaryTokenStillWorksOnALinkedServer(t *testing.T) {
	h := linkedLibrary(t)
	// The whole point of the shape check: a token with no dots in it must not
	// be put through the verifier, and an account service must not change
	// what an existing client can do.
	got := h.ask("/library/sections", "X-Plex-Token", h.token)
	if got.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", got.Code, got.Body.String())
	}
}

func TestAnAssertionForAnotherServerReachesNothingHere(t *testing.T) {
	h := linkedLibrary(t)
	got := h.ask("/library/sections", plexapi.AssertionHeader,
		h.assertion(t, map[string]any{"aud": "somebody-elses-server"}))
	if got.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, body %s", got.Code, got.Body.String())
	}
	users, err := h.db.Users(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range users {
		if user.FromCloud() {
			t.Fatalf("an unverified assertion created an account: %+v", user)
		}
	}
}

func TestATamperedAssertionReachesNothing(t *testing.T) {
	h := linkedLibrary(t)
	signed := h.assertion(t, nil)
	tampered := signed[:len(signed)-2] + "xy"

	if got := h.ask("/library/sections", plexapi.AssertionHeader, tampered); got.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, body %s", got.Code, got.Body.String())
	}
}

func TestTheSameAssertionCannotBeUsedTwice(t *testing.T) {
	h := linkedLibrary(t)
	if got := h.ask("/library/sections", plexapi.AssertionHeader, h.assertion(t, nil)); got.Code != http.StatusForbidden {
		t.Fatalf("priming request: status %d", got.Code)
	}
	user, err := h.db.UserForCloudSubject(context.Background(), "account-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetUserActive(context.Background(), user.ID, true); err != nil {
		t.Fatal(err)
	}

	reused := h.assertion(t, nil)
	if got := h.ask("/library/sections", plexapi.AssertionHeader, reused); got.Code != http.StatusOK {
		t.Fatalf("first use: status %d, body %s", got.Code, got.Body.String())
	}
	// Replay inside the five minutes an assertion lives is otherwise free.
	if got := h.ask("/library/sections", plexapi.AssertionHeader, reused); got.Code != http.StatusUnauthorized {
		t.Fatalf("second use: status %d, body %s", got.Code, got.Body.String())
	}
}

func TestAnUnclaimedServerIgnoresAssertionsEntirely(t *testing.T) {
	srv, token, _ := library(t)
	req := httptest.NewRequest(http.MethodGet, "/library/sections", nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set(plexapi.AssertionHeader, "a.b.c")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	// Unauthorized rather than anything cleverer, and the token path is
	// untouched: a server nobody claimed works exactly as it always has.
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", rec.Code)
	}

	ok := httptest.NewRequest(http.MethodGet, "/library/sections", nil)
	ok.Header.Set("Accept", "application/json")
	ok.Header.Set("X-Plex-Token", token)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, ok)
	if rec.Code != http.StatusOK {
		t.Fatalf("the token path broke on an unclaimed server: %d", rec.Code)
	}
}

func TestASuspendedAccountsOwnTokenStopsWorking(t *testing.T) {
	srv, token, db := library(t)
	user, err := db.UserForToken(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetUserActive(context.Background(), user.ID, false); err != nil {
		t.Fatal(err)
	}

	// Suspending somebody and leaving their television working would not be
	// suspending them.
	req := httptest.NewRequest(http.MethodGet, "/library/sections", nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestACloudAccountSeesOnlyTheLibrariesItWasGranted(t *testing.T) {
	h := linkedLibrary(t)
	if got := h.ask("/library/sections", plexapi.AssertionHeader, h.assertion(t, nil)); got.Code != http.StatusForbidden {
		t.Fatalf("priming request: status %d", got.Code)
	}
	user, err := h.db.UserForCloudSubject(context.Background(), "account-1")
	if err != nil {
		t.Fatal(err)
	}
	sections, err := h.db.Sections(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) < 2 {
		t.Fatalf("expected two libraries, got %d", len(sections))
	}
	// Granted one of the two: a cloud identity reaches what the administrator
	// gave its account and not a library more.
	if err := h.db.SetUserSections(context.Background(), user.ID, []int64{sections[0].ID}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetUserActive(context.Background(), user.ID, true); err != nil {
		t.Fatal(err)
	}

	got := h.ask("/library/sections", plexapi.AssertionHeader, h.assertion(t, nil))
	if got.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", got.Code, got.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	container, _ := body["MediaContainer"].(map[string]any)
	listed, _ := container["Directory"].([]any)
	if len(listed) != 1 {
		t.Fatalf("listed %d libraries, want 1: %s", len(listed), got.Body.String())
	}
}
