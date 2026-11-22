package transcode

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Session is one file being made playable for one client.
type Session struct {
	ID string

	manager *Manager
	dir     string
	req     Request
	segment time.Duration

	// accel is the hardware encoder this session uses, or nil for the
	// processor. Copied from the manager when the session starts and then
	// left alone: a session that began on the card finishes on it, so a
	// stream does not change its character halfway through.
	accel *Accel

	mu       sync.Mutex
	cmd      *exec.Cmd
	waiting  chan struct{} // closed when the current process has been reaped
	startSeg int
	produced map[string]bool // segments ffmpeg has finished, across restarts
	highest  int
	ended    bool
	lastSeen time.Time
	stderr   *capped
}

// Mode is what this session is doing to the file, for a status page.
func (s *Session) Mode() string { return string(s.req.Plan.Mode) }

// Touch records that somebody is still watching. Without this the watchdog
// would kill a session during a long pause, and with nothing else to go on it
// is the only evidence there is that the client is still there.
func (s *Session) Touch() {
	s.mu.Lock()
	s.lastSeen = time.Now()
	s.mu.Unlock()
}

// LastSeen is when a client last asked this session for anything.
func (s *Session) LastSeen() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSeen
}

// Playlist is the HLS playlist a player reads.
//
// Two kinds, because two kinds of session produce segments differently:
//
//   - When the video is being re-encoded, keyframes are forced onto segment
//     boundaries, so every segment is exactly as long as asked for and the
//     playlist can be written out for the whole runtime before a single
//     segment exists. That is what gives a full scrub bar from the first
//     second and makes seeking anywhere possible.
//   - When the video is copied, the segments break on the keyframes that are
//     already in the file, so only ffmpeg knows how long each one is. Its own
//     playlist is passed through as it grows — which it does far faster than
//     playback, because copying is not decoding.
func (s *Session) Playlist(ctx context.Context) ([]byte, error) {
	if s.computed() {
		return s.computedPlaylist(), nil
	}
	deadline := time.Now().Add(defaultWait)
	for {
		if body, err := os.ReadFile(filepath.Join(s.dir, ffmpegPlaylistName)); err == nil && len(body) > 0 {
			if names, _, err := readPlaylist(filepath.Join(s.dir, ffmpegPlaylistName)); err == nil && len(names) > 0 {
				return body, nil
			}
		}
		if err := s.waitTick(ctx, deadline); err != nil {
			return nil, err
		}
	}
}

// computed reports whether this session's segments are exactly the length
// they were asked to be, which is true whenever the video is re-encoded.
func (s *Session) computed() bool { return s.req.Plan.VideoCodec != "copy" }

func (s *Session) computedPlaylist() []byte {
	seconds := s.segment.Seconds()
	count := int(s.req.Duration / s.segment)
	remainder := s.req.Duration - time.Duration(count)*s.segment

	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", int(seconds+0.999))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"%s\"\n", initName)
	for i := range count {
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%s\n", seconds, segmentName(i))
	}
	if remainder > time.Second/2 {
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%s\n", remainder.Seconds(), segmentName(count))
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return []byte(b.String())
}

// Segment returns the path of one piece of the file, waiting for it to be
// written and, where it can, moving the encoder to where the client is.
func (s *Session) Segment(ctx context.Context, name string) (string, error) {
	s.Touch()
	if !safeName(name) {
		return "", fmt.Errorf("bad segment name %q", name)
	}
	path := filepath.Join(s.dir, name)

	if name == initName {
		// The init segment is written before anything else, so waiting for it
		// is waiting for ffmpeg to have opened the file at all.
		return path, s.waitFor(ctx, func() bool { return exists(path) })
	}

	index, ok := segmentIndex(name)
	if !ok {
		return "", fmt.Errorf("bad segment name %q", name)
	}
	if s.computed() {
		if err := s.cover(index); err != nil {
			return "", err
		}
	}
	return path, s.waitFor(ctx, func() bool { return s.has(name) })
}

// cover makes sure the encoder is working on, or has already passed, the
// segment being asked for.
//
// A seek forward is the case this exists for: without it the client would
// wait for every minute in between to be encoded, which on a long film is a
// wait nobody sits through. Restarting ffmpeg at the seek point costs a
// process and a second.
func (s *Session) cover(index int) error {
	if s.has(segmentName(index)) {
		return nil
	}
	s.mu.Lock()
	start, highest, ended := s.startSeg, s.highest, s.ended
	s.mu.Unlock()

	// Already on its way: this run began at or before it, and it is close
	// enough ahead to be worth waiting for rather than restarting.
	if index >= start && index <= highest+segmentsAhead && !ended {
		return nil
	}
	// The encoder reached the end of the file and this segment never
	// appeared, so it is past the end rather than late.
	if index >= start && ended {
		return ErrNoSegment
	}
	return s.startAt(index)
}

// startAt runs ffmpeg from a given segment, replacing whatever was running.
func (s *Session) startAt(index int) error {
	s.mu.Lock()
	previous, waiting := s.cmd, s.waiting
	s.cmd, s.waiting = nil, nil
	s.startSeg = index
	s.highest = index - 1
	s.ended = false
	if s.produced == nil {
		s.produced = map[string]bool{}
	}
	s.lastSeen = time.Now()
	s.mu.Unlock()

	if previous != nil {
		killProcessGroup(previous)
		<-waiting
	}

	args := s.args(index)
	cmd := exec.Command(s.manager.ffmpeg, args...)
	cmd.Dir = s.dir
	stderr := &capped{limit: 8 << 10}
	cmd.Stderr = stderr
	cmd.Stdout = nil
	setProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}
	done := make(chan struct{})

	s.mu.Lock()
	s.cmd, s.waiting, s.stderr = cmd, done, stderr
	s.mu.Unlock()

	go func() {
		err := cmd.Wait()
		close(done)
		if err != nil && !killed(err) {
			s.manager.logger().Warn("ffmpeg stopped",
				"session", s.ID, "error", err, "stderr", strings.TrimSpace(stderr.String()))
		}
	}()
	return nil
}

// args builds the command line.
//
// The shape of it is the whole of this package's opinion about transcoding:
// one video stream, one audio stream, no subtitles, segments of a fixed
// length, and — when the video is re-encoded — keyframes forced onto the
// segment boundaries so the playlist can be written before the encoding is.
func (s *Session) args(index int) []string {
	seconds := s.segment.Seconds()
	start := time.Duration(index) * s.segment

	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y"}
	// Ahead of everything, because an accelerator that opens a device wants
	// it open before the input is read.
	if s.accel != nil && s.req.Plan.VideoCodec != "copy" {
		args = append(args, s.accel.Before...)
	}
	if start > 0 {
		// Before the input, so ffmpeg seeks rather than decodes its way
		// there: the difference on a long film is seconds against minutes.
		args = append(args, "-ss", fmt.Sprintf("%.3f", start.Seconds()))
	}
	args = append(args, "-i", s.req.Path, "-copyts", "-muxpreload", "0", "-muxdelay", "0")

	plan := s.req.Plan
	// A bitmap subtitle is composited rather than rendered, which needs the
	// subtitle stream inside the filtergraph and therefore a named output to
	// map. Everything else maps the first video and audio streams straight
	// through.
	if plan.Burning() && plan.Burn.Picture {
		args = append(args, "-filter_complex", s.overlay(plan.Height), "-map", "[vout]", "-map", "0:a:0?", "-sn", "-dn")
	} else {
		args = append(args, "-map", "0:v:0?", "-map", "0:a:0?", "-sn", "-dn")
	}

	if plan.VideoCodec == "copy" {
		args = append(args, "-c:v", "copy")
	} else {
		args = s.video(args, seconds, plan.Height, plan.VideoBitRate)
	}
	if plan.AudioCodec == "copy" {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "aac", "-b:a", "160k")
		if plan.Channels > 0 {
			args = append(args, "-ac", strconv.Itoa(plan.Channels))
		}
	}

	args = append(args,
		"-f", "hls",
		"-hls_time", fmt.Sprintf("%g", seconds),
		"-hls_playlist_type", "event",
		"-hls_list_size", "0",
		"-hls_flags", "independent_segments+temp_file",
		"-hls_segment_type", "fmp4",
		"-hls_fmp4_init_filename", initName,
		"-hls_segment_filename", filepath.Join(s.dir, "seg%05d.m4s"),
		"-start_number", strconv.Itoa(index),
		filepath.Join(s.dir, ffmpegPlaylistName))
	return args
}

// has reports whether a segment has been finished, reading ffmpeg's own
// playlist as the authority and remembering what it said across restarts.
func (s *Session) has(name string) bool {
	s.mu.Lock()
	if s.produced[name] {
		s.mu.Unlock()
		return true
	}
	s.mu.Unlock()

	names, ended, err := readPlaylist(filepath.Join(s.dir, ffmpegPlaylistName))
	if err != nil {
		return false
	}
	s.mu.Lock()
	for _, produced := range names {
		s.produced[produced] = true
		if index, ok := segmentIndex(produced); ok && index > s.highest {
			s.highest = index
		}
	}
	s.ended = ended
	found := s.produced[name]
	s.mu.Unlock()
	return found
}

func (s *Session) waitFor(ctx context.Context, ready func() bool) error {
	deadline := time.Now().Add(defaultWait)
	for {
		if ready() {
			return nil
		}
		if err := s.waitTick(ctx, deadline); err != nil {
			return err
		}
	}
}

func (s *Session) waitTick(ctx context.Context, deadline time.Time) error {
	if time.Now().After(deadline) {
		return ErrNotReady
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(100 * time.Millisecond):
		s.Touch()
		return nil
	}
}

// close kills the process group and removes the session's files.
func (s *Session) close() {
	s.mu.Lock()
	cmd, waiting := s.cmd, s.waiting
	s.cmd, s.waiting = nil, nil
	s.mu.Unlock()

	if cmd != nil {
		killProcessGroup(cmd)
		<-waiting
	}
	_ = os.RemoveAll(s.dir)
}

func segmentName(index int) string { return fmt.Sprintf("seg%05d.m4s", index) }

func segmentIndex(name string) (int, bool) {
	if !strings.HasPrefix(name, "seg") || !strings.HasSuffix(name, ".m4s") {
		return 0, false
	}
	index, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "seg"), ".m4s"))
	if err != nil || index < 0 {
		return 0, false
	}
	return index, true
}

// safeName keeps a URL from naming a file outside the session's directory.
func safeName(name string) bool {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return false
	}
	return name == initName || strings.HasPrefix(name, "seg")
}

func exists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}

// capped collects a subprocess's complaints without letting it grow this
// process: the same reasoning as the probe's output buffer.
type capped struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.limit - len(c.buf); room > 0 {
		if len(p) <= room {
			c.buf = append(c.buf, p...)
		} else {
			c.buf = append(c.buf, p[:room]...)
		}
	}
	return len(p), nil
}

func (c *capped) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.buf)
}

// Drawing subtitles into the picture.
//
// Two different jobs wearing one name. A text subtitle is rendered by libass
// through the `subtitles` filter, which reads the file again itself and picks
// events by the frame's own timestamp — which works here only because the
// command keeps original timestamps with -copyts, so a stream started an hour
// in still lines up. A bitmap subtitle is already pictures, and pictures are
// composited: the stream goes into the filtergraph and is overlaid on the
// video.
//
// The bitmap path gives up on hardware encoding for the session. Compositing
// onto frames that live on a graphics device is a different filter chain for
// every vendor, and getting one of them subtly wrong produces a stream that
// plays with the subtitles in the wrong place — worse than a core's worth of
// software encoding for one film.

// subtitleFilter is the filter that renders a text subtitle, or "".
func (s *Session) subtitleFilter() string {
	burn := s.req.Plan.Burn
	if burn == nil || burn.Picture {
		return ""
	}
	filter := "subtitles=filename=" + escapeFilterValue(burn.Path)
	if burn.Index >= 0 {
		filter += ":si=" + strconv.Itoa(burn.Index)
	}
	return filter
}

// overlay is the filtergraph for a bitmap subtitle: scale the picture, then
// composite the subtitle onto it.
//
// Scaled first so the overlay is done once at the output size rather than at
// the source's, and because scaling a frame that already has subtitles drawn
// on it blurs them.
func (s *Session) overlay(height int) string {
	burn := s.req.Plan.Burn
	video := "[0:v:0]"
	if height > 0 {
		video += fmt.Sprintf("scale=-2:%d,", height)
	}
	// format=yuv420p before the overlay, because a bitmap subtitle arrives as
	// RGB and overlaying onto a format the encoder cannot take is how this
	// fails at the end of the chain rather than at the start.
	video += "format=yuv420p[vbase];"
	subtitle := fmt.Sprintf("[0:%d]", burn.Index)
	return video + "[vbase]" + subtitle + "overlay[vout]"
}

// escapeFilterValue quotes a path for an ffmpeg filter argument.
//
// Filter arguments are parsed twice — once for the graph, once for the filter
// — so a Windows path or a film with a colon in its name needs both the
// backslash and the colon escaped. Single-quoting handles the separators and
// the quote itself is escaped out of and back into the quoting.
func escapeFilterValue(value string) string {
	var out strings.Builder
	out.WriteByte('\'')
	for _, r := range value {
		switch r {
		case '\'':
			// Out of the quotes, an escaped quote, back in.
			out.WriteString(`'\''`)
		case '\\', ':':
			out.WriteByte('\\')
			out.WriteRune(r)
		default:
			out.WriteRune(r)
		}
	}
	out.WriteByte('\'')
	return out.String()
}
