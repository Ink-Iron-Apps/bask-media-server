// Package metadata describes where titles, summaries and artwork come from.
//
// The interface is here and the providers are under it, so BMS never depends
// on one service's shape. A library with no provider configured still works:
// filenames give titles and the scanner is enough to browse and play. A
// provider only ever adds.
package metadata

import (
	"context"
	"errors"
)

// ErrNoMatch means the provider searched and found nothing worth claiming.
// Distinct from a transport failure: no match is a final answer for this
// title, while a failure is worth retrying later.
var ErrNoMatch = errors.New("no match")

// Match is what a provider found for one thing.
type Match struct {
	// ProviderID identifies the match in the provider's own namespace, kept so
	// a later pass can fetch more without searching again.
	ProviderID string

	Title       string
	Year        int
	Summary     string
	PosterURL   string
	BackdropURL string
}

// Provider answers questions about movies and shows.
//
// Deliberately narrow. Everything BMS needs for a Plex-shaped library is a
// title, a year, a summary and two images, and a wider interface would be a
// wider surface to keep working against a service nobody here controls.
type Provider interface {
	// Name identifies the provider in logs and in the agent field a client
	// displays.
	Name() string

	// Movie matches a film by title and, when known, year.
	Movie(ctx context.Context, title string, year int) (Match, error)

	// Show matches a series by title and, when known, its first year.
	Show(ctx context.Context, title string, year int) (Match, error)

	// Episode fetches one episode of a show the provider has already matched.
	Episode(ctx context.Context, showProviderID string, season, number int) (Match, error)
}
