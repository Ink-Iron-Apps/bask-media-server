// Package admin serves the server's own web interface.
//
// Three rules shape what is in here, all of them from the project's reasons
// for existing:
//
//   - No Node at serve time and no build step. The page is three files
//     compiled into the binary with go:embed and written in the browser's own
//     languages, so "one binary, one database file" stays true and a release
//     is still a single artefact.
//   - A browser is not a media client. Signing in mints a session with an
//     expiry, stored separately from the long-lived tokens a television holds,
//     so signing out of a web page logs nothing else out.
//   - Nothing here is a second source of truth. Every page reads the same
//     store the Plex surface reads, and every button runs the same pass the
//     command line runs.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/inknironapps/bask-media-server/internal/artwork"
	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/jobs"
	"github.com/inknironapps/bask-media-server/internal/nowplaying"
	"github.com/inknironapps/bask-media-server/internal/store"
	"github.com/inknironapps/bask-media-server/internal/subtitles"
	"github.com/inknironapps/bask-media-server/internal/webhook"
)

// cookieName holds the browser session. Scoped to /admin so it is not sent
// with a media request, which is a URL that can end up in a log or a player's
// crash report.
const cookieName = "bms_admin"

// csrfHeader is required on every request that changes something.
//
// A browser will not add a custom header to a cross-site form post, and
// sending one through fetch needs a preflight this server never approves —
// there is no CORS configuration here at all. Together with a SameSite=Strict
// cookie that is the whole cross-site request forgery defence, and it costs a
// header rather than a token mint and a hidden field.
const csrfHeader = "X-BMS-Admin"

// maxBody caps a request body. Every request here is a small JSON object;
// without a cap a POST is an invitation to send a gigabyte.
const maxBody = 64 << 10

// Server is the admin interface.
type Server struct {
	DB   *store.DB
	Jobs *jobs.Runner
	Log  *slog.Logger

	// Name is the fallback name for this server, from the command line. The
	// stored name wins when one has been set from the interface.
	Name      string
	MachineID string
	Version   string

	// Artwork is optional; with no cache the status page reports no artwork
	// rather than failing.
	Artwork *artwork.Cache

	// Transcoding reports how many files are being converted right now, for
	// the status page. Nil means this server cannot convert anything, which
	// is what no ffmpeg looks like.
	Transcoding func() int

	// Playing is the register of who is watching what. Nil means the page
	// shows no streams and nothing can be stopped from here.
	Playing *nowplaying.Registry

	// Fetcher searches a provider for subtitles a file does not have. Nil
	// means the page does not offer it.
	Fetcher *subtitles.Fetcher

	// Notifier delivers webhook events. Nil means destinations can still be
	// listed and edited but nothing is sent, which is what a server built
	// without one looks like — and the page says so rather than pretending
	// the test button works.
	Notifier *webhook.Notifier

	// BackupDir is where database copies are written and read back, and
	// KeepBackups how many are kept. They mirror what the job runner was
	// given, so the page lists the directory the passes actually write to.
	BackupDir   string
	KeepBackups int

	// Started is when the process came up, for the uptime on the status page.
	// Zero means the first request sets it.
	Started time.Time

	// SessionTTL overrides how long a sign-in lasts. Zero means the store's
	// default.
	SessionTTL time.Duration

	once  sync.Once
	gate  signInGate
	since time.Time
}

type sessionUserKey struct{}

// Handler builds the admin routes.
//
// Patterns carry the /admin prefix because the handler is mounted on the same
// mux as the Plex surface without stripping it: one listener, one port, and
// a client that asks for /identity and a browser that asks for /admin/ reach
// the same process.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// The shell and its assets are public, because a sign-in page has to be
	// reachable by someone who has not signed in. They contain no library
	// data: everything on the page arrives from the API below, which does not.
	mux.HandleFunc("GET /admin/{$}", s.page)
	mux.HandleFunc("GET /admin/app.js", s.asset("app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /admin/app.css", s.asset("app.css", "text/css; charset=utf-8"))

	mux.HandleFunc("POST /admin/api/session", s.signIn)
	mux.HandleFunc("DELETE /admin/api/session", s.signOut)

	authed := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.authenticate(h))
	}

	authed("GET /admin/api/session", s.whoami)
	authed("GET /admin/api/overview", s.overview)

	authed("GET /admin/api/playing", s.listPlaying)
	authed("POST /admin/api/playing/terminate", s.terminatePlaying)

	authed("GET /admin/api/sections", s.listSections)
	authed("POST /admin/api/sections", s.createSection)
	authed("PATCH /admin/api/sections/{id}", s.updateSection)
	authed("DELETE /admin/api/sections/{id}", s.deleteSection)
	authed("POST /admin/api/sections/{id}/scan", s.scanSection)

	authed("GET /admin/api/jobs", s.listJobs)
	authed("POST /admin/api/jobs", s.startJob)
	authed("POST /admin/api/jobs/{id}/cancel", s.cancelJob)

	authed("GET /admin/api/users", s.listUsers)
	authed("POST /admin/api/users", s.createUser)
	authed("PATCH /admin/api/users/{id}", s.updateUser)
	authed("DELETE /admin/api/users/{id}", s.deleteUser)
	authed("GET /admin/api/users/{id}/tokens", s.listTokens)
	authed("POST /admin/api/users/{id}/tokens", s.issueToken)
	authed("PATCH /admin/api/tokens/{id}", s.updateToken)
	authed("DELETE /admin/api/tokens/{id}", s.revokeToken)

	authed("GET /admin/api/collections", s.listCollections)
	authed("POST /admin/api/collections", s.createCollection)
	authed("GET /admin/api/collections/fields", s.vocabulary)
	authed("POST /admin/api/collections/preview", s.previewRules)
	authed("GET /admin/api/collections/{id}", s.readCollection)
	authed("PATCH /admin/api/collections/{id}", s.updateCollection)
	authed("DELETE /admin/api/collections/{id}", s.deleteCollection)
	authed("GET /admin/api/items", s.findItems)
	authed("GET /admin/api/items/{id}", s.readItem)
	authed("PATCH /admin/api/items/{id}", s.editItem)
	authed("POST /admin/api/items/{id}/revert", s.revertItem)
	authed("GET /admin/api/items/{id}/candidates", s.candidates)
	authed("POST /admin/api/items/{id}/match", s.rematchItem)
	authed("GET /admin/api/items/{id}/subtitles", s.findSubtitles)
	authed("POST /admin/api/items/{id}/subtitles", s.fetchSubtitle)

	authed("GET /admin/api/backups", s.listBackups)
	authed("POST /admin/api/backups", s.startBackup)
	authed("GET /admin/api/backups/{name}", s.downloadBackup)
	authed("DELETE /admin/api/backups/{name}", s.deleteBackup)

	authed("GET /admin/api/history", s.listHistory)
	authed("DELETE /admin/api/history", s.forgetHistory)

	authed("GET /admin/api/webhooks", s.listWebhooks)
	authed("POST /admin/api/webhooks", s.createWebhook)
	authed("PATCH /admin/api/webhooks/{id}", s.updateWebhook)
	authed("DELETE /admin/api/webhooks/{id}", s.deleteWebhook)
	authed("POST /admin/api/webhooks/{id}/test", s.testWebhook)

	authed("GET /admin/api/settings", s.readSettings)
	authed("PATCH /admin/api/settings", s.writeSettings)

	authed("GET /admin/api/browse", s.browse)

	return mux
}

// authenticate requires a live session belonging to an administrator, and a
// same-site header on anything that writes.
func (s *Server) authenticate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Header.Get(csrfHeader) == "" {
			writeError(w, http.StatusForbidden, "this request has to come from the admin page")
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
		if !user.Admin {
			// A session for a non-administrator exists only if an account was
			// demoted while signed in; the interface is administration, so it
			// is a refusal rather than a reduced page.
			writeError(w, http.StatusForbidden, "this account cannot administer the server")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), sessionUserKey{}, user)))
	}
}

func sessionUser(ctx context.Context) *domain.User {
	user, _ := ctx.Value(sessionUserKey{}).(*domain.User)
	return user
}

// signIn exchanges an API token for a browser session.
//
// The token is the one printed on first run, or one issued from this page, so
// there is no second credential to manage and no password store to get wrong.
// Only an administrator's token is accepted.
func (s *Server) signIn(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &body) {
		return
	}
	if wait, ok := s.gate.allow(time.Now()); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many attempts; wait a moment")
		return
	}

	user, err := s.DB.UserForToken(r.Context(), strings.TrimSpace(body.Token))
	if errors.Is(err, store.ErrNotFound) {
		s.gate.failed(time.Now())
		s.logger().Warn("rejected an admin sign-in", "remote", clientIP(r))
		writeError(w, http.StatusUnauthorized, "that token is not valid")
		return
	}
	if err != nil {
		s.fail(w, "resolve token", err)
		return
	}
	if !user.Admin {
		s.gate.failed(time.Now())
		writeError(w, http.StatusForbidden, "that token belongs to an account that cannot administer the server")
		return
	}

	// Zero token: the admin interface administers the server rather than
	// browsing a library, so it is never narrowed by a token's grants.
	secret, err := s.DB.CreateSession(r.Context(), user.ID, 0, s.SessionTTL)
	if err != nil {
		s.fail(w, "create session", err)
		return
	}
	s.gate.succeeded()
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    secret,
		Path:     "/admin",
		HttpOnly: true,
		Secure:   isTLS(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(s.sessionTTL().Seconds()),
	})
	// Expired sessions are cheapest to clear when somebody signs in, which is
	// the only moment the table grows.
	if _, err := s.DB.SweepSessions(r.Context()); err != nil {
		s.logger().Warn("could not sweep expired sessions", "error", err)
	}
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
	user := sessionUser(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"username": user.Username, "admin": user.Admin})
}

func (s *Server) clearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/admin",
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

// signInGate is a crude limiter on failed sign-ins.
//
// Not per address on purpose: a map keyed by client address is a map an
// attacker chooses the size of, and the thing being protected is one token on
// one small server. A short global pause after a run of failures makes a
// guessing attack take geological time and costs a legitimate administrator
// one retry if they fumble five in a row.
type signInGate struct {
	mu       sync.Mutex
	failures int
	until    time.Time
}

const (
	gateAfter = 5
	gateFor   = 30 * time.Second
)

func (g *signInGate) allow(now time.Time) (time.Duration, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if now.Before(g.until) {
		return g.until.Sub(now), false
	}
	return 0, true
}

func (g *signInGate) failed(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failures++
	if g.failures >= gateAfter {
		g.failures = 0
		g.until = now.Add(gateFor)
	}
}

func (g *signInGate) succeeded() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failures = 0
}

func isTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	// Behind a reverse proxy the connection to this process is plain even
	// though the browser's is not, and a cookie marked Secure over what the
	// browser thinks is HTTP is a cookie the browser discards.
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func clientIP(r *http.Request) string {
	host, _, found := strings.Cut(r.RemoteAddr, ":")
	if !found {
		return r.RemoteAddr
	}
	return host
}

// uptime is how long the process has been up.
func (s *Server) uptime() time.Duration {
	s.once.Do(func() {
		s.since = s.Started
		if s.since.IsZero() {
			s.since = time.Now()
		}
	})
	return time.Since(s.since)
}

func (s *Server) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	s.logger().Error("admin request failed", "what", what, "error", err)
	writeError(w, http.StatusInternalServerError, "something went wrong on the server")
}

// decode reads a small JSON body, answering the client itself on failure.
func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		if errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "that request needs a body")
			return false
		}
		writeError(w, http.StatusBadRequest, "could not read that request: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// An admin page is current state, never a cached copy.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// memory is what the budget asks every long-running process to be able to say
// about itself: see docs/RESOURCE-BUDGET.md.
func memory() (heapMB uint64, goroutines int) {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapAlloc >> 20, runtime.NumGoroutine()
}
