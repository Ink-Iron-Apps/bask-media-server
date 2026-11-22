package webhook_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/nowplaying"
	"github.com/inknironapps/bask-media-server/internal/webhook"
)

// The test for the wording, because the wording is the feature: what arrives
// on somebody's phone has to be readable without the library open.
func TestPlaybackReadsLikeASentence(t *testing.T) {
	stream := nowplaying.Stream{
		Key: "jellyfin|tv|12", Surface: "jellyfin",
		UserID: 3, UserName: "Riley", ItemID: 12,
		Title: "Big Hill", Subtitle: "", Device: "Living Room TV",
		Position: 90 * time.Second, Duration: 95 * time.Minute,
	}

	event, ok := webhook.Playback(nowplaying.Change{Kind: nowplaying.Started, Stream: stream})
	if !ok {
		t.Fatal("a start is an event")
	}
	if event.Kind != webhook.KindPlaybackStart {
		t.Errorf("wrong kind: %q", event.Kind)
	}
	if event.Title != "Big Hill" {
		t.Errorf("wrong title: %q", event.Title)
	}
	if event.Text != "Riley started playing on Living Room TV." {
		t.Errorf("wrong sentence: %q", event.Text)
	}
	if event.Data["itemId"] != int64(12) || event.Data["user"] != "Riley" {
		t.Errorf("a program needs the identifiers: %+v", event.Data)
	}
	if event.Data["duration"] != 5700 {
		t.Errorf("the duration should be seconds: %+v", event.Data["duration"])
	}

	// An episode is a show and an episode on one line.
	stream.Subtitle = "The Second One"
	event, _ = webhook.Playback(nowplaying.Change{Kind: nowplaying.Stopped, Stream: stream})
	if event.Title != "Big Hill — The Second One" {
		t.Errorf("a show and its episode belong on one line: %q", event.Title)
	}
	if event.Text != "Riley stopped playing on Living Room TV." {
		t.Errorf("wrong sentence: %q", event.Text)
	}
}

// A stream somebody stopped says who stopped it and why, because that is the
// whole of the message.
func TestATerminatedStreamSaysWhy(t *testing.T) {
	event, ok := webhook.Playback(nowplaying.Change{
		Kind: nowplaying.Stopped,
		Stream: nowplaying.Stream{
			Key: "plex|phone|4", UserName: "Guest", Device: "Phone",
			Title: "Casino", Reason: "An administrator stopped this stream.",
		},
	})
	if !ok {
		t.Fatal("a stop is an event")
	}
	if !strings.Contains(event.Text, "was stopped") || !strings.Contains(event.Text, "administrator") {
		t.Errorf("the reason is the message: %q", event.Text)
	}
	if event.Data["reason"] == "" {
		t.Error("a program should get the reason too")
	}
}

func TestAStreamWithNoNameIsStillAnEvent(t *testing.T) {
	event, _ := webhook.Playback(nowplaying.Change{
		Kind:   nowplaying.Paused,
		Stream: nowplaying.Stream{Key: "web|user:1|3", Title: "Heat"},
	})
	if event.Kind != webhook.KindPlaybackPause {
		t.Fatalf("wrong kind: %q", event.Kind)
	}
	if event.Text != "Somebody paused." {
		t.Errorf("an unnamed viewer is somebody: %q", event.Text)
	}
}

func TestAnUnknownChangeIsNotAnEvent(t *testing.T) {
	if _, ok := webhook.Playback(nowplaying.Change{Kind: nowplaying.ChangeKind("wondered")}); ok {
		t.Error("a change this package does not know should not become an event")
	}
}

func TestANewItemNamesItsLibrary(t *testing.T) {
	event := webhook.NewItem(domain.Item{
		ID: 9, SectionID: 2, Kind: domain.KindMovie, Title: "Big Hill", Year: 2026,
		Duration: 95 * time.Minute, Rating: "15",
	}, "Films")

	if event.Kind != webhook.KindLibraryNew {
		t.Errorf("wrong kind: %q", event.Kind)
	}
	if event.Text != "Big Hill (2026) was added to Films." {
		t.Errorf("wrong sentence: %q", event.Text)
	}
	if event.Data["itemId"] != int64(9) || event.Data["rating"] != "15" {
		t.Errorf("a program needs the identifiers: %+v", event.Data)
	}
}

func TestAFinishedPassAndAFailedOne(t *testing.T) {
	done := webhook.JobFinished("scan", "every library", "420 files in 2 libraries", 42*time.Second, nil)
	if done.Kind != webhook.KindJobDone {
		t.Errorf("wrong kind: %q", done.Kind)
	}
	if done.Urgent {
		t.Error("a pass that worked is not urgent")
	}
	if done.Text != "Scan of every library finished in 42 seconds: 420 files in 2 libraries." {
		t.Errorf("wrong sentence: %q", done.Text)
	}

	failed := webhook.JobFinished("backup", "backups", "copying", 3*time.Minute, errors.New("no space left on device"))
	if failed.Kind != webhook.KindJobFailed {
		t.Errorf("wrong kind: %q", failed.Kind)
	}
	if !failed.Urgent {
		t.Error("a failed pass is the one thing here somebody should look at now")
	}
	if !strings.Contains(failed.Text, "no space left on device") {
		t.Errorf("the reason belongs in the message: %q", failed.Text)
	}
	if failed.Data["error"] == "" {
		t.Error("a program should get the error too")
	}
}

func TestTheTestEventSaysWhatItIs(t *testing.T) {
	event := webhook.Test("Bask")
	if event.Kind != webhook.KindTest {
		t.Errorf("wrong kind: %q", event.Kind)
	}
	if !strings.Contains(event.Text, "Bask") {
		t.Errorf("a test should say which server sent it: %q", event.Text)
	}
}
