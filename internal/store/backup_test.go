package store_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// A copy taken while the server runs has to be a database, not a file that
// happens to be the right size: it is opened and read back here, because that
// is the only check that catches a copy taken between the database and its
// write-ahead log.
func TestABackupIsADatabaseThatOpens(t *testing.T) {
	db, ctx := open(t)
	if _, err := db.CreateSection(ctx, "Films", domain.SectionMovies, []string{t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	user, err := db.CreateUser(ctx, "riley", true)
	if err != nil {
		t.Fatal(err)
	}
	token, err := db.IssueToken(ctx, user.ID, "the television")
	if err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(t.TempDir(), "backups")
	path := filepath.Join(dir, store.BackupName(time.Now()))
	size, err := db.Backup(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if size <= 0 {
		t.Fatalf("the backup is %d bytes", size)
	}

	// Opened as an ordinary database, with no log file beside it, and asked
	// for what was in the original.
	copied, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("the backup does not open: %v", err)
	}
	t.Cleanup(func() { copied.Close() })

	sections, err := copied.Sections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 1 || sections[0].Title != "Films" {
		t.Fatalf("the backup holds %v", sections)
	}
	users, err := copied.CountUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if users != 1 {
		t.Fatalf("the backup holds %d accounts", users)
	}
	// And the token that account holds still resolves against the copy,
	// which is the fact that matters: a restored database has to let the
	// televisions back in without anybody re-pairing them.
	if _, err := copied.Credential(ctx, token); err != nil {
		t.Fatalf("the token did not survive the copy: %v", err)
	}
	_ = user

	// And the original is still usable: this is an online copy, not a move.
	if _, err := db.Sections(ctx); err != nil {
		t.Fatalf("the database the backup was taken from: %v", err)
	}
}

// Writing over an existing file is refused. A backup that replaced the one
// good copy with a broken one would be the worst thing this could do.
func TestABackupNeverOverwritesAnything(t *testing.T) {
	db, ctx := open(t)
	dir := t.TempDir()
	path := filepath.Join(dir, store.BackupName(time.Now()))
	if err := os.WriteFile(path, []byte("something precious"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Backup(ctx, path); err == nil {
		t.Fatal("a backup overwrote a file that was already there")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "something precious" {
		t.Fatalf("the file was changed: %q", body)
	}
}

// The newest are kept and the rest go, because a backup that fills the disk
// takes the server down, which is the opposite of the point.
func TestOnlyTheNewestBackupsAreKept(t *testing.T) {
	db, ctx := open(t)
	dir := filepath.Join(t.TempDir(), "backups")

	at := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
	for i := range 5 {
		path := filepath.Join(dir, store.BackupName(at.AddDate(0, 0, i)))
		if _, err := db.Backup(ctx, path); err != nil {
			t.Fatal(err)
		}
	}

	found, err := store.Backups(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 5 {
		t.Fatalf("listed %d backups", len(found))
	}
	// Newest first, which is the order a page shows them in.
	if found[0].Name != store.BackupName(at.AddDate(0, 0, 4)) {
		t.Fatalf("the newest is %q", found[0].Name)
	}

	removed, err := store.PruneBackups(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 3 {
		t.Fatalf("removed %d", removed)
	}
	left, err := store.Backups(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 {
		t.Fatalf("%d left: %v", len(left), left)
	}
	if left[0].Name != store.BackupName(at.AddDate(0, 0, 4)) || left[1].Name != store.BackupName(at.AddDate(0, 0, 3)) {
		t.Fatalf("the wrong two were kept: %v", left)
	}

	// Keeping none keeps everything, because a configuration that reads as
	// "keep no backups" must not be the one that deletes every copy.
	if removed, err := store.PruneBackups(dir, 0); err != nil || removed != 0 {
		t.Fatalf("pruning to zero removed %d (%v)", removed, err)
	}
	if left, _ := store.Backups(dir); len(left) != 2 {
		t.Fatalf("%d left after pruning to zero", len(left))
	}
}

// Only the files this package writes are listed, so nothing else in the
// folder is ever offered for deletion.
func TestOnlyOurOwnFilesAreListed(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"notes.txt", "bms.db", "bms-nonsense.db", "bms-20261101-030000.db.part"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "bms-20261101-030000.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	found, err := store.Backups(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Name != "bms-20261101-030000.db" {
		t.Fatalf("listed %v", found)
	}
}

// A directory that does not exist yet is not an error: a server that has
// never backed up has nothing to list, and the page says so.
func TestNoBackupDirectoryIsNotAFailure(t *testing.T) {
	found, err := store.Backups(filepath.Join(t.TempDir(), "never-made"))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("found %v", found)
	}
}
