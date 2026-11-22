// Package nowplaying is who is watching what, right now, and the one button
// that can stop them.
//
// Every surface this server speaks already tells it where a viewer is: a
// Jellyfin client posts to /Sessions/Playing every few seconds, a Plex client
// calls /:/timeline, the browser posts its own position. Until now all three
// wrote a bookmark and forgot the rest, so the server knew where everybody
// had got to and nothing at all about who was watching. This is the register
// those reports also write to.
//
// Three decisions shape it:
//
//   - It is memory, not a table. A stream is true only while it is happening,
//     and a row that outlives the process it describes is a lie that has to be
//     cleaned up later. Restarting the server ends every stream by definition.
//   - A stream nobody has reported for a while is over. There is no event for
//     a television being switched off at the wall, which is the same problem
//     the transcode watchdog has and the same answer: a deadline.
//   - Terminating is a latch, not a message. This server cannot push anything
//     to a client — there is no websocket here — so stopping somebody means
//     killing the work and then refusing the next request, whenever it comes.
//     The latch is what makes the refusal outlive the one request that was in
//     flight when the administrator pressed the button.
package nowplaying

import (
	"sort"
	"sync"
	"time"
)

// defaultIdle is how long a stream survives with nobody reporting it.
//
// Clients report every ten seconds or so, and a pause is still a report, so a
// gap this long means the client is not there any more. Long enough to ride
// out a dropped request on a bad connection, short enough that the page is
// not showing somebody who went to bed.
const defaultIdle = 90 * time.Second

// Stopper is the part of the transcode manager this package uses: the two
// ways of ending work that has been started for a stream.
type Stopper interface {
	Stop(id string)
	StopKey(key string)
}

// Stream is one viewer watching one thing on one device.
type Stream struct {
	// Key identifies the stream across the requests that make it up. It is
	// derived from whatever the client's own protocol uses to mean "this
	// play" — a Jellyfin play session id, a Plex session identifier — so the
	// progress report and the file request can be recognised as the same
	// stream without this server handing out an id of its own.
	Key string

	// Surface is which protocol the client is speaking: jellyfin, plex or web.
	Surface string

	UserID   int64
	UserName string

	ItemID int64

	// Title is what to show, and Subtitle the line under it: a series and its
	// episode, an artist and their album.
	Title    string
	Subtitle string

	// Client, Device and Address are who is asking, as they described
	// themselves. Reported for display only and never trusted for anything:
	// a header is whatever the client wrote in it.
	Client  string
	Device  string
	Address string

	// Mode is direct, remux or transcode, and Why the sentence the playback
	// decision wrote when it was not direct. The pair is the answer to the
	// question an operator actually has when the machine is hot.
	Mode string
	Why  string

	// Session is the transcoding session behind this stream, where there is
	// one. Empty for a direct play, which costs this server nothing to serve
	// and has nothing to kill but the stream itself.
	Session string

	Position time.Duration
	Duration time.Duration
	Paused   bool

	// Since is when this stream was first reported and Updated when it last
	// was. Since survives every later report, so the page can say how long
	// somebody has been watching rather than how long ago they checked in.
	Since   time.Time
	Updated time.Time

	// Terminated and Reason are the latch. A terminated stream is not shown
	// as playing any more, but it stays in the register so that the next
	// request carrying its key can be refused with the reason.
	Terminated bool
	Reason     string
}

// Registry holds the live streams.
type Registry struct {
	idle    time.Duration
	now     func() time.Time
	stopper Stopper

	mu      sync.Mutex
	streams map[string]*Stream
}

// Options configure a registry.
type Options struct {
	// Idle is how long a stream survives unreported. Zero means ninety
	// seconds.
	Idle time.Duration

	// Transcoder is how terminating ends the work behind a stream. Nil means
	// a server with no ffmpeg, where there is none.
	Transcoder Stopper

	// Now is the clock, for tests.
	Now func() time.Time
}

// New prepares a registry.
func New(opts Options) *Registry {
	idle := opts.Idle
	if idle <= 0 {
		idle = defaultIdle
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Registry{idle: idle, now: now, stopper: opts.Transcoder, streams: map[string]*Stream{}}
}

// Report records a stream, or updates one that is already known.
//
// Called from the handler a client was going to call anyway, so a viewer
// appears on the page without the client being asked to do anything new.
//
// Fields that the caller may not know are not overwritten with nothing: a
// progress report carries a position and no transcoding details, and the
// negotiation that started the transcode carried the details and no position.
// Each request fills in what it knows and the register keeps the rest.
func (r *Registry) Report(in Stream) {
	if in.Key == "" {
		return
	}
	now := r.now()

	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(now)

	existing, found := r.streams[in.Key]
	if !found {
		fresh := in
		fresh.Since = now
		fresh.Updated = now
		r.streams[in.Key] = &fresh
		return
	}

	existing.Updated = now
	existing.Paused = in.Paused
	if in.Position > 0 {
		existing.Position = in.Position
	}
	if in.Duration > 0 {
		existing.Duration = in.Duration
	}
	if in.ItemID != 0 && in.ItemID != existing.ItemID {
		// The same client playing something else: a play session id is reused
		// across an episode boundary by some clients, and showing the
		// previous episode's title for the rest of the evening is worse than
		// treating it as a new stream.
		existing.ItemID = in.ItemID
		existing.Title = in.Title
		existing.Subtitle = in.Subtitle
		existing.Since = now
		existing.Position = in.Position
		existing.Duration = in.Duration
		existing.Mode, existing.Why, existing.Session = in.Mode, in.Why, in.Session
		return
	}
	if in.Title != "" {
		existing.Title = in.Title
	}
	if in.Subtitle != "" {
		existing.Subtitle = in.Subtitle
	}
	if in.UserName != "" {
		existing.UserName = in.UserName
	}
	if in.UserID != 0 {
		existing.UserID = in.UserID
	}
	if in.Client != "" {
		existing.Client = in.Client
	}
	if in.Device != "" {
		existing.Device = in.Device
	}
	if in.Address != "" {
		existing.Address = in.Address
	}
	if in.Mode != "" {
		existing.Mode = in.Mode
	}
	if in.Why != "" {
		existing.Why = in.Why
	}
	if in.Session != "" {
		existing.Session = in.Session
	}
}

// Stopped forgets a stream, because the client said it had finished. The
// cheapest and most reliable signal there is, and the reason the deadline is
// a safety net rather than the mechanism.
func (r *Registry) Stopped(key string) {
	if key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.streams, key)
}

// List is everything playing, newest first.
//
// Terminated streams are left out: somebody who has been stopped is not
// watching, whatever their client is still asking for.
func (r *Registry) List() []Stream {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(now)

	out := make([]Stream, 0, len(r.streams))
	for _, stream := range r.streams {
		if stream.Terminated {
			continue
		}
		out = append(out, *stream)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Since.Equal(out[j].Since) {
			return out[i].Since.After(out[j].Since)
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Count is how many streams are playing, for the status line.
func (r *Registry) Count() int {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(now)

	n := 0
	for _, stream := range r.streams {
		if !stream.Terminated {
			n++
		}
	}
	return n
}

// CountFor is how many streams this account is playing, not counting one it
// is about to start again.
//
// For the cap on how many things one account may play at once. The key is
// excluded because a client re-reading its own playlist or renegotiating the
// same stream is not a second stream, and counting it as one would mean a cap
// of one could never play anything.
func (r *Registry) CountFor(userID int64, except string) int {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(now)

	n := 0
	for key, stream := range r.streams {
		if stream.Terminated || stream.UserID != userID || key == except {
			continue
		}
		n++
	}
	return n
}

// Terminate stops a stream and arms the latch.
//
// The order matters. The latch is set first, so a request that arrives while
// the encoder is being killed is refused rather than being handed a segment
// from a process that is about to die. Then the work behind it is ended: the
// session by name where the stream was transcoding, and the key as well,
// because a client that renegotiated may have started another one.
func (r *Registry) Terminate(key, reason string) (Stream, bool) {
	if reason == "" {
		reason = "An administrator stopped this stream."
	}
	r.mu.Lock()
	stream, found := r.streams[key]
	if found {
		stream.Terminated = true
		stream.Reason = reason
	}
	var copied Stream
	if found {
		copied = *stream
	}
	r.mu.Unlock()

	if !found {
		return Stream{}, false
	}
	if r.stopper != nil {
		if copied.Session != "" {
			r.stopper.Stop(copied.Session)
		}
		r.stopper.StopKey(copied.Key)
	}
	return copied, true
}

// Stopping reports whether this stream has been terminated, and why.
//
// Every request that serves bytes for a stream asks this first. It is the
// whole of the enforcement: there is nothing to push to a client, so the
// refusal has to be waiting for it when it next asks.
func (r *Registry) Stopping(key string) (string, bool) {
	if key == "" {
		return "", false
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(now)

	stream, found := r.streams[key]
	if !found || !stream.Terminated {
		return "", false
	}
	// Touched, so the latch outlives a client that keeps asking. A player
	// that retries for a minute should be refused for that whole minute
	// rather than have the latch expire underneath it and start working
	// again.
	stream.Updated = now
	return stream.Reason, true
}

// expireLocked drops streams nobody has reported inside the deadline.
func (r *Registry) expireLocked(now time.Time) {
	cutoff := now.Add(-r.idle)
	for key, stream := range r.streams {
		if stream.Updated.Before(cutoff) {
			delete(r.streams, key)
		}
	}
}
