package store

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// Watching a show in the order it happened.
//
// The problem this solves, in its own words: Doctor Who's Christmas specials
// went out between series, they advance the story, and they live in season
// zero. Sorting by season and then by episode puts every one of them before
// the pilot, which is both wrong and the first thing anybody notices.
//
// Three sources of truth, in this order, because they differ in how much
// they know:
//
//   - What somebody said by hand. Exact, and nothing overwrites it.
//   - What TheTVDB records about the special: after which season, or before
//     which season and episode. Exact where it exists, and it exists for
//     every show anybody has curated.
//   - When it aired. Not placement but a very good proxy for it, free,
//     and automatic: a special that went out on 25 December 2005 falls
//     between the series that ended in June and the one that began in
//     April, which is exactly right. It is wrong for a special that aired
//     out of step with the story — a retrospective years later, a Christmas
//     episode broadcast mid-series — which is what the first source is for.
//
// A special with none of the three goes last rather than first. Nothing is
// known about where it belongs, and the end of the list is the one place
// that cannot be mistaken for a deliberate answer.
//
// And one thing none of the three can tell apart: a story episode from a
// minisode. Doctor Who's Tardisodes are a minute each, went out between the
// episodes of series two, and both the air date and TheTVDB place them
// there — faithfully, and not at all what somebody watching the series
// wants. The only fact that separates "The Christmas Invasion" from
// "Tardisode 1" is that one is an hour and the other is a minute, so the
// length is what decides: see [tooShortToWeave]. It is a heuristic and it
// is wrong sometimes, which is what the hand placement is for — and a hand
// placement overrides the length as well.

// SetAired records when an episode first went out.
//
// Only ever filled in, never cleared: a provider that has stopped saying is
// not a reason to forget, and the same rule the genres and the credits
// follow.
func (d *DB) SetAired(ctx context.Context, itemID int64, when time.Time) error {
	if when.IsZero() {
		return nil
	}
	_, err := d.sql.ExecContext(ctx,
		`UPDATE items SET aired = ? WHERE id = ?`, millis(when.UTC()), itemID)
	if err != nil {
		return fmt.Errorf("set the air date: %w", err)
	}
	return nil
}

// SetPlacement records where a special belongs.
//
// byHand marks it as somebody's own answer, which a later metadata pass then
// leaves alone — without freezing the item's title and artwork the way the
// item lock would. A pass calls this with byHand false and is refused where
// a hand-set placement is already there.
func (d *DB) SetPlacement(ctx context.Context, itemID int64, placement domain.Placement, byHand bool) error {
	guard := ""
	if !byHand {
		// A pass must not undo somebody's own answer.
		guard = " AND placed_by_hand = 0"
	}
	res, err := d.sql.ExecContext(ctx,
		`UPDATE items SET airs_after_season = ?, airs_before_season = ?,
		   airs_before_episode = ?, placed_by_hand = ?, updated_at = ?
		 WHERE id = ?`+guard,
		placement.AfterSeason, placement.BeforeSeason, placement.BeforeEpisode,
		boolToInt(byHand), millis(time.Now().UTC()), itemID)
	if err != nil {
		return fmt.Errorf("place a special: %w", err)
	}
	if byHand {
		if affected, err := res.RowsAffected(); err == nil && affected == 0 {
			return ErrNotFound
		}
	}
	return nil
}

// EpisodeOrder is every episode of a show in the order it should be watched,
// with the specials where they belong.
//
// Computed here rather than in SQL. The rules interleave two different kinds
// of answer — a position relative to another episode, and a date compared
// against other episodes' dates — and expressing that as one ORDER BY would
// be a window function nobody could read or change. A show is a few hundred
// rows at most, which is one query and a sort.
func (d *DB) EpisodeOrder(ctx context.Context, showID int64) ([]domain.Item, error) {
	seasons, _, err := d.Children(ctx, showID, Page{Size: 500})
	if err != nil {
		return nil, err
	}

	var (
		regular  []domain.Item
		specials []domain.Item
	)
	for _, season := range seasons {
		episodes, _, err := d.Children(ctx, season.ID, Page{Size: 2000})
		if err != nil {
			return nil, err
		}
		for _, episode := range episodes {
			if episode.Kind != domain.KindEpisode {
				continue
			}
			if season.Index == 0 {
				specials = append(specials, episode)
				continue
			}
			regular = append(regular, episode)
		}
	}
	return weave(regular, specials, seasons), nil
}

// weave puts the specials among the regular episodes.
//
// The regular episodes are the spine: they are already in order and their
// order is not in question. Each special is given the position of the
// episode it belongs before, and sorts just ahead of it; one that belongs
// after a season is given the position just past that season's last
// episode.
func weave(regular, specials []domain.Item, seasons []domain.Item) []domain.Item {
	// slot is a sortable position: which season, which episode within it,
	// and a tie-break that puts a special ahead of the episode it shares a
	// position with.
	type slot struct {
		season  int
		episode int
		ahead   int // 0 for a special placed here, 1 for the episode itself
		item    domain.Item
	}

	sorted := make([]slot, 0, len(regular)+len(specials))
	seasonOf := map[int64]int{}
	for _, season := range seasons {
		seasonOf[season.ID] = season.Index
	}
	for _, episode := range regular {
		index := 0
		if episode.ParentID != nil {
			index = seasonOf[*episode.ParentID]
		}
		sorted = append(sorted, slot{season: index, episode: episode.Index, ahead: 1, item: episode})
	}

	// The last episode number in each season, so "after season two" has
	// somewhere to land.
	last := map[int]int{}
	for _, one := range sorted {
		if one.episode > last[one.season] {
			last[one.season] = one.episode
		}
	}

	// Unplaceable specials go at the end rather than the beginning. Nothing
	// is known about where they belong, and the end is the one place that
	// cannot be read as a deliberate answer.
	var trailing []domain.Item

	// How long a typical episode of this show is, so a minisode can be told
	// from an episode.
	typical := typicalLength(regular)

	for _, special := range specials {
		placement := special.Placement
		switch {
		// A hand placement wins over everything, the length included:
		// somebody who places a seven-minute special between two series
		// means it.
		case placement.ByHand && placement.Known():
			// Fall through to the cases below, which read the same fields.
		case tooShortToWeave(special.Duration, typical):
			// A minisode, a webisode, a charity sketch: it goes in the list
			// so it can still be played, but at the end rather than in the
			// middle of a series it is not part of.
			trailing = append(trailing, special)
			continue
		}
		switch {
		case placement.BeforeSeason != domain.NoPlacement:
			episode := placement.BeforeEpisode
			if episode == domain.NoPlacement {
				// Before a season, with no episode named: the start of it.
				episode = 0
			}
			sorted = append(sorted, slot{
				season: placement.BeforeSeason, episode: episode, ahead: 0, item: special,
			})
		case placement.AfterSeason != domain.NoPlacement:
			sorted = append(sorted, slot{
				season: placement.AfterSeason, episode: last[placement.AfterSeason] + 1,
				ahead: 0, item: special,
			})
		case !special.Aired.IsZero():
			season, episode, found := byDate(special.Aired, regular, seasonOf)
			if !found {
				trailing = append(trailing, special)
				continue
			}
			sorted = append(sorted, slot{season: season, episode: episode, ahead: 0, item: special})
		default:
			trailing = append(trailing, special)
		}
	}

	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.season != b.season {
			return a.season < b.season
		}
		if a.episode != b.episode {
			return a.episode < b.episode
		}
		// A special placed at a position comes before the episode itself,
		// and two specials at one position keep the order they arrived in.
		if a.ahead != b.ahead {
			return a.ahead < b.ahead
		}
		return a.item.ID < b.item.ID
	})

	out := make([]domain.Item, 0, len(sorted)+len(trailing))
	for _, one := range sorted {
		out = append(out, one.item)
	}
	// Among themselves, by date and then by number: a run of specials at the
	// end is still worth putting in some order.
	sort.SliceStable(trailing, func(i, j int) bool {
		if !trailing[i].Aired.Equal(trailing[j].Aired) {
			return trailing[i].Aired.Before(trailing[j].Aired)
		}
		return trailing[i].Index < trailing[j].Index
	})
	return append(out, trailing...)
}

// byDate is the position a special's air date puts it in: the episode it
// aired before.
//
// Reports found=false when no regular episode has a date to compare against,
// which is a show nothing has been enriched for — guessing from episode
// numbers alone would be inventing an answer.
func byDate(aired time.Time, regular []domain.Item, seasonOf map[int64]int) (season, episode int, found bool) {
	dated := false
	for _, one := range regular {
		if one.Aired.IsZero() {
			continue
		}
		dated = true
		if one.Aired.After(aired) {
			index := 0
			if one.ParentID != nil {
				index = seasonOf[*one.ParentID]
			}
			return index, one.Index, true
		}
	}
	if !dated {
		return 0, 0, false
	}
	// Later than everything: after the last season's last episode, which is
	// where a special that has not been overtaken yet belongs.
	highest, lastEpisode := 0, 0
	for _, one := range regular {
		index := 0
		if one.ParentID != nil {
			index = seasonOf[*one.ParentID]
		}
		if index > highest || (index == highest && one.Index > lastEpisode) {
			highest, lastEpisode = index, one.Index
		}
	}
	return highest, lastEpisode + 1, true
}

// Specials is a show's season-zero episodes.
//
// Read on their own because that is what a placement pass writes to: it has
// a list keyed by episode number and needs the rows to match them against.
func (d *DB) Specials(ctx context.Context, showID int64) ([]domain.Item, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items
		  WHERE kind = 'episode' AND extra = '' AND parent_id IN (
		      SELECT id FROM items WHERE parent_id = ? AND kind = 'season' AND idx = 0
		  )
		  ORDER BY idx`, showID)
	if err != nil {
		return nil, fmt.Errorf("list specials: %w", err)
	}
	defer rows.Close()
	return d.collect(ctx, rows)
}

// typicalLength is how long an episode of this show runs, as the median of
// the regular episodes.
//
// The median rather than the mean, because a feature-length finale or a
// double-length pilot would drag a mean up and start excluding real
// episodes. Zero when nothing has a duration yet, which is a library that
// has not been probed and where nothing can be judged by length.
func typicalLength(regular []domain.Item) time.Duration {
	lengths := make([]time.Duration, 0, len(regular))
	for _, episode := range regular {
		if episode.Duration > 0 {
			lengths = append(lengths, episode.Duration)
		}
	}
	if len(lengths) == 0 {
		return 0
	}
	sort.Slice(lengths, func(i, j int) bool { return lengths[i] < lengths[j] })
	return lengths[len(lengths)/2]
}

// weaveFloor is the fraction of a typical episode a special has to reach to
// be woven in among them.
//
// A third. Doctor Who's episodes run about 45 minutes, so a special has to
// be a quarter of an hour to count — which keeps the Tardisodes, the
// Children in Need sketches and the behind-the-scenes featurettes out of a
// series, and lets every Christmas special in.
//
// Relative rather than absolute because an absolute floor is wrong for a
// show of short episodes: eleven minutes is a full episode of some comedies,
// and a flat "under fifteen minutes" would exclude the entire series.
const weaveFloor = 3

// tooShortToWeave reports whether a special is too short to be a story
// episode of this show.
//
// False when the show's own length is unknown: a library that has not been
// probed cannot judge anything by length, and leaving every special out of
// the order would be worse than leaving them in.
func tooShortToWeave(length, typical time.Duration) bool {
	if typical <= 0 || length <= 0 {
		return false
	}
	return length*weaveFloor < typical
}
