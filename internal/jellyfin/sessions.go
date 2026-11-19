package jellyfin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// report is what a client sends while something plays.
type report struct {
	ItemID        string `json:"ItemId"`
	MediaSourceID string `json:"MediaSourceId"`
	PositionTicks int64  `json:"PositionTicks"`
	IsPaused      bool   `json:"IsPaused"`
	PlaySessionID string `json:"PlaySessionId"`
}

// playing records where a viewer is.
//
// Sent when playback starts and every few seconds after, which is how
// stopping on one device and picking up on another works. The same row the
// Plex surface writes and the web interface writes: one position per account
// per item, whatever was playing it.
func (s *Server) playing(w http.ResponseWriter, r *http.Request) {
	s.record(w, r, false)
}

// stopped is the last report, and the one that matters most: it is what
// lands when somebody turns the television off mid-episode.
func (s *Server) stopped(w http.ResponseWriter, r *http.Request) {
	s.record(w, r, true)
}

func (s *Server) record(w http.ResponseWriter, r *http.Request, final bool) {
	user := userFrom(r.Context())
	var body report
	raw, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
	}

	id, _, err := parseID(body.ItemID)
	if err != nil {
		// A client reporting on something this server does not have is not a
		// fault worth refusing: there is simply nothing to write.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	item, err := s.DB.Item(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		s.fail(w, "read item", err)
		return
	}

	offset := durationOfTicks(body.PositionTicks)
	progress := domain.Progress{
		UserID:   user.ID,
		ItemID:   id,
		Offset:   offset,
		Duration: item.Duration,
	}
	// A stop near the end is a finished item, which is the one thing a
	// position alone cannot say: see domain.Finished for where the line is.
	if final && domain.Finished(offset, item.Duration) {
		progress.Watched = true
	}
	if err := s.DB.SaveProgress(r.Context(), progress); err != nil {
		s.fail(w, "save progress", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// markPlayed and markUnplayed are the viewer saying so by hand.
func (s *Server) markPlayed(w http.ResponseWriter, r *http.Request)   { s.setWatched(w, r, true) }
func (s *Server) markUnplayed(w http.ResponseWriter, r *http.Request) { s.setWatched(w, r, false) }

func (s *Server) setWatched(w http.ResponseWriter, r *http.Request, watched bool) {
	user := userFrom(r.Context())
	id, _, err := parseID(r.PathValue("item"))
	if err != nil {
		http.Error(w, "bad item", http.StatusBadRequest)
		return
	}
	item, err := s.DB.Item(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such item", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "read item", err)
		return
	}

	progress := domain.Progress{UserID: user.ID, ItemID: id, Duration: item.Duration, Watched: watched}
	if watched {
		progress.Offset = item.Duration
	}
	if err := s.DB.SaveProgress(r.Context(), progress); err != nil {
		s.fail(w, "save progress", err)
		return
	}
	writeJSON(w, http.StatusOK, &userData{
		PlaybackPositionTicks: ticks(progress.Offset),
		Played:                watched,
		Key:                   itemID(id),
	})
}
