package admin_test

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/jobs"
)

// backupsIn points the interface at a directory of its own and returns it.
func backupsIn(t *testing.T, h *harness) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "backups")
	h.panel.BackupDir = dir
	h.runner.BackupDir = dir
	h.runner.KeepBackups = 2
	h.panel.KeepBackups = 2
	return dir
}

// waitForBackup waits for the pass to finish, because it runs in the
// background the way every other pass does.
//
// Waiting for the job rather than for the file: the file appears the moment
// VACUUM INTO opens it and is complete only when the statement returns, so a
// test that waits for a directory entry reads a half-written database.
func waitForBackup(t *testing.T, h *harness) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		done, running := 0, 0
		for _, job := range h.runner.Jobs() {
			if job.Kind != jobs.KindBackup {
				continue
			}
			switch job.State {
			case jobs.StateDone:
				done++
			case jobs.StateFailed, jobs.StateCanceled:
				t.Fatalf("the backup pass %s: %s", job.State, job.Error)
			default:
				running++
			}
		}
		if done > 0 && running == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the backup pass did not finish")
}

// The whole flow: nothing, a button, a file, a listing, a download.
func TestABackupIsTakenListedAndDownloaded(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	dir := backupsIn(t, h)

	_, before := h.call(http.MethodGet, "/admin/api/backups", nil)
	if backups, _ := before["backups"].([]any); len(backups) != 0 {
		t.Fatalf("a server that has never backed up lists %v", backups)
	}
	if before["directory"] != dir {
		t.Fatalf("the page would write to %v", before["directory"])
	}

	res, body := h.call(http.MethodPost, "/admin/api/backups", nil)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("back up now = %d: %v", res.StatusCode, body)
	}
	waitForBackup(t, h)

	_, after := h.call(http.MethodGet, "/admin/api/backups", nil)
	backups, _ := after["backups"].([]any)
	if len(backups) != 1 {
		t.Fatalf("listed %v", backups)
	}
	first, _ := backups[0].(map[string]any)
	name, _ := first["name"].(string)
	if !strings.HasPrefix(name, "bms-") || !strings.HasSuffix(name, ".db") {
		t.Fatalf("the backup is called %q", name)
	}

	// Downloaded as a file, because a copy on the same disk protects against
	// a mistake and not against the disk.
	req, err := http.NewRequest(http.MethodGet, h.server.URL+"/admin/api/backups/"+name, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-BMS-Admin", "1")
	download, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer download.Body.Close()
	if download.StatusCode != http.StatusOK {
		t.Fatalf("download = %d", download.StatusCode)
	}
	if got := download.Header.Get("Content-Disposition"); !strings.Contains(got, name) {
		t.Fatalf("content disposition = %q", got)
	}
	// A SQLite file, which is the one check that says this is a database and
	// not an error page with a .db name.
	head := make([]byte, 16)
	if _, err := io.ReadFull(download.Body, head); err != nil {
		t.Fatal(err)
	}
	if string(head) != "SQLite format 3\x00" {
		t.Fatalf("the download starts %q", head)
	}
	if _, err := io.Copy(io.Discard, download.Body); err != nil {
		t.Fatal(err)
	}
}

// A name that is not one of ours is refused before anything touches the disk,
// which matters most for the route that deletes.
func TestABackupNameIsNotAPath(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	dir := backupsIn(t, h)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(dir), "precious.db")
	if err := os.WriteFile(outside, []byte("not a backup"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"..%2Fprecious.db", "precious.db", "bms-nonsense.db"} {
		res, _ := h.call(http.MethodDelete, "/admin/api/backups/"+name, nil)
		if res.StatusCode != http.StatusBadRequest && res.StatusCode != http.StatusNotFound {
			t.Errorf("DELETE %s = %d", name, res.StatusCode)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("a file outside the backup directory was removed: %v", err)
	}
}

// Removing one leaves the database and the others alone.
func TestABackupCanBeRemoved(t *testing.T) {
	h := newHarness(t)
	h.signIn(h.adminToken)
	backupsIn(t, h)

	h.call(http.MethodPost, "/admin/api/backups", nil)
	waitForBackup(t, h)
	_, listed := h.call(http.MethodGet, "/admin/api/backups", nil)
	backups, _ := listed["backups"].([]any)
	first, _ := backups[0].(map[string]any)
	name, _ := first["name"].(string)

	if res, _ := h.call(http.MethodDelete, "/admin/api/backups/"+name, nil); res.StatusCode != http.StatusNoContent {
		t.Fatalf("remove = %d", res.StatusCode)
	}
	if _, after := h.call(http.MethodGet, "/admin/api/backups", nil); len(after["backups"].([]any)) != 0 {
		t.Fatal("the backup is still listed")
	}
	// The database itself is untouched, which is the thing this must never
	// get wrong.
	if _, err := h.db.Sections(t.Context()); err != nil {
		t.Fatalf("the database: %v", err)
	}
}

// Nobody without a session sees the backups, let alone downloads one: it is
// the whole library's worth of accounts, tokens and watched state in a file.
func TestBackupsNeedAnAdministrator(t *testing.T) {
	h := newHarness(t)
	for _, call := range [][2]string{
		{http.MethodGet, "/admin/api/backups"},
		{http.MethodPost, "/admin/api/backups"},
		{http.MethodGet, "/admin/api/backups/bms-20261101-030000.db"},
		{http.MethodDelete, "/admin/api/backups/bms-20261101-030000.db"},
	} {
		res, _ := h.call(call[0], call[1], nil)
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without a session = %d", call[0], call[1], res.StatusCode)
		}
	}
}
