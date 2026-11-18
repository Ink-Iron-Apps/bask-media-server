// Package domain holds what BMS knows about a library, in BMS's own terms.
//
// Nothing in here is shaped by a wire protocol. The Plex-compatible API is a
// presenter over these types and keeps its own vocabulary — ratingKey,
// MediaContainer, X-Plex headers — at the HTTP edge, so a second presenter
// (BMS's native API) can be added later without touching storage or scanning.
// The rule is one way only: domain types never import a presenter, and a
// presenter concept never reaches a column.
package domain

import "time"

// Kind is what a library item is, independent of how any client names it.
type Kind string

const (
	KindMovie   Kind = "movie"
	KindShow    Kind = "show"
	KindSeason  Kind = "season"
	KindEpisode Kind = "episode"
)

// SectionKind is what a library holds. A section is the unit a client browses
// and the unit a scan walks.
type SectionKind string

const (
	SectionMovies SectionKind = "movies"
	SectionShows  SectionKind = "shows"
)

// Section is one configured library: a kind plus the directories that hold it.
type Section struct {
	ID        int64
	Title     string
	Kind      SectionKind
	Roots     []string
	ScannedAt time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Item is anything a client can browse or play: a movie, a show, one of its
// seasons, or an episode.
//
// One table rather than four, because every client-facing operation —
// list a section, open a thing, play a thing, record progress — is the same
// operation whatever the kind, and a join per level would buy nothing but
// queries nobody can read.
type Item struct {
	ID        int64
	SectionID int64
	Kind      Kind

	// ParentID is the season for an episode and the show for a season. Movies
	// and shows have none.
	ParentID *int64

	Title string

	// SortTitle is Title with a leading article dropped, computed once at scan
	// time so ordering is an index lookup rather than a per-request transform.
	SortTitle string

	Year    int
	Index   int // episode number within a season, or season number within a show
	Summary string

	// Duration is the runtime, measured by ffprobe when the file was scanned.
	Duration time.Duration

	AddedAt   time.Time
	UpdatedAt time.Time

	// ProviderID is the match in the metadata provider's namespace, kept so a
	// later pass can fetch more without searching again.
	ProviderID string

	// PosterKey and ArtKey name images in the artwork cache. Empty means
	// nothing has been fetched, which is a library that is browsable and
	// playable and simply has no pictures yet.
	PosterKey string
	ArtKey    string

	// MetadataAt is when a provider last answered for this item. Zero means
	// never, which is what an enrichment pass looks for.
	MetadataAt time.Time

	// Parts are the files behind a playable item. A movie or episode normally
	// has exactly one; more than one means multiple versions of the same thing.
	Parts []Part
}

// Playable reports whether an item has files of its own, as opposed to
// gathering children.
func (i Item) Playable() bool { return i.Kind == KindMovie || i.Kind == KindEpisode }

// Part is one file on disk.
type Part struct {
	ID        int64
	ItemID    int64
	Path      string
	Size      int64
	Container string

	// ModifiedAt is the file's mtime as the last scan saw it. A scan that finds
	// the same path at the same size and mtime leaves the row alone, which is
	// what makes a rescan of an unchanged library cost a stat per file and no
	// writes at all.
	ModifiedAt time.Time
}

// User is an account on this server. BMS has no external identity provider by
// design: the point of the project is that nothing about it belongs to someone
// else.
type User struct {
	ID        int64
	Username  string
	Admin     bool
	CreatedAt time.Time
}

// Token is a long-lived credential a client presents on every request, in the
// header or the query string. Stored hashed: a leaked database should not be a
// leaked set of working credentials.
type Token struct {
	ID         int64
	UserID     int64
	Label      string
	Hash       []byte
	CreatedAt  time.Time
	LastSeenAt time.Time
}

// Progress is one user's position in one item.
type Progress struct {
	UserID    int64
	ItemID    int64
	Offset    time.Duration
	Duration  time.Duration
	Watched   bool
	ViewCount int
	UpdatedAt time.Time
}

// Watched is the threshold past which an item counts as finished rather than
// in progress. Plex uses 90%; matching it keeps "Continue Watching" behaving
// the way the household already expects.
const WatchedFraction = 0.9

// Finished reports whether an offset is far enough through a duration to count
// as watched rather than resumable.
func Finished(offset, duration time.Duration) bool {
	if duration <= 0 {
		return false
	}
	return float64(offset) >= float64(duration)*WatchedFraction
}

// MarkerType is a stretch of a file a viewer usually wants to skip.
type MarkerType string

const (
	MarkerIntro   MarkerType = "intro"
	MarkerCredits MarkerType = "credits"
)

// MarkerSource is how a marker was arrived at, kept because the two are not
// equally trustworthy and a later pass has to know which of its own work it
// may replace.
//
//   - SourceChapter: a chapter in the file says so. The person who made the
//     file labelled it, which beats anything guessed from the content.
//   - SourceAudio: the same audio was found at the same place in other
//     episodes of the season. Right far more often than not, and the only
//     thing that works at all on a library with no chapters, but a guess.
type MarkerSource string

const (
	SourceChapter MarkerSource = "chapter"
	SourceAudio   MarkerSource = "audio"
)

// Marker is one skippable stretch of one item.
type Marker struct {
	ID     int64
	ItemID int64
	Type   MarkerType
	Start  time.Duration
	End    time.Duration
	Source MarkerSource
}

// Chapter is a chapter the file declares.
//
// Stored as scanned facts in their own right — a client shows them on the
// timeline — and read a second time by marker detection, which is the cheapest
// detector there is when the titles are meaningful.
type Chapter struct {
	ID     int64
	ItemID int64
	Index  int
	Title  string
	Start  time.Duration
	End    time.Duration
}

// Duration is how long a marker runs for.
func (m Marker) Duration() time.Duration { return m.End - m.Start }
