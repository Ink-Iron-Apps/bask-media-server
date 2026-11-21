package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/inknironapps/bask-media-server/internal/admin"
	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/jobs"
	"github.com/inknironapps/bask-media-server/internal/plexapi"
	"github.com/inknironapps/bask-media-server/internal/store"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("modernc.org/sqlite.(*conn).interruptOnDone.func1"),
	)
}

// harness is a running server with both surfaces on it — the admin interface
// under /admin and the Plex-compatible API at the root — because that is how
// it is actually mounted, and because one of these tests proves a change made
// in the interface is what a media client then sees.
type harness struct {
	t      *testing.T
	db     *store.DB
	server *httptest.Server
	client *http.Client

	adminToken  string
	viewerToken string
	adminID     int64
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bms.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	owner, err := db.CreateUser(ctx, "admin", true)
	if err != nil {
		t.Fatal(err)
	}
	adminToken, err := db.IssueToken(ctx, owner.ID, "first run")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := db.CreateUser(ctx, "kitchen-tv", false)
	if err != nil {
		t.Fatal(err)
	}
	viewerToken, err := db.IssueToken(ctx, viewer.ID, "kitchen")
	if err != nil {
		t.Fatal(err)
	}

	// The same provider resolution the real server uses, so a test about what
	// the interface claims is a test about what the server would do.
	runner := &jobs.Runner{DB: db, Provider: jobs.TMDBProvider(db, "", 2)}
	t.Cleanup(runner.Shutdown)

	api := &plexapi.Server{Store: db, Name: "Bask Media Server", MachineID: "machine-1", Version: "test"}
	panel := &admin.Server{DB: db, Jobs: runner, Name: "Bask Media Server", MachineID: "machine-1", Version: "test"}

	mux := http.NewServeMux()
	mux.Handle("/admin/", panel.Handler())
	mux.Handle("/", api.Handler())
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{
		t: t, db: db, server: server,
		client:      &http.Client{Jar: jar},
		adminToken:  adminToken,
		viewerToken: viewerToken,
		adminID:     owner.ID,
	}
}

// call makes a request the way the page does: same-origin, with the header
// that every write requires.
func (h *harness) call(method, path string, body any) (*http.Response, map[string]any) {
	h.t.Helper()
	return h.raw(method, path, body, true)
}

func (h *harness) raw(method, path string, body any, csrf bool) (*http.Response, map[string]any) {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		h.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf {
		req.Header.Set("X-BMS-Admin", "1")
	}
	res, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()

	var decoded map[string]any
	raw, _ := io.ReadAll(res.Body)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	return res, decoded
}

// list reads an endpoint that answers with an array.
func (h *harness) list(path string) []map[string]any {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.server.URL+path, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("X-BMS-Admin", "1")
	res, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		h.t.Fatalf("GET %s = %d", path, res.StatusCode)
	}
	var out []map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		h.t.Fatal(err)
	}
	return out
}

func (h *harness) signIn(token string) *http.Response {
	h.t.Helper()
	res, _ := h.call(http.MethodPost, "/admin/api/session", map[string]string{"token": token})
	return res
}

// The page is reachable without signing in, because a sign-in form has to be;
// nothing that knows anything about the library is.
func TestNothingIsReadableWithoutASession(t *testing.T) {
	h := newHarness(t)

	for _, path := range []string{"/admin/api/overview", "/admin/api/sections", "/admin/api/users", "/admin/api/settings"} {
		res, _ := h.call(http.MethodGet, path, nil)
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s = %d, want 401", path, res.StatusCode)
		}
	}
	res, _ := h.call(http.MethodGet, "/admin/", nil)
	if res.StatusCode != http.StatusOK {
		t.Errorf("the sign-in page answered %d, want 200", res.StatusCode)
	}
	if got := res.Header.Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'none'") {
		t.Errorf("the page has no content security policy: %q", got)
	}
}

func TestSigningInWithAnAdminTokenAndOutAgain(t *testing.T) {
	h := newHarness(t)

	if res := h.signIn(h.adminToken); res.StatusCode != http.StatusOK {
		t.Fatalf("sign-in = %d", res.StatusCode)
	}
	res, body := h.call(http.MethodGet, "/admin/api/overview", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("overview after signing in = %d: %v", res.StatusCode, body)
	}
	if body["version"] != "test" {
		t.Errorf("overview = %v", body)
	}

	if res, _ := h.call(http.MethodDelete, "/admin/api/session", nil); res.StatusCode != http.StatusNoContent {
		t.Fatalf("sign-out = %d", res.StatusCode)
	}
	if res, _ := h.call(http.MethodGet, "/admin/api/overview", nil); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("still signed in after signing out: %d", res.StatusCode)
	}
}

// A token that plays films is not a token that administers the server.
func TestAViewersTokenCannotSignIn(t *testing.T) {
	h := newHarness(t)

	res, body := h.call(http.MethodPost, "/admin/api/session", map[string]string{"token": h.viewerToken})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("a viewer signed in: %d %v", res.StatusCode, body)
	}
	if res, _ := h.call(http.MethodGet, "/admin/api/overview", nil); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a refused sign-in left a session: %d", res.StatusCode)
	}
}

// The cross-site defence: a cookie alone is not enough to change anything,
// because a form on another site can send the cookie but cannot add a header.
func TestAWriteWithoutTheSameSiteHeaderIsRefused(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)

	res, _ := h.raw(http.MethodPost, "/admin/api/sections",
		map[string]any{"title": "Films", "kind": "movies", "roots": []string{t.TempDir()}}, false)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("a header-less write = %d, want 403", res.StatusCode)
	}
	if sections := h.list("/admin/api/sections"); len(sections) != 0 {
		t.Errorf("the refused write created a library: %v", sections)
	}
}

func TestAddingALibrary(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	root := t.TempDir()

	res, body := h.call(http.MethodPost, "/admin/api/sections",
		map[string]any{"title": "Films", "kind": "movies", "roots": []string{root}})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("adding a library = %d: %v", res.StatusCode, body)
	}

	sections := h.list("/admin/api/sections")
	if len(sections) != 1 || sections[0]["title"] != "Films" || sections[0]["kind"] != "movies" {
		t.Fatalf("sections = %v", sections)
	}

	// The same library, seen through the store the media clients read.
	stored, err := h.db.Sections(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Roots[0] != root {
		t.Errorf("stored sections = %+v", stored)
	}
}

// A folder that is not there is the single most common setup mistake, and the
// moment to say so is while somebody is looking at the form.
func TestALibraryWithNoSuchFolderIsRefused(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)

	missing := filepath.Join(t.TempDir(), "not-here")
	res, body := h.call(http.MethodPost, "/admin/api/sections",
		map[string]any{"title": "Films", "kind": "movies", "roots": []string{missing}})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a missing folder was accepted: %d %v", res.StatusCode, body)
	}
	if message, _ := body["error"].(string); !strings.Contains(message, missing) {
		t.Errorf("the refusal does not say which folder: %q", message)
	}
}

// A root that has gone away — an unplugged disk — is reported rather than
// quietly pruning the library on the next scan.
func TestALibraryWhoseFolderVanishedSaysSo(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)

	root := filepath.Join(t.TempDir(), "media")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if res, body := h.call(http.MethodPost, "/admin/api/sections",
		map[string]any{"title": "Films", "kind": "movies", "roots": []string{root}}); res.StatusCode != http.StatusCreated {
		t.Fatalf("adding a library = %d: %v", res.StatusCode, body)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}

	sections := h.list("/admin/api/sections")
	missing, _ := sections[0]["missing"].([]any)
	if len(missing) != 1 || missing[0] != root {
		t.Errorf("a vanished root was not reported: %v", sections[0])
	}
}

// Removing a library forgets rows. It must never be able to delete media, so
// the files are still there afterwards.
func TestRemovingALibraryLeavesTheFilesAlone(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)

	root := t.TempDir()
	film := filepath.Join(root, "Dinotopia (2002).mkv")
	if err := os.WriteFile(film, []byte("not really a film"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, body := h.call(http.MethodPost, "/admin/api/sections",
		map[string]any{"title": "Films", "kind": "movies", "roots": []string{root}})
	id := int64(body["id"].(float64))

	if res, _ := h.call(http.MethodDelete, "/admin/api/sections/"+itoa(id), nil); res.StatusCode != http.StatusNoContent {
		t.Fatalf("removing a library = %d", res.StatusCode)
	}
	if sections := h.list("/admin/api/sections"); len(sections) != 0 {
		t.Errorf("the library is still listed: %v", sections)
	}
	if _, err := os.Stat(film); err != nil {
		t.Errorf("removing a library deleted a file: %v", err)
	}
}

// An account with no credential cannot be used, so one comes with the account
// — and it is shown exactly once, because only its hash is stored.
func TestANewAccountComesWithAWorkingTokenShownOnce(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.signIn(h.adminToken)

	res, body := h.call(http.MethodPost, "/admin/api/users", map[string]any{"username": "bedroom", "admin": false})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("creating an account = %d: %v", res.StatusCode, body)
	}
	token, _ := body["token"].(string)
	if token == "" {
		t.Fatal("no token came back with the account")
	}

	user, err := h.db.UserForToken(ctx, token)
	if err != nil {
		t.Fatalf("the issued token does not work: %v", err)
	}
	if user.Username != "bedroom" || user.Admin {
		t.Errorf("the token belongs to %+v", user)
	}

	// Listing the account's credentials describes them without being one.
	id := int64(body["id"].(float64))
	tokens := h.list("/admin/api/users/" + itoa(id) + "/tokens")
	if len(tokens) != 1 {
		t.Fatalf("tokens = %v", tokens)
	}
	for _, field := range []string{"token", "hash", "secret"} {
		if _, found := tokens[0][field]; found {
			t.Errorf("the token listing leaks %q: %v", field, tokens[0])
		}
	}
}

// Revoking a token signs that client out and leaves every other client alone.
func TestRevokingOneTokenLeavesTheOthers(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.signIn(h.adminToken)

	_, body := h.call(http.MethodPost, "/admin/api/users/"+itoa(h.adminID)+"/tokens",
		map[string]any{"label": "laptop"})
	second, _ := body["token"].(string)
	if second == "" {
		t.Fatal("no token was issued")
	}

	tokens := h.list("/admin/api/users/" + itoa(h.adminID) + "/tokens")
	if len(tokens) != 2 {
		t.Fatalf("tokens = %v", tokens)
	}
	var revoke int64
	for _, token := range tokens {
		if token["label"] == "laptop" {
			revoke = int64(token["id"].(float64))
		}
	}

	if res, _ := h.call(http.MethodDelete, "/admin/api/tokens/"+itoa(revoke), nil); res.StatusCode != http.StatusNoContent {
		t.Fatalf("revoking = %d", res.StatusCode)
	}
	if _, err := h.db.UserForToken(ctx, second); err == nil {
		t.Error("the revoked token still works")
	}
	if _, err := h.db.UserForToken(ctx, h.adminToken); err != nil {
		t.Errorf("revoking one token broke another: %v", err)
	}
	// And the session made from that first token is still signed in.
	if res, _ := h.call(http.MethodGet, "/admin/api/overview", nil); res.StatusCode != http.StatusOK {
		t.Errorf("revoking a token signed the browser out: %d", res.StatusCode)
	}
}

// There is no account service to recover from locking yourself out: the last
// administrator cannot be deleted or demoted.
func TestTheLastAdministratorCannotBeRemoved(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)

	res, body := h.call(http.MethodDelete, "/admin/api/users/"+itoa(h.adminID), nil)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("deleting the only administrator = %d: %v", res.StatusCode, body)
	}
	res, _ = h.call(http.MethodPatch, "/admin/api/users/"+itoa(h.adminID), map[string]any{"admin": false})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("demoting the only administrator = %d", res.StatusCode)
	}

	// With a second administrator it is allowed, because somebody is left.
	_, made := h.call(http.MethodPost, "/admin/api/users", map[string]any{"username": "second", "admin": true})
	if res, _ := h.call(http.MethodPatch, "/admin/api/users/"+itoa(h.adminID), map[string]any{"admin": false}); res.StatusCode != http.StatusNoContent {
		t.Fatalf("demoting with a second administrator = %d (second = %v)", res.StatusCode, made)
	}
}

// Renaming the server is the one setting that a media client sees, so this
// test asks the Plex surface rather than the database.
func TestRenamingTheServerIsWhatClientsThenSee(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)

	if res, body := h.call(http.MethodPatch, "/admin/api/settings", map[string]any{"name": "The Cupboard"}); res.StatusCode != http.StatusNoContent {
		t.Fatalf("renaming = %d: %v", res.StatusCode, body)
	}

	req, err := http.NewRequest(http.MethodGet, h.server.URL+"/identity", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json")
	res, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	var identity struct {
		MediaContainer struct {
			FriendlyName      string `json:"friendlyName"`
			MachineIdentifier string `json:"machineIdentifier"`
		} `json:"MediaContainer"`
	}
	if err := json.NewDecoder(res.Body).Decode(&identity); err != nil {
		t.Fatal(err)
	}
	if identity.MediaContainer.FriendlyName != "The Cupboard" {
		t.Errorf("a client sees %q, want the new name", identity.MediaContainer.FriendlyName)
	}
	// The identity itself must not have moved: a client that sees it change
	// forgets every setting and every download it had.
	if identity.MediaContainer.MachineIdentifier != "machine-1" {
		t.Errorf("renaming changed the machine identifier to %q", identity.MediaContainer.MachineIdentifier)
	}
}

// The key is write-only: it is stored, it is reported as stored, and it is
// never sent back to a browser.
func TestTheMetadataKeyIsNeverSentBack(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)

	if res, _ := h.call(http.MethodPatch, "/admin/api/settings", map[string]any{"tmdbKey": "sekrit-key"}); res.StatusCode != http.StatusNoContent {
		t.Fatal("the key was not accepted")
	}
	res, body := h.call(http.MethodGet, "/admin/api/settings", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("settings = %d", res.StatusCode)
	}
	if body["tmdbKeyStored"] != true {
		t.Errorf("settings do not report the stored key: %v", body)
	}
	encoded, _ := json.Marshal(body)
	if strings.Contains(string(encoded), "sekrit-key") {
		t.Errorf("the key came back to the page: %s", encoded)
	}
}

// The status page says what this server can and cannot do, and it has to be
// the truth: a page claiming metadata can be fetched, over a button that then
// refuses, is worse than no claim at all.
func TestWhatTheServerCanDoFollowsWhatItHas(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)

	_, before := h.call(http.MethodGet, "/admin/api/overview", nil)
	if before["canEnrich"] != false {
		t.Errorf("with no key the page claims metadata can be fetched: %v", before["canEnrich"])
	}
	// No ffmpeg and no ffprobe in this harness, and nothing may pretend
	// otherwise.
	if before["canDetect"] != false || before["canScan"] != false {
		t.Errorf("the page claims tools it does not have: %v", before)
	}

	if res, _ := h.call(http.MethodPatch, "/admin/api/settings", map[string]any{"tmdbKey": "a-key"}); res.StatusCode != http.StatusNoContent {
		t.Fatal("the key was not accepted")
	}
	_, after := h.call(http.MethodGet, "/admin/api/overview", nil)
	if after["canEnrich"] != true {
		t.Errorf("a key was saved and the page still says metadata cannot be fetched: %v", after["canEnrich"])
	}
}

// Setting up a library means pointing at a folder, so the server lists what it
// can see and counts what is worth scanning.
func TestBrowsingListsFoldersAndCountsMedia(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Season 01"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"S01E01.mkv", "S01E02.mp4", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res, body := h.call(http.MethodGet, "/admin/api/browse?path="+root, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("browse = %d: %v", res.StatusCode, body)
	}
	folders, _ := body["folders"].([]any)
	if len(folders) != 1 {
		t.Errorf("folders = %v, want just the season (hidden folders are clutter)", folders)
	}
	if body["media"] != float64(2) {
		t.Errorf("media = %v, want the two playable files", body["media"])
	}

	// Relative paths are refused: the roots a section stores are absolute, and
	// a relative one would be read against whatever directory the server was
	// started in.
	if res, _ := h.call(http.MethodGet, "/admin/api/browse?path=relative/path", nil); res.StatusCode != http.StatusBadRequest {
		t.Errorf("a relative path was accepted: %d", res.StatusCode)
	}
}

// A pass that cannot run says why. 422 rather than 500: the request was fine
// and so is the server.
func TestAPassThatNeedsAToolItDoesNotHaveExplainsItself(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)

	res, body := h.call(http.MethodPost, "/admin/api/jobs", map[string]any{"kind": "markers"})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("starting markers with no ffmpeg = %d: %v", res.StatusCode, body)
	}
	if message, _ := body["error"].(string); !strings.Contains(message, "ffmpeg") {
		t.Errorf("the refusal does not mention ffmpeg: %q", message)
	}
}

// Guessing a 32-byte token is not feasible, but a sign-in form that answers
// instantly forever is still worth slowing down.
func TestRepeatedFailedSignInsAreSlowedDown(t *testing.T) {
	h := newHarness(t)

	var limited bool
	for range 8 {
		res, _ := h.call(http.MethodPost, "/admin/api/session", map[string]string{"token": "wrong"})
		if res.StatusCode == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("a run of failed sign-ins was never slowed down")
	}

	// A fresh server, to show the pause is on the attempts and not on the
	// token: the right one works first time.
	fresh := newHarness(t)
	if res := fresh.signIn(fresh.adminToken); res.StatusCode != http.StatusOK {
		t.Errorf("the real token was refused: %d", res.StatusCode)
	}
}

// The page and its assets come out of the binary, which is what keeps a
// release one file.
func TestThePageAndItsAssetsAreEmbedded(t *testing.T) {
	h := newHarness(t)

	for path, contentType := range map[string]string{
		"/admin/app.js":  "text/javascript",
		"/admin/app.css": "text/css",
	} {
		req, _ := http.NewRequest(http.MethodGet, h.server.URL+path, nil)
		res, err := h.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d", path, res.StatusCode)
		}
		if !strings.Contains(res.Header.Get("Content-Type"), contentType) {
			t.Errorf("%s served as %q", path, res.Header.Get("Content-Type"))
		}
		if len(body) < 500 {
			t.Errorf("%s is %d bytes, which is not the asset", path, len(body))
		}
	}
}

// A session lasts until it expires, and then it is simply gone: no refresh, no
// sliding window to get wrong.
func TestAnExpiredSessionIsNotASession(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	secret, err := h.db.CreateSession(ctx, h.adminID, 0, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := h.db.SessionUser(ctx, secret); err == nil {
		t.Fatal("an expired session still resolves")
	}

	// And it is gone from the table rather than sitting there for ever.
	stats, err := h.db.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sessions != 0 {
		t.Errorf("%d expired sessions are still stored", stats.Sessions)
	}
}

// Counts are what the status page draws, so they have to describe the library
// the Plex surface serves rather than a second idea of it.
func TestTheStatusPageCountsTheLibrary(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.signIn(h.adminToken)

	section, err := h.db.CreateSection(ctx, "Films", domain.SectionMovies, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	writer := h.db.NewWriter(10)
	for i := range 3 {
		if err := writer.Add(ctx, store.Found{
			SectionID: section.ID,
			Kind:      domain.KindMovie,
			Title:     "Film " + itoa(int64(i)),
			Year:      2000 + i,
			Path:      filepath.Join(t.TempDir(), "film"+itoa(int64(i))+".mkv"),
			Size:      1 << 20,
			Container: "mkv",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	_, body := h.call(http.MethodGet, "/admin/api/overview", nil)
	stats, _ := body["stats"].(map[string]any)
	if stats["movies"] != float64(3) {
		t.Errorf("overview counts %v movies, want 3", stats["movies"])
	}
	if stats["bytes"] != float64(3<<20) {
		t.Errorf("overview counts %v bytes, want %d", stats["bytes"], 3<<20)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
