package plexapi

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// genresWanted reads the genre filter off a request.
//
// Plex separates values with a comma, and a client that was handed a genre's
// id sends the id while one that was handed a name sends the name. Both
// reach the store, which resolves whichever arrived.
func genresWanted(r *http.Request) []string {
	return wordsWanted(r, store.TagGenre)
}

// wordsWanted reads one kind's filter off a request.
//
// Plex separates values with a comma, and a client that was handed a row's
// id sends the id while one that was handed a name sends the name. Both
// reach the store, which resolves whichever arrived.
func wordsWanted(r *http.Request, kind store.TagKind) []string {
	var out []string
	query := r.URL.Query()
	for _, name := range []string{string(kind), string(kind) + "!"} {
		for _, value := range strings.Split(query.Get(name), ",") {
			if value = strings.TrimSpace(value); value != "" {
				out = append(out, value)
			}
		}
	}
	return out
}

// filters lists what a library can be narrowed by.
//
// Plex's own answer is a long list covering decades, resolutions, studios and
// a dozen more; BMS offers the ones it can actually answer. A filter in this
// list that the listing then ignores would be a menu that does nothing, which
// is worse for somebody using it than a shorter menu.
func (s *Server) filters(w http.ResponseWriter, r *http.Request) {
	section, ok := s.sectionOf(w, r)
	if !ok {
		return
	}

	base := "/library/sections/" + strconv.FormatInt(section.ID, 10)
	out := container{
		Identifier:          "com.plexapp.plugins.library",
		LibrarySectionID:    section.ID,
		LibrarySectionTitle: section.Title,
		Title1:              section.Title,
	}
	// Only the kinds something in this library actually carries. A filter
	// row leading to an empty list of values is a menu entry that wastes a
	// tap and says nothing.
	top := string(section.Kind.TopKind())
	for _, kind := range store.TagKinds() {
		values, err := s.db(r.Context()).SectionTags(r.Context(), section.ID, top, kind)
		if err != nil {
			s.fail(w, "list "+string(kind)+"s", err)
			return
		}
		if len(values) == 0 {
			continue
		}
		out.Directory = append(out.Directory, entry{
			Elem: "Directory", Type: "filter",
			Filter: string(kind), FilterType: "string",
			Key: base + "/" + string(kind), Title: kind.Label(),
		})
	}
	out.Size = len(out.Directory)
	out.TotalSize = out.Size
	write(w, r, out)
}

// genreValues lists the genres in use in one library.
//
// The key is the genre's own row id, which is what Plex sends and what a
// client stores; fastKey is the listing it stands for, so a client that
// prefers to follow a link rather than build a query has one. Both lead to
// the same place, and the listing accepts the name as well, because BMS's own
// pages hand a name back.
func (s *Server) genreValues(w http.ResponseWriter, r *http.Request) {
	s.tagValuesOf(w, r, store.TagGenre)
}

func (s *Server) labelValues(w http.ResponseWriter, r *http.Request) {
	s.tagValuesOf(w, r, store.TagLabel)
}

func (s *Server) tagValues(w http.ResponseWriter, r *http.Request) {
	s.tagValuesOf(w, r, store.TagTag)
}

func (s *Server) moodValues(w http.ResponseWriter, r *http.Request) {
	s.tagValuesOf(w, r, store.TagMood)
}

func (s *Server) styleValues(w http.ResponseWriter, r *http.Request) {
	s.tagValuesOf(w, r, store.TagStyle)
}

// tagValuesOf lists one kind of word in use in one library.
func (s *Server) tagValuesOf(w http.ResponseWriter, r *http.Request, kind store.TagKind) {
	section, ok := s.sectionOf(w, r)
	if !ok {
		return
	}
	top := section.Kind.TopKind()
	genres, err := s.db(r.Context()).SectionTags(r.Context(), section.ID, string(top), kind)
	if err != nil {
		s.fail(w, "list "+string(kind)+"s", err)
		return
	}

	base := "/library/sections/" + strconv.FormatInt(section.ID, 10)
	out := container{
		Identifier:          "com.plexapp.plugins.library",
		LibrarySectionID:    section.ID,
		LibrarySectionTitle: section.Title,
		Title1:              section.Title,
		Title2:              kind.Label(),
		ViewGroup:           string(kind),
	}
	for _, genre := range genres {
		out.Directory = append(out.Directory, entry{
			Elem:  "Directory",
			Key:   strconv.FormatInt(genre.ID, 10),
			Title: genre.Name,
			// How many things carry it, which is what a client draws beside
			// the name so somebody can see the shelf is worth opening.
			Size:    genre.Held,
			FastKey: base + "/all?" + string(kind) + "=" + url.QueryEscape(genre.Name),
		})
	}
	out.Size = len(out.Directory)
	out.TotalSize = out.Size
	write(w, r, out)
}

// sectionOf reads the library a request names, answering the client itself
// when there is none.
func (s *Server) sectionOf(w http.ResponseWriter, r *http.Request) (*domain.Section, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad section", http.StatusBadRequest)
		return nil, false
	}
	section, err := s.db(r.Context()).Section(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such section", http.StatusNotFound)
		return nil, false
	}
	if err != nil {
		s.fail(w, "read section", err)
		return nil, false
	}
	return section, true
}
