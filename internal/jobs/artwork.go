package jobs

import (
	"context"
	"log/slog"

	"github.com/inknironapps/bask-media-server/internal/metadata"
	"github.com/inknironapps/bask-media-server/internal/metadata/tmdb"
	"github.com/inknironapps/bask-media-server/internal/metadata/tvdb"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// ArtworkSources is where pictures can be browsed, resolved at the moment
// somebody opens a chooser rather than when the server came up.
//
// Later for the same reason the provider is: a key can be saved from the
// admin interface on a server that is already running, and a list built at
// startup would be a list built before there was a key.
//
// A source whose key is missing is left out rather than offered and then
// failing, and a source whose key is wrong is left out with a line in the
// log: a chooser is an interactive thing, and the honest answer to "this
// service is not set up" is not showing it.
func ArtworkSources(db *store.DB, tmdbFallback string, concurrency int, log *slog.Logger) func(context.Context) []metadata.ArtworkSource {
	return func(ctx context.Context) []metadata.ArtworkSource {
		if log == nil {
			log = slog.Default()
		}
		var out []metadata.ArtworkSource

		// TMDB first, because it is the same key the library was matched
		// with: a server that enriches at all can browse pictures with no
		// further setting.
		key := tmdbFallback
		if stored, err := db.ServerValue(ctx, store.KeyTMDB); err != nil {
			log.Warn("could not read the TMDB key", "error", err)
		} else if stored != "" {
			key = stored
		}
		if key != "" {
			if client, err := tmdb.New(key, concurrency); err == nil {
				out = append(out, client)
			} else {
				log.Warn("TMDB artwork is not available", "error", err)
			}
		}

		tvdbKey, err := db.ServerValue(ctx, store.KeyTVDB)
		if err != nil {
			log.Warn("could not read TheTVDB key", "error", err)
			return out
		}
		if tvdbKey == "" {
			return out
		}
		pin, err := db.ServerValue(ctx, store.KeyTVDBPIN)
		if err != nil {
			log.Warn("could not read TheTVDB pin", "error", err)
		}
		if client, err := tvdb.New(tvdbKey, pin); err == nil {
			out = append(out, client)
		} else {
			log.Warn("TheTVDB artwork is not available", "error", err)
		}
		return out
	}
}
