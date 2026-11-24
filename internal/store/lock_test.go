package store_test

import (
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// A scan writes in batches. What matters is that the write lock is held for
// the writes and nothing else: a scanner probes its files between them, and
// anything else in the process that needs to write — recording a library
// visit, creating a session for somebody signing in — must not be refused
// while that happens.
//
// Found on a real library: a scan of 2110 films made the admin interface
// unusable, because signing in is a write and it failed with SQLITE_BUSY.

func TestAnotherWriteSucceedsBetweenBatches(t *testing.T) {
	s := newShelf(t)

	writer := s.db.NewWriter(5)
	defer writer.Rollback()

	// Two batches' worth, handed over whole the way a scan now does.
	batch := make([]store.Found, 0, 5)
	for i := range 5 {
		batch = append(batch, store.Found{
			SectionID: s.films, Kind: domain.KindMovie,
			Title: "Film " + string(rune('A'+i)), Year: 2000 + i,
			Path: t.TempDir() + "/f.mkv", Container: "mkv", Size: 1,
		})
	}
	if err := writer.AddAll(s.ctx, batch); err != nil {
		t.Fatal(err)
	}

	// The transaction is closed, so an unrelated write goes through. This is
	// the whole point: with the old loop the transaction stayed open between
	// items and this failed.
	if err := s.db.MarkSwept(s.ctx, s.films, time.Now().UTC()); err != nil {
		t.Fatalf("a write was refused after a batch: %v", err)
	}
	if _, err := s.db.CreateSession(s.ctx, s.user, 0, time.Hour); err != nil {
		t.Fatalf("signing in was refused after a batch: %v", err)
	}
}

func TestAddAllLeavesNoTransactionOpen(t *testing.T) {
	s := newShelf(t)
	writer := s.db.NewWriter(500)
	defer writer.Rollback()

	if err := writer.AddAll(s.ctx, []store.Found{{
		SectionID: s.films, Kind: domain.KindMovie, Title: "One", Year: 2020,
		Path: t.TempDir() + "/one.mkv", Container: "mkv", Size: 1,
	}}); err != nil {
		t.Fatal(err)
	}

	// Committed on return rather than when a batch fills: a scan whose last
	// batch is short must not leave its rows in an open transaction, and
	// nothing else may be locked out waiting for it.
	if err := s.db.MarkSwept(s.ctx, s.films, time.Now().UTC()); err != nil {
		t.Fatalf("the transaction was still open: %v", err)
	}
	items, _, err := s.db.SectionItems(s.ctx, s.films, domain.KindMovie, store.Page{Size: 50})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, item := range items {
		if item.Title == "One" {
			found = true
		}
	}
	if !found {
		t.Fatal("the row was not committed")
	}
}

func TestAnEmptyBatchIsNotATransaction(t *testing.T) {
	s := newShelf(t)
	writer := s.db.NewWriter(500)
	defer writer.Rollback()

	// A scan that found nothing, or whose final batch is empty, must not
	// open a transaction at all.
	if err := writer.AddAll(s.ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.db.MarkSwept(s.ctx, s.films, time.Now().UTC()); err != nil {
		t.Fatalf("an empty batch held a lock: %v", err)
	}
}
