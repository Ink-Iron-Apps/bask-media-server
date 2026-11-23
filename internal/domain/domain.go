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

	// An extra belongs to something rather than being it: a trailer, a
	// deleted scene, a featurette. A kind of its own rather than a flag on a
	// movie, because everything that lists a library has to leave them out —
	// a trailer in the films row is the mistake this replaces.
	KindExtra Kind = "extra"
)

// ExtraKind is which sort of extra a file is.
//
// The names are the ones Plex and Jellyfin use in their own APIs, because
// both of them are spoken here and a third spelling would mean translating
// twice.
type ExtraKind string

const (
	ExtraTrailer         ExtraKind = "trailer"
	ExtraBehindTheScenes ExtraKind = "behindTheScenes"
	ExtraDeleted         ExtraKind = "deletedScene"
	ExtraFeaturette      ExtraKind = "featurette"
	ExtraInterview       ExtraKind = "interview"
	ExtraScene           ExtraKind = "scene"
	ExtraShort           ExtraKind = "short"
	ExtraOther           ExtraKind = "other"
)

// Label is what a client shows above a row of these.
func (e ExtraKind) Label() string {
	switch e {
	case ExtraTrailer:
		return "Trailer"
	case ExtraBehindTheScenes:
		return "Behind the Scenes"
	case ExtraDeleted:
		return "Deleted Scene"
	case ExtraFeaturette:
		return "Featurette"
	case ExtraInterview:
		return "Interview"
	case ExtraScene:
		return "Scene"
	case ExtraShort:
		return "Short"
	default:
		return "Extra"
	}
}

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

	// Settings is what this library wants done to it. Never nil when a
	// section comes out of the store: an unset column reads as the defaults,
	// so every caller can read these without a check.
	Settings SectionSettings

	// SweptAt is when the scheduler last looked at this library, as opposed
	// to [Section.ScannedAt], which is when a scan last finished. The two
	// differ for a library whose scan failed or is still running, and
	// keeping them apart is what stops a failing library being retried in a
	// tight loop.
	SweptAt time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// SectionSettings is what one library wants done to it, as opposed to what is
// true of the whole server.
//
// JSON tags on purpose: one shape for the column, the admin API and the page
// that edits it, the same arrangement a smart collection's rules use.
//
// Every zero value is the behaviour BMS had before these existed, so a
// library that predates the column is a library with the old behaviour and
// no migration had to guess at anything.
type SectionSettings struct {
	// ScanEvery is how often to scan this library without being asked, and
	// zero means never — which is what every library did until now.
	//
	// Minutes rather than a duration, because it arrives from a form and
	// goes into JSON, and because the useful range is a quarter of an hour
	// to a day.
	ScanEvery int `json:"scanEvery,omitempty"`

	// Enrich says whether to ask the metadata provider about this library at
	// all. Off is for the libraries no provider knows: home videos, a folder
	// of recordings, anything whose titles are not a catalogue's titles.
	// Nil means on, because that is what every library did before.
	Enrich *bool `json:"enrich,omitempty"`

	// People says whether to fetch cast and crew for this library. Its own
	// setting rather than part of Enrich because it is the expensive half —
	// a request per title on top of the match — and a music or photo library
	// has no use for it at all.
	People *bool `json:"people,omitempty"`

	// Markers says whether to look for intros and credits here. Off for a
	// library of films, where there is nothing to skip into and the audio
	// pass is the slowest thing this server does.
	Markers *bool `json:"markers,omitempty"`
}

// EnrichWanted reports whether a metadata pass should touch this library.
func (s SectionSettings) EnrichWanted() bool { return s.Enrich == nil || *s.Enrich }

// PeopleWanted reports whether cast and crew should be fetched here.
//
// Defaults to on for the kinds a provider has people for, and is never on
// for music or photographs: TMDB has nothing to say about an album, and
// asking would spend a request to find out.
func (s SectionSettings) PeopleWanted(kind SectionKind) bool {
	switch kind {
	case SectionMovies, SectionShows:
	default:
		return false
	}
	return s.People == nil || *s.People
}

// MarkersWanted reports whether marker detection should run here.
func (s SectionSettings) MarkersWanted() bool { return s.Markers == nil || *s.Markers }

// ScanInterval is how often this library scans itself, and zero for never.
func (s SectionSettings) ScanInterval() time.Duration {
	if s.ScanEvery <= 0 {
		return 0
	}
	return time.Duration(s.ScanEvery) * time.Minute
}

// CreditRole is what somebody did on a thing.
//
// Four, because four is what the clients draw: a cast list, and three crew
// jobs each of which gets its own line on a detail page. A provider's
// hundred department names collapse into these or are dropped, because a
// credit nothing displays is a row in a table for nobody.
type CreditRole string

const (
	// CreditActor is somebody in it, and the only role with a part.
	CreditActor CreditRole = "actor"

	CreditDirector CreditRole = "director"
	CreditWriter   CreditRole = "writer"
	CreditProducer CreditRole = "producer"
)

// CreditRoles is every role, in the order a detail page shows them.
func CreditRoles() []CreditRole {
	return []CreditRole{CreditDirector, CreditWriter, CreditProducer, CreditActor}
}

// Valid reports whether this is a role BMS stores.
func (r CreditRole) Valid() bool {
	switch r {
	case CreditActor, CreditDirector, CreditWriter, CreditProducer:
		return true
	default:
		return false
	}
}

// Credit is one person's involvement in one thing.
//
// Here rather than only in the store because every surface renders it: a
// Plex client reads a Role element, a Jellyfin client a People entry, and
// the viewing pages a line under the title.
type Credit struct {
	Name string
	Role CreditRole

	// Part is the character an actor played, and empty for everyone else.
	Part string

	// Thumb names their picture in the artwork cache, and is empty when
	// nothing has been fetched.
	Thumb string
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

	// Locked means somebody set this item's details by hand, so the
	// enrichment pass leaves it alone.
	Locked bool

	// Extra is which sort of extra this is, for the items whose kind is
	// KindExtra, and empty for everything else.
	Extra ExtraKind

	// Rating is the content rating as the board that gave it writes it —
	// "PG-13", "15", "TV-MA" — and RatingAge the age it stands for, which is
	// what a restriction compares against. [UnknownAge] means nobody has
	// said, which is every item until a metadata pass reaches it.
	Rating    string
	RatingAge int

	// Genres are what this is about, in the words a provider or the file's
	// own tags used: "Comedy", "Science Fiction", "Shoegaze". Sorted, and
	// empty for anything nothing has said about yet — which is every item in
	// a library with no provider configured and no tags in its files.
	//
	// On the thing that has them and not on what is inside it. A provider
	// gives a series its genres and says nothing about episode four, so an
	// episode carries none.
	Genres []string

	// Tags, Labels, Moods and Styles are the other words attached to a
	// thing, and the same shape as Genres. Genre is a provider's and a
	// file's; tag and label are a person's, which is what makes them worth
	// having — a shelf nobody else decides. Mood and style are music's,
	// shown in their own rows by a music client.
	//
	// Read only where a whole item is read. A listing fills Genres alone,
	// because a grid draws one row of chips and four more queries per page
	// would buy nothing anybody can see.
	Tags   []string
	Labels []string
	Moods  []string
	Styles []string

	// Studio is who made it, as the provider names them. Empty until a
	// metadata pass that was asked for people has reached it.
	Studio string

	// People are the credits: who directed, wrote and produced it, and who
	// is in it. In the order a detail page shows them — crew first, then the
	// cast in the provider's own billing order — and read only where a whole
	// item is read, for the reason the other lists are.
	People []Credit

	// Parts are the files behind a playable item. A movie or episode normally
	// has exactly one; more than one means multiple versions of the same thing.
	Parts []Part
}

// Playable reports whether an item has files of its own, as opposed to
// gathering children.
func (i Item) Playable() bool {
	switch i.Kind {
	case KindMovie, KindEpisode, KindTrack, KindPhoto, KindExtra:
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
	// Edition is which cut of a film this file is — the Director's Cut, the
	// Theatrical — read from the name. Empty for the ordinary case of one
	// file, and for everything that is not a film.
	Edition string
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
	// Device is what the client holding this token last called itself, as it
	// described itself in a header. Shown, never trusted.
	Device string
}

// Credential is who a presented secret belongs to and what it may reach: the
// account, the token row it was resolved through, and the libraries left after
// the account's grants and the token's are combined.
//
// Restricted says whether Sections means anything at all, and it exists
// because an empty list has to be able to mean "nothing". Two grants that do
// not overlap — an account allowed the films, a television allowed the music —
// are contradictory instructions, and the honest answer to them is that this
// device reaches nothing rather than everything. Reading emptiness as "no
// restriction" would turn exactly that case inside out.
type Credential struct {
	User    User
	TokenID int64

	// Restricted is false for the ordinary credential nobody has narrowed,
	// which reaches every library whatever Sections holds.
	Restricted bool

	// Sections is what it may reach when Restricted. Meaningless otherwise.
	Sections []int64

	// Ceiling is the highest age rating this credential may see, -1 for no
	// limit, and AllowUnrated whether the things nobody has rated are
	// allowed through. Both come from the account: a rating is about who is
	// watching rather than about which device they are on.
	Ceiling      int
	AllowUnrated bool

	// MaxStreams is how many things this account may play at once and
	// MaxBitRate the most it may be sent, in bits per second. Zero for no
	// limit. On the account rather than the device for the same reason the
	// ceiling is: both are about a person and what a household has agreed
	// they may take of a machine everybody shares.
	MaxStreams int
	MaxBitRate int64

	// Unlocked means a PIN was presented, so this credential's rating
	// ceiling does not apply for now. Set by the surface that took the PIN,
	// never stored.
	Unlocked bool
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

// PlaylistKind is what a playlist holds, which is what a client's Playlists
// tab filters on: a music client asking for audio playlists should not be
// offered a list of films.
//
// Derived from the contents rather than declared when the list is made, and
// a list holding both is mixed — a real answer, not a failure to choose.
type PlaylistKind string

const (
	PlaylistAudio PlaylistKind = "audio"
	PlaylistVideo PlaylistKind = "video"
	PlaylistMixed PlaylistKind = "mixed"
)

// Playlist is one account's arrangement of things in the library.
//
// Never shared: whether these twelve tracks belong together is an opinion,
// and two people in a household are allowed different ones.
type Playlist struct {
	ID        int64
	UserID    int64
	Title     string
	Kind      PlaylistKind
	Entries   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// PlaylistEntry is one line of a playlist.
//
// It carries its own id because the same track twice is two entries, and
// removing or moving "the track" would otherwise be ambiguous.
type PlaylistEntry struct {
	ID   int64
	Item Item
}

// Collection is the library's own grouping — a trilogy, a director, a
// Christmas shelf.
//
// The library's rather than a person's: "these four films are one story" is a
// fact about the films, so everybody who can see them sees the same grouping.
type Collection struct {
	ID        int64
	Title     string
	SortTitle string
	Summary   string
	PosterKey string
	// Held is how many of its items the asker may see, which is not
	// necessarily how many are in it.
	Held int

	// Smart is the rule set that decides what is in it, and nil for a
	// collection somebody filled by hand. A smart collection has no stored
	// membership at all: it is a question asked of the library every time
	// somebody opens it, which is the whole point — a film that arrives
	// tonight is in "Unwatched science fiction" tonight, with nobody
	// remembering to put it there.
	Smart *Smart

	// Unreadable means this collection has rules the server could not parse,
	// so it holds nothing until they are fixed. One corrupt row must not
	// take a listing down with it, and silently showing it as an ordinary
	// empty collection would hide the fault from the only person who can
	// repair it.
	Unreadable bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Smart is what a smart collection asks of the library.
//
// The JSON tags are deliberate and they are the same shape in three places:
// the rules column, the administration API and the editor that writes them.
// One shape rather than three, because a rule set that means one thing on the
// wire and another in the database is a bug nobody can see.
type Smart struct {
	// Match decides whether an item has to satisfy every rule or any one of
	// them. Empty reads as SmartAll, so a rule set with no opinion behaves
	// like the one somebody would have written.
	Match SmartMatch `json:"match,omitempty"`

	Rules []SmartRule `json:"rules"`

	// Sort is the order the collection is shown in, and Desc reverses it.
	// A collection nobody arranged by hand still has to come out in some
	// order, and "whatever the database felt like" is not an order.
	Sort SmartSort `json:"sort,omitempty"`
	Desc bool      `json:"desc,omitempty"`

	// Limit caps how many it holds, counted after the sort, and zero means
	// no cap. This is what makes "the ten most recently added films" a
	// collection rather than a library.
	Limit int `json:"limit,omitempty"`
}

// SmartMatch is whether every rule has to hold or only one of them.
type SmartMatch string

const (
	SmartAll SmartMatch = "all"
	SmartAny SmartMatch = "any"
)

// SmartSort is the order a smart collection comes out in.
type SmartSort string

const (
	SmartByTitle    SmartSort = "title"
	SmartByYear     SmartSort = "year"
	SmartByAdded    SmartSort = "added"
	SmartByDuration SmartSort = "duration"
)

// SmartRule is one condition: a field of an item, a comparison, and what to
// compare it against.
//
// Value is a string whatever the field holds, because that is what arrives
// from a form and what goes into a JSON column, and the server turns it into
// a number or an identifier when it compiles the rule. The alternative is a
// field of four optional types, three of which are always empty.
type SmartRule struct {
	Field SmartField `json:"field"`
	Op    SmartOp    `json:"op"`
	Value string     `json:"value,omitempty"`
}

// SmartField is what a rule looks at. The server owns the list of fields and
// which comparisons each one allows — see the store, which is where the rule
// becomes SQL.
type SmartField string

const (
	SmartLibrary     SmartField = "library"
	SmartKind        SmartField = "kind"
	SmartTitle       SmartField = "title"
	SmartSummary     SmartField = "summary"
	SmartPerformer   SmartField = "performer"
	SmartGenre       SmartField = "genre"
	SmartDirector    SmartField = "director"
	SmartActor       SmartField = "actor"
	SmartStudio      SmartField = "studio"
	SmartYear        SmartField = "year"
	SmartCertificate SmartField = "certificate"
	SmartAge         SmartField = "age"
	SmartAdded       SmartField = "added"
	SmartDuration    SmartField = "duration"
	SmartResolution  SmartField = "resolution"
	SmartContainer   SmartField = "container"
	SmartEdition     SmartField = "edition"
	SmartWatched     SmartField = "watched"
)

// SmartOp is how a field is compared.
type SmartOp string

const (
	SmartIs       SmartOp = "is"
	SmartIsNot    SmartOp = "isnot"
	SmartContains SmartOp = "contains"
	SmartStarts   SmartOp = "starts"
	SmartBefore   SmartOp = "before"
	SmartAfter    SmartOp = "after"
	SmartAtLeast  SmartOp = "atleast"
	SmartAtMost   SmartOp = "atmost"
	SmartWithin   SmartOp = "within"

	// The three states of having seen something, which are three operators
	// rather than one field because "partly watched" is not the negation of
	// anything.
	SmartYes    SmartOp = "yes"
	SmartNo     SmartOp = "no"
	SmartPartly SmartOp = "partly"
)

// Webhook is one place this server tells when something happens.
//
// The server has no way to push anything to a client, and a household that
// wants a light to dim when a film starts, or a message when a new episode
// lands, has nowhere to hang that behaviour. This is that hook: an address,
// what to send it, and which events are worth waking it for.
type Webhook struct {
	ID  int64
	URL string

	// Format is how the body is shaped: Plain JSON for something that parses
	// it, or a one-line message for a notification service.
	Format WebhookFormat

	// Events is what this destination is told about. Each entry is either an
	// exact kind ("playback.start") or a family ("playback.*"). Empty means
	// everything, which is what a fresh webhook gets.
	Events []string

	// Enabled is whether anything is sent. A destination that is failing is
	// better switched off than deleted, because deleting it loses the URL.
	Enabled bool

	// HasToken and HasSecret report whether a bearer token and a signing
	// secret have been set. The values themselves are never read back out of
	// the store for display: they are credentials for somebody else's
	// service, and a page that shows them is a page that leaks them.
	HasToken  bool
	HasSecret bool

	CreatedAt time.Time

	// What happened last time. Kept so a destination that quietly stopped
	// working can be seen to have stopped, rather than being discovered
	// months later by somebody wondering why their lights never dim.
	LastAt     time.Time
	LastStatus int
	LastError  string
	Failures   int
}

// WebhookFormat is the shape of what a webhook receives.
type WebhookFormat string

const (
	// WebhookJSON posts a JSON object describing the event. For anything
	// that parses it: Home Assistant, n8n, a script of your own.
	WebhookJSON WebhookFormat = "json"

	// WebhookNtfy posts the event's one-line message as plain text, with the
	// title, tags and priority in headers. That is exactly what ntfy expects,
	// so a topic URL needs no translation in between.
	WebhookNtfy WebhookFormat = "ntfy"
)

// Valid reports whether a format is one this server knows how to send.
func (f WebhookFormat) Valid() bool {
	return f == WebhookJSON || f == WebhookNtfy
}

// Watch is one thing somebody watched, once, on one device.
//
// Written when it starts, kept up to date while it runs, closed when it ends.
// The names are stored beside the identifiers rather than read through them:
// this is a record of what happened, and what happened does not change
// because a film was later deleted or an account renamed.
type Watch struct {
	ID int64

	// UserID and ItemID are zero where the account or the item has since
	// been removed. Name, Title and Subtitle are what they were called at
	// the time and are always there.
	UserID int64
	Name   string
	ItemID int64
	Title  string

	// Subtitle is the line under the title: an episode's name under its
	// show, a track's album.
	Subtitle string
	Kind     Kind

	// Surface is which protocol the client was speaking, and Client, Device
	// and Address how it described itself. Reported by the client, so true
	// only in the sense that it is what was said.
	Surface string
	Client  string
	Device  string
	Address string

	// Mode is direct, remux or transcode: what this server had to do to
	// serve it. The answer to "why was the machine hot on Tuesday".
	Mode string

	// Reason is why a stream ended where somebody ended it, and empty where
	// it simply finished.
	Reason string

	Started time.Time

	// Ended is when the stream stopped, or the last moment it was known to
	// be playing where the server never saw it stop. Complete says which.
	Ended    time.Time
	Complete bool

	// Paused is how much of the time between Started and Ended the viewer
	// had it paused, so Watched can be the part they were actually there
	// for.
	Paused time.Duration

	// Position is how far in they had got, and Length how long the thing is.
	// Both are what the client last reported.
	Position time.Duration
	Length   time.Duration
}

// Watched is how long somebody actually watched: the time the stream was
// open, less the time it sat paused.
//
// Not the position, which is where they got to. Somebody who skips to the
// last ten minutes of a film has a position of two hours and watched ten
// minutes, and both facts are worth having.
func (w Watch) Watched() time.Duration {
	watched := w.Ended.Sub(w.Started) - w.Paused
	if watched < 0 {
		return 0
	}
	return watched
}

// WatchTotal is what one viewer, or one title, added up to over a window.
type WatchTotal struct {
	// UserID and Name identify a viewer's total; Title and ItemID a title's.
	// Each total is one or the other, depending on which question was asked.
	UserID int64
	Name   string
	ItemID int64
	Title  string

	// Plays is how many separate streams, and Watched their total length.
	Plays   int
	Watched time.Duration
}
