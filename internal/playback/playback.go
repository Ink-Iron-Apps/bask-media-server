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

	// MaxBitRate caps the stream, in bits per second. This is how a client
	// asks for a smaller version of a file it could otherwise play — a
	// quality preset over a slow link — so it forces a re-encode on its own,
	// with nothing wrong with the codecs or the container.
	MaxBitRate int64

	// BitRate is what the file actually runs at, for the cases where the
	// container does not declare one: the caller knows the file size and the
	// duration, which is the only other way to answer it. Zero means "use
	// what the probe found".
	BitRate int64
}

// Cause is one machine-readable reason a plan is not a direct play.
//
// Plan.Why is for a person. These are for a protocol: both Jellyfin and Plex
// report why a stream is being converted as a list of stable names, and a
// presenter cannot derive them from prose.
type Cause string

const (
	CauseContainer  Cause = "container"
	CauseVideoCodec Cause = "video-codec"
	CauseAudioCodec Cause = "audio-codec"
	CauseResolution Cause = "resolution"
	CauseChannels   Cause = "channels"
	CauseBitRate    Cause = "bit-rate"
)

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

	// Causes are what made this more than a direct play, in the order they
	// were found.
	Causes []Cause

	// Burn is a subtitle to draw into the picture, or nil.
	//
	// Not something [Decide] ever asks for: burning in is a client saying it
	// cannot render a subtitle itself, which is a separate request from
	// whether it can play the file. Every surface that honours it sets this
	// and forces a re-encode, because a video stream cannot be copied and
	// drawn on at the same time.
	Burn *BurnIn
}

// BurnIn is a subtitle to draw into the picture.
//
// Some subtitles cannot be handed to a client at all. A PGS track off a
// Blu-ray is a sequence of pictures, and a television that wants text gets
// nothing from it; an ASS track with positioning and fonts renders as
// misplaced lines in most players. The answer both of those have always had
// is to draw the subtitle into the video while transcoding, which is the one
// thing a media server can do that a client cannot.
type BurnIn struct {
	// Path is the file holding the subtitle: the media file for an embedded
	// track, the sidecar for one beside it.
	Path string

	// Index is which subtitle stream inside Path, counted among the subtitle
	// streams rather than all streams, because that is what ffmpeg's
	// subtitles filter takes. Negative for a sidecar, which has one.
	Index int

	// Picture is true for a bitmap subtitle — PGS, VobSub — which has to be
	// composited onto the frames rather than rendered as text.
	Picture bool
}

// Burning reports whether this plan draws a subtitle into the picture.
func (p Plan) Burning() bool { return p.Burn != nil }

// Key is the part of a plan that decides whether two requests can share one
// encoder.
//
// The subtitle belongs in it: a viewer switching subtitle tracks is asking
// for a different picture, and without this they would be handed the session
// that is already burning the other one.
func (b *BurnIn) Key() string {
	if b == nil {
		return ""
	}
	return fmt.Sprintf("%s#%d", b.Path, b.Index)
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
		plan.Causes = append(plan.Causes, CauseVideoCodec)
		reasons = append(reasons, fmt.Sprintf("%s video is not supported here", display(video.Codec)))
	case caps.MaxHeight > 0 && video.Height > caps.MaxHeight:
		plan.Mode = ModeTranscode
		plan.VideoCodec = "h264"
		plan.Height = caps.MaxHeight
		plan.Causes = append(plan.Causes, CauseResolution)
		reasons = append(reasons, fmt.Sprintf("%dp is larger than this client asked for", video.Height))
	}

	// Audio.
	switch {
	case audio == nil:
	case !supported(caps.Audio, audio.Codec):
		plan.Mode = ModeTranscode
		plan.AudioCodec = "aac"
		plan.Causes = append(plan.Causes, CauseAudioCodec)
		reasons = append(reasons, fmt.Sprintf("%s audio is not supported here", display(audio.Codec)))
	case caps.MaxChannels > 0 && audio.Channels > caps.MaxChannels:
		plan.Mode = ModeTranscode
		plan.AudioCodec = "aac"
		plan.Channels = caps.MaxChannels
		plan.Causes = append(plan.Causes, CauseChannels)
		reasons = append(reasons, fmt.Sprintf("%d channels have to be mixed to %d", audio.Channels, caps.MaxChannels))
	}

	// Container, which only matters when nothing else already forced a
	// re-encode: a transcode writes its own container anyway.
	if plan.Mode == ModeDirect && !supported(caps.Containers, container) {
		plan.Mode = ModeRemux
		plan.Causes = append(plan.Causes, CauseContainer)
		reasons = append(reasons, fmt.Sprintf("%s is not a container this client plays", strings.ToUpper(container)))

		// A remux has to put the packets somewhere, and the somewhere is
		// fragmented MP4. Most audio codecs go in one; the older lossless
		// ones do not, and copying them into it produces a file that either
		// fails to write or fails to play. Those are re-encoded instead,
		// which for an audio-only file costs a fraction of a core.
		if video == nil && audio != nil && !fitsInMP4(audio.Codec) {
			plan.Mode = ModeTranscode
			plan.AudioCodec = "aac"
			plan.Causes = append(plan.Causes, CauseAudioCodec)
			reasons = append(reasons,
				fmt.Sprintf("%s cannot be rewrapped without re-encoding", display(audio.Codec)))
		}
	}

	// A bit rate ceiling, which is a request about the link rather than about
	// the file: a client that can decode every stream in it still cannot pull
	// 30 Mbit/s over mobile data. Checked last because a re-encode that is
	// already happening carries the ceiling anyway.
	if rate := rateOf(info, caps); caps.MaxBitRate > 0 && rate > caps.MaxBitRate && video != nil {
		plan.Mode = ModeTranscode
		plan.VideoCodec = "h264"
		plan.Causes = append(plan.Causes, CauseBitRate)
		reasons = append(reasons, fmt.Sprintf("%s is more than this client asked for",
			megabits(rate)))
		if height := heightForBitRate(caps.MaxBitRate); height > 0 && (video.Height == 0 || height < video.Height) {
			plan.Height = height
		}
	}

	if plan.Mode != ModeDirect && plan.VideoCodec == "h264" {
		plan.VideoBitRate = bitRateFor(heightOf(video, plan.Height))
		// The ceiling the client named wins over the one this server would
		// have chosen for the picture size: it was asked for.
		if caps.MaxBitRate > 0 && caps.MaxBitRate < plan.VideoBitRate {
			plan.VideoBitRate = caps.MaxBitRate
		}
	}
	plan.Why = strings.Join(reasons, "; ")
	if plan.Mode == ModeDirect {
		plan.Why = "the file plays as it is"
	}
	return plan
}

// rateOf is what the file runs at: what the caller measured, else what the
// container declared, else the video stream's own.
func rateOf(info ffprobe.Info, caps Capabilities) int64 {
	if caps.BitRate > 0 {
		return caps.BitRate
	}
	if info.BitRate > 0 {
		return info.BitRate
	}
	if video := info.Video(); video != nil {
		return video.BitRate
	}
	return 0
}

// heightForBitRate is the largest picture this ceiling pays for.
//
// A client asking for 2 Mbit/s of a 4K film is asking for something that
// cannot look like 4K, and encoding it at full size spends the whole budget
// on macroblocks. The ladder is the inverse of [bitRateFor], so the two
// cannot disagree about what a size costs.
func heightForBitRate(ceiling int64) int {
	for _, height := range []int{2160, 1080, 720, 480, 360} {
		if bitRateFor(height) <= ceiling {
			return height
		}
	}
	return 360
}

// megabits says a rate the way a person would.
func megabits(rate int64) string {
	if rate < 1_000_000 {
		return fmt.Sprintf("%d kbps", rate/1000)
	}
	return fmt.Sprintf("%.1f Mbps", float64(rate)/1_000_000)
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
