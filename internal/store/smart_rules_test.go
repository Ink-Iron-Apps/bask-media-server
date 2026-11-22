package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// Rules the server cannot parse make a collection that holds nothing and says
// so, rather than one that quietly holds the whole library or a listing that
// fails for everybody because of one bad row.
//
// Inside the package because the only way to store rules this server would
// never write is to write them past the function that validates them, which
// is what an older build or a hand-edited database amounts to.
func TestUnreadableRulesHoldNothingAndSaySo(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "bms.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	made, err := db.CreateCollection(ctx, "Corrupt", "")
	if err != nil {
		t.Fatal(err)
	}
	sound, err := db.CreateCollection(ctx, "Sound", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.ExecContext(ctx,
		`UPDATE collections SET rules = ? WHERE id = ?`, "{not json at all", made.ID); err != nil {
		t.Fatal(err)
	}

	found, err := db.AnyCollection(ctx, made.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !found.Unreadable || found.Smart != nil {
		t.Fatalf("unreadable = %v, rules = %+v", found.Unreadable, found.Smart)
	}

	items, total, err := db.CollectionItems(ctx, made.ID, Page{Size: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 || total != 0 {
		t.Fatalf("a collection with unreadable rules held %d of %d", len(items), total)
	}

	// A viewer is not offered it at all, which is what every other empty
	// collection does; the editor above is the only thing told about it.
	if _, err := db.Collection(ctx, made.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a viewer was offered a collection with unreadable rules: %v", err)
	}

	// The rest of the listing still works, which is the whole point of not
	// returning an error from the scan.
	all, err := db.EveryCollection(ctx)
	if err != nil {
		t.Fatalf("one bad row took the listing down: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("listed %d collections, wanted both", len(all))
	}
	for _, c := range all {
		if c.ID == sound.ID && c.Unreadable {
			t.Fatal("a sound collection was marked unreadable")
		}
	}
}

// What goes into the rules column, and what comes back out of it.
func TestARuleSetSurvivesStorage(t *testing.T) {
	want := domain.Smart{
		Match: domain.SmartAny,
		Rules: []domain.SmartRule{
			{Field: domain.SmartYear, Op: domain.SmartAfter, Value: "2000"},
			{Field: domain.SmartWatched, Op: domain.SmartNo},
		},
		Sort: domain.SmartByAdded, Desc: true, Limit: 20,
	}
	stored, err := marshalSmart(&want)
	if err != nil {
		t.Fatal(err)
	}
	got, readable := parseSmart(stored)
	if !readable || got == nil {
		t.Fatalf("stored %q and could not read it back", stored)
	}
	if got.Match != want.Match || got.Sort != want.Sort || !got.Desc || got.Limit != want.Limit {
		t.Fatalf("read back %+v", *got)
	}
	if len(got.Rules) != 2 || got.Rules[1].Op != domain.SmartNo {
		t.Fatalf("read back rules %+v", got.Rules)
	}

	// No rules at all is the empty string, because empty is what tells every
	// reader a collection is filled by hand.
	if blank, err := marshalSmart(nil); err != nil || blank != "" {
		t.Fatalf("no rules stored as %q (%v)", blank, err)
	}
	if rules, readable := parseSmart("   "); !readable || rules != nil {
		t.Fatalf("whitespace read as %+v", rules)
	}
}
