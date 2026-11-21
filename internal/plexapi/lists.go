package plexapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// Playlists, collections and the star, in Plex's vocabulary.
//
// Plex spells all three differently from Jellyfin and the differences are not
// cosmetic. A playlist is its own top-level thing at /playlists rather than
// an item type; a collection is a Directory of type "collection" under the
// library it belongs to; and Plex has no favourites at all — it has a ten
// point rating, and its clients send a ten when somebody presses the heart.
// That last one is the only place here where a judgement had to be made
// rather than a shape translated.

// plexRatingIsFavourite is where a ten point scale becomes a yes or a no.
//
// Plex's heart sends 10 and its clear sends 0, which is the whole of what its
// clients actually use the rating for. Six is the midpoint of the half-star
// scale a viewer could in principle drag to, so anything at or above it is
// kept as a favourite and anything below clears it. BMS has no rating of its
// own to lose precision from, so nothing is thrown away that was ever stored.
const plexRatingIsFavourite = 6.0

/* ---------- the star ---------- */

// rate is Plex's PUT /:/rate, which its clients send for the heart.
func (s *Server) rate(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	query := r.URL.Query()
	id, err := ratingKeyOf(query.Get("key"), query.Get("ratingKey"), query.Get("identifier"))
	if err != nil {
		http.Error(w, "bad ratingKey", http.StatusBadRequest)
		return
	}
	rating, err := strconv.ParseFloat(strings.TrimSpace(query.Get("rating")), 64)
	if err != nil {
		http.Error(w, "bad rating", http.StatusBadRequest)
		return
	}

	// Through the scoped handle, so rating something in a library this
	// device cannot see writes nothing.
	if err := s.db(r.Context()).SetFavourite(r.Context(), user.ID, id, rating >= plexRatingIsFavourite); err != nil {
		s.fail(w, "set favourite", err)
		return
	}
	write(w, r, container{})
}

/* ---------- playlists ---------- */

// playlists is the list, and also the one route that creates one, because
// Plex uses the same path with a different method.
func (s *Server) playlists(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodPost {
		s.createPlaylist(w, r)
		return
	}

	lists, err := s.db(r.Context()).Playlists(r.Context(), user.ID)
	if err != nil {
		s.fail(w, "list playlists", err)
		return
	}
	out := container{Identifier: "com.plexapp.plugins.library"}
	for _, list := range lists {
		out.Metadata = append(out.Metadata, playlistEntry(list))
	}
	out.Size = len(out.Metadata)
	write(w, r, out)
}

func playlistEntry(p domain.Playlist) entry {
	return entry{
		Elem:      "Playlist",
		RatingKey: strconv.FormatInt(p.ID, 10),
		Key:       "/playlists/" + strconv.FormatInt(p.ID, 10) + "/items",
		Type:      "playlist",
		// Plex calls it playlistType and its clients filter a music library's
		// playlist shelf on it, the same way Jellyfin's clients filter on
		// media type.
		Title:      p.Title,
		Summary:    "",
		LeafCount:  p.Entries,
		ChildCount: p.Entries,
		AddedAt:    millisOf(p.CreatedAt),
		UpdatedAt:  millisOf(p.UpdatedAt),
		PlaylistKind: map[domain.PlaylistKind]string{
			domain.PlaylistAudio: "audio",
			domain.PlaylistVideo: "video",
		}[p.Kind],
		Smart: false,
	}
}

func (s *Server) createPlaylist(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	query := r.URL.Query()

	made, err := s.db(r.Context()).CreatePlaylist(r.Context(), user.ID, query.Get("title"))
	if err != nil {
		s.fail(w, "create playlist", err)
		return
	}
	// Plex passes what goes in as a URI naming the items, which is the one
	// place its API is genuinely awkward: everything after the last slash is
	// a comma separated list of rating keys.
	if ids := ratingKeys(query.Get("uri")); len(ids) > 0 {
		if _, err := s.db(r.Context()).AddToPlaylist(r.Context(), user.ID, made.ID, ids); err != nil {
			s.fail(w, "fill playlist", err)
			return
		}
	}

	read, err := s.db(r.Context()).Playlist(r.Context(), user.ID, made.ID)
	if err != nil {
		s.fail(w, "read playlist", err)
		return
	}
	out := container{Identifier: "com.plexapp.plugins.library"}
	out.Metadata = append(out.Metadata, playlistEntry(*read))
	out.Size = 1
	write(w, r, out)
}

// playlistItems is what is in one, and the route that adds to it.
func (s *Server) playlistItems(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("playlist"), 10, 64)
	if err != nil {
		http.Error(w, "bad playlist", http.StatusBadRequest)
		return
	}

	if r.Method == http.MethodPut {
		ids := ratingKeys(r.URL.Query().Get("uri"))
		if _, err := s.db(r.Context()).AddToPlaylist(r.Context(), user.ID, id, ids); errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no such playlist", http.StatusNotFound)
			return
		} else if err != nil {
			s.fail(w, "add to playlist", err)
			return
		}
		write(w, r, container{})
		return
	}

	page := store.Page{Start: intHeader(r, "X-Plex-Container-Start", "X-Plex-Container-Start"), Size: intHeader(r, "X-Plex-Container-Size", "X-Plex-Container-Size")}
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
	out := container{
		Identifier: "com.plexapp.plugins.library",
		TotalSize:  total,
		Offset:     page.Start,
	}
	s.appendItems(r.Context(), &out, nil, items, extras{})
	// The entry's own id, so a client can remove this line rather than every
	// copy of the track. Plex calls it playlistItemID.
	for i := range out.Metadata {
		if i < len(entries) {
			out.Metadata[i].PlaylistItemID = strconv.FormatInt(entries[i].ID, 10)
		}
	}
	out.TotalSize = total
	write(w, r, out)
}

func (s *Server) removePlaylistItem(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("playlist"), 10, 64)
	if err != nil {
		http.Error(w, "bad playlist", http.StatusBadRequest)
		return
	}
	entry, err := strconv.ParseInt(r.PathValue("entry"), 10, 64)
	if err != nil {
		http.Error(w, "bad item", http.StatusBadRequest)
		return
	}

	if _, err := s.db(r.Context()).RemoveFromPlaylist(r.Context(), user.ID, id, []int64{entry}); errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such playlist", http.StatusNotFound)
		return
	} else if err != nil {
		s.fail(w, "remove from playlist", err)
		return
	}
	write(w, r, container{})
}

func (s *Server) deletePlaylist(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("playlist"), 10, 64)
	if err != nil {
		http.Error(w, "bad playlist", http.StatusBadRequest)
		return
	}
	if err := s.db(r.Context()).DeletePlaylist(r.Context(), user.ID, id); errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such playlist", http.StatusNotFound)
		return
	} else if err != nil {
		s.fail(w, "delete playlist", err)
		return
	}
	write(w, r, container{})
}

// ratingKeys reads the item ids out of the URI Plex sends when it adds
// things to a playlist.
//
// The shape is `server://<machine>/com.plexapp.plugins.library/library/
// metadata/12,13,14`, and the only part that matters is the list at the end.
// Anything that is not a number is dropped rather than refused: a client
// naming one item this server does not have should not lose the others.
func ratingKeys(uri string) []int64 {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return nil
	}
	if cut := strings.LastIndex(uri, "/"); cut >= 0 {
		uri = uri[cut+1:]
	}
	var out []int64
	for _, part := range strings.Split(uri, ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil && id > 0 {
			out = append(out, id)
		}
	}
	return out
}

/* ---------- collections ---------- */

// collections lists the library's groupings.
//
// Plex asks for them per library and BMS does not keep them per library — a
// collection can hold films from two of them — so every collection holding
// something from the asked-for library is returned. A client sees the
// grouping under each library it reaches into, which is what it would show
// for a Plex collection spanning nothing at all.
func (s *Server) collections(w http.ResponseWriter, r *http.Request) {
	section, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad section", http.StatusBadRequest)
		return
	}
	if _, err := s.db(r.Context()).Section(r.Context(), section); errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such section", http.StatusNotFound)
		return
	}

	all, err := s.db(r.Context()).Collections(r.Context())
	if err != nil {
		s.fail(w, "list collections", err)
		return
	}

	out := container{
		Identifier:       "com.plexapp.plugins.library",
		LibrarySectionID: section,
		ViewGroup:        "collection",
	}
	for _, c := range all {
		items, _, err := s.db(r.Context()).CollectionItems(r.Context(), c.ID, store.Page{Size: 500})
		if err != nil {
			s.fail(w, "read collection", err)
			return
		}
		held := 0
		for _, item := range items {
			if item.SectionID == section {
				held++
			}
		}
		if held == 0 {
			continue
		}
		out.Directory = append(out.Directory, collectionEntry(c, section, held))
	}
	out.Size = len(out.Directory)
	write(w, r, out)
}

func collectionEntry(c domain.Collection, section int64, held int) entry {
	return entry{
		Elem:             "Directory",
		RatingKey:        strconv.FormatInt(c.ID, 10),
		Key:              "/library/collections/" + strconv.FormatInt(c.ID, 10) + "/children",
		Type:             "collection",
		Title:            c.Title,
		Summary:          c.Summary,
		ChildCount:       held,
		LibrarySectionID: section,
		AddedAt:          millisOf(c.CreatedAt),
		UpdatedAt:        millisOf(c.UpdatedAt),
	}
}

// collectionChildren is what is in one.
func (s *Server) collectionChildren(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad collection", http.StatusBadRequest)
		return
	}
	found, err := s.db(r.Context()).Collection(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such collection", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "read collection", err)
		return
	}

	page := store.Page{Start: intHeader(r, "X-Plex-Container-Start", "X-Plex-Container-Start"), Size: intHeader(r, "X-Plex-Container-Size", "X-Plex-Container-Size")}
	items, total, err := s.db(r.Context()).CollectionItems(r.Context(), id, page)
	if err != nil {
		s.fail(w, "read collection", err)
		return
	}

	out := container{
		Identifier: "com.plexapp.plugins.library",
		Title1:     found.Title,
		Title2:     found.Title,
		TotalSize:  total,
		Offset:     page.Start,
	}
	s.appendItems(r.Context(), &out, nil, items, extras{})
	out.TotalSize = total
	write(w, r, out)
}
