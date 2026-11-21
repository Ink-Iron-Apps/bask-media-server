// Package plexapi presents the library in the shape Plex clients expect.
//
// This is a presenter and nothing more: it reads domain types from the store
// and writes Plex's vocabulary — MediaContainer, ratingKey, Directory, Video,
// Media, Part — on the way out. Nothing in here is imported by the store or
// the scanner, which is what keeps a second presenter (BMS's own API) a matter
// of adding a package rather than unpicking one.
//
// The shape is reverse-engineered from what the clients actually send and
// accept; python-plexapi is the de-facto written spec. Only the subset Bask
// calls is implemented, because that is the subset that has to be right.
package plexapi

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// container is one MediaContainer response.
//
// Plex serves XML by default and JSON when the client asks for it, with the
// same attribute names in both. One struct with both tag sets keeps the two
// from drifting, which is the usual way a hand-written compatibility layer
// starts failing on one client and not another.
type container struct {
	XMLName xml.Name `xml:"MediaContainer" json:"-"`

	Size                int    `xml:"size,attr" json:"size"`
	TotalSize           int    `xml:"totalSize,attr,omitempty" json:"totalSize,omitempty"`
	Offset              int    `xml:"offset,attr,omitempty" json:"offset,omitempty"`
	Identifier          string `xml:"identifier,attr,omitempty" json:"identifier,omitempty"`
	MachineIdentifier   string `xml:"machineIdentifier,attr,omitempty" json:"machineIdentifier,omitempty"`
	Version             string `xml:"version,attr,omitempty" json:"version,omitempty"`
	FriendlyName        string `xml:"friendlyName,attr,omitempty" json:"friendlyName,omitempty"`
	Claimed             bool   `xml:"claimed,attr,omitempty" json:"claimed,omitempty"`
	Title1              string `xml:"title1,attr,omitempty" json:"title1,omitempty"`
	Title2              string `xml:"title2,attr,omitempty" json:"title2,omitempty"`
	ViewGroup           string `xml:"viewGroup,attr,omitempty" json:"viewGroup,omitempty"`
	MediaTagPrefix      string `xml:"mediaTagPrefix,attr,omitempty" json:"mediaTagPrefix,omitempty"`
	AllowSync           bool   `xml:"allowSync,attr,omitempty" json:"allowSync,omitempty"`
	LibrarySectionID    int64  `xml:"librarySectionID,attr,omitempty" json:"librarySectionID,omitempty"`
	LibrarySectionTitle string `xml:"librarySectionTitle,attr,omitempty" json:"librarySectionTitle,omitempty"`

	// Directory holds sections, shows and seasons; Metadata holds movies and
	// episodes. In XML the elements are named per entry type, which is what
	// the Elem field on entry does.
	Directory []entry `xml:"-" json:"Directory,omitempty"`
	Metadata  []entry `xml:"-" json:"Metadata,omitempty"`
}

// entry is one row inside a container: a section, a show, a season, a movie or
// an episode. One struct again, with omitempty doing the work, because a Plex
// client reads the fields it cares about for the type it asked for and ignores
// the rest.
type entry struct {
	Elem string `xml:"-" json:"-"`

	// Section fields.
	Key   string `xml:"key,attr,omitempty" json:"key,omitempty"`
	Type  string `xml:"type,attr,omitempty" json:"type,omitempty"`
	Title string `xml:"title,attr,omitempty" json:"title,omitempty"`
	Agent string `xml:"agent,attr,omitempty" json:"agent,omitempty"`

	Scanner  string `xml:"scanner,attr,omitempty" json:"scanner,omitempty"`
	Language string `xml:"language,attr,omitempty" json:"language,omitempty"`
	UUID     string `xml:"uuid,attr,omitempty" json:"uuid,omitempty"`

	// Item fields.
	RatingKey            string `xml:"ratingKey,attr,omitempty" json:"ratingKey,omitempty"`
	ParentRatingKey      string `xml:"parentRatingKey,attr,omitempty" json:"parentRatingKey,omitempty"`
	GrandparentRatingKey string `xml:"grandparentRatingKey,attr,omitempty" json:"grandparentRatingKey,omitempty"`
	GUID                 string `xml:"guid,attr,omitempty" json:"guid,omitempty"`
	Summary              string `xml:"summary,attr,omitempty" json:"summary,omitempty"`
	Index                int    `xml:"index,attr,omitempty" json:"index,omitempty"`
	ParentIndex          int    `xml:"parentIndex,attr,omitempty" json:"parentIndex,omitempty"`
	Year                 int    `xml:"year,attr,omitempty" json:"year,omitempty"`
	Duration             int64  `xml:"duration,attr,omitempty" json:"duration,omitempty"`
	ViewOffset           int64  `xml:"viewOffset,attr,omitempty" json:"viewOffset,omitempty"`
	ViewCount            int    `xml:"viewCount,attr,omitempty" json:"viewCount,omitempty"`
	LastViewedAt         int64  `xml:"lastViewedAt,attr,omitempty" json:"lastViewedAt,omitempty"`
	AddedAt              int64  `xml:"addedAt,attr,omitempty" json:"addedAt,omitempty"`
	UpdatedAt            int64  `xml:"updatedAt,attr,omitempty" json:"updatedAt,omitempty"`
	Thumb                string `xml:"thumb,attr,omitempty" json:"thumb,omitempty"`
	Art                  string `xml:"art,attr,omitempty" json:"art,omitempty"`
	ParentTitle          string `xml:"parentTitle,attr,omitempty" json:"parentTitle,omitempty"`
	GrandparentTitle     string `xml:"grandparentTitle,attr,omitempty" json:"grandparentTitle,omitempty"`
	ParentKey            string `xml:"parentKey,attr,omitempty" json:"parentKey,omitempty"`
	ParentThumb          string `xml:"parentThumb,attr,omitempty" json:"parentThumb,omitempty"`
	GrandparentThumb     string `xml:"grandparentThumb,attr,omitempty" json:"grandparentThumb,omitempty"`

	// OriginalTitle is who performs this track when that is not the album's
	// artist, which is the useful half of a row on a compilation. Plex puts
	// the performer here and the clients read it from here.
	OriginalTitle string `xml:"originalTitle,attr,omitempty" json:"originalTitle,omitempty"`

	// OriginallyAvailableAt is the date a thing came out — and, for a
	// photograph, the day it was taken, which is what a client groups a
	// camera roll by.
	OriginallyAvailableAt string `xml:"originallyAvailableAt,attr,omitempty" json:"originallyAvailableAt,omitempty"`
	LibrarySectionID      int64  `xml:"librarySectionID,attr,omitempty" json:"librarySectionID,omitempty"`
	LibrarySectionTitle   string `xml:"librarySectionTitle,attr,omitempty" json:"librarySectionTitle,omitempty"`
	LeafCount             int    `xml:"leafCount,attr,omitempty" json:"leafCount,omitempty"`
	ChildCount            int    `xml:"childCount,attr,omitempty" json:"childCount,omitempty"`

	// PlaylistKind is Plex's playlistType, which a client's playlist shelf
	// filters on: a music client asking for audio playlists should not be
	// offered a list of films.
	PlaylistKind string `xml:"playlistType,attr,omitempty" json:"playlistType,omitempty"`

	// Smart says a playlist is built from a rule rather than by hand. Always
	// false here, and sent because a client that reads no value at all treats
	// some playlists as unopenable.
	Smart bool `xml:"smart,attr" json:"smart"`

	// PlaylistItemID identifies one line of a playlist rather than the thing
	// on it, because the same track twice is two lines and a client removing
	// one has to be able to say which.
	PlaylistItemID string `xml:"playlistItemID,attr,omitempty" json:"playlistItemID,omitempty"`

	Media    []media    `xml:"Media,omitempty" json:"Media,omitempty"`
	Location []location `xml:"Location,omitempty" json:"Location,omitempty"`
	Marker   []marker   `xml:"Marker,omitempty" json:"Marker,omitempty"`
	Chapter  []chapter  `xml:"Chapter,omitempty" json:"Chapter,omitempty"`
}

// marker is a stretch a client offers to skip. The attribute names are Plex's,
// down to the millisecond offsets, because that is what the clients read: Bask
// looks for exactly id, type, startTimeOffset and endTimeOffset, and drops any
// marker missing one of them.
type marker struct {
	ID              int64  `xml:"id,attr" json:"id"`
	Type            string `xml:"type,attr" json:"type"`
	StartTimeOffset int64  `xml:"startTimeOffset,attr" json:"startTimeOffset"`
	EndTimeOffset   int64  `xml:"endTimeOffset,attr" json:"endTimeOffset"`
	Final           bool   `xml:"final,attr,omitempty" json:"final,omitempty"`
}

// chapter is a chapter on the timeline. Plex carries the title as `tag`; Bask
// accepts either, and sending both costs nothing and helps anything that only
// reads one.
type chapter struct {
	ID              int64  `xml:"id,attr" json:"id"`
	Index           int    `xml:"index,attr,omitempty" json:"index,omitempty"`
	Tag             string `xml:"tag,attr,omitempty" json:"tag,omitempty"`
	Title           string `xml:"title,attr,omitempty" json:"title,omitempty"`
	StartTimeOffset int64  `xml:"startTimeOffset,attr" json:"startTimeOffset"`
	EndTimeOffset   int64  `xml:"endTimeOffset,attr" json:"endTimeOffset"`
}

type media struct {
	ID              int64  `xml:"id,attr" json:"id"`
	Duration        int64  `xml:"duration,attr,omitempty" json:"duration,omitempty"`
	Container       string `xml:"container,attr,omitempty" json:"container,omitempty"`
	VideoResolution string `xml:"videoResolution,attr,omitempty" json:"videoResolution,omitempty"`

	// Width, Height and AspectRatio describe a photograph, which is the one
	// kind of item whose dimensions are its content rather than a detail of
	// how it was encoded.
	Width       int     `xml:"width,attr,omitempty" json:"width,omitempty"`
	Height      int     `xml:"height,attr,omitempty" json:"height,omitempty"`
	AspectRatio float64 `xml:"aspectRatio,attr,omitempty" json:"aspectRatio,omitempty"`

	// AudioCodec and AudioChannels are what a music client puts beside a
	// track — "FLAC · 2.0" — and what it uses to decide whether it can play
	// one without asking the server for anything.
	AudioCodec    string `xml:"audioCodec,attr,omitempty" json:"audioCodec,omitempty"`
	AudioChannels int    `xml:"audioChannels,attr,omitempty" json:"audioChannels,omitempty"`
	Bitrate       int    `xml:"bitrate,attr,omitempty" json:"bitrate,omitempty"`

	Part []part `xml:"Part" json:"Part"`
}

type part struct {
	ID        int64  `xml:"id,attr" json:"id"`
	Key       string `xml:"key,attr" json:"key"`
	File      string `xml:"file,attr,omitempty" json:"file,omitempty"`
	Size      int64  `xml:"size,attr,omitempty" json:"size,omitempty"`
	Container string `xml:"container,attr,omitempty" json:"container,omitempty"`
	Duration  int64  `xml:"duration,attr,omitempty" json:"duration,omitempty"`

	// Stream describes the tracks inside the file, and the sidecars beside
	// it. Only subtitles for now: a client reads the video and audio it is
	// playing from the file itself, and what it cannot know without being
	// told is that there is a .srt in the folder.
	Stream []stream `xml:"Stream,omitempty" json:"Stream,omitempty"`
}

// streamType values as Plex numbers them. Clients switch on these integers,
// so they are the wire, not an internal choice.
const (
	streamVideo    = 1
	streamAudio    = 2
	streamSubtitle = 3
)

type stream struct {
	ID         int64  `xml:"id,attr" json:"id"`
	StreamType int    `xml:"streamType,attr" json:"streamType"`
	Index      int    `xml:"index,attr,omitempty" json:"index,omitempty"`
	Codec      string `xml:"codec,attr,omitempty" json:"codec,omitempty"`
	Language   string `xml:"language,attr,omitempty" json:"language,omitempty"`
	// LanguageCode is the three-letter code; language is what to show.
	LanguageCode    string `xml:"languageCode,attr,omitempty" json:"languageCode,omitempty"`
	Title           string `xml:"title,attr,omitempty" json:"title,omitempty"`
	DisplayTitle    string `xml:"displayTitle,attr,omitempty" json:"displayTitle,omitempty"`
	ExtendedTitle   string `xml:"extendedDisplayTitle,attr,omitempty" json:"extendedDisplayTitle,omitempty"`
	Selected        bool   `xml:"selected,attr,omitempty" json:"selected,omitempty"`
	Default         bool   `xml:"default,attr,omitempty" json:"default,omitempty"`
	Forced          bool   `xml:"forced,attr,omitempty" json:"forced,omitempty"`
	HearingImpaired bool   `xml:"hearingImpaired,attr,omitempty" json:"hearingImpaired,omitempty"`
	// Key is present only for a subtitle that is a file of its own, which is
	// how a Plex client tells a sidecar from a stream in the container, and
	// where it fetches the text from.
	Key string `xml:"key,attr,omitempty" json:"key,omitempty"`
}

type location struct {
	ID   int64  `xml:"id,attr,omitempty" json:"id,omitempty"`
	Path string `xml:"path,attr" json:"path"`
}

// write sends a container as XML or JSON depending on what the client asked
// for, which Plex decides from the Accept header alone.
func write(w http.ResponseWriter, r *http.Request, c container) {
	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json")
		// A JSON MediaContainer is wrapped in its own name, unlike the XML.
		if err := json.NewEncoder(w).Encode(map[string]container{"MediaContainer": c}); err != nil {
			return
		}
		return
	}

	w.Header().Set("Content-Type", "application/xml;charset=utf-8")
	if _, err := w.Write([]byte(xml.Header)); err != nil {
		return
	}
	if err := encodeXML(w, c); err != nil {
		return
	}
}

func wantsJSON(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	for _, candidate := range []string{"application/json", "text/json"} {
		if len(accept) >= len(candidate) && containsFold(accept, candidate) {
			return true
		}
	}
	return false
}

func containsFold(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if equalFold(haystack[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// encodeXML writes the container with each entry under its own element name.
//
// encoding/xml cannot choose an element name per slice member, and Plex's
// clients care: a section is a <Directory>, a movie is a <Video>. The entries
// are therefore marshalled one at a time with a name supplied, which is also
// why the container's own slices are tagged xml:"-".
func encodeXML(w http.ResponseWriter, c container) error {
	enc := xml.NewEncoder(w)
	start := xml.StartElement{Name: xml.Name{Local: "MediaContainer"}, Attr: containerAttrs(c)}
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	for _, group := range [][]entry{c.Directory, c.Metadata} {
		for _, e := range group {
			name := e.Elem
			if name == "" {
				name = "Directory"
			}
			if err := enc.EncodeElement(e, xml.StartElement{Name: xml.Name{Local: name}}); err != nil {
				return err
			}
		}
	}
	if err := enc.EncodeToken(start.End()); err != nil {
		return err
	}
	return enc.Flush()
}

func containerAttrs(c container) []xml.Attr {
	var attrs []xml.Attr
	add := func(name, value string) {
		if value != "" {
			attrs = append(attrs, xml.Attr{Name: xml.Name{Local: name}, Value: value})
		}
	}
	add("size", strconv.Itoa(c.Size))
	if c.TotalSize > 0 {
		add("totalSize", strconv.Itoa(c.TotalSize))
	}
	if c.Offset > 0 {
		add("offset", strconv.Itoa(c.Offset))
	}
	add("identifier", c.Identifier)
	add("machineIdentifier", c.MachineIdentifier)
	add("version", c.Version)
	add("friendlyName", c.FriendlyName)
	if c.Claimed {
		add("claimed", "1")
	}
	add("title1", c.Title1)
	add("title2", c.Title2)
	add("viewGroup", c.ViewGroup)
	add("mediaTagPrefix", c.MediaTagPrefix)
	if c.AllowSync {
		add("allowSync", "1")
	}
	if c.LibrarySectionID > 0 {
		add("librarySectionID", strconv.FormatInt(c.LibrarySectionID, 10))
	}
	add("librarySectionTitle", c.LibrarySectionTitle)
	return attrs
}

// elementFor is the XML element name a kind is served as.
//
// Plex names the element after what the thing is, and the clients switch on
// it: anything a client descends into is a Directory, a film or an episode is
// a Video, and music and photographs have elements of their own — a client
// that is handed a <Video> for a track will try to show a picture for it.
func elementFor(kind domain.Kind) string {
	switch kind {
	case domain.KindShow, domain.KindSeason, domain.KindArtist, domain.KindAlbum, domain.KindPhotoAlbum:
		return "Directory"
	case domain.KindTrack:
		return "Track"
	case domain.KindPhoto:
		return "Photo"
	default:
		return "Video"
	}
}

// container reports whether a kind gathers other things rather than being a
// file, which is what decides whether its key points at its children.
func isContainer(kind domain.Kind) bool { return elementFor(kind) == "Directory" }

// plexType is the type string a client switches on.
func plexType(kind domain.Kind) string { return string(kind) }

func millisOf(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func keyFor(id int64) string { return fmt.Sprintf("/library/metadata/%d", id) }
