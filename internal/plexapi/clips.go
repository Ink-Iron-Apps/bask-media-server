package plexapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// Trailers, featurettes and deleted scenes, in Plex's vocabulary.
//
// Plex calls them extras and renders each one as a Video of type "clip" with
// a subtype saying which sort it is — which is the same set of words the
// database stores, because both surfaces here were written against the two
// APIs rather than inventing a third spelling.

// clipSubtypes maps a sort of extra to the subtype a Plex client reads.
func clipSubtype(kind domain.ExtraKind) string {
	switch kind {
	case domain.ExtraTrailer:
		return "trailer"
	case domain.ExtraBehindTheScenes:
		return "behindTheScenes"
	case domain.ExtraDeleted:
		return "deleted"
	case domain.ExtraFeaturette:
		return "featurette"
	case domain.ExtraInterview:
		return "interview"
	case domain.ExtraScene:
		return "scene"
	case domain.ExtraShort:
		return "short"
	default:
		return "other"
	}
}

// extrasOf is /library/metadata/{key}/extras.
func (s *Server) extrasOf(w http.ResponseWriter, r *http.Request) {
	id, err := ratingKeyOf(r.PathValue("key"), "")
	if err != nil {
		http.Error(w, "bad key", http.StatusBadRequest)
		return
	}
	// Resolved through the scoped handle first: an extra of something this
	// device cannot see is as invisible as the thing itself.
	if _, err := s.db(r.Context()).Item(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such item", http.StatusNotFound)
		return
	} else if err != nil {
		s.fail(w, "read item", err)
		return
	}

	found, err := s.db(r.Context()).Extras(r.Context(), id)
	if err != nil {
		s.fail(w, "read extras", err)
		return
	}

	out := container{Identifier: "com.plexapp.plugins.library"}
	s.appendItems(r.Context(), &out, nil, found, extras{})
	// The type and subtype are Plex's words for what these rows are, and the
	// client draws the row off the subtype: a trailer goes behind the play
	// button, everything else goes in the extras shelf.
	//
	// Matched by rating key rather than by position, because the position a
	// row lands in is a decision appendItems makes and this would be the
	// place that breaks if it ever made a different one.
	sorts := make(map[string]domain.ExtraKind, len(found))
	for _, extra := range found {
		sorts[strconv.FormatInt(extra.ID, 10)] = extra.Extra
	}
	for i := range out.Metadata {
		out.Metadata[i].Elem = "Video"
		out.Metadata[i].Type = "clip"
		out.Metadata[i].Subtype = clipSubtype(sorts[out.Metadata[i].RatingKey])
	}
	write(w, r, out)
}
