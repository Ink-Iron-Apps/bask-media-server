package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// Per-library settings, and the question the scheduler asks of them.
//
// The settings live in one JSON column for the same reasons a smart
// collection's rules do: they are only ever read and written whole, by the
// page that writes them, and a column per setting would be a migration for
// every new one. Unreadable JSON reads as the defaults rather than failing —
// a library whose settings were corrupted should still be browsable, and the
// page that rewrites them is where that gets noticed.

// maxSettingsBytes bounds what will be stored in the column. The settings are
// four fields; anything near this is not settings.
const maxSettingsBytes = 4 << 10

// scanFloor is the shortest interval a library may scan itself at.
//
// A scan of an unchanged library costs a stat per file, which is cheap but
// not free, and a library set to one minute would spend more time walking
// than waiting. Fifteen minutes is frequent enough that a film added before
// dinner is there after it.
const scanFloor = 15 * time.Minute

// parseSectionSettings reads the column, defaulting on anything it cannot
// make sense of.
func parseSectionSettings(stored string) domain.SectionSettings {
	if stored == "" {
		return domain.SectionSettings{}
	}
	var out domain.SectionSettings
	if err := json.Unmarshal([]byte(stored), &out); err != nil {
		return domain.SectionSettings{}
	}
	return out
}

func marshalSectionSettings(s domain.SectionSettings) (string, error) {
	// The zero value is the empty string, so a library set back to the
	// defaults is indistinguishable from one that never had settings.
	if s == (domain.SectionSettings{}) {
		return "", nil
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("store settings: %w", err)
	}
	if len(encoded) > maxSettingsBytes {
		return "", fmt.Errorf("store settings: too large")
	}
	return string(encoded), nil
}

// ValidateSectionSettings refuses a setting that cannot mean what it says.
//
// Refused at save time rather than clamped silently, because a scan interval
// of one minute is somebody misunderstanding the field and a message is more
// use to them than a number quietly becoming fifteen.
func ValidateSectionSettings(s domain.SectionSettings) error {
	if s.ScanEvery < 0 {
		return fmt.Errorf("a scan interval cannot be negative")
	}
	if s.ScanEvery > 0 && time.Duration(s.ScanEvery)*time.Minute < scanFloor {
		return fmt.Errorf("a library cannot scan itself more often than every %d minutes",
			int(scanFloor.Minutes()))
	}
	// A week. Past this somebody means "never", and the field for that is
	// zero, which is also what the page offers.
	if s.ScanEvery > 7*24*60 {
		return fmt.Errorf("a scan interval longer than a week means never — leave it empty for that")
	}
	return nil
}

// SetSectionSettings stores what one library wants done to it.
func (d *DB) SetSectionSettings(ctx context.Context, sectionID int64, s domain.SectionSettings) error {
	if err := ValidateSectionSettings(s); err != nil {
		return err
	}
	stored, err := marshalSectionSettings(s)
	if err != nil {
		return err
	}
	res, err := d.sql.ExecContext(ctx,
		`UPDATE sections SET settings = ?, updated_at = ? WHERE id = ?`,
		stored, millis(time.Now().UTC()), sectionID)
	if err != nil {
		return fmt.Errorf("set settings: %w", err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return ErrNotFound
	}
	return nil
}

// DueForScan is the libraries whose own interval says it is time, oldest
// visit first.
//
// Measured from when the scheduler last looked rather than from when a scan
// last finished, which is what keeps a library whose scan fails from being
// retried in a tight loop: the visit is recorded either way.
//
// Unrestricted on purpose. This is the scheduler asking, not a viewer: a
// library nobody can see still has to be scanned, or it would quietly stop
// updating for the one account that can.
func (d *DB) DueForScan(ctx context.Context, now time.Time) ([]domain.Section, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT id, title, kind, roots, scanned_at, created_at, updated_at, settings, swept_at
		   FROM sections WHERE settings <> '' ORDER BY swept_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list libraries to scan: %w", err)
	}
	defer rows.Close()

	var out []domain.Section
	for rows.Next() {
		var (
			s                                        domain.Section
			kind, roots, settings                    string
			scannedAt, createdAt, updatedAt, sweptAt int64
		)
		if err := rows.Scan(&s.ID, &s.Title, &kind, &roots, &scannedAt, &createdAt, &updatedAt,
			&settings, &sweptAt); err != nil {
			return nil, fmt.Errorf("scan library row: %w", err)
		}
		s.Kind = domain.SectionKind(kind)
		if roots != "" {
			s.Roots = strings.Split(roots, "\n")
		}
		s.Settings = parseSectionSettings(settings)
		s.ScannedAt, s.CreatedAt, s.UpdatedAt = fromMillis(scannedAt), fromMillis(createdAt), fromMillis(updatedAt)
		s.SweptAt = fromMillis(sweptAt)

		every := s.Settings.ScanInterval()
		if every <= 0 {
			continue
		}
		// A library that has never been swept is due immediately, which is
		// what somebody who just turned this on expects to see.
		if s.SweptAt.IsZero() || !now.Before(s.SweptAt.Add(every)) {
			out = append(out, s)
		}
	}
	return out, rows.Err()
}

// MarkSwept records that the scheduler looked at this library, whatever came
// of it.
func (d *DB) MarkSwept(ctx context.Context, sectionID int64, when time.Time) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE sections SET swept_at = ? WHERE id = ?`, millis(when), sectionID)
	if err != nil {
		return fmt.Errorf("mark swept: %w", err)
	}
	return nil
}

// allowedSections is the ids of the libraries whose settings permit a pass.
//
// Read in Go rather than decided in SQL, because the settings are a JSON
// column and a query that picked them apart would be a second parser to keep
// agreeing with the first. The table has a handful of rows, so reading all of
// them to filter a batch query is not a cost worth optimising.
//
// Returns nil when every library is allowed, which lets the caller skip the
// clause entirely rather than build an IN list of everything.
func (d *DB) allowedSections(ctx context.Context, allow func(domain.Section) bool) (ids []int64, all bool, err error) {
	sections, err := d.Sections(ctx)
	if err != nil {
		return nil, false, err
	}
	allowed := make([]int64, 0, len(sections))
	for _, section := range sections {
		if allow(section) {
			allowed = append(allowed, section.ID)
		}
	}
	return allowed, len(allowed) == len(sections), nil
}

// sectionFilter is the SQL and arguments for "in one of these libraries".
//
// An empty list is not no filter: it is a filter nothing satisfies, which is
// the honest reading of "every library has this turned off". Returning no
// clause there would run the pass over the whole library instead, which is
// the opposite of what was asked.
func sectionFilter(column string, ids []int64, all bool) (string, []any) {
	if all {
		return "", nil
	}
	if len(ids) == 0 {
		return " AND 0 = 1", nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	return " AND " + column + " IN (" + questionMarks(len(ids)) + ")", args
}
