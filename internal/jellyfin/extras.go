package jellyfin

import (
	"errors"
	"net/http"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// Trailers, featurettes and deleted scenes, as a Jellyfin client asks for
// them.
//
// Two routes for the same rows, because Jellyfin has two and its clients use
// both: special features are everything that belongs to an item, and local
// trailers are the subset that are trailers. The counts go on the item itself,
// which is how a client knows to draw the row at all before it asks.

// extraTypeOf is Jellyfin's spelling of a sort of extra, which is not quite
// the spelling in the database: its clients switch on these exact strings.
func extraTypeOf(kind domain.ExtraKind) string {
	switch kind {
	case domain.ExtraTrailer:
		return "Trailer"
	case domain.ExtraBehindTheScenes:
		return "BehindTheScenes"
	case domain.ExtraDeleted:
		return "DeletedScene"
	case domain.ExtraFeaturette:
		return "Featurette"
	case domain.ExtraInterview:
		return "Interview"
	case domain.ExtraScene:
		return "Scene"
	case domain.ExtraShort:
		return "Short"
	default:
		return "Clip"
	}
}

// specialFeatures is everything that belongs to this item.
func (s *Server) specialFeatures(w http.ResponseWriter, r *http.Request) {
	s.extrasOf(w, r, false)
}

// localTrailers is the trailers among them, which is the row a client draws
// behind its play button.
func (s *Server) localTrailers(w http.ResponseWriter, r *http.Request) {
	s.extrasOf(w, r, true)
}

func (s *Server) extrasOf(w http.ResponseWriter, r *http.Request, trailersOnly bool) {
	id, from, err := parseID(r.PathValue("item"))
	if err != nil || from != spaceItem {
		http.Error(w, "bad item", http.StatusBadRequest)
		return
	}
	// Through the scoped handle: an extra of a film in a library this device
	// cannot see is as invisible as the film.
	if _, err := s.db(r.Context()).Item(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such item", http.StatusNotFound)
		return
	} else if err != nil {
		s.fail(w, "read item", err)
		return
	}

	extras, err := s.db(r.Context()).Extras(r.Context(), id)
	if err != nil {
		s.fail(w, "read extras", err)
		return
	}

	// An array, not a container: both of these routes answer with a bare
	// list, unlike every other listing on this surface.
	out := make([]item, 0, len(extras))
	for _, extra := range extras {
		if trailersOnly && extra.Extra != domain.ExtraTrailer {
			continue
		}
		out = append(out, s.renderExtra(extra))
	}
	writeJSON(w, http.StatusOK, out)
}

// renderExtra is one extra as a client reads it.
func (s *Server) renderExtra(extra domain.Item) item {
	out := s.render(extra, related{}, domain.Progress{})
	out.Type = extraTypeOf(extra.Extra)
	out.ExtraType = extraTypeOf(extra.Extra)
	out.MediaType = "Video"
	out.IsFolder = false
	return out
}

// countExtras is how many extras an item has, and how many of them are
// trailers.
//
// Read as rows rather than as a count query, because the two numbers come
// from the same handful of rows and a library item has a handful at most: two
// counts would be two statements for the same work.
func (s *Server) countExtras(r *http.Request, itemID int64) (features, trailers int) {
	extras, err := s.db(r.Context()).Extras(r.Context(), itemID)
	if err != nil {
		s.logger().Warn("could not count extras", "item", itemID, "error", err)
		return 0, 0
	}
	for _, extra := range extras {
		if extra.Extra == domain.ExtraTrailer {
			trailers++
			continue
		}
		features++
	}
	return features, trailers
}
