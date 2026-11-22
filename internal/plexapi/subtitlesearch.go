package plexapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/opensubtitles"
	"github.com/inknironapps/bask-media-server/internal/store"
	"github.com/inknironapps/bask-media-server/internal/subtitles"
)

// Searching a provider for a subtitle this file does not have, in the shape
// Plex clients ask for it.
//
// Two routes at one path, told apart by method: a GET offers what is out
// there and a PUT fetches one. That is Plex's own arrangement, and it is the
// one Bask already speaks — its player asks for an exact match when a film
// has no subtitles at all, and takes nothing otherwise.

// searchSubtitles offers what a provider has for this item.
func (s *Server) searchSubtitles(w http.ResponseWriter, r *http.Request) {
	item, part, ok := s.itemAndFile(w, r)
	if !ok {
		return
	}
	want := wantFrom(r)

	found, err := s.Fetcher.Search(r.Context(), *item, *part, want)
	switch {
	case errors.Is(err, subtitles.ErrOff), errors.Is(err, opensubtitles.ErrNoKey):
		// Off rather than broken: an empty list, because a client asking is
		// not doing anything wrong and a 500 would read as a fault.
		write(w, r, container{Identifier: "com.plexapp.plugins.library"})
		return
	case errors.Is(err, opensubtitles.ErrQuota):
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	case err != nil:
		s.fail(w, "search for subtitles", err)
		return
	}

	// What this file already has, so a subtitle already fetched is marked
	// rather than offered again as if it were new.
	already := map[string]bool{}
	if existing, err := s.db(r.Context()).Subtitles(r.Context(), []int64{item.ID}); err == nil {
		for _, sub := range existing[item.ID] {
			already[strings.ToLower(sub.Language)] = true
		}
	}

	out := container{Identifier: "com.plexapp.plugins.library"}
	for position, candidate := range found {
		out.Stream = append(out.Stream, stream{
			// The id is a number because a client expects one; the handle it
			// has to send back is the key.
			ID:              int64(position + 1),
			StreamType:      streamSubtitle,
			Key:             candidate.Handle,
			Codec:           "srt",
			Language:        candidate.Language,
			LanguageCode:    candidate.Language,
			Title:           candidate.Release,
			DisplayTitle:    displayTitleOf(candidate),
			ExtendedTitle:   displayTitleOf(candidate),
			Forced:          candidate.Forced,
			HearingImpaired: candidate.HearingImpaired,
			ProviderTitle:   "OpenSubtitles",
			// A hash match is the only score worth anything, so it is the
			// only one this surface claims: the rest are ordered behind it.
			Score:        scoreOf(candidate),
			PerfectMatch: candidate.Exact,
			Downloaded:   already[strings.ToLower(candidate.Language)],
		})
	}
	out.Size = len(out.Stream)
	write(w, r, out)
}

// fetchSubtitle downloads the one a client chose and attaches it.
func (s *Server) fetchSubtitle(w http.ResponseWriter, r *http.Request) {
	item, part, ok := s.itemAndFile(w, r)
	if !ok {
		return
	}
	handle := strings.TrimSpace(r.URL.Query().Get("key"))
	if handle == "" {
		http.Error(w, "which subtitle", http.StatusBadRequest)
		return
	}

	saved, err := s.Fetcher.Fetch(r.Context(), *item, *part, handle, wantFrom(r))
	switch {
	case errors.Is(err, subtitles.ErrOff), errors.Is(err, opensubtitles.ErrNoKey):
		http.Error(w, subtitles.ErrOff.Error(), http.StatusNotFound)
		return
	case errors.Is(err, opensubtitles.ErrQuota):
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	case errors.Is(err, subtitles.ErrNotWritable):
		// Said plainly, because this one is fixable by whoever owns the
		// folder and nothing else here can fix it.
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		s.fail(w, "fetch a subtitle", err)
		return
	}

	s.logger().Info("fetched a subtitle for a client",
		"item", item.ID, "language", saved.Language, "forced", saved.Forced)
	// The client reads the new track off the item's metadata, which is where
	// it was going to look anyway: one container, no body worth parsing.
	write(w, r, container{Identifier: "com.plexapp.plugins.library", Size: 1})
}

// itemAndFile resolves the item in the path and the file behind it.
func (s *Server) itemAndFile(w http.ResponseWriter, r *http.Request) (*domain.Item, *domain.Part, bool) {
	if s.Fetcher == nil {
		http.Error(w, "this server cannot search for subtitles", http.StatusNotFound)
		return nil, nil, false
	}
	id, err := ratingKeyOf(r.PathValue("key"), "")
	if err != nil {
		http.Error(w, "bad key", http.StatusBadRequest)
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

// wantFrom reads what the client asked for.
func wantFrom(r *http.Request) subtitles.Want {
	q := r.URL.Query()
	return subtitles.Want{
		Language:        strings.TrimSpace(q.Get("language")),
		HearingImpaired: flagged(q.Get("hearingImpaired")),
		Forced:          flagged(q.Get("forced")),
	}
}

// flagged reads the 1-or-0 a Plex client sends for a boolean.
func flagged(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// displayTitleOf is the line a person chooses by.
func displayTitleOf(candidate subtitles.Candidate) string {
	parts := []string{}
	if candidate.Exact {
		// First, because it is the only thing on the row that means the
		// subtitle cannot drift out of time.
		parts = append(parts, "Exact match")
	}
	if candidate.Release != "" {
		parts = append(parts, candidate.Release)
	} else if candidate.Name != "" {
		parts = append(parts, candidate.Name)
	}
	if candidate.Forced {
		parts = append(parts, "forced")
	}
	if candidate.HearingImpaired {
		parts = append(parts, "SDH")
	}
	if candidate.Downloads > 0 {
		parts = append(parts, strconv.Itoa(candidate.Downloads)+" downloads")
	}
	if len(parts) == 0 {
		return "OpenSubtitles"
	}
	return strings.Join(parts, " · ")
}

// scoreOf is a number for a client that sorts by one.
//
// A hash match is 100 and everything else is below it, because the two are
// different kinds of claim: one is about this file, the other is about how
// many people liked a subtitle for some other release.
func scoreOf(candidate subtitles.Candidate) float64 {
	if candidate.Exact {
		return 100
	}
	if candidate.Rating > 0 {
		return candidate.Rating
	}
	return 0
}
