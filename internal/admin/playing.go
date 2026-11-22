package admin

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// Now playing: who is watching what, and the button that stops them.
//
// The register itself holds an item id and no titles, because it is written
// from the progress reports clients send every few seconds and a title lookup
// on that path would be a database read per viewer per tick for something
// nobody is looking at. The names are resolved here instead, once, when an
// administrator actually opens the page.

// streamView is one stream as the page draws it.
type streamView struct {
	Key     string `json:"key"`
	Surface string `json:"surface"`
	User    string `json:"user"`

	// Title and Subtitle are the item, named the way somebody scanning the
	// page needs it: the series and the episode, not the episode alone.
	Title    string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"`

	Client  string `json:"client,omitempty"`
	Device  string `json:"device,omitempty"`
	Address string `json:"address,omitempty"`

	Mode string `json:"mode"`
	Why  string `json:"why,omitempty"`

	PositionMS int64 `json:"positionMs"`
	DurationMS int64 `json:"durationMs"`
	Paused     bool  `json:"paused"`

	// ForSeconds is how long this stream has been going, which is the number
	// that tells an operator whether what they are looking at is a film
	// somebody is watching or a client that has been stuck since lunchtime.
	ForSeconds int64 `json:"forSeconds"`
}

// listPlaying is the Now Playing panel.
func (s *Server) listPlaying(w http.ResponseWriter, r *http.Request) {
	if s.Playing == nil {
		writeJSON(w, http.StatusOK, map[string]any{"playing": []streamView{}})
		return
	}
	live := s.Playing.List()
	out := make([]streamView, 0, len(live))
	// One cache for the page, because a household watching three episodes of
	// the same series resolves the same show three times otherwise.
	titles := map[int64]title{}
	for _, stream := range live {
		named := s.titleOf(r.Context(), titles, stream.ItemID)
		if named.main == "" {
			// The item has been removed since the stream started, which is
			// rare and not worth hiding the stream over.
			named.main = stream.Title
		}
		if named.main == "" {
			named.main = "Something that is no longer in the library"
		}
		out = append(out, streamView{
			Key: stream.Key, Surface: stream.Surface, User: stream.UserName,
			Title: named.main, Subtitle: named.sub,
			Client: stream.Client, Device: stream.Device, Address: stream.Address,
			Mode: stream.Mode, Why: stream.Why,
			PositionMS: stream.Position.Milliseconds(),
			DurationMS: stream.Duration.Milliseconds(),
			Paused:     stream.Paused,
			ForSeconds: int64(time.Since(stream.Since).Seconds()),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"playing": out})
}

// terminatePlaying stops a stream.
//
// A reason is asked for and passed through to the client, because the Plex
// clients will show it and because "stopped by the administrator" with no
// sentence after it is how a household ends up asking what broke.
func (s *Server) terminatePlaying(w http.ResponseWriter, r *http.Request) {
	if s.Playing == nil {
		writeError(w, http.StatusNotFound, "this server is not tracking playback")
		return
	}
	var body struct {
		Key    string `json:"key"`
		Reason string `json:"reason"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Key == "" {
		writeError(w, http.StatusBadRequest, "which stream")
		return
	}
	stopped, found := s.Playing.Terminate(body.Key, body.Reason)
	if !found {
		// Already over, which is the same outcome the administrator wanted.
		writeJSON(w, http.StatusOK, map[string]any{"stopped": false})
		return
	}
	s.logger().Info("stopped a stream",
		"surface", stopped.Surface, "user", stopped.UserName,
		"item", stopped.ItemID, "device", stopped.Device, "reason", stopped.Reason)
	writeJSON(w, http.StatusOK, map[string]any{"stopped": true})
}

// title is an item named for a list: the line and the line under it.
type title struct {
	main string
	sub  string
}

// titleOf names an item the way a person reads it.
//
// An episode on its own is "Episode 4", which says nothing. The series and
// the numbering are what make the line useful, and they are two reads up the
// parent chain — cached for the page, because a household watches one series
// at a time.
func (s *Server) titleOf(ctx context.Context, cache map[int64]title, id int64) title {
	if found, ok := cache[id]; ok {
		return found
	}
	named := title{}
	item, err := s.DB.Item(ctx, id)
	if err != nil {
		cache[id] = named
		return named
	}
	named.main = item.Title

	switch item.Kind {
	case domain.KindEpisode:
		named.main = item.Title
		if show, season := s.lineage(ctx, item); show != "" {
			named.sub = named.main
			named.main = show
			if season > 0 && item.Index > 0 {
				named.sub = fmt.Sprintf("S%02dE%02d · %s", season, item.Index, item.Title)
			}
		}
	case domain.KindTrack:
		if album, _ := s.lineage(ctx, item); album != "" {
			named.sub = album
		}
	default:
		if item.Year > 0 {
			named.sub = strconv.Itoa(item.Year)
		}
	}
	cache[id] = named
	return named
}

// lineage is the name two levels up and the number one level up: the series
// and the season for an episode, the album for a track.
func (s *Server) lineage(ctx context.Context, item *domain.Item) (name string, index int) {
	if item.ParentID == nil {
		return "", 0
	}
	parent, err := s.DB.Item(ctx, *item.ParentID)
	if err != nil {
		return "", 0
	}
	if parent.ParentID == nil {
		return parent.Title, parent.Index
	}
	grandparent, err := s.DB.Item(ctx, *parent.ParentID)
	if err != nil {
		return parent.Title, parent.Index
	}
	return grandparent.Title, parent.Index
}

// playingCount is how many streams there are, for the status line. Written as
// a function so the overview can call it whether or not there is a register.
func (s *Server) playingCount() int {
	if s.Playing == nil {
		return 0
	}
	return s.Playing.Count()
}
