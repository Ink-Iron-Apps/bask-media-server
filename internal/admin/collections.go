package admin

import (
	"errors"
	"net/http"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// Collections are the library's own groupings, so they are made here rather
// than by a viewer: "these four films are one story" is a fact about the
// films, and everybody who can see them sees the same grouping.
//
// Playlists and favourites are deliberately absent from this interface. They
// belong to an account, and an administrator reaching into somebody's
// playlists is the wrong shape even on a server they own.

type collectionView struct {
	ID      int64            `json:"id"`
	Title   string           `json:"title"`
	Summary string           `json:"summary"`
	Held    int              `json:"held"`
	Items   []collectionItem `json:"items,omitempty"`
}

type collectionItem struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	Year    int    `json:"year"`
	Kind    string `json:"kind"`
	Library string `json:"library"`
}

func (s *Server) listCollections(w http.ResponseWriter, r *http.Request) {
	all, err := s.DB.EveryCollection(r.Context())
	if err != nil {
		s.fail(w, "list collections", err)
		return
	}
	out := make([]collectionView, 0, len(all))
	for _, c := range all {
		out = append(out, collectionView{ID: c.ID, Title: c.Title, Summary: c.Summary, Held: c.Held})
	}
	writeJSON(w, http.StatusOK, out)
}

// readCollection is the editor's view: the grouping and everything in it, in
// the order it was arranged.
func (s *Server) readCollection(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	found, err := s.DB.AnyCollection(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such collection")
		return
	}
	if err != nil {
		s.fail(w, "read collection", err)
		return
	}

	items, _, err := s.DB.CollectionItems(r.Context(), id, store.Page{Size: 500})
	if err != nil {
		s.fail(w, "read collection", err)
		return
	}
	named, err := s.libraryNames(r)
	if err != nil {
		s.fail(w, "list sections", err)
		return
	}

	view := collectionView{ID: found.ID, Title: found.Title, Summary: found.Summary, Held: found.Held}
	for _, item := range items {
		view.Items = append(view.Items, collectionItem{
			ID: item.ID, Title: item.Title, Year: item.Year,
			Kind: string(item.Kind), Library: named[item.SectionID],
		})
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) createCollection(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	}
	if !decode(w, r, &body) {
		return
	}
	made, err := s.DB.CreateCollection(r.Context(), body.Title, body.Summary)
	if err != nil {
		// A name is the one thing a collection cannot do without, and the
		// titles are unique, so both failures are the client's to fix.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, collectionView{ID: made.ID, Title: made.Title, Summary: made.Summary})
}

func (s *Server) updateCollection(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	var body struct {
		Title   *string  `json:"title"`
		Summary *string  `json:"summary"`
		Items   *[]int64 `json:"items"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Title == nil && body.Summary == nil && body.Items == nil {
		writeError(w, http.StatusBadRequest, "nothing to change")
		return
	}

	found, err := s.DB.AnyCollection(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such collection")
		return
	}
	if err != nil {
		s.fail(w, "read collection", err)
		return
	}

	if body.Title != nil || body.Summary != nil {
		title, summary := found.Title, found.Summary
		if body.Title != nil {
			title = *body.Title
		}
		if body.Summary != nil {
			summary = *body.Summary
		}
		if err := s.DB.UpdateCollection(r.Context(), id, title, summary); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if body.Items != nil {
		if err := s.DB.SetCollectionItems(r.Context(), id, *body.Items); err != nil {
			s.fail(w, "set collection", err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteCollection(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	err := s.DB.DeleteCollection(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such collection")
		return
	}
	if err != nil {
		s.fail(w, "delete collection", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// findItems is the search behind the collection editor's "add something" box.
//
// Only the things a collection is made of: a season or a track in one would
// be a grouping nobody asked for, and leaving them out of the search is
// cheaper than explaining why they cannot be added.
func (s *Server) findItems(w http.ResponseWriter, r *http.Request) {
	term := strings.TrimSpace(r.URL.Query().Get("q"))
	if term == "" {
		writeJSON(w, http.StatusOK, []collectionItem{})
		return
	}

	found, _, err := s.DB.Search(r.Context(), term, store.Page{Size: 50})
	if err != nil {
		s.fail(w, "search", err)
		return
	}
	named, err := s.libraryNames(r)
	if err != nil {
		s.fail(w, "list sections", err)
		return
	}

	out := make([]collectionItem, 0, len(found))
	for _, item := range found {
		switch item.Kind {
		case domain.KindMovie, domain.KindShow, domain.KindAlbum, domain.KindArtist, domain.KindPhotoAlbum:
		default:
			continue
		}
		out = append(out, collectionItem{
			ID: item.ID, Title: item.Title, Year: item.Year,
			Kind: string(item.Kind), Library: named[item.SectionID],
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// libraryNames maps section ids to titles, so a search result can say which
// library it came from — two films with the same name in different libraries
// are otherwise indistinguishable in a picker.
func (s *Server) libraryNames(r *http.Request) (map[int64]string, error) {
	sections, err := s.DB.Sections(r.Context())
	if err != nil {
		return nil, err
	}
	named := make(map[int64]string, len(sections))
	for _, section := range sections {
		named[section.ID] = section.Title
	}
	return named, nil
}
