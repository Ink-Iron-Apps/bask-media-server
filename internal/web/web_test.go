package web_test

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

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/httpsession"
	"github.com/inknironapps/bask-media-server/internal/store"
	"github.com/inknironapps/bask-media-server/internal/web"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("modernc.org/sqlite.(*conn).interruptOnDone.func1"),
	)
}

const filmBody = "this file stands in for ninety minutes of film"

type harness struct {
	t      *testing.T
	db     *store.DB
	server *httptest.Server
	client *http.Client

	token    string
	userID   int64
	filmID   int64
	showID   int64
	seasonID int64
	firstEp  int64
	filmPart int64
}

// library builds one of each shape the interface has to draw: a film, and a
// show with a season and two episodes, with a marker on the first one.
func newHarness(t *testing.T) *harness { return newHarnessWith(t, nil) }

// newHarnessWith builds the same library and lets a test fit the server with
// the pieces that decide how a file is played.
func newHarnessWith(t *testing.T, fit func(*web.Server)) *harness {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bms.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	viewer, err := db.CreateUser(ctx, "kitchen-tv", false)
	if err != nil {
		t.Fatal(err)
	}
	token, err := db.IssueToken(ctx, viewer.ID, "kitchen")
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	films, err := db.CreateSection(ctx, "Films", domain.SectionMovies, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	shows, err := db.CreateSection(ctx, "TV Shows", domain.SectionShows, []string{root})
	if err != nil {
		t.Fatal(err)
	}

	write := func(name, body string) string {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	writer := db.NewWriter(10)
	if err := writer.Add(ctx, store.Found{
		SectionID: films.ID, Kind: domain.KindMovie,
		Title: "Dinotopia", Year: 2002, Duration: 90 * time.Minute,
		Path: write("Dinotopia (2002).mp4", filmBody), Size: int64(len(filmBody)), Container: "mp4",
	}); err != nil {
		t.Fatal(err)
	}
	for episode := 1; episode <= 2; episode++ {
		if err := writer.Add(ctx, store.Found{
			SectionID: shows.ID, Kind: domain.KindEpisode,
			Show: "Hornblower", ShowYear: 1998, SeasonNum: 1, EpisodeNum: episode,
			Title: "Part " + strconv.Itoa(episode), Duration: 42 * time.Minute,
			Path: write("Hornblower - S01E0"+strconv.Itoa(episode)+".mkv", filmBody),
			Size: int64(len(filmBody)), Container: "mkv",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	h := &harness{t: t, db: db, token: token, userID: viewer.ID}

	movies, _, err := db.SectionItems(ctx, films.ID, domain.KindMovie, store.Page{})
	if err != nil || len(movies) != 1 {
		t.Fatalf("films = %v (%v)", movies, err)
	}
	h.filmID = movies[0].ID
	h.filmPart = movies[0].Parts[0].ID

	showList, _, err := db.SectionItems(ctx, shows.ID, domain.KindShow, store.Page{})
	if err != nil || len(showList) != 1 {
		t.Fatalf("shows = %v (%v)", showList, err)
	}
	h.showID = showList[0].ID
	seasons, _, err := db.Children(ctx, h.showID, store.Page{})
	if err != nil || len(seasons) != 1 {
		t.Fatalf("seasons = %v (%v)", seasons, err)
	}
	h.seasonID = seasons[0].ID
	episodes, err := db.EpisodesWithParts(ctx, h.seasonID)
	if err != nil || len(episodes) != 2 {
		t.Fatalf("episodes = %v (%v)", episodes, err)
	}
	h.firstEp = episodes[0].ID

	if err := db.ReplaceMarkers(ctx, h.firstEp, domain.SourceAudio, []domain.Marker{{
		ItemID: h.firstEp, Type: domain.MarkerIntro,
		Start: 0, End: 85 * time.Second, Source: domain.SourceAudio,
	}}); err != nil {
		t.Fatal(err)
	}

	viewing := &web.Server{Store: db, Name: "Bask Media Server"}
	if fit != nil {
		fit(viewing)
	}
	server := httptest.NewServer(viewing.Handler())
	t.Cleanup(server.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	h.server, h.client = server, &http.Client{Jar: jar}
	return h
}

func (h *harness) do(method, path string, body any, csrf bool) (*http.Response, map[string]any) {
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
		req.Header.Set("X-BMS-Web", "1")
	}
	res, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()

	raw, _ := io.ReadAll(res.Body)
	var decoded map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	return res, decoded
}

func (h *harness) call(method, path string, body any) (*http.Response, map[string]any) {
	h.t.Helper()
	return h.do(method, path, body, true)
}

func (h *harness) signIn() {
	h.t.Helper()
	res, body := h.call(http.MethodPost, "/web/api/session", map[string]string{"token": h.token})
	if res.StatusCode != http.StatusOK {
		h.t.Fatalf("sign-in = %d: %v", res.StatusCode, body)
	}
}

func items(t *testing.T, body map[string]any, key string) []map[string]any {
	t.Helper()
	raw, _ := body[key].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		item, _ := entry.(map[string]any)
		out = append(out, item)
	}
	return out
}

// Watching is not administering: an ordinary account signs in here, and the
// same account cannot reach the admin interface.
func TestAViewerSignsInHere(t *testing.T) {
	h := newHarness(t)

	if res, _ := h.call(http.MethodGet, "/web/api/home", nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("home without a session = %d", res.StatusCode)
	}
	h.signIn()

	res, body := h.call(http.MethodGet, "/web/api/home", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("home = %d: %v", res.StatusCode, body)
	}
	if body["name"] != "Bask Media Server" {
		t.Errorf("home = %v", body)
	}
	if sections := items(t, body, "sections"); len(sections) != 2 {
		t.Errorf("got %d libraries, want both", len(sections))
	}
}

func TestThePageIsServedToSomebodyWithNoSession(t *testing.T) {
	h := newHarness(t)

	res, _ := h.call(http.MethodGet, "/web/", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the page = %d", res.StatusCode)
	}
	policy := res.Header.Get("Content-Security-Policy")
	if !strings.Contains(policy, "media-src 'self'") {
		t.Errorf("the player's own source is not allowed by the policy: %q", policy)
	}
}

func TestALibraryIsListedWithItsPosters(t *testing.T) {
	h := newHarness(t)
	h.signIn()

	res, body := h.call(http.MethodGet, "/web/api/sections/1", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("library = %d: %v", res.StatusCode, body)
	}
	list := items(t, body, "items")
	if len(list) != 1 || list[0]["title"] != "Dinotopia" {
		t.Fatalf("items = %v", list)
	}
	if list[0]["poster"] != false {
		t.Errorf("an unenriched film claims a poster: %v", list[0])
	}
	if body["total"] != float64(1) {
		t.Errorf("total = %v", body["total"])
	}
}

// A show opens on its seasons, a season on its episodes, and an episode knows
// what it belongs to without the page asking again.
func TestAShowOpensOnItsSeasonsAndThenItsEpisodes(t *testing.T) {
	h := newHarness(t)
	h.signIn()

	_, show := h.call(http.MethodGet, "/web/api/items/"+strconv.FormatInt(h.showID, 10), nil)
	seasons := items(t, show, "children")
	if len(seasons) != 1 || seasons[0]["kind"] != "season" {
		t.Fatalf("seasons = %v", seasons)
	}

	_, season := h.call(http.MethodGet, "/web/api/items/"+strconv.FormatInt(h.seasonID, 10), nil)
	episodes := items(t, season, "children")
	if len(episodes) != 2 {
		t.Fatalf("episodes = %v", episodes)
	}
	if episodes[0]["subtitle"] != "Hornblower · S1 · E1" {
		t.Errorf("episode subtitle = %v", episodes[0]["subtitle"])
	}
	parent, _ := season["parent"].(map[string]any)
	if parent["title"] != "Hornblower" {
		t.Errorf("a season does not know its show: %v", season["parent"])
	}
}

// The markers the detector found are what the player's Skip button is made
// of, so they have to reach this surface too.
func TestAnEpisodeCarriesItsMarkersAndItsFile(t *testing.T) {
	h := newHarness(t)
	h.signIn()

	_, body := h.call(http.MethodGet, "/web/api/items/"+strconv.FormatInt(h.firstEp, 10), nil)
	markers := items(t, body, "markers")
	if len(markers) != 1 || markers[0]["type"] != "intro" {
		t.Fatalf("markers = %v", markers)
	}
	if markers[0]["endMs"] != float64(85000) {
		t.Errorf("intro ends at %v, want 85000", markers[0]["endMs"])
	}
	parts := items(t, body, "parts")
	if len(parts) != 1 || parts[0]["container"] != "mkv" {
		t.Fatalf("parts = %v", parts)
	}
}

// Seeking is the whole reason the file is served with ranges rather than
// written out from the top.
func TestTheFileIsServedWithRanges(t *testing.T) {
	h := newHarness(t)
	h.signIn()

	req, err := http.NewRequest(http.MethodGet, h.server.URL+"/web/stream/"+strconv.FormatInt(h.filmPart, 10), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=5-14")
	res, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)

	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("a range request = %d, want 206", res.StatusCode)
	}
	if string(body) != filmBody[5:15] {
		t.Errorf("got %q, want the ten bytes that were asked for", body)
	}
	if got := res.Header.Get("Content-Range"); got != "bytes 5-14/"+strconv.Itoa(len(filmBody)) {
		t.Errorf("Content-Range = %q", got)
	}
}

// A file is not public: without a session the stream is refused, which is the
// cookie doing a job no header could, because a <video> sets no headers.
func TestAFileNeedsASession(t *testing.T) {
	h := newHarness(t)

	res, _ := h.call(http.MethodGet, "/web/stream/"+strconv.FormatInt(h.filmPart, 10), nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated stream = %d, want 401", res.StatusCode)
	}
}

// Watching something is the one thing this interface writes, and it has to
// come back as where to resume.
func TestWatchingIsRememberedAndResumed(t *testing.T) {
	h := newHarness(t)
	h.signIn()

	res, _ := h.call(http.MethodPost, "/web/api/progress", map[string]any{
		"itemId": h.filmID, "offsetMs": 1200000, "durationMs": 5400000,
	})
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("saving progress = %d", res.StatusCode)
	}

	_, home := h.call(http.MethodGet, "/web/api/home", nil)
	resume := items(t, home, "continue")
	if len(resume) != 1 || resume[0]["id"] != float64(h.filmID) {
		t.Fatalf("continue watching = %v", resume)
	}
	if resume[0]["offsetMs"] != float64(1200000) {
		t.Errorf("resumes at %v, want 20 minutes in", resume[0]["offsetMs"])
	}

	// Past the threshold it stops being something to continue and becomes
	// something watched, which is the store's rule and not a second one here.
	if res, _ := h.call(http.MethodPost, "/web/api/progress", map[string]any{
		"itemId": h.filmID, "offsetMs": 5300000, "durationMs": 5400000,
	}); res.StatusCode != http.StatusNoContent {
		t.Fatal("saving progress failed")
	}
	_, home = h.call(http.MethodGet, "/web/api/home", nil)
	if resume := items(t, home, "continue"); len(resume) != 0 {
		t.Errorf("a finished film is still in Continue Watching: %v", resume)
	}
}

// Progress is a write, so it needs the header a cross-site form cannot add.
func TestProgressWithoutTheSameSiteHeaderIsRefused(t *testing.T) {
	h := newHarness(t)
	h.signIn()

	res, _ := h.do(http.MethodPost, "/web/api/progress", map[string]any{
		"itemId": h.filmID, "offsetMs": 60000, "durationMs": 5400000,
	}, false)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("a header-less write = %d, want 403", res.StatusCode)
	}
}

// One account's position is not another's: progress is per viewer, which is
// the reason this server has accounts at all.
func TestOneViewersPositionIsTheirOwn(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.signIn()

	if res, _ := h.call(http.MethodPost, "/web/api/progress", map[string]any{
		"itemId": h.filmID, "offsetMs": 600000, "durationMs": 5400000,
	}); res.StatusCode != http.StatusNoContent {
		t.Fatal("saving progress failed")
	}

	other, err := h.db.CreateUser(ctx, "bedroom", false)
	if err != nil {
		t.Fatal(err)
	}
	token, err := h.db.IssueToken(ctx, other.ID, "bedroom")
	if err != nil {
		t.Fatal(err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	h.client = &http.Client{Jar: jar}
	h.token = token
	h.signIn()

	_, home := h.call(http.MethodGet, "/web/api/home", nil)
	if resume := items(t, home, "continue"); len(resume) != 0 {
		t.Errorf("a second account sees the first one's position: %v", resume)
	}
}

func TestSearchFindsAcrossLibraries(t *testing.T) {
	h := newHarness(t)
	h.signIn()

	_, body := h.call(http.MethodGet, "/web/api/search?q=horn", nil)
	found := items(t, body, "items")
	if len(found) == 0 {
		t.Fatalf("search found nothing: %v", body)
	}
	if found[0]["title"] != "Hornblower" {
		t.Errorf("search = %v", found)
	}

	// One letter is not a search; it is every row in the library.
	_, short := h.call(http.MethodGet, "/web/api/search?q=h", nil)
	if list := items(t, short, "items"); len(list) != 0 {
		t.Errorf("a one-letter search returned %d rows", len(list))
	}
}

// Without artwork there is nothing to send, and the grid draws a letter
// instead of waiting for an image that will never come.
func TestAnItemWithNoPosterSaysSo(t *testing.T) {
	h := newHarness(t)
	h.signIn()

	res, _ := h.call(http.MethodGet, "/web/image/"+strconv.FormatInt(h.filmID, 10), nil)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("an image for an unenriched film = %d, want 404", res.StatusCode)
	}
}

// Signing in to watch issues the session both interfaces read, at the root
// rather than at /web, so the admin panel takes it too.
func TestSigningInIssuesTheSharedSession(t *testing.T) {
	h := newHarness(t)

	res, body := h.call(http.MethodPost, "/web/api/session", map[string]string{"token": h.token})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("sign-in = %d: %v", res.StatusCode, body)
	}

	var found *http.Cookie
	for _, cookie := range res.Cookies() {
		if cookie.Name == httpsession.Name {
			found = cookie
		}
	}
	if found == nil {
		t.Fatal("sign-in issued no shared session cookie")
	}
	if found.Path != "/" {
		t.Errorf("the session is scoped to %q, so the admin interface would not see it", found.Path)
	}
	if !found.HttpOnly || found.SameSite != http.SameSiteStrictMode {
		t.Errorf("the session cookie lost its protections: HttpOnly=%v SameSite=%v", found.HttpOnly, found.SameSite)
	}
	if found.MaxAge <= 0 {
		t.Errorf("MaxAge=%d, so the session dies with the browser window", found.MaxAge)
	}
}

// A browser still holding the cookie this replaced keeps watching, and is
// upgraded without being asked for a token.
func TestTheOldWatchCookieIsHonouredAndUpgraded(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	secret, err := h.db.CreateSession(ctx, h.userID, 0, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest(http.MethodGet, h.server.URL+"/web/api/session", nil)
	req.AddCookie(&http.Cookie{Name: "bms_web", Value: secret})
	res, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("an old cookie was refused: %d", res.StatusCode)
	}

	var upgraded, cleared bool
	for _, cookie := range res.Cookies() {
		if cookie.Name == httpsession.Name && cookie.Value == secret {
			upgraded = true
		}
		if cookie.Name == "bms_web" && cookie.MaxAge < 0 {
			cleared = true
		}
	}
	if !upgraded {
		t.Error("the response did not issue the shared cookie")
	}
	if !cleared {
		t.Error("the response did not expire the old cookie")
	}
}
