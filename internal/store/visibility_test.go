package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

func open(t *testing.T) (*store.DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bms.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, ctx
}

// The one rule in this package that is dangerous to get wrong: a handle
// restricted to no libraries must see no libraries. Reading an empty set as
// "unrestricted" is how an empty grant list becomes full access.
func TestAHandleRestrictedToNothingSeesNothing(t *testing.T) {
	db, ctx := open(t)
	if _, err := db.CreateSection(ctx, "Films", domain.SectionMovies, []string{t.TempDir()}); err != nil {
		t.Fatal(err)
	}

	sections, err := db.Visible(nil).Sections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 0 {
		t.Fatalf("a handle allowed nowhere saw %d libraries", len(sections))
	}

	// And the unrestricted handle is a different thing, not the same thing.
	all, err := db.Sections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("the unrestricted handle saw %d libraries", len(all))
	}
}

// Scoped is where the policy lives: no grants means no restriction, which is
// what keeps every existing token working after the migration.
func TestNoGrantsMeansEveryLibrary(t *testing.T) {
	db, ctx := open(t)
	if _, err := db.CreateSection(ctx, "Films", domain.SectionMovies, []string{t.TempDir()}); err != nil {
		t.Fatal(err)
	}

	user, err := db.CreateUser(ctx, "owner", true)
	if err != nil {
		t.Fatal(err)
	}
	token, err := db.IssueToken(ctx, user.ID, "tablet")
	if err != nil {
		t.Fatal(err)
	}
	cred, err := db.Credential(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if len(cred.Sections) != 0 {
		t.Fatalf("a fresh token came with grants: %v", cred.Sections)
	}

	sections, err := db.Scoped(cred).Sections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 1 {
		t.Fatalf("an ungranted token saw %d libraries, want 1", len(sections))
	}
}

// A grant for a library that is later deleted leaves no row behind, so a
// device is not left restricted to something that no longer exists.
func TestDeletingALibraryTakesItsGrantsWithIt(t *testing.T) {
	db, ctx := open(t)
	films, err := db.CreateSection(ctx, "Films", domain.SectionMovies, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	user, err := db.CreateUser(ctx, "owner", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.IssueToken(ctx, user.ID, "tablet"); err != nil {
		t.Fatal(err)
	}
	tokens, err := db.Tokens(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetTokenSections(ctx, tokens[0].ID, []int64{films.ID}); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteSection(ctx, films.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := db.TokenSections(ctx, tokens[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Fatalf("a grant outlived its library: %v", stored)
	}
}

// What a client calls itself is recorded and nothing more: it does not become
// part of the credential.
func TestADeviceNameIsRecordedAndNotTrusted(t *testing.T) {
	db, ctx := open(t)
	user, err := db.CreateUser(ctx, "owner", true)
	if err != nil {
		t.Fatal(err)
	}
	token, err := db.IssueToken(ctx, user.ID, "television")
	if err != nil {
		t.Fatal(err)
	}
	cred, err := db.Credential(ctx, token)
	if err != nil {
		t.Fatal(err)
	}

	db.NoteTokenDevice(ctx, cred.TokenID, "Living room TV")
	tokens, err := db.Tokens(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tokens[0].Device != "Living room TV" {
		t.Fatalf("device = %q", tokens[0].Device)
	}

	// An absurdly long claim is cut rather than stored whole, since a client
	// writes this field itself.
	long := make([]byte, 500)
	for i := range long {
		long[i] = 'x'
	}
	db.NoteTokenDevice(ctx, cred.TokenID, string(long))
	tokens, err = db.Tokens(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens[0].Device) != 64 {
		t.Fatalf("a 500-character device name was stored as %d characters", len(tokens[0].Device))
	}
}
