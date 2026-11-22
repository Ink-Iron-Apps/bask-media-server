package transcode_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/playback"
	"github.com/inknironapps/bask-media-server/internal/transcode"
)

// recordingFFmpeg is the fake encoder again, with the command line written
// out: these tests are about what this package asks ffmpeg to do, which is
// the only part of burning in a subtitle that lives here.
func recordingFFmpeg(t *testing.T) (ffmpeg string, args func() string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake encoder is a shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "recording-ffmpeg")
	log := filepath.Join(dir, "args.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> ` + log + `
out="$PWD"
printf 'x' > "$out/init.mp4"
printf 'xxxx' > "$out/seg00000.m4s"
cat > "$out/stream.m3u8" <<EOT
#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:4
#EXT-X-MAP:URI="init.mp4"
#EXTINF:4.000,
seg00000.m4s
EOT
sleep 300
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// The encoding command, not whatever else ran the binary: hardware
	// detection runs it too, and its probe line would match some of the
	// same strings.
	return path, func() string {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if body, err := os.ReadFile(log); err == nil {
				for _, line := range strings.Split(string(body), "\n") {
					if strings.Contains(line, "-hls_segment_filename") {
						return line
					}
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("ffmpeg was never asked to encode anything")
		return ""
	}
}

func startBurning(t *testing.T, burn *playback.BurnIn, height int) string {
	t.Helper()
	ffmpeg, args := recordingFFmpeg(t)
	m := manager(t, ffmpeg)

	if _, err := m.Start(transcode.Request{
		Key:  "burn",
		Path: "/library/Films/Big Hill (2026)/Big Hill (2026).mkv",
		Plan: playback.Plan{
			Mode: playback.ModeTranscode, VideoCodec: "h264", AudioCodec: "aac",
			Height: height, Burn: burn,
		},
		Duration: time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	return args()
}

// A text subtitle is rendered by libass, from the file it lives in, by the
// index that counts subtitle streams — which is what the filter takes.
func TestATextSubtitleIsRenderedIntoThePicture(t *testing.T) {
	line := startBurning(t, &playback.BurnIn{
		Path: "/library/Films/Big Hill (2026)/Big Hill (2026).mkv", Index: 1,
	}, 720)

	if !strings.Contains(line, "subtitles=filename=") {
		t.Fatalf("no subtitles filter: %s", line)
	}
	if !strings.Contains(line, ":si=1") {
		t.Fatalf("the wrong subtitle stream: %s", line)
	}
	// Rendered before the scale, so the text is drawn once at the output
	// size rather than scaled after being drawn.
	subtitleAt := strings.Index(line, "subtitles=")
	scaleAt := strings.Index(line, "scale=-2:720")
	if subtitleAt < 0 || scaleAt < 0 || subtitleAt > scaleAt {
		t.Fatalf("the filter chain is in the wrong order: %s", line)
	}
	// One video stream and one audio stream, as always: the subtitle goes
	// into the picture rather than out as a stream of its own.
	if !strings.Contains(line, "-sn") {
		t.Fatalf("subtitles were also sent as a stream: %s", line)
	}
	if strings.Contains(line, "filter_complex") {
		t.Fatalf("a text subtitle went through a filtergraph: %s", line)
	}
}

// A sidecar file has one subtitle in it, so there is no index: the filter
// reads the file.
func TestASidecarIsRenderedWithoutAnIndex(t *testing.T) {
	line := startBurning(t, &playback.BurnIn{
		Path: "/library/Films/Big Hill (2026)/Big Hill (2026).en.srt", Index: -1,
	}, 0)

	if !strings.Contains(line, "Big Hill (2026).en.srt") {
		t.Fatalf("the sidecar was not used: %s", line)
	}
	if strings.Contains(line, "si=") {
		t.Fatalf("a sidecar was given a stream index: %s", line)
	}
}

// A bitmap subtitle is composited: the stream goes into the filtergraph by
// the number the container gives it, and the graph's output is what is
// encoded.
func TestABitmapSubtitleIsComposited(t *testing.T) {
	line := startBurning(t, &playback.BurnIn{
		Path: "/library/Films/Big Hill (2026)/Big Hill (2026).mkv", Index: 3, Picture: true,
	}, 1080)

	if !strings.Contains(line, "-filter_complex") {
		t.Fatalf("no filtergraph: %s", line)
	}
	if !strings.Contains(line, "overlay[vout]") {
		t.Fatalf("nothing was overlaid: %s", line)
	}
	if !strings.Contains(line, "[0:3]") {
		t.Fatalf("the wrong stream was overlaid: %s", line)
	}
	if !strings.Contains(line, "-map [vout]") {
		t.Fatalf("the graph's output was not what was encoded: %s", line)
	}
	// Scaled before the overlay, so the subtitle is drawn at the output size
	// rather than scaled after being drawn.
	scaleAt := strings.Index(line, "scale=-2:1080")
	overlayAt := strings.Index(line, "overlay")
	if scaleAt < 0 || scaleAt > overlayAt {
		t.Fatalf("the graph scales after overlaying: %s", line)
	}
	// And no second -vf scaling the already-composited picture.
	if strings.Contains(line, "-vf") {
		t.Fatalf("the composited picture was scaled again: %s", line)
	}
}

// A path with a colon or a quote in it is still one filter argument. Film
// titles contain both, and an unescaped colon is read as the next option.
func TestAnAwkwardPathIsEscaped(t *testing.T) {
	line := startBurning(t, &playback.BurnIn{
		Path: `/library/Films/Alien: Romulus (2024)/Alien's cut.srt`, Index: -1,
	}, 0)

	if !strings.Contains(line, `Alien\:`) {
		t.Fatalf("the colon was not escaped: %s", line)
	}
	if !strings.Contains(line, `'\''`) {
		t.Fatalf("the quote was not escaped: %s", line)
	}
}

// Nothing changes for a session with no subtitle to draw, which is almost
// every session.
func TestWithNothingToBurnTheCommandIsUnchanged(t *testing.T) {
	line := startBurning(t, nil, 720)

	if strings.Contains(line, "subtitles=") || strings.Contains(line, "filter_complex") {
		t.Fatalf("a filter appeared with nothing to draw: %s", line)
	}
	if !strings.Contains(line, "-vf scale=-2:720") {
		t.Fatalf("the ordinary scale is missing: %s", line)
	}
}
