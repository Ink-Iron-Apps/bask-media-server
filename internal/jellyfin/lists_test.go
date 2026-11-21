package jellyfin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// anItem returns the id of the first film the client can see, as the client
// spells it.
func (c call) anItem(t *testing.T) string {
	t.Helper()
	films := c.items("/Items?IncludeItemTypes=Movie&Recursive=true")
	if len(films) == 0 {
		t.Fatal("the fixture has no films")
	}
	return films[0]["Id"].(string)
}

// someTrack walks down to a track the way a client does: the music library's
// artists, then an album, then its tracks. A track has a parent, so it is not
// at any library's top level and the flat /Items query does not reach it.
func (c call) someTrack(t *testing.T) string {
	t.Helper()
	for _, artist := range c.items("/Artists") {
		for _, album := range c.items("/Items?ParentId=" + artist["Id"].(string)) {
			tracks := c.items("/Items?ParentId=" + album["Id"].(string))
			if len(tracks) > 0 {
				return tracks[0]["Id"].(string)
			}
		}
	}
	t.Fatal("the fixture has no tracks")
	return ""
}

/* ---------- favourites ---------- */

func TestTheStarIsKeptAndComesBack(t *testing.T) {
	server, token, _ := library(t)
	c := call{t: t, server: server, token: token}
	film := c.anItem(t)

	res, body := c.do(http.MethodPost, "/UserFavoriteItems/"+film, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("favourite = %d %s", res.StatusCode, body)
	}

	// It comes back on the item itself…
	read := c.json("/Users/" + c.me() + "/Items/" + film)
	data, _ := read["UserData"].(map[string]any)
	if data == nil || data["IsFavorite"] != true {
		t.Fatalf("the star did not stick: %v", read["UserData"])
	}

	// …and on the shelf a client builds from Filters.
	marked := c.items("/Items?Filters=IsFavorite&Recursive=true")
	if len(marked) != 1 || marked[0]["Id"] != film {
		t.Fatalf("favourites shelf = %v", marked)
	}

	// And unpressing it empties the shelf again.
	if res, body := c.do(http.MethodDelete, "/UserFavoriteItems/"+film, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("unfavourite = %d %s", res.StatusCode, body)
	}
	if left := c.items("/Items?Filters=IsFavorite&Recursive=true"); len(left) != 0 {
		t.Fatalf("still on the shelf: %v", left)
	}
}

func TestAFavouriteInAHiddenLibraryNeverReachesTheShelf(t *testing.T) {
	server, token, db := library(t)
	c := call{t: t, server: server, token: token}
	film := c.anItem(t)
	if res, _ := c.do(http.MethodPost, "/UserFavoriteItems/"+film, nil); res.StatusCode != http.StatusOK {
		t.Fatal("could not favourite the film")
	}

	narrow(t, db, "Music")
	if marked := c.items("/Items?Filters=IsFavorite&Recursive=true"); len(marked) != 0 {
		t.Fatalf("a music-only device was shown %v", marked)
	}
}

/* ---------- playlists ---------- */

func TestAPlaylistIsMadeFilledAndPlayedBack(t *testing.T) {
	server, token, _ := library(t)
	c := call{t: t, server: server, token: token}
	track := c.someTrack(t)

	res, body := c.do(http.MethodPost, "/Playlists", map[string]any{
		"Name": "Evening", "Ids": []string{track},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("create = %d %s", res.StatusCode, body)
	}
	var created map[string]any
	decode(t, body, &created)
	id, _ := created["Id"].(string)
	if id == "" {
		t.Fatalf("no id came back: %s", body)
	}

	listed := c.items("/Playlists")
	if len(listed) != 1 || listed[0]["Name"] != "Evening" {
		t.Fatalf("playlists = %v", listed)
	}
	if listed[0]["Type"] != "Playlist" {
		t.Fatalf("a playlist came back as %v", listed[0]["Type"])
	}

	entries := c.items("/Playlists/" + id + "/Items")
	if len(entries) != 1 || entries[0]["Id"] != track {
		t.Fatalf("entries = %v", entries)
	}
	// The entry's own id, which is what a client needs to remove this line
	// rather than every copy of the track.
	if entries[0]["PlaylistItemId"] == nil || entries[0]["PlaylistItemId"] == "" {
		t.Fatalf("no entry id on %v", entries[0])
	}
	// And it carries what is needed to play it.
	if entries[0]["MediaSources"] == nil {
		t.Fatalf("an entry came back with nothing to play: %v", entries[0])
	}
}

func TestAPlaylistIsFoundThroughTheItemsQueryToo(t *testing.T) {
	server, token, _ := library(t)
	c := call{t: t, server: server, token: token}
	res, body := c.do(http.MethodPost, "/Playlists", map[string]any{"Name": "Found"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("create = %d %s", res.StatusCode, body)
	}

	// A client's Playlists tab asks this way rather than at /Playlists.
	listed := c.items("/Items?IncludeItemTypes=Playlist&Recursive=true")
	if len(listed) != 1 || listed[0]["Name"] != "Found" {
		t.Fatalf("items query for playlists = %v", listed)
	}
}

func TestTheSameTrackTwiceIsTwoLinesAndOneCanGo(t *testing.T) {
	server, token, _ := library(t)
	c := call{t: t, server: server, token: token}
	track := c.someTrack(t)

	res, body := c.do(http.MethodPost, "/Playlists", map[string]any{
		"Name": "Repeat", "Ids": []string{track, track},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("create = %d %s", res.StatusCode, body)
	}
	var created map[string]any
	decode(t, body, &created)
	id := created["Id"].(string)

	entries := c.items("/Playlists/" + id + "/Items")
	if len(entries) != 2 {
		t.Fatalf("expected two lines, got %d", len(entries))
	}
	first := entries[0]["PlaylistItemId"].(string)
	if first == entries[1]["PlaylistItemId"].(string) {
		t.Fatal("two lines share an id, so neither can be removed on its own")
	}

	res, body = c.do(http.MethodDelete, "/Playlists/"+id+"/Items?EntryIds="+first, nil)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("remove = %d %s", res.StatusCode, body)
	}
	left := c.items("/Playlists/" + id + "/Items")
	if len(left) != 1 {
		t.Fatalf("after removing one line, %d left", len(left))
	}
}

func TestAnEntryIsMovedToWhereTheClientDroppedIt(t *testing.T) {
	server, token, _ := library(t)
	c := call{t: t, server: server, token: token}
	film := c.anItem(t)
	track := c.someTrack(t)

	res, body := c.do(http.MethodPost, "/Playlists", map[string]any{
		"Name": "Order", "Ids": []string{film, track},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("create = %d %s", res.StatusCode, body)
	}
	var created map[string]any
	decode(t, body, &created)
	id := created["Id"].(string)

	entries := c.items("/Playlists/" + id + "/Items")
	last := entries[1]["PlaylistItemId"].(string)

	res, body = c.do(http.MethodPost, "/Playlists/"+id+"/Items/"+last+"/Move/0", nil)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("move = %d %s", res.StatusCode, body)
	}
	after := c.items("/Playlists/" + id + "/Items")
	if after[0]["Id"] != track {
		t.Fatalf("the move did not take: %v", after[0]["Name"])
	}
}

// A playlist belongs to one account, and another account's credential must
// not reach it — nor be told it exists.
func TestAnotherAccountCannotReachAPlaylist(t *testing.T) {
	server, token, db := library(t)
	ctx := context.Background()
	c := call{t: t, server: server, token: token}

	res, body := c.do(http.MethodPost, "/Playlists", map[string]any{"Name": "Mine"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("create = %d %s", res.StatusCode, body)
	}
	var created map[string]any
	decode(t, body, &created)
	id := created["Id"].(string)

	other, err := db.CreateUser(ctx, "sam", false)
	if err != nil {
		t.Fatal(err)
	}
	theirToken, err := db.IssueToken(ctx, other.ID, "sam's tablet")
	if err != nil {
		t.Fatal(err)
	}
	theirs := call{t: t, server: server, token: theirToken}

	if listed := theirs.items("/Playlists"); len(listed) != 0 {
		t.Fatalf("another account was shown %v", listed)
	}
	res, _ = theirs.do(http.MethodGet, "/Playlists/"+id+"/Items", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("another account read the playlist: %d", res.StatusCode)
	}
}

// The playlist survives a hidden library; the entry inside it does not.
func TestAPlaylistSurvivesALibraryBeingHidden(t *testing.T) {
	server, token, db := library(t)
	c := call{t: t, server: server, token: token}
	film := c.anItem(t)
	track := c.someTrack(t)

	res, body := c.do(http.MethodPost, "/Playlists", map[string]any{
		"Name": "Both", "Ids": []string{film, track},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("create = %d %s", res.StatusCode, body)
	}
	var created map[string]any
	decode(t, body, &created)
	id := created["Id"].(string)

	narrow(t, db, "Films")

	if listed := c.items("/Playlists"); len(listed) != 1 {
		t.Fatalf("the playlist itself vanished: %v", listed)
	}
	entries := c.items("/Playlists/" + id + "/Items")
	if len(entries) != 1 || entries[0]["Id"] != film {
		t.Fatalf("a films-only device saw %v", entries)
	}
}

/* ---------- collections ---------- */

func TestACollectionIsBrowsedIntoLikeAFolder(t *testing.T) {
	server, token, db := library(t)
	ctx := context.Background()
	c := call{t: t, server: server, token: token}
	film := c.anItem(t)

	made, err := db.CreateCollection(ctx, "Science fiction", "Two of them")
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := parseIDForTest(film)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetCollectionItems(ctx, made.ID, []int64{id}); err != nil {
		t.Fatal(err)
	}

	listed := c.items("/Items?IncludeItemTypes=BoxSet&Recursive=true")
	if len(listed) != 1 {
		t.Fatalf("collections = %v", listed)
	}
	if listed[0]["Type"] != "BoxSet" || listed[0]["Name"] != "Science fiction" {
		t.Fatalf("collection row = %v", listed[0])
	}
	if listed[0]["IsFolder"] != true {
		t.Fatal("a collection that is not a folder cannot be opened")
	}

	collection := listed[0]["Id"].(string)
	inside := c.items("/Items?ParentId=" + collection)
	if len(inside) != 1 || inside[0]["Id"] != film {
		t.Fatalf("inside the collection: %v", inside)
	}

	// And opening it directly answers with the collection, not a 404.
	opened := c.json("/Users/" + c.me() + "/Items/" + collection)
	if opened["Name"] != "Science fiction" {
		t.Fatalf("opening the collection gave %v", opened)
	}
}

// A collection everybody can see is still invisible to a device that may not
// see what is in it, because an empty group announces that something exists.
func TestACollectionOfHiddenFilmsIsNotOffered(t *testing.T) {
	server, token, db := library(t)
	ctx := context.Background()
	c := call{t: t, server: server, token: token}
	film := c.anItem(t)

	made, err := db.CreateCollection(ctx, "Films only", "")
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := parseIDForTest(film)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetCollectionItems(ctx, made.ID, []int64{id}); err != nil {
		t.Fatal(err)
	}

	narrow(t, db, "Music")

	if listed := c.items("/Items?IncludeItemTypes=BoxSet&Recursive=true"); len(listed) != 0 {
		t.Fatalf("a music-only device was shown %v", listed)
	}
	res, _ := c.do(http.MethodGet, "/Users/"+c.me()+"/Items/"+collectionIDForTest(made.ID), nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("reading it directly gave %d", res.StatusCode)
	}
}

// A client that asks for something this server has none of must be told
// there are none, rather than handed the whole library.
func TestAskingForSomethingThatDoesNotExistAnswersEmpty(t *testing.T) {
	server, token, _ := library(t)
	c := call{t: t, server: server, token: token}

	for _, path := range []string{
		"/Items?IncludeItemTypes=LiveTvChannel&Recursive=true",
		"/Items?IncludeItemTypes=BoxSet&Recursive=true",
		"/Items?IncludeItemTypes=Playlist&Recursive=true",
	} {
		if listed := c.items(path); len(listed) != 0 {
			t.Errorf("GET %s returned %d rows", path, len(listed))
		}
	}
}

func TestPagingAListOfPlaylists(t *testing.T) {
	server, token, _ := library(t)
	c := call{t: t, server: server, token: token}
	for _, name := range []string{"One", "Two", "Three"} {
		if res, body := c.do(http.MethodPost, "/Playlists", map[string]any{"Name": name}); res.StatusCode != http.StatusOK {
			t.Fatalf("create %s = %d %s", name, res.StatusCode, body)
		}
	}

	page := c.json("/Items?IncludeItemTypes=Playlist&StartIndex=1&Limit=1")
	rows, _ := page["Items"].([]any)
	if len(rows) != 1 {
		t.Fatalf("a window of one returned %d", len(rows))
	}
	if total, _ := page["TotalRecordCount"].(float64); int(total) != 3 {
		t.Fatalf("total = %v, want 3", page["TotalRecordCount"])
	}
}

// The test package cannot reach the server's own id helpers, and writing
// them out here is the point: if the wire format changes, these break, which
// is a change to what clients see and should not pass quietly.
func parseIDForTest(value string) (int64, int, error) {
	trimmed := strings.TrimLeft(strings.TrimPrefix(value, "f"), "0")
	id, err := strconv.ParseInt(trimmed, 16, 64)
	return id, 0, err
}

func collectionIDForTest(id int64) string { return fmt.Sprintf("d%031x", id) }

// decode reads a reply body a test needs more than items() gives it.
func decode(t *testing.T, body []byte, into any) {
	t.Helper()
	if err := json.Unmarshal(body, into); err != nil {
		t.Fatalf("reply was not JSON: %v (%s)", err, body)
	}
}
