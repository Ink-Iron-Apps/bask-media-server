package jellyfin_test

import (
	"context"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
	"github.com/inknironapps/bask-media-server/internal/transcode"
)

// burnable is a converting server whose film has two subtitles: one a
// sequence of pictures, which no client can render, and one text.
func burnable(t *testing.T) (call, string, *store.DB) {
	t.Helper()
	server, token, db := library(t)
	manager, err := transcode.New(transcode.Options{
		FFmpeg: fakeEncoder(t),
		Root:   filepath.Join(t.TempDir(), "sessions"),
		Accel:  "off",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Shutdown)
	server.Prober = streamProber{}
	server.Transcoder = manager

	c := call{t: t, server: server, token: token}
	film := c.film()

	ctx := context.Background()
	id, err := strconv.ParseInt(strings.TrimLeft(film, "0"), 16, 64)
	if err != nil {
		t.Fatalf("the film's id is %q: %v", film, err)
	}
	parts, err := db.Parts(ctx, id)
	if err != nil || len(parts) == 0 {
		t.Fatalf("parts = %v (%v)", parts, err)
	}
	if err := db.ReplaceSubtitles(ctx, id, parts[0].ID, []domain.Subtitle{
		{StreamIndex: 2, Codec: "hdmv_pgs_subtitle", Language: "fre"},
		{StreamIndex: 3, Codec: "subrip", Language: "eng"},
	}); err != nil {
		t.Fatal(err)
	}
	return c, film, db
}

// sessionOf is the stream id inside a variant playlist, which is where this
// surface puts it.
func sessionOf(t *testing.T, body []byte) string {
	t.Helper()
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.Contains(line, "/hls/") {
			continue
		}
		rest := line[strings.Index(line, "/hls/")+len("/hls/"):]
		if cut := strings.Index(rest, "/"); cut > 0 {
			return rest[:cut]
		}
	}
	t.Fatalf("no stream in the playlist: %s", body)
	return ""
}

// Two subtitles are two different pictures, so they are two encoders: a
// viewer switching tracks must not be handed the stream that is already
// drawing the other one.
func TestEachBurnedInSubtitleIsItsOwnStream(t *testing.T) {
	c, film, _ := burnable(t)
	base := "/Videos/" + film + "/main.m3u8?PlaySessionId=play-1&VideoCodec=h264&AudioCodec=aac"

	res, first := c.do(http.MethodGet, base+"&SubtitleStreamIndex=0&SubtitleMethod=Encode", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("playlist = %d: %s", res.StatusCode, first)
	}
	res, second := c.do(http.MethodGet, base+"&SubtitleStreamIndex=1&SubtitleMethod=Encode", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("playlist = %d: %s", res.StatusCode, second)
	}

	one, two := sessionOf(t, first), sessionOf(t, second)
	if one == two {
		t.Fatalf("both subtitles were handed the same encoder: %s", one)
	}

	// Asking again for the first is the first stream again, because that is
	// what a client re-reading its playlist costs.
	_, again := c.do(http.MethodGet, base+"&SubtitleStreamIndex=0&SubtitleMethod=Encode", nil)
	if sessionOf(t, again) != one {
		t.Fatalf("asking twice started two encoders")
	}

	// And the client saying it has stopped stops everything that play
	// session started, which is the whole point of the keys sharing a
	// prefix: it names only the play session.
	if res, _ := c.do(http.MethodDelete, "/Videos/ActiveEncodings?PlaySessionId=play-1", nil); res.StatusCode != http.StatusNoContent {
		t.Fatalf("stop = %d", res.StatusCode)
	}
	for _, session := range []string{one, two} {
		res, _ := c.do(http.MethodGet, "/Videos/"+film+"/hls/"+session+"/seg00000.m4s", nil)
		if res.StatusCode != http.StatusGone {
			t.Errorf("stream %s = %d after the client stopped, want 410", session, res.StatusCode)
		}
	}
}

// A client that asks for a text subtitle without asking for it to be encoded
// is rendering it itself, so nothing is burned in — that stream is the same
// one it would have got with no subtitle at all.
func TestATextSubtitleIsLeftToTheClient(t *testing.T) {
	c, film, _ := burnable(t)
	base := "/Videos/" + film + "/main.m3u8?PlaySessionId=play-2&VideoCodec=h264&AudioCodec=aac"

	_, plain := c.do(http.MethodGet, base, nil)
	_, chosen := c.do(http.MethodGet, base+"&SubtitleStreamIndex=1", nil)
	if sessionOf(t, plain) != sessionOf(t, chosen) {
		t.Fatal("choosing a text subtitle started a second encoder for nothing")
	}
}

// A picture subtitle is burned in whether or not the client said to: there is
// no other way to deliver one, and a film that plays with the subtitles
// silently missing is worse than one that costs a conversion.
func TestAPictureSubtitleIsBurnedInWithoutBeingAsked(t *testing.T) {
	c, film, _ := burnable(t)
	base := "/Videos/" + film + "/main.m3u8?PlaySessionId=play-3&VideoCodec=h264&AudioCodec=aac"

	_, plain := c.do(http.MethodGet, base, nil)
	_, picture := c.do(http.MethodGet, base+"&SubtitleStreamIndex=0", nil)
	if sessionOf(t, plain) == sessionOf(t, picture) {
		t.Fatal("a picture subtitle was not burned in")
	}
}
