package playback_test

import (
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/inknironapps/bask-media-server/internal/ffprobe"
	"github.com/inknironapps/bask-media-server/internal/playback"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func file(videoCodec string, height int, audioCodec string, channels int) ffprobe.Info {
	info := ffprobe.Info{}
	if videoCodec != "" {
		info.Streams = append(info.Streams, ffprobe.Stream{
			Index: 0, Type: "video", Codec: videoCodec, Width: height * 16 / 9, Height: height,
		})
	}
	if audioCodec != "" {
		info.Streams = append(info.Streams, ffprobe.Stream{
			Index: 1, Type: "audio", Codec: audioCodec, Channels: channels,
		})
	}
	return info
}

// The cheap answer has to be the usual one: a modern file in a modern
// container is sent as it is, and the server does no work at all.
func TestAnMP4OfH264AndAACIsSentAsItIs(t *testing.T) {
	plan := playback.Decide(file("h264", 1080, "aac", 2), "mp4", playback.Browser)

	if plan.Mode != playback.ModeDirect {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.Transcoding() {
		t.Error("a direct play claims to need ffmpeg")
	}
}

// The common case a browser cannot do anything with: good streams, a
// container it does not open. Copying the packets costs no decoding.
func TestAContainerAClientCannotOpenIsRemuxed(t *testing.T) {
	caps := playback.Browser
	caps.Containers = []string{"mp4"}

	plan := playback.Decide(file("h264", 1080, "aac", 2), "mkv", caps)

	if plan.Mode != playback.ModeRemux {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.VideoCodec != "copy" || plan.AudioCodec != "copy" {
		t.Errorf("a remux re-encodes something: %+v", plan)
	}
	if !strings.Contains(plan.Why, "MKV") {
		t.Errorf("the reason does not name the container: %q", plan.Why)
	}
}

// H.265 is the file a browser opens and cannot show, and the reason has to
// say so in words somebody can act on.
func TestAnUnsupportedVideoCodecIsReEncoded(t *testing.T) {
	plan := playback.Decide(file("hevc", 2160, "aac", 2), "mkv", playback.Browser)

	if plan.Mode != playback.ModeTranscode || plan.VideoCodec != "h264" {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.AudioCodec != "copy" {
		t.Errorf("the audio was re-encoded for no reason: %+v", plan)
	}
	if !strings.Contains(plan.Why, "H.265") {
		t.Errorf("the reason does not name the codec: %q", plan.Why)
	}
}

// Surround on a laptop: the picture is untouched and only the audio is mixed,
// because re-encoding the video as well would cost a core for nothing.
func TestSurroundAudioIsMixedWithoutTouchingThePicture(t *testing.T) {
	// A client that plays the codec but wants two channels: a laptop, where
	// mixing is the difference between hearing the dialogue and not.
	caps := playback.Browser
	caps.Audio = append(caps.Audio, "ac3")
	caps.MaxChannels = 2

	plan := playback.Decide(file("h264", 1080, "ac3", 6), "mp4", caps)

	if plan.Mode != playback.ModeTranscode {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.VideoCodec != "copy" {
		t.Errorf("the picture was re-encoded: %+v", plan)
	}
	if plan.AudioCodec != "aac" {
		t.Errorf("the audio was not re-encoded: %+v", plan)
	}
}

// A client that asks for a smaller picture gets one, with a bitrate to match:
// the point of asking is usually a link that cannot carry the original.
func TestAHeightLimitScalesAndCapsTheBitrate(t *testing.T) {
	caps := playback.Browser
	caps.MaxHeight = 720

	plan := playback.Decide(file("h264", 2160, "aac", 2), "mp4", caps)

	if plan.Mode != playback.ModeTranscode || plan.Height != 720 {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.VideoBitRate != 3_000_000 {
		t.Errorf("bitrate for 720p = %d", plan.VideoBitRate)
	}
	// A file already inside the limit is not re-encoded to prove the point.
	if same := playback.Decide(file("h264", 720, "aac", 2), "mp4", caps); same.Mode != playback.ModeDirect {
		t.Errorf("a file already within the limit was transcoded: %+v", same)
	}
}

// Multichannel audio in a codec the client decodes is left alone, because
// mixing it would re-encode for nothing: this is the difference between
// serving a file and having an opinion about it.
func TestSurroundIsLeftAloneWhenTheClientCanDecodeIt(t *testing.T) {
	caps := playback.Browser
	caps.Audio = append(caps.Audio, "ac3")

	plan := playback.Decide(file("h264", 1080, "ac3", 6), "mp4", caps)

	if plan.Mode != playback.ModeDirect {
		t.Fatalf("plan = %+v", plan)
	}
}

// A client that says nothing about what it can play is taken at its word,
// which is what keeps the Plex surface's behaviour unchanged.
func TestAClientThatClaimsNothingGetsTheFile(t *testing.T) {
	plan := playback.Decide(file("hevc", 2160, "truehd", 8), "mkv", playback.Capabilities{})

	if plan.Mode != playback.ModeDirect {
		t.Fatalf("plan = %+v", plan)
	}
}
