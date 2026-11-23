package admin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/artwork"
	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/metadata"
	"github.com/inknironapps/bask-media-server/internal/metadata/posterdb"
	"github.com/inknironapps/bask-media-server/internal/serve"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// Choosing the picture on an item.
//
// Three ways in, because the three services people actually use work in
// three different shapes:
//
//   - Browse. TMDB and TheTVDB both answer "every picture you have of this",
//     so the editor draws a grid of them and somebody clicks one.
//   - Paste a link. ThePosterDB has no search API and its terms do not
//     permit crawling, so there is nothing to browse: somebody finds a
//     poster there, copies the address, and pastes it. The same box takes a
//     link to anywhere else, because a URL is a URL.
//   - Upload. A picture that is already on the machine somebody is sitting
//     at, which no amount of API gets at.
//
// Whichever way it arrives, the image is fetched into this server's own
// artwork cache and the item is locked. Nothing is hotlinked: a shelf that
// goes blank because a third party reorganised its CDN is not a shelf.

// artView is one picture on offer.
type artView struct {
	Kind     string  `json:"kind"`
	URL      string  `json:"url"`
	ThumbURL string  `json:"thumbUrl"`
	Width    int     `json:"width,omitempty"`
	Height   int     `json:"height,omitempty"`
	Language string  `json:"language,omitempty"`
	Votes    float64 `json:"votes,omitempty"`
	Source   string  `json:"source"`
}

// artworkReply is what the chooser is drawn from.
type artworkReply struct {
	// Art is everything every source offered, already in the order somebody
	// would want to look through it.
	Art []artView `json:"art"`

	// Sources names what was asked, so the page can say "TMDB had nothing"
	// rather than drawing an empty grid with no explanation. Failed carries
	// the ones that could not be reached and why.
	Sources []string `json:"sources"`
	Failed  []string `json:"failed,omitempty"`

	// Why is set when there was nothing to ask at all, which is a server
	// with no keys saved rather than a film nobody has a picture of.
	Why string `json:"why,omitempty"`

	// Link describes the paste box, which is always available.
	Link string `json:"link"`
}

// artwork lists the pictures every configured source has of one item.
func (s *Server) artwork(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	item, err := s.DB.Item(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such item")
		return
	}
	if err != nil {
		s.fail(w, "read item", err)
		return
	}

	out := artworkReply{
		Art:  []artView{},
		Link: "Paste the address of a picture — a ThePosterDB poster, or any image on the web.",
	}

	switch item.Kind {
	case domain.KindMovie, domain.KindShow:
	default:
		// A season, an episode, an album: the services here answer about
		// films and series, and a chooser that searched for the title of
		// episode four would offer the pictures of something else entirely.
		out.Why = "Only a film or a series can be looked up. A picture pasted or uploaded here still works."
		writeJSON(w, http.StatusOK, out)
		return
	}

	sources := s.artSources(r.Context())
	if len(sources) == 0 {
		out.Why = "No artwork service is set up. Save a TMDB key — and a TheTVDB key, if you have one — in Settings."
		writeJSON(w, http.StatusOK, out)
		return
	}

	want := metadata.ArtQuery{Kind: item.Kind, Title: item.Title, Year: item.Year}
	for _, source := range sources {
		out.Sources = append(out.Sources, source.Name())

		// The provider id belongs to whichever service matched the library,
		// so it is only offered to that one: handing TMDB's number to
		// TheTVDB would fetch the artwork of whatever happens to have it.
		query := want
		if source.Name() == "tmdb" {
			query.ProviderID = item.ProviderID
		}

		art, err := source.Art(r.Context(), query)
		if err != nil {
			// One service being down is not the chooser being down: the
			// others are still drawn, and this one is named.
			s.logger().Warn("could not list artwork", "source", source.Name(), "item", id, "error", err)
			out.Failed = append(out.Failed, source.Name()+": "+err.Error())
			continue
		}
		for _, one := range art {
			out.Art = append(out.Art, artView{
				Kind: string(one.Kind), URL: one.URL, ThumbURL: one.ThumbURL,
				Width: one.Width, Height: one.Height, Language: one.Language,
				Votes: one.Votes, Source: one.Source,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// chooseArtwork fetches a picture somebody picked and puts it on the item.
//
// The URL is fetched here and now rather than recorded, so the answer to the
// request is whether the picture is actually in hand: a chooser that said
// "saved" and left a dead link would be worse than one that failed.
func (s *Server) chooseArtwork(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	var body struct {
		Kind string `json:"kind"`
		URL  string `json:"url"`
	}
	if !decode(w, r, &body) {
		return
	}

	kind, ok := artKindOf(body.Kind)
	if !ok {
		writeError(w, http.StatusBadRequest, "a picture is either a poster or a backdrop")
		return
	}
	address, err := imageAddress(body.URL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.Artwork == nil {
		writeError(w, http.StatusServiceUnavailable, "this server has no artwork cache")
		return
	}

	key, err := s.Artwork.Fetch(r.Context(), address)
	if errors.Is(err, artwork.ErrNotAnImage) {
		// The commonest way a pasted link fails, and the one worth its own
		// words: the address led to a page rather than to a picture, which
		// is what a login wall or a dead link answers with.
		writeError(w, http.StatusBadRequest,
			"that address led to a page rather than a picture — open the image itself and copy its address")
		return
	}
	if err != nil {
		// The reason is shown: a 404 from the service and a refused
		// connection want different things done about them.
		writeError(w, http.StatusBadGateway, "that picture did not fetch: "+err.Error())
		return
	}
	if err := s.apply(r, id, kind, key); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no such item")
			return
		}
		s.fail(w, "save the picture", err)
		return
	}
	s.logger().Info("chose artwork by hand", "item", id, "kind", kind, "from", address)
	writeJSON(w, http.StatusOK, map[string]any{
		"kind": string(kind),
		"note": "Saved, and this item is locked so a metadata pass will not replace it.",
	})
}

// uploadArtwork takes a picture off the machine somebody is sitting at.
//
// The body is the image itself rather than a multipart form: there is one
// field, the page already has the bytes in hand, and a form would be a
// boundary to parse for no gain.
func (s *Server) uploadArtwork(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	kind, ok := artKindOf(r.URL.Query().Get("kind"))
	if !ok {
		writeError(w, http.StatusBadRequest, "a picture is either a poster or a backdrop")
		return
	}
	if s.Artwork == nil {
		writeError(w, http.StatusServiceUnavailable, "this server has no artwork cache")
		return
	}

	// Bounded, because this is an upload: a poster is a few hundred
	// kilobytes and anything past the cap is a mistake or a flood.
	data, err := io.ReadAll(io.LimitReader(r.Body, maxUploadBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "that upload did not arrive in one piece")
		return
	}
	if len(data) == 0 {
		writeError(w, http.StatusBadRequest, "there was no picture in that")
		return
	}
	if len(data) > maxUploadBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "that picture is larger than 12 MB")
		return
	}
	// Checked by its own first bytes rather than by what the page claimed:
	// the cache renders what it is given, and a PDF named poster.jpg would
	// be a broken shelf discovered later. The same test a pasted link's
	// download is held to, and for the same reason — this is a file from
	// outside the library.
	if !artwork.IsImage(data) {
		writeError(w, http.StatusBadRequest, "that file is not a JPEG or PNG picture")
		return
	}

	key, err := s.Artwork.Store(data)
	if err != nil {
		s.fail(w, "store the picture", err)
		return
	}
	if err := s.apply(r, id, kind, key); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no such item")
			return
		}
		s.fail(w, "save the picture", err)
		return
	}
	s.logger().Info("uploaded artwork", "item", id, "kind", kind, "bytes", len(data))
	writeJSON(w, http.StatusOK, map[string]any{
		"kind": string(kind),
		"note": "Saved, and this item is locked so a metadata pass will not replace it.",
	})
}

// maxUploadBytes bounds a hand-supplied picture. A 4K backdrop as a PNG is
// comfortably under this; a film is not.
const maxUploadBytes = 12 << 20

// apply puts a cached picture on the item, as the kind it is.
func (s *Server) apply(r *http.Request, id int64, kind metadata.ArtKind, key string) error {
	if kind == metadata.ArtBackdrop {
		return s.DB.SetArtwork(r.Context(), id, "", key)
	}
	return s.DB.SetArtwork(r.Context(), id, key, "")
}

// artSources is the services this server can browse, or nothing.
func (s *Server) artSources(ctx context.Context) []metadata.ArtworkSource {
	if s.Jobs == nil || s.Jobs.Sources == nil {
		return nil
	}
	return s.Jobs.Sources(ctx)
}

func artKindOf(value string) (metadata.ArtKind, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "poster", "thumb":
		return metadata.ArtPoster, true
	case "backdrop", "art", "background":
		return metadata.ArtBackdrop, true
	}
	return "", false
}

// imageAddress is the address to fetch for whatever somebody pasted.
//
// ThePosterDB is recognised because its poster pages are what a person has
// in the clipboard and the picture behind one lives at a different address.
// Everything else has to be an http address already: a file path or a data
// URL here would be this server reading its own disk on a request from a
// page, which is a different and much larger decision.
func imageAddress(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("there was no address in that")
	}
	if posterdb.Mine(raw) {
		return posterdb.Image(raw)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("that is not an address")
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return "", errors.New("the address has to start with http or https")
	}
	if parsed.Host == "" {
		return "", errors.New("that address names no site")
	}
	return parsed.String(), nil
}

// itemImage serves the picture an item has now, for the editor to show
// beside the chooser.
//
// Its own route rather than borrowing the viewing surface's: this one is
// behind the administration session, and an administrator correcting a
// library must be able to see the poster on an item inside a library their
// own account is not granted — the whole point of the page is to fix what is
// wrong with it.
func (s *Server) itemImage(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	if s.Artwork == nil {
		http.Error(w, "no artwork", http.StatusNotFound)
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

	key := item.PosterKey
	if kind, _ := artKindOf(r.URL.Query().Get("kind")); kind == metadata.ArtBackdrop {
		key = item.ArtKey
	}
	if key == "" {
		// A 404 and not a placeholder: the page draws nothing for an image
		// that does not load, which is the honest state of an item with no
		// picture yet.
		http.Error(w, "no image", http.StatusNotFound)
		return
	}

	// Clamped, because the width comes from a URL and rendering is the
	// memory-hungry half of serving artwork.
	width, height := min(intQueryOf(r, "w"), 4000), min(intQueryOf(r, "h"), 4000)
	if err := serve.Image(w, r, s.Artwork, key, width, height); err != nil {
		if errors.Is(err, artwork.ErrNotCached) {
			http.Error(w, "no image", http.StatusNotFound)
			return
		}
		s.fail(w, "serve artwork", err)
	}
}

// intQueryOf reads a positive number from the query string, or zero.
func intQueryOf(r *http.Request, name string) int {
	if value := r.URL.Query().Get(name); value != "" {
		if n, err := strconv.Atoi(value); err == nil && n > 0 {
			return n
		}
	}
	return 0
}
