package plexapi

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/artwork"
	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/serve"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// Browsing a library by who made what, and the pictures of the people.
//
// Same shape as the word filters beside them: a menu row per role, and a
// route per role listing the names in use with a count. Offered only where
// the library has credits at all, which is to say only where somebody asked
// for them in that library's settings.

func (s *Server) directorValues(w http.ResponseWriter, r *http.Request) {
	s.peopleValuesOf(w, r, domain.CreditDirector)
}

func (s *Server) writerValues(w http.ResponseWriter, r *http.Request) {
	s.peopleValuesOf(w, r, domain.CreditWriter)
}

func (s *Server) producerValues(w http.ResponseWriter, r *http.Request) {
	s.peopleValuesOf(w, r, domain.CreditProducer)
}

func (s *Server) actorValues(w http.ResponseWriter, r *http.Request) {
	s.peopleValuesOf(w, r, domain.CreditActor)
}

func (s *Server) peopleValuesOf(w http.ResponseWriter, r *http.Request, role domain.CreditRole) {
	section, ok := s.sectionOf(w, r)
	if !ok {
		return
	}
	top := string(section.Kind.TopKind())
	names, err := s.db(r.Context()).SectionPeople(r.Context(), section.ID, top, role)
	if err != nil {
		s.fail(w, "list "+string(role)+"s", err)
		return
	}

	base := "/library/sections/" + strconv.FormatInt(section.ID, 10)
	out := container{
		Identifier:          "com.plexapp.plugins.library",
		LibrarySectionID:    section.ID,
		LibrarySectionTitle: section.Title,
		Title1:              section.Title,
		Title2:              roleLabel(role),
		ViewGroup:           string(role),
	}
	for _, person := range names {
		out.Directory = append(out.Directory, entry{
			Elem: "Directory",
			Key:  strconv.FormatInt(person.ID, 10), Title: person.Name, Size: person.Held,
			FastKey: base + "/all?" + string(role) + "=" + url.QueryEscape(person.Name),
		})
	}
	out.Size = len(out.Directory)
	out.TotalSize = out.Size
	write(w, r, out)
}

// studioValues lists who made what in one library.
//
// A studio has no row of its own — it is a column on the item — so the keys
// here are the names themselves. A client only ever hands one back as a
// filter value, which takes a name.
func (s *Server) studioValues(w http.ResponseWriter, r *http.Request) {
	section, ok := s.sectionOf(w, r)
	if !ok {
		return
	}
	top := string(section.Kind.TopKind())
	studios, err := s.db(r.Context()).StudioNames(r.Context(), section.ID, top)
	if err != nil {
		s.fail(w, "list studios", err)
		return
	}

	base := "/library/sections/" + strconv.FormatInt(section.ID, 10)
	out := container{
		Identifier:          "com.plexapp.plugins.library",
		LibrarySectionID:    section.ID,
		LibrarySectionTitle: section.Title,
		Title1:              section.Title,
		Title2:              "Studio",
		ViewGroup:           "studio",
	}
	for _, studio := range studios {
		out.Directory = append(out.Directory, entry{
			Elem: "Directory",
			Key:  studio.Name, Title: studio.Name, Size: studio.Held,
			FastKey: base + "/all?studio=" + url.QueryEscape(studio.Name),
		})
	}
	out.Size = len(out.Directory)
	out.TotalSize = out.Size
	write(w, r, out)
}

// personThumb serves a photograph out of the artwork cache.
//
// A route of its own rather than the item image route, because a person is
// not an item: the key is the cache key, which is what the credit carries.
func (s *Server) personThumb(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" || s.Artwork == nil {
		http.Error(w, "no image", http.StatusNotFound)
		return
	}
	width, height := min(intQuery(r, "width"), 1000), min(intQuery(r, "height"), 1000)
	if err := serve.Image(w, r, s.Artwork, key, width, height); err != nil {
		if errors.Is(err, artwork.ErrNotCached) {
			http.Error(w, "no image", http.StatusNotFound)
			return
		}
		s.fail(w, "serve a photograph", err)
	}
}

// roleLabel is what a client's menu calls one role.
func roleLabel(role domain.CreditRole) string {
	switch role {
	case domain.CreditDirector:
		return "Director"
	case domain.CreditWriter:
		return "Writer"
	case domain.CreditProducer:
		return "Producer"
	default:
		return "Actor"
	}
}

// creditsWanted reads one role's filter off a request.
//
// Names rather than ids, resolved by the store the same way a genre's are:
// a client hands back whatever the menu gave it, and the menu gives a row id
// while BMS's own pages give a name.
func creditsWanted(r *http.Request, role domain.CreditRole) []string {
	var out []string
	for _, value := range strings.Split(r.URL.Query().Get(string(role)), ",") {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

// allLeaves is every episode of a show in the order it should be watched.
//
// The specials go where they belong rather than all of them before the
// pilot, which is what season-then-episode does to a show whose Christmas
// episodes went out between series. See [store.DB.EpisodeOrder] for which
// answer wins when the sources disagree.
func (s *Server) allLeaves(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("key"), 10, 64)
	if err != nil {
		http.Error(w, "bad key", http.StatusBadRequest)
		return
	}
	show, err := s.db(r.Context()).Item(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such item", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "read item", err)
		return
	}

	episodes, err := s.db(r.Context()).EpisodeOrder(r.Context(), id)
	if err != nil {
		s.fail(w, "list episodes", err)
		return
	}
	section, err := s.db(r.Context()).Section(r.Context(), show.SectionID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, "read section", err)
		return
	}

	out := container{
		Identifier:          "com.plexapp.plugins.library",
		LibrarySectionID:    show.SectionID,
		LibrarySectionTitle: sectionTitle(section),
		Title1:              sectionTitle(section),
		Title2:              show.Title,
		ViewGroup:           "episode",
		MediaTagPrefix:      "/system/bundle/media/flags/",
	}
	s.appendItems(r.Context(), &out, section, episodes, extrasFrom(r))
	write(w, r, out)
}

// sectionTitle is a library's name, or nothing when it has gone.
func sectionTitle(section *domain.Section) string {
	if section == nil {
		return ""
	}
	return section.Title
}
