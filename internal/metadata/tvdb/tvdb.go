// Package tvdb fetches artwork from TheTVDB.
//
// An artwork source and not a metadata provider, on purpose. BMS matches a
// library against one service, and matching it against two would mean two
// ids per item, two sets of titles that disagree and a rule for which wins.
// What TheTVDB is wanted for here is the pictures: it has artwork for
// television that TMDB does not, which is the whole reason somebody asks for
// it.
//
// Hand-rolled over net/http for the same reasons the TMDB client is: four
// endpoints, small JSON, and timeouts that are this program's decision.
package tvdb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/metadata"
)

const (
	apiBase = "https://api4.thetvdb.com/v4"

	// Artwork is addressed by a full URL in the response, so there is no size
	// prefix to build. TheTVDB serves a thumbnail of its own beside each one.

	// maxResponseBytes caps what a confused or hostile endpoint can be read
	// into memory. A series' artwork list is the largest answer here and runs
	// to a few hundred kilobytes.
	maxResponseBytes = 4 << 20

	// tokenLife is how long a login is reused. The documented life is about a
	// month; a day keeps the window short enough that a revoked key stops
	// working the same day, at a cost of one extra login.
	tokenLife = 24 * time.Hour
)

// Client talks to TheTVDB.
type Client struct {
	http *http.Client

	key string

	// pin is the subscriber PIN a user-supported key needs, and empty for a
	// company key. Sent only when it is set, because sending an empty one is
	// refused rather than ignored.
	pin string

	// mu guards the bearer token. A login is one request that every other
	// request needs, so the first caller makes it and the rest wait rather
	// than each logging in.
	mu      sync.Mutex
	token   string
	expires time.Time
}

// New builds a client. The key is required: without one there is no source,
// and the caller is expected to offer the other sources alone.
func New(key, pin string) (*Client, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errors.New("no TheTVDB key")
	}
	return &Client{
		http: &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        2,
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     30 * time.Second,
			},
		},
		key: key,
		pin: strings.TrimSpace(pin),
	}, nil
}

// Name identifies the source.
func (c *Client) Name() string { return "tvdb" }

// BaseURL is swapped in tests, so the suite runs with no key and no network.
var BaseURL = apiBase

// Art lists TheTVDB's pictures of a film or a series.
//
// Always a search first, because TheTVDB's ids are its own: an item matched
// against TMDB carries TMDB's id, and handing that number to this service
// would fetch the artwork of whatever happens to have it here.
func (c *Client) Art(ctx context.Context, want metadata.ArtQuery) ([]metadata.Art, error) {
	id, kind, err := c.find(ctx, want)
	if err != nil {
		return nil, err
	}
	if id == "" {
		// Searched and found nothing. An answer, not a failure.
		return nil, nil
	}

	var raw []artworkEntry
	switch kind {
	case "series":
		raw, err = c.seriesArt(ctx, id)
	default:
		raw, err = c.movieArt(ctx, id)
	}
	if err != nil {
		return nil, err
	}

	art := make([]metadata.Art, 0, len(raw))
	for _, entry := range raw {
		converted, ok := convert(entry)
		if !ok {
			continue
		}
		art = append(art, converted)
	}
	sortArt(art)
	return art, nil
}

// artworkTypes are TheTVDB's own numbers for what a picture is.
//
// Named here because the service returns the number and nothing else, and a
// chooser that offered "type 3" would be unusable. Only the two shapes BMS
// draws are mapped; a banner, a clear logo and an icon are left out, because
// an image nothing displays is a row that cannot be chosen.
//
// 2 and 14 are the upright poster for a series and a film, 3 and 15 the wide
// background for each.
var artworkTypes = map[int]metadata.ArtKind{
	2:  metadata.ArtPoster,
	14: metadata.ArtPoster,
	3:  metadata.ArtBackdrop,
	15: metadata.ArtBackdrop,
}

type artworkEntry struct {
	ID        int64   `json:"id"`
	Image     string  `json:"image"`
	Thumbnail string  `json:"thumbnail"`
	Language  string  `json:"language"`
	Type      int     `json:"type"`
	Score     float64 `json:"score"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
}

func convert(entry artworkEntry) (metadata.Art, bool) {
	kind, known := artworkTypes[entry.Type]
	if !known || entry.Image == "" {
		return metadata.Art{}, false
	}
	thumb := entry.Thumbnail
	if thumb == "" {
		thumb = entry.Image
	}
	return metadata.Art{
		Kind:     kind,
		URL:      entry.Image,
		ThumbURL: thumb,
		Width:    entry.Width,
		Height:   entry.Height,
		// TheTVDB writes a three-letter code, or "null" as a word for a
		// picture with no text. Both become the empty string this program
		// uses for textless.
		Language: language(entry.Language),
		Votes:    entry.Score,
		Source:   "tvdb",
	}, true
}

// language turns TheTVDB's code into the two-letter one BMS reports, and the
// empty string for an image with no text on it.
//
// "null" as a four-character string is how the service says textless, and it
// has to be read as such: left alone it would appear in a chooser as a
// language called null.
func language(code string) string {
	code = strings.TrimSpace(strings.ToLower(code))
	if code == "" || code == "null" || code == "none" {
		return ""
	}
	// Three-letter codes for the languages a household here is likely to
	// read. Anything else is reported as it came, because a code nobody
	// recognises is still better than hiding the image.
	switch code {
	case "eng":
		return "en"
	case "fra", "fre":
		return "fr"
	case "deu", "ger":
		return "de"
	case "spa":
		return "es"
	case "ita":
		return "it"
	case "nld", "dut":
		return "nl"
	case "jpn":
		return "ja"
	}
	return code
}

// sortArt is the order somebody wants to look through: posters first, then
// textless, then best regarded. The same rule as the TMDB source, for the
// same reason — a client draws the title itself.
func sortArt(art []metadata.Art) {
	sort.SliceStable(art, func(i, j int) bool {
		a, b := art[i], art[j]
		if a.Kind != b.Kind {
			return a.Kind == metadata.ArtPoster
		}
		if (a.Language == "") != (b.Language == "") {
			return a.Language == ""
		}
		if a.Votes != b.Votes {
			return a.Votes > b.Votes
		}
		return a.Width > b.Width
	})
}

type searchResponse struct {
	Data []struct {
		TVDBID string `json:"tvdb_id"`
		Type   string `json:"type"`
		Name   string `json:"name"`
		Year   string `json:"year"`
	} `json:"data"`
}

// find is TheTVDB's own id for the thing, and which sort of record it is.
//
// The year is used to choose between results rather than to filter the
// search: a file named for a festival year and a service dated to a wide
// release disagree often enough that filtering would lose the match
// entirely, while preferring the right year when it is there costs nothing.
func (c *Client) find(ctx context.Context, want metadata.ArtQuery) (id, kind string, err error) {
	kind = "movie"
	if want.Kind == domain.KindShow {
		kind = "series"
	}

	params := url.Values{"query": {want.Title}, "type": {kind}, "limit": {"10"}}
	var out searchResponse
	if err := c.get(ctx, "/search", params, &out); err != nil {
		if errors.Is(err, errNotFound) {
			return "", kind, nil
		}
		return "", kind, err
	}
	if len(out.Data) == 0 {
		return "", kind, nil
	}

	best := out.Data[0]
	if want.Year > 0 {
		for _, candidate := range out.Data {
			if year, convErr := strconv.Atoi(candidate.Year); convErr == nil && year == want.Year {
				best = candidate
				break
			}
		}
	}
	return best.TVDBID, kind, nil
}

func (c *Client) seriesArt(ctx context.Context, id string) ([]artworkEntry, error) {
	var out struct {
		Data struct {
			Artworks []artworkEntry `json:"artworks"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/series/"+id+"/artworks", url.Values{}, &out); err != nil {
		if errors.Is(err, errNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return out.Data.Artworks, nil
}

// movieArt reads a film's pictures, which live on the extended record rather
// than on a route of their own.
func (c *Client) movieArt(ctx context.Context, id string) ([]artworkEntry, error) {
	var out struct {
		Data struct {
			Artworks []artworkEntry `json:"artworks"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/movies/"+id+"/extended", url.Values{}, &out); err != nil {
		if errors.Is(err, errNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return out.Data.Artworks, nil
}

var errNotFound = errors.New("not found")

// bearer is a login token, made on the first request and reused.
func (c *Client) bearer(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expires) {
		return c.token, nil
	}

	body := map[string]string{"apikey": c.key}
	if c.pin != "" {
		// Sent only when there is one: an empty pin is refused rather than
		// ignored, so a company key with the field present never logs in.
		body["pin"] = c.pin
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("tvdb login: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, BaseURL+"/login", bytes.NewReader(encoded))
	if err != nil {
		return "", fmt.Errorf("tvdb login: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("tvdb login: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", errors.New("TheTVDB rejected the key")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tvdb login: status %d", resp.StatusCode)
	}

	var out struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&out); err != nil {
		return "", fmt.Errorf("tvdb login: %w", err)
	}
	if out.Data.Token == "" {
		return "", errors.New("TheTVDB returned no token")
	}
	c.token, c.expires = out.Data.Token, time.Now().Add(tokenLife)
	return c.token, nil
}

func (c *Client) get(ctx context.Context, path string, params url.Values, out any) error {
	token, err := c.bearer(ctx)
	if err != nil {
		return err
	}

	endpoint := BaseURL + path
	if encoded := params.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("tvdb request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("tvdb %s: %w", path, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		// Drained before closing so the connection is reused rather than
		// torn down.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		return errNotFound
	case resp.StatusCode == http.StatusUnauthorized:
		// The token went stale early. Forgotten here so the next request
		// logs in again rather than failing for the rest of the day.
		c.forget()
		return errors.New("TheTVDB rejected the login")
	case resp.StatusCode == http.StatusTooManyRequests:
		return errors.New("TheTVDB rate limited this request")
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("tvdb %s: status %d", path, resp.StatusCode)
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); err != nil {
		return fmt.Errorf("tvdb %s: %w", path, err)
	}
	return nil
}

func (c *Client) forget() {
	c.mu.Lock()
	c.token, c.expires = "", time.Time{}
	c.mu.Unlock()
}
