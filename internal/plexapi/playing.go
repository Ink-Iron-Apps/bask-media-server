package plexapi

import (
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/nowplaying"
	"github.com/inknironapps/bask-media-server/internal/playback"
)

// Who is watching what, on this surface, and how a Plex client is told its
// stream has been stopped.
//
// Plex has a protocol answer for this and it is worth using: the response to
// a timeline report may carry a termination code and a sentence, and clients
// honour it by stopping and showing the sentence. So a terminated stream is
// told twice — politely, the next time it reports where it is, and firmly,
// the next time it asks for bytes.

// plexTerminationStopped is the code Plex servers send for a stream an
// administrator ended. The clients key their behaviour off the code and show
// the text, so both have to be there.
const plexTerminationStopped = 2006

// streamKey identifies a stream across the requests that make it up: the
// client's own identifier and the item.
//
// Not the session identifier a client puts on its timeline reports, because
// it does not put one on the range request for the file — and the range
// request is the only thing a direct play consists of. The client identifier
// is on every request a Plex client makes.
func streamKey(r *http.Request, itemID int64) string {
	device := strings.TrimSpace(r.Header.Get("X-Plex-Client-Identifier"))
	if device == "" {
		device = deviceFrom(r)
	}
	if device == "" {
		if user := userFrom(r.Context()); user != nil {
			device = "user:" + strconv.FormatInt(user.ID, 10)
		}
	}
	if device == "" {
		return ""
	}
	return "plex|" + device + "|" + strconv.FormatInt(itemID, 10)
}

// report tells the register about this stream.
func (s *Server) report(r *http.Request, in nowplaying.Stream) {
	if s.Playing == nil || in.Key == "" {
		return
	}
	in.Surface = "plex"
	if user := userFrom(r.Context()); user != nil {
		in.UserID, in.UserName = user.ID, user.Username
	}
	in.Client = strings.TrimSpace(r.Header.Get("X-Plex-Product"))
	in.Device = deviceFrom(r)
	in.Address = addressOf(r)
	// Every stream on this surface is the file itself: there is no
	// transcoding endpoint here, and a Plex client that cannot play
	// something plays nothing rather than asking this server to convert it.
	if in.Mode == "" {
		in.Mode = string(playback.ModeDirect)
	}
	s.Playing.Report(in)
}

func (s *Server) forget(key string) {
	if s.Playing != nil {
		s.Playing.Stopped(key)
	}
}

// refused answers a request belonging to a terminated stream.
func (s *Server) refused(w http.ResponseWriter, key string) bool {
	if s.Playing == nil {
		return false
	}
	reason, stopping := s.Playing.Stopping(key)
	if !stopping {
		return false
	}
	http.Error(w, reason, http.StatusForbidden)
	return true
}

// addressOf is where a request came from, for the page.
func addressOf(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		if first, _, found := strings.Cut(forwarded, ","); found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(forwarded)
	}
	// SplitHostPort rather than cutting at a colon: an IPv6 address is full
	// of them, and the one that matters is the last.
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
