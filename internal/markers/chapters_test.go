package markers_test

import (
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/markers"
)

func chapter(title string, start, end time.Duration) domain.Chapter {
	return domain.Chapter{ItemID: 7, Title: title, Start: start, End: end}
}

func TestFromChaptersReadsTitles(t *testing.T) {
	found := markers.FromChapters([]domain.Chapter{
		chapter("Intro", 0, 90*time.Second),
		chapter("Chapter 2", 90*time.Second, 10*time.Minute),
		chapter("End Credits", 20*time.Minute, 22*time.Minute),
	})

	if len(found) != 2 {
		t.Fatalf("want an intro and a credits marker, got %d: %+v", len(found), found)
	}
	if found[0].Type != domain.MarkerIntro || found[0].End != 90*time.Second {
		t.Errorf("intro marker wrong: %+v", found[0])
	}
	if found[1].Type != domain.MarkerCredits || found[1].Start != 20*time.Minute {
		t.Errorf("credits marker wrong: %+v", found[1])
	}
	for _, m := range found {
		if m.ItemID != 7 {
			t.Errorf("marker lost its item: %+v", m)
		}
		if m.Source != domain.SourceChapter {
			t.Errorf("marker should say where it came from: %+v", m)
		}
	}
}

// The patterns Bask applies client-side, so a library that produced a button
// there produces one here.
func TestFromChaptersRecognisesTheUsualNames(t *testing.T) {
	cases := map[string]domain.MarkerType{
		"Intro":           domain.MarkerIntro,
		"intro":           domain.MarkerIntro,
		"Introduction":    domain.MarkerIntro,
		"Opening":         domain.MarkerIntro,
		"OP":              domain.MarkerIntro,
		"OP 1":            domain.MarkerIntro,
		"Credits":         domain.MarkerCredits,
		"End Credits":     domain.MarkerCredits,
		"Closing Credits": domain.MarkerCredits,
		"Outro":           domain.MarkerCredits,
		"Ending":          domain.MarkerCredits,
		"ED":              domain.MarkerCredits,
	}
	for title, want := range cases {
		found := markers.FromChapters([]domain.Chapter{chapter(title, 0, 60*time.Second)})
		if len(found) != 1 {
			t.Errorf("%q produced no marker", title)
			continue
		}
		if found[0].Type != want {
			t.Errorf("%q classified as %s, want %s", title, found[0].Type, want)
		}
	}

	for _, title := range []string{"", "Chapter 1", "Part 2", "The Opening Ceremony Heist", "Scene 4"} {
		if found := markers.FromChapters([]domain.Chapter{chapter(title, 0, 60*time.Second)}); len(found) != 0 {
			// "The Opening Ceremony Heist" is the interesting one: a word
			// boundary match on "opening" inside a real chapter title would
			// put a Skip Intro button over the film.
			t.Errorf("%q should not be a marker, got %+v", title, found)
		}
	}
}

// A movie's first chapter named "Opening Credits" is ten minutes of film, and
// skipping it skips the opening of the film.
func TestFromChaptersDropsAnImplausiblyLongIntro(t *testing.T) {
	long := markers.FromChapters([]domain.Chapter{chapter("Opening", 0, markers.MaxChapterIntro+time.Second)})
	if len(long) != 0 {
		t.Errorf("a %v intro should be dropped, got %+v", markers.MaxChapterIntro+time.Second, long)
	}

	atTheLimit := markers.FromChapters([]domain.Chapter{chapter("Opening", 0, markers.MaxChapterIntro)})
	if len(atTheLimit) != 1 {
		t.Errorf("an intro at the limit should be kept, got %+v", atTheLimit)
	}

	// Credits are legitimately long, so the cap must not apply to them.
	credits := markers.FromChapters([]domain.Chapter{chapter("End Credits", time.Hour, time.Hour+12*time.Minute)})
	if len(credits) != 1 {
		t.Errorf("long credits should be kept, got %+v", credits)
	}
}

func TestFromChaptersIgnoresUnusableBounds(t *testing.T) {
	found := markers.FromChapters([]domain.Chapter{
		chapter("Intro", 60*time.Second, 60*time.Second),
		chapter("Credits", 90*time.Second, 30*time.Second),
	})
	if len(found) != 0 {
		t.Errorf("a marker with no length is a button that skips nowhere: %+v", found)
	}
}
