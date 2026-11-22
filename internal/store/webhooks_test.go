package store_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// The rule this package exists to keep: a token and a secret are credentials
// for somebody else's service, and the read a page uses must not carry them.
func TestTheListAPageReadsCarriesNoCredentials(t *testing.T) {
	db, ctx := open(t)

	if _, err := db.AddWebhook(ctx, store.NewWebhook{
		URL: "https://example.test/hook", Format: domain.WebhookJSON,
		Token: "tk_secret", Secret: "shhh", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	hooks, err := db.Webhooks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(hooks) != 1 {
		t.Fatalf("expected one webhook, got %d", len(hooks))
	}
	if !hooks[0].HasToken || !hooks[0].HasSecret {
		t.Errorf("the page should be told that a token and a secret are set: %+v", hooks[0])
	}

	// The delivery path, which is the only thing that may hold them.
	dests, err := db.Destinations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dests) != 1 || dests[0].Token != "tk_secret" || dests[0].Secret != "shhh" {
		t.Fatalf("the notifier needs the credentials: %+v", dests)
	}
}

func TestADisabledDestinationIsNotDeliveredTo(t *testing.T) {
	db, ctx := open(t)

	on, err := db.AddWebhook(ctx, store.NewWebhook{URL: "https://on.test/hook", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddWebhook(ctx, store.NewWebhook{URL: "https://off.test/hook", Enabled: false}); err != nil {
		t.Fatal(err)
	}

	dests, err := db.Destinations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dests) != 1 || dests[0].ID != on.ID {
		t.Fatalf("only the enabled destination should be delivered to, got %+v", dests)
	}

	// Both are still on the page: switching one off is not deleting it.
	hooks, err := db.Webhooks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(hooks) != 2 {
		t.Fatalf("expected both webhooks on the page, got %d", len(hooks))
	}
}

func TestEditingAWebhookLeavesWhatItDoesNotMention(t *testing.T) {
	db, ctx := open(t)

	made, err := db.AddWebhook(ctx, store.NewWebhook{
		URL: "https://example.test/hook", Format: domain.WebhookJSON,
		Events: []string{"playback.*"}, Token: "tk", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	ntfy := domain.WebhookNtfy
	edited, err := db.EditWebhook(ctx, made.ID, store.WebhookEdit{Format: &ntfy})
	if err != nil {
		t.Fatal(err)
	}
	if edited.Format != domain.WebhookNtfy {
		t.Errorf("format did not change: %q", edited.Format)
	}
	if len(edited.Events) != 1 || edited.Events[0] != "playback.*" {
		t.Errorf("the subscription should be untouched: %v", edited.Events)
	}
	if !edited.HasToken {
		t.Error("the token should be untouched")
	}

	// An empty string is a real instruction: forget it.
	blank := ""
	cleared, err := db.EditWebhook(ctx, made.ID, store.WebhookEdit{Token: &blank})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.HasToken {
		t.Error("clearing the token should have removed it")
	}
}

// Failures counts consecutive failures, because that is the only form of the
// number that tells an operator anything.
func TestASuccessfulDeliveryClearsTheFailureCount(t *testing.T) {
	db, ctx := open(t)
	made, err := db.AddWebhook(ctx, store.NewWebhook{URL: "https://example.test/hook", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	for range 3 {
		if err := db.RecordDelivery(ctx, made.ID, 0, "could not reach it", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	hook, err := db.Webhook(ctx, made.ID)
	if err != nil {
		t.Fatal(err)
	}
	if hook.Failures != 3 || hook.LastError == "" {
		t.Fatalf("three failures should be recorded as three: %+v", hook)
	}

	if err := db.RecordDelivery(ctx, made.ID, 204, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	hook, err = db.Webhook(ctx, made.ID)
	if err != nil {
		t.Fatal(err)
	}
	if hook.Failures != 0 || hook.LastError != "" || hook.LastStatus != 204 {
		t.Fatalf("a success should clear the run of failures: %+v", hook)
	}
	if hook.LastAt.IsZero() {
		t.Error("a delivery should be dated")
	}
}

func TestAMissingWebhookIsNotFound(t *testing.T) {
	db, ctx := open(t)
	if _, err := db.Webhook(ctx, 404); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
	if err := db.DeleteWebhook(ctx, 404); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
	enabled := true
	if _, err := db.EditWebhook(ctx, 404, store.WebhookEdit{Enabled: &enabled}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// What a scan announces: the things a person would call new, and not the rows
// created to hold them.
func TestAddedSinceNamesFilmsAndEpisodesAndNothingElse(t *testing.T) {
	db, ctx := open(t)
	section, err := db.CreateSection(ctx, "Shows", domain.SectionShows, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	before := time.Now().Add(-time.Minute)
	writer := db.NewWriter(10)
	if err := writer.Add(ctx, store.Found{
		SectionID: section.ID, Kind: domain.KindEpisode,
		Show: "Big Hill", ShowYear: 2026, SeasonNum: 1, EpisodeNum: 2,
		Title: "The Second One", Path: filepath.Join("/library", "s01e02.mkv"),
		Container: "mkv", Size: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	added, err := db.AddedSince(ctx, before, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 1 {
		t.Fatalf("expected the episode alone, got %d: %+v", len(added), added)
	}
	if added[0].Kind != domain.KindEpisode || added[0].Title != "The Second One" {
		t.Errorf("wrong item announced: %+v", added[0])
	}

	// Nothing arrived after the episode, so nothing is new.
	later, err := db.AddedSince(ctx, time.Now().Add(time.Minute), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(later) != 0 {
		t.Errorf("nothing should be new after now: %+v", later)
	}
}
