package tmdb

import (
	"context"
	"net/url"
	"strings"
)

// Reading a content rating out of TMDB.
//
// Its own request, because the search endpoints do not carry one: a film's
// certifications live under /movie/{id}/release_dates and a series' under
// /tv/{id}/content_ratings. One more call per matched item on an enrichment
// pass, which is a background pass that already makes one.
//
// Which country's rating, though, is a decision. TMDB has dozens for a
// well-known film and they disagree — a film is R in America, 15 in Britain
// and 16 in Germany. The ones asked for are tried in order, so a household
// gets the system it recognises, and a library with no rating for any of them
// gets none rather than a number from a country nobody here reads.

// ratingCountries are the certification systems to look for, in order.
//
// US first because TMDB's American data is the most complete by a wide
// margin, then Britain, then the ones most likely to be filled in for a
// European release.
var ratingCountries = []string{"US", "GB", "IE", "CA", "AU", "DE", "FR", "NL"}

// movieRating is a film's certification, or "".
func (c *Client) movieRating(ctx context.Context, providerID string) string {
	var out struct {
		Results []struct {
			Country      string `json:"iso_3166_1"`
			ReleaseDates []struct {
				Certification string `json:"certification"`
				Type          int    `json:"type"`
			} `json:"release_dates"`
		} `json:"results"`
	}
	if err := c.get(ctx, "/movie/"+providerID+"/release_dates", url.Values{}, &out); err != nil {
		return ""
	}

	found := map[string]string{}
	for _, result := range out.Results {
		for _, release := range result.ReleaseDates {
			certification := strings.TrimSpace(release.Certification)
			if certification == "" {
				continue
			}
			country := strings.ToUpper(result.Country)
			// The first non-empty certification for a country wins: a film
			// re-rated for a later release has several, and they are the same
			// rating often enough that choosing between them is not worth a
			// rule nobody could predict.
			if _, already := found[country]; !already {
				found[country] = certification
			}
		}
	}
	return preferred(found)
}

// showRating is a series' certification, or "".
func (c *Client) showRating(ctx context.Context, providerID string) string {
	var out struct {
		Results []struct {
			Country string `json:"iso_3166_1"`
			Rating  string `json:"rating"`
		} `json:"results"`
	}
	if err := c.get(ctx, "/tv/"+providerID+"/content_ratings", url.Values{}, &out); err != nil {
		return ""
	}

	found := map[string]string{}
	for _, result := range out.Results {
		if rating := strings.TrimSpace(result.Rating); rating != "" {
			found[strings.ToUpper(result.Country)] = rating
		}
	}
	return preferred(found)
}

// preferred picks the first country this server reads, and nothing when none
// of them has one.
func preferred(found map[string]string) string {
	for _, country := range ratingCountries {
		if rating, ok := found[country]; ok {
			return rating
		}
	}
	return ""
}
