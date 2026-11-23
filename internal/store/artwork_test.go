package store_test

import (
	"testing"

	"github.com/inknironapps/bask-media-server/internal/store"
)

func TestSetArtworkWritesOnlyWhatItIsGiven(t *testing.T) {
	s := newShelf(t)

	if err := s.db.SetArtwork(s.ctx, s.filmA, "poster-one", ""); err != nil {
		t.Fatal(err)
	}
	item, err := s.db.Item(s.ctx, s.filmA)
	if err != nil {
		t.Fatal(err)
	}
	if item.PosterKey != "poster-one" {
		t.Fatalf("poster = %q", item.PosterKey)
	}
	if item.ArtKey != "" {
		t.Fatalf("a backdrop appeared from nowhere: %q", item.ArtKey)
	}
	// Locked, because the point of choosing a picture by hand is that the
	// next metadata pass does not replace it.
	if !item.Locked {
		t.Fatal("the item was not locked")
	}
	// But still unmatched: a chosen poster is not an answer about what this
	// film is, so the next pass still looks at it.
	if !item.MetadataAt.IsZero() {
		t.Fatalf("choosing a picture marked the item as answered: %v", item.MetadataAt)
	}

	// A backdrop next, which must leave the poster where it is.
	if err := s.db.SetArtwork(s.ctx, s.filmA, "", "art-one"); err != nil {
		t.Fatal(err)
	}
	item, err = s.db.Item(s.ctx, s.filmA)
	if err != nil {
		t.Fatal(err)
	}
	if item.PosterKey != "poster-one" || item.ArtKey != "art-one" {
		t.Fatalf("poster %q, art %q", item.PosterKey, item.ArtKey)
	}
}

func TestSetArtworkRefusesAnEmptyChoiceAndAMissingItem(t *testing.T) {
	s := newShelf(t)

	if err := s.db.SetArtwork(s.ctx, s.filmA, "", ""); err == nil {
		t.Fatal("setting nothing was accepted")
	}
	if err := s.db.SetArtwork(s.ctx, 999999, "poster", ""); err == nil {
		t.Fatal("a picture was put on an item that does not exist")
	}
}

func TestAMetadataPassCannotReplaceAChosenPicture(t *testing.T) {
	s := newShelf(t)
	if err := s.db.SetArtwork(s.ctx, s.filmA, "chosen", ""); err != nil {
		t.Fatal(err)
	}

	// The enrichment pass looks for items nobody has answered for and is not
	// locked. A locked one is not offered to it at all, which is the whole
	// mechanism: the pass cannot overwrite what it never sees.
	waiting, err := s.db.NeedsMetadata(s.ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range waiting {
		if item.ID == s.filmA {
			t.Fatal("an item with a hand-chosen poster was offered to the metadata pass")
		}
	}

	// And unlocking puts it back in the queue, which is how somebody changes
	// their mind.
	unlock := false
	if err := s.db.EditItem(s.ctx, s.filmA, store.Edit{Locked: &unlock}); err != nil {
		t.Fatal(err)
	}
	waiting, err = s.db.NeedsMetadata(s.ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range waiting {
		if item.ID == s.filmA {
			found = true
		}
	}
	if !found {
		t.Fatal("unlocking did not put the item back in the queue")
	}
}
