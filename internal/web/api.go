package web

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"strconv"
	"time"

	"github.com/inknironapps/bask-media-server/internal/artwork"
	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/serve"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// itemView is one thing as the page draws it: enough to put a poster on a
// grid and a sentence under it, and nothing a browser cannot use.
type itemView struct {
	ID       int64  `json:"id"`
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"`
	Year     int    `json:"year,omitempty"`
	Index    int    `json:"index,omitempty"`
	Summary  string `json:"summary,omitempty"`

	DurationMS int64 `json:"durationMs,omitempty"`
	Poster     bool  `json:"poster"`
	Art        bool  `json:"art"`
	ParentID   int64 `json:"parentId,omitempty"`

	// Where this viewer is in it. Their own row and nobody else's: progress
	// is per account, which is the point of having accounts.
	OffsetMS int64 `json:"offsetMs,omitempty"`
	Watched  bool  `json:"watched,omitempty"`
}

// views renders items for one viewer, with their progress and — for episodes
// — the show and season they belong to.
//
// The parent lookups are cached per call and a page is at most a few dozen
// items, so a grid costs two queries plus a handful of point reads rather
// than a query per poster.
func (s *Server) views(ctx context.Context, items []domain.Item, userID int64) ([]itemView, error) {
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	progress, err := s.DB.Progress(ctx, userID, ids)
	if err != nil {
		return nil, err
	}

	parents := map[int64]*domain.Item{}
	parent := func(id int64) *domain.Item {
		if known, found := parents[id]; found {
			return known
		}
		it, err := s.DB.Item(ctx, id)
		if err != nil {
			it = nil
		}
		parents[id] = it
		return it
	}

	out := make([]itemView, 0, len(items))
	for _, it := range items {
		view := itemView{
			ID: it.ID, Kind: string(it.Kind), Title: it.Title, Year: it.Year,
			Index: it.Index, Summary: it.Summary,
			DurationMS: it.Duration.Milliseconds(),
			Poster:     it.PosterKey != "", Art: it.ArtKey != "",
		}
		if it.ParentID != nil {
			view.ParentID = *it.ParentID
		}
		if p, found := progress[it.ID]; found {
			view.OffsetMS, view.Watched = p.Offset.Milliseconds(), p.Watched
		}
		if it.Kind == domain.KindEpisode && it.ParentID != nil {
			if season := parent(*it.ParentID); season != nil {
				view.Subtitle = "S" + strconv.Itoa(season.Index) + " · E" + strconv.Itoa(it.Index)
				if season.ParentID != nil {
					if show := parent(*season.ParentID); show != nil {
						view.Subtitle = show.Title + " · " + view.Subtitle
					}
				}
			}
		}
		if it.Kind == domain.KindSeason && it.Title == "" {
			view.Title = "Season " + strconv.Itoa(it.Index)
		}
		out = append(out, view)
	}
	return out, nil
}

// home is what the interface opens on: where this viewer was, and the
// libraries they can go into.
func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())

	sections, err := s.DB.Sections(r.Context())
	if err != nil {
		s.fail(w, "list sections", err)
		return
	}
	resume, err := s.DB.ContinueWatching(r.Context(), user.ID, 20)
	if err != nil {
		s.fail(w, "continue watching", err)
		return
	}
	views, err := s.views(r.Context(), resume, user.ID)
	if err != nil {
		s.fail(w, "read progress", err)
		return
	}

	type sectionView struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
		Kind  string `json:"kind"`
	}
	list := make([]sectionView, 0, len(sections))
	for _, sec := range sections {
		list = append(list, sectionView{ID: sec.ID, Title: sec.Title, Kind: string(sec.Kind)})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"name":     s.friendlyName(r),
		"sections": list,
		"continue": views,
	})
}

func (s *Server) friendlyName(r *http.Request) string {
	stored, err := s.DB.ServerValue(r.Context(), store.KeyFriendlyName)
	if err == nil && stored != "" {
		return stored
	}
	return s.Name
}

// section lists a library, a page at a time. The page is the same window the
// Plex surface pages with, for the same reason: nothing may ask for a whole
// library at once.
func (s *Server) section(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	user := userFrom(r.Context())

	section, err := s.DB.Section(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such library")
		return
	}
	if err != nil {
		s.fail(w, "read section", err)
		return
	}

	kind := domain.KindMovie
	if section.Kind == domain.SectionShows {
		kind = domain.KindShow
	}
	page := store.Page{Start: intQuery(r, "start"), Size: intQuery(r, "size")}
	items, total, err := s.DB.SectionItems(r.Context(), id, kind, page)
	if err != nil {
		s.fail(w, "list items", err)
		return
	}
	views, err := s.views(r.Context(), items, user.ID)
	if err != nil {
		s.fail(w, "read progress", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id": section.ID, "title": section.Title, "kind": string(section.Kind),
		"total": total, "start": page.Start, "items": views,
	})
}

// item is one thing and what is under it: a show's seasons, a season's
// episodes, or a film's file and markers.
func (s *Server) item(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	user := userFrom(r.Context())

	found, err := s.DB.Item(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such item")
		return
	}
	if err != nil {
		s.fail(w, "read item", err)
		return
	}

	views, err := s.views(r.Context(), []domain.Item{*found}, user.ID)
	if err != nil {
		s.fail(w, "read progress", err)
		return
	}
	out := map[string]any{"item": views[0]}

	if !found.Playable() {
		children, _, err := s.DB.Children(r.Context(), found.ID, store.Page{Size: 200})
		if err != nil {
			s.fail(w, "read children", err)
			return
		}
		childViews, err := s.views(r.Context(), children, user.ID)
		if err != nil {
			s.fail(w, "read progress", err)
			return
		}
		out["children"] = childViews
	}

	// Parts and markers are what a player needs: which file to ask for, and
	// where the parts of it are that a viewer wants to skip.
	type partView struct {
		ID        int64  `json:"id"`
		Container string `json:"container"`
		Size      int64  `json:"size"`
	}
	parts := make([]partView, 0, len(found.Parts))
	for _, p := range found.Parts {
		parts = append(parts, partView{ID: p.ID, Container: p.Container, Size: p.Size})
	}
	out["parts"] = parts

	markers, err := s.DB.Markers(r.Context(), []int64{found.ID})
	if err != nil {
		s.fail(w, "read markers", err)
		return
	}
	type markerView struct {
		Type    string `json:"type"`
		StartMS int64  `json:"startMs"`
		EndMS   int64  `json:"endMs"`
		Final   bool   `json:"final"`
	}
	list := make([]markerView, 0, len(markers[found.ID]))
	for _, m := range markers[found.ID] {
		list = append(list, markerView{
			Type: string(m.Type), StartMS: m.Start.Milliseconds(), EndMS: m.End.Milliseconds(),
			// Credits that run to the end of the file are the end of the
			// episode, which is a different button from "skip this bit".
			Final: m.Type == domain.MarkerCredits && found.Duration > 0 && found.Duration-m.End <= time.Second,
		})
	}
	out["markers"] = list

	// The season and show above an episode, so a player can say what is on
	// without another round trip.
	if found.ParentID != nil {
		if parent, err := s.DB.Item(r.Context(), *found.ParentID); err == nil {
			title := parent.Title
			if parent.Kind == domain.KindSeason && title == "" {
				title = "Season " + strconv.Itoa(parent.Index)
			}
			out["parent"] = map[string]any{"id": parent.ID, "title": title, "kind": string(parent.Kind)}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	query := r.URL.Query().Get("q")
	if len(query) < 2 {
		writeJSON(w, http.StatusOK, map[string]any{"items": []itemView{}})
		return
	}

	items, _, err := s.DB.Search(r.Context(), query, store.Page{Size: 40})
	if err != nil {
		s.fail(w, "search", err)
		return
	}
	views, err := s.views(r.Context(), items, user.ID)
	if err != nil {
		s.fail(w, "read progress", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": views})
}

// saveProgress records where this viewer is. Sent every few seconds while
// something plays, and on pause, so closing a laptop mid-episode still
// resumes in the right place on the television.
func (s *Server) saveProgress(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ItemID     int64 `json:"itemId"`
		OffsetMS   int64 `json:"offsetMs"`
		DurationMS int64 `json:"durationMs"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.ItemID <= 0 || body.OffsetMS < 0 {
		writeError(w, http.StatusBadRequest, "that is not a position")
		return
	}
	user := userFrom(r.Context())

	if err := s.DB.SaveProgress(r.Context(), domain.Progress{
		UserID:   user.ID,
		ItemID:   body.ItemID,
		Offset:   time.Duration(body.OffsetMS) * time.Millisecond,
		Duration: time.Duration(body.DurationMS) * time.Millisecond,
	}); err != nil {
		s.fail(w, "save progress", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// image serves a poster or a backdrop at the size the grid asked for.
func (s *Server) image(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	if s.Artwork == nil {
		http.Error(w, "no artwork", http.StatusNotFound)
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
	if r.URL.Query().Get("kind") == "art" {
		key = item.ArtKey
	}
	if key == "" {
		http.Error(w, "no image", http.StatusNotFound)
		return
	}
	// Clamped, because the width is a number from a URL and rendering is the
	// memory-hungry half of serving artwork.
	width, height := min(intQuery(r, "w"), 2000), min(intQuery(r, "h"), 2000)
	if err := serve.Image(w, r, s.Artwork, key, width, height); err != nil {
		if errors.Is(err, artwork.ErrNotCached) {
			http.Error(w, "no image", http.StatusNotFound)
			return
		}
		s.fail(w, "serve artwork", err)
	}
}

// stream sends the file itself, with ranges, which is what lets the browser's
// own player seek.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	part, err := s.DB.Part(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such file", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "read part", err)
		return
	}

	if err := serve.File(w, r, part.Path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.logger().Warn("part file is missing", "path", part.Path, "error", err)
			http.Error(w, "file is gone", http.StatusNotFound)
			return
		}
		s.fail(w, "stream part", err)
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
