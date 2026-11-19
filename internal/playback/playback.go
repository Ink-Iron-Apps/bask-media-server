// Package playback decides how a file should reach a particular client.
//
// Three answers, in order of what they cost:
//
//   - Direct: send the file. Nothing is decoded on the server, the kernel
//     does the copying, and seeking is a byte range. This is the answer for
//     most of a modern library and the one worth working to keep.
//   - Remux: the streams are fine and the container is not — an MKV of H.264
//     and AAC in a browser. The packets are copied into a container the
//     client understands, which costs a stream copy and no decoding.
//   - Transcode: something has to be re-encoded, which costs a core for as
//     long as it plays. The last resort, and the thing a media server is
//     usually blamed for.
//
// The decision is about the client, not the file: the same episode is a
// direct play on the television, a remux in a browser and a transcode on a
// phone over mobile data. So capabilities come in as an argument and nothing
// here is stored.
package playback

import (
	"fmt"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/ffprobe"
)

// Mode is what has to happen to a file for this client to play it.
type Mode string

const (
	ModeDirect    Mode = "direct"
	ModeRemux     Mode = "remux"
	ModeTranscode Mode = "transcode"
)

// Capabilities is what a client can play.
type Capabilities struct {
	// Containers, video and audio codecs, as ffprobe names them. Empty means
	// "anything", which is what a client that has not said looks like.
	Containers []string
	Video      []string
	Audio      []string

	// MaxHeight caps the picture, for a small screen or a slow link. Zero
	// means no limit.
	MaxHeight int

	// MaxChannels caps the audio layout: a browser that is given 5.1 plays
	// it, and a laptop's two speakers get the centre channel mixed away. Zero
	// means no limit.
	MaxChannels int
}

// Browser is the fallback for a browser that has not said what it can play.
//
// A fallback only: the page asks the browser itself — canPlayType and
// MediaSource.isTypeSupported — and sends the answer, because a list written
// here is a guess about software that updates every six weeks, and a wrong
// guess either transcodes a file that would have played or plays one that
// will not. H.265 is absent because support for it is patchy enough that
// guessing yes is the expensive mistake.
var Browser = Capabilities{
	Containers: []string{"mp4", "m4v", "mov", "webm"},
	Video:      []string{"h264", "vp8", "vp9", "av1"},
	Audio:      []string{"aac", "mp3", "opus", "vorbis", "flac"},
}

// Plan is the answer: what to do, and what to tell a person when it is not
// the cheap one.
type Plan struct {
	Mode Mode
	Why  string

	// VideoCodec and AudioCodec are what ffmpeg is asked for: "copy" when the
	// packets can be carried over untouched.
	VideoCodec string
	AudioCodec string

	// Height is the picture to scale to, zero to leave it alone.
	Height int

	// Channels is the audio layout to mix to, zero to leave it alone.
	Channels int

	// VideoBitRate is the ceiling for a re-encode, in bits per second.
	VideoBitRate int64
}

// Transcoding reports whether this plan needs ffmpeg at all.
func (p Plan) Transcoding() bool { return p.Mode != ModeDirect }

// Decide works out how to serve one file to one client.
//
// container is the file's own, as the scanner recorded it, because the
// extension is what the client will see and ffprobe's format names are a
// comma-separated list that answers a different question.
func Decide(info ffprobe.Info, container string, caps Capabilities) Plan {
	video, audio := info.Video(), info.Audio()

	plan := Plan{Mode: ModeDirect, VideoCodec: "copy", AudioCodec: "copy"}
	var reasons []string

	// Video.
	switch {
	case video == nil:
		// No picture: an audio file, or a container this probe could not read.
		// Nothing to re-encode on the video side either way.
	case !supported(caps.Video, video.Codec):
		plan.Mode = ModeTranscode
		plan.VideoCodec = "h264"
		reasons = append(reasons, fmt.Sprintf("%s video is not supported here", display(video.Codec)))
	case caps.MaxHeight > 0 && video.Height > caps.MaxHeight:
		plan.Mode = ModeTranscode
		plan.VideoCodec = "h264"
		plan.Height = caps.MaxHeight
		reasons = append(reasons, fmt.Sprintf("%dp is larger than this client asked for", video.Height))
	}

	// Audio.
	switch {
	case audio == nil:
	case !supported(caps.Audio, audio.Codec):
		plan.Mode = ModeTranscode
		plan.AudioCodec = "aac"
		reasons = append(reasons, fmt.Sprintf("%s audio is not supported here", display(audio.Codec)))
	case caps.MaxChannels > 0 && audio.Channels > caps.MaxChannels:
		plan.Mode = ModeTranscode
		plan.AudioCodec = "aac"
		plan.Channels = caps.MaxChannels
		reasons = append(reasons, fmt.Sprintf("%d channels have to be mixed to %d", audio.Channels, caps.MaxChannels))
	}

	// Container, which only matters when nothing else already forced a
	// re-encode: a transcode writes its own container anyway.
	if plan.Mode == ModeDirect && !supported(caps.Containers, container) {
		plan.Mode = ModeRemux
		reasons = append(reasons, fmt.Sprintf("%s is not a container this client plays", strings.ToUpper(container)))

		// A remux has to put the packets somewhere, and the somewhere is
		// fragmented MP4. Most audio codecs go in one; the older lossless
		// ones do not, and copying them into it produces a file that either
		// fails to write or fails to play. Those are re-encoded instead,
		// which for an audio-only file costs a fraction of a core.
		if video == nil && audio != nil && !fitsInMP4(audio.Codec) {
			plan.Mode = ModeTranscode
			plan.AudioCodec = "aac"
			reasons = append(reasons,
				fmt.Sprintf("%s cannot be rewrapped without re-encoding", display(audio.Codec)))
		}
	}

	if plan.Mode != ModeDirect && plan.VideoCodec == "h264" {
		plan.VideoBitRate = bitRateFor(heightOf(video, plan.Height))
	}
	plan.Why = strings.Join(reasons, "; ")
	if plan.Mode == ModeDirect {
		plan.Why = "the file plays as it is"
	}
	return plan
}

func heightOf(video *ffprobe.Stream, planned int) int {
	if planned > 0 {
		return planned
	}
	if video != nil {
		return video.Height
	}
	return 0
}

// bitRateFor is a ceiling per picture size, chosen so a re-encode looks
// acceptable without the server deciding to saturate a link it cannot see.
func bitRateFor(height int) int64 {
	switch {
	case height <= 0:
		return 3_000_000
	case height <= 360:
		return 1_000_000
	case height <= 480:
		return 1_500_000
	case height <= 720:
		return 3_000_000
	case height <= 1080:
		return 6_000_000
	default:
		return 10_000_000
	}
}

// fitsInMP4 reports whether a codec's packets can be carried in a fragmented
// MP4 untouched, which is what a remux does.
func fitsInMP4(codec string) bool {
	switch strings.ToLower(codec) {
	case "aac", "alac", "mp3", "ac3", "eac3", "flac", "opus", "dts", "truehd":
		return true
	default:
		// Vorbis, WMA, PCM, Monkey's Audio, WavPack and the rest: formats
		// that predate MP4 or were never mapped into it.
		return false
	}
}

func supported(list []string, value string) bool {
	if len(list) == 0 {
		return true // a client that said nothing is taken at its word
	}
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return false
	}
	for _, allowed := range list {
		if strings.EqualFold(allowed, value) {
			return true
		}
	}
	return false
}

// display names a codec the way a person would say it.
func display(codec string) string {
	switch strings.ToLower(codec) {
	case "hevc":
		return "H.265"
	case "h264":
		return "H.264"
	case "mpeg4":
		return "MPEG-4"
	case "vc1":
		return "VC-1"
	case "dts":
		return "DTS"
	case "eac3":
		return "Dolby Digital Plus"
	case "ac3":
		return "Dolby Digital"
	case "truehd":
		return "Dolby TrueHD"
	default:
		return strings.ToUpper(codec)
	}
}
