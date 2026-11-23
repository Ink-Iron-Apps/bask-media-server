package jellyfin

import (
	"errors"
	"net/http"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/store"
)

// genresWanted reads the genre filter off a request.
//
// Jellyfin separates multiple genres with a vertical bar and some clients
// send a comma instead, so both are accepted: the separator is punctuation,
// not a decision, and refusing one spelling would silently drop the filter
// and show the whole library.
//
// GenreIds is read as well, with the same values, because BMS hands a genre
// its own name as its id — a client that kept the id and one that kept the
// name are asking the same question.
func genresWanted(r *http.Request) []string {
	query := r.URL.Query()
	var out []string
	for _, name := range []string{"Genres", "GenreIds", "Genre"} {
		out = append(out, splitList(query.Get(name))...)
	}
	return out
}

func splitList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	fields := strings.FieldsFunc(value, func(r rune) bool { return r == '|' || r == ',' })
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if field = strings.TrimSpace(field); field != "" {
			out = append(out, field)
		}
	}
	return out
}

// filtersReply is Jellyfin's `/Items/Filters`: the values a client can
// narrow a library by, which is what fills its filter sheet.
//
// Every list is sent even when empty, because a client that reads a missing
// key and a client that reads an empty array both have to end up with no
// filter offered rather than an error.
type filtersReply struct {
	Genres          []string `json:"Genres"`
	OfficialRatings []string `json:"OfficialRatings"`
	Tags            []string `json:"Tags"`
	Years           []int    `json:"Years"`
}

// filters answers for one library.
//
// Tags are empty because BMS has none: a client asking is told there is
// nothing to filter by rather than being given a key that does not work.
func (s *Server) filters(w http.ResponseWriter, r *http.Request) {
	parent := strings.TrimSpace(r.URL.Query().Get("ParentId"))
	if parent == "" {
		parent = strings.TrimSpace(r.URL.Query().Get("parentId"))
	}
	out := filtersReply{Genres: []string{}, OfficialRatings: []string{}, Tags: []string{}, Years: []int{}}
	if parent == "" {
		// No library named: nothing to narrow, which is the honest answer to
		// a filter sheet that has not been opened on anything yet.
		writeJSON(w, http.StatusOK, out)
		return
	}

	id, from, err := parseID(parent)
	if err != nil || from != spaceLibrary {
		writeJSON(w, http.StatusOK, out)
		return
	}
	section, err := s.db(r.Context()).Section(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such library", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "read library", err)
		return
	}

	kind := section.Kind.TopKind()
	genres, err := s.db(r.Context()).SectionGenres(r.Context(), section.ID, string(kind))
	if err != nil {
		s.fail(w, "list genres", err)
		return
	}
	for _, genre := range genres {
		out.Genres = append(out.Genres, genre.Name)
	}

	facets, err := s.db(r.Context()).SectionFacets(r.Context(), section.ID, string(kind))
	if err != nil {
		s.fail(w, "list filters", err)
		return
	}
	out.OfficialRatings, out.Years = facets.Ratings, facets.Years
	if out.OfficialRatings == nil {
		out.OfficialRatings = []string{}
	}
	if out.Years == nil {
		out.Years = []int{}
	}
	writeJSON(w, http.StatusOK, out)
}

// genres answers Jellyfin's `/Genres`, which is the route a client reaches
// for when the aggregate filter endpoint is not there — and which some
// clients use to draw a genre shelf of its own.
//
// The rows are items in Jellyfin's vocabulary, because that is what the
// endpoint returns: a genre is a thing with a name and an id, and the id is
// the name for the reason given on the item's own GenreItems.
func (s *Server) genres(w http.ResponseWriter, r *http.Request) {
	parent := strings.TrimSpace(r.URL.Query().Get("ParentId"))
	if parent == "" {
		parent = strings.TrimSpace(r.URL.Query().Get("parentId"))
	}

	var named []store.GenreCount
	if parent == "" {
		// Every genre in the library, which is what a client asking without
		// a parent means. No counts, because they would be counts of
		// different kinds added together and mean nothing.
		names, err := s.db(r.Context()).GenreNames(r.Context())
		if err != nil {
			s.fail(w, "list genres", err)
			return
		}
		for _, name := range names {
			named = append(named, store.GenreCount{Name: name})
		}
	} else {
		id, from, err := parseID(parent)
		if err != nil || from != spaceLibrary {
			writeJSON(w, http.StatusOK, itemsReply{Items: []item{}})
			return
		}
		section, err := s.db(r.Context()).Section(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no such library", http.StatusNotFound)
			return
		}
		if err != nil {
			s.fail(w, "read library", err)
			return
		}
		named, err = s.db(r.Context()).SectionGenres(r.Context(), section.ID, string(section.Kind.TopKind()))
		if err != nil {
			s.fail(w, "list genres", err)
			return
		}
	}

	out := make([]item, 0, len(named))
	for _, genre := range named {
		out = append(out, item{
			ID:       genre.Name,
			ServerID: s.serverID(),
			Name:     genre.Name,
			Type:     "Genre",
			IsFolder: true,
			// What a client puts beside the name in its filter sheet, and
			// zero where the count would be across kinds and so meaningless.
			ChildCount: genre.Held,
		})
	}
	writeJSON(w, http.StatusOK, itemsReply{Items: out, TotalRecordCount: len(out)})
}
