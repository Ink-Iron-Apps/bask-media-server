package admin

import (
	"errors"
	"net/http"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/opensubtitles"
	"github.com/inknironapps/bask-media-server/internal/store"
	"github.com/inknironapps/bask-media-server/internal/subtitles"
)

// Finding a subtitle from the interface, for the film somebody is looking at.
//
// The same search a client can ask for, in the place an administrator already
// is when they notice a film has no subtitles — and the only place it can be
// done at all for a household whose clients do not offer it.

// offerView is one subtitle on offer, as the page lists it.
type offerView struct {
	Handle   string `json:"handle"`
	Language string `json:"language,omitempty"`
	Release  string `json:"release,omitempty"`
	Name     string `json:"name,omitempty"`

	// Exact means it was authored against this very file, which is the only
	// claim here that means it cannot drift out of time.
	Exact bool `json:"exact"`

	HearingImpaired bool `json:"hearingImpaired,omitempty"`
	Forced          bool `json:"forced,omitempty"`
	Downloads       int  `json:"downloads,omitempty"`
}

// findSubtitles offers what the service has for this item.
func (s *Server) findSubtitles(w http.ResponseWriter, r *http.Request) {
	item, part, ok := s.itemAndFile(w, r)
	if !ok {
		return
	}
	want := subtitles.Want{
		Language:        strings.TrimSpace(r.URL.Query().Get("language")),
		HearingImpaired: r.URL.Query().Get("hearingImpaired") == "1",
		Forced:          r.URL.Query().Get("forced") == "1",
	}

	found, err := s.Fetcher.Search(r.Context(), *item, *part, want)
	switch {
	case errors.Is(err, subtitles.ErrOff), errors.Is(err, opensubtitles.ErrNoKey):
		// 409 with the sentence, because this one is fixed by saving a key on
		// the settings page and the page can say so.
		writeError(w, http.StatusConflict, subtitles.ErrOff.Error())
		return
	case errors.Is(err, opensubtitles.ErrQuota):
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	case err != nil:
		s.fail(w, "search for subtitles", err)
		return
	}

	out := make([]offerView, 0, len(found))
	for _, candidate := range found {
		out = append(out, offerView{
			Handle: candidate.Handle, Language: candidate.Language,
			Release: candidate.Release, Name: candidate.Name, Exact: candidate.Exact,
			HearingImpaired: candidate.HearingImpaired, Forced: candidate.Forced,
			Downloads: candidate.Downloads,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"subtitles": out})
}

// fetchSubtitle downloads the one somebody picked.
func (s *Server) fetchSubtitle(w http.ResponseWriter, r *http.Request) {
	item, part, ok := s.itemAndFile(w, r)
	if !ok {
		return
	}
	var body struct {
		Handle          string `json:"handle"`
		Language        string `json:"language"`
		HearingImpaired bool   `json:"hearingImpaired"`
		Forced          bool   `json:"forced"`
	}
	if !decode(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Handle) == "" {
		writeError(w, http.StatusBadRequest, "which subtitle")
		return
	}

	saved, err := s.Fetcher.Fetch(r.Context(), *item, *part, body.Handle, subtitles.Want{
		Language: body.Language, HearingImpaired: body.HearingImpaired, Forced: body.Forced,
	})
	switch {
	case errors.Is(err, subtitles.ErrOff), errors.Is(err, opensubtitles.ErrNoKey):
		writeError(w, http.StatusConflict, subtitles.ErrOff.Error())
		return
	case errors.Is(err, opensubtitles.ErrQuota):
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	case errors.Is(err, subtitles.ErrNotWritable):
		// Worth saying plainly: nothing this server can do fixes it, and
		// whoever owns the folder can.
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.fail(w, "fetch a subtitle", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"language": saved.Language,
		"path":     saved.Path,
		"forced":   saved.Forced,
		"hearing":  saved.Hearing,
	})
}

// itemAndFile resolves the item in the path and the file behind it.
func (s *Server) itemAndFile(w http.ResponseWriter, r *http.Request) (*domain.Item, *domain.Part, bool) {
	if s.Fetcher == nil {
		writeError(w, http.StatusConflict, "this server cannot search for subtitles")
		return nil, nil, false
	}
	id, ok := idFrom(w, r)
	if !ok {
		return nil, nil, false
	}
	item, err := s.DB.Item(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such item")
		return nil, nil, false
	}
	if err != nil {
		s.fail(w, "read item", err)
		return nil, nil, false
	}
	if len(item.Parts) == 0 {
		writeError(w, http.StatusNotFound, "there is no file for that")
		return nil, nil, false
	}
	part := item.Parts[0]
	return item, &part, true
}
