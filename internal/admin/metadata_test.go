package admin_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/metadata"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// provider is a metadata provider with two films in it, one of which is the
// wrong one. Both Provider and Matcher, which is what the real one is.
type provider struct{ calls int }

func (p *provider) Name() string { return "test" }

// Movie always answers with the wrong film, which is the situation this
// feature exists for: a provider that is confident and wrong.
func (p *provider) Movie(context.Context, string, int) (metadata.Match, error) {
	return metadata.Match{ProviderID: "666", Title: "Blue Steel", Year: 1989, Summary: "Not it."}, nil
}

func (p *provider) Show(context.Context, string, int) (metadata.Match, error) {
	return metadata.Match{ProviderID: "900", Title: "The Wrong Series", Year: 2001}, nil
}

func (p *provider) Episode(context.Context, string, int, int) (metadata.Match, error) {
	return metadata.Match{}, metadata.ErrNoMatch
}

func (p *provider) MovieCandidates(_ context.Context, title string, _ int) ([]metadata.Match, error) {
	p.calls++
	return []metadata.Match{
		{ProviderID: "666", Title: "Blue Steel", Year: 1989, Summary: "Not it."},
		{ProviderID: "108", Title: "Three Colours: Blue", Year: 1993, Summary: "It.", PosterURL: "https://example.test/p.jpg"},
	}, nil
}

func (p *provider) ShowCandidates(context.Context, string, int) ([]metadata.Match, error) {
	return []metadata.Match{{ProviderID: "777", Title: "The Right Series", Year: 1999}}, nil
}

func (p *provider) MovieByID(_ context.Context, id string) (metadata.Match, error) {
	switch id {
	case "108":
		return metadata.Match{
			ProviderID: "108", Title: "Three Colours: Blue", Year: 1993, Summary: "It.",
			Rating: "R",
		}, nil
	case "666":
		return metadata.Match{ProviderID: "666", Title: "Blue Steel", Year: 1989, Summary: "Not it."}, nil
	}
	return metadata.Match{}, metadata.ErrNoMatch
}

func (p *provider) ShowByID(_ context.Context, id string) (metadata.Match, error) {
	if id == "777" {
		return metadata.Match{ProviderID: "777", Title: "The Right Series", Year: 1999}, nil
	}
	return metadata.Match{}, metadata.ErrNoMatch
}

// film is one film in one library, as a scan would leave it.
func film(t *testing.T, h *harness, scannedTitle string, year int) int64 {
	t.Helper()
	ctx := context.Background()
	section, err := h.db.CreateSection(ctx, "Films", domain.SectionMovies, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), scannedTitle+".mkv")
	if err := os.WriteFile(path, []byte("film"), 0o600); err != nil {
		t.Fatal(err)
	}
	writer := h.db.NewWriter(1)
	if err := writer.Add(ctx, store.Found{
		SectionID: section.ID, Kind: domain.KindMovie, Title: scannedTitle, Year: year,
		Path: path, Container: "mkv", Size: 4,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	items, _, err := h.db.SectionItems(ctx, section.ID, domain.KindMovie, store.Page{Size: 1})
	if err != nil || len(items) != 1 {
		t.Fatalf("seeding failed: %v %v", items, err)
	}
	return items[0].ID
}

// A title typed by hand is kept, and the item is locked so the next metadata
// pass leaves it alone.
func TestATitleIsCorrectedByHand(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	id := film(t, h, "blue.1993.1080p", 1993)
	path := "/admin/api/items/" + strconv.FormatInt(id, 10)

	_, before := h.call(http.MethodGet, path, nil)
	if before["title"] != "blue.1993.1080p" {
		t.Fatalf("the editor opened on %v", before["title"])
	}
	if before["locked"] != false {
		t.Fatalf("a freshly scanned film is locked: %v", before)
	}
	// The file is named in the editor, because the first thing anybody checks
	// about a wrong match is which file it is.
	if files, _ := before["files"].([]any); len(files) != 1 {
		t.Fatalf("the editor did not say which file it is: %v", before["files"])
	}

	if res, body := h.call(http.MethodPatch, path, map[string]any{
		"title": "Three Colours: Blue", "year": 1993, "summary": "A widow in Paris.",
	}); res.StatusCode != http.StatusNoContent {
		t.Fatalf("edit = %d: %v", res.StatusCode, body)
	}

	_, after := h.call(http.MethodGet, path, nil)
	if after["title"] != "Three Colours: Blue" || after["summary"] != "A widow in Paris." {
		t.Fatalf("the correction did not stick: %v", after)
	}
	if after["locked"] != true {
		t.Fatal("a corrected item was not locked")
	}
	// And the filename is still shown, because that is what explains why a
	// rescan does not undo this.
	if after["scannedTitle"] != "blue.1993.1080p" {
		t.Fatalf("scanned title = %v", after["scannedTitle"])
	}

	// A client sees the corrected title, which is the whole point.
	item, err := h.db.Item(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if item.Title != "Three Colours: Blue" {
		t.Fatalf("the library still says %q", item.Title)
	}
}

// Reverting puts the filename's own title back and unlocks the item.
func TestACorrectionCanBeUndone(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	id := film(t, h, "blue.1993.1080p", 1993)
	path := "/admin/api/items/" + strconv.FormatInt(id, 10)

	h.call(http.MethodPatch, path, map[string]any{"title": "Blue Steel"})
	if res, body := h.call(http.MethodPost, path+"/revert", nil); res.StatusCode != http.StatusNoContent {
		t.Fatalf("revert = %d: %v", res.StatusCode, body)
	}

	_, after := h.call(http.MethodGet, path, nil)
	if after["title"] != "blue.1993.1080p" {
		t.Fatalf("title = %v", after["title"])
	}
	if after["locked"] != false {
		t.Fatal("reverting left the item locked, so no pass would ever fix it")
	}
}

// A title that is only spaces is refused: a row with no name cannot be found
// again from any interface that lists it.
func TestAnItemCannotBeRenamedToNothing(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	id := film(t, h, "blue.1993", 1993)

	res, _ := h.call(http.MethodPatch, "/admin/api/items/"+strconv.FormatInt(id, 10), map[string]any{"title": "   "})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("renaming to nothing = %d, want 400", res.StatusCode)
	}
}

// The wrong match, corrected: the provider's other entries are offered and
// the one somebody picks replaces the summary and the artwork.
func TestAWrongMatchIsFixedByPickingTheRightOne(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	fake := &provider{}
	h.runner.Provider = func(context.Context) (metadata.Provider, error) { return fake, nil }

	id := film(t, h, "Blue", 1993)
	path := "/admin/api/items/" + strconv.FormatInt(id, 10)
	ctx := context.Background()

	// The wrong match, as an enrichment pass would have left it.
	if err := h.db.SaveMetadata(ctx, id, "666", "Not it.", "poster-wrong", "art-wrong"); err != nil {
		t.Fatal(err)
	}

	_, detail := h.call(http.MethodGet, path, nil)
	if detail["canRematch"] != true {
		t.Fatalf("the editor would not offer a rematch: %v", detail)
	}

	_, found := h.call(http.MethodGet, path+"/candidates?q=Three+Colours", nil)
	candidates, _ := found["candidates"].([]any)
	if len(candidates) != 2 {
		t.Fatalf("candidates = %v", candidates)
	}
	first, _ := candidates[0].(map[string]any)
	if first["current"] != true {
		t.Fatalf("the entry it is matched to now is not marked as current: %v", first)
	}

	_, matched := h.call(http.MethodPost, path+"/match", map[string]any{"providerId": "108"})
	if matched["title"] != "Three Colours: Blue" {
		t.Fatalf("match = %v", matched)
	}

	item, err := h.db.Item(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if item.ProviderID != "108" {
		t.Fatalf("provider id = %q", item.ProviderID)
	}
	if item.Summary != "It." {
		t.Fatalf("summary = %q", item.Summary)
	}
	// The rating comes with the match: a film re-matched to a children's
	// title that is still rated 18 is the mistake this avoids, and the other
	// way round matters more.
	if item.Rating != "R" || item.RatingAge != 17 {
		t.Fatalf("rating = %q (%d)", item.Rating, item.RatingAge)
	}
	// The old artwork is gone rather than left beside the new summary: half
	// of one match and half of another is worse than an empty poster.
	if item.PosterKey == "poster-wrong" || item.ArtKey == "art-wrong" {
		t.Fatalf("the old artwork survived the correction: %+v", item)
	}
}

// Correcting a series unmatches its episodes, because an episode is addressed
// through its show's provider id and was therefore wrong too.
func TestCorrectingASeriesUnmatchesItsEpisodes(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	fake := &provider{}
	h.runner.Provider = func(context.Context) (metadata.Provider, error) { return fake, nil }
	ctx := context.Background()

	section, err := h.db.CreateSection(ctx, "TV", domain.SectionShows, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	writer := h.db.NewWriter(1)
	if err := writer.Add(ctx, store.Found{
		SectionID: section.ID, Kind: domain.KindEpisode,
		Show: "Wrong", SeasonNum: 1, EpisodeNum: 1, Title: "Pilot",
		Path: filepath.Join(t.TempDir(), "s01e01.mkv"), Container: "mkv", Size: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	shows, _, err := h.db.SectionItems(ctx, section.ID, domain.KindShow, store.Page{Size: 1})
	if err != nil || len(shows) != 1 {
		t.Fatalf("seeding failed: %v %v", shows, err)
	}
	for _, item := range mustNeedMetadata(t, h) {
		if err := h.db.SaveMetadata(ctx, item.ID, "900", "The wrong series.", "", ""); err != nil {
			t.Fatal(err)
		}
	}

	path := "/admin/api/items/" + strconv.FormatInt(shows[0].ID, 10)
	_, matched := h.call(http.MethodPost, path+"/match", map[string]any{"providerId": "777"})
	if matched["title"] != "The Right Series" {
		t.Fatalf("match = %v", matched)
	}
	if note, _ := matched["note"].(string); note == "" {
		t.Fatal("nothing told the operator that the episodes need fetching again")
	}

	// The series itself is matched — that is what just happened — and
	// everything under it is waiting to be fetched through the new id.
	show, err := h.db.Item(ctx, shows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if show.ProviderID != "777" {
		t.Fatalf("the series still points at %q", show.ProviderID)
	}
	waiting := mustNeedMetadata(t, h)
	if len(waiting) != 2 {
		t.Fatalf("expected the season and the episode to be unmatched, got %v", waiting)
	}
	for _, item := range waiting {
		if item.ProviderID != "" || item.Summary != "" {
			t.Fatalf("%s kept what the wrong series gave it: %+v", item.Kind, item)
		}
	}
}

// A provider that cannot be asked for a list, or a server with no key at
// all, offers the fields and says why the rest is missing: a button that
// fails is worse than a sentence.
func TestWithNoProviderTheFieldsStillWork(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	id := film(t, h, "blue.1993", 1993)
	path := "/admin/api/items/" + strconv.FormatInt(id, 10)

	_, detail := h.call(http.MethodGet, path, nil)
	if detail["canRematch"] != false {
		t.Fatalf("a server with no TMDB key offered a rematch: %v", detail)
	}
	if why, _ := detail["why"].(string); why == "" {
		t.Fatal("it did not say why")
	}
	res, _ := h.call(http.MethodGet, path+"/candidates", nil)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("asking anyway = %d, want 409", res.StatusCode)
	}

	// And the hand-typed correction still works, which is the half of this
	// that needs no provider at all.
	if res, _ := h.call(http.MethodPatch, path, map[string]any{"title": "Three Colours: Blue"}); res.StatusCode != http.StatusNoContent {
		t.Fatalf("editing without a provider = %d", res.StatusCode)
	}
}

func mustNeedMetadata(t *testing.T, h *harness) []domain.Item {
	t.Helper()
	items, err := h.db.NeedsMetadata(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	return items
}
