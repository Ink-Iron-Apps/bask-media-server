package web_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/web"
)

// subtitled puts one picture subtitle and one text subtitle on the episode
// the harness plays, and returns their ids.
func subtitled(t *testing.T, h *harness) (picture, text int64) {
	t.Helper()
	ctx := context.Background()

	parts, err := h.db.Parts(ctx, h.firstEp)
	if err != nil || len(parts) == 0 {
		t.Fatalf("parts = %v (%v)", parts, err)
	}
	path := filepath.Join(t.TempDir(), "episode.en.srt")
	if err := os.WriteFile(path, []byte(vttSample), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.db.ReplaceSubtitles(ctx, h.firstEp, parts[0].ID, []domain.Subtitle{
		{StreamIndex: 3, Codec: "hdmv_pgs_subtitle", Language: "fre"},
		{External: true, StreamIndex: -1, Path: path, Codec: "srt", Language: "eng"},
	}); err != nil {
		t.Fatal(err)
	}

	stored, err := h.db.Subtitles(ctx, []int64{h.firstEp})
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range stored[h.firstEp] {
		if sub.Text() {
			text = sub.ID
		} else {
			picture = sub.ID
		}
	}
	if picture == 0 || text == 0 {
		t.Fatalf("expected one of each, got %v", stored[h.firstEp])
	}
	return picture, text
}

// A picture subtitle is a sequence of images, which a browser cannot draw.
// Asking for it is asking for a conversion, whatever the file would otherwise
// have been.
func TestBurningInASubtitleConvertsAFileThatWouldHaveBeenSentAsItIs(t *testing.T) {
	h := newHarnessWith(t, func(s *web.Server) {
		s.Prober = prober{contents("h264", "aac", 1080, 2)}
		s.Transcoder = fakeEncoder(t)
	})
	h.signIn()
	picture, _ := subtitled(t, h)

	// Without it, this file is direct played: that is what makes the next
	// answer a decision rather than a coincidence.
	_, plain := h.call(http.MethodPost, "/web/api/play", map[string]any{
		"itemId": h.firstEp, "capabilities": browser,
	})
	if plain["mode"] != "direct" {
		t.Fatalf("the file was not direct played to begin with: %v", plain)
	}

	res, body := h.call(http.MethodPost, "/web/api/play", map[string]any{
		"itemId": h.firstEp, "capabilities": browser, "burnSubtitleId": picture,
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("play = %d: %v", res.StatusCode, body)
	}
	if body["mode"] != "transcode" {
		t.Fatalf("burning in did not convert: %v", body)
	}
	if why, _ := body["why"].(string); !strings.Contains(why, "subtitle") {
		t.Errorf("why = %q, want the subtitle named", why)
	}
	if url, _ := body["url"].(string); !strings.HasPrefix(url, "/web/hls/") {
		t.Errorf("url = %v, want a stream being made", body["url"])
	}
	if body["session"] == nil {
		t.Error("there is no session to stop")
	}
}

// Two subtitles are two different pictures, so they are two encoders rather
// than one shared one — otherwise switching tracks hands the viewer the
// stream that is already drawing the other.
func TestSwitchingSubtitlesIsADifferentStream(t *testing.T) {
	h := newHarnessWith(t, func(s *web.Server) {
		s.Prober = prober{contents("h264", "aac", 1080, 2)}
		s.Transcoder = fakeEncoder(t)
	})
	h.signIn()
	picture, text := subtitled(t, h)

	_, first := h.call(http.MethodPost, "/web/api/play", map[string]any{
		"itemId": h.firstEp, "capabilities": browser, "burnSubtitleId": picture,
	})
	_, second := h.call(http.MethodPost, "/web/api/play", map[string]any{
		"itemId": h.firstEp, "capabilities": browser, "burnSubtitleId": text,
	})
	if first["session"] == nil || second["session"] == nil {
		t.Fatalf("one of them is not a stream: %v %v", first, second)
	}
	if first["session"] == second["session"] {
		t.Fatalf("both subtitles were handed the same encoder: %v", first["session"])
	}

	// And asking for the same one again is the same stream, because that is
	// what a page reload costs.
	_, again := h.call(http.MethodPost, "/web/api/play", map[string]any{
		"itemId": h.firstEp, "capabilities": browser, "burnSubtitleId": picture,
	})
	if again["session"] != first["session"] {
		t.Fatalf("asking twice started two encoders: %v then %v", first["session"], again["session"])
	}
}

// A subtitle id that is not this item's is ignored rather than refused: the
// page asked for something that is not there, and the film still plays.
func TestASubtitleThatIsNotThereIsIgnored(t *testing.T) {
	h := newHarnessWith(t, func(s *web.Server) {
		s.Prober = prober{contents("h264", "aac", 1080, 2)}
		s.Transcoder = fakeEncoder(t)
	})
	h.signIn()
	subtitled(t, h)

	_, body := h.call(http.MethodPost, "/web/api/play", map[string]any{
		"itemId": h.firstEp, "capabilities": browser, "burnSubtitleId": 9999,
	})
	if body["mode"] != "direct" {
		t.Fatalf("a subtitle nobody has converted the stream: %v", body)
	}
}

// With no ffmpeg there is nothing to draw with, and the page is told so
// rather than being handed a file with the subtitle missing.
func TestWithNoConverterBurningInSaysSo(t *testing.T) {
	h := newHarnessWith(t, func(s *web.Server) {
		s.Prober = prober{contents("h264", "aac", 1080, 2)}
	})
	h.signIn()
	picture, _ := subtitled(t, h)

	_, body := h.call(http.MethodPost, "/web/api/play", map[string]any{
		"itemId": h.firstEp, "capabilities": browser, "burnSubtitleId": picture,
	})
	if body["mode"] != "unsupported" {
		t.Fatalf("mode = %v", body["mode"])
	}
	if why, _ := body["why"].(string); !strings.Contains(why, "ffmpeg") {
		t.Errorf("why = %q", why)
	}
	// And the item page still lists the subtitle, so the viewer can see it
	// exists even though it cannot be drawn here.
	_, detail := h.call(http.MethodGet, "/web/api/items/"+strconv.FormatInt(h.firstEp, 10), nil)
	if list := items(t, detail, "subtitles"); len(list) != 2 {
		t.Fatalf("subtitles = %v", list)
	}
}
