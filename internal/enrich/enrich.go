// Package enrich fills in what the filenames could not say.
//
// A separate pass from the scan on purpose. Scanning is local and fast and
// must work with no network and no API key; enrichment is neither, so it runs
// afterwards, can be interrupted at any point, and picks up where it stopped
// because the work left to do is a query rather than a position in a list.
package enrich

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/inknironapps/bask-media-server/internal/artwork"
	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/metadata"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// Result is what one pass did.
type Result struct {
	Matched   int
	Unmatched int
	Images    int
	Freed     int64
	Elapsed   time.Duration
}

// Enricher asks a provider about everything no provider has answered for yet.
type Enricher struct {
	DB       *store.DB
	Provider metadata.Provider
	Artwork  *artwork.Cache
	Log      *slog.Logger

	// Batch is how many items one round handles. Rounds exist so a pass over a
	// cold library commits as it goes and can be stopped without losing what
	// it has already matched.
	Batch int

	// Specials is where a series' specials belong, resolved when a pass
	// runs. Nil, or a nil source, means the specials are placed by their
	// air dates alone — which for a show whose Christmas episodes went out
	// between series is already the right answer.
	Specials func(ctx context.Context) metadata.SpecialsSource

	// Progress is told what the pass has done so far, after each item. Called
	// from the pass's own goroutine, so it must not block. Optional.
	Progress func(Result)
}

// Run enriches until nothing is left or the context ends.
func (e *Enricher) Run(ctx context.Context) (Result, error) {
	started := time.Now()
	log := e.Log
	if log == nil {
		log = slog.Default()
	}
	batch := e.Batch
	if batch <= 0 {
		batch = 100
	}

	var result Result
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		items, err := e.DB.NeedsMetadata(ctx, batch)
		if err != nil {
			return result, err
		}
		if len(items) == 0 {
			break
		}

		for _, item := range items {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			matched, images, err := e.one(ctx, item)
			switch {
			case err != nil && errors.Is(err, context.Canceled):
				return result, err
			case err != nil:
				// A failure is not a final answer: leave metadata_at at zero
				// so the next pass tries again rather than writing off a title
				// because the network blinked.
				log.Warn("could not enrich", "title", item.Title, "kind", string(item.Kind), "error", err)
				continue
			}
			result.Images += images
			if matched {
				result.Matched++
			} else {
				result.Unmatched++
			}
			if e.Progress != nil {
				e.Progress(result)
			}
		}
	}

	if err := e.DB.InheritArtwork(ctx); err != nil {
		return result, err
	}
	// A provider rates a series, not every episode of it. Without this a
	// rating ceiling would hide a show and show every episode inside it,
	// which is the whole restriction defeated by opening a folder.
	if err := e.DB.InheritRatings(ctx); err != nil {
		return result, err
	}
	if e.Artwork != nil {
		freed, err := e.Artwork.Sweep()
		if err != nil {
			log.Warn("could not sweep the artwork cache", "error", err)
		}
		result.Freed = freed
	}

	result.Elapsed = time.Since(started)
	log.Info("enriched library",
		"matched", result.Matched, "unmatched", result.Unmatched, "images", result.Images,
		"freedMB", result.Freed>>20, "elapsed", result.Elapsed.Round(time.Millisecond))
	return result, nil
}

// one enriches a single item, reporting whether it matched and how many images
// were fetched.
func (e *Enricher) one(ctx context.Context, item domain.Item) (matched bool, images int, err error) {
	var match metadata.Match

	switch item.Kind {
	case domain.KindMovie:
		match, err = e.Provider.Movie(ctx, item.Title, item.Year)
	case domain.KindShow:
		match, err = e.Provider.Show(ctx, item.Title, item.Year)
	case domain.KindEpisode:
		match, err = e.episode(ctx, item)
	case domain.KindSeason:
		// Seasons take their show's artwork, which InheritArtwork does in one
		// statement at the end of the pass. Nothing to ask a provider.
		return false, 0, e.DB.MarkMetadataAttempted(ctx, item.ID)
	default:
		return false, 0, nil
	}

	if errors.Is(err, metadata.ErrNoMatch) {
		// Asked and answered: nothing out there matches this title, so record
		// the attempt rather than asking again on every pass forever.
		return false, 0, e.DB.MarkMetadataAttempted(ctx, item.ID)
	}
	if err != nil {
		return false, 0, err
	}

	posterKey, artKey := "", ""
	if e.Artwork != nil {
		if match.PosterURL != "" {
			if key, ferr := e.Artwork.Fetch(ctx, match.PosterURL); ferr == nil {
				posterKey, images = key, images+1
			} else if !errors.Is(ferr, context.Canceled) {
				e.logger().Debug("poster did not fetch", "title", item.Title, "error", ferr)
			}
		}
		if match.BackdropURL != "" {
			if key, ferr := e.Artwork.Fetch(ctx, match.BackdropURL); ferr == nil {
				artKey, images = key, images+1
			} else if !errors.Is(ferr, context.Canceled) {
				e.logger().Debug("backdrop did not fetch", "title", item.Title, "error", ferr)
			}
		}
	}

	if err := e.DB.SaveMetadata(ctx, item.ID, match.ProviderID, match.Summary, posterKey, artKey); err != nil {
		return false, images, err
	}
	// The rating is written separately because it is written as two things —
	// the label a client shows and the age a restriction compares against —
	// and only when the provider had one: an empty rating must not erase a
	// rating that is already there.
	if match.Rating != "" {
		if err := e.DB.SetRating(ctx, item.ID, match.Rating); err != nil {
			return false, images, err
		}
	}
	// Genres, for the same reason and with the same rule: an empty list is a
	// provider that did not say, and a provider that did not say must not
	// clear the genres a file's own tags or somebody's correction put there.
	if len(match.Genres) > 0 {
		if err := e.DB.SetGenres(ctx, item.ID, match.Genres); err != nil {
			return false, images, err
		}
	}
	// When it went out, which is what places a special between two seasons
	// without anybody being asked. Filled in, never cleared: a provider
	// that has stopped saying is not a reason to forget.
	if err := e.DB.SetAired(ctx, item.ID, match.Aired); err != nil {
		return false, images, err
	}
	// And where this series' specials belong, which only TheTVDB knows.
	if err := e.placeSpecials(ctx, item); err != nil {
		return false, images, err
	}
	// And the credits, which are the expensive half and so are asked for
	// only where the library said it wanted them.
	fetched, err := e.people(ctx, item, match.ProviderID)
	if err != nil {
		return false, images, err
	}
	images += fetched
	return true, images, nil
}

// people fetches the cast, the crew and the studio, where the library asked
// for them.
//
// A separate request per title, which is why it is a setting. Reports how
// many photographs were fetched, because they go into the same cache and the
// same tally as the posters.
//
// A failure here does not fail the item. The title, the summary and the
// poster are already saved by the time this runs, and losing a cast list to
// a blink in the network is not worth undoing them — the next pass over an
// unlocked item will try again.
func (e *Enricher) people(ctx context.Context, item domain.Item, providerID string) (images int, err error) {
	if providerID == "" {
		return 0, nil
	}
	source, ok := e.Provider.(metadata.PeopleSource)
	if !ok {
		return 0, nil
	}

	switch item.Kind {
	case domain.KindMovie, domain.KindShow:
	default:
		// A season and an episode take the series' credits, which is what
		// every client shows on them anyway, and asking per episode would
		// be a request per episode for the same answer.
		return 0, nil
	}

	section, err := e.DB.Section(ctx, item.SectionID)
	if err != nil {
		return 0, err
	}
	if !section.Settings.PeopleWanted(section.Kind) {
		return 0, nil
	}

	credits, err := source.People(ctx, metadata.ArtQuery{
		Kind: item.Kind, Title: item.Title, Year: item.Year, ProviderID: providerID,
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return 0, err
		}
		e.logger().Debug("could not read the credits", "title", item.Title, "error", err)
		return 0, nil
	}

	if err := e.DB.SetStudio(ctx, item.ID, credits.Studio); err != nil {
		return 0, err
	}
	if len(credits.People) == 0 {
		// Nothing said, so nothing cleared: the same rule as the genres
		// above.
		return 0, nil
	}

	stored := make([]store.Credit, 0, len(credits.People))
	for _, person := range credits.People {
		credit := store.Credit{
			Credit: domain.Credit{
				Name: person.Name, Role: person.Role, Part: person.Part,
			},
			Position: person.Position,
		}
		// A photograph is worth having but never worth failing over: a cast
		// row with a name and no face is still a cast row.
		if e.Artwork != nil && person.ThumbURL != "" {
			if key, ferr := e.Artwork.Fetch(ctx, person.ThumbURL); ferr == nil {
				credit.Thumb, images = key, images+1
			} else if errors.Is(ferr, context.Canceled) {
				return images, ferr
			}
		}
		stored = append(stored, credit)
	}
	if err := e.DB.SetCredits(ctx, item.ID, stored); err != nil {
		return images, err
	}
	return images, nil
}

// placeSpecials asks where a series' specials belong and records it.
//
// Only for a series, and only once per series rather than per special: the
// answer is one request covering all of them, and asking per episode would
// be a request each for the same list.
//
// Only TheTVDB can answer this. The library is still matched against one
// provider — nothing here replaces a title and no second id is stored — so
// the join is the episode number within season zero, which is also why a
// placement from here never overwrites one somebody set by hand.
//
// A failure is not a failure of the item. The specials still have their air
// dates, which is a good enough answer on its own for a show whose
// Christmas episodes went out between series.
func (e *Enricher) placeSpecials(ctx context.Context, show domain.Item) error {
	if show.Kind != domain.KindShow || e.Specials == nil {
		return nil
	}
	source := e.Specials(ctx)
	if source == nil {
		return nil
	}

	placements, err := source.Placements(ctx, metadata.ArtQuery{
		Kind: show.Kind, Title: show.Title, Year: show.Year,
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		e.logger().Debug("could not read where the specials belong",
			"show", show.Title, "error", err)
		return nil
	}
	if len(placements) == 0 {
		return nil
	}

	specials, err := e.DB.Specials(ctx, show.ID)
	if err != nil {
		return err
	}
	placed := 0
	for _, special := range specials {
		placement, found := placements[special.Index]
		if !found {
			continue
		}
		// byHand false, so the store refuses to overwrite somebody's own
		// answer rather than this having to know about it.
		if err := e.DB.SetPlacement(ctx, special.ID, placement, false); err != nil {
			return err
		}
		placed++
	}
	if placed > 0 {
		e.logger().Info("placed specials among the seasons",
			"show", show.Title, "specials", placed)
	}
	return nil
}

// episode needs its show's provider id, which is why shows are enriched first.
func (e *Enricher) episode(ctx context.Context, episode domain.Item) (metadata.Match, error) {
	if episode.ParentID == nil {
		return metadata.Match{}, metadata.ErrNoMatch
	}
	season, err := e.DB.Item(ctx, *episode.ParentID)
	if err != nil {
		return metadata.Match{}, err
	}
	if season.ParentID == nil {
		return metadata.Match{}, metadata.ErrNoMatch
	}
	show, err := e.DB.Item(ctx, *season.ParentID)
	if err != nil {
		return metadata.Match{}, err
	}
	if show.ProviderID == "" {
		// The show did not match, so neither can its episodes. Not an error:
		// the filename already named the episode, and this is the case where
		// the provider has nothing to add.
		return metadata.Match{}, metadata.ErrNoMatch
	}
	return e.Provider.Episode(ctx, show.ProviderID, season.Index, episode.Index)
}

func (e *Enricher) logger() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}
