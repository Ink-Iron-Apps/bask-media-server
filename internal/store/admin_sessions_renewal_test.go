package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/store"
)

// A session a browser keeps using keeps living. Without this, a fortnight
// after signing in the token has to be typed again for no reason the person
// using it can see — the session was never idle, it just aged out.
func TestAUsedSessionIsExtended(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bms.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	user, err := db.CreateUser(ctx, "admin", true)
	if err != nil {
		t.Fatal(err)
	}

	const ttl = 200 * time.Millisecond
	secret, err := db.CreateSession(ctx, user.ID, 0, ttl)
	if err != nil {
		t.Fatal(err)
	}

	// Fresh: over half its life left, so nothing is written and the caller is
	// not asked to send a cookie it does not need to send.
	if _, renewed, err := db.SessionCredentialRenewing(ctx, secret, ttl); err != nil || renewed {
		t.Fatalf("a fresh session renewed: renewed=%v err=%v", renewed, err)
	}

	// Past half its life: renewed, and good for a full ttl from now.
	time.Sleep(ttl/2 + 20*time.Millisecond)
	if _, renewed, err := db.SessionCredentialRenewing(ctx, secret, ttl); err != nil || !renewed {
		t.Fatalf("a session past half its life did not renew: renewed=%v err=%v", renewed, err)
	}

	// Past the original expiry, and still signed in because of that renewal.
	time.Sleep(ttl/2 + 20*time.Millisecond)
	if _, _, err := db.SessionCredentialRenewing(ctx, secret, ttl); err != nil {
		t.Fatalf("the renewed session expired on its original schedule: %v", err)
	}
}

// Renewal is for sessions in use. One nobody touches still runs out.
func TestAnUntouchedSessionStillExpires(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bms.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	user, err := db.CreateUser(ctx, "admin", true)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := db.CreateSession(ctx, user.ID, 0, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(60 * time.Millisecond)
	if _, _, err := db.SessionCredentialRenewing(ctx, secret, 20*time.Millisecond); err == nil {
		t.Fatal("an expired session still resolves")
	}
}
