package plexapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

const subtitleText = "1\n00:00:02,000 --> 00:00:05,500\nThey are not from here.\n"

// Bask reads the Stream list on a Part to build its subtitle menu, and tells
// a sidecar from an embedded track by whether the stream carries a key. So
// the shape of this is not cosmetic either.
func TestSubtitlesAreServedInPlexsShape(t *testing.T) {
	srv, token, db := library(t)
	ctx := context.Background()

	movie := findMovie(t, db, "Arrival")
	parts, err := db.Parts(ctx, movie.ID)
	if err != nil || len(parts) == 0 {
		t.Fatalf("parts = %v (%v)", parts, err)
	}
	if err := db.ReplaceSubtitles(ctx, movie.ID, parts[0].ID, []domain.Subtitle{
		{External: true, StreamIndex: -1, Path: sidecarPath(t, db, movie.ID),
			Codec: "srt", Language: "eng", Forced: true},
		{StreamIndex: 2, Codec: "subrip", Language: "fre", Title: "Français", Default: true},
		{StreamIndex: 3, Codec: "hdmv_pgs_subtitle", Language: "eng"},
	}); err != nil {
		t.Fatal(err)
	}

	item := firstMetadata(t, get(t, srv, token, "/library/metadata/"+itoa(movie.ID)))
	media, _ := item["Media"].([]any)
	if len(media) == 0 {
		t.Fatalf("no Media: %v", item)
	}
	first, _ := media[0].(map[string]any)
	partList, _ := first["Part"].([]any)
	part, _ := partList[0].(map[string]any)

	streams, _ := part["Stream"].([]any)
	if len(streams) != 3 {
		t.Fatalf("got %d streams, want the three subtitles: %v", len(streams), part)
	}

	var sidecar, embedded map[string]any
	for _, raw := range streams {
		stream, _ := raw.(map[string]any)
		if stream["streamType"] != float64(3) {
			t.Errorf("a subtitle has streamType %v, want 3", stream["streamType"])
		}
		if _, hasKey := stream["key"]; hasKey {
			sidecar = stream
		} else if stream["codec"] == "subrip" {
			embedded = stream
		}
	}
	if sidecar == nil || embedded == nil {
		t.Fatalf("expected one sidecar with a key and one embedded without: %v", streams)
	}
	if key, _ := sidecar["key"].(string); !strings.HasPrefix(key, "/library/streams/") {
		t.Errorf("sidecar key = %v", sidecar["key"])
	}
	if sidecar["forced"] != true || sidecar["languageCode"] != "eng" {
		t.Errorf("sidecar = %v", sidecar)
	}
	if embedded["default"] != true || embedded["languageCode"] != "fre" {
		t.Errorf("embedded = %v", embedded)
	}
	if title, _ := embedded["displayTitle"].(string); !strings.Contains(title, "French") {
		t.Errorf("displayTitle = %v, want something a person can read", embedded["displayTitle"])
	}
}

// A client fetches the text at the key it was given, with the extension it
// wants: Bask asks for .srt, a browser for .vtt, and both are the same file
// read two ways.
func TestTheTextOfASidecarIsServed(t *testing.T) {
	srv, token, db := library(t)
	ctx := context.Background()

	movie := findMovie(t, db, "Arrival")
	parts, err := db.Parts(ctx, movie.ID)
	if err != nil || len(parts) == 0 {
		t.Fatalf("parts = %v (%v)", parts, err)
	}
	path := sidecarPath(t, db, movie.ID)
	if err := os.WriteFile(path, []byte(subtitleText), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceSubtitles(ctx, movie.ID, parts[0].ID, []domain.Subtitle{
		{External: true, StreamIndex: -1, Path: path, Codec: "srt", Language: "eng"},
	}); err != nil {
		t.Fatal(err)
	}
	subs, err := db.Subtitles(ctx, []int64{movie.ID})
	if err != nil || len(subs[movie.ID]) != 1 {
		t.Fatalf("subtitles = %v (%v)", subs, err)
	}
	id := itoa(subs[movie.ID][0].ID)

	srt := text(t, srv, token, "/library/streams/"+id+".srt")
	if srt != subtitleText {
		t.Errorf("SubRip came back changed:\n%s", srt)
	}

	vtt := text(t, srv, token, "/library/streams/"+id+".vtt")
	if !strings.HasPrefix(vtt, "WEBVTT") {
		t.Fatalf("WebVTT has no header:\n%s", vtt)
	}
	if !strings.Contains(vtt, "00:00:02.000 --> 00:00:05.500") {
		t.Errorf("timestamps were not converted:\n%s", vtt)
	}
	if !strings.Contains(vtt, "They are not from here.") {
		t.Errorf("the text was lost:\n%s", vtt)
	}
}

// A picture of words cannot be handed over as text, and the client has to be
// able to tell that apart from a server fault.
func TestAPictureSubtitleIsNotServedAsText(t *testing.T) {
	srv, token, db := library(t)
	ctx := context.Background()

	movie := findMovie(t, db, "Arrival")
	parts, err := db.Parts(ctx, movie.ID)
	if err != nil || len(parts) == 0 {
		t.Fatalf("parts = %v (%v)", parts, err)
	}
	if err := db.ReplaceSubtitles(ctx, movie.ID, parts[0].ID, []domain.Subtitle{
		{StreamIndex: 2, Codec: "hdmv_pgs_subtitle", Language: "eng"},
	}); err != nil {
		t.Fatal(err)
	}
	subs, err := db.Subtitles(ctx, []int64{movie.ID})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/library/streams/"+itoa(subs[movie.ID][0].ID)+".srt", nil)
	req.Header.Set("X-Plex-Token", token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "picture") {
		t.Errorf("the answer does not say why: %s", rec.Body.String())
	}
}

func findMovie(t *testing.T, db *store.DB, title string) domain.Item {
	t.Helper()
	ctx := context.Background()
	sections, err := db.Sections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range sections {
		items, _, err := db.SectionItems(ctx, section.ID, domain.KindMovie, store.Page{})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			if strings.Contains(item.Title, title) {
				return item
			}
		}
	}
	t.Fatalf("no movie called %q", title)
	return domain.Item{}
}

// sidecarPath is the .srt the fixture library writes beside Arrival.
func sidecarPath(t *testing.T, db *store.DB, itemID int64) string {
	t.Helper()
	parts, err := db.Parts(context.Background(), itemID)
	if err != nil || len(parts) == 0 {
		t.Fatalf("parts = %v (%v)", parts, err)
	}
	return strings.TrimSuffix(parts[0].Path, ".mkv") + ".srt"
}

func text(t *testing.T, srv interface{ Handler() http.Handler }, token, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Plex-Token", token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d, body %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}
