package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// Changing the same thing about many items at once.
//
// The one-at-a-time editor already exists and is the right tool for a wrong
// title. This is for the other job: a folder of forty films that all want
// the same label, a library whose certificates all came out blank, a set of
// items somebody wants to stop a metadata pass from touching. Doing that
// forty times through a page is the kind of work people write scripts to
// avoid, and then the script is the thing with no validation in it.
//
// Three decisions, which are what make this different from a loop over
// EditItem.
//
// Only the fields where "the same for all of them" is a sensible thing to
// want. A title is not one of them: forty items with one title is forty
// items nobody can tell apart, and the one-at-a-time editor is right there.
// So this takes the words, the studio, the certificate and the lock, and
// refuses to touch a title, a year or a summary.
//
// Words are added or removed, not replaced. The whole point of a bulk edit
// over a search result is "also label these Christmas", and replacing would
// mean every other label on every one of them was lost to a field somebody
// left empty. Replacing is still available one item at a time, where the
// page can show what is there now.
//
// One transaction for the whole batch. Half an applied bulk edit is worse
// than none: somebody would have no way of knowing where it stopped, and
// the obvious next move — run it again — would double the additions on the
// items it already reached.

// BulkEdit is what to change about every item in a set.
//
// Pointers and slices that are nil mean "leave this alone", the same
// arrangement [Edit] uses, because a form sends every field and most of them
// are untouched.
type BulkEdit struct {
	// AddWords and RemoveWords are words to attach to, or take off, every
	// item — keyed by kind. Add before remove, so asking for both with the
	// same word is a removal, which is at least predictable.
	AddWords    map[TagKind][]string
	RemoveWords map[TagKind][]string

	// Studio is who made them, where a provider got it wrong for a whole
	// folder. Empty string clears it, which is why this is a pointer.
	Studio *string

	// Rating is the certificate, for the libraries where a provider has none
	// and a household wants its own ceiling to work. Written as both the
	// label and the age, like [DB.SetRating].
	Rating *string

	// Locked stops, or allows, a metadata pass touching them.
	Locked *bool
}

// Empty reports whether this edit would do nothing, so a caller can refuse
// it rather than reporting that it changed many items.
func (b BulkEdit) Empty() bool {
	for _, words := range b.AddWords {
		if len(words) > 0 {
			return false
		}
	}
	for _, words := range b.RemoveWords {
		if len(words) > 0 {
			return false
		}
	}
	return b.Studio == nil && b.Rating == nil && b.Locked == nil
}

// maxBulkItems bounds one bulk edit.
//
// A whole library is a reasonable thing to want to label, and a whole
// library is also the thing most likely to be a mistake. The cap is high
// enough for any real selection and low enough that one transaction stays
// short; past it, a caller should be narrowing its search rather than
// widening its edit.
const maxBulkItems = 2000

// BulkEditItems applies the same change to every item in a set and reports
// how many rows it touched.
//
// One transaction, because half an applied bulk edit is worse than none.
func (d *DB) BulkEditItems(ctx context.Context, itemIDs []int64, edit BulkEdit) (int, error) {
	if len(itemIDs) == 0 {
		return 0, fmt.Errorf("nothing was selected")
	}
	if len(itemIDs) > maxBulkItems {
		return 0, fmt.Errorf("%d items at once is more than this will do — narrow the search", len(itemIDs))
	}
	if edit.Empty() {
		return 0, fmt.Errorf("nothing was asked for")
	}
	for kind := range edit.AddWords {
		if !kind.Valid() {
			return 0, fmt.Errorf("%q is not a kind of word this server keeps", kind)
		}
	}
	for kind := range edit.RemoveWords {
		if !kind.Valid() {
			return 0, fmt.Errorf("%q is not a kind of word this server keeps", kind)
		}
	}

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("bulk edit: %w", err)
	}
	defer tx.Rollback()

	now := millis(time.Now().UTC())
	touched := 0
	for _, itemID := range itemIDs {
		// Every item is checked rather than trusted: the ids came from a
		// page, and an id that is not there must not be silently counted as
		// changed.
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM items WHERE id = ?`, itemID).Scan(&exists); err != nil {
			return 0, fmt.Errorf("bulk edit: %w", err)
		}
		if exists == 0 {
			return 0, ErrNotFound
		}

		if err := bulkWords(ctx, tx, itemID, edit); err != nil {
			return 0, err
		}
		if err := bulkColumns(ctx, tx, itemID, edit, now); err != nil {
			return 0, err
		}
		touched++
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("bulk edit: %w", err)
	}
	return touched, nil
}

// bulkWords adds and removes words on one item.
//
// Read, change, write, rather than an insert and a delete: the existing list
// is needed anyway to tell an addition that is already there from one that
// is new, and replaceTagsTx is the one place that knows how a list is
// stored.
func bulkWords(ctx context.Context, tx execer, itemID int64, edit BulkEdit) error {
	for _, kind := range TagKinds() {
		adding, removing := edit.AddWords[kind], edit.RemoveWords[kind]
		if len(adding) == 0 && len(removing) == 0 {
			continue
		}

		rows, err := tx.QueryContext(ctx,
			`SELECT t.name FROM item_tags it JOIN tags t ON t.id = it.tag_id
			  WHERE it.item_id = ? AND t.kind = ? ORDER BY t.name`, itemID, string(kind))
		if err != nil {
			return fmt.Errorf("read %ss: %w", kind, err)
		}
		var held []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return fmt.Errorf("scan %s: %w", kind, err)
			}
			held = append(held, name)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		wanted := append([]string{}, held...)
		wanted = append(wanted, adding...)
		// Removal last, so asking to add and remove the same word is a
		// removal. Predictable beats clever: the alternative is a rule
		// nobody could guess.
		if len(removing) > 0 {
			drop := make(map[string]struct{}, len(removing))
			for _, name := range removing {
				drop[strings.ToLower(strings.Join(strings.Fields(name), " "))] = struct{}{}
			}
			kept := wanted[:0]
			for _, name := range wanted {
				if _, found := drop[strings.ToLower(strings.Join(strings.Fields(name), " "))]; found {
					continue
				}
				kept = append(kept, name)
			}
			wanted = kept
		}

		if err := replaceTagsTx(ctx, tx, kind, itemID, wanted); err != nil {
			return err
		}
	}
	return nil
}

// bulkColumns writes the plain fields.
func bulkColumns(ctx context.Context, tx execer, itemID int64, edit BulkEdit, now int64) error {
	sets := []string{}
	args := []any{}
	if edit.Studio != nil {
		sets = append(sets, "studio = ?")
		args = append(args, strings.Join(strings.Fields(*edit.Studio), " "))
	}
	if edit.Rating != nil {
		// Both halves, like the one-at-a-time rating: the label a client
		// shows and the age a restriction compares against, which must not
		// be able to disagree.
		label := strings.TrimSpace(*edit.Rating)
		sets = append(sets, "rating = ?", "rating_age = ?")
		args = append(args, label, domain.AgeOfRating(label))
	}
	if edit.Locked != nil {
		sets = append(sets, "locked = ?")
		args = append(args, boolToInt(*edit.Locked))
	}
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, now, itemID)

	if _, err := tx.ExecContext(ctx,
		`UPDATE items SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		return fmt.Errorf("bulk edit: %w", err)
	}
	return nil
}
