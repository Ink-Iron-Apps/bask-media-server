// Package jellyfin serves the library in the shape Jellyfin clients expect.
//
// A second presenter over the same domain, beside internal/plexapi, and the
// reason the domain never learned either vocabulary: a MediaContainer and a
// BaseItemDto describe the same film, and neither word reaches storage or
// scanning.
//
// Why a second one at all. A Plex-protocol client can only find a server
// through plex.tv, which a server that belongs to its owner is never going
// to be on. Jellyfin's clients are told an address and a user, which is the
// shape a self-hosted server actually has — so this presenter is what makes
// BMS reachable from Bask, from Findroid, from a browser, from anything that
// speaks it, with no account service in the middle.
//
// What is deliberately not here: Jellyfin's server-side library management,
// its plugin surface, its user administration, Live TV, and its own
// transcoding vocabulary. This is the read path a client needs to browse and
// play, which is what BMS has to offer.
package jellyfin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/artwork"
	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
	"github.com/inknironapps/bask-media-server/internal/subtitles"
	"github.com/inknironapps/bask-media-server/internal/transcode"
)

// Server answers the Jellyfin API over BMS's library.
type Server struct {
	DB   *store.DB
	Log  *slog.Logger
	Name string

	// MachineID is this server's identity, the same one the Plex surface
	// reports: a client that has both configured is talking to one server and
	// should be told so.
	MachineID string
	Version   string

	// Artwork serves posters; nil means a library with no pictures.
	Artwork *artwork.Cache

	// Subtitles converts a track to the format a client asks for. Nil means
	// sidecars are served as they are and embedded ones are refused.
	Subtitles *subtitles.Converter

	// Transcoder is used when a client says it cannot play a file. Nil means
	// this server only direct plays, which is what it did before there was
	// one.
	Transcoder *transcode.Manager
}

// Handler builds the routes.
//
// Jellyfin's paths are case-insensitive in practice — its own clients send
// `/Users/{id}/Views` and `/users/{id}/views` interchangeably — so every
// pattern is registered as written and the mux is wrapped in a normaliser
// that fixes the case of the fixed segments. Go's mux matches paths exactly,
// and a client that guesses the other case would otherwise get a 404 from a
// server that has the route.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Unauthenticated: this is how a client decides whether an address is a
	// server at all, before anybody has typed a password.
	mux.HandleFunc("GET /System/Info/Public", s.publicInfo)
	mux.HandleFunc("POST /Users/AuthenticateByName", s.authenticateByName)
	mux.HandleFunc("GET /QuickConnect/Enabled", s.quickConnectDisabled)

	authed := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.authenticate(h))
	}

	authed("GET /System/Info", s.systemInfo)
	authed("GET /Users/Me", s.me)
	authed("GET /Users/{user}", s.userByID)
	authed("GET /Users/{user}/Views", s.views)
	authed("GET /Users/{user}/Items", s.items)
	authed("GET /Users/{user}/Items/Latest", s.latest)
	authed("GET /Users/{user}/Items/Resume", s.resume)
	authed("GET /Users/{user}/Items/{item}", s.item)
	authed("GET /Items", s.items)
	authed("GET /Items/{item}/Images/{type}", s.image)
	authed("GET /Items/{item}/PlaybackInfo", s.playbackInfo)
	authed("POST /Items/{item}/PlaybackInfo", s.playbackInfo)
	authed("GET /Shows/{series}/Seasons", s.seasons)
	authed("GET /Shows/{series}/Episodes", s.episodes)
	authed("GET /Shows/NextUp", s.nextUp)
	authed("GET /Artists", s.artists)
	authed("GET /Artists/AlbumArtists", s.artists)
	authed("GET /Videos/{item}/stream", s.stream)
	authed("GET /Audio/{item}/stream", s.stream)
	// Clients ask for the file by name as often as by path — `stream.mkv`,
	// `stream.mp3` — and a pattern cannot carry a wildcard inside a segment,
	// so the name arrives whole and the handler reads it.
	authed("GET /Videos/{item}/{file}", s.streamNamed)
	authed("GET /Audio/{item}/{file}", s.streamNamed)
	// Jellyfin spells a subtitle request `…/Subtitles/{index}/Stream.vtt`, and
	// sometimes puts a start offset between the two; the file name carries
	// the format, so it arrives whole — a pattern cannot split a segment.
	authed("GET /Videos/{item}/{source}/Subtitles/{index}/{file}", s.subtitleStream)
	authed("GET /Videos/{item}/{source}/Subtitles/{index}/{position}/{file}", s.subtitleStream)

	// A client announces itself and reports where it is in a file. None of it
	// changes the library, and the last one is how progress gets home.
	authed("POST /Sessions/Capabilities/Full", s.noContent)
	authed("POST /Sessions/Playing", s.playing)
	authed("POST /Sessions/Playing/Progress", s.playing)
	authed("POST /Sessions/Playing/Stopped", s.stopped)
	authed("DELETE /Items/{item}/UserData", s.noContent)
	authed("POST /Users/{user}/PlayedItems/{item}", s.markPlayed)
	authed("DELETE /Users/{user}/PlayedItems/{item}", s.markUnplayed)

	// An image is fetched by an <img> tag that cannot set a header, so the
	// token rides in the query string there — see authenticate.
	return normalisePath(mux)
}

// normalisePath fixes the case of Jellyfin's fixed path segments.
//
// The API is PascalCase and its clients mostly send it that way, but not all
// of them do, and a server that 404s on `/users/x/views` is a server that
// half the ecosystem cannot talk to. Only the known segment names are
// touched: an item id is a path value and is left exactly as it arrived.
func normalisePath(next http.Handler) http.Handler {
	known := map[string]string{}
	for _, segment := range []string{
		"System", "Info", "Public", "Users", "AuthenticateByName", "Views", "Items",
		"Latest", "Resume", "Images", "PlaybackInfo", "Shows", "Seasons", "Episodes",
		"NextUp", "Artists", "AlbumArtists", "Videos", "Audio", "Subtitles",
		"Sessions", "Capabilities", "Full", "Playing", "Progress", "Stopped", "Me",
		// "Stream" is deliberately absent: it is a file name here, not a
		// fixed segment, and `/Videos/{id}/stream` is the spelling the route
		// matches. Normalising it to "Stream" turned a stream request into a
		// 404 on a server that had the file.
		"QuickConnect", "Enabled", "UserData", "PlayedItems",
	} {
		known[strings.ToLower(segment)] = segment
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		changed := false
		for i, part := range parts {
			if fixed, found := known[strings.ToLower(part)]; found && fixed != part {
				parts[i] = fixed
				changed = true
			}
		}
		if changed {
			r = r.Clone(r.Context())
			r.URL.Path = strings.Join(parts, "/")
		}
		next.ServeHTTP(w, r)
	})
}

type userKey struct{}

// authenticate resolves the access token a client was given.
//
// Jellyfin carries it in three places and clients use all of them: an
// `X-Emby-Token` header, the `Token="…"` field of the `Authorization` header,
// and — for the URLs that end up in an <img> or a <video>, where no header
// can be set — an `api_key` query parameter.
func (s *Server) authenticate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := tokenFrom(r)
		user, err := s.DB.UserForToken(r.Context(), token)
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err != nil {
			s.logger().Error("token lookup failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey{}, user)))
	}
}

func tokenFrom(r *http.Request) string {
	for _, header := range []string{"X-Emby-Token", "X-MediaBrowser-Token"} {
		if value := r.Header.Get(header); value != "" {
			return value
		}
	}
	for _, header := range []string{"Authorization", "X-Emby-Authorization"} {
		if token := tokenInAuthorization(r.Header.Get(header)); token != "" {
			return token
		}
	}
	return r.URL.Query().Get("api_key")
}

// tokenInAuthorization reads the Token field out of Jellyfin's own
// Authorization scheme: `MediaBrowser Client="Bask", Device="…", Token="…"`.
func tokenInAuthorization(value string) string {
	if value == "" {
		return ""
	}
	for _, field := range strings.Split(value, ",") {
		field = strings.TrimSpace(field)
		// The scheme name is glued to the first field: `MediaBrowser Token=…`.
		if space := strings.LastIndex(field, " "); space >= 0 && strings.Contains(field[:space], "=") == false {
			field = strings.TrimSpace(field[space+1:])
		}
		name, raw, found := strings.Cut(field, "=")
		if !found || !strings.EqualFold(strings.TrimSpace(name), "Token") {
			continue
		}
		return strings.Trim(strings.TrimSpace(raw), `"`)
	}
	return ""
}

func userFrom(ctx context.Context) *domain.User {
	user, _ := ctx.Value(userKey{}).(*domain.User)
	return user
}

func (s *Server) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// friendlyName is what clients show for this server, read per request so a
// rename in the admin interface reaches the next client that asks.
func (s *Server) friendlyName(ctx context.Context) string {
	stored, err := s.DB.ServerValue(ctx, store.KeyFriendlyName)
	if err == nil && stored != "" {
		return stored
	}
	if s.Name != "" {
		return s.Name
	}
	return "Bask Media Server"
}

// serverID is this server's identity in the shape Jellyfin uses: its clients
// treat the id as an opaque string but several store it in a GUID column, so
// it is the machine identifier cut to the 32 hex characters that fits one.
func (s *Server) serverID() string {
	id := strings.ReplaceAll(s.MachineID, "-", "")
	if len(id) >= 32 {
		return id[:32]
	}
	return fmt.Sprintf("%032s", id)
}

func (s *Server) version() string {
	if s.Version != "" {
		return s.Version
	}
	return "10.10.0"
}

/* ---------- ids ---------- */

// itemID renders a row id as the 32-hex string a Jellyfin client expects.
//
// Jellyfin's ids are GUIDs, and while the API passes them as strings, enough
// clients parse them as GUIDs that handing out "12" would break them. The
// mapping is the row id in hex, so it is reversible without a lookup table
// and two servers never collide with each other's library.
func itemID(id int64) string { return fmt.Sprintf("%032x", id) }

// sectionID is the same for a library, in a space of its own.
//
// Libraries and the things in them are separate tables with separate id
// sequences, and Jellyfin addresses both as plain ids: without a marker,
// library 1 and film 1 are the same string, and whichever the server looked
// up first would shadow the other. The leading `f` is that marker — still a
// hex digit, still a GUID to a client that parses one.
func sectionID(id int64) string { return fmt.Sprintf("f%031x", id) }

// parseID reads an id back, saying which kind it was. Accepts the GUID
// spelling with dashes, which is how some clients normalise what they were
// given.
func parseID(value string) (id int64, library bool, err error) {
	cleaned := strings.ReplaceAll(strings.TrimSpace(value), "-", "")
	if cleaned == "" {
		return 0, false, errors.New("no id")
	}
	if strings.HasPrefix(strings.ToLower(cleaned), "f") {
		library, cleaned = true, cleaned[1:]
	}
	trimmed := strings.TrimLeft(cleaned, "0")
	if trimmed == "" {
		return 0, library, fmt.Errorf("not an id: %q", value)
	}
	id, err = strconv.ParseInt(trimmed, 16, 64)
	if err != nil || id <= 0 {
		return 0, library, fmt.Errorf("not an id: %q", value)
	}
	return id, library, nil
}

/* ---------- replies ---------- */

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) noContent(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	s.logger().Error("jellyfin request failed", "what", what, "error", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func intQuery(r *http.Request, name string) int {
	if v := r.URL.Query().Get(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

func boolQuery(r *http.Request, name string) bool {
	value := strings.ToLower(strings.TrimSpace(r.URL.Query().Get(name)))
	return value == "true" || value == "1"
}

// Prefixes are the paths this presenter answers.
//
// Two APIs share one root here — a client looks for the Plex one at `/` and
// the Jellyfin one at paths of its own — so the server that mounts both needs
// to know which paths belong to which. The two vocabularies do not overlap:
// Plex's are lower-case and start at `/library`, `/identity` and `/:`, and
// every path below is PascalCase.
func Prefixes() []string {
	return []string{
		"/System/", "/Users/", "/Items", "/Items/", "/Videos/", "/Audio/",
		"/Shows/", "/Artists", "/Artists/", "/Sessions/", "/QuickConnect/",
	}
}
