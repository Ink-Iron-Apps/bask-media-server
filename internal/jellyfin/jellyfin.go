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
	"github.com/inknironapps/bask-media-server/internal/nowplaying"
	"github.com/inknironapps/bask-media-server/internal/store"
	"github.com/inknironapps/bask-media-server/internal/subtitles"
)

// Server answers the Jellyfin API over BMS's library.
type Server struct {
	// Store is the library, unrestricted. Handlers do not query it directly:
	// they call s.db(ctx), which narrows it to what the request's credential
	// may see. The exceptions are the reads that decide the restriction —
	// resolving a token, reading a server-wide setting — and they say so by
	// naming this field.
	Store *store.DB
	Log   *slog.Logger
	Name  string

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
	Transcoder Transcoder

	// Prober measures a file before playing it, so a device profile can be
	// answered with more than a guess. Nil means direct play: see
	// [Server.negotiate].
	Prober Prober

	// Playing is the register of who is watching what. Nil means this
	// presenter reports nothing, which is what it did before there was a
	// page to report to — progress is still written either way.
	Playing *nowplaying.Registry

	// Fetcher searches a provider for subtitles a file does not have. Nil
	// means this server does not offer that, which is also what no key looks
	// like.
	Fetcher *subtitles.Fetcher
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
		// What a library can be narrowed by, which is what fills a client's
		// filter sheet. `/Items/Filters` is the one request that answers for
		// every facet at once; `/Genres` is what a client reaches for when
		// that endpoint is not there.
		{"GET /Items/Filters", s.filters, false},
		{"GET /Items/Filters2", s.filters, false},
		{"GET /Genres", s.genres, false},
		{"GET /Users/{user}/Items/Filters", s.filters, false},
		{"GET /Items/{item}/Images/{type}", s.image, false},
		{"GET /Items/{item}/PlaybackInfo", s.playbackInfo, false},
		{"POST /Items/{item}/PlaybackInfo", s.playbackInfo, false},
		// Extras, at both spellings Jellyfin's own clients use. These two
		// answered an empty list until there was something to put in it.
		{"GET /Users/{user}/Items/{item}/SpecialFeatures", s.specialFeatures, false},
		{"GET /Items/{item}/SpecialFeatures", s.specialFeatures, false},
		{"GET /Users/{user}/Items/{item}/LocalTrailers", s.localTrailers, false},
		{"GET /Items/{item}/LocalTrailers", s.localTrailers, false},
		// Subtitles from a provider: Jellyfin calls it a remote search.
		{"GET /Items/{item}/RemoteSearch/Subtitles/{language}", s.searchSubtitles, false},
		{"POST /Items/{item}/RemoteSearch/Subtitles/{subtitle}", s.fetchSubtitle, false},
		{"GET /Shows/{series}/Seasons", s.seasons, false},
		{"GET /Shows/{series}/Episodes", s.episodes, false},
		{"GET /Shows/NextUp", s.nextUp, false},
		// Playlists. A playlist is one account's, so none of these take a
		// user in the path: the credential already said whose it is.
		{"GET /Playlists", s.playlists, false},
		{"POST /Playlists", s.createPlaylist, false},
		{"GET /Playlists/{playlist}/Items", s.playlistItems, false},
		{"POST /Playlists/{playlist}/Items", s.addToPlaylist, false},
		{"DELETE /Playlists/{playlist}/Items", s.removeFromPlaylist, false},
		{"POST /Playlists/{playlist}/Items/{entry}/Move/{index}", s.movePlaylistItem, false},

		{"GET /Artists", s.artists, false},
		{"GET /Artists/AlbumArtists", s.artists, false},

		{"GET /Videos/{item}/stream", s.stream, false},
		{"GET /Audio/{item}/stream", s.stream, false},
		// Clients ask for the file by name as often as by path —
		// `stream.mkv`, `stream.mp3` — and a pattern cannot carry a wildcard
		// inside a segment, so the name arrives whole and the handler reads
		// it.
		// The converted stream: the playlist a client is pointed at, the one
		// it names next, and the segments under it.
		{"GET /Videos/{item}/master.m3u8", s.transcodeMaster, false},
		{"GET /Videos/{item}/main.m3u8", s.transcodeVariant, false},
		{"GET /Videos/{item}/hls/{session}/{file}", s.transcodeSegment, false},
		{"DELETE /Videos/ActiveEncodings", s.stopEncoding, false},

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

		// The star. Jellyfin clients use both spellings of the path and tell
		// the two apart by method.
		{"POST /UserFavoriteItems/{item}", s.addFavourite, false},
		{"DELETE /UserFavoriteItems/{item}", s.dropFavourite, false},
		{"POST /Users/{user}/FavoriteItems/{item}", s.addFavourite, false},
		{"DELETE /Users/{user}/FavoriteItems/{item}", s.dropFavourite, false},

		// Answered so a client's shelves resolve rather than error. A
		// missing route reads as a broken server, and there is nothing here
		// that looks like "similar to this".
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

// sharedWithPlex are the first segments the Plex surface also answers on.
//
// The two APIs are told apart by case — Plex's paths are lower case and
// these are PascalCase — and that works until a word appears in both. The
// lower-case courtesy spelling of such a word is the other API's real path,
// and registering it here takes the route away: `/playlists` is Plex's, and
// mirroring `/Playlists` onto it answered a Plex client with a 401 from this
// surface. A Jellyfin client that mis-cases one of these gets a 404, which
// is the smaller harm by a long way.
var sharedWithPlex = map[string]bool{
	"playlists": true,
}

// Prefixes are the paths this presenter answers, for a server that mounts it
// beside another API at the same root.
//
// Derived from [Server.routes], so a route added without a prefix cannot
// 404 behind a mux that never sends it here. Includes the lower-case
// spellings, because clients are not consistent about the case of the fixed
// segments and the mux matches a path exactly — except where that spelling
// belongs to the other API, which is [sharedWithPlex].
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

		lower := strings.ToLower(first)
		if lower == first || sharedWithPlex[lower] {
			continue
		}
		out = append(out, "/"+lower, "/"+lower+"/")
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
//
// A segment the Plex surface also uses is left out of this list, however
// often a Jellyfin client mis-cases it. The two APIs are told apart by case —
// Plex's paths are lower case and these are PascalCase — so case-folding a
// shared word here takes the other surface's route away: `/playlists` is
// Plex's, and folding it to `/Playlists` answered a Plex client with a
// Jellyfin 401. Being lenient about one spelling is not worth breaking the
// other API.
func normalisePath(next http.Handler) http.Handler {
	known := map[string]string{}
	for _, segment := range []string{
		"System", "Info", "Public", "Users", "AuthenticateByName", "Views", "Items",
		"Latest", "Resume", "Images", "PlaybackInfo", "Shows", "Seasons", "Episodes",
		"NextUp", "Artists", "AlbumArtists", "Videos", "Audio", "Subtitles",
		"Sessions", "Capabilities", "Full", "Playing", "Progress", "Stopped", "Me",
		"QuickConnect", "Enabled", "UserData", "PlayedItems", "UserItems",
		"UserPlayedItems", "UserFavoriteItems", "DisplayPreferences", "ActiveEncodings",
		"RemoteSearch",
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

type credKey struct{}

// authenticate resolves the access token a client was given.
//
// Jellyfin carries it in three places and clients use all of them: an
// `X-Emby-Token` header, the `Token="…"` field of the `Authorization` header,
// and — for the URLs that end up in an <img> or a <video>, where no header
// can be set — an `api_key` query parameter.
func (s *Server) authenticate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := tokenFrom(r)
		cred, err := s.Store.Credential(r.Context(), token)
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err != nil {
			s.logger().Error("token lookup failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// What the client calls itself, kept beside the label a human typed so
		// the token list reads as a list of devices. Written here because this
		// is the only place that sees both the header and the token row.
		s.Store.NoteTokenDevice(r.Context(), cred.TokenID, deviceFrom(r))
		next(w, r.WithContext(context.WithValue(r.Context(), credKey{}, cred)))
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
	return fieldInAuthorization(value, "Token")
}

// fieldInAuthorization reads one named field out of that scheme.
func fieldInAuthorization(value, want string) string {
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
		if !found || !strings.EqualFold(strings.TrimSpace(name), want) {
			continue
		}
		return strings.Trim(strings.TrimSpace(raw), `"`)
	}
	return ""
}

func userFrom(ctx context.Context) *domain.User {
	cred := credFrom(ctx)
	if cred == nil {
		return nil
	}
	return &cred.User
}

// credFrom is the credential this request authenticated with, nil on the
// handful of routes that answer without one.
func credFrom(ctx context.Context) *domain.Credential {
	cred, _ := ctx.Value(credKey{}).(*domain.Credential)
	return cred
}

// db is the library as this request's credential may see it.
//
// Every handler reads through this rather than through s.Store, so hiding a
// library is one decision taken once at the edge instead of a condition each
// handler has to remember. A route that answers without a credential gets the
// unrestricted handle, which is correct: those routes answer questions about
// the server rather than about its contents.
func (s *Server) db(ctx context.Context) *store.DB {
	return s.Store.Scoped(credFrom(ctx))
}

// deviceFrom reads how a client described itself: the Device field of
// Jellyfin's own Authorization scheme, falling back to the device id when it
// gave no name.
//
// Diagnostics only. A client writes these fields itself, so they are worth
// showing and worth nothing as a gate — see [store.DB.NoteTokenDevice].
func deviceFrom(r *http.Request) string {
	for _, want := range []string{"Device", "DeviceId"} {
		for _, header := range []string{"Authorization", "X-Emby-Authorization"} {
			if value := fieldInAuthorization(r.Header.Get(header), want); value != "" {
				return value
			}
		}
	}
	return ""
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
	stored, err := s.Store.ServerValue(ctx, store.KeyFriendlyName)
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

// space says which table an id came from.
//
// Libraries, items, playlists and collections are four tables with four id
// sequences, and Jellyfin addresses all of them as plain ids: without a
// marker, library 1 and film 1 are the same string and whichever the server
// looked up first would shadow the other. The marker is a leading hex digit,
// so what comes out is still a GUID to a client that parses one.
//
// The letters are picked from the end of the alphabet so they cannot collide
// with the leading zeros an item id has.
type space int

const (
	spaceItem space = iota
	spaceLibrary
	spacePlaylist
	spaceCollection
)

func sectionID(id int64) string    { return fmt.Sprintf("f%031x", id) }
func playlistID(id int64) string   { return fmt.Sprintf("e%031x", id) }
func collectionID(id int64) string { return fmt.Sprintf("d%031x", id) }

// parseID reads an id back, saying which table it belongs to. Accepts the
// GUID spelling with dashes, which is how some clients normalise what they
// were given.
func parseID(value string) (id int64, from space, err error) {
	cleaned := strings.ReplaceAll(strings.TrimSpace(value), "-", "")
	if cleaned == "" {
		return 0, spaceItem, errors.New("no id")
	}
	switch strings.ToLower(cleaned[:1]) {
	case "f":
		from, cleaned = spaceLibrary, cleaned[1:]
	case "e":
		from, cleaned = spacePlaylist, cleaned[1:]
	case "d":
		from, cleaned = spaceCollection, cleaned[1:]
	}
	trimmed := strings.TrimLeft(cleaned, "0")
	if trimmed == "" {
		return 0, from, fmt.Errorf("not an id: %q", value)
	}
	id, err = strconv.ParseInt(trimmed, 16, 64)
	if err != nil || id <= 0 {
		return 0, from, fmt.Errorf("not an id: %q", value)
	}
	return id, from, nil
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
