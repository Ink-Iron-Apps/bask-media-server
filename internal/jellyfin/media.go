package jellyfin

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/artwork"
	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/scan"
	"github.com/inknironapps/bask-media-server/internal/serve"
	"github.com/inknironapps/bask-media-server/internal/store"
	"github.com/inknironapps/bask-media-server/internal/subtitles"
)

// mediaSource is one file, as a client is told about it.
type mediaSource struct {
	ID                     string        `json:"Id"`
	Path                   string        `json:"Path,omitempty"`
	Protocol               string        `json:"Protocol"`
	Container              string        `json:"Container,omitempty"`
	Size                   int64         `json:"Size,omitempty"`
	Name                   string        `json:"Name,omitempty"`
	RunTimeTicks           int64         `json:"RunTimeTicks,omitempty"`
	IsRemote               bool          `json:"IsRemote"`
	SupportsDirectPlay     bool          `json:"SupportsDirectPlay"`
	SupportsDirectStream   bool          `json:"SupportsDirectStream"`
	SupportsTranscoding    bool          `json:"SupportsTranscoding"`
	MediaStreams           []mediaStream `json:"MediaStreams,omitempty"`
	DirectStreamURL        string        `json:"DirectStreamUrl,omitempty"`
	TranscodingURL         string        `json:"TranscodingUrl,omitempty"`
	TranscodingSubProtocol string        `json:"TranscodingSubProtocol,omitempty"`
	ETag                   string        `json:"ETag,omitempty"`
}

// mediaStream is one track inside a file. Only subtitles are described from
// the database — the picture and the sound are read from the file by whatever
// plays it, and saying anything about them here would be repeating a probe
// that the client is about to do anyway.
type mediaStream struct {
	Index                  int    `json:"Index"`
	Type                   string `json:"Type"`
	Codec                  string `json:"Codec,omitempty"`
	Language               string `json:"Language,omitempty"`
	DisplayTitle           string `json:"DisplayTitle,omitempty"`
	Title                  string `json:"Title,omitempty"`
	IsDefault              bool   `json:"IsDefault"`
	IsForced               bool   `json:"IsForced"`
	IsExternal             bool   `json:"IsExternal"`
	IsTextSubtitle         bool   `json:"IsTextSubtitleStream"`
	SupportsExternalStream bool   `json:"SupportsExternalStream"`
	DeliveryMethod         string `json:"DeliveryMethod,omitempty"`
	DeliveryURL            string `json:"DeliveryUrl,omitempty"`
	Path                   string `json:"Path,omitempty"`
}

// mediaSource describes the file behind an item.
//
// Direct play is claimed for everything, which is the honest answer: the
// server has the file and will send it whole, and whether this client can
// decode it is a question only the client can answer. A client that decides
// it cannot asks for a transcode, and that is when ffmpeg is involved.
func (s *Server) mediaSource(it domain.Item, part domain.Part) mediaSource {
	source := mediaSource{
		ID:                   itemID(part.ID),
		Path:                 part.Path,
		Protocol:             "File",
		Container:            part.Container,
		Size:                 part.Size,
		Name:                 it.Title,
		RunTimeTicks:         ticks(it.Duration),
		SupportsDirectPlay:   true,
		SupportsDirectStream: true,
		SupportsTranscoding:  s.Transcoder != nil,
		DirectStreamURL:      streamPath(it, part),
		ETag:                 itemID(part.ID),
	}
	if s.Transcoder != nil && mediaTypeOf(it.Kind) == "Video" {
		source.TranscodingURL = fmt.Sprintf("/Videos/%s/master.m3u8?MediaSourceId=%s&api_key=",
			itemID(it.ID), itemID(part.ID))
		source.TranscodingSubProtocol = "hls"
	}
	return source
}

// streamPath is where the file itself is served from.
func streamPath(it domain.Item, part domain.Part) string {
	kind := "Videos"
	if mediaTypeOf(it.Kind) == "Audio" {
		kind = "Audio"
	}
	path := fmt.Sprintf("/%s/%s/stream", kind, itemID(it.ID))
	if part.Container != "" {
		path += "." + part.Container
	}
	return path + "?static=true&MediaSourceId=" + itemID(part.ID)
}

// streamsOf describes an item's subtitles, which is the one thing about a
// file a client cannot find out by opening it: a sidecar is a different file.
func (s *Server) streamsOf(ctx context.Context, it domain.Item) []mediaStream {
	if !it.Playable() {
		return nil
	}
	subs, err := s.DB.Subtitles(ctx, []int64{it.ID})
	if err != nil {
		s.logger().Warn("could not read subtitles", "error", err)
		return nil
	}
	list := subs[it.ID]
	out := make([]mediaStream, 0, len(list))
	for position, sub := range list {
		// Numbered by position, not by the index inside the container: a
		// sidecar has none (it is a file of its own, and carries -1), and a
		// client switching tracks sends back what it was given.
		index := position
		stream := mediaStream{
			Index:                  index,
			Type:                   "Subtitle",
			Codec:                  sub.Codec,
			Language:               sub.Language,
			Title:                  sub.Title,
			DisplayTitle:           subtitleLabel(sub),
			IsDefault:              sub.Default,
			IsForced:               sub.Forced,
			IsExternal:             sub.External,
			IsTextSubtitle:         sub.Text(),
			SupportsExternalStream: sub.Text(),
			DeliveryMethod:         "Embed",
		}
		if sub.Text() {
			stream.DeliveryMethod = "External"
			stream.DeliveryURL = fmt.Sprintf("/Videos/%s/%s/Subtitles/%d/Stream.vtt",
				itemID(it.ID), itemID(sub.PartID), sub.ID)
		}
		if sub.External {
			stream.Path = sub.Path
		}
		out = append(out, stream)
	}
	return out
}

// subtitleLabel is what a client shows in its menu.
func subtitleLabel(sub domain.Subtitle) string {
	parts := []string{}
	if name := scan.LanguageName(sub.Language); name != "" && sub.Language != "" {
		parts = append(parts, name)
	}
	if sub.Title != "" {
		parts = append(parts, sub.Title)
	}
	if sub.Forced {
		parts = append(parts, "Forced")
	}
	if sub.Hearing {
		parts = append(parts, "SDH")
	}
	if len(parts) == 0 {
		return strings.ToUpper(sub.Codec)
	}
	return strings.Join(parts, " · ")
}

// image serves a poster, a backdrop, or a photograph.
func (s *Server) image(w http.ResponseWriter, r *http.Request) {
	if s.Artwork == nil {
		http.Error(w, "no artwork", http.StatusNotFound)
		return
	}
	id, _, err := parseID(r.PathValue("item"))
	if err != nil {
		http.Error(w, "bad item", http.StatusBadRequest)
		return
	}
	found, err := s.DB.Item(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such item", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "read item", err)
		return
	}

	width := intQuery(r, "maxWidth")
	if width == 0 {
		width = intQuery(r, "fillWidth")
	}
	height := intQuery(r, "maxHeight")
	if height == 0 {
		height = intQuery(r, "fillHeight")
	}
	width, height = min(width, 4000), min(height, 4000)

	// A photograph is served from the library, where it already is; a poster
	// comes out of the artwork cache.
	if part, found := s.photoSource(r.Context(), found); found {
		if err := serve.Photo(w, r, s.Artwork, part.Path, width, height, part.Orientation); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				http.Error(w, "file is gone", http.StatusNotFound)
				return
			}
			s.fail(w, "serve photo", err)
		}
		return
	}

	key := found.PosterKey
	if strings.EqualFold(r.PathValue("type"), "Backdrop") && found.ArtKey != "" {
		key = found.ArtKey
	}
	if key == "" {
		// A season or an episode with no picture of its own shows the show's,
		// which is what the row above it already carries.
		if parent := s.inheritedPoster(r.Context(), found); parent != "" {
			key = parent
		}
	}
	if key == "" {
		http.Error(w, "no image", http.StatusNotFound)
		return
	}
	if err := serve.Image(w, r, s.Artwork, key, width, height); err != nil {
		if errors.Is(err, artwork.ErrNotCached) {
			http.Error(w, "no image", http.StatusNotFound)
			return
		}
		s.fail(w, "serve artwork", err)
	}
}

// inheritedPoster walks up for a picture, one level at a time, so a track
// shows its album's cover and an episode its show's.
func (s *Server) inheritedPoster(ctx context.Context, it *domain.Item) string {
	parent := it.ParentID
	for range 2 {
		if parent == nil {
			return ""
		}
		above, err := s.DB.Item(ctx, *parent)
		if err != nil {
			return ""
		}
		if above.PosterKey != "" {
			return above.PosterKey
		}
		parent = above.ParentID
	}
	return ""
}

func (s *Server) photoSource(ctx context.Context, it *domain.Item) (domain.Part, bool) {
	switch it.Kind {
	case domain.KindPhoto:
		parts := it.Parts
		if len(parts) == 0 {
			found, err := s.DB.Parts(ctx, it.ID)
			if err != nil {
				return domain.Part{}, false
			}
			parts = found
		}
		if len(parts) > 0 {
			return parts[0], true
		}
	case domain.KindPhotoAlbum:
		part, err := s.DB.AlbumCover(ctx, it.ID)
		if err == nil {
			return *part, true
		}
	}
	return domain.Part{}, false
}

// streamNamed answers the spelling that carries a container: `stream.mkv`.
// Anything else under that path is not a stream request.
func (s *Server) streamNamed(w http.ResponseWriter, r *http.Request) {
	name := strings.ToLower(r.PathValue("file"))
	if name != "stream" && !strings.HasPrefix(name, "stream.") {
		http.NotFound(w, r)
		return
	}
	s.stream(w, r)
}

// stream sends the file, with ranges, which is what lets a client seek.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	id, _, err := parseID(r.PathValue("item"))
	if err != nil {
		http.Error(w, "bad item", http.StatusBadRequest)
		return
	}
	found, err := s.DB.Item(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such item", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "read item", err)
		return
	}
	if len(found.Parts) == 0 {
		http.Error(w, "nothing to play", http.StatusNotFound)
		return
	}

	part := found.Parts[0]
	// A client that names the source it wants gets that one: an item with two
	// versions has two files behind it.
	if wanted := r.URL.Query().Get("MediaSourceId"); wanted != "" {
		if sourceID, _, err := parseID(wanted); err == nil {
			for _, candidate := range found.Parts {
				if candidate.ID == sourceID {
					part = candidate
				}
			}
		}
	}

	if err := serve.File(w, r, part.Path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.logger().Warn("part file is missing", "path", part.Path)
			http.Error(w, "file is gone", http.StatusNotFound)
			return
		}
		s.fail(w, "stream", err)
	}
}

// playbackInfo is where a client asks how it may play something.
//
// The answer is always "here is the file": BMS direct plays by default, and a
// client that cannot decode what it is given asks for a transcode by URL
// rather than being told in advance what it can manage. That keeps the
// decision where the knowledge is.
func (s *Server) playbackInfo(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	id, _, err := parseID(r.PathValue("item"))
	if err != nil {
		http.Error(w, "bad item", http.StatusBadRequest)
		return
	}
	found, err := s.DB.Item(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such item", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "read item", err)
		return
	}

	streams := s.streamsOf(r.Context(), *found)
	sources := make([]mediaSource, 0, len(found.Parts))
	for _, part := range found.Parts {
		source := s.mediaSource(*found, part)
		source.MediaStreams = streams
		sources = append(sources, source)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"MediaSources":  sources,
		"PlaySessionId": fmt.Sprintf("%s-%d", itemID(found.ID), user.ID),
	})
}

// subtitleStream hands over one subtitle in the format asked for.
func (s *Server) subtitleStream(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("index"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad subtitle", http.StatusBadRequest)
		return
	}
	sub, err := s.DB.Subtitle(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such subtitle", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "read subtitle", err)
		return
	}
	if !sub.Text() {
		// A picture subtitle has no text to hand over; it plays where the
		// renderer is, which is the client.
		http.Error(w, "that subtitle is a picture", http.StatusNotImplemented)
		return
	}
	if s.Subtitles == nil {
		http.Error(w, "no converter", http.StatusNotImplemented)
		return
	}

	// "Stream.vtt" — the format is the extension, and anything before the dot
	// is Jellyfin's own word for "the subtitle itself".
	name := r.PathValue("file")
	format := ""
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		format = name[dot+1:]
	}
	want := subtitles.FormatOf(format)
	body, served, err := s.Subtitles.Text(r.Context(), *sub, want)
	if err != nil {
		if errors.Is(err, subtitles.ErrNotText) {
			http.Error(w, "that subtitle is a picture", http.StatusNotImplemented)
			return
		}
		s.fail(w, "convert subtitle", err)
		return
	}
	w.Header().Set("Content-Type", served.ContentType())
	_, _ = w.Write(body)
}
