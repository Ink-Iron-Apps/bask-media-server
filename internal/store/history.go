package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// The activity log: what was watched, by whom, on what, for how long.
//
// The one table in this database that is a record rather than a state. Every
// other row describes how things are now and is rewritten when they change; a
// row here describes a Tuesday evening and is never true or false again. That
// is why the names are written into it rather than joined to, and why nothing
// here updates a row once it has been closed.

// historyColumns is every column a Watch is read from, in scan order.
const historyColumns = `id, IFNULL(user_id, 0), user_name, IFNULL(item_id, 0), title, subtitle,
	kind, surface, client, device, address, mode, reason,
	started_at, ended_at, paused_ms, position_ms, duration_ms, complete`

// NewWatch is a stream that has just started.
type NewWatch struct {
	UserID   int64
	Name     string
	ItemID   int64
	Title    string
	Subtitle string
	Kind     domain.Kind
	Surface  string
	Client   string
	Device   string
	Address  string
	Mode     string
	Started  time.Time
	Position time.Duration
	Length   time.Duration
}

// StartWatch opens a row for a stream and returns its id.
//
// Opened at the start rather than written at the end, so that a server which
// loses power in the middle of a film still has a record of the film. What it
// loses is the last minute of its length, which is the right thing to lose.
func (d *DB) StartWatch(ctx context.Context, in NewWatch) (int64, error) {
	started := in.Started
	if started.IsZero() {
		started = time.Now().UTC()
	}
	res, err := d.sql.ExecContext(ctx,
		`INSERT INTO history
		   (user_id, user_name, item_id, title, subtitle, kind, surface, client, device,
		    address, mode, started_at, ended_at, position_ms, duration_ms, complete)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		nullableID(in.UserID), in.Name, nullableID(in.ItemID), in.Title, in.Subtitle,
		string(in.Kind), in.Surface, in.Client, in.Device, in.Address, in.Mode,
		millis(started.UTC()), millis(started.UTC()),
		in.Position.Milliseconds(), in.Length.Milliseconds())
	if err != nil {
		return 0, fmt.Errorf("start history: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("start history: %w", err)
	}
	return id, nil
}

// WatchProgress is where a stream has got to.
type WatchProgress struct {
	At       time.Time
	Position time.Duration
	Length   time.Duration
	Paused   time.Duration
	Mode     string

	// Reason is why the stream was stopped, where somebody stopped it.
	Reason string

	// Done closes the row: the server saw this stream end, rather than
	// losing sight of it.
	Done bool
}

// TouchWatch brings an open row up to date, and closes it when Done.
//
// Called while a stream runs as well as when it ends, because ended_at means
// "the last moment this was known to be playing". A row that is never touched
// again is not wrong, only short.
//
// Refuses to reopen a closed row: a client that reports once more after the
// stream was stopped should not resurrect it.
func (d *DB) TouchWatch(ctx context.Context, id int64, in WatchProgress) error {
	at := in.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	sets := []string{"ended_at = ?", "paused_ms = ?"}
	args := []any{millis(at.UTC()), in.Paused.Milliseconds()}
	if in.Position > 0 {
		sets, args = append(sets, "position_ms = ?"), append(args, in.Position.Milliseconds())
	}
	if in.Length > 0 {
		sets, args = append(sets, "duration_ms = ?"), append(args, in.Length.Milliseconds())
	}
	if in.Mode != "" {
		sets, args = append(sets, "mode = ?"), append(args, in.Mode)
	}
	if in.Reason != "" {
		sets, args = append(sets, "reason = ?"), append(args, in.Reason)
	}
	if in.Done {
		sets = append(sets, "complete = 1")
	}
	args = append(args, id)

	if _, err := d.sql.ExecContext(ctx,
		`UPDATE history SET `+strings.Join(sets, ", ")+` WHERE id = ? AND complete = 0`, args...); err != nil {
		return fmt.Errorf("update history: %w", err)
	}
	return nil
}

// HistoryFilter narrows what is read back. Zero means everything.
type HistoryFilter struct {
	UserID int64
	ItemID int64

	// Since and Until bound the window by when a stream started.
	Since time.Time
	Until time.Time
}

// History is what was watched, newest first, with the total that matched.
func (d *DB) History(ctx context.Context, filter HistoryFilter, page Page) ([]domain.Watch, int, error) {
	where, args := filter.clause()
	offset, limit := page.Window(200)

	var total int
	if err := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM history`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count history: %w", err)
	}

	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+historyColumns+` FROM history`+where+
			` ORDER BY started_at DESC, id DESC LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("read history: %w", err)
	}
	defer rows.Close()

	var out []domain.Watch
	for rows.Next() {
		watch, err := scanWatch(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, watch)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read history: %w", err)
	}
	return out, total, nil
}

// Watch is one row, for following a stream that is still open.
func (d *DB) Watch(ctx context.Context, id int64) (*domain.Watch, error) {
	row := d.sql.QueryRowContext(ctx, `SELECT `+historyColumns+` FROM history WHERE id = ?`, id)
	watch, err := scanWatch(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &watch, nil
}

func (f HistoryFilter) clause() (string, []any) {
	var (
		terms []string
		args  []any
	)
	if f.UserID != 0 {
		terms, args = append(terms, "user_id = ?"), append(args, f.UserID)
	}
	if f.ItemID != 0 {
		terms, args = append(terms, "item_id = ?"), append(args, f.ItemID)
	}
	if !f.Since.IsZero() {
		terms, args = append(terms, "started_at >= ?"), append(args, millis(f.Since.UTC()))
	}
	if !f.Until.IsZero() {
		terms, args = append(terms, "started_at < ?"), append(args, millis(f.Until.UTC()))
	}
	if len(terms) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(terms, " AND "), args
}

func scanWatch(row scanner) (domain.Watch, error) {
	var (
		watch                               domain.Watch
		kind                                string
		started, ended, paused, pos, length int64
		complete                            bool
	)
	if err := row.Scan(&watch.ID, &watch.UserID, &watch.Name, &watch.ItemID, &watch.Title,
		&watch.Subtitle, &kind, &watch.Surface, &watch.Client, &watch.Device, &watch.Address,
		&watch.Mode, &watch.Reason, &started, &ended, &paused, &pos, &length, &complete); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Watch{}, err
		}
		return domain.Watch{}, fmt.Errorf("read a watch: %w", err)
	}
	watch.Kind = domain.Kind(kind)
	watch.Started = fromMillis(started)
	watch.Ended = fromMillis(ended)
	watch.Paused = millisDuration(paused)
	watch.Position = millisDuration(pos)
	watch.Length = millisDuration(length)
	watch.Complete = complete
	return watch, nil
}

// watchedExpression is how long a row was actually watched, in SQL.
//
// The same arithmetic as domain.Watch.Watched, and the reason both exist: a
// list of rows computes it in Go, and a total over ten thousand rows has to
// compute it in the database. Clamped at zero, because a clock that went
// backwards should not subtract from somebody's evening.
const watchedExpression = `MAX(ended_at - started_at - paused_ms, 0)`

// WatchersSince is how much each viewer watched, most first.
func (d *DB) WatchersSince(ctx context.Context, since time.Time) ([]domain.WatchTotal, error) {
	return d.totalsSince(ctx, since, true)
}

// TitlesSince is how much each title was watched, most first.
func (d *DB) TitlesSince(ctx context.Context, since time.Time, limit int) ([]domain.WatchTotal, error) {
	totals, err := d.totalsSince(ctx, since, false)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(totals) > limit {
		totals = totals[:limit]
	}
	return totals, nil
}

// totalsSince groups the window by viewer or by title.
//
// Grouped by the name rather than the id on purpose. An account that has been
// deleted still watched things, and a row whose user_id is now null would
// otherwise all collapse into one anonymous heap with everybody else's.
func (d *DB) totalsSince(ctx context.Context, since time.Time, byUser bool) ([]domain.WatchTotal, error) {
	group, label, id := "user_name", "user_name", "IFNULL(MAX(user_id), 0)"
	if !byUser {
		group, label, id = "title", "title", "IFNULL(MAX(item_id), 0)"
	}
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+label+`, `+id+`, COUNT(*), SUM(`+watchedExpression+`)
		   FROM history WHERE started_at >= ?
		  GROUP BY `+group+` ORDER BY SUM(`+watchedExpression+`) DESC, COUNT(*) DESC`,
		millis(since.UTC()))
	if err != nil {
		return nil, fmt.Errorf("total history: %w", err)
	}
	defer rows.Close()

	var out []domain.WatchTotal
	for rows.Next() {
		var (
			name    string
			ref     int64
			plays   int
			watched sql.NullInt64
		)
		if err := rows.Scan(&name, &ref, &plays, &watched); err != nil {
			return nil, fmt.Errorf("total history: %w", err)
		}
		total := domain.WatchTotal{Plays: plays, Watched: millisDuration(watched.Int64)}
		if byUser {
			total.Name, total.UserID = name, ref
		} else {
			total.Title, total.ItemID = name, ref
		}
		out = append(out, total)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("total history: %w", err)
	}
	return out, nil
}

// ForgetHistory removes rows that started before a moment, and everything
// when that moment is zero. Reports how many rows went.
//
// Here because a record of who watched what is the most personal thing this
// server holds, and a household that wants it gone should not have to open
// the database to do it.
func (d *DB) ForgetHistory(ctx context.Context, before time.Time) (int, error) {
	query, args := `DELETE FROM history`, []any(nil)
	if !before.IsZero() {
		query, args = query+` WHERE started_at < ?`, []any{millis(before.UTC())}
	}
	res, err := d.sql.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("forget history: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return int(affected), nil
}

// nullableID writes zero as NULL, so a removed account or item leaves the row
// standing rather than taking it with them.
func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}
