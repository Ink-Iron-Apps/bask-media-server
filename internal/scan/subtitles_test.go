package scan_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/ffprobe"
	"github.com/inknironapps/bask-media-server/internal/scan"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// withSubtitles is a probe that reports a film with one embedded subtitle
// stream, which is what a remux of a disc looks like.
type withSubtitles struct{}

func (withSubtitles) Probe(context.Context, string) (ffprobe.Info, error) {
	return ffprobe.Info{
		Duration: 90 * time.Minute,
		Streams: []ffprobe.Stream{
			{Index: 0, Type: "video", Codec: "h264", Height: 1080},
			{Index: 1, Type: "audio", Codec: "aac", Channels: 6},
			{Index: 2, Type: "subtitle", Codec: "subrip", Language: "fre", Title: "Français", Default: true},
			{Index: 3, Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "eng", Forced: true},
		},
	}, nil
}

// A library the way the renamers actually write one: the film, its subtitles
// beside it, and a second film's subtitle in the same folder.
func TestAScanFindsSidecarsAndEmbeddedStreams(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	write := func(name string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Dinotopia (2002).mkv")
	write("Dinotopia (2002).en.srt")
	write("Dinotopia (2002).eng.forced.srt")
	write("Dinotopia (2002).fr.sdh.srt")
	write("Something Else (1999).en.srt")   // another film's, in the same folder
	write("Dinotopia (2002) extras.srt")    // belongs to "Dinotopia (2002) extras.mkv"
	write("Dinotopia (2002) - English.srt") // a separator other than a dot, with a language

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bms.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	section, err := db.CreateSection(ctx, "Films", domain.SectionMovies, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	scanner := &scan.Scanner{DB: db, Workers: 2, Prober: withSubtitles{}}
	if _, err := scanner.Section(ctx, *section); err != nil {
		t.Fatal(err)
	}

	items, _, err := db.SectionItems(ctx, section.ID, domain.KindMovie, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d films: %+v", len(items), items)
	}
	subs, err := db.Subtitles(ctx, []int64{items[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	found := subs[items[0].ID]
	if len(found) != 6 {
		t.Fatalf("got %d subtitles, want two embedded and four beside the film:\n%+v", len(found), found)
	}

	var (
		external  int
		languages = map[string]int{}
		forced    int
		hearing   int
		pictures  int
	)
	for _, sub := range found {
		if sub.External {
			external++
			if sub.Path == "" {
				t.Errorf("an external subtitle has no path: %+v", sub)
			}
		} else if sub.Path != filepath.Join(root, "Dinotopia (2002).mkv") {
			// An embedded subtitle's text is inside the media file, and the
			// row has to say which file that is: without it, reading one
			// means looking up its part first, and the first attempt at this
			// shipped with an empty path and a 500 at the other end.
			t.Errorf("an embedded subtitle does not name the file it is in: %+v", sub)
		}
		languages[sub.Language]++
		if sub.Forced {
			forced++
		}
		if sub.Hearing {
			hearing++
		}
		if !sub.Text() {
			pictures++
		}
	}
	if external != 4 {
		t.Errorf("got %d sidecars, want the four that belong to this film", external)
	}
	for _, sub := range found {
		if strings.Contains(sub.Path, "extras") {
			t.Errorf("another film's subtitle was attached to this one: %s", sub.Path)
		}
	}
	if languages["eng"] != 4 {
		// "en" and "eng" are the same language written two ways, and a client
		// grouping by language must see one.
		t.Errorf("English subtitles = %d, want four: %+v", languages["eng"], languages)
	}
	if languages["fre"] != 2 {
		t.Errorf("French subtitles = %d, want the embedded one and the SDH sidecar: %+v", languages["fre"], languages)
	}
	if forced != 2 || hearing != 1 {
		t.Errorf("forced = %d, hearing impaired = %d: %+v", forced, hearing, found)
	}
	if pictures != 1 {
		t.Errorf("picture subtitles = %d, want the one PGS stream", pictures)
	}
}

// A deleted sidecar has to stop being offered, and a scan with no probe must
// not delete the embedded streams an earlier scan with one recorded.
func TestARescanFollowsWhatIsThereNow(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	film := filepath.Join(root, "Dinotopia (2002).mkv")
	sidecar := filepath.Join(root, "Dinotopia (2002).en.srt")
	for _, path := range []string{film, sidecar} {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bms.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	section, err := db.CreateSection(ctx, "Films", domain.SectionMovies, []string{root})
	if err != nil {
		t.Fatal(err)
	}

	scanner := &scan.Scanner{DB: db, Workers: 1, Prober: withSubtitles{}}
	if _, err := scanner.Section(ctx, *section); err != nil {
		t.Fatal(err)
	}
	items, _, err := db.SectionItems(ctx, section.ID, domain.KindMovie, store.Page{})
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %+v (%v)", items, err)
	}
	id := items[0].ID

	if err := os.Remove(sidecar); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Section(ctx, *section); err != nil {
		t.Fatal(err)
	}
	subs, err := db.Subtitles(ctx, []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range subs[id] {
		if sub.External {
			t.Errorf("a deleted sidecar is still offered: %+v", sub)
		}
	}
	if len(subs[id]) != 2 {
		t.Errorf("got %d embedded subtitles after the rescan, want the two from the container", len(subs[id]))
	}

	// And a scan on a machine with no ffprobe keeps what the probe found.
	blind := &scan.Scanner{DB: db, Workers: 1}
	if _, err := blind.Section(ctx, *section); err != nil {
		t.Fatal(err)
	}
	subs, err = db.Subtitles(ctx, []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	if len(subs[id]) != 2 {
		t.Errorf("a scan with no probe deleted the embedded subtitles: %+v", subs[id])
	}
}
