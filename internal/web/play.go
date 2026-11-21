package web

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/ffprobe"
	"github.com/inknironapps/bask-media-server/internal/playback"
	"github.com/inknironapps/bask-media-server/internal/serve"
	"github.com/inknironapps/bask-media-server/internal/store"
	"github.com/inknironapps/bask-media-server/internal/transcode"
)

// Prober measures a file so the server can tell whether this client could
// play it untouched. An interface so the web package can be tested without
// ffprobe, and so a server without one simply direct plays everything.
type Prober interface {
	Probe(ctx context.Context, path string) (ffprobe.Info, error)
}

// Transcoder is the subset of the transcode manager this package uses.
type Transcoder interface {
	Start(req transcode.Request) (*transcode.Session, error)
	Get(id string) (*transcode.Session, bool)
	Stop(id string)
}

// play works out how this client should receive this file, and gets whatever
// is needed ready.
//
// The client says what it can decode and the server answers with a URL. The
// first answer it tries to give is always the file itself: a direct play
// costs this server nothing, keeps every frame the file had, and is what a
// media server should be doing almost all of the time. Transcoding happens
// when the client has said, in its own words, that it cannot play what is in
// the file.
func (s *Server) play(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ItemID       int64 `json:"itemId"`
		PartID       int64 `json:"partId"`
		StartMS      int64 `json:"startMs"`
		Capabilities struct {
			Containers []string `json:"containers"`
			Video      []string `json:"video"`
			Audio      []string `json:"audio"`
			MaxHeight  int      `json:"maxHeight"`
		} `json:"capabilities"`
	}
	if !decode(w, r, &body) {
		return
	}

	item, err := s.db(r.Context()).Item(r.Context(), body.ItemID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such item")
		return
	}
	if err != nil {
		s.fail(w, "read item", err)
		return
	}
	part, err := s.partOf(item.Parts, body.PartID)
	if err != nil {
		writeError(w, http.StatusNotFound, "there is no file for that")
		return
	}

	direct := map[string]any{
		"mode":    string(playback.ModeDirect),
		"label":   "Direct play",
		"why":     "the file plays as it is",
		"url":     "/web/stream/" + strconv.FormatInt(part.ID, 10),
		"startMs": body.StartMS,
	}

	// No probe, nothing to decide with: send the file. A server with no
	// ffprobe is a server that direct plays, which is what it did before any
	// of this existed.
	if s.Prober == nil {
		writeJSON(w, http.StatusOK, direct)
		return
	}
	info, err := s.Prober.Probe(r.Context(), part.Path)
	if err != nil {
		s.logger().Warn("could not probe before playing; sending the file as it is",
			"path", part.Path, "error", err)
		writeJSON(w, http.StatusOK, direct)
		return
	}

	caps := playback.Capabilities{
		Containers: body.Capabilities.Containers,
		Video:      body.Capabilities.Video,
		Audio:      body.Capabilities.Audio,
		MaxHeight:  body.Capabilities.MaxHeight,
	}
	plan := playback.Decide(info, part.Container, caps)
	if plan.Mode == playback.ModeDirect {
		writeJSON(w, http.StatusOK, direct)
		return
	}

	// Something has to be converted, and this server may not be able to:
	// no ffmpeg is a fact to report, not a spinner.
	if s.Transcoder == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"mode":  "unsupported",
			"label": "Cannot play",
			"why":   plan.Why + ", and this server has no ffmpeg to convert it",
			"url":   "/web/stream/" + strconv.FormatInt(part.ID, 10),
		})
		return
	}

	duration := item.Duration
	if duration <= 0 {
		duration = info.Duration
	}
	session, err := s.Transcoder.Start(transcode.Request{
		Key:      sessionKey(part.ID, plan),
		Path:     part.Path,
		Plan:     plan,
		Duration: duration,
		StartAt:  time.Duration(body.StartMS) * time.Millisecond,
	})
	if errors.Is(err, transcode.ErrBusy) {
		writeError(w, http.StatusServiceUnavailable,
			"this server is already converting as much as it can; try again when something finishes")
		return
	}
	if err != nil {
		s.fail(w, "start transcoding", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"mode":    string(plan.Mode),
		"label":   label(plan.Mode),
		"why":     plan.Why,
		"url":     "/web/hls/" + session.ID + "/index.m3u8",
		"session": session.ID,
		"startMs": body.StartMS,
		"hls":     true,
	})
}

// label says what is happening in the words these systems have always used,
// because somebody comparing this to what they had will be looking for them.
func label(mode playback.Mode) string {
	switch mode {
	case playback.ModeRemux:
		// Nothing is re-encoded: the packets are put in a different container.
		return "Direct stream"
	case playback.ModeTranscode:
		return "Transcoding"
	default:
		return "Direct play"
	}
}

func sessionKey(partID int64, plan playback.Plan) string {
	return strings.Join([]string{
		strconv.FormatInt(partID, 10),
		plan.VideoCodec, plan.AudioCodec,
		strconv.Itoa(plan.Height), strconv.Itoa(plan.Channels),
	}, "|")
}

// partOf picks the file to play: the one asked for, or the first, which is
// what an item with one version has.
func (s *Server) partOf(parts []domain.Part, wanted int64) (domain.Part, error) {
	for _, part := range parts {
		if wanted == 0 || part.ID == wanted {
			return part, nil
		}
	}
	return domain.Part{}, errors.New("no such part")
}

// hls serves a transcoding session's playlist and segments.
//
// Every request also says the client is still there, which is the only signal
// this server gets: see the watchdog in internal/transcode.
func (s *Server) hls(w http.ResponseWriter, r *http.Request) {
	if s.Transcoder == nil {
		http.Error(w, "no transcoder", http.StatusNotFound)
		return
	}
	session, found := s.Transcoder.Get(r.PathValue("session"))
	if !found {
		// A session that has been reaped is gone, and the client should ask
		// for a new one rather than retrying this URL for ever.
		http.Error(w, "that playback session has ended", http.StatusGone)
		return
	}
	session.Touch()

	name := r.PathValue("name")
	if name == "index.m3u8" {
		body, err := session.Playlist(r.Context())
		if err != nil {
			s.transcodeError(w, "playlist", err)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(body)
		return
	}

	path, err := session.Segment(r.Context(), name)
	if err != nil {
		s.transcodeError(w, "segment "+name, err)
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	// A segment's name identifies its content within a session, and a session
	// is thrown away when playback stops.
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if err := serve.File(w, r, path); err != nil {
		s.fail(w, "serve segment", err)
	}
}

func (s *Server) transcodeError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, transcode.ErrNoSegment):
		http.Error(w, "no such segment", http.StatusNotFound)
	case errors.Is(err, transcode.ErrNotReady):
		// 503 with a Retry-After, because the honest answer is "ask again":
		// the encoder is working on exactly this.
		w.Header().Set("Retry-After", "1")
		http.Error(w, "still converting", http.StatusServiceUnavailable)
	case errors.Is(err, context.Canceled):
		// The client went away mid-wait, which needs no response at all.
	default:
		s.logger().Warn("transcode request failed", "what", what, "error", err)
		http.Error(w, "could not convert that", http.StatusInternalServerError)
	}
}

// stopPlayback ends a transcoding session now rather than waiting for the
// watchdog: the client knows it has stopped, and saying so frees a core.
func (s *Server) stopPlayback(w http.ResponseWriter, r *http.Request) {
	if s.Transcoder != nil {
		s.Transcoder.Stop(r.PathValue("session"))
	}
	w.WriteHeader(http.StatusNoContent)
}

// vendor serves the one third-party file this page uses: see
// web/vendor/README.md for what it is and why it is compiled in.
func (s *Server) vendor(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.PathValue("name"))
	if name != "hls.light.min.js" {
		http.NotFound(w, r)
		return
	}
	s.asset("vendor/"+name, "text/javascript; charset=utf-8")(w, r)
}
