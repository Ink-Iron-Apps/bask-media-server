package jellyfin

import (
	"errors"
	"net/http"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/opensubtitles"
	"github.com/inknironapps/bask-media-server/internal/store"
	"github.com/inknironapps/bask-media-server/internal/subtitles"
)

// Searching a provider for a subtitle, as a Jellyfin client asks.
//
// Jellyfin calls it a remote search: a list of what is out there, then a post
// naming the one to fetch. The same two steps as the Plex surface, because it
// is the same work underneath — the only differences are the spelling of the
// route and the shape of a row.

// remoteSubtitle is one subtitle on offer.
type remoteSubtitle struct {
	// ID is the handle to post back. Opaque: it names the provider and the
	// provider's own id for the file.
	ID string `json:"Id"`

	ProviderName string `json:"ProviderName"`
	Name         string `json:"Name"`
	Format       string `json:"Format,omitempty"`
	Author       string `json:"Author,omitempty"`
	Comment      string `json:"Comment,omitempty"`

	ThreeLetterISOLanguageName string `json:"ThreeLetterISOLanguageName,omitempty"`

	DownloadCount   int     `json:"DownloadCount,omitempty"`
	CommunityRating float64 `json:"CommunityRating,omitempty"`

	// IsHashMatch is the one claim on this row that means the subtitle was
	// authored against this very file, so it cannot drift out of time.
	IsHashMatch bool `json:"IsHashMatch"`

	IsForced        bool `json:"IsForced"`
	HearingImpaired bool `json:"HearingImpaired"`
}

// searchSubtitles offers what a provider has.
func (s *Server) searchSubtitles(w http.ResponseWriter, r *http.Request) {
	item, part, ok := s.itemAndFile(w, r)
	if !ok {
		return
	}
	// The language is in the path, the rest are query flags: that is how
	// Jellyfin spells this request.
	want := subtitles.Want{
		Language:        strings.TrimSpace(r.PathValue("language")),
		HearingImpaired: boolQuery(r, "IsHearingImpaired"),
		Forced:          boolQuery(r, "IsForced"),
	}

	found, err := s.Fetcher.Search(r.Context(), *item, *part, want)
	switch {
	case errors.Is(err, subtitles.ErrOff), errors.Is(err, opensubtitles.ErrNoKey):
		// An empty list rather than an error: searching is off, which is not
		// a fault of the client's.
		writeJSON(w, http.StatusOK, []remoteSubtitle{})
		return
	case errors.Is(err, opensubtitles.ErrQuota):
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	case err != nil:
		s.fail(w, "search for subtitles", err)
		return
	}

	out := make([]remoteSubtitle, 0, len(found))
	for _, candidate := range found {
		out = append(out, remoteSubtitle{
			ID:                         candidate.Handle,
			ProviderName:               "OpenSubtitles",
			Name:                       subtitleOfferName(candidate),
			Format:                     "srt",
			Comment:                    candidate.Release,
			ThreeLetterISOLanguageName: candidate.Language,
			DownloadCount:              candidate.Downloads,
			CommunityRating:            candidate.Rating,
			IsHashMatch:                candidate.Exact,
			IsForced:                   candidate.Forced,
			HearingImpaired:            candidate.HearingImpaired,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// fetchSubtitle downloads the one a client posted back.
func (s *Server) fetchSubtitle(w http.ResponseWriter, r *http.Request) {
	item, part, ok := s.itemAndFile(w, r)
	if !ok {
		return
	}
	handle := r.PathValue("subtitle")
	if handle == "" {
		http.Error(w, "which subtitle", http.StatusBadRequest)
		return
	}
	// The language is in the handle's company rather than in it, so it comes
	// from the query — and defaults inside the fetcher when it is missing.
	want := subtitles.Want{
		Language:        strings.TrimSpace(r.URL.Query().Get("Language")),
		HearingImpaired: boolQuery(r, "IsHearingImpaired"),
		Forced:          boolQuery(r, "IsForced"),
	}

	saved, err := s.Fetcher.Fetch(r.Context(), *item, *part, handle, want)
	switch {
	case errors.Is(err, subtitles.ErrOff), errors.Is(err, opensubtitles.ErrNoKey):
		http.Error(w, subtitles.ErrOff.Error(), http.StatusNotFound)
		return
	case errors.Is(err, opensubtitles.ErrQuota):
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	case errors.Is(err, subtitles.ErrNotWritable):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		s.fail(w, "fetch a subtitle", err)
		return
	}

	s.logger().Info("fetched a subtitle for a client",
		"item", item.ID, "language", saved.Language, "forced", saved.Forced)
	w.WriteHeader(http.StatusNoContent)
}

// itemAndFile resolves the item in the path and the file behind it.
func (s *Server) itemAndFile(w http.ResponseWriter, r *http.Request) (*domain.Item, *domain.Part, bool) {
	if s.Fetcher == nil {
		http.Error(w, "this server cannot search for subtitles", http.StatusNotFound)
		return nil, nil, false
	}
	id, from, err := parseID(r.PathValue("item"))
	if err != nil || from != spaceItem {
		http.Error(w, "bad item", http.StatusBadRequest)
		return nil, nil, false
	}
	item, err := s.db(r.Context()).Item(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such item", http.StatusNotFound)
		return nil, nil, false
	}
	if err != nil {
		s.fail(w, "read item", err)
		return nil, nil, false
	}
	if len(item.Parts) == 0 {
		http.Error(w, "there is no file for that", http.StatusNotFound)
		return nil, nil, false
	}
	part := item.Parts[0]
	return item, &part, true
}

// subtitleOfferName is the line a client shows for one offer.
func subtitleOfferName(candidate subtitles.Candidate) string {
	parts := []string{}
	if candidate.Exact {
		parts = append(parts, "Exact match")
	}
	switch {
	case candidate.Release != "":
		parts = append(parts, candidate.Release)
	case candidate.Name != "":
		parts = append(parts, candidate.Name)
	}
	if candidate.Forced {
		parts = append(parts, "forced")
	}
	if candidate.HearingImpaired {
		parts = append(parts, "SDH")
	}
	if len(parts) == 0 {
		return "OpenSubtitles"
	}
	return strings.Join(parts, " · ")
}
