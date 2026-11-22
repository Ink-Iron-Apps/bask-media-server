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

// Webhooks: where this server tells when something happens.
//
// The read side comes in two shapes on purpose. Webhooks is for a person
// looking at a page and never carries the token or the secret; Destinations is
// for the notifier about to send something and carries both. One function
// doing either job is one refactor away from a page that shows a credential.

// webhookColumns is every column, in the order both scanners expect.
const webhookColumns = `id, url, format, events, token, secret, enabled,
	created_at, last_at, last_status, last_error, failures`

// Destination is a webhook with the credentials needed to deliver to it.
//
// Not in the domain package, because nothing outside the delivery path should
// be able to hold one by accident.
type Destination struct {
	domain.Webhook

	// Token is sent as a bearer token where the receiver wants one, and
	// Secret is what the signature over the body is computed with. Either
	// may be empty.
	Token  string
	Secret string
}

// Webhooks is every destination, oldest first, without their credentials.
func (d *DB) Webhooks(ctx context.Context) ([]domain.Webhook, error) {
	full, err := d.destinations(ctx, false)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Webhook, 0, len(full))
	for _, dest := range full {
		out = append(out, dest.Webhook)
	}
	return out, nil
}

// Destinations is every enabled destination, with their credentials, for the
// notifier to deliver to.
//
// Disabled rows are left out here rather than filtered by the caller: a
// destination somebody switched off should take no part in delivery at all,
// and the one place that decides that is this query.
func (d *DB) Destinations(ctx context.Context) ([]Destination, error) {
	return d.destinations(ctx, true)
}

func (d *DB) destinations(ctx context.Context, enabledOnly bool) ([]Destination, error) {
	query := `SELECT ` + webhookColumns + ` FROM webhooks`
	if enabledOnly {
		query += ` WHERE enabled = 1`
	}
	query += ` ORDER BY id`

	rows, err := d.sql.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list webhooks: %w", err)
	}
	defer rows.Close()

	var out []Destination
	for rows.Next() {
		dest, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dest)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list webhooks: %w", err)
	}
	return out, nil
}

// Webhook is one destination, without its credentials.
func (d *DB) Webhook(ctx context.Context, id int64) (*domain.Webhook, error) {
	dest, err := d.destination(ctx, id)
	if err != nil {
		return nil, err
	}
	hook := dest.Webhook
	return &hook, nil
}

// Destination is one destination with its credentials, for a test delivery.
func (d *DB) Destination(ctx context.Context, id int64) (*Destination, error) {
	return d.destination(ctx, id)
}

func (d *DB) destination(ctx context.Context, id int64) (*Destination, error) {
	row := d.sql.QueryRowContext(ctx, `SELECT `+webhookColumns+` FROM webhooks WHERE id = ?`, id)
	dest, err := scanWebhook(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &dest, nil
}

// scanner is a row or a rows, so one function reads either.
type scanner interface {
	Scan(dest ...any) error
}

func scanWebhook(row scanner) (Destination, error) {
	var (
		dest                        Destination
		format, events              string
		enabled                     bool
		created, last               int64
		status, failures            int
		lastError, token, secretRaw string
	)
	if err := row.Scan(&dest.ID, &dest.URL, &format, &events, &token, &secretRaw, &enabled,
		&created, &last, &status, &lastError, &failures); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Destination{}, err
		}
		return Destination{}, fmt.Errorf("read webhook: %w", err)
	}
	dest.Format = domain.WebhookFormat(format)
	dest.Events = splitEvents(events)
	dest.Enabled = enabled
	dest.CreatedAt = fromMillis(created)
	dest.LastAt = fromMillis(last)
	dest.LastStatus = status
	dest.LastError = lastError
	dest.Failures = failures
	dest.Token, dest.Secret = token, secretRaw
	dest.HasToken, dest.HasSecret = token != "", secretRaw != ""
	return dest, nil
}

// NewWebhook is what it takes to add a destination.
//
// A struct rather than six arguments because five of them are strings, and a
// call where the token and the secret can be swapped by miscounting commas is
// a call that will be.
type NewWebhook struct {
	URL     string
	Format  domain.WebhookFormat
	Events  []string
	Token   string
	Secret  string
	Enabled bool
}

// AddWebhook records a destination.
func (d *DB) AddWebhook(ctx context.Context, in NewWebhook) (*domain.Webhook, error) {
	if strings.TrimSpace(in.URL) == "" {
		return nil, fmt.Errorf("a webhook needs an address")
	}
	if in.Format == "" {
		in.Format = domain.WebhookJSON
	}
	if !in.Format.Valid() {
		return nil, fmt.Errorf("%q is not a format this server can send", in.Format)
	}
	res, err := d.sql.ExecContext(ctx,
		`INSERT INTO webhooks (url, format, events, token, secret, enabled, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(in.URL), string(in.Format), joinEvents(in.Events),
		in.Token, in.Secret, in.Enabled, millis(time.Now().UTC()))
	if err != nil {
		return nil, fmt.Errorf("add webhook: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("add webhook: %w", err)
	}
	return d.Webhook(ctx, id)
}

// WebhookEdit is a change to a destination. Nil fields are left alone.
//
// Pointers rather than values so that clearing a token ("" means remove it)
// is a different request from not mentioning it, which is what a page that
// cannot show the current value has to be able to say.
type WebhookEdit struct {
	URL     *string
	Format  *domain.WebhookFormat
	Events  *[]string
	Token   *string
	Secret  *string
	Enabled *bool
}

// EditWebhook changes a destination.
func (d *DB) EditWebhook(ctx context.Context, id int64, edit WebhookEdit) (*domain.Webhook, error) {
	var (
		sets []string
		args []any
	)
	if edit.URL != nil {
		url := strings.TrimSpace(*edit.URL)
		if url == "" {
			return nil, fmt.Errorf("a webhook needs an address")
		}
		sets, args = append(sets, "url = ?"), append(args, url)
	}
	if edit.Format != nil {
		if !edit.Format.Valid() {
			return nil, fmt.Errorf("%q is not a format this server can send", *edit.Format)
		}
		sets, args = append(sets, "format = ?"), append(args, string(*edit.Format))
	}
	if edit.Events != nil {
		sets, args = append(sets, "events = ?"), append(args, joinEvents(*edit.Events))
	}
	if edit.Token != nil {
		sets, args = append(sets, "token = ?"), append(args, *edit.Token)
	}
	if edit.Secret != nil {
		sets, args = append(sets, "secret = ?"), append(args, *edit.Secret)
	}
	if edit.Enabled != nil {
		// Switching a destination back on clears the failure count, because
		// the number people read it as is "how broken is this now", and a
		// count carried over from before a fix answers a question nobody
		// asked.
		sets, args = append(sets, "enabled = ?"), append(args, *edit.Enabled)
		if *edit.Enabled {
			sets = append(sets, "failures = 0", "last_error = ''")
		}
	}
	if len(sets) == 0 {
		return d.Webhook(ctx, id)
	}
	args = append(args, id)
	res, err := d.sql.ExecContext(ctx,
		`UPDATE webhooks SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("edit webhook: %w", err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return nil, ErrNotFound
	}
	return d.Webhook(ctx, id)
}

// DeleteWebhook forgets a destination.
func (d *DB) DeleteWebhook(ctx context.Context, id int64) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM webhooks WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete webhook: %w", err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordDelivery stores how the last delivery to a destination went.
//
// status is the HTTP status where there was a reply, and failure the reason
// where there was not. A success resets the failure count: what the number
// means is consecutive failures, which is the only form of it that tells an
// operator anything.
func (d *DB) RecordDelivery(ctx context.Context, id int64, status int, failure string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	query := `UPDATE webhooks SET last_at = ?, last_status = ?, last_error = ?, failures = 0 WHERE id = ?`
	if failure != "" {
		query = `UPDATE webhooks SET last_at = ?, last_status = ?, last_error = ?, failures = failures + 1 WHERE id = ?`
	}
	if _, err := d.sql.ExecContext(ctx, query, millis(at.UTC()), status, failure, id); err != nil {
		return fmt.Errorf("record delivery: %w", err)
	}
	return nil
}

// AddedSince is what arrived in the library after a moment, for announcing it.
//
// Only the kinds a person would want to hear about: a new episode is news, the
// season row created to hold it is not, and neither is a deleted scene. The
// limit is a cap the caller uses to decide whether to announce items one by
// one at all, so it asks for one more than it wants and takes a long answer
// as "too many to list".
func (d *DB) AddedSince(ctx context.Context, since time.Time, limit int) ([]domain.Item, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items
		  WHERE kind IN ('movie','episode','album','photo') AND extra = '' AND added_at >= ?
		  ORDER BY added_at, id LIMIT ?`,
		millis(since.UTC()), limit)
	if err != nil {
		return nil, fmt.Errorf("added since: %w", err)
	}
	defer rows.Close()
	return d.collect(ctx, rows)
}

// joinEvents stores a subscription as a comma-separated list.
//
// A list in a column rather than a table of its own: it is read whole, written
// whole, never joined against, and never more than a handful of entries. A
// second table would be a migration and three queries to express a string.
func joinEvents(events []string) string {
	cleaned := make([]string, 0, len(events))
	for _, event := range events {
		event = strings.TrimSpace(event)
		if event != "" {
			cleaned = append(cleaned, event)
		}
	}
	return strings.Join(cleaned, ",")
}

func splitEvents(stored string) []string {
	if strings.TrimSpace(stored) == "" {
		return nil
	}
	parts := strings.Split(stored, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
