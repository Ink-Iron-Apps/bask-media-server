package web_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/ffprobe"
	"github.com/inknironapps/bask-media-server/internal/transcode"
	"github.com/inknironapps/bask-media-server/internal/web"
)

// prober answers with whatever the test says is in the file.
type prober struct{ info ffprobe.Info }

func (p prober) Probe(context.Context, string) (ffprobe.Info, error) { return p.info, nil }

func contents(video, audio string, height, channels int) ffprobe.Info {
	return ffprobe.Info{
		Duration: 42 * time.Minute,
		Streams: []ffprobe.Stream{
			{Index: 0, Type: "video", Codec: video, Height: height, Width: height * 16 / 9},
			{Index: 1, Type: "audio", Codec: audio, Channels: channels},
		},
	}
}

// browser is what a modern browser reports: it plays MKV, H.264 and AAC, and
// not H.265 or Dolby Digital.
var browser = map[string]any{
	"containers": []string{"mp4", "m4v", "mov", "webm", "mkv"},
	"video":      []string{"h264", "vp8", "vp9", "av1"},
	"audio":      []string{"aac", "mp3", "opus", "flac"},
}

// fakeEncoder stands in for ffmpeg: it writes a plausible session directory
// and waits, so the wiring can be tested without encoding anything.
func fakeEncoder(t *testing.T) *transcode.Manager {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake encoder is a shell script")
	}
	script := filepath.Join(t.TempDir(), "fake-ffmpeg")
	body := `#!/bin/sh
printf 'init' > "$PWD/init.mp4"
printf 'segment' > "$PWD/seg00000.m4s"
cat > "$PWD/stream.m3u8" <<EOT
#EXTM3U
#EXT-X-MAP:URI="init.mp4"
#EXTINF:4.000,
seg00000.m4s
EOT
sleep 60
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	manager, err := transcode.New(transcode.Options{
		FFmpeg: script, Root: filepath.Join(t.TempDir(), "sessions"),
		// Off: detection runs the binary it is given, and this one sleeps.
		Accel: "off",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Shutdown)
	return manager
}

// The answer that costs this server nothing has to be the usual one: a file
// the browser can decode is sent as it is, exactly as it was before any of
// the conversion machinery existed.
func TestAFileTheBrowserCanDecodeIsSentAsItIs(t *testing.T) {
	h := newHarnessWith(t, func(s *web.Server) {
		s.Prober = prober{contents("h264", "aac", 1080, 2)}
		s.Transcoder = fakeEncoder(t)
	})
	h.signIn()

	res, body := h.call(http.MethodPost, "/web/api/play", map[string]any{
		"itemId": h.firstEp, "capabilities": browser,
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("play = %d: %v", res.StatusCode, body)
	}
	if body["mode"] != "direct" || body["label"] != "Direct play" {
		t.Fatalf("an MKV of H.264 and AAC was not direct played: %v", body)
	}
	if url, _ := body["url"].(string); !strings.HasPrefix(url, "/web/stream/") {
		t.Errorf("url = %v, want the file itself", body["url"])
	}
}

// And the file it cannot decode is converted, with the reason said in words.
func TestAFileTheBrowserCannotDecodeIsConverted(t *testing.T) {
	h := newHarnessWith(t, func(s *web.Server) {
		s.Prober = prober{contents("hevc", "aac", 2160, 2)}
		s.Transcoder = fakeEncoder(t)
	})
	h.signIn()

	res, body := h.call(http.MethodPost, "/web/api/play", map[string]any{
		"itemId": h.firstEp, "capabilities": browser,
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("play = %d: %v", res.StatusCode, body)
	}
	if body["mode"] != "transcode" || body["hls"] != true {
		t.Fatalf("H.265 was not converted: %v", body)
	}
	if why, _ := body["why"].(string); !strings.Contains(why, "H.265") {
		t.Errorf("the reason does not name the codec: %q", why)
	}

	session, _ := body["session"].(string)
	if session == "" {
		t.Fatal("no session came back")
	}

	// The playlist is written before the encoding is, so a player has the
	// whole runtime to scrub over from the first second.
	res, _ = h.call(http.MethodGet, "/web/hls/"+session+"/index.m3u8", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("playlist = %d", res.StatusCode)
	}

	// Stopping is the client saying it has gone, which frees the core now
	// rather than when the watchdog notices.
	if res, _ := h.call(http.MethodPost, "/web/api/play/"+session+"/stop", nil); res.StatusCode != http.StatusNoContent {
		t.Fatalf("stopping = %d", res.StatusCode)
	}
	if res, _ := h.call(http.MethodGet, "/web/hls/"+session+"/index.m3u8", nil); res.StatusCode != http.StatusGone {
		t.Errorf("a stopped session still answers: %d", res.StatusCode)
	}
}

// Only what has to be re-encoded is: Dolby Digital in a browser costs the
// audio and leaves every frame of the picture alone.
func TestOnlyTheStreamThatHasToBeConvertedIs(t *testing.T) {
	h := newHarnessWith(t, func(s *web.Server) {
		s.Prober = prober{contents("h264", "ac3", 1080, 6)}
		s.Transcoder = fakeEncoder(t)
	})
	h.signIn()

	_, body := h.call(http.MethodPost, "/web/api/play", map[string]any{
		"itemId": h.firstEp, "capabilities": browser,
	})
	if body["mode"] != "transcode" {
		t.Fatalf("play = %v", body)
	}
	if why, _ := body["why"].(string); !strings.Contains(why, "Dolby Digital") {
		t.Errorf("the reason does not name what is being converted: %q", why)
	}
}

// A server with no ffmpeg says so, rather than offering a stream it cannot
// produce.
func TestWithNoFFmpegTheAnswerIsASentence(t *testing.T) {
	h := newHarnessWith(t, func(s *web.Server) {
		s.Prober = prober{contents("hevc", "dts", 2160, 6)}
	})
	h.signIn()

	_, body := h.call(http.MethodPost, "/web/api/play", map[string]any{
		"itemId": h.firstEp, "capabilities": browser,
	})
	if body["mode"] != "unsupported" {
		t.Fatalf("play = %v", body)
	}
	if why, _ := body["why"].(string); !strings.Contains(why, "ffmpeg") {
		t.Errorf("the reason does not mention ffmpeg: %q", why)
	}
}

// A server with no ffprobe cannot know what is in a file, and the right
// answer then is the one that was right before: send the file.
func TestWithNoProbeEverythingIsDirect(t *testing.T) {
	h := newHarnessWith(t, func(s *web.Server) { s.Transcoder = fakeEncoder(t) })
	h.signIn()

	_, body := h.call(http.MethodPost, "/web/api/play", map[string]any{
		"itemId": h.firstEp, "capabilities": browser,
	})
	if body["mode"] != "direct" {
		t.Fatalf("play = %v", body)
	}
}

// A client that says nothing about itself is a client that can play
// anything, which is how the Plex surface has always behaved and how Bask
// reaches this server today.
func TestAClientThatClaimsNothingIsSentTheFile(t *testing.T) {
	h := newHarnessWith(t, func(s *web.Server) {
		s.Prober = prober{contents("hevc", "truehd", 2160, 8)}
		s.Transcoder = fakeEncoder(t)
	})
	h.signIn()

	_, body := h.call(http.MethodPost, "/web/api/play", map[string]any{"itemId": h.firstEp})
	if body["mode"] != "direct" {
		t.Fatalf("play = %v", body)
	}
	if url, _ := body["url"].(string); url != "/web/stream/"+strconv.FormatInt(partOfEpisode(t, h), 10) {
		t.Errorf("url = %v", body["url"])
	}
}

func partOfEpisode(t *testing.T, h *harness) int64 {
	t.Helper()
	parts, err := h.db.Parts(context.Background(), h.firstEp)
	if err != nil || len(parts) == 0 {
		t.Fatalf("no part for the episode: %v", err)
	}
	return parts[0].ID
}
