package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// The things somebody means to watch.
//
// A playlist with a reserved name rather than a table of its own, and that is
// the whole design: every surface this server speaks already lists playlists,
// so a watchlist built this way appears on the television, in Plex and in the
// browser without a line of protocol being added to any of them. A table
// would have needed three new APIs and would have been invisible to every
// client that did not learn one.
//
// One per account, made the first time somebody adds to it, so an account
// that never uses it has no empty list cluttering its playlists.

// WatchlistTitle is the reserved name. Matched case-insensitively, so
// somebody who made their own "watchlist" before this existed keeps using it
// rather than ending up with two.
const WatchlistTitle = "Watchlist"

// Watchlist is an account's watchlist, created if it does not exist yet.
func (d *DB) Watchlist(ctx context.Context, userID int64) (*domain.Playlist, error) {
	if found, err := d.findWatchlist(ctx, userID); err == nil {
		return found, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	return d.CreatePlaylist(ctx, userID, WatchlistTitle)
}

// WatchlistIfAny is the account's watchlist, or ErrNotFound: for the reads
// that should not create one by asking.
func (d *DB) WatchlistIfAny(ctx context.Context, userID int64) (*domain.Playlist, error) {
	return d.findWatchlist(ctx, userID)
}

func (d *DB) findWatchlist(ctx context.Context, userID int64) (*domain.Playlist, error) {
	lists, err := d.Playlists(ctx, userID)
	if err != nil {
		return nil, err
	}
	for _, list := range lists {
		if strings.EqualFold(list.Title, WatchlistTitle) {
			found := list
			return &found, nil
		}
	}
	return nil, ErrNotFound
}

// AddToWatchlist puts an item on it, creating the list the first time.
//
// Reports whether anything was added, so a second press of the same button
// is not an error: wanting to watch something twice is the same wish.
func (d *DB) AddToWatchlist(ctx context.Context, userID, itemID int64) (bool, error) {
	list, err := d.Watchlist(ctx, userID)
	if err != nil {
		return false, err
	}
	if already, err := d.OnWatchlist(ctx, userID, itemID); err != nil {
		return false, err
	} else if already {
		return false, nil
	}
	added, err := d.AddToPlaylist(ctx, userID, list.ID, []int64{itemID})
	if err != nil {
		return false, err
	}
	return added > 0, nil
}

// RemoveFromWatchlist takes an item off it, by the item rather than by the
// line it is on: a button that says "remove this film" has a film and not a
// playlist entry.
func (d *DB) RemoveFromWatchlist(ctx context.Context, userID, itemID int64) (bool, error) {
	list, err := d.findWatchlist(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	rows, err := d.sql.QueryContext(ctx,
		`SELECT id FROM playlist_entries WHERE playlist_id = ? AND item_id = ?`, list.ID, itemID)
	if err != nil {
		return false, fmt.Errorf("read watchlist: %w", err)
	}
	var entries []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, fmt.Errorf("read watchlist: %w", err)
		}
		entries = append(entries, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("read watchlist: %w", err)
	}
	if len(entries) == 0 {
		return false, nil
	}
	removed, err := d.RemoveFromPlaylist(ctx, userID, list.ID, entries)
	if err != nil {
		return false, err
	}
	return removed > 0, nil
}

// OnWatchlist reports whether an item is on it.
func (d *DB) OnWatchlist(ctx context.Context, userID, itemID int64) (bool, error) {
	list, err := d.findWatchlist(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var n int
	if err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM playlist_entries WHERE playlist_id = ? AND item_id = ?`,
		list.ID, itemID).Scan(&n); err != nil {
		return false, fmt.Errorf("read watchlist: %w", err)
	}
	return n > 0, nil
}

// WatchlistSet is which of these items are on the account's watchlist, in one
// query, for a page that draws a button per row.
func (d *DB) WatchlistSet(ctx context.Context, userID int64, itemIDs []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(itemIDs) == 0 {
		return out, nil
	}
	list, err := d.findWatchlist(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}

	placeholders, args := inClause(itemIDs)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT item_id FROM playlist_entries WHERE playlist_id = ? AND item_id IN (`+placeholders+`)`,
		append([]any{list.ID}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("read watchlist: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read watchlist: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}
