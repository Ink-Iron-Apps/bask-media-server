package plexapi

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/inknironapps/bask-media-server/internal/artwork"
	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/serve"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// Server serves the Plex-compatible surface.
type Server struct {
	DB   *store.DB
	Log  *slog.Logger
	Name string // friendlyName a client shows for this server
	// MachineID is this server's permanent identity. A client that sees it
	// change treats BMS as a different server and forgets every setting,
	// every playback position and every download it had, so it is generated
	// once and stored.
	MachineID string
	Version   string

	// Artwork is optional: with no cache the library serves no images, which
	// is a poster-less client rather than a broken one.
	Artwork *artwork.Cache
}

type userKey struct{}

// Handler builds the routes. Go's pattern-matching mux covers everything here,
// so there is no router dependency and no reflection in the hot path.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Unauthenticated: a client asks who this is before it has a token, and
	// Bask's "connect to a server" flow needs exactly this much.
	mux.HandleFunc("GET /identity", s.identity)

	authed := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.authenticate(h))
	}

	authed("GET /library/sections", s.sections)
	authed("GET /library/sections/{id}/all", s.sectionAll)
	authed("GET /library/metadata/{key}", s.metadata)
	authed("GET /library/metadata/{key}/children", s.children)
	authed("GET /library/parts/{id}", s.streamPart)
	authed("GET /library/metadata/{key}/thumb", s.thumb)
	authed("GET /library/metadata/{key}/art", s.art)
	// Plex clients ask for a resized image through one endpoint, passing the
	// image they want as a url parameter. Serving it ourselves is what keeps a
	// poster wall from pulling full-size images to a phone.
	authed("GET /photo/:/transcode", s.photoTranscode)
	authed("GET /hubs/search", s.search)
	authed("GET /search", s.search)
	authed("GET /hubs", s.hubs)
	authed("GET /:/timeline", s.timeline)
	authed("GET /:/scrobble", s.scrobble)
	authed("GET /:/unscrobble", s.unscrobble)

	return mux
}

// authenticate resolves X-Plex-Token, from the header or the query string,
// since clients use both and a stream URL can only carry the query form.
func (s *Server) authenticate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Plex-Token")
		if token == "" {
			token = r.URL.Query().Get("X-Plex-Token")
		}
		user, err := s.DB.UserForToken(r.Context(), token)
		if errors.Is(err, store.ErrNotFound) {
			// 401 with the header Plex clients expect, so a client prompts for
			// a sign-in rather than reporting a broken server.
			w.Header().Set("X-Plex-Protocol", "1.0")
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

// friendlyName is what clients show for this server.
//
// Read per request rather than held, because the admin interface can rename
// the server while it is running and a client that asked a second ago should
// see the new name without the server being restarted. It is one indexed point
// read against a table with a handful of rows.
func (s *Server) friendlyName(ctx context.Context) string {
	stored, err := s.DB.ServerValue(ctx, store.KeyFriendlyName)
	if err != nil {
		s.logger().Warn("could not read the server name", "error", err)
		return s.Name
	}
	if stored != "" {
		return stored
	}
	return s.Name
}

func (s *Server) identity(w http.ResponseWriter, r *http.Request) {
	write(w, r, container{
		Size:              0,
		MachineIdentifier: s.MachineID,
		Version:           s.Version,
		FriendlyName:      s.friendlyName(r.Context()),
		Claimed:           false,
	})
}

func (s *Server) sections(w http.ResponseWriter, r *http.Request) {
	sections, err := s.DB.Sections(r.Context())
	if err != nil {
		s.fail(w, "list sections", err)
		return
	}

	out := container{
		Identifier:     "com.plexapp.plugins.library",
		MediaTagPrefix: "/system/bundle/media/flags/",
		AllowSync:      true,
		Title1:         s.friendlyName(r.Context()),
	}
	for _, sec := range sections {
		e := entry{
			Elem:      "Directory",
			Key:       strconv.FormatInt(sec.ID, 10),
			Type:      sectionType(sec.Kind),
			Title:     sec.Title,
			Agent:     "tv.plex.agents.none",
			Scanner:   "BMS Scanner",
			Language:  "en-US",
			UUID:      s.MachineID + "-" + strconv.FormatInt(sec.ID, 10),
			UpdatedAt: millisOf(sec.UpdatedAt),
		}
		for _, root := range sec.Roots {
			e.Location = append(e.Location, location{ID: sec.ID, Path: root})
		}
		out.Directory = append(out.Directory, e)
	}
	out.Size = len(out.Directory)
	write(w, r, out)
}

func sectionType(kind domain.SectionKind) string {
	if kind == domain.SectionShows {
		return "show"
	}
	return "movie"
}

func (s *Server) sectionAll(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad section", http.StatusBadRequest)
		return
	}
	section, err := s.DB.Section(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such section", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "read section", err)
		return
	}

	kind := domain.KindMovie
	viewGroup := "movie"
	if section.Kind == domain.SectionShows {
		kind, viewGroup = domain.KindShow, "show"
	}

	page := pageFrom(r)
	items, total, err := s.DB.SectionItems(r.Context(), id, kind, page)
	if err != nil {
		s.fail(w, "list items", err)
		return
	}

	out := container{
		Identifier:          "com.plexapp.plugins.library",
		MediaTagPrefix:      "/system/bundle/media/flags/",
		AllowSync:           true,
		LibrarySectionID:    section.ID,
		LibrarySectionTitle: section.Title,
		Title1:              section.Title,
		Title2:              "All " + section.Title,
		ViewGroup:           viewGroup,
		TotalSize:           total,
		Offset:              page.Start,
	}
	s.appendItems(r.Context(), &out, section, items, extrasFrom(r))
	write(w, r, out)
}

func (s *Server) metadata(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("key"), 10, 64)
	if err != nil {
		http.Error(w, "bad key", http.StatusBadRequest)
		return
	}
	item, err := s.DB.Item(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such item", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "read item", err)
		return
	}
	section, err := s.DB.Section(r.Context(), item.SectionID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, "read section", err)
		return
	}

	out := container{
		Identifier:     "com.plexapp.plugins.library",
		MediaTagPrefix: "/system/bundle/media/flags/",
		AllowSync:      true,
	}
	if section != nil {
		out.LibrarySectionID, out.LibrarySectionTitle = section.ID, section.Title
	}
	s.appendItems(r.Context(), &out, section, []domain.Item{*item}, allExtras)
	write(w, r, out)
}

func (s *Server) children(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("key"), 10, 64)
	if err != nil {
		http.Error(w, "bad key", http.StatusBadRequest)
		return
	}
	parent, err := s.DB.Item(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such item", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "read item", err)
		return
	}

	page := pageFrom(r)
	items, total, err := s.DB.Children(r.Context(), id, page)
	if err != nil {
		s.fail(w, "list children", err)
		return
	}
	section, err := s.DB.Section(r.Context(), parent.SectionID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, "read section", err)
		return
	}

	viewGroup := "season"
	if parent.Kind == domain.KindSeason {
		viewGroup = "episode"
	}
	out := container{
		Identifier:     "com.plexapp.plugins.library",
		MediaTagPrefix: "/system/bundle/media/flags/",
		AllowSync:      true,
		ViewGroup:      viewGroup,
		Title1:         parent.Title,
		TotalSize:      total,
		Offset:         page.Start,
	}
	if section != nil {
		out.LibrarySectionID, out.LibrarySectionTitle = section.ID, section.Title
	}
	s.appendItems(r.Context(), &out, section, items, extrasFrom(r))
	write(w, r, out)
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("query")
	if query == "" {
		write(w, r, container{Size: 0})
		return
	}
	items, total, err := s.DB.Search(r.Context(), query, pageFrom(r))
	if err != nil {
		s.fail(w, "search", err)
		return
	}
	out := container{Identifier: "com.plexapp.plugins.library", TotalSize: total}
	s.appendItems(r.Context(), &out, nil, items, extrasFrom(r))
	write(w, r, out)
}

// hubs is the home screen: for now, Continue Watching only.
//
// Plex's home is a list of hubs, each with its own key and items. One real hub
// beats an empty response, and the shape is what matters for the client — more
// rows (Recently Added, On Deck per section) slot into the same structure.
func (s *Server) hubs(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	items, err := s.DB.ContinueWatching(r.Context(), user.ID, 20)
	if err != nil {
		s.fail(w, "continue watching", err)
		return
	}
	out := container{Identifier: "com.plexapp.plugins.library"}
	s.appendItems(r.Context(), &out, nil, items, extrasFrom(r))
	out.Title1 = "Continue Watching"
	write(w, r, out)
}

// timeline records playback progress. Plex clients call it every few seconds
// while playing, so it does the least work it can: resolve, write, 200.
func (s *Server) timeline(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	q := r.URL.Query()

	id, err := ratingKeyOf(q.Get("ratingKey"), q.Get("key"))
	if err != nil {
		http.Error(w, "bad ratingKey", http.StatusBadRequest)
		return
	}
	offset, _ := strconv.ParseInt(q.Get("time"), 10, 64)
	duration, _ := strconv.ParseInt(q.Get("duration"), 10, 64)

	p := domain.Progress{
		UserID:   user.ID,
		ItemID:   id,
		Offset:   time.Duration(offset) * time.Millisecond,
		Duration: time.Duration(duration) * time.Millisecond,
	}
	if q.Get("state") == "stopped" && domain.Finished(p.Offset, p.Duration) {
		p.Watched = true
	}
	if err := s.DB.SaveProgress(r.Context(), p); err != nil {
		s.fail(w, "save progress", err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) scrobble(w http.ResponseWriter, r *http.Request)   { s.setWatched(w, r, true) }
func (s *Server) unscrobble(w http.ResponseWriter, r *http.Request) { s.setWatched(w, r, false) }

func (s *Server) setWatched(w http.ResponseWriter, r *http.Request, watched bool) {
	user := userFrom(r.Context())
	id, err := ratingKeyOf(r.URL.Query().Get("key"), r.URL.Query().Get("ratingKey"))
	if err != nil {
		http.Error(w, "bad key", http.StatusBadRequest)
		return
	}

	item, err := s.DB.Item(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such item", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "read item", err)
		return
	}

	p := domain.Progress{UserID: user.ID, ItemID: id, Duration: item.Duration, Watched: watched}
	if watched {
		p.Offset = item.Duration
	}
	if err := s.DB.SaveProgress(r.Context(), p); err != nil {
		s.fail(w, "save progress", err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// streamPart serves the file itself.
//
// http.ServeContent does the work, which is the point: ranges, If-Range,
// If-Modified-Since and seeking are the standard library's problem, and this
// process holds one 32 KiB copy buffer per stream rather than any part of the
// file. A seek in a player is a Range header, so this is also what makes
// scrubbing work without a transcoder.
func (s *Server) streamPart(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad part", http.StatusBadRequest)
		return
	}
	p, err := s.DB.Part(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such part", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "read part", err)
		return
	}

	if err := serve.File(w, r, p.Path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The row outlived the file. Report it as gone rather than as a
			// server fault, and let the next scan prune the row.
			s.logger().Warn("part file is missing", "path", p.Path, "error", err)
			http.Error(w, "file is gone", http.StatusNotFound)
			return
		}
		s.fail(w, "stream part", err)
	}
}

func (s *Server) thumb(w http.ResponseWriter, r *http.Request) { s.serveImage(w, r, false) }
func (s *Server) art(w http.ResponseWriter, r *http.Request)   { s.serveImage(w, r, true) }

// serveImage sends an item's poster or backdrop, resized when asked.
func (s *Server) serveImage(w http.ResponseWriter, r *http.Request, backdrop bool) {
	if s.Artwork == nil {
		http.Error(w, "no artwork", http.StatusNotFound)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("key"), 10, 64)
	if err != nil {
		http.Error(w, "bad key", http.StatusBadRequest)
		return
	}
	item, err := s.DB.Item(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such item", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "read item", err)
		return
	}

	key := item.PosterKey
	if backdrop {
		key = item.ArtKey
	}
	if key == "" {
		http.Error(w, "no image", http.StatusNotFound)
		return
	}
	s.writeImage(w, r, key, intQuery(r, "width"), intQuery(r, "height"))
}

// photoTranscode serves Plex's resize endpoint.
//
// The url parameter is one of this server's own image paths, so the id and
// which image is wanted are parsed out of it rather than trusted as a target
// to fetch: an endpoint that resizes whatever URL it is handed is an open
// proxy, and this one is reachable with any valid token.
func (s *Server) photoTranscode(w http.ResponseWriter, r *http.Request) {
	if s.Artwork == nil {
		http.Error(w, "no artwork", http.StatusNotFound)
		return
	}
	raw := r.URL.Query().Get("url")
	if raw == "" {
		http.Error(w, "no url", http.StatusBadRequest)
		return
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		http.Error(w, "bad url", http.StatusBadRequest)
		return
	}

	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	// library/metadata/{id}/{thumb|art}
	if len(segments) != 4 || segments[0] != "library" || segments[1] != "metadata" {
		http.Error(w, "unsupported url", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(segments[2], 10, 64)
	if err != nil {
		http.Error(w, "bad url", http.StatusBadRequest)
		return
	}
	item, err := s.DB.Item(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such item", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "read item", err)
		return
	}

	key := item.PosterKey
	if segments[3] == "art" {
		key = item.ArtKey
	}
	if key == "" {
		http.Error(w, "no image", http.StatusNotFound)
		return
	}
	s.writeImage(w, r, key, intQuery(r, "width"), intQuery(r, "height"))
}

func (s *Server) writeImage(w http.ResponseWriter, r *http.Request, key string, width, height int) {
	if err := serve.Image(w, r, s.Artwork, key, width, height); err != nil {
		if errors.Is(err, artwork.ErrNotCached) {
			http.Error(w, "no image", http.StatusNotFound)
			return
		}
		s.fail(w, "serve artwork", err)
	}
}

func intQuery(r *http.Request, name string) int {
	if v := r.URL.Query().Get(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// appendItems renders items into the right slice for their kind.
func (s *Server) appendItems(ctx context.Context, out *container, section *domain.Section, items []domain.Item, want extras) {
	user := userFrom(ctx)

	ids := make([]int64, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	var progress map[int64]domain.Progress
	if user != nil {
		if p, err := s.DB.Progress(ctx, user.ID, ids); err == nil {
			progress = p
		} else {
			s.logger().Warn("could not read progress", "error", err)
		}
	}

	// Two queries for the whole page rather than two per row, and only when
	// the client said it wants them: a poster wall has no use for markers, and
	// a season of episodes would otherwise be fifty round trips to find out.
	var (
		markersByItem  map[int64][]domain.Marker
		chaptersByItem map[int64][]domain.Chapter
	)
	if want.markers {
		if m, err := s.DB.Markers(ctx, ids); err == nil {
			markersByItem = m
		} else {
			s.logger().Warn("could not read markers", "error", err)
		}
	}
	if want.chapters {
		if c, err := s.DB.Chapters(ctx, ids); err == nil {
			chaptersByItem = c
		} else {
			s.logger().Warn("could not read chapters", "error", err)
		}
	}

	for _, it := range items {
		e := s.renderItem(it, section, progress[it.ID], markersByItem[it.ID], chaptersByItem[it.ID])
		if e.Elem == "Directory" {
			out.Directory = append(out.Directory, e)
		} else {
			out.Metadata = append(out.Metadata, e)
		}
	}
	out.Size = len(out.Directory) + len(out.Metadata)
	if out.TotalSize == 0 {
		out.TotalSize = out.Size
	}
}

func (s *Server) renderItem(
	it domain.Item,
	section *domain.Section,
	p domain.Progress,
	itemMarkers []domain.Marker,
	itemChapters []domain.Chapter,
) entry {
	e := entry{
		Elem:      elementFor(it.Kind),
		RatingKey: strconv.FormatInt(it.ID, 10),
		Key:       keyFor(it.ID),
		Type:      plexType(it.Kind),
		Title:     it.Title,
		Summary:   it.Summary,
		Year:      it.Year,
		Index:     it.Index,
		Duration:  it.Duration.Milliseconds(),
		AddedAt:   millisOf(it.AddedAt),
		UpdatedAt: millisOf(it.UpdatedAt),
		GUID:      "bms://" + string(it.Kind) + "/" + strconv.FormatInt(it.ID, 10),
	}
	if it.Kind == domain.KindShow || it.Kind == domain.KindSeason {
		// A container's key is its children, which is how a client knows to
		// descend rather than to play.
		e.Key = keyFor(it.ID) + "/children"
	}
	if it.ParentID != nil {
		e.ParentRatingKey = strconv.FormatInt(*it.ParentID, 10)
	}
	if it.PosterKey != "" {
		e.Thumb = keyFor(it.ID) + "/thumb"
	}
	if it.ArtKey != "" {
		e.Art = keyFor(it.ID) + "/art"
	}
	if section != nil {
		e.LibrarySectionID, e.LibrarySectionTitle = section.ID, section.Title
	}
	if p.Offset > 0 && !p.Watched {
		e.ViewOffset = p.Offset.Milliseconds()
	}
	if p.ViewCount > 0 {
		e.ViewCount = p.ViewCount
	}
	if !p.UpdatedAt.IsZero() {
		e.LastViewedAt = millisOf(p.UpdatedAt)
	}

	for _, m := range itemMarkers {
		e.Marker = append(e.Marker, marker{
			ID:              m.ID,
			Type:            string(m.Type),
			StartTimeOffset: m.Start.Milliseconds(),
			EndTimeOffset:   m.End.Milliseconds(),
			// Plex marks the credits that run to the end of the file, which is
			// what lets a client offer "Next episode" instead of "Skip".
			Final: m.Type == domain.MarkerCredits && reachesEnd(m.End, it.Duration),
		})
	}
	for _, ch := range itemChapters {
		e.Chapter = append(e.Chapter, chapter{
			ID:              ch.ID,
			Index:           ch.Index,
			Tag:             ch.Title,
			Title:           ch.Title,
			StartTimeOffset: ch.Start.Milliseconds(),
			EndTimeOffset:   ch.End.Milliseconds(),
		})
	}

	if len(it.Parts) > 0 {
		m := media{ID: it.ID, Duration: it.Duration.Milliseconds(), Container: it.Parts[0].Container}
		for _, pt := range it.Parts {
			m.Part = append(m.Part, part{
				ID:        pt.ID,
				Key:       "/library/parts/" + strconv.FormatInt(pt.ID, 10),
				File:      pt.Path,
				Size:      pt.Size,
				Container: pt.Container,
				Duration:  it.Duration.Milliseconds(),
			})
		}
		e.Media = append(e.Media, m)
	}
	return e
}

// extras is what a client asked to be told beyond the item itself.
//
// Plex makes these opt-in per request (includeMarkers=1, includeChapters=1)
// and the clients send them only where they matter — Bask asks on the calls it
// makes before playback, and not while drawing a library. Honouring that is
// what keeps browsing a library two queries rather than two per poster.
type extras struct {
	markers  bool
	chapters bool
}

// allExtras is what the single-item metadata call serves regardless of what
// was asked for: it is one row, the extra cost is two indexed lookups, and a
// client that forgot the parameter still gets a Skip Intro button.
var allExtras = extras{markers: true, chapters: true}

func extrasFrom(r *http.Request) extras {
	q := r.URL.Query()
	return extras{markers: truthy(q.Get("includeMarkers")), chapters: truthy(q.Get("includeChapters"))}
}

// truthy accepts what the clients actually send: 1, true, yes.
func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// reachesEnd reports whether a marker runs to the end of the file, allowing a
// second of slack: a credits marker and a container's own duration disagree by
// a frame or two routinely, and a client switching on "is this the end" should
// not be decided by rounding.
func reachesEnd(end, duration time.Duration) bool {
	if duration <= 0 {
		return false
	}
	return duration-end <= time.Second
}

// pageFrom reads the window the client asked for. Plex clients send it as
// headers; some send the query form, so both are accepted.
func pageFrom(r *http.Request) store.Page {
	start := intHeader(r, "X-Plex-Container-Start", "containerStart")
	size := intHeader(r, "X-Plex-Container-Size", "containerSize")
	return store.Page{Start: start, Size: size}
}

func intHeader(r *http.Request, header, query string) int {
	if v := r.Header.Get(header); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	if v := r.URL.Query().Get(query); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

// ratingKeyOf accepts either a bare id or a /library/metadata/{id} key, since
// clients send both depending on the call.
func ratingKeyOf(values ...string) (int64, error) {
	for _, v := range values {
		if v == "" {
			continue
		}
		v = strings.TrimPrefix(v, "/library/metadata/")
		v = strings.TrimSuffix(v, "/children")
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			return id, nil
		}
	}
	return 0, errors.New("no usable rating key")
}

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	s.logger().Error("request failed", "what", what, "error", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
