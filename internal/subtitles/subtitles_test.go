package subtitles_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/subtitles"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

const sample = "1\n00:00:01,000 --> 00:00:04,000\nFirst line.\n\n2\n00:01:02,500 --> 00:01:05,250\nSecond line.\n"

func sidecar(t *testing.T, name, body string) domain.Subtitle {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return domain.Subtitle{External: true, StreamIndex: -1, Path: path, Codec: subtitleCodec(name)}
}

func subtitleCodec(name string) string {
	switch filepath.Ext(name) {
	case ".vtt":
		return "webvtt"
	case ".ass":
		return "ass"
	default:
		return "srt"
	}
}

// The browser's case, and the most common subtitle in any library: a .srt
// beside the film, wanted as WebVTT.
func TestSubRipBecomesWebVTT(t *testing.T) {
	converter := &subtitles.Converter{} // deliberately no ffmpeg
	sub := sidecar(t, "film.en.srt", sample)

	body, format, err := converter.Text(context.Background(), sub, subtitles.VTT)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)

	if format != subtitles.VTT {
		t.Errorf("format = %q", format)
	}
	if !strings.HasPrefix(text, "WEBVTT\n") {
		t.Fatalf("no WebVTT header:\n%s", text)
	}
	if !strings.Contains(text, "00:00:01.000 --> 00:00:04.000") {
		t.Errorf("timestamps were not converted:\n%s", text)
	}
	if strings.Contains(text, ",250") {
		t.Errorf("a comma survived in a timestamp:\n%s", text)
	}
	if !strings.Contains(text, "Second line.") {
		t.Errorf("the text was lost:\n%s", text)
	}
}

// Asked for what it already is, a sidecar is sent as it is: no conversion, no
// process, no chance of changing it.
func TestASidecarInTheWantedFormatIsSentUntouched(t *testing.T) {
	converter := &subtitles.Converter{}
	sub := sidecar(t, "film.en.srt", sample)

	body, format, err := converter.Text(context.Background(), sub, subtitles.SRT)
	if err != nil {
		t.Fatal(err)
	}
	if format != subtitles.SRT || string(body) != sample {
		t.Errorf("got %q", body)
	}
}

// A subtitle written in a code page, which is still half of what is in
// circulation, must not reach a browser as mojibake.
func TestALatin1SubtitleIsDecoded(t *testing.T) {
	converter := &subtitles.Converter{}
	// "Voilà" in Latin-1: the à is a single 0xE0 byte, which is not UTF-8.
	sub := sidecar(t, "film.fr.srt", "1\n00:00:01,000 --> 00:00:02,000\nVoil\xe0.\n")

	body, _, err := converter.Text(context.Background(), sub, subtitles.VTT)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "Voilà.") {
		t.Errorf("got %q", body)
	}
}

// A picture of words is not text, and saying so is better than an empty file
// that looks like a server fault.
func TestAPictureSubtitleSaysSo(t *testing.T) {
	converter := &subtitles.Converter{}
	sub := domain.Subtitle{Codec: "hdmv_pgs_subtitle", Path: "/wherever.sup", External: true}

	if _, _, err := converter.Text(context.Background(), sub, subtitles.VTT); err == nil {
		t.Fatal("a PGS subtitle was offered as text")
	} else if !strings.Contains(err.Error(), "picture") {
		t.Errorf("error = %v", err)
	}
}

// Without ffmpeg the two cheap paths still work and everything else is
// refused in a way a caller can act on.
func TestWithoutFFmpegTheConversionsThatNeedItAreRefused(t *testing.T) {
	converter := &subtitles.Converter{}
	sub := sidecar(t, "film.en.ass", "[Script Info]\n")

	if _, _, err := converter.Text(context.Background(), sub, subtitles.VTT); err == nil {
		t.Fatal("an ASS file was converted with no converter")
	}
}

// With ffmpeg, the formats that need it are converted — including a stream
// inside the container, which is where most subtitles actually live.
func TestWithFFmpegAnEmbeddedStreamBecomesWebVTT(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg")
	}
	dir := t.TempDir()
	srt := filepath.Join(dir, "source.srt")
	if err := os.WriteFile(srt, []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(dir, "film.mkv")
	build := exec.Command(ffmpeg, "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=160x90:rate=5:duration=70",
		"-i", srt, "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:s", "srt", "-metadata:s:s:0", "language=eng", media)
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("could not build a file with subtitles: %v: %s", err, out)
	}

	converter := &subtitles.Converter{FFmpeg: ffmpeg}
	sub := domain.Subtitle{Path: media, StreamIndex: 1, Codec: "subrip"}

	body, format, err := converter.Text(context.Background(), sub, subtitles.VTT)
	if err != nil {
		t.Fatal(err)
	}
	if format != subtitles.VTT || !strings.HasPrefix(string(body), "WEBVTT") {
		t.Fatalf("got %q", body)
	}
	if !strings.Contains(string(body), "Second line.") {
		t.Errorf("the embedded text did not come back:\n%s", body)
	}
}
