// Package transcode turns a file a client cannot play into one it can, while
// it plays.
//
// The shape is HLS: ffmpeg writes numbered segments into a directory and the
// server hands them out as they are asked for. That is how every server in
// this space does it, and for good reasons — a segment is a plain file, the
// client decides how far ahead to read, and a client that walks away stops
// asking, which is how this package knows to stop working.
//
// Rule 5 of docs/RESOURCE-BUDGET.md is the whole design constraint here: an
// abandoned ffmpeg is the biggest real resource leak in this class of
// software, because nothing about the client's departure is an event. So
// every session has an idle deadline, every process is started in its own
// process group and killed as a group, the number of them at once is capped,
// and the manager kills what is left when the server stops.
package transcode

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/inknironapps/bask-media-server/internal/playback"
)

// ErrBusy means every transcoding slot is in use. One machine can encode a
// couple of streams; pretending otherwise turns one viewer's film into
// everybody's stutter.
var ErrBusy = errors.New("this server is already transcoding as much as it can")

// ErrNotReady means a segment was asked for and is still being written. It is
// a timeout rather than a failure: the client should ask again.
var ErrNotReady = errors.New("that part of the file is not ready yet")

// ErrNoSegment means there is no such part of the file: the encoder reached
// the end without producing it, so no amount of waiting will help.
var ErrNoSegment = errors.New("no such segment")

const (
	defaultSegment     = 4 * time.Second
	defaultIdle        = 60 * time.Second
	defaultSessions    = 3
	defaultWait        = 30 * time.Second
	initName           = "init.mp4"
	ffmpegPlaylistName = "stream.m3u8"
	// segmentsAhead is how far past what has been produced a client may ask
	// before the encoder is restarted there instead. Enough to cover normal
	// buffering, small enough that a seek does not wait for minutes of
	// encoding nobody will watch.
	segmentsAhead = 3
)

// Options configure a manager. They are settled once, at construction,
// because the watchdog goroutine reads them while requests are being served
// and a field somebody can change later is a field somebody can change
// during a read.
type Options struct {
	// FFmpeg is the binary to run. Empty looks on PATH.
	FFmpeg string

	// Root is where session directories live. Empty uses a directory in the
	// system temporary folder. Whatever is there when the server starts is
	// removed: it belongs to a run that is not coming back.
	Root string

	// MaxSessions is how many files may be transcoded at once. Zero means
	// three, which is about what one desktop can do without the people using
	// it noticing.
	MaxSessions int

	// IdleTimeout is how long a session survives with nobody asking it for
	// anything. Zero means a minute.
	IdleTimeout time.Duration

	// SegmentLength is how long each piece of the stream is. Zero means four
	// seconds, which is what most players are tuned for.
	SegmentLength time.Duration

	// Accel asks for hardware encoding. Empty or "auto" tries what this
	// machine has, in order, and keeps the first that proves it works; "off"
	// stays on the processor; a name — vaapi, nvenc, qsv, videotoolbox, amf
	// — tries only that one and falls back to the processor if it does not
	// work, rather than refusing to start.
	//
	// Hardware encoding is an optimisation, so no setting of this can stop a
	// file being watchable.
	Accel string

	Log *slog.Logger
}

// Manager owns the transcoding sessions.
type Manager struct {
	ffmpeg  string
	root    string
	options Options
	accel   *Accel // nil means the processor

	mu       sync.Mutex
	sessions map[string]*Session
	byKey    map[string]*Session
	stop     chan struct{}
	stopped  bool
	wg       sync.WaitGroup
}

// New prepares a manager and starts the watchdog that reaps idle sessions.
func New(opts Options) (*Manager, error) {
	ffmpeg := opts.FFmpeg
	if ffmpeg == "" {
		found, err := exec.LookPath("ffmpeg")
		if err != nil {
			return nil, fmt.Errorf("ffmpeg not found: %w", err)
		}
		ffmpeg = found
	}
	root := opts.Root
	if root == "" {
		root = filepath.Join(os.TempDir(), "bms-transcode")
	}
	// Absolute, always. Every session runs ffmpeg with the session directory
	// as its working directory, so a relative root — which is what a server
	// started with a relative database path hands over — would be resolved
	// against that directory instead of against where the server was started,
	// and ffmpeg writes its segments into a path that does not exist. The
	// failure is one line of ffmpeg stderr about an init segment, nowhere
	// near the cause.
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("transcode directory: %w", err)
	}
	root = absolute
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("transcode directory: %w", err)
	}
	// Anything already in there belongs to a previous run of this server,
	// which is not coming back for it.
	if entries, err := os.ReadDir(root); err == nil {
		for _, entry := range entries {
			_ = os.RemoveAll(filepath.Join(root, entry.Name()))
		}
	}

	m := &Manager{
		ffmpeg: ffmpeg, root: root, options: opts,
		sessions: map[string]*Session{}, byKey: map[string]*Session{},
		stop: make(chan struct{}),
	}

	// Probed at startup rather than on the first stream, so the cost lands
	// here instead of on whoever presses play first, and so the log says on
	// one line what this server will be doing for the rest of its life.
	m.accel = Detect(context.Background(), ffmpeg, opts.Accel)
	if m.accel != nil {
		m.logger().Info("encoding with hardware", "accelerator", m.accel.Name, "encoder", m.accel.Encoder)
	} else if asked := strings.ToLower(strings.TrimSpace(opts.Accel)); asked != "" && asked != "off" && asked != "auto" {
		m.logger().Warn("no hardware encoder; staying on the processor", "asked", opts.Accel)
	}

	m.wg.Add(1)
	go m.reap()
	return m, nil
}

// Accel names the hardware encoder in use, or "" for the processor. For the
// status page, which is where an operator finds out whether the card they
// installed is being used.
func (m *Manager) Accel() string {
	if m.accel == nil {
		return ""
	}
	return m.accel.Name
}

// Root is where this manager keeps its working files.
func (m *Manager) Root() string { return m.root }

// Request is one client wanting one file played.
type Request struct {
	// Key identifies what is being played, independently of when: the same
	// file, plan and client gets the same session back rather than a second
	// ffmpeg, which is what a page reload would otherwise cost.
	Key string

	Path     string
	Plan     playback.Plan
	Duration time.Duration

	// StartAt is where playback begins. A seek past what has been produced
	// restarts the encoder here rather than encoding everything in between.
	StartAt time.Duration
}

// Start returns a session for this request, reusing one where it can.
func (m *Manager) Start(req Request) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return nil, errors.New("the server is shutting down")
	}

	if existing, found := m.byKey[req.Key]; found && req.Key != "" {
		existing.Touch()
		return existing, nil
	}
	if len(m.sessions) >= m.maxSessions() {
		// Idle sessions are reaped on a timer, but a viewer waiting now
		// should not wait for the next tick.
		m.reapIdleLocked()
		if len(m.sessions) >= m.maxSessions() {
			return nil, ErrBusy
		}
	}

	id := newID()
	dir := filepath.Join(m.root, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("session directory: %w", err)
	}

	session := &Session{
		ID:       id,
		manager:  m,
		dir:      dir,
		req:      req,
		segment:  m.segmentLength(),
		accel:    m.accel,
		lastSeen: time.Now(),
	}
	m.sessions[id] = session
	if req.Key != "" {
		m.byKey[req.Key] = session
	}

	if err := session.startAt(segmentOf(req.StartAt, session.segment)); err != nil {
		delete(m.sessions, id)
		delete(m.byKey, req.Key)
		_ = os.RemoveAll(dir)
		return nil, err
	}
	m.logger().Info("started transcoding",
		"session", id, "path", req.Path, "mode", string(req.Plan.Mode),
		"video", req.Plan.VideoCodec, "audio", req.Plan.AudioCodec, "startAt", req.StartAt.Round(time.Second))
	return session, nil
}

// Get returns a live session.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, found := m.sessions[id]
	return session, found
}

// Stop ends one session: the client said it was done, which is the cheapest
// and most reliable signal there is.
func (m *Manager) Stop(id string) {
	m.mu.Lock()
	session, found := m.sessions[id]
	if found {
		delete(m.sessions, id)
		if session.req.Key != "" && m.byKey[session.req.Key] == session {
			delete(m.byKey, session.req.Key)
		}
	}
	m.mu.Unlock()
	if found {
		session.close()
		m.logger().Info("stopped transcoding", "session", id)
	}
}

// Sessions reports how many are running, for the status page.
// StopKey ends the session started for this key, if there is one.
//
// A client that says it has stopped names what it was playing rather than the
// session: a Jellyfin client reports its own play session id, and that is
// what started this one. Freeing the core now beats waiting for the idle
// deadline to notice.
func (m *Manager) StopKey(key string) {
	if key == "" {
		return
	}
	m.mu.Lock()
	session, found := m.byKey[key]
	m.mu.Unlock()
	if found {
		m.Stop(session.ID)
	}
}

// StopPrefix ends every session whose key begins with this prefix.
//
// For a client that says it has stopped but names only part of what it was
// playing: a Jellyfin client reports its play session id, while the key also
// carries which file and which burned-in subtitle it was — so the thing it
// can name is a prefix of the thing that has to be stopped.
func (m *Manager) StopPrefix(prefix string) {
	if prefix == "" {
		return
	}
	m.mu.Lock()
	var ids []string
	for key, session := range m.byKey {
		if strings.HasPrefix(key, prefix) {
			ids = append(ids, session.ID)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.Stop(id)
	}
}

func (m *Manager) Sessions() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// Shutdown kills everything still running and removes its working files.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	m.stopped = true
	close(m.stop)
	running := make([]*Session, 0, len(m.sessions))
	for _, session := range m.sessions {
		running = append(running, session)
	}
	m.sessions = map[string]*Session{}
	m.byKey = map[string]*Session{}
	m.mu.Unlock()

	for _, session := range running {
		session.close()
	}
	m.wg.Wait()
	_ = os.RemoveAll(m.root)
}

// reap kills sessions nobody is asking for any more.
//
// The watchdog exists because the obvious signal does not: a client that
// closes a laptop sends nothing, and an ffmpeg encoding to a directory nobody
// reads would otherwise run to the end of the film.
func (m *Manager) reap() {
	defer m.wg.Done()
	// The interval follows the deadline rather than being a constant, so a
	// short timeout — a test, or a server tuned for a small machine — is
	// actually honoured instead of waiting for the next tick of a timer that
	// was sized for the default.
	interval := min(max(m.idleTimeout()/3, 200*time.Millisecond), 10*time.Second)
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-timer.C:
			m.mu.Lock()
			m.reapIdleLocked()
			m.mu.Unlock()
			timer.Reset(interval)
		}
	}
}

func (m *Manager) reapIdleLocked() {
	deadline := time.Now().Add(-m.idleTimeout())
	for id, session := range m.sessions {
		if session.LastSeen().After(deadline) {
			continue
		}
		delete(m.sessions, id)
		if session.req.Key != "" && m.byKey[session.req.Key] == session {
			delete(m.byKey, session.req.Key)
		}
		// Closed off the lock, because killing a process group and removing
		// its files takes long enough that holding the lock would stall
		// every other request — and counted on the WaitGroup, because
		// Shutdown removes the root directory and must not do that while a
		// reaper is still killing something inside it.
		m.wg.Add(1)
		go func(s *Session) {
			defer m.wg.Done()
			s.close()
			m.logger().Info("reaped an idle transcode", "session", s.ID)
		}(session)
	}
}

func (m *Manager) maxSessions() int {
	if m.options.MaxSessions > 0 {
		return m.options.MaxSessions
	}
	return defaultSessions
}

func (m *Manager) idleTimeout() time.Duration {
	if m.options.IdleTimeout > 0 {
		return m.options.IdleTimeout
	}
	return defaultIdle
}

func (m *Manager) segmentLength() time.Duration {
	if m.options.SegmentLength > 0 {
		return m.options.SegmentLength
	}
	return defaultSegment
}

func (m *Manager) logger() *slog.Logger {
	if m.options.Log != nil {
		return m.options.Log
	}
	return slog.Default()
}

func segmentOf(at, length time.Duration) int {
	if at <= 0 || length <= 0 {
		return 0
	}
	return int(at / length)
}

// newID is a name for a session directory and a URL. Not a credential: the
// routes that serve a session are behind the same session cookie as
// everything else.
func newID() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.Itoa(os.Getpid()%9973)
}

// readPlaylist returns the segment names ffmpeg has finished writing, and
// whether it has declared the file complete.
//
// ffmpeg's own playlist is the authority on which segments exist: a file on
// disk may be half-written, and a name in the playlist means the muxer closed
// it.
func readPlaylist(path string) (names []string, ended bool, err error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "#EXT-X-ENDLIST" {
			ended = true
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		names = append(names, line)
	}
	return names, ended, scanner.Err()
}
