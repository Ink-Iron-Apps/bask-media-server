package web

import (
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/nowplaying"
)

// Who is watching what, from the browser, and how a stream here is stopped.

// streamKey identifies a stream across the requests that make it up.
//
// The account and the item, because a browser has no device identifier of its
// own and the one thing that could stand in for one is the session cookie,
// which is a credential and has no business in a list on a page. The cost is
// that one account watching the same thing in two browsers at once is one
// line rather than two — a situation nobody is actually in, and the lesser of
// the two problems.
func streamKey(r *http.Request, itemID int64) string {
	user := userFrom(r.Context())
	if user == nil {
		return ""
	}
	return "web|user:" + strconv.FormatInt(user.ID, 10) + "|" + strconv.FormatInt(itemID, 10)
}

// report tells the register about this stream.
func (s *Server) report(r *http.Request, in nowplaying.Stream) {
	if s.Playing == nil || in.Key == "" {
		return
	}
	in.Surface = "web"
	if user := userFrom(r.Context()); user != nil {
		in.UserID, in.UserName = user.ID, user.Username
	}
	// The browser's own description of itself is all there is, and it is
	// long. The first token of the user agent is the readable part of it.
	in.Client = "Browser"
	in.Device = browserOf(r.UserAgent())
	in.Address = addressOf(r)
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
	// The page watches for this status while playing and shows the sentence,
	// so a viewer learns why the film stopped rather than seeing the player
	// fail.
	writeError(w, http.StatusForbidden, reason)
	return true
}

// browserOf is which browser this is, from the user agent, for the page.
//
// Reported by the browser and therefore worth nothing except as a label. The
// list is short on purpose: a user agent is not a fact about a device, and
// trying to parse one properly is a library and a monthly update.
func browserOf(agent string) string {
	switch {
	case agent == "":
		return "Browser"
	case strings.Contains(agent, "Firefox/"):
		return "Firefox"
	case strings.Contains(agent, "Edg/"):
		return "Edge"
	case strings.Contains(agent, "OPR/"):
		return "Opera"
	case strings.Contains(agent, "Chrome/"):
		return "Chrome"
	case strings.Contains(agent, "Safari/"):
		return "Safari"
	default:
		return "Browser"
	}
}

// addressOf is where a request came from.
func addressOf(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		if first, _, found := strings.Cut(forwarded, ","); found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(forwarded)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// tooManyStreams reports whether this account is already playing as much as
// it is allowed, and answers the page if so.
//
// Counted from the register of what is playing now, so a stream that ended
// without saying so stops counting at the idle deadline rather than using up
// a cap for ever.
func (s *Server) tooManyStreams(w http.ResponseWriter, r *http.Request, key string) bool {
	cred := credFrom(r.Context())
	if cred == nil || cred.MaxStreams <= 0 || s.Playing == nil {
		return false
	}
	if s.Playing.CountFor(cred.User.ID, key) < cred.MaxStreams {
		return false
	}
	writeError(w, http.StatusTooManyRequests,
		"this account is already watching as much as it is allowed to at once")
	return true
}
