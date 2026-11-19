// Package web is the interface for watching, as opposed to administering.
//
// It exists because a server that can only be used through an app is a server
// you cannot check from someone else's laptop. It is the same library the
// Plex-compatible surface serves, read through the same store, with a player
// for what the browser in front of you can decode — and a plain sentence when
// it cannot, because BMS does not transcode and pretending otherwise would
// waste somebody's evening.
//
// Built like the admin interface and for the same reasons: three files
// compiled into the binary, no framework, no build step, and a session that
// expires rather than a long-lived token in a browser.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/inknironapps/bask-media-server/internal/artwork"
	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// cookieName holds the viewer's session, scoped to /web so it is not sent
// with a request a media client makes.
const cookieName = "bms_web"

// csrfHeader is required on the requests that change something — which here
// is playback progress. Same reasoning as the admin interface: a cross-site
// form can send a cookie but cannot add a header.
const csrfHeader = "X-BMS-Web"

const maxBody = 8 << 10

// Server is the viewing interface.
type Server struct {
	DB  *store.DB
	Log *slog.Logger

	// Name is the fallback name shown in the corner; the stored one wins.
	Name string

	// Artwork is optional. Without it the grid draws placeholders, which is a
	// library you can still browse and play.
	Artwork *artwork.Cache

	// Prober measures a file before playing it, so the server can tell
	// whether this client needs anything converted. Nil means everything is
	// sent as it is, which is what a server with no ffprobe does.
	Prober Prober

	// Transcoder converts what a client cannot play. Nil means a file this
	// browser cannot decode is reported as such rather than converted.
	Transcoder Transcoder

	// SessionTTL overrides how long a sign-in lasts. Zero means the store's
	// default.
	SessionTTL time.Duration
}

type userKey struct{}

// Handler builds the viewing routes, which carry their /web prefix because
// the handler is mounted beside the Plex surface without stripping it.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /web/{$}", s.page)
	mux.HandleFunc("GET /web/app.js", s.asset("app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /web/app.css", s.asset("app.css", "text/css; charset=utf-8"))

	mux.HandleFunc("POST /web/api/session", s.signIn)
	mux.HandleFunc("DELETE /web/api/session", s.signOut)

	watcher := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.authenticate(h))
	}

	watcher("GET /web/api/session", s.whoami)
	watcher("GET /web/api/home", s.home)
	watcher("GET /web/api/sections/{id}", s.section)
	watcher("GET /web/api/items/{id}", s.item)
	watcher("GET /web/api/search", s.search)
	watcher("POST /web/api/progress", s.saveProgress)
	watcher("POST /web/api/play", s.play)
	watcher("POST /web/api/play/{session}/stop", s.stopPlayback)

	// Images and media are ordinary GETs so a browser can put them in an
	// <img> or a <video>, where no header can be set and the cookie is all
	// there is.
	watcher("GET /web/image/{id}", s.image)
	watcher("GET /web/stream/{id}", s.stream)
	watcher("GET /web/hls/{session}/{name}", s.hls)

	// The one vendored file, served like the rest of the page.
	mux.HandleFunc("GET /web/vendor/{name}", s.vendor)

	return mux
}

// authenticate requires a session. Any account, not only an administrator:
// this is the interface for watching, and a household's accounts are exactly
// the people who should reach it.
func (s *Server) authenticate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Header.Get(csrfHeader) == "" {
			writeError(w, http.StatusForbidden, "this request has to come from the page")
			return
		}
		cookie, err := r.Cookie(cookieName)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "sign in first")
			return
		}
		user, err := s.DB.SessionUser(r.Context(), cookie.Value)
		if errors.Is(err, store.ErrNotFound) {
			s.clearCookie(w, r)
			writeError(w, http.StatusUnauthorized, "that session has expired")
			return
		}
		if err != nil {
			s.fail(w, "resolve session", err)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey{}, user)))
	}
}

func userFrom(ctx context.Context) *domain.User {
	user, _ := ctx.Value(userKey{}).(*domain.User)
	return user
}

func (s *Server) signIn(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &body) {
		return
	}
	user, err := s.DB.UserForToken(r.Context(), strings.TrimSpace(body.Token))
	if errors.Is(err, store.ErrNotFound) {
		s.logger().Warn("rejected a sign-in to the web interface", "remote", clientIP(r))
		writeError(w, http.StatusUnauthorized, "that token is not valid")
		return
	}
	if err != nil {
		s.fail(w, "resolve token", err)
		return
	}

	secret, err := s.DB.CreateSession(r.Context(), user.ID, s.sessionTTL())
	if err != nil {
		s.fail(w, "create session", err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    secret,
		Path:     "/web",
		HttpOnly: true,
		Secure:   isTLS(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(s.sessionTTL().Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"username": user.Username, "admin": user.Admin})
}

func (s *Server) signOut(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(cookieName); err == nil {
		if err := s.DB.DeleteSession(r.Context(), cookie.Value); err != nil {
			s.fail(w, "delete session", err)
			return
		}
	}
	s.clearCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) whoami(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"username": user.Username, "admin": user.Admin})
}

func (s *Server) clearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/web",
		HttpOnly: true,
		Secure:   isTLS(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

func (s *Server) sessionTTL() time.Duration {
	if s.SessionTTL > 0 {
		return s.SessionTTL
	}
	return store.SessionTTL
}

func isTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func clientIP(r *http.Request) string {
	host, _, found := strings.Cut(r.RemoteAddr, ":")
	if !found {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	s.logger().Error("web request failed", "what", what, "error", err)
	writeError(w, http.StatusInternalServerError, "something went wrong on the server")
}

func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		if errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "that request needs a body")
			return false
		}
		writeError(w, http.StatusBadRequest, "could not read that request")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func idFrom(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "that is not an id")
		return 0, false
	}
	return id, true
}
