package tvdb

import (
	"context"
	"errors"
	"net/url"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/metadata"
)

// Where a series' specials belong.
//
// This is the one thing TheTVDB has that TMDB does not. A special's record
// carries the season it airs after, or the season and episode it airs
// before, and every client that gets Doctor Who's Christmas episodes right
// is reading those three fields. TMDB has an open request for them and
// nothing to offer in the meantime.
//
// So TheTVDB is asked for this and nothing else. The library is still
// matched against one provider: no title here replaces a title, no id is
// stored, and a show that cannot be found just has no placement. The cost of
// that narrowness is the match being by name, which is why a placement from
// here never overwrites one somebody set themselves.

// Placements fetches where each of a series' specials belongs, keyed by the
// episode number within season zero.
//
// Keyed by number because that is what both sides have: BMS files a special
// as season zero episode N from its filename, and TheTVDB numbers them the
// same way. It is also the weak joint — a library numbered differently from
// the archive will place the wrong specials — which is the other reason a
// hand placement wins.
func (c *Client) Placements(ctx context.Context, want metadata.ArtQuery) (map[int]domain.Placement, error) {
	if want.Kind != domain.KindShow {
		return nil, nil
	}
	id, _, err := c.find(ctx, want)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, nil
	}

	// The specials season only. The default season type is the aired order,
	// where season zero is where the specials live.
	var out struct {
		Data struct {
			Episodes []struct {
				Number            int  `json:"number"`
				SeasonNumber      int  `json:"seasonNumber"`
				AirsAfterSeason   *int `json:"airsAfterSeason"`
				AirsBeforeSeason  *int `json:"airsBeforeSeason"`
				AirsBeforeEpisode *int `json:"airsBeforeEpisode"`
			} `json:"episodes"`
		} `json:"data"`
	}
	params := url.Values{"season": {"0"}, "page": {"0"}}
	if err := c.get(ctx, "/series/"+id+"/episodes/default", params, &out); err != nil {
		if errors.Is(err, errNotFound) {
			return nil, nil
		}
		return nil, err
	}

	placements := map[int]domain.Placement{}
	for _, episode := range out.Data.Episodes {
		if episode.SeasonNumber != 0 {
			continue
		}
		placement := domain.NewPlacement()
		if episode.AirsAfterSeason != nil {
			placement.AfterSeason = *episode.AirsAfterSeason
		}
		if episode.AirsBeforeSeason != nil {
			placement.BeforeSeason = *episode.AirsBeforeSeason
		}
		if episode.AirsBeforeEpisode != nil {
			placement.BeforeEpisode = *episode.AirsBeforeEpisode
		}
		// A record that says nothing is not a placement. Storing it would
		// overwrite nothing with nothing and cost a write per special.
		if !placement.Known() {
			continue
		}
		// "Before episode" without "before season" cannot be acted on: the
		// episode number names no season. TheTVDB's own documentation says
		// the two go together, and a record with only the one is a record
		// somebody filled in half way.
		if placement.BeforeSeason == domain.NoPlacement {
			placement.BeforeEpisode = domain.NoPlacement
		}
		placements[episode.Number] = placement
	}
	return placements, nil
}

var _ metadata.SpecialsSource = (*Client)(nil)
