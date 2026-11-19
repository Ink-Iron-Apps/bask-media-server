// Package ffprobe measures media files by asking ffprobe.
//
// A subprocess rather than a library binding: BMS never decodes video itself,
// so linking libav would buy nothing but a C toolchain and a cross-compilation
// problem. ffprobe is also what every other server in this space uses, so a
// machine that can run one can run BMS.
package ffprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// maxOutput caps what one probe may write back.
//
// ffprobe asked for a duration and a chapter list should answer in a few
// kilobytes; a file with a thousand chapters is still well inside this. A
// plain bytes.Buffer would instead grow to whatever a subprocess decides to
// write, which is an unbounded allocation driven by a file on disk — the one
// thing docs/RESOURCE-BUDGET.md forbids outright.
const maxOutput = 1 << 20

// Prober runs a known ffprobe binary.
type Prober struct {
	Path string

	// Timeout bounds one probe. A file on a sleeping drive or a dying disk can
	// hang forever, and a scan that hangs is indistinguishable from a scan
	// that crashed.
	Timeout time.Duration
}

// New finds ffprobe, preferring an explicit path and falling back to PATH.
func New(path string) (*Prober, error) {
	if path == "" {
		found, err := exec.LookPath("ffprobe")
		if err != nil {
			return nil, fmt.Errorf("ffprobe not found: %w", err)
		}
		path = found
	}
	return &Prober{Path: path, Timeout: 30 * time.Second}, nil
}

// Info is what one probe learned about a file.
type Info struct {
	Duration time.Duration

	// Chapters are the file's own chapters, in file order, with Index filled
	// in and ItemID left for the store. Empty for the many files that declare
	// none.
	Chapters []domain.Chapter

	// Streams are what is inside the container. Read at playback time rather
	// than stored, because what matters about them — can this client decode
	// it — is a question about the client, and the answer changes with the
	// client and not with the file.
	Streams []Stream
}

// Stream is one track in a file.
type Stream struct {
	Index    int
	Type     string // video, audio, subtitle
	Codec    string
	Profile  string
	Width    int
	Height   int
	Channels int
	BitRate  int64
	Language string
	Title    string

	// Default, Forced and Hearing are the container's dispositions, which are
	// how a file says which track to show without being asked and which one
	// carries only the translated foreign dialogue.
	Default bool
	Forced  bool
	Hearing bool
}

// Video returns the first video stream, or nil for a file with none.
func (i Info) Video() *Stream { return i.first("video") }

// Audio returns the first audio stream, or nil.
func (i Info) Audio() *Stream { return i.first("audio") }

// Subtitles returns every subtitle stream in the file.
func (i Info) Subtitles() []Stream {
	var out []Stream
	for _, stream := range i.Streams {
		if stream.Type == "subtitle" {
			out = append(out, stream)
		}
	}
	return out
}

func (i Info) first(kind string) *Stream {
	for idx := range i.Streams {
		if i.Streams[idx].Type == kind {
			return &i.Streams[idx]
		}
	}
	return nil
}

type probeOutput struct {
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
	Streams []struct {
		Index     int    `json:"index"`
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		Profile   string `json:"profile"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		Channels  int    `json:"channels"`
		BitRate   string `json:"bit_rate"`
		Tags      struct {
			Language string `json:"language"`
			Title    string `json:"title"`
		} `json:"tags"`
		Disposition struct {
			Default         int `json:"default"`
			Forced          int `json:"forced"`
			HearingImpaired int `json:"hearing_impaired"`
		} `json:"disposition"`
	} `json:"streams"`
	Chapters []struct {
		StartTime string `json:"start_time"`
		EndTime   string `json:"end_time"`
		Tags      struct {
			Title string `json:"title"`
		} `json:"tags"`
	} `json:"chapters"`
}

// Duration reports how long a file plays for.
func (p *Prober) Duration(ctx context.Context, path string) (time.Duration, error) {
	info, err := p.Probe(ctx, path)
	if err != nil {
		return 0, err
	}
	return info.Duration, nil
}

// Probe measures a file: how long it runs, and what chapters it declares.
//
// One subprocess for both, because the expensive part of a probe is opening
// the file and reading its header, and asking twice would double the cost of a
// scan to learn two things from the same bytes.
func (p *Prober) Probe(ctx context.Context, path string) (Info, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// One invocation for all three questions — how long, what chapters, what
	// is inside — because the expensive part is opening the file and reading
	// its header, and asking three times would pay that three times.
	cmd := exec.CommandContext(ctx, p.Path,
		"-v", "error",
		"-show_entries",
		"format=duration:stream=index,codec_type,codec_name,profile,width,height,channels,bit_rate:"+
			"stream_tags=language,title:stream_disposition=default,forced,hearing_impaired",
		"-show_chapters",
		"-of", "json",
		path)

	out := &capped{limit: maxOutput}
	cmd.Stdout = out
	cmd.Stderr = nil

	if err := cmd.Run(); err != nil {
		return Info{}, fmt.Errorf("probe %s: %w", path, err)
	}

	var parsed probeOutput
	if err := json.Unmarshal(out.buf, &parsed); err != nil {
		return Info{}, fmt.Errorf("probe %s: %w", path, err)
	}
	if parsed.Format.Duration == "" {
		return Info{}, fmt.Errorf("probe %s: no duration reported", path)
	}
	seconds, err := strconv.ParseFloat(parsed.Format.Duration, 64)
	if err != nil {
		return Info{}, fmt.Errorf("probe %s: %w", path, err)
	}

	info := Info{Duration: time.Duration(seconds * float64(time.Second))}
	for _, st := range parsed.Streams {
		stream := Stream{
			Index: st.Index, Type: st.CodecType, Codec: st.CodecName, Profile: st.Profile,
			Width: st.Width, Height: st.Height, Channels: st.Channels,
			Language: st.Tags.Language, Title: st.Tags.Title,
			Default: st.Disposition.Default == 1,
			Forced:  st.Disposition.Forced == 1,
			Hearing: st.Disposition.HearingImpaired == 1,
		}
		if st.BitRate != "" {
			if rate, err := strconv.ParseInt(st.BitRate, 10, 64); err == nil {
				stream.BitRate = rate
			}
		}
		info.Streams = append(info.Streams, stream)
	}
	for i, ch := range parsed.Chapters {
		start, startErr := parseSeconds(ch.StartTime)
		end, endErr := parseSeconds(ch.EndTime)
		if startErr != nil || endErr != nil || end <= start {
			// A chapter with no usable bounds is dropped rather than guessed
			// at: it would otherwise become a marker with the wrong end, and a
			// Skip Intro button that skips into the episode.
			continue
		}
		info.Chapters = append(info.Chapters, domain.Chapter{
			Index: i + 1,
			Title: ch.Tags.Title,
			Start: start,
			End:   end,
		})
	}
	return info, nil
}

func parseSeconds(value string) (time.Duration, error) {
	if value == "" {
		return 0, errors.New("empty")
	}
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, err
	}
	if seconds < 0 {
		return 0, errors.New("negative")
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

// capped collects output up to a limit and discards the rest, so a subprocess
// cannot make this process grow.
type capped struct {
	buf   []byte
	limit int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.limit - len(c.buf); room > 0 {
		if len(p) <= room {
			c.buf = append(c.buf, p...)
		} else {
			c.buf = append(c.buf, p[:room]...)
		}
	}
	// Claims the whole write either way: reporting a short write would make
	// ffprobe's own stdout fail, and the truncated JSON is handled where it is
	// parsed.
	return len(p), nil
}

var _ io.Writer = (*capped)(nil)
