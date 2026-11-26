package admin_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/cloud"
	"github.com/inknironapps/bask-media-server/internal/cloudauth"
)

// Claiming a server is the administrator's one interaction with the account
// service, so the interface has to make the state plain: unsupported,
// unclaimed, or claimed, and who is waiting.

// accountService stands up the three routes a server calls, and reports what
// it was asked.
type accountService struct {
	server *httptest.Server

	token     string
	addresses []string
	auth      string
}

func newAccountService(t *testing.T) *accountService {
	t.Helper()
	public, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	fake := &accountService{token: "claim-abc"}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/bask-keys.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []cloudauth.Key{{
			ID: "k1", Algorithm: "EdDSA", Key: base64.RawURLEncoding.EncodeToString(public),
		}}})
	})
	mux.HandleFunc("POST /servers/claim", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Token string `json:"token"`
			Name  string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if body.Token != fake.token {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "that claim token is not good any more"})
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"server": map[string]any{"id": "server-1", "name": body.Name},
			"secret": "server-secret",
		})
	})
	mux.HandleFunc("PUT /servers/{id}/addresses", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Addresses []string `json:"addresses"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		fake.addresses = body.Addresses
		fake.auth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	})

	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

// withCloud gives the harness's interface an account-service client pointed at
// the fake, as the real server wires one.
func withCloud(t *testing.T, h *harness, fake *accountService) {
	t.Helper()
	h.panel.Cloud = &cloud.Service{Values: h.db, HTTP: fake.server.Client()}
}

func TestAServerWithNoAccountServiceSupportSaysSo(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)

	res, body := h.call(http.MethodGet, "/admin/api/cloud", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	// Unsupported and unclaimed are different, and only one of them has a
	// claim button on it.
	if body["supported"] != false {
		t.Fatalf("supported = %v, want false", body["supported"])
	}
	if body["linked"] != false {
		t.Fatalf("linked = %v, want false", body["linked"])
	}
}

func TestClaimingFromTheInterfaceStoresTheLink(t *testing.T) {
	h := newHarness(t)
	fake := newAccountService(t)
	withCloud(t, h, fake)
	h.signIn(h.adminToken)

	res, body := h.call(http.MethodPost, "/admin/api/cloud/claim", map[string]any{
		"issuer": fake.server.URL,
		"token":  "claim-abc",
		"name":   "The house",
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %v", res.StatusCode, body)
	}
	if body["serverId"] != "server-1" || body["name"] != "The house" {
		t.Fatalf("unexpected answer %v", body)
	}
	// The credential that speaks for this server is never in an answer the
	// admin interface serves.
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "server-secret") {
		t.Fatalf("the secret was served: %s", encoded)
	}

	_, status := h.call(http.MethodGet, "/admin/api/cloud", nil)
	if status["linked"] != true {
		t.Fatalf("linked = %v, want true", status["linked"])
	}
}

func TestARefusedClaimTellsTheAdministratorWhatTheServiceSaid(t *testing.T) {
	h := newHarness(t)
	fake := newAccountService(t)
	withCloud(t, h, fake)
	h.signIn(h.adminToken)

	res, body := h.call(http.MethodPost, "/admin/api/cloud/claim", map[string]any{
		"issuer": fake.server.URL,
		"token":  "claim-expired",
	})
	// 400 rather than 500: an expired claim token is something the
	// administrator fixes by minting another, not a fault in this server.
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, body %v", res.StatusCode, body)
	}
	message, _ := body["error"].(string)
	if !strings.Contains(message, "not good any more") {
		t.Fatalf("the service's message was lost: %q", message)
	}
}

func TestUnlinkingLeavesTheAccountsItMadeAlone(t *testing.T) {
	h := newHarness(t)
	fake := newAccountService(t)
	withCloud(t, h, fake)
	h.signIn(h.adminToken)

	if res, body := h.call(http.MethodPost, "/admin/api/cloud/claim", map[string]any{
		"issuer": fake.server.URL, "token": "claim-abc",
	}); res.StatusCode != http.StatusOK {
		t.Fatalf("claim: status %d, body %v", res.StatusCode, body)
	}
	guest, err := h.db.CreateCloudUser(context.Background(), "account-1", "Rae", "rae@example.com")
	if err != nil {
		t.Fatal(err)
	}

	if res, _ := h.call(http.MethodDelete, "/admin/api/cloud", nil); res.StatusCode != http.StatusNoContent {
		t.Fatalf("unlink: status %d", res.StatusCode)
	}

	// The account holds library grants an administrator made, and dropping it
	// would quietly undo that work. It simply cannot be authenticated as.
	if _, err := h.db.User(context.Background(), guest.ID); err != nil {
		t.Fatalf("unlinking removed a local account: %v", err)
	}
	_, status := h.call(http.MethodGet, "/admin/api/cloud", nil)
	if status["linked"] != false {
		t.Fatalf("linked = %v, want false", status["linked"])
	}
}

func TestTheInterfaceCountsWhoIsWaiting(t *testing.T) {
	h := newHarness(t)
	fake := newAccountService(t)
	withCloud(t, h, fake)
	h.signIn(h.adminToken)

	waiting, err := h.db.CreateCloudUser(context.Background(), "account-1", "Rae", "rae@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.CreateCloudUser(context.Background(), "account-2", "Sam", "sam@example.com"); err != nil {
		t.Fatal(err)
	}

	_, body := h.call(http.MethodGet, "/admin/api/cloud", nil)
	if body["waiting"] != float64(2) {
		t.Fatalf("waiting = %v, want 2", body["waiting"])
	}

	// Allowing one leaves one waiting, which is the number the page is for.
	if res, _ := h.call(http.MethodPatch, "/admin/api/users/"+itoa(waiting.ID), map[string]any{
		"active": true,
	}); res.StatusCode != http.StatusNoContent {
		t.Fatalf("activate: status %d", res.StatusCode)
	}
	_, body = h.call(http.MethodGet, "/admin/api/cloud", nil)
	if body["waiting"] != float64(1) {
		t.Fatalf("waiting = %v, want 1", body["waiting"])
	}
}

func TestTheUserListMarksACloudAccountAndWhetherItIsAllowed(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	if _, err := h.db.CreateCloudUser(context.Background(), "account-1", "Rae", "rae@example.com"); err != nil {
		t.Fatal(err)
	}

	var cloudRow, localRow map[string]any
	for _, row := range h.list("/admin/api/users") {
		if row["fromCloud"] == true {
			cloudRow = row
			continue
		}
		if row["username"] == "admin" {
			localRow = row
		}
	}
	if cloudRow == nil {
		t.Fatal("the cloud account is not marked as one")
	}
	// The pair the page needs: this one came from the service, and it is not
	// allowed yet.
	if cloudRow["active"] != false {
		t.Fatalf("active = %v, want false", cloudRow["active"])
	}
	// And an ordinary account is unaffected: making it was the approval.
	if localRow == nil || localRow["active"] != true {
		t.Fatalf("a local account is not active: %v", localRow)
	}
}

func TestPublishingAddressesBeforeClaimingIsRefused(t *testing.T) {
	h := newHarness(t)
	fake := newAccountService(t)
	withCloud(t, h, fake)
	h.signIn(h.adminToken)

	res, _ := h.call(http.MethodPut, "/admin/api/cloud/addresses", map[string]any{
		"addresses": []string{"https://bask.example.com"},
	})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("status %d, want 409", res.StatusCode)
	}
}

func TestPublishedAddressesReachTheService(t *testing.T) {
	h := newHarness(t)
	fake := newAccountService(t)
	withCloud(t, h, fake)
	h.signIn(h.adminToken)
	if res, body := h.call(http.MethodPost, "/admin/api/cloud/claim", map[string]any{
		"issuer": fake.server.URL, "token": "claim-abc",
	}); res.StatusCode != http.StatusOK {
		t.Fatalf("claim: status %d, body %v", res.StatusCode, body)
	}

	res, _ := h.call(http.MethodPut, "/admin/api/cloud/addresses", map[string]any{
		"addresses": []string{"https://bask.example.com", "http://192.168.1.10:32400"},
	})
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", res.StatusCode)
	}
	if fake.auth != "Bearer server-secret" {
		t.Fatalf("authorization = %q", fake.auth)
	}
	if len(fake.addresses) != 2 {
		t.Fatalf("addresses = %v", fake.addresses)
	}
}

func TestOnlyASignedInAdministratorCanClaim(t *testing.T) {
	h := newHarness(t)
	fake := newAccountService(t)
	withCloud(t, h, fake)

	// No sign-in: claiming binds this server to somebody's account, which is
	// not something a passer-by does.
	res, _ := h.call(http.MethodPost, "/admin/api/cloud/claim", map[string]any{
		"issuer": fake.server.URL, "token": "claim-abc",
	})
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", res.StatusCode)
	}
}
