package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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

	// Rules is the question a smart collection asks, and null for one
	// somebody filled by hand. Unreadable says the stored rules could not be
	// parsed, so it holds nothing until they are replaced — the page says so
	// rather than showing an ordinary empty collection.
	Rules      *domain.Smart `json:"rules"`
	Unreadable bool          `json:"unreadable,omitempty"`
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
		out = append(out, collectionView{
			ID: c.ID, Title: c.Title, Summary: c.Summary, Held: c.Held,
			Rules: c.Smart, Unreadable: c.Unreadable,
		})
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
	// The administrator's own handle, so a watch rule in a smart collection
	// is answered by somebody: "unwatched" has no meaning without an account
	// to have not watched it, and the page says whose view this is.
	db := s.viewing(r)

	found, err := db.AnyCollection(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such collection")
		return
	}
	if err != nil {
		s.fail(w, "read collection", err)
		return
	}

	items, _, err := db.CollectionItems(r.Context(), id, store.Page{Size: 500})
	if err != nil {
		s.fail(w, "read collection", err)
		return
	}
	named, err := s.libraryNames(r)
	if err != nil {
		s.fail(w, "list sections", err)
		return
	}

	view := collectionView{
		ID: found.ID, Title: found.Title, Summary: found.Summary, Held: found.Held,
		Rules: found.Smart, Unreadable: found.Unreadable,
	}
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

		// Raw, because three things have to be told apart and two of them
		// are the same in Go: no rules key at all (leave it as it is), a
		// rules key holding null (make it hand-picked again), and a rule set
		// (make it smart). A *domain.Smart cannot say which of the first two
		// arrived.
		Rules json.RawMessage `json:"rules"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Title == nil && body.Summary == nil && body.Items == nil && body.Rules == nil {
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
	// Rules before items, so one request can make a collection hand-picked
	// again and fill it in the same breath.
	if body.Rules != nil {
		rules, err := readRules(body.Rules)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.DB.SetCollectionRules(r.Context(), id, rules); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusNotFound, "no such collection")
				return
			}
			// Every other failure here is a rule set the server will not
			// have, which is the client's to fix and not a fault.
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if body.Items != nil {
		err := s.DB.SetCollectionItems(r.Context(), id, *body.Items)
		if errors.Is(err, store.ErrSmartCollection) {
			writeError(w, http.StatusConflict,
				"this collection is filled by its rules — remove them to pick films by hand")
			return
		}
		if err != nil {
			s.fail(w, "set collection", err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// readRules turns the rules field of a request into a rule set: nil for an
// explicit null, which is how a collection goes back to being hand-picked.
func readRules(raw json.RawMessage) (*domain.Smart, error) {
	if string(bytes.TrimSpace(raw)) == "null" {
		return nil, nil
	}
	var rules domain.Smart
	if err := json.Unmarshal(raw, &rules); err != nil {
		return nil, fmt.Errorf("those rules are not a rule set: %w", err)
	}
	if err := store.ValidateSmart(rules); err != nil {
		return nil, err
	}
	return &rules, nil
}

// vocabulary tells the editor what it may offer: the fields, how each one may
// be compared, and the orders a collection can be shown in.
//
// Served rather than written into the page, because the server is what has to
// be able to answer the question. An editor carrying its own copy agrees with
// this one until somebody adds a field, and then offers a rule that will not
// save.
func (s *Server) vocabulary(w http.ResponseWriter, r *http.Request) {
	fields := store.SmartFields()

	// Genre is the one field whose choices the library decides rather than
	// the server: the words come from a provider and from the files
	// themselves, so the editor is handed the ones actually in use. Offered
	// as suggestions and not as the only allowed values — a rule may name a
	// genre nothing holds yet, and the shelf fills when something does.
	genres, err := s.viewing(r).GenreNames(r.Context())
	if err != nil {
		s.fail(w, "list genres", err)
		return
	}
	for i := range fields {
		if fields[i].Field == domain.SmartGenre {
			fields[i].Choices = genres
		}
	}

	writeJSON(w, http.StatusOK, struct {
		Fields  []store.SmartFieldInfo `json:"fields"`
		Sorts   []domain.SmartSort     `json:"sorts"`
		Matches []domain.SmartMatch    `json:"matches"`
	}{fields, store.SmartSorts(), store.SmartMatches()})
}

// previewRules answers a rule set without storing it, so somebody can see
// what their question matches before committing to it.
//
// A rule set the server will not have comes back as 400 with the reason,
// which is the same answer saving it would give: the editor can show one
// message for both.
func (s *Server) previewRules(w http.ResponseWriter, r *http.Request) {
	var rules domain.Smart
	if !decode(w, r, &rules) {
		return
	}

	db := s.viewing(r)
	items, held, err := db.SmartPreview(r.Context(), rules, store.Page{Size: previewSize})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.fail(w, "preview rules", err)
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	named, err := s.libraryNames(r)
	if err != nil {
		s.fail(w, "list sections", err)
		return
	}

	view := collectionView{Held: held, Rules: &rules}
	for _, item := range items {
		view.Items = append(view.Items, collectionItem{
			ID: item.ID, Title: item.Title, Year: item.Year,
			Kind: string(item.Kind), Library: named[item.SectionID],
		})
	}
	writeJSON(w, http.StatusOK, view)
}

// previewSize is how much of the answer the editor is shown. Enough to see
// that the rules do what somebody meant, and short of sending a library down
// a wire on every keystroke — the count beside it is the whole answer.
const previewSize = 60

// viewing is the administrator's own handle: unrestricted, because this
// interface is, and carrying their account so watch-state rules mean
// something. The page says whose watch state it is showing.
func (s *Server) viewing(r *http.Request) *store.DB {
	if user := sessionUser(r.Context()); user != nil {
		return s.DB.Viewer(user.ID)
	}
	return s.DB
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
