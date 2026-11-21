package jellyfin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/store"
)

// doWithAuth makes a request that carries its token the way a real client
// does, in Jellyfin's own Authorization scheme beside the fields describing
// the device.
func (c call) doWithAuth(method, path, authorization string) (*http.Response, []byte) {
	c.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(nil))
	req.Header.Set("Authorization", authorization)
	rec := httptest.NewRecorder()
	c.server.Handler().ServeHTTP(rec, req)
	return rec.Result(), rec.Body.Bytes()
}

func (c call) jsonWithAuth(path, authorization string) map[string]any {
	c.t.Helper()
	res, body := c.doWithAuth(http.MethodGet, path, authorization)
	if res.StatusCode != http.StatusOK {
		c.t.Fatalf("GET %s: %d %s", path, res.StatusCode, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		c.t.Fatalf("GET %s: %v (%s)", path, err, body)
	}
	return out
}

// narrow restricts the token the harness issued to the named libraries, which
// is what the admin interface does when somebody ticks boxes under a device.
func narrow(t *testing.T, db *store.DB, titles ...string) {
	t.Helper()
	ctx := context.Background()

	sections, err := db.Sections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wanted := make([]int64, 0, len(titles))
	for _, title := range titles {
		found := false
		for _, section := range sections {
			if section.Title == title {
				wanted = append(wanted, section.ID)
				found = true
			}
		}
		if !found {
			t.Fatalf("no library called %q", title)
		}
	}

	users, err := db.Users(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) == 0 {
		t.Fatal("no accounts")
	}
	tokens, err := db.Tokens(ctx, users[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 1 {
		t.Fatalf("expected one token, got %d", len(tokens))
	}
	if err := db.SetTokenSections(ctx, tokens[0].ID, wanted); err != nil {
		t.Fatal(err)
	}
}

// The library list is the first thing a client asks for, and a device that may
// see one library should be told about one library.
func TestADeviceSeesOnlyTheLibrariesItWasGiven(t *testing.T) {
	server, token, db := library(t)
	c := call{t: t, server: server, token: token}

	if views := c.views(); len(views) != 4 {
		t.Fatalf("expected four libraries before narrowing, got %d", len(views))
	}

	narrow(t, db, "Films")

	views := c.views()
	if len(views) != 1 {
		t.Fatalf("expected one library, got %d: %v", len(views), views)
	}
	if _, ok := views["Films"]; !ok {
		t.Fatalf("expected Films, got %v", views)
	}
}

// Hiding a library from a list is theatre on its own: a client that already
// knows an item's id has to be refused as well, or the television that was
// told to forget a library carries on playing from it.
func TestAHiddenItemIsGoneEveryWayItCanBeAsked(t *testing.T) {
	server, token, db := library(t)
	c := call{t: t, server: server, token: token}

	film := c.film()
	narrow(t, db, "Music")

	// Each of these resolves the item before it does anything else, so each
	// of them is a door that has to be shut.
	for _, path := range []string{
		"/Users/" + strings.Repeat("0", 32) + "/Items/" + film,
		"/Items/" + film + "/Images/Primary",
		"/Items/" + film + "/PlaybackInfo",
		"/Videos/" + film + "/stream",
		"/Videos/" + film + "/master.m3u8",
	} {
		res, body := c.do(http.MethodGet, path, nil)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: want 404, got %d %s", path, res.StatusCode, body)
		}
	}
}

// The item is reachable again the moment its library is ticked back on, which
// is what makes this a setting rather than a deletion.
func TestTickingTheLibraryBackOnRestoresTheItem(t *testing.T) {
	server, token, db := library(t)
	c := call{t: t, server: server, token: token}

	film := c.film()
	narrow(t, db, "Music")
	narrow(t, db, "Films")

	res, body := c.do(http.MethodGet, "/Items/"+film+"/PlaybackInfo", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("PlaybackInfo after re-granting: %d %s", res.StatusCode, body)
	}
}

// Search is the easiest place to forget, because it is the one list that is
// not asked for a library first.
func TestSearchDoesNotReachIntoAHiddenLibrary(t *testing.T) {
	server, token, db := library(t)
	c := call{t: t, server: server, token: token}

	before := c.items("/Items?SearchTerm=a&Recursive=true")
	narrow(t, db, "Photos")
	after := c.items("/Items?SearchTerm=a&Recursive=true")

	if len(before) == 0 {
		t.Fatal("the unrestricted search found nothing, so this proves nothing")
	}
	for _, row := range after {
		if row["Type"] == "Movie" || row["Type"] == "Episode" {
			t.Fatalf("a photographs-only device found %v in search", row["Name"])
		}
	}
	if len(after) >= len(before) {
		t.Fatalf("narrowing did not shrink the search: %d then %d", len(before), len(after))
	}
}

// The home screen rows are built across every library at once, so they are
// the other place a hidden library leaks.
func TestTheHomeScreenRowsRespectTheRestriction(t *testing.T) {
	server, token, db := library(t)
	c := call{t: t, server: server, token: token}

	me := c.me()
	narrow(t, db, "Photos")

	// Latest answers with a bare array and /Items with a paged reply, so each
	// is read in its own shape.
	for _, row := range c.rows("/Users/" + me + "/Items/Latest") {
		if row["Type"] == "Movie" || row["Type"] == "Episode" {
			t.Errorf("Latest returned %v to a photographs-only device", row["Name"])
		}
	}
	for _, row := range c.items("/Users/" + me + "/Items") {
		if row["Type"] == "Movie" || row["Type"] == "Episode" {
			t.Errorf("the library list returned %v to a photographs-only device", row["Name"])
		}
	}
}

// rows reads a reply that is a bare JSON array rather than a paged envelope.
func (c call) rows(path string) []map[string]any {
	c.t.Helper()
	res, body := c.do(http.MethodGet, path, nil)
	if res.StatusCode != http.StatusOK {
		c.t.Fatalf("GET %s: %d %s", path, res.StatusCode, body)
	}
	var out []map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		c.t.Fatalf("GET %s: %v (%s)", path, err, body)
	}
	return out
}

// A token nobody has narrowed reaches everything. This is the case every
// existing database is in, and the migration must not change it.
func TestATokenWithNoGrantsSeesEverything(t *testing.T) {
	server, token, _ := library(t)
	c := call{t: t, server: server, token: token}

	if views := c.views(); len(views) != 4 {
		t.Fatalf("expected every library, got %d: %v", len(views), views)
	}
}

// Two devices, one account: narrowing one must not narrow the other. This is
// the whole point — the restriction belongs to the credential, not the person.
func TestNarrowingOneDeviceLeavesTheOtherAlone(t *testing.T) {
	server, token, db := library(t)
	ctx := context.Background()

	users, err := db.Users(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.IssueToken(ctx, users[0].ID, "television")
	if err != nil {
		t.Fatal(err)
	}

	sections, err := db.Sections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var films int64
	for _, section := range sections {
		if section.Title == "Films" {
			films = section.ID
		}
	}
	tokens, err := db.Tokens(ctx, users[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, t2 := range tokens {
		if t2.Label == "television" {
			if err := db.SetTokenSections(ctx, t2.ID, []int64{films}); err != nil {
				t.Fatal(err)
			}
		}
	}

	television := call{t: t, server: server, token: second}
	if views := television.views(); len(views) != 1 {
		t.Fatalf("television should see one library, got %d", len(views))
	}
	tablet := call{t: t, server: server, token: token}
	if views := tablet.views(); len(views) != 4 {
		t.Fatalf("tablet should still see every library, got %d", len(views))
	}
}

// What the client calls itself is recorded so a list of credentials reads as a
// list of devices. Recorded, never consulted: the next test is the one that
// matters.
func TestTheDeviceNameIsRecordedAgainstTheToken(t *testing.T) {
	server, token, db := library(t)
	ctx := context.Background()

	req := call{t: t, server: server, token: ""}
	res, body := req.doWithAuth(http.MethodGet, "/Users/Me",
		`MediaBrowser Client="Bask", Device="Living room TV", DeviceId="abc123", Token="`+token+`"`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("signed-in request: %d %s", res.StatusCode, body)
	}

	users, err := db.Users(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := db.Tokens(ctx, users[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if tokens[0].Device != "Living room TV" {
		t.Fatalf("device not recorded: %q", tokens[0].Device)
	}
}

// A client writes its own device name, so a device name must not be able to
// widen what a narrowed token reaches.
func TestAClaimedDeviceNameCannotWidenWhatATokenSees(t *testing.T) {
	server, token, db := library(t)
	narrow(t, db, "Films")

	c := call{t: t, server: server, token: ""}
	reply := c.jsonWithAuth("/Users/Me",
		`MediaBrowser Client="Bask", Device="the tablet that sees everything", Token="`+token+`"`)
	me, _ := reply["Id"].(string)
	if me == "" {
		t.Fatal("no account id came back")
	}

	views := call{t: t, server: server, token: token}.views()
	if len(views) != 1 {
		t.Fatalf("expected one library whatever the client calls itself, got %d", len(views))
	}
}
