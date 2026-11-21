package transcode_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/transcode"
)

// fakeFFmpeg writes a script standing in for ffmpeg, which records every
// command line it was given and succeeds or fails according to what the test
// is about.
//
// A fake rather than the real thing because this machine has no graphics
// card, and the behaviour worth pinning down is what BMS decides rather than
// what a card does: which candidates are tried, in what order, that one that
// fails is not used, and that none of it can stop a file being watchable.
func probeFFmpeg(t *testing.T, succeedFor string) (path string, log string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake encoder is a shell script")
	}
	dir := t.TempDir()
	path = filepath.Join(dir, "ffmpeg")
	log = filepath.Join(dir, "calls.log")

	// Succeeds only when the command line names the encoder the test wants
	// to work; every other probe fails the way a missing driver does.
	script := `#!/bin/sh
echo "$@" >> ` + log + `
case "$*" in
  *` + succeedFor + `*) exit 0 ;;
esac
exit 1
`
	if succeedFor == "" {
		script = `#!/bin/sh
echo "$@" >> ` + log + `
exit 1
`
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, log
}

func readLog(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// Support is claimed only after it has been proved on this machine, because
// an ffmpeg compiled with an encoder is not a machine that can run it.
func TestAnAcceleratorIsProvedBeforeItIsUsed(t *testing.T) {
	ffmpeg, log := probeFFmpeg(t, "h264_vaapi")

	found := transcode.Detect(context.Background(), ffmpeg, "auto")
	if found == nil {
		t.Fatal("the one accelerator that works was not found")
	}
	if found.Encoder != "h264_vaapi" {
		t.Fatalf("picked %q", found.Encoder)
	}

	// And it got there by trying: a probe was actually run.
	calls := readLog(t, log)
	if len(calls) == 0 {
		t.Fatal("nothing was probed, so nothing was proved")
	}
	for _, call := range calls {
		if !strings.Contains(call, "testsrc2") {
			t.Fatalf("a probe encoded something other than a generated picture: %s", call)
		}
	}
}

// A machine with no working hardware gets the processor, silently and
// without failing anything.
func TestNoHardwareIsNotAFailure(t *testing.T) {
	ffmpeg, log := probeFFmpeg(t, "")

	if found := transcode.Detect(context.Background(), ffmpeg, "auto"); found != nil {
		t.Fatalf("claimed %q on a machine where every probe failed", found.Name)
	}
	if len(readLog(t, log)) == 0 {
		t.Fatal("gave up without trying anything")
	}
}

// Off means off: no probe, no cost, no surprise in the log.
func TestOffProbesNothing(t *testing.T) {
	ffmpeg, log := probeFFmpeg(t, "h264_vaapi")

	for _, spelling := range []string{"off", "none", "cpu", "OFF"} {
		if found := transcode.Detect(context.Background(), ffmpeg, spelling); found != nil {
			t.Fatalf("%q still picked %q", spelling, found.Name)
		}
	}
	if calls := readLog(t, log); len(calls) != 0 {
		t.Fatalf("off ran %d probes", len(calls))
	}
}

// Naming one tries that one and nothing else, so an operator who knows what
// their machine has does not pay for the others being tried first.
func TestNamingOneTriesOnlyThatOne(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the named accelerators differ per platform")
	}
	ffmpeg, log := probeFFmpeg(t, "h264_nvenc")

	found := transcode.Detect(context.Background(), ffmpeg, "nvenc")
	if found == nil || found.Name != "nvenc" {
		t.Fatalf("asked for nvenc, got %v", found)
	}
	calls := readLog(t, log)
	if len(calls) != 1 {
		t.Fatalf("asking for one ran %d probes: %v", len(calls), calls)
	}
}

// And naming one this machine cannot do falls back rather than refusing to
// start: hardware encoding is an optimisation, and a server that will not
// run because of one is worse than a server that encodes on the processor.
func TestNamingOneThatDoesNotWorkFallsBack(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the named accelerators differ per platform")
	}
	ffmpeg, _ := probeFFmpeg(t, "h264_vaapi")

	if found := transcode.Detect(context.Background(), ffmpeg, "nvenc"); found != nil {
		t.Fatalf("asked for nvenc on a machine that only does vaapi, got %q", found.Name)
	}
}

// A name this server has never heard of is the same as no hardware, not a
// crash and not a silent fall through to something else.
func TestAnUnknownNameIsJustNoHardware(t *testing.T) {
	ffmpeg, log := probeFFmpeg(t, "h264_vaapi")

	if found := transcode.Detect(context.Background(), ffmpeg, "quantum"); found != nil {
		t.Fatalf("picked %q for an accelerator nobody has", found.Name)
	}
	if calls := readLog(t, log); len(calls) != 0 {
		t.Fatalf("an unknown name ran %d probes", len(calls))
	}
}

// VAAPI scales on the device, and the filter has to be written in the order
// the hardware wants: upload first, resize there. Pulling frames back to
// system memory to resize them gives away most of what the card was for.
func TestVAAPIScalesOnTheDevice(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("vaapi is the Linux one")
	}
	ffmpeg, log := probeFFmpeg(t, "h264_vaapi")

	if found := transcode.Detect(context.Background(), ffmpeg, "vaapi"); found == nil {
		t.Fatal("vaapi was not found")
	}
	calls := readLog(t, log)
	if len(calls) != 1 {
		t.Fatalf("expected one probe, got %v", calls)
	}
	probe := calls[0]
	for _, want := range []string{"-vaapi_device", "format=nv12,hwupload,scale_vaapi", "h264_vaapi"} {
		if !strings.Contains(probe, want) {
			t.Errorf("the probe did not carry %q: %s", want, probe)
		}
	}
}
