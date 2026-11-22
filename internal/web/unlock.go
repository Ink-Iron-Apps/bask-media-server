package web

import (
	"errors"
	"net/http"
	"time"

	"github.com/inknironapps/bask-media-server/internal/store"
)

// Lifting a rating ceiling for an evening.
//
// A restricted account cannot see what it is restricted from — the handle it
// reads through filters it out of every listing — so there is nothing to
// click on and ask about. The way back in is a PIN, typed once, which lifts
// the ceiling for this browser session and for a few hours.
//
// Only the browser, because this is the only interface this server owns. A
// television speaking Jellyfin has its own idea of parental controls and no
// route here to type a PIN into; what it gets is a library with the
// restricted titles simply not in it, which is the honest version of the
// same rule.

// unlock takes a PIN and lifts this session's ceiling if it is the right one.
func (s *Server) unlock(w http.ResponseWriter, r *http.Request) {
	cred := credFrom(r.Context())
	if cred == nil {
		writeError(w, http.StatusUnauthorized, "sign in first")
		return
	}
	var body struct {
		PIN string `json:"pin"`
	}
	if !decode(w, r, &body) {
		return
	}

	ok, err := s.Store.CheckUserPIN(r.Context(), cred.User.ID, body.PIN)
	if err != nil {
		s.fail(w, "check the PIN", err)
		return
	}
	if !ok {
		// Deliberately the same answer whether there is no PIN or the wrong
		// one was given: the difference is worth nothing to somebody who
		// knows it and worth a guess to somebody who does not.
		writeError(w, http.StatusForbidden, "that is not the PIN")
		return
	}

	cookie, err := r.Cookie(cookieName)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "sign in first")
		return
	}
	until, err := s.Store.UnlockSession(r.Context(), cookie.Value)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnauthorized, "that session has expired")
		return
	}
	if err != nil {
		s.fail(w, "unlock", err)
		return
	}
	s.logger().Info("a rating ceiling was lifted with a PIN",
		"user", cred.User.Username, "until", until.Format(time.RFC3339))
	writeJSON(w, http.StatusOK, map[string]any{"unlocked": true, "until": until.Format(time.RFC3339)})
}

// lock puts the ceiling back before the unlock runs out.
func (s *Server) lock(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "sign in first")
		return
	}
	if err := s.Store.LockSession(r.Context(), cookie.Value); err != nil {
		s.fail(w, "lock", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
