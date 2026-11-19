package jellyfin

import (
	"errors"
	"net/http"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// views lists the libraries.
func (s *Server) views(w http.ResponseWriter, r *http.Request) {
	sections, err := s.DB.Sections(r.Context())
	if err != nil {
		s.fail(w, "list sections", err)
		return
	}
	out := make([]item, 0, len(sections))
	for _, section := range sections {
		out = append(out, s.view(section))
	}
	writeJSON(w, http.StatusOK, itemsReply{Items: out, TotalRecordCount: len(out)})
}

// items is the one query a client browses with.
//
// Jellyfin's `/Items` takes a dozen parameters and clients combine them
// freely; the ones that change what comes back are honoured and the rest are
// ignored rather than refused, because a client that asked for a sort BMS
// does not have still wants its list.
//
// What is honoured: ParentId (what to look inside), IncludeItemTypes (which
// kinds), Recursive (look past the first level), SearchTerm, and the
// StartIndex/Limit window every list is paged by.
func (s *Server) items(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	query := r.URL.Query()

	if term := strings.TrimSpace(query.Get("SearchTerm")); term != "" {
		s.searchItems(w, r, term)
		return
	}

	page := store.Page{Start: intQuery(r, "StartIndex"), Size: intQuery(r, "Limit")}
	wanted, understood := wantedKinds(query.Get("IncludeItemTypes"))
	if !understood {
		nothing(w, page)
		return
	}

	parent := strings.TrimSpace(query.Get("ParentId"))
	if parent == "" {
		// No parent: everything of the asked-for kinds across the library,
		// which is what a client does for "all movies" and for a sweep it
		// builds a home screen from.
		s.everything(w, r, wanted, page)
		return
	}

	id, library, err := parseID(parent)
	if err != nil {
		http.Error(w, "bad ParentId", http.StatusBadRequest)
		return
	}

	// The id says which it is: a library's top level, or a parent's children.
	if library {
		section, err := s.DB.Section(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no such library", http.StatusNotFound)
			return
		}
		if err != nil {
			s.fail(w, "read section", err)
			return
		}
		kind := section.Kind.TopKind()
		if boolQuery(r, "Recursive") && len(wanted) > 0 {
			// "Everything of this kind in this library", which is how a
			// client lists every episode of a show library or every track.
			s.sectionRecursive(w, r, *section, wanted, page)
			return
		}
		if len(wanted) > 0 && !wanted[kind] {
			s.sectionRecursive(w, r, *section, wanted, page)
			return
		}
		list, total, err := s.DB.SectionItems(r.Context(), section.ID, kind, page)
		if err != nil {
			s.fail(w, "list section", err)
			return
		}
		s.writeItems(w, r, list, total, page, user)
		return
	}

	children, total, err := s.DB.Children(r.Context(), id, page)
	if err != nil {
		s.fail(w, "list children", err)
		return
	}
	children = keepKinds(children, wanted)
	s.writeItems(w, r, children, total, page, user)
}

// everything answers a query with no parent: the asked-for kinds across every
// library, or every library's top level when nothing was asked for.
func (s *Server) everything(w http.ResponseWriter, r *http.Request, wanted map[domain.Kind]bool, page store.Page) {
	user := userFrom(r.Context())
	sections, err := s.DB.Sections(r.Context())
	if err != nil {
		s.fail(w, "list sections", err)
		return
	}

	var all []domain.Item
	for _, section := range sections {
		kinds := []domain.Kind{section.Kind.TopKind()}
		if len(wanted) > 0 {
			kinds = nil
			for kind := range wanted {
				kinds = append(kinds, kind)
			}
		}
		for _, kind := range kinds {
			// Paged per section and then cut to the window, because the page
			// is a window over the whole answer rather than over one library.
			list, _, err := s.DB.SectionItems(r.Context(), section.ID, kind, store.Page{Size: 500})
			if err != nil {
				s.fail(w, "list section", err)
				return
			}
			all = append(all, list...)
		}
	}
	total := len(all)
	all = window(all, page)
	s.writeItems(w, r, all, total, page, user)
}

// sectionRecursive lists one library's contents of a given kind, however deep
// they sit: a show library's episodes, a music library's tracks.
func (s *Server) sectionRecursive(w http.ResponseWriter, r *http.Request, section domain.Section, wanted map[domain.Kind]bool, page store.Page) {
	user := userFrom(r.Context())
	kinds := make([]domain.Kind, 0, len(wanted))
	for kind := range wanted {
		kinds = append(kinds, kind)
	}
	if len(kinds) == 0 {
		kinds = []domain.Kind{section.Kind.TopKind()}
	}

	var all []domain.Item
	for _, kind := range kinds {
		list, err := s.DB.SectionItemsOfKind(r.Context(), section.ID, kind, 1000)
		if err != nil {
			s.fail(w, "list library", err)
			return
		}
		all = append(all, list...)
	}
	total := len(all)
	s.writeItems(w, r, window(all, page), total, page, user)
}

func (s *Server) searchItems(w http.ResponseWriter, r *http.Request, term string) {
	user := userFrom(r.Context())
	page := store.Page{Start: intQuery(r, "StartIndex"), Size: intQuery(r, "Limit")}
	found, total, err := s.DB.Search(r.Context(), term, page)
	if err != nil {
		s.fail(w, "search", err)
		return
	}
	wanted, understood := wantedKinds(r.URL.Query().Get("IncludeItemTypes"))
	if !understood {
		nothing(w, page)
		return
	}
	found = keepKinds(found, wanted)
	s.writeItems(w, r, found, total, page, user)
}

// item is one thing, with everything a client needs to open or play it.
func (s *Server) item(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	id, library, err := parseID(r.PathValue("item"))
	if err != nil {
		http.Error(w, "bad item", http.StatusBadRequest)
		return
	}

	// A client opening a library asks for it here, the same way it asks for
	// anything else.
	if library {
		section, err := s.DB.Section(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no such library", http.StatusNotFound)
			return
		}
		if err != nil {
			s.fail(w, "read section", err)
			return
		}
		writeJSON(w, http.StatusOK, s.view(*section))
		return
	}

	found, err := s.DB.Item(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such item", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "read item", err)
		return
	}

	rel := s.relatives(r.Context(), []domain.Item{*found})
	progress := s.progressFor(r, user, []domain.Item{*found})
	rendered := s.render(*found, rel, progress[found.ID])
	rendered.MediaStreams = s.streamsOf(r.Context(), *found)
	if len(rendered.MediaSources) > 0 {
		rendered.MediaSources[0].MediaStreams = rendered.MediaStreams
	}
	writeJSON(w, http.StatusOK, rendered)
}

// seasons and episodes are the two shortcuts a television client takes
// instead of walking the tree with /Items.
func (s *Server) seasons(w http.ResponseWriter, r *http.Request) {
	s.childrenOf(w, r, r.PathValue("series"), domain.KindSeason)
}

func (s *Server) episodes(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	id, _, err := parseID(r.PathValue("series"))
	if err != nil {
		http.Error(w, "bad series", http.StatusBadRequest)
		return
	}

	// One season, when the client names it; otherwise every episode of the
	// show, in order, which is what a "play next" needs.
	if season := strings.TrimSpace(r.URL.Query().Get("SeasonId")); season != "" {
		s.childrenOf(w, r, season, domain.KindEpisode)
		return
	}

	seasons, _, err := s.DB.Children(r.Context(), id, store.Page{Size: 200})
	if err != nil {
		s.fail(w, "list seasons", err)
		return
	}
	var all []domain.Item
	for _, season := range seasons {
		episodes, _, err := s.DB.Children(r.Context(), season.ID, store.Page{Size: 500})
		if err != nil {
			s.fail(w, "list episodes", err)
			return
		}
		all = append(all, episodes...)
	}
	page := store.Page{Start: intQuery(r, "StartIndex"), Size: intQuery(r, "Limit")}
	total := len(all)
	s.writeItems(w, r, window(all, page), total, page, user)
}

func (s *Server) childrenOf(w http.ResponseWriter, r *http.Request, rawID string, kind domain.Kind) {
	user := userFrom(r.Context())
	id, _, err := parseID(rawID)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	page := store.Page{Start: intQuery(r, "StartIndex"), Size: intQuery(r, "Limit")}
	children, total, err := s.DB.Children(r.Context(), id, page)
	if err != nil {
		s.fail(w, "list children", err)
		return
	}
	children = keepKinds(children, map[domain.Kind]bool{kind: true})
	s.writeItems(w, r, children, total, page, user)
}

// artists is the music library's top level, which Jellyfin addresses through
// its own path rather than through /Items.
func (s *Server) artists(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	page := store.Page{Start: intQuery(r, "StartIndex"), Size: intQuery(r, "Limit")}

	sections, err := s.DB.Sections(r.Context())
	if err != nil {
		s.fail(w, "list sections", err)
		return
	}
	var all []domain.Item
	for _, section := range sections {
		if section.Kind != domain.SectionMusic {
			continue
		}
		list, _, err := s.DB.SectionItems(r.Context(), section.ID, domain.KindArtist, store.Page{Size: 500})
		if err != nil {
			s.fail(w, "list artists", err)
			return
		}
		all = append(all, list...)
	}
	total := len(all)
	s.writeItems(w, r, window(all, page), total, page, user)
}

// latest is what arrived recently, and resume is where this viewer left off.
// Both are rows on a client's home screen.
func (s *Server) latest(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	limit := intQuery(r, "Limit")
	if limit <= 0 {
		limit = 20
	}
	recent, err := s.DB.RecentlyAdded(r.Context(), limit)
	if err != nil {
		s.fail(w, "recently added", err)
		return
	}
	// /Items/Latest answers with a bare array, not the usual envelope.
	rel := s.relatives(r.Context(), recent)
	progress := s.progressFor(r, user, recent)
	out := make([]item, 0, len(recent))
	for _, it := range recent {
		out = append(out, s.render(it, rel, progress[it.ID]))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	limit := intQuery(r, "Limit")
	if limit <= 0 {
		limit = 20
	}
	items, err := s.DB.ContinueWatching(r.Context(), user.ID, limit)
	if err != nil {
		s.fail(w, "continue watching", err)
		return
	}
	s.writeItems(w, r, items, len(items), store.Page{}, user)
}

// nextUp is the next unwatched episode of each show in progress. BMS keeps
// what it takes to answer it — progress per item, episodes in order — so a
// client's "up next" row is a real answer rather than an empty one.
func (s *Server) nextUp(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	limit := intQuery(r, "Limit")
	if limit <= 0 {
		limit = 20
	}
	items, err := s.DB.NextUp(r.Context(), user.ID, limit)
	if err != nil {
		s.fail(w, "next up", err)
		return
	}
	s.writeItems(w, r, items, len(items), store.Page{}, user)
}

// emptyList and emptyItems answer for the things BMS does not have: a bare
// array and an envelope respectively, because Jellyfin uses both shapes and a
// client parses whichever it expects.
func (s *Server) emptyList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []item{})
}

func (s *Server) emptyItems(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, itemsReply{Items: []item{}})
}

// displayPreferences is where a client keeps its own view settings. BMS does
// not store them, and an empty set is a server saying "you decide".
func (s *Server) displayPreferences(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"Id":                 r.PathValue("id"),
		"SortBy":             "SortName",
		"SortOrder":          "Ascending",
		"RememberIndexing":   false,
		"RememberSorting":    false,
		"ScrollDirection":    "Horizontal",
		"ShowBackdrop":       true,
		"ShowSidebar":        false,
		"PrimaryImageHeight": 250,
		"PrimaryImageWidth":  250,
		"CustomPrefs":        map[string]any{},
	})
}

/* ---------- shared ---------- */

func (s *Server) writeItems(w http.ResponseWriter, r *http.Request, items []domain.Item, total int, page store.Page, user *domain.User) {
	rel := s.relatives(r.Context(), items)
	progress := s.progressFor(r, user, items)

	out := make([]item, 0, len(items))
	for _, it := range items {
		out = append(out, s.render(it, rel, progress[it.ID]))
	}
	if total < len(out) {
		total = len(out)
	}
	writeJSON(w, http.StatusOK, itemsReply{Items: out, TotalRecordCount: total, StartIndex: page.Start})
}

func (s *Server) progressFor(r *http.Request, user *domain.User, items []domain.Item) map[int64]domain.Progress {
	if user == nil || len(items) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	progress, err := s.DB.Progress(r.Context(), user.ID, ids)
	if err != nil {
		s.logger().Warn("could not read progress", "error", err)
		return nil
	}
	return progress
}

// wantedKinds reads IncludeItemTypes.
//
// Three answers, and the third one matters: no filter at all, a filter this
// server understands, and a filter naming only things it does not have —
// Playlist, BoxSet, LiveTvChannel. The last is not "no filter": a client
// asking for playlists on a server with none must be told there are none,
// and the first version of this answered it with every film, artist and
// photo album in the library, which is what a client's Playlists tab then
// showed.
func wantedKinds(value string) (kinds map[domain.Kind]bool, understood bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, true
	}
	out := map[domain.Kind]bool{}
	for _, name := range strings.Split(value, ",") {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "movie":
			out[domain.KindMovie] = true
		case "series":
			out[domain.KindShow] = true
		case "season":
			out[domain.KindSeason] = true
		case "episode":
			out[domain.KindEpisode] = true
		case "musicartist", "albumartist":
			out[domain.KindArtist] = true
		case "musicalbum":
			out[domain.KindAlbum] = true
		case "audio", "musicvideo":
			out[domain.KindTrack] = true
		case "photoalbum":
			out[domain.KindPhotoAlbum] = true
		case "photo":
			out[domain.KindPhoto] = true
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// nothing answers a query for something this server does not have.
func nothing(w http.ResponseWriter, page store.Page) {
	writeJSON(w, http.StatusOK, itemsReply{Items: []item{}, StartIndex: page.Start})
}

func keepKinds(items []domain.Item, wanted map[domain.Kind]bool) []domain.Item {
	if len(wanted) == 0 {
		return items
	}
	out := items[:0]
	for _, it := range items {
		if wanted[it.Kind] {
			out = append(out, it)
		}
	}
	return out
}

// window cuts a gathered list to the page a client asked for.
func window(items []domain.Item, page store.Page) []domain.Item {
	start := page.Start
	if start < 0 || start >= len(items) {
		if start > 0 {
			return nil
		}
		start = 0
	}
	end := len(items)
	if page.Size > 0 && start+page.Size < end {
		end = start + page.Size
	}
	return items[start:end]
}
