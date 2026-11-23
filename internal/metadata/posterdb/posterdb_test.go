package posterdb_test

import (
	"testing"

	"github.com/inknironapps/bask-media-server/internal/metadata/posterdb"
)

func TestImageAcceptsTheShapesAPersonHas(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://theposterdb.com/poster/123456", "https://theposterdb.com/api/assets/123456"},
		{"https://theposterdb.com/api/assets/123456", "https://theposterdb.com/api/assets/123456"},
		{"http://www.theposterdb.com/poster/9/", "https://theposterdb.com/api/assets/9"},
		{"  123456  ", "https://theposterdb.com/api/assets/123456"},
	} {
		got, err := posterdb.Image(tc.in)
		if err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("%q became %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestImageRefusesWhatItCannotTurnIntoOnePicture(t *testing.T) {
	for _, in := range []string{
		"", "https://theposterdb.com/", "https://theposterdb.com/user/someone",
		"https://example.com/poster/1", "https://theposterdb.com/poster/not-a-number",
	} {
		if got, err := posterdb.Image(in); err == nil {
			t.Fatalf("%q was accepted as %q", in, got)
		}
	}
}

func TestASetSaysWhyItIsRefused(t *testing.T) {
	_, err := posterdb.Image("https://theposterdb.com/set/4242")
	if err == nil {
		t.Fatal("a set was accepted")
	}
	// The message matters: a set is the most likely thing to be pasted, and
	// "not a poster" would not tell somebody what to do instead.
	if err == posterdb.ErrNotAPoster {
		t.Fatalf("a set got the generic refusal: %v", err)
	}
}

func TestMineTellsThisSiteFromAnother(t *testing.T) {
	if !posterdb.Mine("https://theposterdb.com/poster/1") {
		t.Fatal("its own address was not recognised")
	}
	if posterdb.Mine("https://image.tmdb.org/t/p/w500/x.jpg") {
		t.Fatal("another site's address was claimed")
	}
}
