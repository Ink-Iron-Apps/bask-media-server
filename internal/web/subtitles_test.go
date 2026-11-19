package web_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

const vttSample = "1\n00:00:02,000 --> 00:00:05,500\nThey are not from here.\n"

// What the player needs is a menu: what exists, what to call it, and which
// ones this page can actually draw.
func TestTheSubtitleMenuSaysWhatCanBeShown(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.signIn()

	parts, err := h.db.Parts(ctx, h.firstEp)
	if err != nil || len(parts) == 0 {
		t.Fatalf("parts = %v (%v)", parts, err)
	}
	path := filepath.Join(t.TempDir(), "episode.en.srt")
	if err := os.WriteFile(path, []byte(vttSample), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.db.ReplaceSubtitles(ctx, h.firstEp, parts[0].ID, []domain.Subtitle{
		{External: true, StreamIndex: -1, Path: path, Codec: "srt", Language: "eng", Forced: true},
		{StreamIndex: 3, Codec: "hdmv_pgs_subtitle", Language: "fre"},
	}); err != nil {
		t.Fatal(err)
	}

	_, body := h.call(http.MethodGet, "/web/api/items/"+strconv.FormatInt(h.firstEp, 10), nil)
	list := items(t, body, "subtitles")
	if len(list) != 2 {
		t.Fatalf("subtitles = %v", list)
	}

	var text, picture map[string]any
	for _, sub := range list {
		if sub["text"] == true {
			text = sub
		} else {
			picture = sub
		}
	}
	if text == nil || picture == nil {
		t.Fatalf("expected one showable and one picture: %v", list)
	}
	if label, _ := text["label"].(string); !strings.Contains(label, "English") || !strings.Contains(label, "forced") {
		t.Errorf("label = %q, want the language and what makes it different", label)
	}
	if url, _ := text["url"].(string); !strings.HasSuffix(url, ".vtt") {
		t.Errorf("url = %v, want WebVTT", text["url"])
	}
	if _, hasURL := picture["url"]; hasURL {
		t.Errorf("a picture subtitle was given a text URL: %v", picture)
	}
}

// The browser's <track> element reads WebVTT and nothing else, so that is
// what this endpoint produces whatever the subtitle started as.
func TestASidecarIsServedAsWebVTT(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.signIn()

	parts, err := h.db.Parts(ctx, h.firstEp)
	if err != nil || len(parts) == 0 {
		t.Fatalf("parts = %v (%v)", parts, err)
	}
	path := filepath.Join(t.TempDir(), "episode.en.srt")
	if err := os.WriteFile(path, []byte(vttSample), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.db.ReplaceSubtitles(ctx, h.firstEp, parts[0].ID, []domain.Subtitle{
		{External: true, StreamIndex: -1, Path: path, Codec: "srt", Language: "eng"},
	}); err != nil {
		t.Fatal(err)
	}
	subs, err := h.db.Subtitles(ctx, []int64{h.firstEp})
	if err != nil || len(subs[h.firstEp]) != 1 {
		t.Fatalf("subtitles = %v (%v)", subs, err)
	}

	req, err := http.NewRequest(http.MethodGet,
		h.server.URL+"/web/subtitles/"+strconv.FormatInt(subs[h.firstEp][0].ID, 10)+".vtt", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", res.StatusCode, body)
	}
	if got := res.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/vtt") {
		t.Errorf("Content-Type = %q", got)
	}
	if !strings.HasPrefix(string(body), "WEBVTT") {
		t.Fatalf("not WebVTT:\n%s", body)
	}
	if !strings.Contains(string(body), "00:00:02.000 --> 00:00:05.500") {
		t.Errorf("timestamps were not converted:\n%s", body)
	}
}

// A subtitle is library data, so it needs a session like everything else.
func TestASubtitleNeedsASession(t *testing.T) {
	h := newHarness(t)

	res, _ := h.call(http.MethodGet, "/web/subtitles/1.vtt", nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("an unauthenticated subtitle = %d, want 401", res.StatusCode)
	}
}
