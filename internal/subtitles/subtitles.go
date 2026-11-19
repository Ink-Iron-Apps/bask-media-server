// Package subtitles gets the text of a subtitle out of wherever it lives and
// into the format a client asked for.
//
// Three cases, in order of what they cost:
//
//   - The file beside the media, in the format already wanted: sent as it is.
//   - SubRip wanted as WebVTT, which is the browser's case and is a small
//     transform, done here rather than by starting a process for it.
//   - Anything else — a stream inside the container, an ASS file, a format
//     conversion — which is one short ffmpeg.
//
// Pictures are not text. PGS and VobSub subtitles are images of words, and
// turning them into text means reading them; this package says so rather than
// producing an empty file.
package subtitles

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// ErrNotText means the subtitle is a picture: it can be rendered by a player
// that knows how, or burned into the video, but it cannot be handed over as
// text.
var ErrNotText = errors.New("that subtitle is a picture, not text")

// ErrNoConverter means a conversion was needed and there is no ffmpeg.
var ErrNoConverter = errors.New("this server cannot convert that subtitle")

// maxText caps what one subtitle may be. A feature-length SubRip file is
// tens of kilobytes; a megabyte is already a damaged file, and eight is well
// past anything legitimate — see rule 1 of docs/RESOURCE-BUDGET.md.
const maxText = 8 << 20

// Converter turns a stored subtitle into text.
type Converter struct {
	// FFmpeg is the binary used for anything that is not a direct read or a
	// SubRip-to-WebVTT transform. Empty means those are the only two things
	// this server can do, which still covers a library of sidecar .srt files
	// being watched in a browser.
	FFmpeg string

	// Timeout bounds one conversion.
	Timeout time.Duration
}

// Format is what a client asked for.
type Format string

const (
	SRT  Format = "srt"
	VTT  Format = "vtt"
	ASS  Format = "ass"
	Auto Format = "" // whatever the subtitle already is
)

// ContentType is what to label a format as over HTTP.
func (f Format) ContentType() string {
	switch f {
	case VTT:
		return "text/vtt; charset=utf-8"
	case ASS:
		return "text/x-ssa; charset=utf-8"
	default:
		return "application/x-subrip; charset=utf-8"
	}
}

// FormatOf reads a format from a file extension or a codec name.
func FormatOf(name string) Format {
	switch strings.ToLower(strings.TrimPrefix(name, ".")) {
	case "vtt", "webvtt":
		return VTT
	case "ass", "ssa":
		return ASS
	case "srt", "subrip":
		return SRT
	default:
		return Auto
	}
}

// Text returns one subtitle in the format asked for.
func (c *Converter) Text(ctx context.Context, sub domain.Subtitle, want Format) ([]byte, Format, error) {
	if !sub.Text() {
		return nil, "", ErrNotText
	}
	have := FormatOf(sub.Codec)
	if want == Auto {
		want = have
		if want == Auto {
			want = SRT
		}
	}

	if sub.External {
		body, err := readCapped(sub.Path)
		if err != nil {
			return nil, "", err
		}
		switch {
		case want == have:
			return body, want, nil
		case have == SRT && want == VTT:
			// The common case for a browser, and a transform small enough
			// that starting a process for it would be the expensive part.
			return SRTToVTT(body), VTT, nil
		}
	}

	if c.FFmpeg == "" {
		return nil, "", ErrNoConverter
	}
	body, err := c.convert(ctx, sub, want)
	if err != nil {
		return nil, "", err
	}
	return body, want, nil
}

// convert runs ffmpeg over the file, taking either the sidecar or one stream
// of the container, and writing to standard output.
func (c *Converter) convert(ctx context.Context, sub domain.Subtitle, want Format) ([]byte, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	muxer := "srt"
	switch want {
	case VTT:
		muxer = "webvtt"
	case ASS:
		muxer = "ass"
	}

	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-i", sub.Path}
	if !sub.External {
		// The index is the stream's position in the container, which is what
		// -map addresses with the 0:i: form.
		args = append(args, "-map", "0:"+strconv.Itoa(sub.StreamIndex))
	}
	args = append(args, "-f", muxer, "-")

	cmd := exec.CommandContext(ctx, c.FFmpeg, args...)
	out := &capped{limit: maxText}
	errs := &capped{limit: 4 << 10}
	cmd.Stdout, cmd.Stderr = out, errs

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("convert subtitle: %w: %s", err, strings.TrimSpace(string(errs.buf)))
	}
	if len(out.buf) == 0 {
		return nil, errors.New("the subtitle converted to nothing")
	}
	return out.buf, nil
}

// SRTToVTT turns SubRip into WebVTT.
//
// The two formats are the same idea with three differences: a header, a dot
// instead of a comma in timestamps, and cue numbers that WebVTT ignores but
// tolerates. Doing it here rather than by running ffmpeg is the difference
// between a few microseconds and a process, on the single most common
// subtitle in any library.
func SRTToVTT(body []byte) []byte {
	body = bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF}) // a byte order mark from a Windows editor

	var out bytes.Buffer
	out.Grow(len(body) + 16)
	out.WriteString("WEBVTT\n\n")

	for line := range bytes.SplitSeq(body, []byte("\n")) {
		text := bytes.TrimRight(line, "\r")
		if timing := bytes.Index(text, []byte("-->")); timing >= 0 {
			// Commas are the decimal separator in SubRip and a syntax error
			// in WebVTT.
			text = bytes.ReplaceAll(text, []byte(","), []byte("."))
		}
		out.Write(text)
		out.WriteByte('\n')
	}
	return out.Bytes()
}

// readCapped reads a sidecar file, refusing one that is implausibly large,
// and makes sure what comes back is text.
func readCapped(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxText {
		return nil, fmt.Errorf("subtitle is %d bytes, which is not a subtitle", info.Size())
	}

	body := make([]byte, info.Size())
	// ReadFull rather than Read: one Read is allowed to return less than the
	// buffer, and a subtitle truncated at an arbitrary byte is worse than an
	// error.
	if _, err := io.ReadFull(file, body); err != nil && info.Size() > 0 {
		return nil, err
	}
	if !utf8.Valid(body) {
		// Subtitles from the days of regional code pages are still in
		// circulation. Latin-1 is the one they are nearly always in, and
		// decoding it here beats handing a browser bytes it will draw as
		// mojibake.
		body = latin1ToUTF8(body)
	}
	return body, nil
}

func latin1ToUTF8(body []byte) []byte {
	out := make([]byte, 0, len(body)*2)
	for _, b := range body {
		out = utf8.AppendRune(out, rune(b))
	}
	return out
}

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
	return len(p), nil
}
