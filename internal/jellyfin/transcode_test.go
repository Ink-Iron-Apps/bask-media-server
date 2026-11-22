package jellyfin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/ffprobe"
	"github.com/inknironapps/bask-media-server/internal/jellyfin"
	"github.com/inknironapps/bask-media-server/internal/transcode"
)

// streamProber reports what is inside a file, which is what a playback
// decision is made of: H.264 and AAC, a 1080p picture, and a bit rate high
// enough that a ceiling can be put under it.
type streamProber struct{}

func (streamProber) Probe(_ context.Context, _ string) (ffprobe.Info, error) {
	return ffprobe.Info{
		Duration: 30 * time.Minute,
		BitRate:  12_000_000,
		Streams: []ffprobe.Stream{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080},
			{Index: 1, Type: "audio", Codec: "aac", Channels: 6},
		},
	}, nil
}

// fakeEncoder stands in for ffmpeg: it writes what a running encoder would
// have written and then waits, so the server's side of the conversation can
// be tested without encoding anything.
func fakeEncoder(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake encoder is a shell script")
	}
	path := filepath.Join(t.TempDir(), "fake-ffmpeg")
	script := `#!/bin/sh
out="$PWD"
sleep 60 &
printf 'x' > "$out/init.mp4"
printf 'xxxx' > "$out/seg00000.m4s"
cat > "$out/stream.m3u8" <<EOT
#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:4
#EXT-X-MAP:URI="init.mp4"
#EXTINF:4.000,
seg00000.m4s
#EXT-X-ENDLIST
EOT
wait
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// converting is a server that can measure a file and convert it.
func converting(t *testing.T) (*jellyfin.Server, string) {
	t.Helper()
	server, token, _ := library(t)
	manager, err := transcode.New(transcode.Options{
		FFmpeg: fakeEncoder(t),
		Root:   filepath.Join(t.TempDir(), "sessions"),
		// Off, because detection runs the binary it is given and this one
		// waits to be killed: every test would otherwise pay ten seconds per
		// candidate accelerator before it started.
		Accel: "off",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Shutdown)
	server.Prober = streamProber{}
	server.Transcoder = manager
	return server, token
}

// profile is a device profile in the shape a client sends one.
func profile(containers, video, audio string, maxBitRate int64) map[string]any {
	body := map[string]any{
		"DeviceProfile": map[string]any{
			"Name": "a client",
			"DirectPlayProfiles": []map[string]any{
				{"Type": "Video", "Container": containers, "VideoCodec": video, "AudioCodec": audio},
			},
		},
	}
	if maxBitRate > 0 {
		body["MaxStreamingBitrate"] = maxBitRate
	}
	return body
}

func (c call) film() string {
	c.t.Helper()
	films := c.items("/Users/" + c.me() + "/Items?ParentId=" + c.views()["Films"]["Id"].(string))
	if len(films) == 0 {
		c.t.Fatal("no films")
	}
	return films[0]["Id"].(string)
}

// negotiated posts a device profile and returns the one media source in the
// answer.
func (c call) negotiated(item string, body any) map[string]any {
	c.t.Helper()
	res, raw := c.do(http.MethodPost, "/Items/"+item+"/PlaybackInfo", body)
	if res.StatusCode != http.StatusOK {
		c.t.Fatalf("PlaybackInfo: %d %s", res.StatusCode, raw)
	}
	var reply map[string]any
	if err := json.Unmarshal(raw, &reply); err != nil {
		c.t.Fatalf("PlaybackInfo: %v (%s)", err, raw)
	}
	sources, _ := reply["MediaSources"].([]any)
	if len(sources) != 1 {
		c.t.Fatalf("sources = %v", sources)
	}
	source, _ := sources[0].(map[string]any)
	source["PlaySessionId"] = reply["PlaySessionId"]
	return source
}

// A client that can play what is in the file is sent the file. This is the
// answer that costs this server nothing, and the one it should be giving
// almost always.
func TestAClientThatCanPlayTheFileIsSentTheFile(t *testing.T) {
	server, token := converting(t)
	c := call{t: t, server: server, token: token}

	source := c.negotiated(c.film(), profile("mkv,mp4", "h264,hevc", "aac,ac3", 0))
	if source["SupportsDirectPlay"] != true {
		t.Errorf("a file this client can play should be offered directly: %v", source)
	}
	if url, found := source["TranscodingUrl"]; found {
		t.Errorf("nothing has to be converted, so there should be no transcoding URL: %v", url)
	}
}

// A client that cannot decode what is in the file is offered a converted
// stream, and told why in the words the protocol uses.
func TestAClientThatCannotDecodeTheVideoIsOfferedAConvertedStream(t *testing.T) {
	server, token := converting(t)
	c := call{t: t, server: server, token: token}

	source := c.negotiated(c.film(), profile("mp4", "vp9", "aac", 0))
	if source["SupportsDirectPlay"] != false {
		t.Errorf("this client said it cannot decode H.264: %v", source)
	}
	url, _ := source["TranscodingUrl"].(string)
	if !strings.HasPrefix(url, "/Videos/") || !strings.Contains(url, "master.m3u8") {
		t.Fatalf("transcoding URL = %q", url)
	}
	if !strings.Contains(url, "VideoCodec=h264") {
		t.Errorf("the plan travels in the URL: %q", url)
	}
	if strings.Contains(url, "api_key=") {
		t.Errorf("the client appends its own token; an empty one is a token that is not valid: %q", url)
	}
	if !reasons(source)["VideoCodecNotSupported"] {
		t.Errorf("reasons = %v", source["TranscodeReasons"])
	}
}

// A ceiling on the bit rate is a client asking for a smaller version of a
// file it could otherwise play — a quality preset, which is the only way most
// clients ask for a conversion at all.
func TestABitRateCeilingIsEnoughToAskForAConversion(t *testing.T) {
	server, token := converting(t)
	c := call{t: t, server: server, token: token}

	source := c.negotiated(c.film(), profile("mkv", "h264", "aac", 3_000_000))
	url, _ := source["TranscodingUrl"].(string)
	if url == "" {
		t.Fatalf("12 Mbps does not fit under a 3 Mbps ceiling: %v", source)
	}
	if !reasons(source)["ContainerBitrateExceedsLimit"] {
		t.Errorf("reasons = %v", source["TranscodeReasons"])
	}
	// The picture is brought down to what the ceiling pays for rather than
	// spending the whole budget on a 1080p frame.
	if !strings.Contains(url, "MaxHeight=720") {
		t.Errorf("expected the ladder to pick 720p: %q", url)
	}
	if !strings.Contains(url, "VideoBitrate=3000000") {
		t.Errorf("the ceiling the client named is the one to encode at: %q", url)
	}
}

// The whole chain: the URL a client is given answers with a playlist, the
// playlist names a stream, and the segments it names are served.
func TestTheStreamAClientIsPointedAtIsThereWhenItAsks(t *testing.T) {
	server, token := converting(t)
	c := call{t: t, server: server, token: token}

	source := c.negotiated(c.film(), profile("mp4", "vp9", "aac", 0))
	master := source["TranscodingUrl"].(string) + "&api_key=" + token

	res, body := c.do(http.MethodGet, master, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("master playlist: %d %s", res.StatusCode, body)
	}
	if !strings.Contains(string(body), "#EXT-X-STREAM-INF") {
		t.Fatalf("master playlist = %s", body)
	}
	variant := nextURI(t, string(body))

	// Resolved the way a player resolves it: against the master's own path.
	res, body = c.do(http.MethodGet, resolve(master, variant), nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("variant playlist: %d %s", res.StatusCode, body)
	}
	playlist := string(body)
	if !strings.Contains(playlist, "/hls/") {
		t.Fatalf("the playlist should name where this server serves segments: %s", playlist)
	}
	// Every URI carries the token, because a player resolving a relative name
	// against the playlist's URL drops the query string that held it.
	for _, line := range strings.Split(playlist, "\n") {
		if strings.HasPrefix(line, "#EXT-X-MAP:") || (line != "" && !strings.HasPrefix(line, "#")) {
			if !strings.Contains(line, "api_key=") {
				t.Fatalf("a URI with no token is a 401 the player cannot explain: %q", line)
			}
		}
	}

	segment := nextURI(t, playlist)
	res, body = c.do(http.MethodGet, segment, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("segment %s: %d %s", segment, res.StatusCode, body)
	}
	if len(body) == 0 {
		t.Error("a segment with no bytes is not a segment")
	}
}

// A client that says it has stopped frees the encoder now, rather than at the
// idle deadline: an ffmpeg nobody is watching is the one real leak in this
// kind of software.
func TestAClientThatSaysItHasStoppedFreesTheEncoder(t *testing.T) {
	server, token := converting(t)
	c := call{t: t, server: server, token: token}

	source := c.negotiated(c.film(), profile("mp4", "vp9", "aac", 0))
	master := source["TranscodingUrl"].(string) + "&api_key=" + token
	_, body := c.do(http.MethodGet, master, nil)
	_, body = c.do(http.MethodGet, resolve(master, nextURI(t, string(body))), nil)
	segment := nextURI(t, string(body))

	play, _ := source["PlaySessionId"].(string)
	res, _ := c.do(http.MethodDelete, "/Videos/ActiveEncodings?PlaySessionId="+play+"&DeviceId=tablet", nil)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("stopping: %d", res.StatusCode)
	}

	res, _ = c.do(http.MethodGet, segment, nil)
	if res.StatusCode != http.StatusGone {
		t.Errorf("a stopped session is gone, not broken: %d", res.StatusCode)
	}
}

// A server with no ffmpeg has one answer, and it is a file rather than a
// spinner.
func TestAServerThatCannotConvertSaysSoByOfferingTheFile(t *testing.T) {
	server, token, _ := library(t)
	server.Prober = streamProber{}
	c := call{t: t, server: server, token: token}

	film := c.film()
	source := c.negotiated(film, profile("mp4", "vp9", "aac", 0))
	if source["SupportsDirectPlay"] != true || source["TranscodingUrl"] != nil {
		t.Errorf("with no ffmpeg the file is the only answer: %v", source)
	}
	res, _ := c.do(http.MethodGet, "/Videos/"+film+"/master.m3u8?VideoCodec=h264", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("a stream this server cannot make should not look available: %d", res.StatusCode)
	}
}

// Music is handed over as the file. The converter produces video segments,
// which is not what a client playing a track asked for.
func TestATrackIsHandedOverAsTheFile(t *testing.T) {
	server, token := converting(t)
	c := call{t: t, server: server, token: token}

	artists := c.items("/Artists")
	albums := c.items("/Users/" + c.me() + "/Items?ParentId=" + artists[0]["Id"].(string))
	tracks := c.items("/Users/" + c.me() + "/Items?ParentId=" + albums[0]["Id"].(string))
	source := c.negotiated(tracks[0]["Id"].(string), profile("mp4", "vp9", "aac", 100_000))
	if source["SupportsDirectPlay"] != true || source["TranscodingUrl"] != nil {
		t.Errorf("a track is the file: %v", source)
	}
}

// A client that sends no device profile is told about the file, which is what
// every client was told before this server could convert anything — and
// which costs no probe at all.
func TestAClientThatSaysNothingAboutItselfIsSentTheFile(t *testing.T) {
	server, token := converting(t)
	c := call{t: t, server: server, token: token}

	source := c.negotiated(c.film(), map[string]any{})
	if source["SupportsDirectPlay"] != true || source["TranscodingUrl"] != nil {
		t.Errorf("source = %v", source)
	}
}

/* ---------- helpers ---------- */

func reasons(source map[string]any) map[string]bool {
	out := map[string]bool{}
	list, _ := source["TranscodeReasons"].([]any)
	for _, entry := range list {
		if name, ok := entry.(string); ok {
			out[name] = true
		}
	}
	return out
}

// nextURI is the first URI a playlist names.
func nextURI(t *testing.T, playlist string) string {
	t.Helper()
	for _, line := range strings.Split(playlist, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line
	}
	t.Fatalf("playlist names nothing: %s", playlist)
	return ""
}

// resolve is what a player does with a relative URI in a playlist: keep the
// playlist's directory, drop its own name and query.
func resolve(base, uri string) string {
	if strings.HasPrefix(uri, "/") {
		return uri
	}
	path := base
	if cut := strings.Index(path, "?"); cut >= 0 {
		path = path[:cut]
	}
	if cut := strings.LastIndex(path, "/"); cut >= 0 {
		path = path[:cut+1]
	}
	return path + uri
}

// A client resuming asks for a stream that begins where it left off, and
// plays it from the top. So the playlist it gets has to begin there too: hand
// it the whole film and it asks for the first segment, which is not what the
// encoder was started on, and it waits on a spinner while the encoder is
// dragged back to the beginning.
func TestAStreamAskedForFromTheMiddleBeginsThere(t *testing.T) {
	server, token := converting(t)
	c := call{t: t, server: server, token: token}

	source := c.negotiated(c.film(), profile("mp4", "vp9", "aac", 0))
	// Two minutes in, in Jellyfin's hundred-nanosecond ticks.
	master := source["TranscodingUrl"].(string) + "&StartTimeTicks=1200000000&api_key=" + token

	_, body := c.do(http.MethodGet, master, nil)
	res, body := c.do(http.MethodGet, resolve(master, nextURI(t, string(body))), nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("variant playlist: %d %s", res.StatusCode, body)
	}
	first := nextURI(t, string(body))
	if strings.Contains(first, "seg00000") {
		t.Errorf("the playlist starts at the beginning of the film, not at the resume point: %q", first)
	}
	if !strings.Contains(first, "seg00030") {
		// Two minutes of four-second segments.
		t.Errorf("first segment = %q", first)
	}
}
