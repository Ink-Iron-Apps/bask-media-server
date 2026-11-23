package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// Who made a thing and who is in it.
//
// Close to the tags in this package but deliberately not the same table. A
// person has a part as well as a name — "Amy Adams as Louise Banks" — and
// the part belongs to the pairing rather than to either side of it, so it
// lives on the joining row. A genre has nothing of its own to say.
//
// A person is one row shared across the library, so an actor in forty films
// is one row rather than forty, and "what else are they in" is an index
// lookup rather than a scan. Identity is the name without regard to case,
// for the reason a genre's is: two spellings of one person is two people
// everywhere it matters. It is also the weakest part of this design, and
// knowingly so — two actual people with one name are one row here. The
// alternative is carrying the provider's own person ids, which would tie the
// library to one provider for a distinction that affects a handful of names.

// Credit is one person's involvement in one thing, plus where the provider
// put them in its own ordering.
//
// The name, role, part and picture are [domain.Credit], because every
// surface renders those. Position is only ever used to order a list on the
// way out, so it stays here.
type Credit struct {
	domain.Credit

	// Position is where the provider put them, which for a cast is billing
	// order — the only thing that makes the first four names the ones worth
	// showing.
	Position int
}

// maxCreditsPerItem bounds one item's credit list.
//
// A blockbuster's full cast runs to hundreds, of whom a client shows eight.
// Storing all of them would be a joining table larger than the library for
// names nobody reads, so the list is cut at the provider's own billing order
// — which is to say the ones worth keeping are the ones kept.
const maxCreditsPerItem = 40

// maxPersonNameLength bounds a name. Long enough for anybody's, short enough
// that a malformed credit is refused.
const maxPersonNameLength = 120

// cleanCredits is what a provider's answer becomes before it is stored.
//
// Trimmed, emptied of blanks, deduplicated on the pairing that identifies a
// credit — the same person can be an actor and the director of one film, and
// both are worth keeping — and cut to the cap in the order given, which is
// the provider's own.
func cleanCredits(credits []Credit) []Credit {
	out := make([]Credit, 0, len(credits))
	seen := make(map[string]struct{}, len(credits))
	for _, credit := range credits {
		credit.Name = strings.Join(strings.Fields(credit.Name), " ")
		credit.Part = strings.Join(strings.Fields(credit.Part), " ")
		if credit.Name == "" || len(credit.Name) > maxPersonNameLength {
			continue
		}
		if !credit.Role.Valid() {
			continue
		}
		if len(credit.Part) > maxPersonNameLength {
			credit.Part = credit.Part[:maxPersonNameLength]
		}
		key := strings.ToLower(credit.Name) + "\x00" + string(credit.Role) + "\x00" + strings.ToLower(credit.Part)
		if _, found := seen[key]; found {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, credit)
		if len(out) == maxCreditsPerItem {
			break
		}
	}
	return out
}

// creditsFor reads the credits of a page of items in one query.
//
// Ordered by role and then by the provider's own position, so a cast comes
// back in billing order and the first four names are the first four names.
func (d *DB) creditsFor(ctx context.Context, itemIDs []int64) (map[int64][]Credit, error) {
	if len(itemIDs) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(itemIDs))
	for _, id := range itemIDs {
		args = append(args, id)
	}
	rows, err := d.sql.QueryContext(ctx,
		`SELECT ip.item_id, p.name, ip.role, ip.part, ip.position, p.thumb_key
		   FROM item_people ip JOIN people p ON p.id = ip.person_id
		  WHERE ip.item_id IN (`+questionMarks(len(itemIDs))+`)
		  ORDER BY ip.item_id, ip.role, ip.position, p.name`, args...)
	if err != nil {
		return nil, fmt.Errorf("read credits: %w", err)
	}
	defer rows.Close()

	out := make(map[int64][]Credit, len(itemIDs))
	for rows.Next() {
		var (
			itemID int64
			credit Credit
			role   string
		)
		if err := rows.Scan(&itemID, &credit.Name, &role, &credit.Part,
			&credit.Position, &credit.Thumb); err != nil {
			return nil, fmt.Errorf("scan credit: %w", err)
		}
		credit.Role = domain.CreditRole(role)
		out[itemID] = append(out[itemID], credit)
	}
	return out, rows.Err()
}

// Credits reads one item's credits.
func (d *DB) Credits(ctx context.Context, itemID int64) ([]Credit, error) {
	byItem, err := d.creditsFor(ctx, []int64{itemID})
	if err != nil {
		return nil, err
	}
	return byItem[itemID], nil
}

// SetCredits makes an item's credits exactly these.
//
// Whole, like every other list here: the caller has a provider's answer, and
// a name the provider dropped should go from here too. A caller with nothing
// to say must not call this.
func (d *DB) SetCredits(ctx context.Context, itemID int64, credits []Credit) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set credits: %w", err)
	}
	defer tx.Rollback()
	if err := replaceCreditsTx(ctx, tx, itemID, credits); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set credits: %w", err)
	}
	return nil
}

func replaceCreditsTx(ctx context.Context, tx execer, itemID int64, credits []Credit) error {
	wanted := cleanCredits(credits)

	// Cleared and rewritten rather than reconciled. A credit list arrives
	// whole from one provider answer, the rows are a handful per item, and
	// working out which of forty names changed would be more code than
	// writing forty rows.
	if _, err := tx.ExecContext(ctx, `DELETE FROM item_people WHERE item_id = ?`, itemID); err != nil {
		return fmt.Errorf("clear credits: %w", err)
	}
	for _, credit := range wanted {
		id, err := personID(ctx, tx, credit.Name)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO item_people (item_id, person_id, role, part, position)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(item_id, person_id, role, part) DO UPDATE SET position = excluded.position`,
			itemID, id, string(credit.Role), credit.Part, credit.Position); err != nil {
			return fmt.Errorf("add credit: %w", err)
		}
		if credit.Thumb != "" {
			// Only ever filled in, never cleared: a picture somebody
			// already has is better than no picture, and a provider that
			// stopped offering one is not a reason to drop it.
			if _, err := tx.ExecContext(ctx,
				`UPDATE people SET thumb_key = ? WHERE id = ? AND thumb_key = ''`,
				credit.Thumb, id); err != nil {
				return fmt.Errorf("record a picture: %w", err)
			}
		}
	}
	return nil
}

// personID is the row for a name, created if this is the first credit to
// mention them.
func personID(ctx context.Context, tx execer, name string) (int64, error) {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO people (name) VALUES (?) ON CONFLICT(name) DO NOTHING`, name); err != nil {
		return 0, fmt.Errorf("record a person: %w", err)
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM people WHERE name = ?`, name).Scan(&id); err != nil {
		return 0, fmt.Errorf("read a person: %w", err)
	}
	return id, nil
}

// SetStudio records who made a thing.
//
// Its own call rather than part of SaveMetadata because it arrives from the
// same request as the credits and is the same kind of fact: something a
// provider volunteered that must not be erased by a later answer that was
// silent about it.
func (d *DB) SetStudio(ctx context.Context, itemID int64, studio string) error {
	studio = strings.Join(strings.Fields(studio), " ")
	if studio == "" {
		return nil
	}
	_, err := d.sql.ExecContext(ctx, `UPDATE items SET studio = ? WHERE id = ?`, studio, itemID)
	if err != nil {
		return fmt.Errorf("set studio: %w", err)
	}
	return nil
}

// PeopleNames lists everybody credited in one role across the library.
//
// What a filter menu and a smart-collection editor offer. Restricted the
// same way every other listing is, so a person who appears only in a hidden
// film is not offered: a menu entry leading to an empty shelf is worse than
// a missing one.
func (d *DB) PeopleNames(ctx context.Context, role domain.CreditRole) ([]string, error) {
	rated, ceiling := d.onlyRated("i.section_id")
	only, scope := d.only("i.section_id")
	args := append([]any{string(role)}, ceiling...)
	args = append(args, scope...)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT DISTINCT p.name FROM people p
		   JOIN item_people ip ON ip.person_id = p.id
		   JOIN items i ON i.id = ip.item_id
		  WHERE ip.role = ? AND i.extra = ''`+rated+only+`
		  ORDER BY p.name`, args...)
	if err != nil {
		return nil, fmt.Errorf("list %ss: %w", role, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan a name: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// SectionPeople lists everybody credited in one role in one library, with
// how many things each worked on.
func (d *DB) SectionPeople(ctx context.Context, sectionID int64, kind string, role domain.CreditRole) ([]GenreCount, error) {
	if !d.canSee(sectionID) {
		return nil, nil
	}
	rated, ceiling := d.onlyRated("i.section_id")
	args := append([]any{string(role), sectionID, kind}, ceiling...)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT p.id, p.name, COUNT(DISTINCT i.id) FROM people p
		   JOIN item_people ip ON ip.person_id = p.id
		   JOIN items i ON i.id = ip.item_id
		  WHERE ip.role = ? AND i.section_id = ? AND i.kind = ? AND i.extra = ''`+rated+`
		  GROUP BY p.id, p.name
		  ORDER BY p.name`, args...)
	if err != nil {
		return nil, fmt.Errorf("list section %ss: %w", role, err)
	}
	defer rows.Close()

	var out []GenreCount
	for rows.Next() {
		var row GenreCount
		if err := rows.Scan(&row.ID, &row.Name, &row.Held); err != nil {
			return nil, fmt.Errorf("scan a name: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// StudioNames lists the studios in use in one library, with counts.
func (d *DB) StudioNames(ctx context.Context, sectionID int64, kind string) ([]GenreCount, error) {
	if !d.canSee(sectionID) {
		return nil, nil
	}
	rated, ceiling := d.onlyRated("section_id")
	args := append([]any{sectionID, kind}, ceiling...)
	rows, err := d.sql.QueryContext(ctx,
		`SELECT studio, COUNT(*) FROM items
		  WHERE section_id = ? AND kind = ? AND extra = '' AND studio <> ''`+rated+`
		  GROUP BY studio ORDER BY studio`, args...)
	if err != nil {
		return nil, fmt.Errorf("list studios: %w", err)
	}
	defer rows.Close()

	var out []GenreCount
	for rows.Next() {
		var row GenreCount
		if err := rows.Scan(&row.Name, &row.Held); err != nil {
			return nil, fmt.Errorf("scan a studio: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// peopleNamesFor resolves what a client sent into names.
//
// A client hands back whatever the menu gave it, and the menu gives a row
// id. A value that matches no row is taken as a name already, and one that
// matches nothing at all is left as it is so the listing comes back empty
// rather than unfiltered.
func (d *DB) peopleNamesFor(ctx context.Context, values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if id, err := strconv.ParseInt(value, 10, 64); err == nil {
			var name string
			if err := d.sql.QueryRowContext(ctx,
				`SELECT name FROM people WHERE id = ?`, id).Scan(&name); err == nil {
				out = append(out, name)
				continue
			}
		}
		out = append(out, value)
	}
	return out
}

// CreditFilter is one menu somebody narrowed a library with.
//
// Roles is usually one — a director menu means directors. It is all of them
// where the wire does not say which role it means: Jellyfin's Person
// parameter means "things this person worked on", so a name matches whether
// they directed it or appeared in it. Expressing that as four filters would
// instead require them to have done all four.
type CreditFilter struct {
	Roles []domain.CreditRole
	Names []string
}

// peopleClause is the SQL and values for "credited to one of these, in one
// of these roles".
//
// Correlated rather than joined, for the reason the tag filter is: a film
// with two of the asked-for actors would otherwise appear twice.
func peopleClause(filter CreditFilter, names []string) (string, []any) {
	if len(filter.Roles) == 0 || len(names) == 0 {
		return "", nil
	}
	args := make([]any, 0, len(filter.Roles)+len(names))
	for _, role := range filter.Roles {
		args = append(args, string(role))
	}
	for _, name := range names {
		args = append(args, name)
	}
	return ` AND EXISTS (SELECT 1 FROM item_people ip JOIN people p ON p.id = ip.person_id
	            WHERE ip.item_id = items.id
	              AND ip.role IN (` + questionMarks(len(filter.Roles)) + `)
	              AND p.name IN (` + questionMarks(len(names)) + `))`, args
}
