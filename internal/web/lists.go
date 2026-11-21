package web

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// The star, the lists somebody made, and the library's own groupings, for the
// interface people watch in.
//
// Everything here reads through the scoped handle, so a browser signed in
// with a narrowed token sees exactly what the app on the same device sees.

/* ---------- favourites ---------- */

func (s *Server) favourites(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	marked, total, err := s.db(r.Context()).Favourites(r.Context(), user.ID, pageFrom(r))
	if err != nil {
		s.fail(w, "list favourites", err)
		return
	}
	views, err := s.views(r.Context(), marked, user.ID)
	if err != nil {
		s.fail(w, "read progress", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": views, "total": total})
}

// setFavourite is the star, pressed and unpressed, at one path by method.
func (s *Server) setFavourite(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	favourite := r.Method == http.MethodPost
	if err := s.db(r.Context()).SetFavourite(r.Context(), user.ID, id, favourite); err != nil {
		s.fail(w, "set favourite", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"favourite": favourite})
}

/* ---------- playlists ---------- */

type playlistView struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	Kind    string `json:"kind"`
	Entries int    `json:"entries"`
}

func (s *Server) playlists(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	lists, err := s.db(r.Context()).Playlists(r.Context(), user.ID)
	if err != nil {
		s.fail(w, "list playlists", err)
		return
	}
	out := make([]playlistView, 0, len(lists))
	for _, list := range lists {
		out = append(out, playlistView{
			ID: list.ID, Title: list.Title, Kind: string(list.Kind), Entries: list.Entries,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"playlists": out})
}

func (s *Server) createPlaylist(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	var body struct {
		Title string  `json:"title"`
		Items []int64 `json:"items"`
	}
	if !decode(w, r, &body) {
		return
	}
	made, err := s.db(r.Context()).CreatePlaylist(r.Context(), user.ID, body.Title)
	if err != nil {
		s.fail(w, "create playlist", err)
		return
	}
	if len(body.Items) > 0 {
		if _, err := s.db(r.Context()).AddToPlaylist(r.Context(), user.ID, made.ID, body.Items); err != nil {
			s.fail(w, "fill playlist", err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, playlistView{ID: made.ID, Title: made.Title, Kind: string(made.Kind)})
}

// playlist is one list and what is in it, each line carrying its own id.
func (s *Server) playlist(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	found, err := s.db(r.Context()).Playlist(r.Context(), user.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such playlist")
		return
	}
	if err != nil {
		s.fail(w, "read playlist", err)
		return
	}

	entries, total, err := s.db(r.Context()).PlaylistEntries(r.Context(), user.ID, id, pageFrom(r))
	if err != nil {
		s.fail(w, "read playlist", err)
		return
	}
	items := make([]domain.Item, 0, len(entries))
	for _, entry := range entries {
		items = append(items, entry.Item)
	}
	views, err := s.views(r.Context(), items, user.ID)
	if err != nil {
		s.fail(w, "read progress", err)
		return
	}

	// The entry id beside the item, because the same track twice is two
	// lines and removing "the track" would be ambiguous.
	lines := make([]map[string]any, 0, len(views))
	for i, view := range views {
		lines = append(lines, map[string]any{"entry": entries[i].ID, "item": view})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"playlist": playlistView{ID: found.ID, Title: found.Title, Kind: string(found.Kind), Entries: found.Entries},
		"lines":    lines,
		"total":    total,
	})
}

func (s *Server) addToPlaylist(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	var body struct {
		Items []int64 `json:"items"`
	}
	if !decode(w, r, &body) {
		return
	}
	added, err := s.db(r.Context()).AddToPlaylist(r.Context(), user.ID, id, body.Items)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such playlist")
		return
	}
	if err != nil {
		s.fail(w, "add to playlist", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"added": added})
}

func (s *Server) removeFromPlaylist(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	entry, err := strconv.ParseInt(r.PathValue("entry"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "that is not an entry")
		return
	}
	if _, err := s.db(r.Context()).RemoveFromPlaylist(r.Context(), user.ID, id, []int64{entry}); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such playlist")
		return
	} else if err != nil {
		s.fail(w, "remove from playlist", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) movePlaylistEntry(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	entry, err := strconv.ParseInt(r.PathValue("entry"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "that is not an entry")
		return
	}
	var body struct {
		To int `json:"to"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := s.db(r.Context()).MovePlaylistEntry(r.Context(), user.ID, id, entry, body.To); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such entry")
		return
	} else if err != nil {
		s.fail(w, "move entry", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deletePlaylist(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	if err := s.db(r.Context()).DeletePlaylist(r.Context(), user.ID, id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such playlist")
		return
	} else if err != nil {
		s.fail(w, "delete playlist", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

/* ---------- collections ---------- */

type collectionView struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	Summary string `json:"summary,omitempty"`
	Held    int    `json:"held"`
}

func (s *Server) collections(w http.ResponseWriter, r *http.Request) {
	all, err := s.db(r.Context()).Collections(r.Context())
	if err != nil {
		s.fail(w, "list collections", err)
		return
	}
	out := make([]collectionView, 0, len(all))
	for _, c := range all {
		out = append(out, collectionView{ID: c.ID, Title: c.Title, Summary: c.Summary, Held: c.Held})
	}
	writeJSON(w, http.StatusOK, map[string]any{"collections": out})
}

func (s *Server) collection(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	found, err := s.db(r.Context()).Collection(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such collection")
		return
	}
	if err != nil {
		s.fail(w, "read collection", err)
		return
	}

	items, total, err := s.db(r.Context()).CollectionItems(r.Context(), id, pageFrom(r))
	if err != nil {
		s.fail(w, "read collection", err)
		return
	}
	views, err := s.views(r.Context(), items, user.ID)
	if err != nil {
		s.fail(w, "read progress", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"collection": collectionView{ID: found.ID, Title: found.Title, Summary: found.Summary, Held: found.Held},
		"items":      views,
		"total":      total,
	})
}

/* ---------- shared ---------- */

func pageFrom(r *http.Request) store.Page {
	return store.Page{
		Start: intQuery(r, "start"),
		Size:  intQuery(r, "size"),
	}
}
