package admin_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// selected is the three films the bulk edits below act on.
func selected(t *testing.T, h *harness) (section int64, ids []int64) {
	t.Helper()
	section = films(t, h)
	items, _, err := h.db.SectionItems(context.Background(), section, domain.KindMovie, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	if len(ids) != 3 {
		t.Fatalf("want three films, have %d", len(ids))
	}
	return section, ids
}

func TestOneLabelOnManyItems(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	_, ids := selected(t, h)
	ctx := context.Background()

	// One of them already has a label, which must survive: adding is the
	// whole point of a bulk edit over a search result, and replacing would
	// lose it.
	if err := h.db.SetTags(ctx, store.TagLabel, ids[0], []string{"Keep"}); err != nil {
		t.Fatal(err)
	}

	res, body := h.call(http.MethodPost, "/admin/api/items/bulk", map[string]any{
		"items":     ids,
		"addLabels": []string{"Christmas"},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("bulk = %d: %v", res.StatusCode, body)
	}
	if int(body["touched"].(float64)) != 3 {
		t.Fatalf("touched = %v", body["touched"])
	}

	first, err := h.db.Tags(ctx, store.TagLabel, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(first, []string{"Christmas", "Keep"}) {
		t.Fatalf("the existing label was lost: %v", first)
	}
	for _, id := range ids[1:] {
		labels, err := h.db.Tags(ctx, store.TagLabel, id)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(labels, []string{"Christmas"}) {
			t.Fatalf("labels on %d = %v", id, labels)
		}
	}
}

func TestAWordInBothBoxesIsRemoved(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	_, ids := selected(t, h)
	ctx := context.Background()

	if err := h.db.SetTags(ctx, store.TagTag, ids[0], []string{"Sunday", "Winter"}); err != nil {
		t.Fatal(err)
	}

	// Asking to add and remove the same word is a removal. Predictable beats
	// clever: the alternative is a rule nobody could guess.
	res, body := h.call(http.MethodPost, "/admin/api/items/bulk", map[string]any{
		"items":      ids,
		"addTags":    []string{"Sunday"},
		"removeTags": []string{"Sunday"},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("bulk = %d: %v", res.StatusCode, body)
	}
	tags, err := h.db.Tags(ctx, store.TagTag, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(tags, []string{"Winter"}) {
		t.Fatalf("tags = %v", tags)
	}
}

func TestACertificateSetInBulkSetsTheAgeTheCeilingReads(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	_, ids := selected(t, h)
	ctx := context.Background()

	res, body := h.call(http.MethodPost, "/admin/api/items/bulk", map[string]any{
		"items":  ids,
		"rating": "15",
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("bulk = %d: %v", res.StatusCode, body)
	}

	for _, id := range ids {
		item, err := h.db.Item(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if item.Rating != "15" {
			t.Fatalf("rating = %q", item.Rating)
		}
		// Both halves, so the label a client shows and the age a
		// restriction compares against cannot disagree.
		if item.RatingAge != 15 {
			t.Fatalf("age = %d, want the 15 the label stands for", item.RatingAge)
		}
	}
}

func TestLockingManyItemsStopsTheMetadataPass(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	_, ids := selected(t, h)
	ctx := context.Background()

	res, body := h.call(http.MethodPost, "/admin/api/items/bulk", map[string]any{
		"items": ids, "locked": true,
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("bulk = %d: %v", res.StatusCode, body)
	}
	// The note says what happened in the words of what was asked.
	if note, _ := body["note"].(string); !strings.HasPrefix(note, "Locked") {
		t.Fatalf("note = %q", note)
	}

	waiting, err := h.db.NeedsMetadata(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range waiting {
		if slices.Contains(ids, item.ID) {
			t.Fatalf("%s was still offered to the metadata pass", item.Title)
		}
	}
}

func TestAnEmptyBulkEditIsRefusedRatherThanReported(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	_, ids := selected(t, h)

	for _, body := range []map[string]any{
		{"items": ids},
		{"items": []int64{}, "addLabels": []string{"Christmas"}},
		{"items": ids, "addLabels": []string{}},
	} {
		res, answer := h.call(http.MethodPost, "/admin/api/items/bulk", body)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("%v = %d: %v", body, res.StatusCode, answer)
		}
	}
}

func TestOneMissingItemFailsTheWholeBatch(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	_, ids := selected(t, h)
	ctx := context.Background()

	res, body := h.call(http.MethodPost, "/admin/api/items/bulk", map[string]any{
		"items":     append(append([]int64{}, ids...), 999999),
		"addLabels": []string{"Christmas"},
	})
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("a missing item = %d: %v", res.StatusCode, body)
	}

	// Nothing was changed, because half an applied bulk edit is worse than
	// none: nobody could tell where it stopped, and running it again would
	// double the additions on the items it already reached.
	for _, id := range ids {
		labels, err := h.db.Tags(ctx, store.TagLabel, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(labels) != 0 {
			t.Fatalf("%d was changed by a batch that failed: %v", id, labels)
		}
	}
}

func TestBulkEditingNeedsAnAdministrator(t *testing.T) {
	h := newHarness(t)
	_, ids := selected(t, h)

	res, _ := h.call(http.MethodPost, "/admin/api/items/bulk", map[string]any{
		"items": ids, "addLabels": []string{"Christmas"},
	})
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a stranger got %d", res.StatusCode)
	}
}

func TestAKindOfWordThisServerDoesNotKeepIsRefused(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	_, ids := selected(t, h)

	// The API names the kinds in its own fields, so an unknown one cannot be
	// sent through it — but the store refuses one anyway, because it is the
	// thing that would write the row.
	if _, err := h.db.BulkEditItems(context.Background(), ids, store.BulkEdit{
		AddWords: map[store.TagKind][]string{store.TagKind("composer"): {"Jóhannsson"}},
	}); err == nil {
		t.Fatal("a kind this server does not keep was accepted")
	}
}
