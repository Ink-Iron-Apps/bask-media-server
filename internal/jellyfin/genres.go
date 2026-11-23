package jellyfin

import (
	"errors"
	"net/http"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/domain"
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

// wordsWanted reads the free-word filter off a request.
//
// Jellyfin's Tags parameter covers what BMS keeps as two kinds, so a client
// asking for one word is asking about either: a label or a tag of that name
// both count. Returned per kind so the store can combine them with OR within
// each and AND across, which here means a thing matches if it carries the
// word as either kind.
func wordsWanted(r *http.Request) []store.WordFilter {
	values := splitList(r.URL.Query().Get("Tags"))
	if len(values) == 0 {
		return nil
	}
	// Both kinds in one filter, so a thing matches if it carries the word as
	// either. Two filters would instead require it to carry the word twice,
	// once as each, which nothing does.
	return []store.WordFilter{{
		Kinds:  []store.TagKind{store.TagLabel, store.TagTag},
		Values: values,
	}}
}

// peopleWanted reads Jellyfin's credit filter off a request.
//
// Jellyfin's Person parameter does not say which role it means: a client
// asking for a name wants everything that name worked on, whether they
// directed it or appeared in it. So it becomes one filter over every role,
// which matches somebody who did any of them — four separate filters would
// instead require them to have done all four.
func peopleWanted(r *http.Request) []store.CreditFilter {
	names := splitList(r.URL.Query().Get("Person"))
	if len(names) == 0 {
		return nil
	}
	return []store.CreditFilter{{Roles: domain.CreditRoles(), Names: names}}
}

// studioWanted reads Jellyfin's studio filter.
func studioWanted(r *http.Request) string {
	for _, name := range splitList(r.URL.Query().Get("Studios")) {
		return name
	}
	return ""
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
	Studios         []string `json:"Studios"`
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
	out := filtersReply{
		Genres: []string{}, OfficialRatings: []string{}, Tags: []string{},
		Years: []int{}, Studios: []string{},
	}
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

	// Tags and labels both fill Jellyfin's Tags list, for the same reason
	// the item's own field does: there is one free-word field on this wire
	// and both kinds are free words.
	for _, word := range []store.TagKind{store.TagLabel, store.TagTag} {
		words, err := s.db(r.Context()).SectionTags(r.Context(), section.ID, string(kind), word)
		if err != nil {
			s.fail(w, "list "+string(word)+"s", err)
			return
		}
		for _, one := range words {
			out.Tags = append(out.Tags, one.Name)
		}
	}

	// Studios, which a client's filter sheet offers beside the genres.
	if studios, err := s.db(r.Context()).StudioNames(r.Context(), section.ID, string(kind)); err != nil {
		s.fail(w, "list studios", err)
		return
	} else {
		for _, studio := range studios {
			out.Studios = append(out.Studios, studio.Name)
		}
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

// sortWanted reads Jellyfin's sort parameters.
//
// Jellyfin names its orders differently from Plex — SortName, DateCreated,
// ProductionYear, Runtime — so they are translated here rather than letting
// the store learn a second vocabulary. A name this server has no order for
// falls back to the default, because a client asking for an order BMS does
// not have still wants its list.
func sortWanted(r *http.Request) (store.SortKey, bool) {
	query := r.URL.Query()
	desc := strings.EqualFold(strings.TrimSpace(query.Get("SortOrder")), "Descending")

	// The first name that means something: a client sends a list, in
	// preference order, and the rest are tie-breaks this server already
	// applies itself.
	for _, name := range splitList(query.Get("SortBy")) {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "sortname", "name", "album", "albumartist":
			return store.SortTitleKey, desc
		case "datecreated", "dateadded":
			return store.SortAdded, desc
		case "productionyear", "premieredate", "startdate":
			return store.SortYear, desc
		case "runtime":
			return store.SortDuration, desc
		case "officialrating", "criticrating":
			return store.SortRating, desc
		}
	}
	return store.SortTitleKey, desc
}
