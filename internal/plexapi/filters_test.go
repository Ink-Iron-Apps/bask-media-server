package plexapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

func TestASortMenuOffersOnlyWhatTheServerHonours(t *testing.T) {
	srv, token, _ := library(t)

	rows := directories(t, get(t, srv, token, "/library/sections/1/sorts"))
	if len(rows) == 0 {
		t.Fatal("no sorts were offered")
	}

	// The first row is the one a client starts with, and it says so.
	if rows[0]["key"] != "titleSort" {
		t.Fatalf("first sort = %v", rows[0]["key"])
	}
	if rows[0]["default"] != true {
		t.Fatalf("no sort was marked as the default: %v", rows[0])
	}

	// Every row carries what a client needs to use it: the key to send, the
	// reverse of it, and which way round it reads best.
	for _, row := range rows {
		for _, field := range []string{"key", "title", "descKey", "defaultDirection"} {
			if row[field] == nil || row[field] == "" {
				t.Fatalf("a sort row is missing %s: %v", field, row)
			}
		}
		if row["type"] != "sort" {
			t.Fatalf("a sort row is typed %v", row["type"])
		}
	}
}

func TestTheAlphabetStripCountsWhatIsBehindEachLetter(t *testing.T) {
	srv, token, _ := library(t)

	rows := directories(t, get(t, srv, token, "/library/sections/1/firstCharacter"))
	if len(rows) == 0 {
		t.Fatal("the strip was empty for a library with films in it")
	}
	for _, row := range rows {
		if row["title"] == nil || row["title"] == "" {
			t.Fatalf("a letter with no title: %v", row)
		}
		// A letter with nothing behind it must not be offered: a strip that
		// jumps to an empty grid is worse than a shorter strip.
		if size, ok := row["size"].(float64); !ok || size <= 0 {
			t.Fatalf("a letter with nothing behind it: %v", row)
		}
		// And it carries the listing it stands for, so a client can follow
		// a link rather than build the query itself.
		if fast, _ := row["fastKey"].(string); fast == "" {
			t.Fatalf("a letter with no listing behind it: %v", row)
		}
	}
}

func TestALibrarysSettingsRequestIsAnsweredEmptyRatherThanMissing(t *testing.T) {
	srv, token, _ := library(t)

	// A client asks on opening every library. An endpoint that is reliably
	// absent is a log line on every visit; an empty list is the honest
	// answer, because none of BMS's per-library settings is a viewer's to
	// change from a television.
	if code := status(t, srv, token, "/library/sections/1/prefs"); code != http.StatusOK {
		t.Fatalf("prefs = %d", code)
	}
}

func TestTheFurnitureRefusesALibraryThatIsNotThere(t *testing.T) {
	srv, token, _ := library(t)

	for _, path := range []string{
		"/library/sections/999/sorts",
		"/library/sections/999/firstCharacter",
		"/library/sections/999/prefs",
		"/library/sections/999/filters",
		"/library/sections/999/genre",
	} {
		if code := status(t, srv, token, path); code != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404", path, code)
		}
	}
}

// directories pulls the Directory rows out of a MediaContainer.
func directories(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, _ := body["Directory"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, one := range raw {
		row, _ := one.(map[string]any)
		out = append(out, row)
	}
	return out
}

func TestOnlyTheWordKindsInUseAreOfferedAsFilters(t *testing.T) {
	srv, token, db := library(t)
	ctx := context.Background()

	// Nothing carries a word yet, so nothing is offered: a filter row
	// leading to an empty list of values wastes a tap and says nothing.
	if rows := directories(t, get(t, srv, token, "/library/sections/1/filters")); len(rows) != 0 {
		t.Fatalf("filters on a library with no words: %v", rows)
	}

	items, _, err := db.SectionItems(ctx, 1, domain.KindMovie, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 {
		t.Fatal("no films to tag")
	}
	if err := db.SetGenres(ctx, items[0].ID, []string{"Science Fiction"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTags(ctx, store.TagLabel, items[0].ID, []string{"Keep"}); err != nil {
		t.Fatal(err)
	}

	rows := directories(t, get(t, srv, token, "/library/sections/1/filters"))
	offered := map[string]string{}
	for _, row := range rows {
		filter, _ := row["filter"].(string)
		key, _ := row["key"].(string)
		offered[filter] = key
	}
	if offered["genre"] != "/library/sections/1/genre" {
		t.Fatalf("genre filter = %q", offered["genre"])
	}
	if offered["label"] != "/library/sections/1/label" {
		t.Fatalf("label filter = %q", offered["label"])
	}
	// Moods and styles are not in use, so they are not offered.
	if _, found := offered["mood"]; found {
		t.Fatalf("a mood filter was offered with no moods: %v", offered)
	}

	// Each kind's values come back on its own route, with the listing behind
	// each one.
	labels := directories(t, get(t, srv, token, "/library/sections/1/label"))
	if len(labels) != 1 || labels[0]["title"] != "Keep" {
		t.Fatalf("labels = %v", labels)
	}
	if fast, _ := labels[0]["fastKey"].(string); fast != "/library/sections/1/all?label=Keep" {
		t.Fatalf("label fastKey = %q", fast)
	}
}

func TestAnItemCarriesEveryKindOfWordItHas(t *testing.T) {
	srv, token, db := library(t)
	ctx := context.Background()

	items, _, err := db.SectionItems(ctx, 1, domain.KindMovie, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	film := items[0].ID
	if err := db.SetGenres(ctx, film, []string{"Drama"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTags(ctx, store.TagLabel, film, []string{"Keep"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTags(ctx, store.TagTag, film, []string{"Sunday"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTags(ctx, store.TagMood, film, []string{"Melancholy"}); err != nil {
		t.Fatal(err)
	}

	body := get(t, srv, token, "/library/metadata/"+itoa(film))
	rows, _ := body["Metadata"].([]any)
	if len(rows) != 1 {
		t.Fatalf("metadata = %v", body)
	}
	row, _ := rows[0].(map[string]any)

	names := func(field string) []string {
		raw, _ := row[field].([]any)
		out := make([]string, 0, len(raw))
		for _, one := range raw {
			entry, _ := one.(map[string]any)
			text, _ := entry["tag"].(string)
			out = append(out, text)
		}
		return out
	}

	if got := names("Genre"); len(got) != 1 || got[0] != "Drama" {
		t.Fatalf("Genre = %v", got)
	}
	// A label and a tag both arrive as Label: Plex's clients have no element
	// for the second and read a free word from the first.
	if got := names("Label"); len(got) != 2 || got[0] != "Keep" || got[1] != "Sunday" {
		t.Fatalf("Label = %v", got)
	}
	if got := names("Mood"); len(got) != 1 || got[0] != "Melancholy" {
		t.Fatalf("Mood = %v", got)
	}
	if _, found := row["Style"]; found {
		t.Fatal("an empty Style list was sent")
	}
}

func TestNarrowingALibraryByALabel(t *testing.T) {
	srv, token, db := library(t)
	ctx := context.Background()

	items, _, err := db.SectionItems(ctx, 1, domain.KindMovie, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) < 2 {
		t.Fatalf("want two films, have %d", len(items))
	}
	if err := db.SetTags(ctx, store.TagLabel, items[0].ID, []string{"Keep"}); err != nil {
		t.Fatal(err)
	}

	body := get(t, srv, token, "/library/sections/1/all?label=Keep")
	rows, _ := body["Metadata"].([]any)
	if len(rows) != 1 {
		t.Fatalf("label=Keep returned %d films", len(rows))
	}

	// A word nothing carries narrows to nothing, rather than quietly showing
	// the whole library.
	body = get(t, srv, token, "/library/sections/1/all?label=Nothing")
	rows, _ = body["Metadata"].([]any)
	if len(rows) != 0 {
		t.Fatalf("label=Nothing returned %d films", len(rows))
	}
}
