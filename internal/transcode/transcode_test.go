package transcode_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/inknironapps/bask-media-server/internal/playback"
	"github.com/inknironapps/bask-media-server/internal/transcode"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// fakeFFmpeg stands in for the real thing where the test is about the
// lifecycle rather than the encoding: it writes a plausible playlist, starts a
// child process, and then waits to be killed.
//
// The child is the point. A session that kills only the process it started
// would leave that child running, which is exactly the leak rule 5 of the
// resource budget is about.
func fakeFFmpeg(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake encoder is a shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-ffmpeg")
	script := `#!/bin/sh
# The session directory is the working directory; the playlist is the last
# argument. Write enough to look like a running encoder, then wait.
out="$PWD"
echo $$ > "$out/parent.pid"
sleep 300 &
echo $! > "$out/child.pid"
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
wait
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func manager(t *testing.T, ffmpeg string, change ...func(*transcode.Options)) *transcode.Manager {
	t.Helper()
	opts := transcode.Options{FFmpeg: ffmpeg, Root: filepath.Join(t.TempDir(), "sessions")}
	for _, apply := range change {
		apply(&opts)
	}
	m, err := transcode.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Shutdown)
	return m
}

func pidsOf(t *testing.T, dir string) (parent, child int) {
	t.Helper()
	read := func(name string) int {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return 0
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
		if err != nil {
			return 0
		}
		return pid
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		parent, child = read("parent.pid"), read("child.pid")
		if parent > 0 && child > 0 {
			return parent, child
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the fake encoder never reported its processes")
	return 0, 0
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func gone(t *testing.T, pid int) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func request(path string) transcode.Request {
	return transcode.Request{
		Key:      path,
		Path:     path,
		Duration: time.Minute,
		Plan: playback.Plan{
			Mode: playback.ModeTranscode, VideoCodec: "h264", AudioCodec: "copy",
			VideoBitRate: 1_000_000,
		},
	}
}

// A client that walks away sends nothing, so the only thing that stops the
// encoder is the server noticing that nobody has asked for anything.
func TestAnIdleSessionIsKilledWithEverythingItStarted(t *testing.T) {
	m := manager(t, fakeFFmpeg(t), func(o *transcode.Options) { o.IdleTimeout = 300 * time.Millisecond })

	session, err := m.Start(request("/some/film.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(m.Root(), session.ID)
	parent, child := pidsOf(t, dir)

	if !gone(t, parent) {
		t.Fatal("the encoder outlived the viewer")
	}
	if !gone(t, child) {
		t.Error("the encoder's own child process is still running")
	}
	if m.Sessions() != 0 {
		t.Errorf("%d sessions are still listed", m.Sessions())
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the session's files are still on disk: %v", err)
	}
}

// Stopping the server must not leave an encoder behind: a process nobody owns
// is a core burning until somebody notices.
func TestShutdownKillsWhatIsRunning(t *testing.T) {
	m, err := transcode.New(transcode.Options{
		FFmpeg: fakeFFmpeg(t), Root: filepath.Join(t.TempDir(), "sessions"),
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := m.Start(request("/some/film.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	root := m.Root()
	parent, child := pidsOf(t, filepath.Join(root, session.ID))

	m.Shutdown()

	if !gone(t, parent) || !gone(t, child) {
		t.Error("shutdown left ffmpeg running")
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("shutdown left its working files: %v", err)
	}
}

// Transcoding is the one thing this server does that costs a core, so there
// is a limit and it is a refusal rather than a queue of half-speed encodes.
func TestTooManyAtOnceIsRefused(t *testing.T) {
	m := manager(t, fakeFFmpeg(t), func(o *transcode.Options) { o.MaxSessions = 1 })

	if _, err := m.Start(request("/first.mkv")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(request("/second.mkv")); !errors.Is(err, transcode.ErrBusy) {
		t.Fatalf("a second session returned %v, want ErrBusy", err)
	}
}

// A page reload must not start a second encoder for the same film: the
// request carries what is being played, and the same thing gets the same
// session.
func TestTheSamePlaybackReusesItsSession(t *testing.T) {
	m := manager(t, fakeFFmpeg(t))

	first, err := m.Start(request("/film.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Start(request("/film.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Errorf("a reload started a second encoder: %s then %s", first.ID, second.ID)
	}
	if m.Sessions() != 1 {
		t.Errorf("%d sessions are running", m.Sessions())
	}
}

// A segment name comes out of a URL, and a URL is written by whoever is
// asking.
func TestASegmentNameCannotLeaveTheSessionDirectory(t *testing.T) {
	m := manager(t, fakeFFmpeg(t))
	session, err := m.Start(request("/film.mkv"))
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"../../etc/passwd", "..", "/etc/passwd", "seg0/../../x", "stream.m3u8"} {
		if _, err := session.Segment(context.Background(), name); err == nil {
			t.Errorf("%q was accepted as a segment name", name)
		}
	}
}

// The playlist for a re-encode is written before the encoding is, which is
// what gives a player a scrub bar over the whole film from the first second.
func TestAReEncodesPlaylistCoversTheWholeRuntimeImmediately(t *testing.T) {
	m := manager(t, fakeFFmpeg(t))
	req := request("/film.mkv")
	req.Duration = 61 * time.Second

	session, err := m.Start(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := session.Playlist(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	playlist := string(body)

	if !strings.Contains(playlist, "#EXT-X-PLAYLIST-TYPE:VOD") || !strings.Contains(playlist, "#EXT-X-ENDLIST") {
		t.Fatalf("playlist is not a complete one:\n%s", playlist)
	}
	if segments := strings.Count(playlist, ".m4s"); segments != 16 {
		// Fifteen whole segments of four seconds, plus the one-second
		// remainder. The initialisation segment is an .mp4 named in
		// EXT-X-MAP and is not one of them.
		t.Errorf("playlist has %d segment lines:\n%s", segments, playlist)
	}
	if !strings.Contains(playlist, "seg00015.m4s") {
		t.Errorf("playlist stops short:\n%s", playlist)
	}
}

// With real ffmpeg: what comes out has to be playable, which means the init
// segment and the pieces after it are real fragments and not empty files.
func TestRealFFmpegProducesPlayableSegments(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg")
	}
	source := sampleVideo(t, ffmpeg, 20)

	m := manager(t, ffmpeg)
	req := request(source)
	req.Duration = 20 * time.Second
	session, err := m.Start(req)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, name := range []string{"init.mp4", "seg00000.m4s", "seg00001.m4s"} {
		path, err := session.Segment(ctx, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if info.Size() < 100 {
			t.Errorf("%s is %d bytes, which is not a fragment", name, info.Size())
		}
	}
}

// Seeking into the middle of a long film must not wait for everything before
// it to be encoded.
func TestSeekingAheadRestartsTheEncoderThere(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg")
	}
	source := sampleVideo(t, ffmpeg, 60)

	m := manager(t, ffmpeg)
	req := request(source)
	req.Duration = 60 * time.Second
	session, err := m.Start(req)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	started := time.Now()
	if _, err := session.Segment(ctx, "seg00012.m4s"); err != nil {
		t.Fatalf("seeking to 48 seconds: %v", err)
	}
	t.Logf("the segment at 48 seconds arrived in %v", time.Since(started).Round(time.Millisecond))

	// The proof that it jumped rather than encoded its way there: the
	// segments in between were never written.
	for _, name := range []string{"seg00003.m4s", "seg00007.m4s"} {
		if _, err := os.Stat(filepath.Join(m.Root(), session.ID, name)); err == nil {
			t.Errorf("%s was encoded on the way to the seek", name)
		}
	}
}

// sampleVideo renders a short clip to transcode, so the test needs no
// fixture file in the repository.
func sampleVideo(t *testing.T, ffmpeg string, seconds int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sample.mp4")
	cmd := exec.Command(ffmpeg, "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x180:rate=15:duration="+strconv.Itoa(seconds),
		"-f", "lavfi", "-i", "anoisesrc=d="+strconv.Itoa(seconds)+":c=pink:a=0.2",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "64k", "-shortest", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not render a sample: %v: %s", err, out)
	}
	return path
}
