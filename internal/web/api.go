package web

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/inknironapps/bask-media-server/internal/artwork"
	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/nowplaying"
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
	Disc     int    `json:"disc,omitempty"`
	Summary  string `json:"summary,omitempty"`

	// Rating is the content rating as the board wrote it, for the line under
	// a title.
	Rating string `json:"rating,omitempty"`

	// Genres are what it is about, for the chips under the summary — and
	// each one is a link back into the library filtered by it, which is what
	// LibraryID is here for.
	Genres    []string `json:"genres,omitempty"`
	LibraryID int64    `json:"libraryId,omitempty"`

	// Watchlist is whether this account means to watch it, so the button is
	// drawn in the state it is in.
	Watchlist bool `json:"watchlist,omitempty"`

	DurationMS int64 `json:"durationMs,omitempty"`
	Poster     bool  `json:"poster"`
	Art        bool  `json:"art"`
	ParentID   int64 `json:"parentId,omitempty"`

	// Performer is who plays on a track, where that is not the album's own
	// artist. The page puts it under the title on a compilation and leaves
	// the row alone on every other album.
	Performer string `json:"performer,omitempty"`

	// TakenAt is when a photograph was taken, as a date the page can show
	// without parsing a timestamp, and empty for everything else.
	TakenAt string `json:"takenAt,omitempty"`

	// Width and Height are a photograph's, as it is meant to be seen, so the
	// page can reserve the right shape before the picture arrives.
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`

	// Where this viewer is in it. Their own row and nobody else's: progress
	// is per account, which is the point of having accounts.
	OffsetMS int64 `json:"offsetMs,omitempty"`
	Watched  bool  `json:"watched,omitempty"`

	// Favourite is whether this viewer starred it, and theirs alone for the
	// same reason. Always sent, never omitted: a page that receives nothing
	// cannot tell "not a favourite" from "this server does not do stars",
	// and would draw an empty star it has no way to fill.
	Favourite bool `json:"favourite"`
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
	progress, err := s.db(ctx).Progress(ctx, userID, ids)
	if err != nil {
		return nil, err
	}
	// One query for the page, like the progress above it.
	favourites, err := s.db(ctx).FavouriteSet(ctx, userID, ids)
	if err != nil {
		return nil, err
	}
	// And one for the watchlist, for the same reason: a grid of forty
	// posters draws forty buttons and must not ask forty times.
	wanted, err := s.db(ctx).WatchlistSet(ctx, userID, ids)
	if err != nil {
		return nil, err
	}

	parents := map[int64]*domain.Item{}
	parent := func(id int64) *domain.Item {
		if known, found := parents[id]; found {
			return known
		}
		it, err := s.db(ctx).Item(ctx, id)
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
			Index: it.Index, Disc: it.Disc, Summary: it.Summary, Rating: it.Rating,
			Genres: it.Genres, LibraryID: it.SectionID,
			DurationMS: it.Duration.Milliseconds(),
			Favourite:  favourites[it.ID],
			Watchlist:  wanted[it.ID],
			// A photograph and an album of them have no poster in the cache:
			// the picture itself is the thumbnail, made on request.
			Poster: it.PosterKey != "" || it.Kind == domain.KindPhoto || it.Kind == domain.KindPhotoAlbum,
			Art:    it.ArtKey != "",
		}
		if !it.TakenAt.IsZero() {
			view.TakenAt = it.TakenAt.Format("2006-01-02")
		}
		if it.Kind == domain.KindPhoto && len(it.Parts) > 0 {
			view.Width, view.Height = displaySize(it.Parts[0])
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
		// A track says who plays it and what it is on, which is what makes a
		// search result readable and what a compilation needs on every row.
		if it.Kind == domain.KindTrack && it.ParentID != nil {
			view.Performer = it.Performer
			var bits []string
			if it.Performer != "" {
				bits = append(bits, it.Performer)
			}
			if album := parent(*it.ParentID); album != nil {
				if len(bits) == 0 && album.ParentID != nil {
					if artist := parent(*album.ParentID); artist != nil {
						bits = append(bits, artist.Title)
					}
				}
				bits = append(bits, album.Title)
			}
			view.Subtitle = strings.Join(bits, " · ")
		}
		if it.Kind == domain.KindAlbum && it.ParentID != nil {
			if artist := parent(*it.ParentID); artist != nil {
				view.Subtitle = artist.Title
			}
		}
		out = append(out, view)
	}
	return out, nil
}

// home is what the interface opens on: where this viewer was, and the
// libraries they can go into.
func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())

	sections, err := s.db(r.Context()).Sections(r.Context())
	if err != nil {
		s.fail(w, "list sections", err)
		return
	}
	resume, err := s.db(r.Context()).ContinueWatching(r.Context(), user.ID, 20)
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
	stored, err := s.Store.ServerValue(r.Context(), store.KeyFriendlyName)
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

	section, err := s.db(r.Context()).Section(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such library")
		return
	}
	if err != nil {
		s.fail(w, "read section", err)
		return
	}

	page := store.Page{
		Start: intQuery(r, "start"), Size: intQuery(r, "size"),
		// A genre a viewer clicked on a film's page, which is what turns a
		// chip into the rest of the shelf.
		Genres: genresWanted(r),
	}
	kind := section.Kind.TopKind()
	items, total, err := s.db(r.Context()).SectionItems(r.Context(), id, kind, page)
	if err != nil {
		s.fail(w, "list items", err)
		return
	}
	views, err := s.views(r.Context(), items, user.ID)
	if err != nil {
		s.fail(w, "read progress", err)
		return
	}

	// The genres in the library, so the page can offer them without a second
	// request — and the one being filtered by, so it can show which.
	genres, err := s.db(r.Context()).SectionGenres(r.Context(), section.ID, string(kind))
	if err != nil {
		s.fail(w, "list genres", err)
		return
	}
	names := make([]string, 0, len(genres))
	for _, genre := range genres {
		names = append(names, genre.Name)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id": section.ID, "title": section.Title, "kind": string(section.Kind),
		"total": total, "start": page.Start, "items": views,
		"genres": names, "genre": strings.Join(page.Genres, ","),
	})
}

// genresWanted reads the genre a viewer is narrowing a library by.
//
// A comma separated list, matched as "any of these", because that is what
// ticking two boxes means and what every other surface here accepts.
func genresWanted(r *http.Request) []string {
	var out []string
	for _, value := range strings.Split(r.URL.Query().Get("genre"), ",") {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

// item is one thing and what is under it: a show's seasons, a season's
// episodes, or a film's file and markers.
func (s *Server) item(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	user := userFrom(r.Context())

	found, err := s.db(r.Context()).Item(r.Context(), id)
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
		children, _, err := s.db(r.Context()).Children(r.Context(), found.ID, store.Page{Size: 200})
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

	markers, err := s.db(r.Context()).Markers(r.Context(), []int64{found.ID})
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

	// Subtitles as a menu: which ones exist, what to call them, and which of
	// them this page can actually show — a picture subtitle has no text to
	// hand over, and saying so is better than an empty track.
	subs, err := s.db(r.Context()).Subtitles(r.Context(), []int64{found.ID})
	if err != nil {
		s.fail(w, "read subtitles", err)
		return
	}
	out["subtitles"] = subtitleViews(subs[found.ID])

	// Trailers and the rest, each with the sort it is, so the page can draw
	// the trailer behind the play button and the others in a shelf. Asked for
	// here rather than on the grid, because a shelf nobody has opened is not
	// worth a query per poster.
	bonus, err := s.db(r.Context()).Extras(r.Context(), found.ID)
	if err != nil {
		s.fail(w, "read extras", err)
		return
	}
	if len(bonus) > 0 {
		bonusViews, err := s.views(r.Context(), bonus, user.ID)
		if err != nil {
			s.fail(w, "read progress", err)
			return
		}
		shelf := make([]map[string]any, 0, len(bonus))
		for i, extra := range bonus {
			shelf = append(shelf, map[string]any{
				"item":  bonusViews[i],
				"sort":  string(extra.Extra),
				"label": extra.Extra.Label(),
			})
		}
		out["extras"] = shelf
	}

	// The season and show above an episode, so a player can say what is on
	// without another round trip.
	if found.ParentID != nil {
		if parent, err := s.db(r.Context()).Item(r.Context(), *found.ParentID); err == nil {
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

	items, _, err := s.db(r.Context()).Search(r.Context(), query, store.Page{Size: 40})
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

	if err := s.db(r.Context()).SaveProgress(r.Context(), domain.Progress{
		UserID:   user.ID,
		ItemID:   body.ItemID,
		Offset:   time.Duration(body.OffsetMS) * time.Millisecond,
		Duration: time.Duration(body.DurationMS) * time.Millisecond,
	}); err != nil {
		s.fail(w, "save progress", err)
		return
	}

	key := streamKey(r, body.ItemID)
	if s.refused(w, key) {
		return
	}
	s.report(r, nowplaying.Stream{
		Key:      key,
		ItemID:   body.ItemID,
		Position: time.Duration(body.OffsetMS) * time.Millisecond,
		Duration: time.Duration(body.DurationMS) * time.Millisecond,
	})
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
	item, err := s.db(r.Context()).Item(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such item", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "read item", err)
		return
	}

	// Clamped, because the width is a number from a URL and rendering is the
	// memory-hungry half of serving artwork.
	width, height := min(intQuery(r, "w"), 4000), min(intQuery(r, "h"), 4000)

	// A photograph is served from the library, where it already is: see
	// serve.Photo. Everything else is a poster in the artwork cache.
	if part, found := s.photoSource(r.Context(), item); found {
		if err := serve.Photo(w, r, s.Artwork, part.Path, width, height, part.Orientation); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				s.logger().Warn("a photograph is missing", "path", part.Path)
				http.Error(w, "file is gone", http.StatusNotFound)
				return
			}
			s.fail(w, "serve photo", err)
		}
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
	if err := serve.Image(w, r, s.Artwork, key, width, height); err != nil {
		if errors.Is(err, artwork.ErrNotCached) {
			http.Error(w, "no image", http.StatusNotFound)
			return
		}
		s.fail(w, "serve artwork", err)
	}
}

// photoSource is the file a photograph is shown from, or the earliest
// photograph inside an album of them, which is the only cover it has.
func (s *Server) photoSource(ctx context.Context, item *domain.Item) (domain.Part, bool) {
	switch item.Kind {
	case domain.KindPhoto:
		parts := item.Parts
		if len(parts) == 0 {
			found, err := s.db(ctx).Parts(ctx, item.ID)
			if err != nil {
				s.logger().Warn("could not read a photograph's file", "item", item.ID, "error", err)
				return domain.Part{}, false
			}
			parts = found
		}
		if len(parts) > 0 {
			return parts[0], true
		}
	case domain.KindPhotoAlbum:
		part, err := s.db(ctx).AlbumCover(ctx, item.ID)
		if err == nil {
			return *part, true
		}
		if !errors.Is(err, store.ErrNotFound) {
			s.logger().Warn("could not read an album cover", "item", item.ID, "error", err)
		}
	}
	return domain.Part{}, false
}

// displaySize is a photograph's dimensions as they are meant to be seen: its
// stored size with a quarter turn applied where its EXIF asks for one.
func displaySize(p domain.Part) (width, height int) {
	if p.Orientation >= 5 && p.Orientation <= 8 {
		return p.Height, p.Width
	}
	return p.Width, p.Height
}

// stream sends the file itself, with ranges, which is what lets the browser's
// own player seek.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	part, err := s.db(r.Context()).Part(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such file", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "read part", err)
		return
	}

	// Stopping a direct play is refusing to keep sending the file: there is
	// no encoder behind it to kill.
	if s.refused(w, streamKey(r, part.ItemID)) {
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
