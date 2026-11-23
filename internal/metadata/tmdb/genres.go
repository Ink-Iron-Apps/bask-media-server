package tmdb

import (
	"context"
	"net/url"
	"sync"
)

// Reading genres out of TMDB.
//
// A search result carries genre_ids and not genre names, and the names live
// in two small lists of their own — nineteen for film, sixteen for
// television — that change about never. So they are fetched once per process
// and kept, rather than asked for per title or paid for with a detail
// request per match: an enrichment pass over a cold library of a thousand
// films costs one extra request in total, not a thousand.
//
// A failure to fetch the list is not a failure to enrich. The pass carries on
// with no genres for that item and tries the list again on the next one,
// because a film with a summary and a poster and no genres is a film in the
// library, and refusing it over a shelf label would be the wrong trade.

// genreNames maps TMDB's ids to names for one media type.
type genreNames struct {
	mu    sync.Mutex
	names map[int64]string
}

// lookup is the names, fetched on the first ask and kept.
//
// The lock is held across the request on purpose. A worker that arrives
// while another is fetching should wait for that answer rather than make the
// same request, and the only thing it is waiting on is the genre list — the
// enrichment pass's own concurrency gate is elsewhere. A failed fetch leaves
// the map empty, so the next ask tries again rather than deciding this
// library has no genres.
func (g *genreNames) lookup(ctx context.Context, c *Client, path string) map[int64]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.names) > 0 {
		return g.names
	}

	var out struct {
		Genres []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"genres"`
	}
	if err := c.get(ctx, path, url.Values{}, &out); err != nil {
		return nil
	}

	// A fresh map each time, never written again once it is published: a
	// caller holding the one from before a refetch is reading something
	// nobody will touch.
	fetched := make(map[int64]string, len(out.Genres))
	for _, genre := range out.Genres {
		if genre.Name != "" {
			fetched[genre.ID] = genre.Name
		}
	}
	if len(fetched) == 0 {
		return nil
	}
	g.names = fetched
	return fetched
}

// movieGenres and showGenres name the ids a search result carried.
//
// An id the list does not know is dropped rather than rendered as a number:
// a shelf called "878" is worse than one film with no genre.
func (c *Client) movieGenres(ctx context.Context, ids []int64) []string {
	return c.named(ctx, &c.movieGenreNames, "/genre/movie/list", ids)
}

func (c *Client) showGenres(ctx context.Context, ids []int64) []string {
	return c.named(ctx, &c.showGenreNames, "/genre/tv/list", ids)
}

func (c *Client) named(ctx context.Context, cache *genreNames, path string, ids []int64) []string {
	if len(ids) == 0 {
		return nil
	}
	names := cache.lookup(ctx, c, path)
	if len(names) == 0 {
		return nil
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if name, found := names[id]; found {
			out = append(out, name)
		}
	}
	return out
}
