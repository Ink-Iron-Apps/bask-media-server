package admin_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// films seeds a library of three, with the lengths and years the rules below
// ask about.
func films(t *testing.T, h *harness) int64 {
	t.Helper()
	ctx := context.Background()
	section, err := h.db.CreateSection(ctx, "Films", domain.SectionMovies, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	writer := h.db.NewWriter(5)
	add := func(title string, year, minutes int) {
		t.Helper()
		if err := writer.Add(ctx, store.Found{
			SectionID: section.ID, Kind: domain.KindMovie, Title: title, Year: year,
			Duration: time.Duration(minutes) * time.Minute,
			Path:     t.TempDir() + "/" + title + ".mkv", Container: "mkv", Size: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	add("Big Hill", 2026, 120)
	add("Heat Haze", 1998, 95)
	add("Short Work", 2010, 14)
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return section.ID
}

// The whole flow through the interface: make a collection, see what rules
// would match before committing to them, save them, and read back a
// collection that fills itself.
func TestACollectionIsGivenRulesAndFillsItself(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	films(t, h)

	res, made := h.call(http.MethodPost, "/admin/api/collections", map[string]any{"title": "Long films"})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d: %v", res.StatusCode, made)
	}
	id := strconv.Itoa(int(made["id"].(float64)))

	rules := map[string]any{
		"match": "all",
		"rules": []any{map[string]any{"field": "duration", "op": "atleast", "value": "60"}},
		"sort":  "year",
		"desc":  true,
	}

	// The preview answers the question without storing it, which is what
	// makes the editor usable: nothing is committed to see what it does.
	res, preview := h.call(http.MethodPost, "/admin/api/collections/preview", rules)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("preview = %d: %v", res.StatusCode, preview)
	}
	if int(preview["held"].(float64)) != 2 {
		t.Fatalf("preview held %v", preview["held"])
	}
	shown := titlesOf(t, preview)
	if len(shown) != 2 || shown[0] != "Big Hill" || shown[1] != "Heat Haze" {
		t.Fatalf("preview showed %v, wanted the newest first", shown)
	}
	// Nothing was stored by asking.
	for _, c := range h.list("/admin/api/collections") {
		if c["rules"] != nil {
			t.Fatalf("a preview stored rules: %v", c)
		}
	}

	if res, body := h.call(http.MethodPatch, "/admin/api/collections/"+id,
		map[string]any{"rules": rules}); res.StatusCode != http.StatusNoContent {
		t.Fatalf("save rules = %d: %v", res.StatusCode, body)
	}

	_, read := h.call(http.MethodGet, "/admin/api/collections/"+id, nil)
	if int(read["held"].(float64)) != 2 {
		t.Fatalf("held %v", read["held"])
	}
	if got := titlesOf(t, read); len(got) != 2 || got[0] != "Big Hill" {
		t.Fatalf("the saved collection holds %v", got)
	}
	stored, ok := read["rules"].(map[string]any)
	if !ok || stored["sort"] != "year" || stored["desc"] != true {
		t.Fatalf("rules read back as %v", read["rules"])
	}

	// A film that arrives later is in it without anybody touching the
	// collection, which is the entire reason for the feature.
	ctx := context.Background()
	sections, err := h.db.Sections(ctx)
	if err != nil || len(sections) == 0 {
		t.Fatalf("no library: %v %v", sections, err)
	}
	writer := h.db.NewWriter(1)
	if err := writer.Add(ctx, store.Found{
		SectionID: sections[0].ID, Kind: domain.KindMovie, Title: "Late Arrival", Year: 2027,
		Duration: 140 * time.Minute,
		Path:     t.TempDir() + "/Late Arrival.mkv", Container: "mkv", Size: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	_, again := h.call(http.MethodGet, "/admin/api/collections/"+id, nil)
	if got := titlesOf(t, again); len(got) != 3 || got[0] != "Late Arrival" {
		t.Fatalf("a new film did not join a collection it answers: %v", got)
	}
}

// titlesOf reads the item titles out of a collection view.
func titlesOf(t *testing.T, view map[string]any) []string {
	t.Helper()
	items, _ := view["items"].([]any)
	out := make([]string, 0, len(items))
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("an item came back as %T", raw)
		}
		out = append(out, item["title"].(string))
	}
	return out
}

// A collection filled by rules is filled by rules: a request to put something
// in it by hand is refused rather than silently stored where nothing reads it.
func TestAHandPickedListIsRefusedOnARuleDrivenCollection(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	films(t, h)

	_, made := h.call(http.MethodPost, "/admin/api/collections", map[string]any{"title": "Everything"})
	id := strconv.Itoa(int(made["id"].(float64)))
	h.call(http.MethodPatch, "/admin/api/collections/"+id, map[string]any{
		"rules": map[string]any{"rules": []any{}},
	})

	item := firstItem(t, h)
	res, body := h.call(http.MethodPatch, "/admin/api/collections/"+id,
		map[string]any{"items": []int64{item}})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("hand-filling a smart collection = %d: %v", res.StatusCode, body)
	}
	if message, _ := body["error"].(string); !strings.Contains(message, "rules") {
		t.Fatalf("error = %q", message)
	}

	// Taking the rules away makes it an ordinary list again, in one request
	// with the list itself.
	if res, body := h.call(http.MethodPatch, "/admin/api/collections/"+id, map[string]any{
		"rules": nil, "items": []int64{item},
	}); res.StatusCode != http.StatusNoContent {
		t.Fatalf("back to hand-picked = %d: %v", res.StatusCode, body)
	}
	_, read := h.call(http.MethodGet, "/admin/api/collections/"+id, nil)
	if read["rules"] != nil {
		t.Fatalf("rules survived = %v", read["rules"])
	}
	if got := titlesOf(t, read); len(got) != 1 {
		t.Fatalf("hand-picked again holds %v", got)
	}
}

// A rule set the server cannot answer is refused with the reason, and the
// same reason comes back from the preview — so the editor can show one
// message for both.
func TestRulesTheServerCannotAnswerAreRefusedWithTheReason(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	films(t, h)

	_, made := h.call(http.MethodPost, "/admin/api/collections", map[string]any{"title": "Nonsense"})
	id := strconv.Itoa(int(made["id"].(float64)))

	// Composer is a real credit on a real film and a field this server has
	// no rule for, which is the case worth refusing: a rule nobody can
	// answer must not be stored as one that matches nothing.
	bad := map[string]any{"rules": []any{
		map[string]any{"field": "composer", "op": "is", "value": "Jóhannsson"},
	}}
	res, body := h.call(http.MethodPatch, "/admin/api/collections/"+id, map[string]any{"rules": bad})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown field = %d: %v", res.StatusCode, body)
	}
	if message, _ := body["error"].(string); !strings.Contains(message, "composer") {
		t.Fatalf("error = %q", message)
	}

	res, body = h.call(http.MethodPost, "/admin/api/collections/preview", bad)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("previewing an unknown field = %d: %v", res.StatusCode, body)
	}

	// And the collection is still the hand-picked one it was made as.
	_, read := h.call(http.MethodGet, "/admin/api/collections/"+id, nil)
	if read["rules"] != nil {
		t.Fatalf("a refused rule set was stored: %v", read["rules"])
	}
}

// The editor builds its menus from the server, so a field the server knows
// about is a field somebody can use.
func TestTheEditorIsToldWhatItMayAsk(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)

	res, body := h.call(http.MethodGet, "/admin/api/collections/fields", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("fields = %d: %v", res.StatusCode, body)
	}
	fields, _ := body["fields"].([]any)
	if len(fields) < 10 {
		t.Fatalf("only %d fields", len(fields))
	}
	seen := map[string]map[string]any{}
	for _, raw := range fields {
		field := raw.(map[string]any)
		seen[field["field"].(string)] = field
	}
	watched, ok := seen["watched"]
	if !ok {
		t.Fatalf("no watched field in %v", seen)
	}
	if watched["value"] != "none" {
		t.Fatalf("watched takes %v", watched["value"])
	}
	if ops, _ := watched["ops"].([]any); len(ops) != 3 {
		t.Fatalf("watched ops = %v", watched["ops"])
	}
	if kinds, ok := seen["kind"]; !ok || len(kinds["choices"].([]any)) == 0 {
		t.Fatalf("the kind field offers no choices: %v", kinds)
	}
	if sorts, _ := body["sorts"].([]any); len(sorts) == 0 {
		t.Fatalf("no orders offered")
	}
}

// Watch rules are answered for whoever is asking, and in this interface that
// is the signed-in administrator — not "nobody", which would show every film
// as unwatched and make the preview a lie.
func TestThePreviewAnswersWatchRulesForTheSignedInAdministrator(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	films(t, h)
	ctx := context.Background()

	item := firstItem(t, h)
	if err := h.db.SaveProgress(ctx, domain.Progress{
		UserID: h.adminID, ItemID: item, Watched: true,
		Offset: 120 * time.Minute, Duration: 120 * time.Minute,
	}); err != nil {
		t.Fatal(err)
	}

	_, preview := h.call(http.MethodPost, "/admin/api/collections/preview", map[string]any{
		"rules": []any{map[string]any{"field": "watched", "op": "no"}},
	})
	for _, title := range titlesOf(t, preview) {
		if title == "Big Hill" {
			t.Fatal("a film this administrator has watched was shown as unwatched")
		}
	}
	if int(preview["held"].(float64)) != 2 {
		t.Fatalf("held %v of three films, one of them watched", preview["held"])
	}
}

// firstItem is the film the fixture puts first by title.
func firstItem(t *testing.T, h *harness) int64 {
	t.Helper()
	found, _, err := h.db.Search(context.Background(), "Big Hill", store.Page{Size: 1})
	if err != nil || len(found) == 0 {
		t.Fatalf("seeding failed: %v %v", found, err)
	}
	return found[0].ID
}
