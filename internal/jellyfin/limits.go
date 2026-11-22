package jellyfin

import (
	"net/http"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/playback"
)

// What an account may take of a server the whole household shares.
//
// Two caps, and they work in opposite ways on purpose. A bandwidth cap
// degrades: the stream is converted down to fit rather than refused, which is
// what somebody on a slow line actually wants. A stream cap refuses: there is
// no smaller version of "a third film at once", and the honest answer is to
// say so.

// capped folds the account's bandwidth cap into what the client asked for.
//
// The lower of the two wins, and either being unset means the other stands
// alone. Folded into the client's own ceiling rather than applied separately
// because the playback decision already knows what to do with one: a file
// above it is converted down, and the ladder is picked to fit.
func (w wish) capped(cred *domain.Credential) wish {
	if cred == nil || cred.MaxBitRate <= 0 {
		return w
	}
	if w.maxBitRate <= 0 || cred.MaxBitRate < w.maxBitRate {
		w.maxBitRate = cred.MaxBitRate
		// The client said nothing about itself, but the household has: that
		// is enough to decide with, so the negotiation must not take the
		// shortcut it takes for a client that said nothing at all.
		w.asked = true
	}
	return w
}

// cappedPlan holds a stream request to the account's bandwidth cap.
//
// The negotiation above already chose a ladder that fits, but the stream
// request is a fresh set of query parameters and a client is free to ask for
// anything in it.
func cappedPlan(plan playback.Plan, cred *domain.Credential) playback.Plan {
	if cred == nil || cred.MaxBitRate <= 0 {
		return plan
	}
	if plan.VideoBitRate <= 0 || plan.VideoBitRate > cred.MaxBitRate {
		plan.VideoBitRate = cred.MaxBitRate
	}
	return plan
}

// tooManyStreams reports whether this account is already playing as much as
// it is allowed, and answers the client if so.
//
// Counted from the register of what is playing now, which is the only thing
// that knows: a stream that ended without saying so stops counting at the
// idle deadline, so a cap cannot be used up by a television somebody switched
// off at the wall.
func (s *Server) tooManyStreams(w http.ResponseWriter, r *http.Request, key string) bool {
	cred := credFrom(r.Context())
	if cred == nil || cred.MaxStreams <= 0 || s.Playing == nil {
		return false
	}
	if s.Playing.CountFor(cred.User.ID, key) < cred.MaxStreams {
		return false
	}
	// 429 rather than 503: nothing is wrong with the server, and the client
	// should not retry until something finishes. The sentence is what a
	// player shows.
	w.Header().Set("Retry-After", "30")
	http.Error(w, "this account is already watching as much as it is allowed to at once",
		http.StatusTooManyRequests)
	s.logger().Info("refused a stream over the account's limit",
		"user", cred.User.Username, "allowed", cred.MaxStreams)
	return true
}
