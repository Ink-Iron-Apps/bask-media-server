package webhook

import (
	"fmt"
	"strings"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/nowplaying"
)

// The sentences. Every event's wording lives in this one file so that the
// thing a person reads on their phone was written on purpose, rather than
// assembled from whatever fields happened to be in scope at the call site.
//
// Two audiences, one event. The text is for a person: short, complete, and
// readable without the library open. The data is for a program: identifiers,
// not prose, so an automation keys on an item id rather than parsing a
// sentence that might be reworded.

// Playback turns a change in the register of live streams into an event.
//
// Returns false for a change nothing subscribes to, so the caller can hand
// over every change the register reports without filtering them itself.
func Playback(change nowplaying.Change) (Event, bool) {
	stream := change.Stream

	var kind, verb string
	switch change.Kind {
	case nowplaying.Started:
		kind, verb = KindPlaybackStart, "started playing"
	case nowplaying.Paused:
		kind, verb = KindPlaybackPause, "paused"
	case nowplaying.Resumed:
		kind, verb = KindPlaybackResume, "resumed"
	case nowplaying.Stopped:
		kind, verb = KindPlaybackStop, "stopped playing"
	default:
		return Event{}, false
	}

	// "Playing" rather than watching or listening. The register holds streams,
	// not kinds, and guessing from the fields it does hold would eventually
	// tell somebody they were watching an album.

	who := stream.UserName
	if who == "" {
		who = "Somebody"
	}
	text := fmt.Sprintf("%s %s", who, verb)
	if where := device(stream); where != "" {
		text += " on " + where
	}
	if stream.Reason != "" {
		// A terminated stream: the reason is the whole point of the message.
		text = fmt.Sprintf("%s was stopped on %s: %s", who, device(stream), stream.Reason)
	}

	return Event{
		Kind:  kind,
		At:    time.Now().UTC(),
		Title: heading(stream.Title, stream.Subtitle),
		Text:  text + ".",
		Data: map[string]any{
			"user":     stream.UserName,
			"userId":   stream.UserID,
			"itemId":   stream.ItemID,
			"title":    stream.Title,
			"subtitle": stream.Subtitle,
			"client":   stream.Client,
			"device":   stream.Device,
			"address":  stream.Address,
			"surface":  stream.Surface,
			"mode":     stream.Mode,
			"session":  stream.Session,
			"paused":   stream.Paused,
			"position": int(stream.Position.Seconds()),
			"duration": int(stream.Duration.Seconds()),
			"reason":   stream.Reason,
		},
	}, true
}

// NewItem is something that arrived in the library.
func NewItem(item domain.Item, section string) Event {
	what := item.Title
	if item.Year > 0 {
		what = fmt.Sprintf("%s (%d)", item.Title, item.Year)
	}
	text := what + " was added"
	if section != "" {
		text += " to " + section
	}
	return Event{
		Kind:  KindLibraryNew,
		At:    time.Now().UTC(),
		Title: what,
		Text:  text + ".",
		Data: map[string]any{
			"itemId":    item.ID,
			"kind":      string(item.Kind),
			"title":     item.Title,
			"year":      item.Year,
			"index":     item.Index,
			"sectionId": item.SectionID,
			"section":   section,
			"duration":  int(item.Duration.Seconds()),
			"rating":    item.Rating,
		},
	}
}

// JobFinished is a background pass that has ended, either way.
//
// Takes what it needs rather than a job: the package that runs the passes
// reads the database, this package reads the database, and one of them
// importing the other is a cycle waiting for the next feature.
func JobFinished(kind, target, note string, elapsed time.Duration, failure error) Event {
	what := strings.TrimSpace(kind)
	if what == "" {
		what = "A pass"
	}
	if target != "" {
		what += " of " + target
	}

	if failure != nil {
		return Event{
			Kind:   KindJobFailed,
			At:     time.Now().UTC(),
			Title:  "Failed: " + what,
			Text:   fmt.Sprintf("%s failed after %s: %s.", sentence(what), spell(elapsed), failure),
			Urgent: true,
			Data: map[string]any{
				"job":     kind,
				"target":  target,
				"note":    note,
				"error":   failure.Error(),
				"seconds": int(elapsed.Seconds()),
			},
		}
	}

	text := fmt.Sprintf("%s finished in %s", sentence(what), spell(elapsed))
	if note != "" {
		text += ": " + note
	}
	return Event{
		Kind:  KindJobDone,
		At:    time.Now().UTC(),
		Title: "Finished: " + what,
		Text:  text + ".",
		Data: map[string]any{
			"job":     kind,
			"target":  target,
			"note":    note,
			"seconds": int(elapsed.Seconds()),
		},
	}
}

// Test is what the button on the settings page sends.
func Test(server string) Event {
	where := server
	if where == "" {
		where = "this server"
	}
	return Event{
		Kind:  KindTest,
		At:    time.Now().UTC(),
		Title: "Test",
		Text:  "This is a test event from " + where + ". If you can read this, the webhook works.",
		Data:  map[string]any{"server": server},
	}
}

// heading is a title and its subtitle as one line, which is what a show and
// an episode have to become when there is one line to say them in.
func heading(title, subtitle string) string {
	switch {
	case title == "":
		return subtitle
	case subtitle == "":
		return title
	default:
		return title + " — " + subtitle
	}
}

// device is the friendliest name the client gave for itself.
func device(stream nowplaying.Stream) string {
	switch {
	case stream.Device != "":
		return stream.Device
	case stream.Client != "":
		return stream.Client
	default:
		return ""
	}
}

// sentence capitalises a phrase that is about to start one.
func sentence(text string) string {
	if text == "" {
		return text
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

// spell is a duration as a person would say it. Seconds under a minute,
// because "0h0m42s" is not an answer anybody wanted.
func spell(d time.Duration) string {
	switch {
	case d < time.Second:
		return "under a second"
	case d < time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	default:
		return fmt.Sprintf("%.1f hours", d.Hours())
	}
}
