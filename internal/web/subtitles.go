package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/scan"
	"github.com/inknironapps/bask-media-server/internal/store"
	"github.com/inknironapps/bask-media-server/internal/subtitles"
)

// subtitleView is one entry in the player's subtitle menu.
type subtitleView struct {
	ID       int64  `json:"id"`
	Label    string `json:"label"`
	Language string `json:"language"`
	Forced   bool   `json:"forced,omitempty"`
	Hearing  bool   `json:"hearing,omitempty"`
	Default  bool   `json:"default,omitempty"`
	External bool   `json:"external,omitempty"`

	// Text says whether this page can show it at all. A PGS or VobSub track
	// is a picture of words: a player with its own renderer draws it from the
	// file, and a browser given a <track> element cannot.
	Text bool   `json:"text"`
	URL  string `json:"url,omitempty"`
}

func subtitleViews(subs []domain.Subtitle) []subtitleView {
	out := make([]subtitleView, 0, len(subs))
	for _, sub := range subs {
		view := subtitleView{
			ID:       sub.ID,
			Label:    subtitleLabel(sub),
			Language: sub.Language,
			Forced:   sub.Forced,
			Hearing:  sub.Hearing,
			Default:  sub.Default,
			External: sub.External,
			Text:     sub.Text(),
		}
		if view.Text {
			view.URL = "/web/subtitles/" + strconv.FormatInt(sub.ID, 10) + ".vtt"
		}
		out = append(out, view)
	}
	return out
}

// subtitleLabel is what somebody reads in the menu: the language first,
// because that is what they are looking for, then whatever distinguishes this
// track from the others in it.
func subtitleLabel(sub domain.Subtitle) string {
	parts := []string{scan.LanguageName(sub.Language)}
	if sub.Title != "" && !strings.EqualFold(sub.Title, parts[0]) {
		parts = append(parts, sub.Title)
	}
	if sub.Forced {
		parts = append(parts, "forced")
	}
	if sub.Hearing {
		parts = append(parts, "SDH")
	}
	return strings.Join(parts, " · ")
}

// subtitle hands the browser one subtitle as WebVTT, which is the only
// subtitle format a <track> element understands.
func (s *Server) subtitle(w http.ResponseWriter, r *http.Request) {
	name, _, _ := strings.Cut(r.PathValue("key"), ".")
	id, err := strconv.ParseInt(name, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "that is not a subtitle", http.StatusBadRequest)
		return
	}
	sub, err := s.db(r.Context()).Subtitle(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such subtitle", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "read subtitle", err)
		return
	}

	converter := s.Subtitles
	if converter == nil {
		// Without one configured, a sidecar .srt still becomes WebVTT, which
		// is most of the subtitles in most libraries.
		converter = &subtitles.Converter{}
	}
	body, _, err := converter.Text(r.Context(), *sub, subtitles.VTT)
	switch {
	case errors.Is(err, subtitles.ErrNotText):
		http.Error(w, "that subtitle is a picture, not text", http.StatusNotImplemented)
		return
	case errors.Is(err, subtitles.ErrNoConverter):
		http.Error(w, "this server has no ffmpeg to convert that subtitle", http.StatusNotImplemented)
		return
	case err != nil:
		s.fail(w, "convert subtitle", err)
		return
	}

	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}
