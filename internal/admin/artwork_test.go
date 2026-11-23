package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/artwork"
	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/jobs"
	"github.com/inknironapps/bask-media-server/internal/metadata"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// fakeSource is a stand-in artwork service: it says what it has and can be
// told to fail, which is the case the chooser has to survive.
type fakeSource struct {
	name string
	art  []metadata.Art
	err  error

	// asked records the query, so a test can show the provider id is only
	// offered to the service it belongs to.
	asked *metadata.ArtQuery
}

func (f *fakeSource) Name() string { return f.name }

func (f *fakeSource) Art(_ context.Context, want metadata.ArtQuery) ([]metadata.Art, error) {
	query := want
	f.asked = &query
	if f.err != nil {
		return nil, f.err
	}
	return f.art, nil
}

// pixels is a small PNG, which is what a picture is for these tests: the
// cache decodes what it stores, so the bytes have to be a real image.
func pixels(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 6))
	for x := range 4 {
		for y := range 6 {
			img.Set(x, y, color.RGBA{R: uint8(x * 40), G: uint8(y * 30), B: 90, A: 255})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// seenAgent is the user agent the last fetch sent, so a test can show this
// program names itself: the Go default is refused outright by a good deal of
// the web, a poster site among them.
var seenAgent atomic.Value

// dressable gives the harness an artwork cache, a library with one film, and
// a place that serves pictures.
func dressable(t *testing.T, h *harness) (filmID int64, pictures *httptest.Server) {
	t.Helper()
	cache, err := artwork.New(filepath.Join(t.TempDir(), "art"), 64<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	h.panel.Artwork = cache

	section := films(t, h)
	items, _, err := h.db.SectionItems(context.Background(), section, domain.KindMovie, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Title == "Big Hill" {
			filmID = item.ID
		}
	}
	if filmID == 0 {
		t.Fatal("no film to dress")
	}

	body := pixels(t)
	pictures = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAgent.Store(r.Header.Get("User-Agent"))
		switch r.URL.Path {
		case "/missing.jpg":
			http.Error(w, "gone", http.StatusNotFound)
			return
		case "/login.jpg":
			// A login wall or a dead link: a cheerful 200 and a page.
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<!doctype html><title>Please sign in</title>"))
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(body)
	}))
	t.Cleanup(pictures.Close)
	return filmID, pictures
}

func TestTheChooserListsWhatEveryServiceHas(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	id, pictures := dressable(t, h)

	one := &fakeSource{name: "tmdb", art: []metadata.Art{{
		Kind: metadata.ArtPoster, URL: pictures.URL + "/a.jpg", ThumbURL: pictures.URL + "/a-small.jpg",
		Width: 680, Height: 1000, Votes: 7, Source: "tmdb",
	}}}
	two := &fakeSource{name: "tvdb", art: []metadata.Art{{
		Kind: metadata.ArtBackdrop, URL: pictures.URL + "/b.jpg", ThumbURL: pictures.URL + "/b-small.jpg",
		Width: 1920, Height: 1080, Language: "en", Source: "tvdb",
	}}}
	h.runner.Sources = func(context.Context) []metadata.ArtworkSource {
		return []metadata.ArtworkSource{one, two}
	}

	res, body := h.call(http.MethodGet, "/admin/api/items/"+strconv.FormatInt(id, 10)+"/artwork", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("artwork = %d: %v", res.StatusCode, body)
	}
	art, _ := body["art"].([]any)
	if len(art) != 2 {
		t.Fatalf("art = %v", body["art"])
	}
	sources, _ := body["sources"].([]any)
	if len(sources) != 2 {
		t.Fatalf("sources = %v", body["sources"])
	}
	if body["failed"] != nil {
		t.Fatalf("failed = %v", body["failed"])
	}
	// The paste box is always offered, because it is the only way to use a
	// service with no search.
	if body["link"] == nil || body["link"] == "" {
		t.Fatal("the link box was not described")
	}
}

func TestOnlyTheMatchingServiceIsGivenTheProviderID(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	id, _ := dressable(t, h)

	// The film carries a TMDB id, as an enriched library's films do.
	if err := h.db.SaveMetadata(context.Background(), id, "4242", "A hill.", "", ""); err != nil {
		t.Fatal(err)
	}

	mine := &fakeSource{name: "tmdb"}
	theirs := &fakeSource{name: "tvdb"}
	h.runner.Sources = func(context.Context) []metadata.ArtworkSource {
		return []metadata.ArtworkSource{mine, theirs}
	}

	if res, body := h.call(http.MethodGet, "/admin/api/items/"+strconv.FormatInt(id, 10)+"/artwork", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("artwork = %d: %v", res.StatusCode, body)
	}
	if mine.asked == nil || mine.asked.ProviderID != "4242" {
		t.Fatalf("tmdb was asked %+v, want the id it owns", mine.asked)
	}
	// Handing TMDB's number to TheTVDB would fetch the artwork of whatever
	// happens to have it there.
	if theirs.asked == nil || theirs.asked.ProviderID != "" {
		t.Fatalf("tvdb was given another service's id: %+v", theirs.asked)
	}
	if theirs.asked.Title != "Big Hill" || theirs.asked.Year != 2026 {
		t.Fatalf("tvdb was asked %+v, want the title and year", theirs.asked)
	}
}

func TestOneServiceFailingDoesNotTakeTheChooserDown(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	id, pictures := dressable(t, h)

	working := &fakeSource{name: "tmdb", art: []metadata.Art{{
		Kind: metadata.ArtPoster, URL: pictures.URL + "/a.jpg", Source: "tmdb",
	}}}
	broken := &fakeSource{name: "tvdb", err: errRefused}
	h.runner.Sources = func(context.Context) []metadata.ArtworkSource {
		return []metadata.ArtworkSource{working, broken}
	}

	res, body := h.call(http.MethodGet, "/admin/api/items/"+strconv.FormatInt(id, 10)+"/artwork", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("artwork = %d: %v", res.StatusCode, body)
	}
	if art, _ := body["art"].([]any); len(art) != 1 {
		t.Fatalf("art = %v", body["art"])
	}
	failed, _ := body["failed"].([]any)
	if len(failed) != 1 {
		t.Fatalf("failed = %v", body["failed"])
	}
	// Named, so the page can say which service is down rather than drawing
	// a short grid with no explanation.
	if line, _ := failed[0].(string); line == "" || line[:4] != "tvdb" {
		t.Fatalf("failed line = %v", failed[0])
	}
}

func TestAServerWithNoKeysSaysSo(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	id, _ := dressable(t, h)

	h.runner.Sources = func(context.Context) []metadata.ArtworkSource { return nil }

	res, body := h.call(http.MethodGet, "/admin/api/items/"+strconv.FormatInt(id, 10)+"/artwork", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("artwork = %d: %v", res.StatusCode, body)
	}
	why, _ := body["why"].(string)
	if why == "" {
		t.Fatal("a server with nothing configured drew an empty grid with no reason")
	}
}

func TestChoosingAPictureFetchesItAndLocksTheItem(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	id, pictures := dressable(t, h)
	key := "/admin/api/items/" + strconv.FormatInt(id, 10)

	res, body := h.call(http.MethodPost, key+"/artwork", map[string]any{
		"kind": "poster", "url": pictures.URL + "/chosen.png",
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("choose = %d: %v", res.StatusCode, body)
	}

	item, err := h.db.Item(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if item.PosterKey == "" {
		t.Fatal("no poster was stored")
	}
	if item.ArtKey != "" {
		t.Fatal("choosing a poster set the backdrop as well")
	}
	// Locked, because the point of choosing by hand is that the next
	// metadata pass does not replace it.
	if !item.Locked {
		t.Fatal("the item was not locked")
	}
	// And the picture is in this server's own cache, not hotlinked.
	if res := h.get(t, key+"/image?kind=poster"); res != http.StatusOK {
		t.Fatalf("the stored poster served %d", res)
	}

	// A backdrop next, which must not disturb the poster.
	res, body = h.call(http.MethodPost, key+"/artwork", map[string]any{
		"kind": "backdrop", "url": pictures.URL + "/wide.png",
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("choose backdrop = %d: %v", res.StatusCode, body)
	}
	after, err := h.db.Item(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if after.PosterKey != item.PosterKey {
		t.Fatal("choosing a backdrop replaced the poster")
	}
	if after.ArtKey == "" {
		t.Fatal("no backdrop was stored")
	}
}

func TestAPosterDBLinkIsTurnedIntoItsPicture(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	id, _ := dressable(t, h)

	// The site itself is not reachable from a test, so what is checked here
	// is the refusal path: a set names many pictures, and the message has to
	// say what to do instead.
	res, body := h.call(http.MethodPost,
		"/admin/api/items/"+strconv.FormatInt(id, 10)+"/artwork",
		map[string]any{"kind": "poster", "url": "https://theposterdb.com/set/4242"})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a set = %d: %v", res.StatusCode, body)
	}
	if message, _ := body["error"].(string); message == "" {
		t.Fatal("a set was refused with no reason")
	}
}

func TestOnlyAnHTTPAddressIsAccepted(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	id, _ := dressable(t, h)
	key := "/admin/api/items/" + strconv.FormatInt(id, 10) + "/artwork"

	for _, address := range []string{
		"", "   ",
		// Reading the server's own disk on a request from a page is a
		// different and much larger decision.
		"file:///etc/passwd",
		"data:image/png;base64,AAAA",
		"not an address at all",
	} {
		res, body := h.call(http.MethodPost, key, map[string]any{"kind": "poster", "url": address})
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("%q = %d: %v", address, res.StatusCode, body)
		}
	}

	// And a kind that is neither of the two a client draws.
	res, body := h.call(http.MethodPost, key, map[string]any{"kind": "logo", "url": "https://example.com/a.png"})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a logo = %d: %v", res.StatusCode, body)
	}
}

func TestAPictureThatDoesNotFetchIsReportedRatherThanSaved(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	id, pictures := dressable(t, h)

	res, body := h.call(http.MethodPost,
		"/admin/api/items/"+strconv.FormatInt(id, 10)+"/artwork",
		map[string]any{"kind": "poster", "url": pictures.URL + "/missing.jpg"})
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("a missing picture = %d: %v", res.StatusCode, body)
	}

	item, err := h.db.Item(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if item.PosterKey != "" {
		t.Fatal("a picture that did not fetch was recorded anyway")
	}
	if item.Locked {
		t.Fatal("a failed choice locked the item")
	}
}

func TestAPictureCanBeUploaded(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	id, _ := dressable(t, h)
	key := "/admin/api/items/" + strconv.FormatInt(id, 10)

	res, body := h.upload(t, key+"/artwork/upload?kind=poster", pixels(t))
	if res != http.StatusOK {
		t.Fatalf("upload = %d: %v", res, body)
	}
	item, err := h.db.Item(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if item.PosterKey == "" {
		t.Fatal("the uploaded picture was not stored")
	}
	if !item.Locked {
		t.Fatal("an uploaded picture did not lock the item")
	}
}

func TestAnUploadThatIsNotAPictureIsRefused(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	id, _ := dressable(t, h)
	key := "/admin/api/items/" + strconv.FormatInt(id, 10)

	// Checked by its own first bytes, not by what the page claimed: the
	// cache renders what it stores, and a PDF named poster.jpg would be a
	// broken shelf discovered later.
	if res, _ := h.upload(t, key+"/artwork/upload?kind=poster", []byte("%PDF-1.7\nnot a picture")); res != http.StatusBadRequest {
		t.Fatalf("a PDF = %d", res)
	}
	if res, _ := h.upload(t, key+"/artwork/upload?kind=poster", nil); res != http.StatusBadRequest {
		t.Fatalf("an empty body = %d", res)
	}

	item, err := h.db.Item(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if item.PosterKey != "" || item.Locked {
		t.Fatal("a refused upload changed the item")
	}
}

func TestOnlyAFilmOrASeriesCanBeLookedUp(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	_, pictures := dressable(t, h)

	ctx := context.Background()
	music, err := h.db.CreateSection(ctx, "Music", domain.SectionMusic, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	writer := h.db.NewWriter(2)
	if err := writer.Add(ctx, store.Found{
		SectionID: music.ID, Kind: domain.KindTrack, Title: "Catch",
		Artist: "A Band", Album: "An Album",
		Path: t.TempDir() + "/catch.flac", Container: "flac", Size: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	asked := &fakeSource{name: "tmdb"}
	h.runner.Sources = func(context.Context) []metadata.ArtworkSource {
		return []metadata.ArtworkSource{asked}
	}

	albums, _, err := h.db.SectionItems(ctx, music.ID, domain.KindArtist, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	artist := albums[0].ID

	res, body := h.call(http.MethodGet, "/admin/api/items/"+strconv.FormatInt(artist, 10)+"/artwork", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("artwork = %d: %v", res.StatusCode, body)
	}
	// Nothing was asked: a search for the name of an artist against a film
	// catalogue would offer the pictures of something else entirely.
	if asked.asked != nil {
		t.Fatalf("a film service was asked about an artist: %+v", asked.asked)
	}
	if why, _ := body["why"].(string); why == "" {
		t.Fatal("no reason was given for an empty chooser")
	}

	// A picture can still be put on it by hand, which is the point of the
	// reason being a note rather than a refusal.
	res, body = h.call(http.MethodPost,
		"/admin/api/items/"+strconv.FormatInt(artist, 10)+"/artwork",
		map[string]any{"kind": "poster", "url": pictures.URL + "/band.png"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("choose on an artist = %d: %v", res.StatusCode, body)
	}
}

func TestTheChooserNeedsAnAdministrator(t *testing.T) {
	h := newHarness(t)
	id, _ := dressable(t, h)

	// Not signed in at all.
	res, _ := h.call(http.MethodGet, "/admin/api/items/"+strconv.FormatInt(id, 10)+"/artwork", nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a stranger got %d", res.StatusCode)
	}
}

// get makes a plain request and reports the status, for the routes that
// answer with bytes rather than JSON.
func (h *harness) get(t *testing.T, path string) int {
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
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode
}

// upload posts raw bytes, which is how a picture off this machine arrives.
func (h *harness) upload(t *testing.T, path string, body []byte) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.server.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-BMS-Admin", "1")
	req.Header.Set("Content-Type", "image/png")
	res, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	out := map[string]any{}
	raw, _ := io.ReadAll(res.Body)
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out
}

// errRefused stands in for a service that could not be reached.
var errRefused = errTest("the service refused the connection")

type errTest string

func (e errTest) Error() string { return string(e) }

func TestTheTVDBKeyIsSavedWithoutEverBeingSentBack(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)

	res, before := h.call(http.MethodGet, "/admin/api/settings", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("settings = %d: %v", res.StatusCode, before)
	}
	if before["tvdbKeyStored"] != false {
		t.Fatalf("tvdbKeyStored = %v on a fresh server", before["tvdbKeyStored"])
	}

	res, body := h.call(http.MethodPatch, "/admin/api/settings",
		map[string]any{"tvdbKey": "  a-key  ", "tvdbPin": "1234"})
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("save = %d: %v", res.StatusCode, body)
	}

	res, after := h.call(http.MethodGet, "/admin/api/settings", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("settings = %d: %v", res.StatusCode, after)
	}
	if after["tvdbKeyStored"] != true || after["tvdbPinStored"] != true {
		t.Fatalf("stored = %v / %v", after["tvdbKeyStored"], after["tvdbPinStored"])
	}
	// The key itself never leaves the server, not even to the page that set
	// it: a secret that is never sent cannot be read out of a cache or a
	// proxy log.
	for name, value := range after {
		if text, ok := value.(string); ok && text == "a-key" {
			t.Fatalf("%s sent the key back", name)
		}
	}

	// Trimmed on the way in, so a pasted key with a stray space works.
	stored, err := h.db.ServerValue(context.Background(), store.KeyTVDB)
	if err != nil {
		t.Fatal(err)
	}
	if stored != "a-key" {
		t.Fatalf("stored key = %q", stored)
	}

	// Clearing it takes the service away again, which is how somebody turns
	// it off: there is no separate switch.
	if res, body := h.call(http.MethodPatch, "/admin/api/settings",
		map[string]any{"tvdbKey": "", "tvdbPin": ""}); res.StatusCode != http.StatusNoContent {
		t.Fatalf("clear = %d: %v", res.StatusCode, body)
	}
	res, cleared := h.call(http.MethodGet, "/admin/api/settings", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("settings = %d: %v", res.StatusCode, cleared)
	}
	if cleared["tvdbKeyStored"] != false {
		t.Fatalf("tvdbKeyStored = %v after clearing", cleared["tvdbKeyStored"])
	}
}

func TestArtworkSourcesAreOfferedOnlyWhereThereIsAKey(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	sources := jobs.ArtworkSources(h.db, "", 2, nil)
	if got := sources(ctx); len(got) != 0 {
		t.Fatalf("a server with no keys offered %d sources", len(got))
	}

	if err := h.db.SetServerValue(ctx, store.KeyTMDB, "a-tmdb-key"); err != nil {
		t.Fatal(err)
	}
	got := sources(ctx)
	if len(got) != 1 || got[0].Name() != "tmdb" {
		t.Fatalf("sources = %v", names(got))
	}

	if err := h.db.SetServerValue(ctx, store.KeyTVDB, "a-tvdb-key"); err != nil {
		t.Fatal(err)
	}
	got = sources(ctx)
	if len(got) != 2 {
		t.Fatalf("sources = %v", names(got))
	}
	// TMDB first, because it is the key the library was matched with: a
	// server that enriches at all can browse pictures with no further setup.
	if got[0].Name() != "tmdb" || got[1].Name() != "tvdb" {
		t.Fatalf("order = %v", names(got))
	}
}

func names(sources []metadata.ArtworkSource) []string {
	out := make([]string, 0, len(sources))
	for _, source := range sources {
		out = append(out, source.Name())
	}
	return out
}

func TestALinkToAPageRatherThanAPictureIsRefused(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	id, pictures := dressable(t, h)

	// The commonest way a pasted address fails: it leads to a login wall or
	// a dead link, which answers 200 with HTML. Storing that would be a
	// poster that is a file on disk and a blank square forever.
	res, body := h.call(http.MethodPost,
		"/admin/api/items/"+strconv.FormatInt(id, 10)+"/artwork",
		map[string]any{"kind": "poster", "url": pictures.URL + "/login.jpg"})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a page = %d: %v", res.StatusCode, body)
	}
	// And the message says what to do instead, because this is the failure
	// somebody can actually act on.
	message, _ := body["error"].(string)
	if !strings.Contains(message, "copy its address") {
		t.Fatalf("error = %q", message)
	}

	item, err := h.db.Item(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if item.PosterKey != "" || item.Locked {
		t.Fatal("a page was stored as a poster")
	}
}

func TestThisProgramNamesItselfWhenFetchingAPicture(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	id, pictures := dressable(t, h)

	seenAgent.Store("")
	res, body := h.call(http.MethodPost,
		"/admin/api/items/"+strconv.FormatInt(id, 10)+"/artwork",
		map[string]any{"kind": "poster", "url": pictures.URL + "/a.png"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("choose = %d: %v", res.StatusCode, body)
	}

	agent, _ := seenAgent.Load().(string)
	if agent != artwork.UserAgent {
		t.Fatalf("user agent = %q, want this program's own name", agent)
	}
	// And nothing about whose library this is: the name is the program and
	// its purpose, which is all a third party is told.
	for _, leak := range []string{"Big Hill", "admin", "Films"} {
		if strings.Contains(agent, leak) {
			t.Fatalf("the user agent carries %q: %q", leak, agent)
		}
	}
}
