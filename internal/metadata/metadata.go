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
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
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

	// Aired is when this first went out, as the provider's own date, and
	// zero when it does not say.
	//
	// The whole date and not just the year, because a special's date is what
	// places it between two seasons: "The Christmas Invasion" went out on
	// 25 December 2005, between series one and two, and the year alone
	// cannot say that.
	Aired time.Time

	// Genres are what the provider says this is about, in its own words.
	// Empty when it has none, which must not erase genres that are already
	// stored: see the note where the enrichment pass writes them.
	Genres []string
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

// ArtKind is which picture of a thing a candidate is.
//
// Two, because two is what a client draws: the upright poster on a shelf and
// the wide one behind a title. A source that has more kinds than these — a
// logo, a season banner — has them folded into whichever of the two they are
// shaped like, or left out, because an image nothing displays is a row in a
// chooser that cannot be chosen.
type ArtKind string

const (
	ArtPoster   ArtKind = "poster"
	ArtBackdrop ArtKind = "backdrop"
)

// Art is one picture on offer.
//
// Everything here is for the person choosing. The URL is what gets fetched;
// the rest is what lets somebody pick between forty near-identical posters:
// how big it is, what language the text on it is in, and how well regarded it
// is where the source keeps a score.
type Art struct {
	Kind ArtKind

	// URL is the full-size image, and ThumbURL a small one for the chooser.
	// A source with no separate small version puts the same URL in both: a
	// grid of full-size posters is slow, not wrong.
	URL      string
	ThumbURL string

	Width  int
	Height int

	// Language is the language of any text on the image, as a two-letter
	// code, and empty for one with no text at all — which is the one most
	// people want, because a client draws the title itself.
	Language string

	// Votes is how well regarded this image is where the source says, and
	// zero where it does not. Only ever used to order a chooser.
	Votes float64

	// Source names where it came from, so a chooser can group by it and say
	// which service a picture is about to be fetched from.
	Source string
}

// ArtQuery is what to find pictures of.
//
// Title and Year are always set, because not every source shares an id
// namespace with the one that matched the item: a library matched against
// TMDB knows nothing about the same film's id at TVDB, and asking by name is
// the only thing every source can answer.
type ArtQuery struct {
	Kind  domain.Kind
	Title string
	Year  int

	// ProviderID is this source's own id for the thing, where the caller
	// happens to have it, which saves a search. Empty is normal.
	ProviderID string
}

// Person is one credit as a provider gives it.
type Person struct {
	Name string
	Role domain.CreditRole

	// Part is the character an actor played, and empty for everyone else.
	Part string

	// Position is where the provider put them, which for a cast is billing
	// order.
	Position int

	// ThumbURL is their photograph, to be fetched into the artwork cache
	// like any other picture. Empty when the provider has none.
	ThumbURL string
}

// Credits is who made a thing and who is in it.
type Credits struct {
	// Studio is who made it, as the provider names them, and empty when it
	// does not say.
	Studio string

	People []Person
}

// PeopleSource is a provider that can also be asked who made a thing.
//
// Separate from [Provider] because it is the expensive half: a request per
// matched title on top of the match itself. A library that does not want
// cast lists should not pay for them, which is a per-library setting, and a
// provider that cannot answer should not have to pretend.
type PeopleSource interface {
	People(ctx context.Context, want ArtQuery) (Credits, error)
}

// SpecialsSource is a provider that can say where a series' specials belong
// in the order it is watched.
//
// Its own interface for the reason [PeopleSource] has one: it is configured
// separately, costs a request of its own, and the provider BMS matches
// against cannot answer it at all — the three fields this returns are
// TheTVDB's, and TMDB has an open request for them.
//
// Keyed by the episode number within season zero, because that is what both
// sides have: BMS files a special as season zero episode N from its
// filename, and the archives number them the same way.
type SpecialsSource interface {
	Placements(ctx context.Context, want ArtQuery) (map[int]domain.Placement, error)
}

// ArtworkSource is somewhere pictures can be browsed.
//
// Separate from [Provider] because the two are configured, paid for and
// chosen between independently: a library can be matched against one service
// and dressed from another, and a source with no search — a site that serves
// a picture by id and nothing else — is not a provider at all and does not
// implement this.
type ArtworkSource interface {
	// Name identifies the source in the chooser and in logs.
	Name() string

	// Art lists what this source has. An empty list is an answer: the
	// service was asked and has no pictures of this, which is different from
	// an error and must not be reported as one.
	Art(ctx context.Context, want ArtQuery) ([]Art, error)
}
