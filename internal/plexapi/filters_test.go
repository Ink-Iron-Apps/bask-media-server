package plexapi_test

import (
	"net/http"
	"testing"
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
