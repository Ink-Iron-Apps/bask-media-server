package scan_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/artwork"
	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/ffprobe"
	"github.com/inknironapps/bask-media-server/internal/scan"
	"github.com/inknironapps/bask-media-server/internal/store"
)

/* ---------- what a path and a tag say between them ---------- */

func TestTagsDecideWhereATrackGoes(t *testing.T) {
	root := "/music"
	path := "/music/whatever/also whatever/track01.flac"

	track := scan.ParseTrack(root, path, scan.Tags{
		Title: "So What", Artist: "Miles Davis", AlbumArtist: "Miles Davis",
		Album: "Kind of Blue", Date: "1959-08-17", Track: 1,
	})

	if track.Artist != "Miles Davis" || track.Album != "Kind of Blue" {
		t.Errorf("filed under %q / %q", track.Artist, track.Album)
	}
	if track.Title != "So What" || track.Number != 1 || track.Year != 1959 {
		t.Errorf("track = %+v", track)
	}
}

// The layout every ripper writes, and the only thing to go on for a file
// whose tags were never filled in.
func TestWithoutTagsThePathDecides(t *testing.T) {
	root := "/music"
	track := scan.ParseTrack(root, "/music/Miles Davis/Kind of Blue (1959)/02 - Blue in Green.flac", scan.Tags{})

	if track.Artist != "Miles Davis" {
		t.Errorf("artist = %q, want the folder above the album", track.Artist)
	}
	if track.Album != "Kind of Blue" || track.Year != 1959 {
		t.Errorf("album = %q (%d)", track.Album, track.Year)
	}
	if track.Number != 2 || track.Title != "Blue in Green" {
		t.Errorf("track %d %q, want 2 and the name after it", track.Number, track.Title)
	}
}

// Two discs of one album both have a track 1, and nothing in the filename
// distinguishes them: the folder does.
func TestAPerDiscFolderNamesTheDisc(t *testing.T) {
	root := "/music"
	one := scan.ParseTrack(root, "/music/Prince/Sign o the Times (1987)/CD1/01 - Sign o the Times.flac", scan.Tags{})
	two := scan.ParseTrack(root, "/music/Prince/Sign o the Times (1987)/Disc 2/01 - U Got the Look.flac", scan.Tags{})

	if one.Disc != 1 || two.Disc != 2 {
		t.Errorf("discs = %d and %d", one.Disc, two.Disc)
	}
	// Both belong to the same album, not to one called "CD1".
	if one.Album != "Sign o the Times" || two.Album != one.Album {
		t.Errorf("albums = %q and %q", one.Album, two.Album)
	}
	if one.Artist != "Prince" || two.Artist != "Prince" {
		t.Errorf("artists = %q and %q", one.Artist, two.Artist)
	}
}

// "1-01 Title" is the other way a ripper writes a multi-disc album.
func TestADiscAndTrackNumberInTheFilename(t *testing.T) {
	track := scan.ParseTrack("/music", "/music/Band/Album/2-05 Closing Time.mp3", scan.Tags{})

	if track.Disc != 2 || track.Number != 5 {
		t.Errorf("disc %d track %d, want 2 and 5", track.Disc, track.Number)
	}
	if track.Title != "Closing Time" {
		t.Errorf("title = %q", track.Title)
	}
}

// A compilation is one album by many people. The album artist keeps it
// together and the performer is what tells the rows apart.
func TestACompilationKeepsItsAlbumArtistAndItsPerformers(t *testing.T) {
	track := scan.ParseTrack("/music", "/music/Various/Now 42/07 - song.mp3", scan.Tags{
		Title: "Song", Artist: "Somebody Else", AlbumArtist: "Various Artists",
		Album: "Now 42", Track: 7,
	})

	if track.Artist != "Various Artists" {
		t.Errorf("artist = %q, want the album's", track.Artist)
	}
	if track.Performer != "Somebody Else" {
		t.Errorf("performer = %q", track.Performer)
	}
}

// A performer that is the album's artist is not worth repeating on every row.
func TestAPerformerThatIsTheArtistIsNotRepeated(t *testing.T) {
	track := scan.ParseTrack("/music", "/music/x/y/01 - a.mp3", scan.Tags{
		Artist: "Miles Davis", AlbumArtist: "Miles Davis", Album: "Kind of Blue", Title: "So What",
	})
	if track.Performer != "" {
		t.Errorf("performer = %q, want nothing: it is the album's artist", track.Performer)
	}
}

// A file with no tags, loose at the top of a library. Filed under a name
// somebody can find rather than dropped.
func TestAFileWithNothingToGoOnIsStillFiled(t *testing.T) {
	track := scan.ParseTrack("/music", "/music/someaudio.mp3", scan.Tags{})

	if track.Artist != scan.UnknownArtist || track.Album != scan.UnknownAlbum {
		t.Errorf("filed under %q / %q", track.Artist, track.Album)
	}
	if track.Title != "someaudio" {
		t.Errorf("title = %q", track.Title)
	}
}

/* ---------- a scan of a music library ---------- */

// tagged is a probe that answers from a table, the way ffprobe answers from a
// file's own metadata.
type tagged struct {
	tags map[string]ffprobe.Tags
}

func (p tagged) Probe(_ context.Context, path string) (ffprobe.Info, error) {
	return ffprobe.Info{
		Duration: 3 * time.Minute,
		Tags:     p.tags[filepath.Base(path)],
		Streams:  []ffprobe.Stream{{Index: 0, Type: "audio", Codec: "flac", Channels: 2}},
	}, nil
}

func musicLibrary(t *testing.T) (*store.DB, domain.Section, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()

	files := map[string][]byte{
		"Miles Davis/Kind of Blue (1959)/cover.jpg":              []byte("a cover"),
		"Miles Davis/Kind of Blue (1959)/01 - So What.flac":      []byte("x"),
		"Miles Davis/Kind of Blue (1959)/02 - Freddie.flac":      []byte("x"),
		"Prince/Sign o the Times/CD1/01 - Sign o the Times.flac": []byte("x"),
		"Prince/Sign o the Times/CD2/01 - U Got the Look.flac":   []byte("x"),
		"loose.mp3": []byte("x"),
		// Not music, and not a photograph either: a picture in a music
		// library is a cover, which is never a row of its own.
		"Miles Davis/folder.jpg": []byte("an artist picture"),
	}
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bms.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	section, err := db.CreateSection(ctx, "Music", domain.SectionMusic, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	return db, *section, root
}

func TestAMusicScanBuildsArtistsAlbumsAndTracks(t *testing.T) {
	ctx := context.Background()
	db, section, _ := musicLibrary(t)

	prober := tagged{tags: map[string]ffprobe.Tags{
		"01 - So What.flac": {
			Title: "So What", Artist: "Miles Davis", AlbumArtist: "Miles Davis",
			Album: "Kind of Blue", Date: "1959", Track: 1,
		},
		"02 - Freddie.flac": {
			Title: "Freddie Freeloader", Artist: "Miles Davis", AlbumArtist: "Miles Davis",
			Album: "Kind of Blue", Date: "1959", Track: 2,
		},
	}}
	images, err := artwork.New(filepath.Join(t.TempDir(), "art"), 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	scanner := &scan.Scanner{DB: db, Workers: 2, Prober: prober, Artwork: images}
	result, err := scanner.Section(ctx, section)
	if err != nil {
		t.Fatal(err)
	}
	if result.Files != 5 {
		t.Errorf("wrote %d files, want the five audio files and none of the pictures", result.Files)
	}

	artists, _, err := db.SectionItems(ctx, section.ID, domain.KindArtist, store.Page{Size: 50})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]domain.Item{}
	for _, artist := range artists {
		names[artist.Title] = artist
	}
	for _, want := range []string{"Miles Davis", "Prince", scan.UnknownArtist} {
		if _, found := names[want]; !found {
			t.Errorf("no artist %q; got %v", want, names)
		}
	}

	// The album, under the artist, with the year the folder and the tags both
	// said, and the cover that was beside it.
	albums, _, err := db.Children(ctx, names["Miles Davis"].ID, store.Page{Size: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(albums) != 1 || albums[0].Title != "Kind of Blue" {
		t.Fatalf("albums = %+v", albums)
	}
	if albums[0].Year != 1959 {
		t.Errorf("year = %d", albums[0].Year)
	}
	if albums[0].PosterKey == "" {
		t.Error("the cover.jpg beside the album was not kept")
	}
	if names["Miles Davis"].PosterKey == "" {
		t.Error("the folder.jpg in the artist's folder was not kept")
	}

	tracks, _, err := db.Children(ctx, albums[0].ID, store.Page{Size: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 {
		t.Fatalf("got %d tracks", len(tracks))
	}
	if tracks[0].Title != "So What" || tracks[0].Index != 1 {
		t.Errorf("first track = %+v", tracks[0])
	}
	if tracks[1].Index != 2 || tracks[1].Duration != 3*time.Minute {
		t.Errorf("second track = %+v", tracks[1])
	}
	if len(tracks[0].Parts) != 1 {
		t.Errorf("a track is a file: %+v", tracks[0].Parts)
	}
}

// Two discs, each with a track 1. Without the disc as part of a track's
// identity the second one overwrites the first and the album loses a song.
func TestBothDiscsOfAnAlbumSurvive(t *testing.T) {
	ctx := context.Background()
	db, section, _ := musicLibrary(t)

	scanner := &scan.Scanner{DB: db, Workers: 1, Prober: tagged{}}
	if _, err := scanner.Section(ctx, section); err != nil {
		t.Fatal(err)
	}

	artists, _, err := db.SectionItems(ctx, section.ID, domain.KindArtist, store.Page{Size: 50})
	if err != nil {
		t.Fatal(err)
	}
	var prince domain.Item
	for _, artist := range artists {
		if artist.Title == "Prince" {
			prince = artist
		}
	}
	albums, _, err := db.Children(ctx, prince.ID, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(albums) != 1 {
		t.Fatalf("a two-disc album is one album: %+v", albums)
	}

	tracks, _, err := db.Children(ctx, albums[0].ID, store.Page{Size: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 {
		t.Fatalf("got %d tracks across two discs", len(tracks))
	}
	if tracks[0].Disc != 1 || tracks[1].Disc != 2 {
		t.Errorf("discs = %d and %d", tracks[0].Disc, tracks[1].Disc)
	}
	// Ordered by disc and then by track, which is the order they play in.
	if tracks[0].Title != "Sign o the Times" {
		t.Errorf("first track = %q", tracks[0].Title)
	}
}

// A library with no folder images at all: the cover is inside the files, and
// a wall of blank squares would be the wrong answer when the picture has been
// there all along.
func TestACoverIsTakenFromInsideTheFileWhenTheFolderHasNone(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dir := filepath.Join(root, "Band", "Album")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "01 - song.mp3"), mp3WithCover("JPEGBYTES"), 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bms.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	section, err := db.CreateSection(ctx, "Music", domain.SectionMusic, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	images, err := artwork.New(filepath.Join(t.TempDir(), "art"), 0, 1)
	if err != nil {
		t.Fatal(err)
	}

	scanner := &scan.Scanner{DB: db, Workers: 1, Prober: tagged{}, Artwork: images}
	if _, err := scanner.Section(ctx, *section); err != nil {
		t.Fatal(err)
	}

	artists, _, err := db.SectionItems(ctx, section.ID, domain.KindArtist, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(artists) != 1 {
		t.Fatalf("artists = %+v", artists)
	}
	albums, _, err := db.Children(ctx, artists[0].ID, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(albums) != 1 || albums[0].PosterKey == "" {
		t.Fatalf("the cover inside the file was not kept: %+v", albums)
	}
}

// mp3WithCover is an ID3v2.4 tag with one front-cover frame, which is what a
// file from a streaming service carries.
func mp3WithCover(data string) []byte {
	var frame bytes.Buffer
	frame.WriteByte(0)
	frame.WriteString("image/jpeg")
	frame.WriteByte(0)
	frame.WriteByte(3) // front cover
	frame.WriteByte(0) // no description
	frame.WriteString(data)

	syncSafe := func(n int) []byte {
		return []byte{byte(n >> 21 & 0x7F), byte(n >> 14 & 0x7F), byte(n >> 7 & 0x7F), byte(n & 0x7F)}
	}
	var body bytes.Buffer
	body.WriteString("APIC")
	body.Write(syncSafe(frame.Len()))
	body.Write([]byte{0, 0})
	body.Write(frame.Bytes())

	var out bytes.Buffer
	out.WriteString("ID3")
	out.Write([]byte{4, 0, 0})
	out.Write(syncSafe(body.Len()))
	out.Write(body.Bytes())
	return out.Bytes()
}
