// Package tmdb matches titles against The Movie Database.
//
// Hand-rolled over net/http rather than an SDK: three endpoints are needed,
// the JSON shapes are four fields each, and a dependency that wraps an HTTP
// call is a dependency that decides its own timeouts, its own retries and its
// own buffer sizes. All three are budget decisions here.
package tmdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/inknironapps/bask-media-server/internal/metadata"
)

const (
	apiBase = "https://api.themoviedb.org/3"

	// Images are addressed by a size-prefixed path. w500 for posters is what
	// every client scales down from anyway, and original backdrops are large
	// enough to be worth not storing: w1280 is a 1080p backdrop.
	posterBase   = "https://image.tmdb.org/t/p/w500"
	backdropBase = "https://image.tmdb.org/t/p/w1280"

	// A response to a search is a few kilobytes. The cap is what stops a
	// confused or hostile endpoint from being read into memory without bound.
	maxResponseBytes = 1 << 20
)

// Client talks to TMDB.
type Client struct {
	http *http.Client

	// key is either a v3 API key, sent as a query parameter, or a v4 read
	// token, sent as a bearer header. Which one is decided by shape, because
	// asking the user which kind of key they pasted is a question with a
	// knowable answer.
	key    string
	bearer bool

	// movieGenreNames and showGenreNames turn the genre ids a search result
	// carries into words, fetched once each. See genres.go.
	movieGenreNames genreNames
	showGenreNames  genreNames

	// gate bounds concurrent requests. TMDB's published limit is generous, but
	// an enrichment pass over a cold library would otherwise open a connection
	// per worker and hold a response buffer for each.
	gate chan struct{}
}

// New builds a client. The key is required: without one there is no provider,
// and the caller is expected to carry on with filenames alone.
func New(key string, concurrency int) (*Client, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errors.New("no TMDB key")
	}
	if concurrency <= 0 {
		concurrency = 4
	}
	return &Client{
		http: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        4,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     30 * time.Second,
			},
		},
		key:    key,
		bearer: strings.Count(key, ".") >= 2, // a v4 token is a JWT
		gate:   make(chan struct{}, concurrency),
	}, nil
}

// Name identifies the provider.
func (c *Client) Name() string { return "tmdb" }

type searchResponse struct {
	Results []struct {
		ID           int64  `json:"id"`
		Title        string `json:"title"`
		Name         string `json:"name"`
		Overview     string `json:"overview"`
		ReleaseDate  string `json:"release_date"`
		FirstAirDate string `json:"first_air_date"`
		PosterPath   string `json:"poster_path"`
		BackdropPath string `json:"backdrop_path"`
		// GenreIDs are ids into a list of names fetched separately: the
		// search endpoint carries the numbers only.
		GenreIDs []int64 `json:"genre_ids"`
	} `json:"results"`
}

type episodeResponse struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Overview  string `json:"overview"`
	AirDate   string `json:"air_date"`
	StillPath string `json:"still_path"`
}

// Movie matches a film.
func (c *Client) Movie(ctx context.Context, title string, year int) (metadata.Match, error) {
	params := url.Values{"query": {title}, "include_adult": {"false"}}
	if year > 0 {
		params.Set("primary_release_year", strconv.Itoa(year))
	}

	var out searchResponse
	if err := c.get(ctx, "/search/movie", params, &out); err != nil {
		return metadata.Match{}, err
	}
	if len(out.Results) == 0 && year > 0 {
		// A year that disagrees with the provider's release year is common —
		// a file named for a festival year, a provider dated to a wide
		// release. One retry without it beats leaving the title unmatched.
		params.Del("primary_release_year")
		if err := c.get(ctx, "/search/movie", params, &out); err != nil {
			return metadata.Match{}, err
		}
	}
	if len(out.Results) == 0 {
		return metadata.Match{}, metadata.ErrNoMatch
	}

	first := out.Results[0]
	id := strconv.FormatInt(first.ID, 10)
	return metadata.Match{
		ProviderID:  id,
		Title:       first.Title,
		Year:        yearOf(first.ReleaseDate),
		Summary:     first.Overview,
		PosterURL:   imageURL(posterBase, first.PosterPath),
		BackdropURL: imageURL(backdropBase, first.BackdropPath),
		// One more request, because the search endpoint does not carry a
		// certification and a rating ceiling is worth nothing without one.
		Rating: c.movieRating(ctx, id),
		Genres: c.movieGenres(ctx, first.GenreIDs),
	}, nil
}

// Show matches a series.
func (c *Client) Show(ctx context.Context, title string, year int) (metadata.Match, error) {
	params := url.Values{"query": {title}, "include_adult": {"false"}}
	if year > 0 {
		params.Set("first_air_date_year", strconv.Itoa(year))
	}

	var out searchResponse
	if err := c.get(ctx, "/search/tv", params, &out); err != nil {
		return metadata.Match{}, err
	}
	if len(out.Results) == 0 && year > 0 {
		params.Del("first_air_date_year")
		if err := c.get(ctx, "/search/tv", params, &out); err != nil {
			return metadata.Match{}, err
		}
	}
	if len(out.Results) == 0 {
		return metadata.Match{}, metadata.ErrNoMatch
	}

	first := out.Results[0]
	id := strconv.FormatInt(first.ID, 10)
	return metadata.Match{
		ProviderID:  id,
		Title:       first.Name,
		Year:        yearOf(first.FirstAirDate),
		Summary:     first.Overview,
		PosterURL:   imageURL(posterBase, first.PosterPath),
		BackdropURL: imageURL(backdropBase, first.BackdropPath),
		Rating:      c.showRating(ctx, id),
		Genres:      c.showGenres(ctx, first.GenreIDs),
	}, nil
}

// Episode fetches one episode of an already-matched show.
func (c *Client) Episode(ctx context.Context, showProviderID string, season, number int) (metadata.Match, error) {
	if showProviderID == "" {
		return metadata.Match{}, metadata.ErrNoMatch
	}
	path := fmt.Sprintf("/tv/%s/season/%d/episode/%d", showProviderID, season, number)

	var out episodeResponse
	if err := c.get(ctx, path, url.Values{}, &out); err != nil {
		if errors.Is(err, errNotFound) {
			return metadata.Match{}, metadata.ErrNoMatch
		}
		return metadata.Match{}, err
	}
	return metadata.Match{
		ProviderID: strconv.FormatInt(out.ID, 10),
		Title:      out.Name,
		Year:       yearOf(out.AirDate),
		Summary:    out.Overview,
		// An episode's own image is its still; there is no episode backdrop.
		PosterURL: imageURL(backdropBase, out.StillPath),
	}, nil
}

var errNotFound = errors.New("not found")

// BaseURL is swapped in tests. A fake TMDB over httptest is how the provider
// is tested without reaching the network, which also keeps the suite runnable
// with no key configured.
var BaseURL = apiBase

func (c *Client) get(ctx context.Context, path string, params url.Values, out any) error {
	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}

	if !c.bearer {
		params = cloneValues(params)
		params.Set("api_key", c.key)
	}

	endpoint := BaseURL + path
	if encoded := params.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("tmdb request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.bearer {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("tmdb %s: %w", path, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		// Drain before closing so the connection can be reused rather than
		// torn down, which matters across a few thousand episode lookups.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		return errNotFound
	case resp.StatusCode == http.StatusUnauthorized:
		return errors.New("tmdb rejected the key")
	case resp.StatusCode == http.StatusTooManyRequests:
		return errors.New("tmdb rate limited this pass")
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("tmdb %s: status %d", path, resp.StatusCode)
	}

	body := io.LimitReader(resp.Body, maxResponseBytes)
	if err := json.NewDecoder(body).Decode(out); err != nil {
		return fmt.Errorf("tmdb %s: %w", path, err)
	}
	return nil
}

func cloneValues(in url.Values) url.Values {
	out := make(url.Values, len(in)+1)
	for k, v := range in {
		out[k] = append([]string(nil), v...)
	}
	return out
}

func imageURL(base, path string) string {
	if path == "" {
		return ""
	}
	return base + path
}

func yearOf(date string) int {
	if len(date) < 4 {
		return 0
	}
	year, err := strconv.Atoi(date[:4])
	if err != nil {
		return 0
	}
	return year
}
