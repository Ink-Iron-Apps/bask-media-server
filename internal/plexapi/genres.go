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
	var out []string
	query := r.URL.Query()
	for _, name := range []string{"genre", "genre!"} {
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
	out.Directory = []entry{{
		Elem: "Directory", Type: "filter",
		Filter: "genre", FilterType: "string",
		Key: base + "/genre", Title: "Genre",
	}}
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
	section, ok := s.sectionOf(w, r)
	if !ok {
		return
	}
	kind := section.Kind.TopKind()
	genres, err := s.db(r.Context()).SectionGenres(r.Context(), section.ID, string(kind))
	if err != nil {
		s.fail(w, "list genres", err)
		return
	}

	base := "/library/sections/" + strconv.FormatInt(section.ID, 10)
	out := container{
		Identifier:          "com.plexapp.plugins.library",
		LibrarySectionID:    section.ID,
		LibrarySectionTitle: section.Title,
		Title1:              section.Title,
		Title2:              "Genre",
		ViewGroup:           "genre",
	}
	for _, genre := range genres {
		out.Directory = append(out.Directory, entry{
			Elem:  "Directory",
			Key:   strconv.FormatInt(genre.ID, 10),
			Title: genre.Name,
			// How many things carry it, which is what a client draws beside
			// the name so somebody can see the shelf is worth opening.
			Size:    genre.Held,
			FastKey: base + "/all?genre=" + url.QueryEscape(genre.Name),
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
