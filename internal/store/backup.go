package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Backing up the one file everything lives in.
//
// "One binary, one database file" is the shape of this server, and the other
// half of that promise is that the file can be copied. It cannot be copied
// with cp while the server is running — a WAL-mode database is a file plus a
// write-ahead log, and a copy taken between the two is a database that opens
// and is wrong in ways nobody notices until they need it.
//
// VACUUM INTO is SQLite's answer: it writes a new, complete, defragmented
// database at a path of your choosing, inside a read transaction, while
// everything else carries on. No locking out of writers for the duration, no
// second process, no WAL to carry alongside the result. The file it produces
// is a plain database that opens anywhere.

// BackupName is what a backup is called: sortable, obvious, and distinct per
// second, which is as often as anybody has reason to take two.
func BackupName(at time.Time) string {
	return "bms-" + at.UTC().Format("20060102-150405") + ".db"
}

// Backup writes a complete copy of the database to path.
//
// The destination must not exist: VACUUM INTO refuses to overwrite, and that
// refusal is worth keeping rather than working around. A backup that silently
// replaced the one good copy with a broken one would be the worst failure
// this package could have.
func (d *DB) Backup(ctx context.Context, path string) (int64, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return 0, fmt.Errorf("backup to %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return 0, fmt.Errorf("backup directory: %w", err)
	}
	if _, err := os.Stat(absolute); err == nil {
		return 0, fmt.Errorf("backup to %s: there is already a file there", absolute)
	}

	// The path goes in as a quoted string literal rather than a parameter:
	// VACUUM is not a prepared statement and will not take one. Single quotes
	// are doubled, which is the whole of the escaping SQLite's string
	// literals need — and the path never comes from a client, only from this
	// server's own configuration.
	quoted := "'" + strings.ReplaceAll(absolute, "'", "''") + "'"
	if _, err := d.sql.ExecContext(ctx, `VACUUM INTO `+quoted); err != nil {
		// A half-written destination is worse than none: it would look like a
		// backup to anybody listing the directory.
		_ = os.Remove(absolute)
		return 0, fmt.Errorf("backup to %s: %w", absolute, err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return 0, fmt.Errorf("backup to %s: %w", absolute, err)
	}
	return info.Size(), nil
}

// Backup is one copy on disk.
type Backup struct {
	Name  string
	Path  string
	Size  int64
	Taken time.Time
}

// Backups lists what is in a backup directory, newest first.
//
// Only the files this package writes: a directory somebody also keeps other
// things in is their business, and a listing that offered to delete a file
// BMS did not create would eventually delete the wrong thing.
func Backups(dir string) ([]Backup, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}

	out := make([]Backup, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !isBackupName(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		out = append(out, Backup{
			Name: entry.Name(), Path: filepath.Join(dir, entry.Name()),
			Size: info.Size(), Taken: info.ModTime().UTC(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// PruneBackups keeps the newest keep backups and removes the rest, reporting
// how many it removed.
//
// A backup that fills the disk is a backup that takes the server down, which
// is the opposite of the point. Zero or less keeps everything, because a
// configuration that reads as "keep none" should not be the one that deletes
// every copy.
func PruneBackups(dir string, keep int) (int, error) {
	if keep <= 0 {
		return 0, nil
	}
	found, err := Backups(dir)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, backup := range found[min(keep, len(found)):] {
		if err := os.Remove(backup.Path); err != nil {
			return removed, fmt.Errorf("remove old backup: %w", err)
		}
		removed++
	}
	return removed, nil
}

// isBackupName reports whether a filename is one of ours: the prefix, the
// timestamp's length, and the extension.
func isBackupName(name string) bool {
	if !strings.HasPrefix(name, "bms-") || !strings.HasSuffix(name, ".db") {
		return false
	}
	stamp := strings.TrimSuffix(strings.TrimPrefix(name, "bms-"), ".db")
	_, err := time.Parse("20060102-150405", stamp)
	return err == nil
}

// DefaultBackupDir is where backups go when nobody has said otherwise: beside
// the database, in a folder of their own.
//
// Beside it rather than in a system-wide location because that is where the
// person running this already looks, and because a database on an external
// drive should have its copies on the same drive by default rather than
// silently filling the boot disk.
func DefaultBackupDir(databasePath string) string {
	return filepath.Join(filepath.Dir(databasePath), "backups")
}
