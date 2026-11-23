package plexapi

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/inknironapps/bask-media-server/internal/store"
)

// The three requests a Plex client makes to build the furniture around a
// library grid: what it can be narrowed by, what it can be sorted by, and
// which letters its titles start with.
//
// BMS answered none of them, so a client fell back to its own guesses — a
// sort menu offering orders this server then ignored, and an alphabet strip
// with nothing behind it. Answering them is the difference between a grid
// that can be navigated and one that can only be scrolled.

// sorts lists the orders this library can be listed in.
//
// Only the orders the listing actually honours. A menu that offers an order
// the server ignores is worse than a short menu: somebody picks it, the grid
// does not change, and nothing says why.
func (s *Server) sorts(w http.ResponseWriter, r *http.Request) {
	section, ok := s.sectionOf(w, r)
	if !ok {
		return
	}

	out := container{
		Identifier:          "com.plexapp.plugins.library",
		LibrarySectionID:    section.ID,
		LibrarySectionTitle: section.Title,
		Title1:              section.Title,
	}
	for i, option := range store.Sorts(section.Kind) {
		direction := "asc"
		if option.Descend {
			direction = "desc"
		}
		out.Directory = append(out.Directory, entry{
			Elem: "Directory", Type: "sort",
			Key: string(option.Key), Title: option.Title,
			// Plex marks the one a client should start with, and the
			// direction it reads best in.
			DefaultDirection: direction,
			Default:          i == 0,
			DescKey:          string(option.Key) + ":desc",
		})
	}
	out.Size = len(out.Directory)
	out.TotalSize = out.Size
	write(w, r, out)
}

// firstCharacter is the alphabet strip beside a long grid.
//
// Read from the same sorted column the listing is ordered by, so the strip
// and the grid agree about where "The Matrix" lives. A strip that jumped to
// the wrong place would be worse than none.
func (s *Server) firstCharacter(w http.ResponseWriter, r *http.Request) {
	section, ok := s.sectionOf(w, r)
	if !ok {
		return
	}
	kind := section.Kind.TopKind()
	letters, err := s.db(r.Context()).FirstLetters(r.Context(), section.ID, kind)
	if err != nil {
		s.fail(w, "list first letters", err)
		return
	}

	base := "/library/sections/" + strconv.FormatInt(section.ID, 10)
	out := container{
		Identifier:          "com.plexapp.plugins.library",
		LibrarySectionID:    section.ID,
		LibrarySectionTitle: section.Title,
		Title1:              section.Title,
		ViewGroup:           string(kind),
	}
	for _, letter := range letters {
		out.Directory = append(out.Directory, entry{
			Elem: "Directory",
			Key:  letter.Key, Title: letter.Title, Size: letter.Held,
			FastKey: base + "/all?titleSort%3E%3E=" + url.QueryEscape(letter.Key),
		})
	}
	out.Size = len(out.Directory)
	out.TotalSize = out.Size
	write(w, r, out)
}

// prefs answers Plex's per-library settings request with an empty list.
//
// BMS has per-library settings, but they are administrative — how often to
// scan, whether to ask a provider — and they are set in its own interface by
// whoever runs the server. None of them is a viewer's to change from a
// television, so none is offered here.
//
// Answered rather than left to 404 because a client asks on opening every
// library, and an endpoint that is reliably absent is a log line on every
// visit plus a code path nobody tests. An empty list is the honest answer:
// there is nothing here for you to set.
func (s *Server) prefs(w http.ResponseWriter, r *http.Request) {
	section, ok := s.sectionOf(w, r)
	if !ok {
		return
	}
	write(w, r, container{
		Identifier:          "com.plexapp.plugins.library",
		LibrarySectionID:    section.ID,
		LibrarySectionTitle: section.Title,
		Size:                0,
		TotalSize:           0,
	})
}
