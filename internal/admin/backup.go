package admin

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/inknironapps/bask-media-server/internal/jobs"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// Backups, in the interface.
//
// The database is the one thing here that cannot be rebuilt: the media is on
// disk and a scan finds it again, but watched state, resume positions,
// playlists, accounts and tokens exist nowhere else. Until now the only way
// to copy it was to stop the server, because a WAL-mode database copied with
// cp is a file plus a log and a copy taken between the two is quietly wrong.

// backupView is one copy on disk.
type backupView struct {
	Name   string `json:"name"`
	SizeMB int64  `json:"sizeMB"`
	Taken  string `json:"taken"`
}

// listBackups is what there is, where it goes, and how many are kept.
func (s *Server) listBackups(w http.ResponseWriter, r *http.Request) {
	dir := s.backupDir()
	found, err := store.Backups(dir)
	if err != nil {
		s.fail(w, "list backups", err)
		return
	}
	out := make([]backupView, 0, len(found))
	for _, backup := range found {
		out = append(out, backupView{
			Name: backup.Name, SizeMB: backup.Size >> 20,
			Taken: backup.Taken.Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"backups":   out,
		"directory": dir,
		"keep":      s.keepBackups(),
	})
}

// downloadBackup hands a copy over, so a backup can live somewhere other than
// the machine that would lose it.
//
// A copy on the same disk as the database protects against a mistake, not
// against the disk. This is the route that makes it protect against the disk.
func (s *Server) downloadBackup(w http.ResponseWriter, r *http.Request) {
	path, ok := s.backupPath(w, r)
	if !ok {
		return
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "no such backup")
		return
	}
	if err != nil {
		s.fail(w, "open backup", err)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		s.fail(w, "read backup", err)
		return
	}

	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(path)+`"`)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), file)
}

// deleteBackup removes one.
func (s *Server) deleteBackup(w http.ResponseWriter, r *http.Request) {
	path, ok := s.backupPath(w, r)
	if !ok {
		return
	}
	if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "no such backup")
		return
	} else if err != nil {
		s.fail(w, "remove backup", err)
		return
	}
	s.logger().Info("removed a backup", "name", filepath.Base(path))
	w.WriteHeader(http.StatusNoContent)
}

// startBackup takes one now.
func (s *Server) startBackup(w http.ResponseWriter, r *http.Request) {
	s.startPass(w, r, jobs.KindBackup, 0)
}

// backupPath turns the name in the URL into a path inside the backup
// directory, refusing anything else.
//
// The name is checked against the pattern this server writes rather than
// merely cleaned, because a path that arrives from a request and ends up in
// os.Remove is exactly the place a traversal belongs. A name with a separator
// in it, or a name that is not one of ours, is refused before anything
// touches the disk.
func (s *Server) backupPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if name == "" || name != filepath.Base(name) || strings.ContainsAny(name, `/\`) {
		writeError(w, http.StatusBadRequest, "that is not a backup")
		return "", false
	}
	dir := s.backupDir()
	found, err := store.Backups(dir)
	if err != nil {
		s.fail(w, "list backups", err)
		return "", false
	}
	for _, backup := range found {
		if backup.Name == name {
			return backup.Path, true
		}
	}
	writeError(w, http.StatusNotFound, "no such backup")
	return "", false
}

func (s *Server) backupDir() string {
	if s.BackupDir != "" {
		return s.BackupDir
	}
	return store.DefaultBackupDir(s.DB.Path())
}

func (s *Server) keepBackups() int {
	if s.KeepBackups > 0 {
		return s.KeepBackups
	}
	return 7
}
