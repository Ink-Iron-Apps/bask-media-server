package store_test

import (
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

func TestALibraryWithNoSettingsBehavesAsItAlwaysDid(t *testing.T) {
	s := newShelf(t)

	section, err := s.db.Section(s.ctx, s.films)
	if err != nil {
		t.Fatal(err)
	}
	// Every default, which is the behaviour before these existed: enriched,
	// markers looked for, and never scanned without being asked.
	if !section.Settings.EnrichWanted() {
		t.Fatal("enrichment was off by default")
	}
	if !section.Settings.MarkersWanted() {
		t.Fatal("marker detection was off by default")
	}
	if section.Settings.ScanInterval() != 0 {
		t.Fatalf("a library scanned itself by default, every %v", section.Settings.ScanInterval())
	}
	if !section.Settings.PeopleWanted(domain.SectionMovies) {
		t.Fatal("people were off by default for a films library")
	}
	// Never for music or photographs, whatever the setting says: a film
	// catalogue has nothing to say about an album, and asking would spend a
	// request to find that out.
	if section.Settings.PeopleWanted(domain.SectionMusic) {
		t.Fatal("people were wanted for a music library")
	}
}

func TestSettingsRoundTripThroughTheColumn(t *testing.T) {
	s := newShelf(t)
	off := false

	want := domain.SectionSettings{ScanEvery: 360, Enrich: &off, Markers: &off}
	if err := s.db.SetSectionSettings(s.ctx, s.films, want); err != nil {
		t.Fatal(err)
	}

	section, err := s.db.Section(s.ctx, s.films)
	if err != nil {
		t.Fatal(err)
	}
	if section.Settings.ScanEvery != 360 {
		t.Fatalf("scanEvery = %d", section.Settings.ScanEvery)
	}
	if section.Settings.EnrichWanted() || section.Settings.MarkersWanted() {
		t.Fatalf("settings = %+v, want both off", section.Settings)
	}
	if section.Settings.ScanInterval() != 6*time.Hour {
		t.Fatalf("interval = %v", section.Settings.ScanInterval())
	}

	// And back to the defaults, which stores nothing rather than storing a
	// description of nothing.
	if err := s.db.SetSectionSettings(s.ctx, s.films, domain.SectionSettings{}); err != nil {
		t.Fatal(err)
	}
	section, err = s.db.Section(s.ctx, s.films)
	if err != nil {
		t.Fatal(err)
	}
	if !section.Settings.EnrichWanted() || section.Settings.ScanEvery != 0 {
		t.Fatalf("settings = %+v, want the defaults", section.Settings)
	}
}

func TestAnIntervalTooShortIsRefusedWithAReason(t *testing.T) {
	s := newShelf(t)

	for _, every := range []int{1, 5, 14, -1, 8 * 24 * 60} {
		err := s.db.SetSectionSettings(s.ctx, s.films, domain.SectionSettings{ScanEvery: every})
		if err == nil {
			t.Fatalf("an interval of %d minutes was accepted", every)
		}
	}
	// The floor itself is allowed.
	if err := s.db.SetSectionSettings(s.ctx, s.films, domain.SectionSettings{ScanEvery: 15}); err != nil {
		t.Fatalf("fifteen minutes was refused: %v", err)
	}
}

func TestOnlyLibrariesWithAnIntervalAreEverDue(t *testing.T) {
	s := newShelf(t)
	now := time.Now().UTC()

	// Nothing is due on a server where nobody asked for a schedule.
	due, err := s.db.DueForScan(s.ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("due = %v on a server with no schedules", due)
	}

	if err := s.db.SetSectionSettings(s.ctx, s.films, domain.SectionSettings{ScanEvery: 60}); err != nil {
		t.Fatal(err)
	}
	// Never swept, so due immediately — which is what somebody who has just
	// turned this on expects to see.
	due, err = s.db.DueForScan(s.ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != s.films {
		t.Fatalf("due = %v", due)
	}

	// Looked at, so not due again until the interval has passed.
	if err := s.db.MarkSwept(s.ctx, s.films, now); err != nil {
		t.Fatal(err)
	}
	due, err = s.db.DueForScan(s.ctx, now.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("due after half an interval = %v", due)
	}
	due, err = s.db.DueForScan(s.ctx, now.Add(61*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("due after the interval = %v", due)
	}
}

func TestEnrichmentSkipsALibraryThatAskedToBeLeftAlone(t *testing.T) {
	s := newShelf(t)

	// Both libraries' items are waiting to be matched.
	waiting, err := s.db.NeedsMetadata(s.ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting) == 0 {
		t.Fatal("nothing was waiting to be enriched")
	}

	off := false
	if err := s.db.SetSectionSettings(s.ctx, s.films, domain.SectionSettings{Enrich: &off}); err != nil {
		t.Fatal(err)
	}
	waiting, err = s.db.NeedsMetadata(s.ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range waiting {
		if item.SectionID == s.films {
			t.Fatalf("%s is in a library that asked not to be enriched", item.Title)
		}
	}

	// Every library turned off means nothing to do, not everything to do:
	// an empty allow-list must not read as no filter at all.
	if err := s.db.SetSectionSettings(s.ctx, s.music, domain.SectionSettings{Enrich: &off}); err != nil {
		t.Fatal(err)
	}
	waiting, err = s.db.NeedsMetadata(s.ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting) != 0 {
		t.Fatalf("%d items were offered after every library turned enrichment off", len(waiting))
	}
}

func TestMarkerDetectionSkipsALibraryThatAskedToBeLeftAlone(t *testing.T) {
	s := newShelf(t)
	off := false
	if err := s.db.SetSectionSettings(s.ctx, s.films, domain.SectionSettings{Markers: &off}); err != nil {
		t.Fatal(err)
	}

	seasons, err := s.db.SeasonsNeedingMarkers(s.ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, season := range seasons {
		if season.SectionID == s.films {
			t.Fatal("a season in a library with detection off was offered")
		}
	}
}
