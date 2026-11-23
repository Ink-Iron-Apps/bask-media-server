package tmdb

import (
	"context"
	"errors"
	"net/url"
	"sort"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/metadata"
)

// Every picture TMDB has of one thing, for somebody choosing between them.
//
// The enrichment pass takes the one image the search result named and is
// done; this is the other job, which is a person looking at forty posters
// and picking the one they want on their shelf. Different enough to be its
// own request: /movie/{id}/images is a list where the search endpoint is one
// answer, and nothing automatic should pay for it.

const (
	// Two sizes per image. The thumbnail is what a grid of candidates draws
	// — forty w500 posters is several megabytes for a decision — and the
	// full size is what is fetched once somebody has chosen.
	thumbPosterBase   = "https://image.tmdb.org/t/p/w185"
	thumbBackdropBase = "https://image.tmdb.org/t/p/w300"
)

type imagesResponse struct {
	Posters   []imageEntry `json:"posters"`
	Backdrops []imageEntry `json:"backdrops"`
}

type imageEntry struct {
	FilePath    string  `json:"file_path"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	Language    string  `json:"iso_639_1"`
	VoteAverage float64 `json:"vote_average"`
	VoteCount   int     `json:"vote_count"`
}

// Art lists TMDB's pictures of a film or a series.
//
// The id is used when the caller has it, because an item matched by the
// enrichment pass already carries TMDB's own id and searching again could
// rank differently and offer the pictures of a different film.
func (c *Client) Art(ctx context.Context, want metadata.ArtQuery) ([]metadata.Art, error) {
	var (
		path   string
		id     = want.ProviderID
		isShow = want.Kind == domain.KindShow
	)
	if id == "" {
		var (
			match metadata.Match
			err   error
		)
		if isShow {
			match, err = c.Show(ctx, want.Title, want.Year)
		} else {
			match, err = c.Movie(ctx, want.Title, want.Year)
		}
		if err != nil {
			// No match is no pictures, which is an answer rather than a
			// failure: the chooser says this source has nothing.
			if errors.Is(err, metadata.ErrNoMatch) {
				return nil, nil
			}
			return nil, err
		}
		id = match.ProviderID
	}
	if id == "" {
		return nil, nil
	}
	if isShow {
		path = "/tv/" + id + "/images"
	} else {
		path = "/movie/" + id + "/images"
	}

	var out imagesResponse
	// Every language, not just one: the picture most people want is the
	// textless one, and asking for English alone hides it.
	params := url.Values{"include_image_language": {"en,null"}}
	if err := c.get(ctx, path, params, &out); err != nil {
		if errors.Is(err, errNotFound) {
			return nil, nil
		}
		return nil, err
	}

	art := make([]metadata.Art, 0, len(out.Posters)+len(out.Backdrops))
	for _, entry := range out.Posters {
		art = append(art, c.art(entry, metadata.ArtPoster))
	}
	for _, entry := range out.Backdrops {
		art = append(art, c.art(entry, metadata.ArtBackdrop))
	}
	sortArt(art)
	return art, nil
}

func (c *Client) art(entry imageEntry, kind metadata.ArtKind) metadata.Art {
	full, thumb := posterBase, thumbPosterBase
	if kind == metadata.ArtBackdrop {
		full, thumb = backdropBase, thumbBackdropBase
	}
	return metadata.Art{
		Kind:     kind,
		URL:      full + entry.FilePath,
		ThumbURL: thumb + entry.FilePath,
		Width:    entry.Width,
		Height:   entry.Height,
		Language: entry.Language,
		Votes:    entry.VoteAverage,
		Source:   "tmdb",
	}
}

// sortArt puts the pictures in the order somebody would want to look at
// them: posters first, then the textless ones, then the best regarded.
//
// Textless before English rather than after, because a client draws the
// title over the picture itself and a poster that already has the title
// written on it reads as a mistake once it is on the shelf.
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
