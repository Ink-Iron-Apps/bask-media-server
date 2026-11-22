package jellyfin

import (
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/nowplaying"
)

// Who is watching what, and the refusal that makes stopping them mean
// something.
//
// Jellyfin's own way of ending somebody's stream is a command pushed down a
// websocket, and there is no websocket here. So a termination is enforced the
// way everything else in this server is: on the next request. The client asks
// for the next segment or the next range of the file, and is told no.

// streamKey identifies a stream across the requests that make it up.
//
// The device and the item, not the client's play session id. Every request a
// client makes carries its device in the Authorization header, including the
// range request for a file it is direct playing — and a play session id does
// not: it appears in the progress report and in a transcoding negotiation and
// nowhere else. Keying on what every request carries is what lets a direct
// play be stopped at all.
func streamKey(r *http.Request, itemID int64) string {
	device := deviceFrom(r)
	if device == "" {
		// A client that did not say which device it is gets one stream per
		// account instead. Rarer than it sounds — every real client sends the
		// header — and better than a key that collides across accounts.
		if user := userFrom(r.Context()); user != nil {
			device = "user:" + strconv.FormatInt(user.ID, 10)
		}
	}
	if device == "" {
		return ""
	}
	return "jellyfin|" + device + "|" + strconv.FormatInt(itemID, 10)
}

// report tells the register about this stream, if there is one to tell.
func (s *Server) report(r *http.Request, in nowplaying.Stream) {
	if s.Playing == nil || in.Key == "" {
		return
	}
	in.Surface = "jellyfin"
	if user := userFrom(r.Context()); user != nil {
		in.UserID, in.UserName = user.ID, user.Username
	}
	in.Client = fieldInAuthorization(r.Header.Get("Authorization"), "Client")
	if in.Client == "" {
		in.Client = fieldInAuthorization(r.Header.Get("X-Emby-Authorization"), "Client")
	}
	in.Device = fieldInAuthorization(r.Header.Get("Authorization"), "Device")
	if in.Device == "" {
		in.Device = fieldInAuthorization(r.Header.Get("X-Emby-Authorization"), "Device")
	}
	in.Address = addressOf(r)
	s.Playing.Report(in)
}

// refused answers a request belonging to a terminated stream, and reports
// whether it did.
//
// 403 rather than 410: the stream was ended by somebody, which is not the
// same as a session that timed out, and the body carries the reason so a
// player with somewhere to put it can show the household why the film
// stopped.
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

// forget drops a stream, because the client said it had finished.
func (s *Server) forget(key string) {
	if s.Playing != nil {
		s.Playing.Stopped(key)
	}
}

// playingOf is the stream a progress report describes.
func playingOf(r *http.Request, item *domain.Item, body report) nowplaying.Stream {
	return nowplaying.Stream{
		Key:      streamKey(r, item.ID),
		ItemID:   item.ID,
		Title:    item.Title,
		Position: durationOfTicks(body.PositionTicks),
		Duration: item.Duration,
		Paused:   body.IsPaused,
	}
}

// addressOf is where a request came from, for the page. The proxy header
// where there is one, because this server is routinely behind one.
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
