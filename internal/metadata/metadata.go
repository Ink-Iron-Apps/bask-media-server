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

	// Rating is the content rating as the board that gave it writes it:
	// "PG-13", "15", "TV-MA". Empty when the provider has none, which is a
	// fact rather than a failure — plenty of films were never submitted to a
	// board anywhere.
	Rating string
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

// Matcher is a provider that can also be asked for the matches it did not
// pick, and for one match by its id.
//
// Separate from [Provider] on purpose. Enrichment needs one answer and should
// not be able to ask for a list; correcting a wrong match needs the list, and
// needs to fetch the entry somebody chose from it. A provider that implements
// only Provider still enriches a library — correcting a match by hand is
// simply unavailable, which the interface reports rather than hiding.
type Matcher interface {
	// MovieCandidates and ShowCandidates are what the provider found,
	// ordered as it ranked them. Empty rather than ErrNoMatch when there is
	// nothing: a search with no results is an answer, not a failure.
	MovieCandidates(ctx context.Context, title string, year int) ([]Match, error)
	ShowCandidates(ctx context.Context, title string, year int) ([]Match, error)

	// MovieByID and ShowByID fetch one entry the provider already named,
	// which is what makes a chosen correction exact rather than another
	// search that might rank differently.
	MovieByID(ctx context.Context, providerID string) (Match, error)
	ShowByID(ctx context.Context, providerID string) (Match, error)
}
