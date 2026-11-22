package tmdb

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"context"

	"github.com/inknironapps/bask-media-server/internal/metadata"
)

// Correcting a match by hand, which needs two things an enrichment pass does
// not: the entries the provider did not pick, and the ability to fetch one of
// them exactly.
//
// A wrong match is the most visible mistake this server can make — the poster
// is the first thing anybody sees — and until now it was permanent, because
// the only way back was a search that would rank the same wrong entry first
// again.

// maxCandidates is how many of the provider's results are offered.
//
// Enough that the right one is nearly always in the list, few enough that
// choosing from it is a glance rather than a scroll. TMDB ranks by
// popularity, so the answer is at the top or it is nowhere near it.
const maxCandidates = 10

// MovieCandidates is what the provider found for this title.
func (c *Client) MovieCandidates(ctx context.Context, title string, year int) ([]metadata.Match, error) {
	params := url.Values{"query": {title}, "include_adult": {"false"}}
	if year > 0 {
		params.Set("primary_release_year", strconv.Itoa(year))
	}
	found, err := c.candidates(ctx, "/search/movie", params, "primary_release_year", year)
	if err != nil {
		return nil, err
	}
	return found, nil
}

// ShowCandidates is the same for a series.
func (c *Client) ShowCandidates(ctx context.Context, title string, year int) ([]metadata.Match, error) {
	params := url.Values{"query": {title}, "include_adult": {"false"}}
	if year > 0 {
		params.Set("first_air_date_year", strconv.Itoa(year))
	}
	return c.candidates(ctx, "/search/tv", params, "first_air_date_year", year)
}

// candidates runs a search and keeps the top of it.
//
// The retry without the year is the same one the single-answer search makes,
// for the same reason: a file named for a festival year and a provider dated
// to a wide release disagree often enough that insisting on the year is how a
// correction ends up offering nothing at all.
func (c *Client) candidates(ctx context.Context, path string, params url.Values, yearParam string, year int) ([]metadata.Match, error) {
	var out searchResponse
	if err := c.get(ctx, path, params, &out); err != nil {
		return nil, err
	}
	if len(out.Results) == 0 && year > 0 {
		params.Del(yearParam)
		if err := c.get(ctx, path, params, &out); err != nil {
			return nil, err
		}
	}

	found := make([]metadata.Match, 0, min(len(out.Results), maxCandidates))
	for _, result := range out.Results {
		if len(found) == maxCandidates {
			break
		}
		name, released := result.Title, result.ReleaseDate
		if name == "" {
			name, released = result.Name, result.FirstAirDate
		}
		found = append(found, metadata.Match{
			ProviderID:  strconv.FormatInt(result.ID, 10),
			Title:       name,
			Year:        yearOf(released),
			Summary:     result.Overview,
			PosterURL:   imageURL(posterBase, result.PosterPath),
			BackdropURL: imageURL(backdropBase, result.BackdropPath),
		})
	}
	return found, nil
}

// MovieByID fetches one film the provider already named.
func (c *Client) MovieByID(ctx context.Context, providerID string) (metadata.Match, error) {
	return c.byID(ctx, "/movie/", providerID)
}

// ShowByID fetches one series the provider already named.
func (c *Client) ShowByID(ctx context.Context, providerID string) (metadata.Match, error) {
	return c.byID(ctx, "/tv/", providerID)
}

// byID reads a detail endpoint, whose fields are the ones a search result
// carries plus a great many this server has no use for.
func (c *Client) byID(ctx context.Context, prefix, providerID string) (metadata.Match, error) {
	if providerID == "" {
		return metadata.Match{}, metadata.ErrNoMatch
	}
	// Digits only: the id goes into a path, and the only ids this provider
	// issues are numbers. A provider id that arrived from a form is not a
	// reason to make a request to a path somebody else chose.
	if _, err := strconv.ParseInt(providerID, 10, 64); err != nil {
		return metadata.Match{}, fmt.Errorf("%w: %q is not a TMDB id", metadata.ErrNoMatch, providerID)
	}

	var out struct {
		ID           int64  `json:"id"`
		Title        string `json:"title"`
		Name         string `json:"name"`
		Overview     string `json:"overview"`
		ReleaseDate  string `json:"release_date"`
		FirstAirDate string `json:"first_air_date"`
		PosterPath   string `json:"poster_path"`
		BackdropPath string `json:"backdrop_path"`
	}
	if err := c.get(ctx, prefix+providerID, url.Values{}, &out); err != nil {
		if errors.Is(err, errNotFound) {
			return metadata.Match{}, metadata.ErrNoMatch
		}
		return metadata.Match{}, err
	}

	name, released := out.Title, out.ReleaseDate
	if name == "" {
		name, released = out.Name, out.FirstAirDate
	}
	return metadata.Match{
		ProviderID:  strconv.FormatInt(out.ID, 10),
		Title:       name,
		Year:        yearOf(released),
		Summary:     out.Overview,
		PosterURL:   imageURL(posterBase, out.PosterPath),
		BackdropURL: imageURL(backdropBase, out.BackdropPath),
	}, nil
}
