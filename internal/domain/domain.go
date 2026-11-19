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

	// Music, three levels deep for the same reason television is: an artist
	// gathers albums, an album gathers tracks, and only the track is a file.
	KindArtist Kind = "artist"
	KindAlbum  Kind = "album"
	KindTrack  Kind = "track"

	// Photos, where the hierarchy is the folder tree itself: an album holds
	// photos and other albums, however deep the person who filed them went.
	KindPhotoAlbum Kind = "photoalbum"
	KindPhoto      Kind = "photo"
)

// SectionKind is what a library holds. A section is the unit a client browses
// and the unit a scan walks.
type SectionKind string

const (
	SectionMovies SectionKind = "movies"
	SectionShows  SectionKind = "shows"
	SectionMusic  SectionKind = "music"
	SectionPhotos SectionKind = "photos"
)

// TopKind is the kind a section's top level holds: what a client sees when it
// opens the library, and what a scan's first level creates.
func (k SectionKind) TopKind() Kind {
	switch k {
	case SectionShows:
		return KindShow
	case SectionMusic:
		return KindArtist
	case SectionPhotos:
		return KindPhotoAlbum
	default:
		return KindMovie
	}
}

// Valid reports whether this is a kind of library BMS knows how to scan.
func (k SectionKind) Valid() bool {
	switch k {
	case SectionMovies, SectionShows, SectionMusic, SectionPhotos:
		return true
	default:
		return false
	}
}

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
	Index   int // episode number within a season, season number within a show, or track number within an album
	Summary string

	// Performer is who plays on a track when that is not the album's artist,
	// which is the whole of what makes a compilation readable: every row of
	// "Now That's What I Call Music" is by somebody different, and the album
	// is filed under one name.
	Performer string

	// Disc is which disc of an album a track is on, and zero for everything
	// else. Part of an item's identity, because two discs of one album
	// routinely both have a track 1.
	Disc int

	// TakenAt is when a photograph was taken, read from the file's own EXIF
	// and falling back to its mtime. Zero for everything that is not a photo,
	// and what a photo album is ordered by: a folder of holiday pictures
	// belongs in the order they happened, not in the order of their names.
	TakenAt time.Time

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
func (i Item) Playable() bool {
	switch i.Kind {
	case KindMovie, KindEpisode, KindTrack, KindPhoto:
		return true
	default:
		return false
	}
}

// Audio reports whether an item is listened to rather than watched, which is
// what decides whether a client reaches for a video element or an audio one.
func (i Item) Audio() bool { return i.Kind == KindTrack }

// Part is one file on disk.
type Part struct {
	ID        int64
	ItemID    int64
	Path      string
	Size      int64
	Container string

	// Width, Height and Orientation describe a photograph: its pixels as
	// stored, and the rotation its EXIF asks for. Zero for everything else,
	// including video — a film's dimensions are read at playback time,
	// because what matters about them is what the client can display, while a
	// photograph's dimensions are its content.
	Width       int
	Height      int
	Orientation int

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

// Subtitle is one set of captions for one file.
//
// Either a stream inside the container or a file beside it, because both are
// routine and a client cannot tell the difference once it is playing: the
// distinction only decides how this server gets at the text.
type Subtitle struct {
	ID     int64
	ItemID int64
	PartID int64

	// External distinguishes a file beside the media from a stream inside it.
	External bool

	// StreamIndex is the stream's index within the container, for an embedded
	// one. Meaningless when External.
	StreamIndex int

	// Path is the file the text is in: the sidecar for an external subtitle,
	// and the media file itself for an embedded one. Always the file to open,
	// so nothing downstream has to look up a part to read a subtitle.
	Path string

	// Codec is what the text is written as: srt, ass, webvtt, mov_text, or
	// one of the bitmap formats (pgs, vobsub) that are pictures rather than
	// text and cannot be converted without reading them.
	Codec string

	// Language is a three-letter code where one could be worked out, and
	// empty where it could not: an unlabelled subtitle is better offered
	// without a language than labelled with a guess.
	Language string

	Title   string
	Forced  bool
	Default bool

	// Hearing marks a track written for viewers who cannot hear the audio —
	// SDH — which carries sound effects and speaker names as well as speech.
	Hearing bool
}

// TextSubtitle reports whether a codec is text, and so can be converted and
// shown by anything. The others are pictures, and need either the player's
// own renderer or burning into the video.
func TextSubtitle(codec string) bool {
	switch codec {
	case "srt", "subrip", "ass", "ssa", "webvtt", "vtt", "mov_text", "text", "eia_608", "subviewer":
		return true
	default:
		return false
	}
}

// Text reports whether this subtitle is text rather than a picture.
func (s Subtitle) Text() bool { return TextSubtitle(s.Codec) }

// Duration is how long a marker runs for.
func (m Marker) Duration() time.Duration { return m.End - m.Start }
