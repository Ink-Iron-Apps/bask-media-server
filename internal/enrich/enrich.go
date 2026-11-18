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
		}
	}

	if err := e.DB.InheritArtwork(ctx); err != nil {
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
	return true, images, nil
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
