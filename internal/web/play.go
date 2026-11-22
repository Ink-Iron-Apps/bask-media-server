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
	"github.com/inknironapps/bask-media-server/internal/nowplaying"
	"github.com/inknironapps/bask-media-server/internal/playback"
	"github.com/inknironapps/bask-media-server/internal/serve"
	"github.com/inknironapps/bask-media-server/internal/store"
	"github.com/inknironapps/bask-media-server/internal/subtitles"
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
		ItemID  int64 `json:"itemId"`
		PartID  int64 `json:"partId"`
		StartMS int64 `json:"startMs"`

		// BurnSubtitleID asks for a subtitle to be drawn into the picture,
		// which is how a browser watches a PGS track: it cannot draw one, and
		// the server can.
		BurnSubtitleID int64 `json:"burnSubtitleId"`

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
	// A stream somebody stopped does not restart by pressing play again
	// until the register has forgotten it.
	key := streamKey(r, item.ID)
	if s.refused(w, key) {
		return
	}
	if s.tooManyStreams(w, r, key) {
		return
	}

	direct := map[string]any{
		"mode":    string(playback.ModeDirect),
		"label":   "Direct play",
		"why":     "the file plays as it is",
		"url":     "/web/stream/" + strconv.FormatInt(part.ID, 10),
		"startMs": body.StartMS,
	}

	// A subtitle to burn in makes this a conversion whatever the file is, so
	// it is resolved before the direct-play answers below.
	burn := s.burnFrom(r, *item, body.BurnSubtitleID)
	if burn != nil && s.Transcoder == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"mode":  "unsupported",
			"label": "Cannot play",
			"why":   "that subtitle has to be drawn into the picture, and this server has no ffmpeg to do it",
			"url":   "/web/stream/" + strconv.FormatInt(part.ID, 10),
		})
		return
	}

	// No probe, nothing to decide with: send the file. A server with no
	// ffprobe is a server that direct plays, which is what it did before any
	// of this existed.
	if s.Prober == nil && burn == nil {
		s.playingDirect(r, key, item)
		writeJSON(w, http.StatusOK, direct)
		return
	}
	var info ffprobe.Info
	if s.Prober != nil {
		measured, err := s.Prober.Probe(r.Context(), part.Path)
		switch {
		case err != nil && burn == nil:
			s.logger().Warn("could not probe before playing; sending the file as it is",
				"path", part.Path, "error", err)
			s.playingDirect(r, key, item)
			writeJSON(w, http.StatusOK, direct)
			return
		case err != nil:
			// Burning in needs a conversion whether or not the file could be
			// measured, so an unmeasured file is converted with defaults
			// rather than sent with the subtitle missing.
			s.logger().Warn("could not probe before burning in a subtitle",
				"path", part.Path, "error", err)
		default:
			info = measured
		}
	}

	caps := playback.Capabilities{
		Containers: body.Capabilities.Containers,
		Video:      body.Capabilities.Video,
		Audio:      body.Capabilities.Audio,
		MaxHeight:  body.Capabilities.MaxHeight,
	}
	// What this account is allowed, folded into what the browser asked for:
	// a file above the cap is converted down rather than refused, which is
	// what somebody on a slow line actually wants.
	if cred := credFrom(r.Context()); cred != nil && cred.MaxBitRate > 0 {
		caps.MaxBitRate = cred.MaxBitRate
	}
	// A container that declares no bit rate is most of Matroska. The size
	// over the duration is the honest substitute, and it is what somebody
	// means by "this file is too big for this link" — the same substitute
	// the Jellyfin surface makes, for the same reason.
	if info.BitRate == 0 && part.Size > 0 && duration(item, info) > 0 {
		caps.BitRate = int64(float64(part.Size*8) / duration(item, info).Seconds())
	}
	plan := playback.Decide(info, part.Container, caps)
	if plan.Mode == playback.ModeDirect && burn == nil {
		s.playingDirect(r, key, item)
		writeJSON(w, http.StatusOK, direct)
		return
	}
	if burn != nil {
		// Nothing can be copied and drawn on at the same time.
		plan.Burn = burn
		plan.Mode, plan.VideoCodec = playback.ModeTranscode, "h264"
		if plan.AudioCodec == "" {
			plan.AudioCodec = "copy"
		}
		if plan.Why == "" {
			plan.Why = "a subtitle is being drawn into the picture"
		} else {
			plan.Why += ", and a subtitle is being drawn into the picture"
		}
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

	session, err := s.Transcoder.Start(transcode.Request{
		Key:      sessionKey(part.ID, plan),
		Path:     part.Path,
		Plan:     plan,
		Duration: duration(item, info),
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

	s.report(r, nowplaying.Stream{
		Key: key, ItemID: item.ID, Title: item.Title, Duration: duration(item, info),
		Mode: string(plan.Mode), Why: plan.Why, Session: session.ID,
	})

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

// duration is how long the thing is: what the scan measured, or what the
// probe just said.
func duration(item *domain.Item, info ffprobe.Info) time.Duration {
	if item.Duration > 0 {
		return item.Duration
	}
	return info.Duration
}

// playingDirect records a stream that is the file itself.
func (s *Server) playingDirect(r *http.Request, key string, item *domain.Item) {
	s.report(r, nowplaying.Stream{
		Key: key, ItemID: item.ID, Title: item.Title,
		Duration: item.Duration, Mode: string(playback.ModeDirect),
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
		// A viewer switching subtitle tracks is asking for a different
		// picture, and without this they would be handed the encoder that is
		// already drawing the other one.
		plan.Burn.Key(),
	}, "|")
}

// burnFrom is the subtitle this request asks to have drawn into the picture.
//
// Only ever by the viewer's own choice: the page offers it for the subtitles
// a browser cannot draw, which is every picture subtitle and the ASS tracks
// whose positioning a <track> element ignores.
func (s *Server) burnFrom(r *http.Request, item domain.Item, subtitleID int64) *playback.BurnIn {
	if subtitleID <= 0 {
		return nil
	}
	found, err := s.db(r.Context()).Subtitles(r.Context(), []int64{item.ID})
	if err != nil {
		s.logger().Warn("could not read subtitles to burn in", "item", item.ID, "error", err)
		return nil
	}
	list := found[item.ID]
	for _, sub := range list {
		if sub.ID == subtitleID {
			return subtitles.BurnIn(list, sub)
		}
	}
	return nil
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

// stopPlayback ends a stream now rather than waiting for the watchdog: the
// page knows it has stopped, and saying so frees a core and takes the viewer
// off the status page at the moment they stopped.
//
// Sent for a direct play too, where there is no session to end — hence the
// item in the query, which is what the register is keyed on. The literal
// "direct" in the path is the page saying there was nothing to convert.
func (s *Server) stopPlayback(w http.ResponseWriter, r *http.Request) {
	if session := r.PathValue("session"); s.Transcoder != nil && session != "direct" {
		s.Transcoder.Stop(session)
	}
	if item, err := strconv.ParseInt(r.URL.Query().Get("itemId"), 10, 64); err == nil && item > 0 {
		s.forget(streamKey(r, item))
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
