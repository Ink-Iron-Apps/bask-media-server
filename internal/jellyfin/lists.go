package jellyfin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// Playlists and collections in Jellyfin's shape.
//
// Jellyfin presents both as items — a playlist is a `Playlist`, a collection
// is a `BoxSet` — so a client browses into one the same way it browses into a
// show. They are separate tables here with ids in spaces of their own, and
// this file is where those two facts are reconciled.

/* ---------- rendering ---------- */

// playlistItem is one playlist as a row a client can open.
func (s *Server) playlistItem(p domain.Playlist) item {
	return item{
		ID:          playlistID(p.ID),
		ServerID:    s.serverID(),
		Name:        p.Title,
		SortName:    store.SortTitle(p.Title),
		Type:        "Playlist",
		MediaType:   playlistMediaType(p.Kind),
		IsFolder:    true,
		ChildCount:  p.Entries,
		DateCreated: p.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// playlistMediaType is what a client's Playlists tab filters on. A list of
// both is left unset rather than guessed at: a client that asked for audio
// playlists is better served by not being offered one than by being offered
// one that turns out to be half films.
func playlistMediaType(kind domain.PlaylistKind) string {
	switch kind {
	case domain.PlaylistAudio:
		return "Audio"
	case domain.PlaylistVideo:
		return "Video"
	default:
		return ""
	}
}

// collectionItem is one collection as a row.
func (s *Server) collectionItem(c domain.Collection) item {
	out := item{
		ID:          collectionID(c.ID),
		ServerID:    s.serverID(),
		Name:        c.Title,
		SortName:    c.SortTitle,
		Type:        "BoxSet",
		IsFolder:    true,
		Overview:    c.Summary,
		ChildCount:  c.Held,
		DateCreated: c.CreatedAt.UTC().Format(time.RFC3339),
	}
	if c.PosterKey != "" {
		out.ImageTags = map[string]string{"Primary": shortTag(c.PosterKey)}
	}
	return out
}

/* ---------- playlists ---------- */

// playlists lists the asking account's own, which is all there are: a
// playlist is never shared.
func (s *Server) playlists(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	lists, err := s.db(r.Context()).Playlists(r.Context(), user.ID)
	if err != nil {
		s.fail(w, "list playlists", err)
		return
	}
	out := make([]item, 0, len(lists))
	for _, list := range lists {
		out = append(out, s.playlistItem(list))
	}
	writeJSON(w, http.StatusOK, itemsReply{Items: out, TotalRecordCount: len(out)})
}

// createPlaylist makes one, optionally with something already in it, which is
// what "add to a new playlist" sends.
func (s *Server) createPlaylist(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var body struct {
		Name  string   `json:"Name"`
		Ids   []string `json:"Ids"`
		Items []string `json:"ItemIdList"`
	}
	if !readBody(w, r, &body) {
		return
	}
	// Clients spell the same field three ways depending on their age, and
	// the query string is a fourth for the ones that send no body at all.
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = strings.TrimSpace(r.URL.Query().Get("Name"))
	}
	wanted := append(append([]string{}, body.Ids...), body.Items...)
	wanted = append(wanted, splitIDs(r.URL.Query().Get("Ids"))...)

	made, err := s.db(r.Context()).CreatePlaylist(r.Context(), user.ID, name)
	if err != nil {
		s.fail(w, "create playlist", err)
		return
	}
	if ids := itemIDs(wanted); len(ids) > 0 {
		if _, err := s.db(r.Context()).AddToPlaylist(r.Context(), user.ID, made.ID, ids); err != nil {
			s.fail(w, "fill playlist", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"Id": playlistID(made.ID)})
}

// playlistItems is what is in one, in order, each entry carrying its own id
// so a client can remove exactly the row it is looking at.
func (s *Server) playlistItems(w http.ResponseWriter, r *http.Request) {
	id, ok := s.playlistFrom(w, r)
	if !ok {
		return
	}
	page := store.Page{Start: intQuery(r, "StartIndex"), Size: intQuery(r, "Limit")}
	s.playlistItemsFor(w, r, id, page)
}

// playlistItemsFor is the same answer for a playlist reached by its own route
// or browsed into as a parent.
func (s *Server) playlistItemsFor(w http.ResponseWriter, r *http.Request, id int64, page store.Page) {
	user := userFrom(r.Context())
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	entries, total, err := s.db(r.Context()).PlaylistEntries(r.Context(), user.ID, id, page)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such playlist", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "read playlist", err)
		return
	}

	items := make([]domain.Item, 0, len(entries))
	for _, entry := range entries {
		items = append(items, entry.Item)
	}
	rel := s.relatives(r.Context(), items, user)
	progress := s.progressFor(r, user, items)

	out := make([]item, 0, len(entries))
	for _, entry := range entries {
		row := s.render(entry.Item, rel, progress[entry.Item.ID])
		// The entry's id, not the item's. The same track twice is two
		// entries, and a client removing "the track" would otherwise be
		// asking the server to choose which one.
		row.PlaylistItemID = strconv.FormatInt(entry.ID, 10)
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, itemsReply{Items: out, TotalRecordCount: total, StartIndex: page.Start})
}

func (s *Server) addToPlaylist(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	id, ok := s.playlistFrom(w, r)
	if !ok {
		return
	}
	ids := itemIDs(splitIDs(r.URL.Query().Get("Ids")))
	if len(ids) == 0 {
		var body struct {
			Ids []string `json:"Ids"`
		}
		if !readBody(w, r, &body) {
			return
		}
		ids = itemIDs(body.Ids)
	}

	if _, err := s.db(r.Context()).AddToPlaylist(r.Context(), user.ID, id, ids); errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such playlist", http.StatusNotFound)
		return
	} else if err != nil {
		s.fail(w, "add to playlist", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeFromPlaylist(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	id, ok := s.playlistFrom(w, r)
	if !ok {
		return
	}

	// Entry ids, which this server hands out as plain numbers rather than in
	// an id space of their own: they are only ever meaningful inside one
	// playlist and a client never shows one.
	var entries []int64
	for _, raw := range splitIDs(r.URL.Query().Get("EntryIds")) {
		if entry, err := strconv.ParseInt(raw, 10, 64); err == nil {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		http.Error(w, "no entries named", http.StatusBadRequest)
		return
	}

	if _, err := s.db(r.Context()).RemoveFromPlaylist(r.Context(), user.ID, id, entries); errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such playlist", http.StatusNotFound)
		return
	} else if err != nil {
		s.fail(w, "remove from playlist", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) movePlaylistItem(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	id, ok := s.playlistFrom(w, r)
	if !ok {
		return
	}
	entry, err := strconv.ParseInt(r.PathValue("entry"), 10, 64)
	if err != nil {
		http.Error(w, "bad entry", http.StatusBadRequest)
		return
	}
	to, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		http.Error(w, "bad index", http.StatusBadRequest)
		return
	}

	if err := s.db(r.Context()).MovePlaylistEntry(r.Context(), user.ID, id, entry, to); errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such entry", http.StatusNotFound)
		return
	} else if err != nil {
		s.fail(w, "move entry", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// playlistFrom reads the id in the path and refuses anything that is not a
// playlist id, so a film id in a playlist route cannot reach a playlist.
func (s *Server) playlistFrom(w http.ResponseWriter, r *http.Request) (int64, bool) {
	if userFrom(r.Context()) == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return 0, false
	}
	id, from, err := parseID(r.PathValue("playlist"))
	if err != nil || from != spacePlaylist {
		http.Error(w, "no such playlist", http.StatusNotFound)
		return 0, false
	}
	return id, true
}

/* ---------- collections ---------- */

// collections lists the library's groupings, leaving out any whose every item
// is in a library this device cannot see.
func (s *Server) collections(w http.ResponseWriter, r *http.Request) ([]item, error) {
	all, err := s.db(r.Context()).Collections(r.Context())
	if err != nil {
		return nil, err
	}
	out := make([]item, 0, len(all))
	for _, c := range all {
		out = append(out, s.collectionItem(c))
	}
	return out, nil
}

// collectionChildren is what is in one.
func (s *Server) collectionChildren(w http.ResponseWriter, r *http.Request, id int64, page store.Page) {
	user := userFrom(r.Context())
	items, total, err := s.db(r.Context()).CollectionItems(r.Context(), id, page)
	if err != nil {
		s.fail(w, "read collection", err)
		return
	}
	s.writeItems(w, r, items, total, page, user)
}

/* ---------- shared ---------- */

// itemIDs reads a list of item ids, dropping anything that is not one. A
// client naming a library or a playlist where an item belongs is not an
// error worth refusing the whole request over.
func itemIDs(raw []string) []int64 {
	out := make([]int64, 0, len(raw))
	for _, value := range raw {
		id, from, err := parseID(value)
		if err != nil || from != spaceItem {
			continue
		}
		out = append(out, id)
	}
	return out
}

// readBody reads a small JSON body, tolerating an empty one: several of
// these routes carry everything in the query string and send nothing at all.
func readBody(w http.ResponseWriter, r *http.Request, into any) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return false
	}
	if len(raw) == 0 {
		return true
	}
	if err := json.Unmarshal(raw, into); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return false
	}
	return true
}

func splitIDs(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
