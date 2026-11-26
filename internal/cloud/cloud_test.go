package cloud_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/cloud"
	"github.com/inknironapps/bask-media-server/internal/cloudauth"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// values is the server table, which is all this package needs of the store.
type values struct {
	mu   sync.Mutex
	rows map[string]string
}

func newValues() *values { return &values{rows: map[string]string{}} }

func (v *values) ServerValue(_ context.Context, key string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.rows[key], nil
}

func (v *values) SetServerValue(_ context.Context, key, value string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.rows[key] = value
	return nil
}

type signer struct {
	kid     string
	public  ed25519.PublicKey
	private ed25519.PrivateKey
}

func newSigner(t *testing.T, kid string) signer {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return signer{kid: kid, public: public, private: private}
}

func (s signer) key() cloudauth.Key {
	return cloudauth.Key{ID: s.kid, Algorithm: "EdDSA", Key: base64.RawURLEncoding.EncodeToString(s.public)}
}

func (s signer) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	encode := func(value any) string {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	body := encode(map[string]any{"alg": "EdDSA", "typ": "bask+jws", "kid": s.kid}) + "." + encode(claims)
	return body + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.private, []byte(body)))
}

// service stands in for the Worker: the three routes a server ever calls.
type service struct {
	t *testing.T

	mu           sync.Mutex
	keys         []cloudauth.Key
	claimToken   string
	serverID     string
	secret       string
	keyFetches   int
	addresses    []string
	addressAuth  string
	claimRefusal string
}

func (f *service) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/bask-keys.json", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.keyFetches++
		keys := f.keys
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
	})
	mux.HandleFunc("POST /servers/claim", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		refusal, expected, id, secret := f.claimRefusal, f.claimToken, f.serverID, f.secret
		f.mu.Unlock()
		if refusal != "" {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": refusal})
			return
		}
		var body struct {
			Token string `json:"token"`
			Name  string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "not json"})
			return
		}
		if body.Token != expected {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "that claim token is not good any more"})
			return
		}
		name := body.Name
		if name == "" {
			name = "Bask Media Server"
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"server": map[string]any{"id": id, "name": name},
			"secret": secret,
		})
	})
	mux.HandleFunc("PUT /servers/{id}/addresses", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Addresses []string `json:"addresses"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "not json"})
			return
		}
		f.mu.Lock()
		f.addresses = body.Addresses
		f.addressAuth = r.Header.Get("Authorization")
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

type harness struct {
	service *service
	server  *httptest.Server
	values  *values
	cloud   *cloud.Service
	now     time.Time
}

func newHarness(t *testing.T, keys ...cloudauth.Key) *harness {
	t.Helper()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	fake := &service{t: t, keys: keys, claimToken: "claim-abc", serverID: "server-1", secret: "server-secret"}
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)
	rows := newValues()
	return &harness{
		service: fake,
		server:  server,
		values:  rows,
		now:     now,
		cloud: &cloud.Service{
			Values: rows,
			HTTP:   server.Client(),
			Now:    func() time.Time { return now },
		},
	}
}

func TestAnUnclaimedServerReportsNoLinkAndVerifiesNothing(t *testing.T) {
	h := newHarness(t)

	link, err := h.cloud.Link(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if link != nil {
		t.Fatalf("expected no link, got %+v", link)
	}
	// Not a bad-assertion error: a server with no account service and a server
	// handed a forged assertion are different situations, and a caller that
	// confused them would tell the viewer the wrong thing.
	if _, err := h.cloud.Verify(context.Background(), "a.b.c"); !errors.Is(err, cloud.ErrNotLinked) {
		t.Fatalf("expected ErrNotLinked, got %v", err)
	}
}

func TestClaimingStoresTheLinkAndPinsTheKeys(t *testing.T) {
	signing := newSigner(t, "k1")
	h := newHarness(t, signing.key())

	link, err := h.cloud.Claim(context.Background(), h.server.URL, "claim-abc", "The house")
	if err != nil {
		t.Fatal(err)
	}
	if link.ServerID != "server-1" || link.Secret != "server-secret" || link.Name != "The house" {
		t.Fatalf("unexpected link %+v", link)
	}
	if len(link.Keys) != 1 || link.Keys[0].ID != "k1" {
		t.Fatalf("keys were not pinned: %+v", link.Keys)
	}
	if !link.ClaimedAt.Equal(h.now) {
		t.Fatalf("claimedAt = %v, want %v", link.ClaimedAt, h.now)
	}

	// Stored, not just held: a restart has to come back claimed.
	raw, err := h.values.ServerValue(context.Background(), cloud.LinkKey)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, "server-secret") {
		t.Fatalf("the link was not stored: %q", raw)
	}

	fresh := &cloud.Service{Values: h.values, HTTP: h.server.Client(), Now: func() time.Time { return h.now }}
	reloaded, err := fresh.Link(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded == nil || reloaded.ServerID != "server-1" {
		t.Fatalf("the link did not survive a restart: %+v", reloaded)
	}
}

func TestTheSecretIsNotInWhatTheAdminInterfaceSees(t *testing.T) {
	signing := newSigner(t, "k1")
	h := newHarness(t, signing.key())
	link, err := h.cloud.Claim(context.Background(), h.server.URL, "claim-abc", "The house")
	if err != nil {
		t.Fatal(err)
	}

	encoded, err := json.Marshal(link.Public())
	if err != nil {
		t.Fatal(err)
	}
	// The admin page is served over whatever the operator exposed it on, and
	// this secret speaks for the server. It is never part of an answer.
	if strings.Contains(string(encoded), "server-secret") {
		t.Fatalf("the secret reached the public view: %s", encoded)
	}
	if !strings.Contains(string(encoded), "server-1") {
		t.Fatalf("the public view is missing the server id: %s", encoded)
	}
}

func TestAClaimThatCannotFetchKeysLeavesTheServerUnclaimed(t *testing.T) {
	// No keys published: a link with a secret and no keys would look claimed
	// in the admin interface while verifying nothing a client presented.
	h := newHarness(t)

	if _, err := h.cloud.Claim(context.Background(), h.server.URL, "claim-abc", "The house"); err == nil {
		t.Fatal("expected the claim to fail")
	}
	stored, err := h.values.ServerValue(context.Background(), cloud.LinkKey)
	if err != nil {
		t.Fatal(err)
	}
	if stored != "" {
		t.Fatalf("a failed claim stored something: %q", stored)
	}
}

func TestARefusedClaimCarriesTheServicesOwnWords(t *testing.T) {
	signing := newSigner(t, "k1")
	h := newHarness(t, signing.key())

	_, err := h.cloud.Claim(context.Background(), h.server.URL, "claim-wrong", "The house")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	// What the administrator reads. "HTTP 404" would send them looking for a
	// networking problem they do not have.
	if !strings.Contains(err.Error(), "not good any more") {
		t.Fatalf("the service's message was lost: %v", err)
	}
}

func TestAnIssuerIsReducedToWhatAnAssertionWillSay(t *testing.T) {
	signing := newSigner(t, "k1")
	h := newHarness(t, signing.key())

	// A trailing slash and a path: an issuer that kept either would never
	// match the `iss` the service signs, and every assertion would be refused
	// for a reason nobody could see.
	link, err := h.cloud.Claim(context.Background(), h.server.URL+"/claim/", "claim-abc", "The house")
	if err != nil {
		t.Fatal(err)
	}
	if link.Issuer != h.server.URL {
		t.Fatalf("issuer = %q, want %q", link.Issuer, h.server.URL)
	}
}

func TestAnAddressWithNoSchemeIsRefusedBeforeAnythingIsCalled(t *testing.T) {
	h := newHarness(t)
	for _, address := range []string{"", "account.example", "ftp://account.example"} {
		if _, err := h.cloud.Claim(context.Background(), address, "claim-abc", ""); err == nil {
			t.Fatalf("expected %q to be refused", address)
		}
	}
	h.service.mu.Lock()
	defer h.service.mu.Unlock()
	if h.service.keyFetches != 0 {
		t.Fatalf("a refused address still called the service %d times", h.service.keyFetches)
	}
}

func claims(now time.Time, issuer, audience string, overrides map[string]any) map[string]any {
	out := map[string]any{
		"iss":   issuer,
		"sub":   "account-1",
		"aud":   audience,
		"email": "rae@example.com",
		"name":  "Rae",
		"iat":   now.Unix(),
		"exp":   now.Add(5 * time.Minute).Unix(),
		"jti":   "one",
	}
	for key, value := range overrides {
		out[key] = value
	}
	return out
}

func TestAClaimedServerVerifiesWhatTheServiceSigned(t *testing.T) {
	signing := newSigner(t, "k1")
	h := newHarness(t, signing.key())
	if _, err := h.cloud.Claim(context.Background(), h.server.URL, "claim-abc", "The house"); err != nil {
		t.Fatal(err)
	}

	verified, err := h.cloud.Verify(context.Background(),
		signing.sign(t, claims(h.now, h.server.URL, "server-1", nil)))
	if err != nil {
		t.Fatal(err)
	}
	if verified.Subject != "account-1" || verified.Email != "rae@example.com" {
		t.Fatalf("unexpected claims %+v", verified)
	}

	h.service.mu.Lock()
	fetches := h.service.keyFetches
	h.service.mu.Unlock()
	// One fetch, at claim time. A key it already holds is no reason to go
	// back, so an unreachable service cannot stop this server working.
	if fetches != 1 {
		t.Fatalf("keys were fetched %d times, want 1", fetches)
	}
}

func TestAnAssertionForAnotherServerIsRefused(t *testing.T) {
	signing := newSigner(t, "k1")
	h := newHarness(t, signing.key())
	if _, err := h.cloud.Claim(context.Background(), h.server.URL, "claim-abc", "The house"); err != nil {
		t.Fatal(err)
	}

	// The check that makes a stolen assertion worthless anywhere but the
	// server it was issued for.
	_, err := h.cloud.Verify(context.Background(),
		signing.sign(t, claims(h.now, h.server.URL, "somebody-elses-server", nil)))
	if !errors.Is(err, cloudauth.ErrNotVerified) {
		t.Fatalf("expected a refusal, got %v", err)
	}
}

func TestAnUnknownKeyIsFetchedOnceAndThenTrusted(t *testing.T) {
	first := newSigner(t, "k1")
	h := newHarness(t, first.key())
	if _, err := h.cloud.Claim(context.Background(), h.server.URL, "claim-abc", "The house"); err != nil {
		t.Fatal(err)
	}

	// Rotation: the service publishes the new key alongside the old one, and
	// a server meets the new kid before it has ever seen it.
	rotated := newSigner(t, "k2")
	h.service.mu.Lock()
	h.service.keys = []cloudauth.Key{first.key(), rotated.key()}
	h.service.mu.Unlock()

	for _, jti := range []string{"one", "two"} {
		if _, err := h.cloud.Verify(context.Background(),
			rotated.sign(t, claims(h.now, h.server.URL, "server-1", map[string]any{"jti": jti}))); err != nil {
			t.Fatalf("assertion %s: %v", jti, err)
		}
	}

	h.service.mu.Lock()
	fetches := h.service.keyFetches
	h.service.mu.Unlock()
	// Two: the claim, and the one refresh the unknown kid caused. The second
	// assertion uses the pinned key.
	if fetches != 2 {
		t.Fatalf("keys were fetched %d times, want 2", fetches)
	}

	// Pinned, so the next restart does not have to ask again.
	stored, err := h.cloud.Link(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Keys) != 2 {
		t.Fatalf("the refreshed keys were not stored: %+v", stored.Keys)
	}
}

func TestAnUnknownKeyTheServiceNeverPublishesIsStillRefused(t *testing.T) {
	known := newSigner(t, "k1")
	h := newHarness(t, known.key())
	if _, err := h.cloud.Claim(context.Background(), h.server.URL, "claim-abc", "The house"); err != nil {
		t.Fatal(err)
	}

	stranger := newSigner(t, "not-a-real-key")
	_, err := h.cloud.Verify(context.Background(),
		stranger.sign(t, claims(h.now, h.server.URL, "server-1", nil)))
	if !errors.Is(err, cloudauth.ErrNotVerified) {
		t.Fatalf("expected a refusal, got %v", err)
	}
	// The refresh happened and found nothing, and the old key is still there.
	stored, err := h.cloud.Link(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Keys) != 1 || stored.Keys[0].ID != "k1" {
		t.Fatalf("a failed lookup disturbed the pinned keys: %+v", stored.Keys)
	}
}

func TestAnUnreachableServiceDoesNotStopVerifyingAKnownKey(t *testing.T) {
	signing := newSigner(t, "k1")
	h := newHarness(t, signing.key())
	if _, err := h.cloud.Claim(context.Background(), h.server.URL, "claim-abc", "The house"); err != nil {
		t.Fatal(err)
	}
	h.server.Close()

	if _, err := h.cloud.Verify(context.Background(),
		signing.sign(t, claims(h.now, h.server.URL, "server-1", nil))); err != nil {
		t.Fatalf("a known key stopped working when the service went away: %v", err)
	}
}

func TestAddressesArePublishedWithTheServersOwnSecret(t *testing.T) {
	signing := newSigner(t, "k1")
	h := newHarness(t, signing.key())
	if _, err := h.cloud.Claim(context.Background(), h.server.URL, "claim-abc", "The house"); err != nil {
		t.Fatal(err)
	}

	err := h.cloud.PublishAddresses(context.Background(), []string{
		"https://bask.example.com", "  ", "http://192.168.1.10:32400",
	})
	if err != nil {
		t.Fatal(err)
	}

	h.service.mu.Lock()
	defer h.service.mu.Unlock()
	if got := h.service.addressAuth; got != "Bearer server-secret" {
		t.Fatalf("authorization = %q", got)
	}
	// Blank entries dropped: a form with an empty row must not publish one.
	want := []string{"https://bask.example.com", "http://192.168.1.10:32400"}
	if len(h.service.addresses) != len(want) {
		t.Fatalf("addresses = %v, want %v", h.service.addresses, want)
	}
	for i, address := range want {
		if h.service.addresses[i] != address {
			t.Fatalf("addresses = %v, want %v", h.service.addresses, want)
		}
	}
}

func TestPublishingAddressesWithNoLinkIsRefusedWithoutACall(t *testing.T) {
	h := newHarness(t)
	if err := h.cloud.PublishAddresses(context.Background(), []string{"https://bask.example.com"}); !errors.Is(err, cloud.ErrNotLinked) {
		t.Fatalf("expected ErrNotLinked, got %v", err)
	}
}

func TestUnlinkingForgetsTheLinkAndStopsVerifying(t *testing.T) {
	signing := newSigner(t, "k1")
	h := newHarness(t, signing.key())
	if _, err := h.cloud.Claim(context.Background(), h.server.URL, "claim-abc", "The house"); err != nil {
		t.Fatal(err)
	}

	if err := h.cloud.Unlink(context.Background()); err != nil {
		t.Fatal(err)
	}
	link, err := h.cloud.Link(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if link != nil {
		t.Fatalf("the link survived unlinking: %+v", link)
	}
	_, err = h.cloud.Verify(context.Background(), signing.sign(t, claims(h.now, h.server.URL, "server-1", nil)))
	if !errors.Is(err, cloud.ErrNotLinked) {
		t.Fatalf("expected ErrNotLinked, got %v", err)
	}
}

func TestAnUnreadableStoredLinkLeavesTheLibraryServing(t *testing.T) {
	h := newHarness(t)
	if err := h.values.SetServerValue(context.Background(), cloud.LinkKey, "{not json"); err != nil {
		t.Fatal(err)
	}

	// Unclaimed rather than fatal: the library is the point, and a corrupted
	// link is something the administrator can fix by claiming again.
	link, err := h.cloud.Link(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if link != nil {
		t.Fatalf("expected no link, got %+v", link)
	}
}
