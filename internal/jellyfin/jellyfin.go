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

// route is one pattern this presenter answers.
type route struct {
	pattern string
	handler http.HandlerFunc
	open    bool // answered without a token
}

// routes is the whole surface, in one table.
//
// One table rather than a list of mux registrations plus a separate list of
// prefixes for whoever mounts this beside another API: the two drifted the
// first time a route was added, and the server 404'd a path its own tests
// said it answered. [Prefixes] is derived from here, so it cannot lag again.
func (s *Server) routes() []route {
	return []route{
		// Answered without a token: this is how an address becomes a server
		// in somebody's list, before anybody has a credential.
		{"GET /System/Info/Public", s.publicInfo, true},
		{"POST /Users/AuthenticateByName", s.authenticateByName, true},
		{"GET /QuickConnect/Enabled", s.quickConnectDisabled, true},

		{"GET /System/Info", s.systemInfo, false},
		{"GET /Users/Me", s.me, false},
		{"GET /Users/{user}", s.userByID, false},
		{"GET /Users/{user}/Views", s.views, false},
		{"GET /Users/{user}/Items", s.items, false},
		{"GET /Users/{user}/Items/Latest", s.latest, false},
		{"GET /Users/{user}/Items/Resume", s.resume, false},
		{"GET /Users/{user}/Items/{item}", s.item, false},
		{"GET /Items", s.items, false},
		{"GET /Items/{item}/Images/{type}", s.image, false},
		{"GET /Items/{item}/PlaybackInfo", s.playbackInfo, false},
		{"POST /Items/{item}/PlaybackInfo", s.playbackInfo, false},
		{"GET /Shows/{series}/Seasons", s.seasons, false},
		{"GET /Shows/{series}/Episodes", s.episodes, false},
		{"GET /Shows/NextUp", s.nextUp, false},
		{"GET /Artists", s.artists, false},
		{"GET /Artists/AlbumArtists", s.artists, false},

		{"GET /Videos/{item}/stream", s.stream, false},
		{"GET /Audio/{item}/stream", s.stream, false},
		// Clients ask for the file by name as often as by path —
		// `stream.mkv`, `stream.mp3` — and a pattern cannot carry a wildcard
		// inside a segment, so the name arrives whole and the handler reads
		// it.
		{"GET /Videos/{item}/{file}", s.streamNamed, false},
		{"GET /Audio/{item}/{file}", s.streamNamed, false},
		// Jellyfin spells a subtitle request `…/Subtitles/{index}/Stream.vtt`,
		// and sometimes puts a start offset between the two.
		{"GET /Videos/{item}/{source}/Subtitles/{index}/{file}", s.subtitleStream, false},
		{"GET /Videos/{item}/{source}/Subtitles/{index}/{position}/{file}", s.subtitleStream, false},

		// A client announces itself and reports where it is in a file. None
		// of it changes the library, and the last one is how progress gets
		// home.
		{"POST /Sessions/Capabilities/Full", s.noContent, false},
		{"POST /Sessions/Playing", s.playing, false},
		{"POST /Sessions/Playing/Progress", s.playing, false},
		{"POST /Sessions/Playing/Stopped", s.stopped, false},
		{"POST /Users/{user}/PlayedItems/{item}", s.markPlayed, false},
		{"DELETE /Users/{user}/PlayedItems/{item}", s.markUnplayed, false},

		// The spellings a current client uses, where the account is the token
		// rather than a path segment. A server that answers only the older
		// user-scoped forms 404s its Continue Watching row and every
		// watched-state write.
		{"GET /UserItems/Resume", s.resume, false},
		{"POST /UserItems/{item}/UserData", s.writeUserData, false},
		{"DELETE /UserItems/{item}/UserData", s.clearUserData, false},
		{"POST /UserPlayedItems/{item}", s.markPlayed, false},
		{"DELETE /UserPlayedItems/{item}", s.markUnplayed, false},

		// Answered so a client's shelves resolve rather than error. BMS has
		// no favourites, no trailers and no extras, and says so with an empty
		// answer instead of a 404 — a missing route reads as a broken server.
		{"POST /UserFavoriteItems/{item}", s.favourite, false},
		{"DELETE /UserFavoriteItems/{item}", s.favourite, false},
		{"GET /Items/{item}/LocalTrailers", s.emptyList, false},
		{"GET /Items/{item}/SpecialFeatures", s.emptyList, false},
		{"GET /Items/{item}/Similar", s.emptyItems, false},
		{"GET /DisplayPreferences/{id}", s.displayPreferences, false},
		{"POST /DisplayPreferences/{id}", s.noContent, false},
	}
}

// Handler builds the routes.
//
// Jellyfin's paths are case-insensitive in practice — its own clients send
// `/Users/{id}/Views` and `/users/{id}/views` interchangeably — so the mux is
// wrapped in a normaliser that fixes the case of the fixed segments. Go's mux
// matches paths exactly, and a client that guesses the other case would
// otherwise get a 404 from a server that has the route.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, r := range s.routes() {
		if r.open {
			mux.HandleFunc(r.pattern, r.handler)
			continue
		}
		mux.Handle(r.pattern, s.authenticate(r.handler))
	}
	return normalisePath(mux)
}

// Prefixes are the paths this presenter answers, for a server that mounts it
// beside another API at the same root.
//
// Derived from [Server.routes], so a route added without a prefix cannot
// 404 behind a mux that never sends it here. The two vocabularies do not
// overlap: Plex's are lower-case and start at `/library`, `/identity` and
// `/:`, and every path here is PascalCase.
func Prefixes() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range (&Server{}).routes() {
		_, path, found := strings.Cut(r.pattern, " ")
		if !found {
			continue
		}
		segments := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
		first := segments[0]
		if first == "" || seen[first] {
			continue
		}
		seen[first] = true
		// Both spellings: the bare path for the routes that are one segment
		// (`/Items`, `/Artists`), and the subtree for everything under it.
		out = append(out, "/"+first, "/"+first+"/")
	}
	return out
}

// normalisePath fixes the case of Jellyfin's fixed path segments.
//
// The API is PascalCase and its clients mostly send it that way, but not all
// of them do, and a server that 404s on `/users/x/views` is a server half the
// ecosystem cannot talk to. Only the known segment names are touched: an item
// id is a path value and is left exactly as it arrived, and `stream` is a
// file name rather than a segment name — renaming it to `Stream` turned a
// request for a file into a 404 from a server that had it.
func normalisePath(next http.Handler) http.Handler {
	known := map[string]string{}
	for _, segment := range []string{
		"System", "Info", "Public", "Users", "AuthenticateByName", "Views", "Items",
		"Latest", "Resume", "Images", "PlaybackInfo", "Shows", "Seasons", "Episodes",
		"NextUp", "Artists", "AlbumArtists", "Videos", "Audio", "Subtitles",
		"Sessions", "Capabilities", "Full", "Playing", "Progress", "Stopped", "Me",
		"QuickConnect", "Enabled", "UserData", "PlayedItems", "UserItems",
		"UserPlayedItems", "UserFavoriteItems", "DisplayPreferences",
		"LocalTrailers", "SpecialFeatures", "Similar",
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
	// And in the query string, for the URLs that end up in an <img> or get
	// handed to a media player, where nothing can set a header. Clients spell
	// the parameter `api_key` and `ApiKey` about equally often, and a query
	// key is case-sensitive: reading only one spelling is a 401 on the URL
	// the player was told to open, which looks to a viewer like a file that
	// will not play.
	for key, values := range r.URL.Query() {
		switch strings.ToLower(key) {
		case "api_key", "apikey", "x-emby-token", "x-mediabrowser-token":
			if len(values) > 0 && values[0] != "" {
				return values[0]
			}
		}
	}
	return ""
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
