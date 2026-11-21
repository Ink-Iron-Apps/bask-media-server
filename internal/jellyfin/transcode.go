package jellyfin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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

// What this file is for.
//
// Everything else in this package hands over files. This is the one path
// where the server does work: a client says, in a device profile, what it can
// decode and how much bandwidth it will accept, and if the file does not fit
// inside that the answer is a converted stream rather than a refusal.
//
// The shape is deliberately narrow. Jellyfin's own transcoding vocabulary is
// enormous — a dozen URL forms, segment containers, subtitle burn-in options,
// tone mapping — and BMS implements the part a client actually follows: one
// HLS playlist whose URL carries the plan, and segments under it. The client
// is told that URL in PlaybackInfo and does not construct one itself, so the
// spelling is this server's to choose.

// Prober measures a file, so this server can tell whether a client could play
// it untouched. Nil means no probe, and no probe means direct play: a server
// without ffprobe behaves as it did before any of this existed.
type Prober interface {
	Probe(ctx context.Context, path string) (ffprobe.Info, error)
}

// Transcoder is the part of the transcode manager this presenter uses.
type Transcoder interface {
	Start(req transcode.Request) (*transcode.Session, error)
	Get(id string) (*transcode.Session, bool)
	Stop(id string)
	StopKey(key string)
}

/* ---------- what the client asked for ---------- */

// wish is a PlaybackInfo request: what to play, and what the client can do
// with it.
type wish struct {
	sourceID     string
	maxBitRate   int64
	startAt      time.Duration
	allowDirect  bool
	allowConvert bool
	profile      deviceProfile
	asked        bool // a device profile arrived at all
}

// deviceProfile is the part of Jellyfin's profile that decides anything here:
// which containers and codecs the client can play. Its transcoding profiles
// are not read, because what this server can produce is not negotiable — one
// HLS ladder of H.264 and AAC, which every client that asks for a transcode
// accepts.
type deviceProfile struct {
	MaxStreamingBitrate int64          `json:"MaxStreamingBitrate"`
	DirectPlayProfiles  []codecProfile `json:"DirectPlayProfiles"`
}

type codecProfile struct {
	Type       string `json:"Type"`
	Container  string `json:"Container"`
	VideoCodec string `json:"VideoCodec"`
	AudioCodec string `json:"AudioCodec"`
}

// wishFrom reads a PlaybackInfo request.
//
// Clients send the same fields twice, in the query string and in the body,
// and not always both: jellyfin-web puts them in the query, Bask sends both,
// and some put the profile in the body alone. The body wins where it says
// anything, because that is where the device profile is.
func wishFrom(r *http.Request) wish {
	out := wish{
		sourceID:     r.URL.Query().Get("MediaSourceId"),
		maxBitRate:   int64(intQuery(r, "MaxStreamingBitrate")),
		allowDirect:  true,
		allowConvert: true,
	}
	if ticks := int64(intQuery(r, "StartTimeTicks")); ticks > 0 {
		out.startAt = time.Duration(ticks) * 100 * time.Nanosecond
	}
	if value := r.URL.Query().Get("EnableDirectPlay"); value != "" {
		out.allowDirect = boolQuery(r, "EnableDirectPlay")
	}
	if value := r.URL.Query().Get("EnableTranscoding"); value != "" {
		out.allowConvert = boolQuery(r, "EnableTranscoding")
	}

	if r.Body == nil {
		return out
	}
	var body struct {
		MediaSourceID       string         `json:"MediaSourceId"`
		MaxStreamingBitrate int64          `json:"MaxStreamingBitrate"`
		StartTimeTicks      int64          `json:"StartTimeTicks"`
		EnableDirectPlay    *bool          `json:"EnableDirectPlay"`
		EnableTranscoding   *bool          `json:"EnableTranscoding"`
		DeviceProfile       *deviceProfile `json:"DeviceProfile"`
	}
	// A malformed or absent body is not an error: a GET has none, and the
	// query string already said enough to answer.
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&body); err != nil {
		return out
	}
	if body.MediaSourceID != "" {
		out.sourceID = body.MediaSourceID
	}
	if body.MaxStreamingBitrate > 0 {
		out.maxBitRate = body.MaxStreamingBitrate
	}
	if body.StartTimeTicks > 0 {
		out.startAt = time.Duration(body.StartTimeTicks) * 100 * time.Nanosecond
	}
	if body.EnableDirectPlay != nil {
		out.allowDirect = *body.EnableDirectPlay
	}
	if body.EnableTranscoding != nil {
		out.allowConvert = *body.EnableTranscoding
	}
	if body.DeviceProfile != nil {
		out.profile, out.asked = *body.DeviceProfile, true
		if out.maxBitRate <= 0 {
			out.maxBitRate = body.DeviceProfile.MaxStreamingBitrate
		}
	}
	return out
}

// capabilities turns the profile into what internal/playback decides with.
//
// An empty list means "anything", which is also what a client that sent no
// profile looks like — and is the right reading of both: a client that has
// not said what it cannot play is taken at its word.
func (w wish) capabilities(it domain.Item) playback.Capabilities {
	wanted := "Video"
	if mediaTypeOf(it.Kind) == "Audio" {
		wanted = "Audio"
	}
	caps := playback.Capabilities{MaxBitRate: w.maxBitRate}
	for _, entry := range w.profile.DirectPlayProfiles {
		if entry.Type != "" && !strings.EqualFold(entry.Type, wanted) {
			continue
		}
		caps.Containers = append(caps.Containers, csv(entry.Container)...)
		caps.Video = append(caps.Video, csv(entry.VideoCodec)...)
		caps.Audio = append(caps.Audio, csv(entry.AudioCodec)...)
	}
	return caps
}

func csv(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

/* ---------- the decision ---------- */

// negotiate works out how this client should receive this file.
//
// Direct play unless the client said something that rules it out. The order
// matters: a server that transcodes when it did not have to has spent a core
// on a file the client would have played.
func (s *Server) negotiate(ctx context.Context, it domain.Item, part domain.Part, want wish) playback.Plan {
	direct := playback.Plan{Mode: playback.ModeDirect, VideoCodec: "copy", AudioCodec: "copy", Why: "the file plays as it is"}
	switch {
	case s.Prober == nil, s.Transcoder == nil:
		// Nothing to decide with, or nothing to convert with.
		return direct
	case !want.asked && want.maxBitRate <= 0:
		// The client said nothing about itself. Asking ffprobe would cost a
		// subprocess to learn something no answer depends on.
		return direct
	case !want.allowConvert:
		return direct
	case mediaTypeOf(it.Kind) != "Video":
		// Music is handed over as the file. The manager produces HLS video
		// segments, which is not what an audio client asked for, and a track
		// is small enough that the file itself is the cheap answer anyway.
		return direct
	}

	info, err := s.Prober.Probe(ctx, part.Path)
	if err != nil {
		s.logger().Warn("could not probe before playing; offering the file as it is",
			"path", part.Path, "error", err)
		return direct
	}
	caps := want.capabilities(it)
	// A container that declares no bit rate is most of Matroska. The size
	// over the duration is the honest substitute, and it is what a viewer
	// means by "this file is too big for this link".
	if info.BitRate == 0 && part.Size > 0 && it.Duration > 0 {
		caps.BitRate = int64(float64(part.Size*8) / it.Duration.Seconds())
	}
	plan := playback.Decide(info, part.Container, caps)
	if plan.Mode == playback.ModeDirect && !want.allowDirect {
		// The client has refused the file without saying what is wrong with
		// it. Converting is the only remaining answer.
		plan.Mode = playback.ModeTranscode
		plan.VideoCodec, plan.AudioCodec = "h264", "aac"
		plan.Why = "this client asked not to be sent the file"
	}
	return plan
}

// negotiatedSource is one file as a client is told about it after a
// negotiation: either the file itself, or a stream that will be made.
func (s *Server) negotiatedSource(ctx context.Context, it domain.Item, part domain.Part, want wish, playSession string) mediaSource {
	source := s.mediaSource(it, part)
	source.TranscodingURL, source.TranscodingSubProtocol = "", ""

	plan := s.negotiate(ctx, it, part, want)
	if plan.Mode == playback.ModeDirect {
		return source
	}

	source.SupportsDirectPlay = false
	source.SupportsDirectStream = false
	source.SupportsTranscoding = true
	source.TranscodingSubProtocol = "hls"
	source.TranscodeReasons = jellyfinReasons(plan.Causes)
	source.TranscodingURL = transcodingURL(it, part, plan, want, playSession)
	return source
}

// jellyfinReasons names the causes the way the protocol does, because that is
// what a client shows when somebody asks why a film is being converted.
func jellyfinReasons(causes []playback.Cause) []string {
	out := make([]string, 0, len(causes))
	for _, cause := range causes {
		switch cause {
		case playback.CauseContainer:
			out = append(out, "ContainerNotSupported")
		case playback.CauseVideoCodec:
			out = append(out, "VideoCodecNotSupported")
		case playback.CauseAudioCodec:
			out = append(out, "AudioCodecNotSupported")
		case playback.CauseResolution:
			out = append(out, "VideoResolutionNotSupported")
		case playback.CauseChannels:
			out = append(out, "AudioChannelsNotSupported")
		case playback.CauseBitRate:
			out = append(out, "ContainerBitrateExceedsLimit")
		}
	}
	if len(out) == 0 {
		out = append(out, "DirectPlayError")
	}
	return out
}

/* ---------- the plan, carried in a URL ---------- */

// transcodingURL is where the converted stream lives.
//
// The plan travels in the query string rather than in a session this server
// remembers, so a playlist request is answerable on its own: no state to
// expire between the negotiation and the first segment, and a client that
// retries an hour later gets the same stream rather than a 410. The values
// are clamped when they are read back, because they arrive from outside.
func transcodingURL(it domain.Item, part domain.Part, plan playback.Plan, want wish, playSession string) string {
	query := url.Values{}
	query.Set("MediaSourceId", itemID(part.ID))
	query.Set("PlaySessionId", playSession)
	query.Set("VideoCodec", plan.VideoCodec)
	query.Set("AudioCodec", plan.AudioCodec)
	if plan.Height > 0 {
		query.Set("MaxHeight", strconv.Itoa(plan.Height))
	}
	if plan.Channels > 0 {
		query.Set("AudioChannels", strconv.Itoa(plan.Channels))
	}
	if plan.VideoBitRate > 0 {
		query.Set("VideoBitrate", strconv.FormatInt(plan.VideoBitRate, 10))
	}
	if want.startAt > 0 {
		query.Set("StartTimeTicks", strconv.FormatInt(int64(want.startAt/(100*time.Nanosecond)), 10))
	}
	if reasons := jellyfinReasons(plan.Causes); len(reasons) > 0 {
		query.Set("TranscodeReasons", strings.Join(reasons, ","))
	}
	// No api_key: the client appends its own, and an empty one here is a
	// parameter that reads as a token that is not valid.
	return "/Videos/" + itemID(it.ID) + "/master.m3u8?" + query.Encode()
}

// wirePlan reads a plan back out of a transcoding URL, clamped.
//
// Everything here came from a client, which has a token but no business
// asking this server to encode 16K at a gigabit. The clamps are the only
// place that is checked, so they are deliberately boring.
func wirePlan(r *http.Request) playback.Plan {
	plan := playback.Plan{
		VideoCodec:   codecOf(r.URL.Query().Get("VideoCodec"), "h264", "copy", "h264"),
		AudioCodec:   codecOf(r.URL.Query().Get("AudioCodec"), "aac", "copy", "aac", "mp3"),
		Height:       min(max(intQuery(r, "MaxHeight"), 0), 4320),
		Channels:     min(max(intQuery(r, "AudioChannels"), 0), 8),
		VideoBitRate: min(max(int64(intQuery(r, "VideoBitrate")), 0), 100_000_000),
	}
	plan.Mode = playback.ModeTranscode
	if plan.VideoCodec == "copy" && plan.AudioCodec == "copy" {
		plan.Mode = playback.ModeRemux
	}
	return plan
}

// codecOf keeps a codec name this server will actually pass to ffmpeg.
func codecOf(value, fallback string, allowed ...string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, name := range allowed {
		if value == name {
			return value
		}
	}
	return fallback
}

// newPlaySession is the id a client reports progress against and stops by.
//
// One per negotiation rather than one per item: two plays of the same film —
// a second device, or a seek that renegotiates — are two streams, and a
// shared id would have the first client's stop end the second client's film.
func newPlaySession(id int64) string {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Sprintf("%s-%d", itemID(id), time.Now().UnixNano())
	}
	return itemID(id) + "-" + hex.EncodeToString(nonce[:])
}

/* ---------- serving the stream ---------- */

// transcodeMaster is the URL a client is given. It names one stream, which is
// the next request.
//
// A master playlist rather than the variant itself because that is what every
// HLS client expects to be handed, and because the one thing it carries —
// BANDWIDTH — is what tells a player how much to buffer before it starts.
func (s *Server) transcodeMaster(w http.ResponseWriter, r *http.Request) {
	if s.Transcoder == nil {
		http.Error(w, "this server has no ffmpeg", http.StatusNotFound)
		return
	}
	if _, _, err := parseID(r.PathValue("item")); err != nil {
		http.Error(w, "bad item", http.StatusBadRequest)
		return
	}
	plan := wirePlan(r)
	bandwidth := plan.VideoBitRate
	if bandwidth <= 0 {
		bandwidth = 8_000_000
	}

	var body strings.Builder
	body.WriteString("#EXTM3U\n")
	fmt.Fprintf(&body, "#EXT-X-STREAM-INF:BANDWIDTH=%d", bandwidth)
	if plan.Height > 0 {
		// Width is not known here and a player only uses the pair as a hint,
		// so the height is written against a 16:9 width rather than guessed
		// from the file.
		fmt.Fprintf(&body, ",RESOLUTION=%dx%d", plan.Height*16/9, plan.Height)
	}
	body.WriteString("\n")
	// The whole query, which carries both the plan and the client's own token:
	// a relative URI keeps the path and drops the query, so it has to be
	// written out again here.
	body.WriteString("main.m3u8?" + r.URL.RawQuery + "\n")

	playlist(w, []byte(body.String()))
}

// transcodeVariant starts the conversion, if it is not already running, and
// hands over the playlist of what it will produce.
func (s *Server) transcodeVariant(w http.ResponseWriter, r *http.Request) {
	session, ok := s.sessionFor(w, r)
	if !ok {
		return
	}
	body, err := session.Playlist(r.Context())
	if err != nil {
		s.transcodeError(w, "playlist", err)
		return
	}
	// A stream asked for from the middle starts there: the client was given
	// this URL to resume with, and it plays what it is handed from the top.
	body = trimPlaylist(body, startFrom(r))
	base := "/Videos/" + r.PathValue("item") + "/hls/" + session.ID + "/"
	token := ""
	if value := tokenFrom(r); value != "" {
		token = "?api_key=" + url.QueryEscape(value)
	}
	playlist(w, rewritePlaylist(body, base, token))
}

// sessionFor finds or starts the conversion this request is asking for.
func (s *Server) sessionFor(w http.ResponseWriter, r *http.Request) (*transcode.Session, bool) {
	if s.Transcoder == nil {
		http.Error(w, "this server has no ffmpeg", http.StatusNotFound)
		return nil, false
	}
	id, _, err := parseID(r.PathValue("item"))
	if err != nil {
		http.Error(w, "bad item", http.StatusBadRequest)
		return nil, false
	}
	found, err := s.db(r.Context()).Item(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such item", http.StatusNotFound)
		return nil, false
	}
	if err != nil {
		s.fail(w, "read item", err)
		return nil, false
	}
	if len(found.Parts) == 0 {
		http.Error(w, "nothing to play", http.StatusNotFound)
		return nil, false
	}

	part := found.Parts[0]
	if wanted := r.URL.Query().Get("MediaSourceId"); wanted != "" {
		if sourceID, _, err := parseID(wanted); err == nil {
			for _, candidate := range found.Parts {
				if candidate.ID == sourceID {
					part = candidate
				}
			}
		}
	}

	session, err := s.Transcoder.Start(transcode.Request{
		// The client's own play session id, so the second request for this
		// playlist gets the ffmpeg the first one started rather than another,
		// and so a client that says it has stopped can be taken at its word.
		Key:      sessionKey(r.URL.Query().Get("PlaySessionId"), part.ID),
		Path:     part.Path,
		Plan:     wirePlan(r),
		Duration: found.Duration,
		StartAt:  startFrom(r),
	})
	if errors.Is(err, transcode.ErrBusy) {
		w.Header().Set("Retry-After", "10")
		http.Error(w, "this server is already converting as much as it can", http.StatusServiceUnavailable)
		return nil, false
	}
	if err != nil {
		s.fail(w, "start transcoding", err)
		return nil, false
	}
	return session, true
}

// startFrom is where in the file this stream begins.
func startFrom(r *http.Request) time.Duration {
	if ticks := int64(intQuery(r, "StartTimeTicks")); ticks > 0 {
		return time.Duration(ticks) * 100 * time.Nanosecond
	}
	return 0
}

func sessionKey(playSession string, partID int64) string {
	if playSession == "" {
		return ""
	}
	return "jellyfin|" + playSession + "|" + strconv.FormatInt(partID, 10)
}

// transcodeSegment hands over one piece of the stream, and says the client is
// still there: the only signal this server gets that anybody is watching.
func (s *Server) transcodeSegment(w http.ResponseWriter, r *http.Request) {
	if s.Transcoder == nil {
		http.Error(w, "this server has no ffmpeg", http.StatusNotFound)
		return
	}
	session, found := s.Transcoder.Get(r.PathValue("session"))
	if !found {
		// Gone rather than missing: the stream this names has been reaped,
		// and the client should negotiate a new one instead of retrying.
		http.Error(w, "that playback session has ended", http.StatusGone)
		return
	}
	session.Touch()

	name := r.PathValue("file")
	if strings.HasSuffix(name, ".m3u8") {
		// A player that resolved the playlist's own name against this path.
		body, err := session.Playlist(r.Context())
		if err != nil {
			s.transcodeError(w, "playlist", err)
			return
		}
		token := ""
		if value := tokenFrom(r); value != "" {
			token = "?api_key=" + url.QueryEscape(value)
		}
		// Already under the directory the segments are served from, so only
		// the token has to be added.
		playlist(w, rewritePlaylist(body, "", token))
		return
	}

	path, err := session.Segment(r.Context(), name)
	if err != nil {
		s.transcodeError(w, "segment "+name, err)
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	// A segment's name identifies its content for as long as the session
	// lives, and the session is thrown away when playback stops.
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if err := serve.File(w, r, path); err != nil {
		s.fail(w, "serve segment", err)
	}
}

// stopEncoding is a client saying it has stopped, which frees a core now
// rather than at the idle deadline.
func (s *Server) stopEncoding(w http.ResponseWriter, r *http.Request) {
	if s.Transcoder != nil {
		play := r.URL.Query().Get("PlaySessionId")
		// The part is not in the request, so every file this play session
		// could have been of is stopped; there is one.
		for _, part := range s.partsOfPlaySession(r.Context(), play) {
			s.Transcoder.StopKey(sessionKey(play, part))
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// partsOfPlaySession is which files a play session id could name.
//
// The id carries the item it was made for, which is enough to ask the
// database for that item's files — a stop that names a play session can then
// be matched without this server keeping a table of live ones.
func (s *Server) partsOfPlaySession(ctx context.Context, play string) []int64 {
	item, _, found := strings.Cut(play, "-")
	if !found {
		return nil
	}
	id, _, err := parseID(item)
	if err != nil {
		return nil
	}
	found2, err := s.db(ctx).Item(ctx, id)
	if err != nil {
		return nil
	}
	out := make([]int64, 0, len(found2.Parts))
	for _, part := range found2.Parts {
		out = append(out, part.ID)
	}
	return out
}

func (s *Server) transcodeError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, transcode.ErrNoSegment):
		http.Error(w, "no such segment", http.StatusNotFound)
	case errors.Is(err, transcode.ErrNotReady):
		// The honest answer is "ask again": the encoder is working on exactly
		// this piece.
		w.Header().Set("Retry-After", "1")
		http.Error(w, "still converting", http.StatusServiceUnavailable)
	case errors.Is(err, context.Canceled):
		// The client went away mid-wait, which needs no response at all.
	default:
		s.logger().Warn("transcode request failed", "what", what, "error", err)
		http.Error(w, "could not convert that", http.StatusInternalServerError)
	}
}

func playlist(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

// rewritePlaylist points a playlist's URIs at where this server serves them,
// carrying the client's token.
//
// ffmpeg writes bare file names, which a player resolves against the playlist
// URL — and the playlist URL is a path this server answers with a playlist,
// not a directory of segments, and resolving drops the query string that
// carried the token. So each URI is written out in full instead.
func rewritePlaylist(body []byte, base, query string) []byte {
	lines := strings.Split(string(body), "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
		case strings.HasPrefix(trimmed, "#EXT-X-MAP:"):
			// #EXT-X-MAP:URI="init.mp4" — the initialisation segment, which
			// is a URI inside an attribute rather than a line of its own.
			lines[i] = rewriteAttribute(trimmed, base, query)
		case strings.HasPrefix(trimmed, "#"):
		default:
			lines[i] = base + trimmed + query
		}
	}
	return []byte(strings.Join(lines, "\n"))
}

// trimPlaylist drops the segments before where this stream starts.
//
// The transcoder's playlist covers the whole runtime, numbered from the
// beginning of the file, which is right for a client that seeks inside a
// stream it opened at the start. A Jellyfin client resuming does something
// else: it asks for a stream that *begins* at the resume point and plays it
// from the top, adding the offset back itself. Handing that client the whole
// playlist has it ask for the first segment of the film, which is not what
// the encoder was started on, and the player sits on a spinner while the
// encoder is dragged back to the beginning.
//
// Entries are dropped by the index in their own name rather than by counting,
// so a playlist that already starts where it should — which is what ffmpeg
// writes for a copy — is left exactly as it is.
func trimPlaylist(body []byte, startAt time.Duration) []byte {
	if startAt <= 0 {
		return body
	}
	lines := strings.Split(string(body), "\n")

	// The segment length is whatever this playlist says its segments are.
	segment := 0.0
	for _, line := range lines {
		if after, found := strings.CutPrefix(strings.TrimSpace(line), "#EXTINF:"); found {
			value := strings.TrimSuffix(strings.TrimSpace(after), ",")
			if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds > 0 {
				segment = seconds
				break
			}
		}
	}
	if segment <= 0 {
		return body
	}
	first := int(startAt.Seconds() / segment)
	if first <= 0 {
		return body
	}

	out := make([]string, 0, len(lines))
	pending := []string{}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "#EXTINF:"):
			// Held until its URI is seen, because the two are one entry.
			pending = append(pending, line)
		case trimmed == "" || strings.HasPrefix(trimmed, "#"):
			out = append(out, line)
		default:
			index, ok := segmentIndex(trimmed)
			if ok && index < first {
				pending = nil
				continue
			}
			out = append(out, pending...)
			out = append(out, line)
			pending = nil
		}
	}
	return []byte(strings.Join(out, "\n"))
}

// segmentIndex reads the number out of a segment's name: seg00014.m4s.
func segmentIndex(name string) (int, bool) {
	digits := strings.TrimLeft(name, "abcdefghijklmnopqrstuvwxyz")
	if cut := strings.IndexByte(digits, '.'); cut >= 0 {
		digits = digits[:cut]
	}
	if digits == "" {
		return 0, false
	}
	index, err := strconv.Atoi(digits)
	if err != nil {
		return 0, false
	}
	return index, true
}

func rewriteAttribute(line, base, query string) string {
	const marker = `URI="`
	start := strings.Index(line, marker)
	if start < 0 {
		return line
	}
	rest := line[start+len(marker):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return line
	}
	return line[:start+len(marker)] + base + rest[:end] + query + rest[end:]
}
