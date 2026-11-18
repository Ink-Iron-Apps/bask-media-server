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
	LibrarySectionID     int64  `xml:"librarySectionID,attr,omitempty" json:"librarySectionID,omitempty"`
	LibrarySectionTitle  string `xml:"librarySectionTitle,attr,omitempty" json:"librarySectionTitle,omitempty"`
	LeafCount            int    `xml:"leafCount,attr,omitempty" json:"leafCount,omitempty"`
	ChildCount           int    `xml:"childCount,attr,omitempty" json:"childCount,omitempty"`

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
	Part            []part `xml:"Part" json:"Part"`
}

type part struct {
	ID        int64  `xml:"id,attr" json:"id"`
	Key       string `xml:"key,attr" json:"key"`
	File      string `xml:"file,attr,omitempty" json:"file,omitempty"`
	Size      int64  `xml:"size,attr,omitempty" json:"size,omitempty"`
	Container string `xml:"container,attr,omitempty" json:"container,omitempty"`
	Duration  int64  `xml:"duration,attr,omitempty" json:"duration,omitempty"`
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

// elementFor is the XML element name a kind is served as. Shows and seasons
// are containers a client descends into, so they are Directory; movies and
// episodes are playable, so they are Video.
func elementFor(kind domain.Kind) string {
	switch kind {
	case domain.KindShow, domain.KindSeason:
		return "Directory"
	default:
		return "Video"
	}
}

// plexType is the type string a client switches on.
func plexType(kind domain.Kind) string { return string(kind) }

func millisOf(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func keyFor(id int64) string { return fmt.Sprintf("/library/metadata/%d", id) }
