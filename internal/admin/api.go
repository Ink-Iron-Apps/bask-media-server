package admin

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/jobs"
	"github.com/inknironapps/bask-media-server/internal/scan"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// overview is the status page: what this server is, what it holds, and what it
// is doing right now.
func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	stats, err := s.DB.Stats(r.Context())
	if err != nil {
		s.fail(w, "read stats", err)
		return
	}
	heapMB, goroutines := memory()

	out := map[string]any{
		"name":       s.friendlyName(r),
		"version":    s.Version,
		"machineId":  s.MachineID,
		"uptime":     int64(s.uptime().Seconds()),
		"heapMB":     heapMB,
		"goroutines": goroutines,
		"databaseMB": bytesOf(s.DB.Path()) >> 20,
		"stats":      stats,
		"jobs":       s.Jobs.Jobs(),
		"canScan":    s.Jobs.Prober != nil,
		// Asked of the runner rather than inferred from a field: whether
		// metadata can be fetched depends on a key that may have been saved
		// from this very page a moment ago.
		"canEnrich": s.Jobs.Can(r.Context(), jobs.KindEnrich) == nil,
		"canDetect": s.Jobs.Printer != nil,
	}
	if s.Artwork != nil {
		if bytes, files, err := s.Artwork.Usage(); err == nil {
			out["artworkMB"] = bytes >> 20
			out["artworkFiles"] = files
			out["artworkCapMB"] = s.Artwork.MaxBytes >> 20
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// bytesOf is a file's size, or zero when it cannot be measured: a status page
// reporting nothing is better than one that fails.
func bytesOf(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func (s *Server) friendlyName(r *http.Request) string {
	stored, err := s.DB.ServerValue(r.Context(), store.KeyFriendlyName)
	if err == nil && stored != "" {
		return stored
	}
	return s.Name
}

// sectionView is a library as the page draws it: the configuration plus how
// much is in it.
type sectionView struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Kind      string    `json:"kind"`
	Roots     []string  `json:"roots"`
	ScannedAt time.Time `json:"scannedAt,omitempty"`
	Items     int       `json:"items"`
	Episodes  int       `json:"episodes"`
	Files     int       `json:"files"`
	Bytes     int64     `json:"bytes"`
	Missing   []string  `json:"missing,omitempty"`
}

func (s *Server) listSections(w http.ResponseWriter, r *http.Request) {
	sections, err := s.DB.Sections(r.Context())
	if err != nil {
		s.fail(w, "list sections", err)
		return
	}
	counts, err := s.DB.SectionStats(r.Context())
	if err != nil {
		s.fail(w, "count sections", err)
		return
	}

	out := make([]sectionView, 0, len(sections))
	for _, sec := range sections {
		stats := counts[sec.ID]
		view := sectionView{
			ID: sec.ID, Title: sec.Title, Kind: string(sec.Kind), Roots: sec.Roots,
			ScannedAt: sec.ScannedAt,
			Items:     stats.Items, Episodes: stats.Episodes, Files: stats.Files, Bytes: stats.Bytes,
		}
		// A root that is not there is the single most common reason a library
		// empties itself: an unplugged disk, or a drive letter that moved. Say
		// so on the page rather than leaving a scan to silently prune
		// everything.
		for _, root := range sec.Roots {
			if info, err := os.Stat(root); err != nil || !info.IsDir() {
				view.Missing = append(view.Missing, root)
			}
		}
		out = append(out, view)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createSection(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title string   `json:"title"`
		Kind  string   `json:"kind"`
		Roots []string `json:"roots"`
	}
	if !decode(w, r, &body) {
		return
	}
	title, kind, roots, ok := s.validSection(w, body.Title, body.Kind, body.Roots)
	if !ok {
		return
	}

	section, err := s.DB.CreateSection(r.Context(), title, kind, roots)
	if err != nil {
		s.fail(w, "create section", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": section.ID})
}

func (s *Server) updateSection(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	var body struct {
		Title string   `json:"title"`
		Roots []string `json:"roots"`
	}
	if !decode(w, r, &body) {
		return
	}
	section, err := s.DB.Section(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such library")
		return
	}
	if err != nil {
		s.fail(w, "read section", err)
		return
	}
	// The kind cannot change: a shows library and a movies library are parsed
	// differently, so switching one would describe every item in it wrongly
	// until a rescan, and some of them for ever.
	title, _, roots, ok := s.validSection(w, body.Title, string(section.Kind), body.Roots)
	if !ok {
		return
	}
	if err := s.DB.UpdateSection(r.Context(), id, title, roots); err != nil {
		s.fail(w, "update section", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) validSection(w http.ResponseWriter, title, kind string, roots []string) (string, domain.SectionKind, []string, bool) {
	title = strings.TrimSpace(title)
	if !usableName(title) {
		writeError(w, http.StatusBadRequest, "a library needs a name")
		return "", "", nil, false
	}
	sectionKind := domain.SectionKind(strings.TrimSpace(kind))
	if sectionKind != domain.SectionMovies && sectionKind != domain.SectionShows {
		writeError(w, http.StatusBadRequest, `a library holds either "movies" or "shows"`)
		return "", "", nil, false
	}

	cleaned := make([]string, 0, len(roots))
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		// Checked here rather than at scan time because this is the moment
		// somebody is looking: a typo reported now is a corrected path, and a
		// typo reported in a log is an empty library nobody can explain.
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			writeError(w, http.StatusBadRequest, "there is no folder at "+root)
			return "", "", nil, false
		}
		cleaned = append(cleaned, filepath.Clean(root))
	}
	if len(cleaned) == 0 {
		writeError(w, http.StatusBadRequest, "a library needs at least one folder")
		return "", "", nil, false
	}
	// Newlines would be indistinguishable from the separator the roots are
	// stored with, which would turn one path into two unusable ones.
	for _, root := range cleaned {
		if strings.ContainsAny(root, "\n\r") {
			writeError(w, http.StatusBadRequest, "a folder path cannot contain a line break")
			return "", "", nil, false
		}
	}
	return title, sectionKind, cleaned, true
}

// deleteSection forgets a library. Nothing on disk is touched, and the page
// says so before asking.
func (s *Server) deleteSection(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	err := s.DB.DeleteSection(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such library")
		return
	}
	if err != nil {
		s.fail(w, "delete section", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) scanSection(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	s.startPass(w, r, jobs.KindScan, id)
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Jobs.Jobs())
}

func (s *Server) startJob(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind      string `json:"kind"`
		SectionID int64  `json:"sectionId,omitempty"`
	}
	if !decode(w, r, &body) {
		return
	}
	s.startPass(w, r, jobs.Kind(body.Kind), body.SectionID)
}

func (s *Server) startPass(w http.ResponseWriter, r *http.Request, kind jobs.Kind, sectionID int64) {
	job, err := s.Jobs.Start(r.Context(), kind, sectionID)
	switch {
	case errors.Is(err, jobs.ErrBusy):
		writeError(w, http.StatusConflict, "that pass is already running")
		return
	case errors.Is(err, jobs.ErrUnavailable):
		// 422 rather than 500: the request was understood and the server is
		// fine, it simply cannot do this — no ffmpeg, no metadata key.
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such library")
		return
	case err != nil:
		s.fail(w, "start job", err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	if err := s.Jobs.Cancel(id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such job")
		return
	} else if err != nil {
		s.fail(w, "cancel job", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// userView is an account as the page draws it. No hash, no token, ever.
type userView struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	Admin     bool      `json:"admin"`
	CreatedAt time.Time `json:"createdAt"`
	Tokens    int       `json:"tokens"`
	Self      bool      `json:"self"`
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.DB.Users(r.Context())
	if err != nil {
		s.fail(w, "list users", err)
		return
	}
	me := sessionUser(r.Context())

	out := make([]userView, 0, len(users))
	for _, u := range users {
		tokens, err := s.DB.Tokens(r.Context(), u.ID)
		if err != nil {
			s.fail(w, "count tokens", err)
			return
		}
		out = append(out, userView{
			ID: u.ID, Username: u.Username, Admin: u.Admin, CreatedAt: u.CreatedAt,
			Tokens: len(tokens), Self: me != nil && me.ID == u.ID,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Admin    bool   `json:"admin"`
	}
	if !decode(w, r, &body) {
		return
	}
	username := strings.TrimSpace(body.Username)
	if !usableName(username) {
		writeError(w, http.StatusBadRequest, "a name is needed, without line breaks, up to 64 characters")
		return
	}

	user, err := s.DB.CreateUser(r.Context(), username, body.Admin)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeError(w, http.StatusConflict, "there is already an account called "+username)
			return
		}
		s.fail(w, "create user", err)
		return
	}
	// A token with the account, because an account with no credential cannot
	// be used and the next thing anybody does is issue one. Shown once: only
	// the hash is stored.
	token, err := s.DB.IssueToken(r.Context(), user.ID, "created with the account")
	if err != nil {
		s.fail(w, "issue token", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": user.ID, "token": token})
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	var body struct {
		Admin *bool `json:"admin"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Admin == nil {
		writeError(w, http.StatusBadRequest, "nothing to change")
		return
	}
	user, err := s.DB.User(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such account")
		return
	}
	if err != nil {
		s.fail(w, "read user", err)
		return
	}
	if user.Admin && !*body.Admin {
		if ok, err := s.lastAdmin(w, r); err != nil || ok {
			return
		}
	}
	if err := s.DB.SetUserAdmin(r.Context(), id, *body.Admin); err != nil {
		s.fail(w, "set admin", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	user, err := s.DB.User(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such account")
		return
	}
	if err != nil {
		s.fail(w, "read user", err)
		return
	}
	if user.Admin {
		if ok, err := s.lastAdmin(w, r); err != nil || ok {
			return
		}
	}
	if err := s.DB.DeleteUser(r.Context(), id); err != nil {
		s.fail(w, "delete user", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// lastAdmin refuses a change that would leave the server with nobody who can
// administer it. There is no account service to recover from that: the only
// way back would be editing the database by hand.
func (s *Server) lastAdmin(w http.ResponseWriter, r *http.Request) (refused bool, err error) {
	admins, err := s.DB.CountAdmins(r.Context())
	if err != nil {
		s.fail(w, "count admins", err)
		return false, err
	}
	if admins <= 1 {
		writeError(w, http.StatusConflict,
			"this is the only account that can administer the server; make another one an administrator first")
		return true, nil
	}
	return false, nil
}

// tokenView describes a credential without being one.
type tokenView struct {
	ID         int64     `json:"id"`
	Label      string    `json:"label"`
	CreatedAt  time.Time `json:"createdAt"`
	LastSeenAt time.Time `json:"lastSeenAt,omitempty"`
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	tokens, err := s.DB.Tokens(r.Context(), id)
	if err != nil {
		s.fail(w, "list tokens", err)
		return
	}
	out := make([]tokenView, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, tokenView{ID: t.ID, Label: t.Label, CreatedAt: t.CreatedAt, LastSeenAt: t.LastSeenAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) issueToken(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	var body struct {
		Label string `json:"label"`
	}
	if !decode(w, r, &body) {
		return
	}
	if _, err := s.DB.User(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such account")
		return
	} else if err != nil {
		s.fail(w, "read user", err)
		return
	}

	label := strings.TrimSpace(body.Label)
	if len(label) > 64 {
		label = label[:64]
	}
	token, err := s.DB.IssueToken(r.Context(), id, label)
	if err != nil {
		s.fail(w, "issue token", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": token})
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	err := s.DB.RevokeToken(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such token")
		return
	}
	if err != nil {
		s.fail(w, "revoke token", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) readSettings(w http.ResponseWriter, r *http.Request) {
	key, err := s.DB.ServerValue(r.Context(), store.KeyTMDB)
	if err != nil {
		s.fail(w, "read settings", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        s.friendlyName(r),
		"nameDefault": s.Name,
		// The key itself never leaves the server, not even to the page that
		// set it: a status is all anybody needs to know, and a secret that is
		// never sent cannot be read out of a browser cache or a proxy log.
		"tmdbKeyStored": key != "",
		// A key can also arrive on the command line or from the environment,
		// which is what a server set up before this page existed looks like.
		"tmdbKeyElsewhere": key == "" && s.Jobs.Can(r.Context(), jobs.KindEnrich) == nil,
		"ffprobe":          s.Jobs.Prober != nil,
		"ffmpeg":           s.Jobs.Printer != nil,
		"sessionDays":      int(s.sessionTTL().Hours() / 24),
	})
}

func (s *Server) writeSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    *string `json:"name"`
		TMDBKey *string `json:"tmdbKey"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		if !usableName(name) {
			writeError(w, http.StatusBadRequest, "a server needs a name, up to 64 characters")
			return
		}
		if err := s.DB.SetServerValue(r.Context(), store.KeyFriendlyName, name); err != nil {
			s.fail(w, "set name", err)
			return
		}
	}
	if body.TMDBKey != nil {
		// Stored in the database in the clear, like every other setting: it is
		// a key to somebody else's read-only catalogue, the database is already
		// the file that holds every token hash, and encrypting it would mean a
		// second secret kept next to the first.
		if err := s.DB.SetServerValue(r.Context(), store.KeyTMDB, strings.TrimSpace(*body.TMDBKey)); err != nil {
			s.fail(w, "set key", err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// browse lists folders so a library can be set up by picking rather than by
// typing a path into a box and hoping.
//
// Reads directories as the user the server runs as, which is what a scan does
// anyway, and lists only directories: this is a folder picker, not a file
// browser, and the media files themselves are counted rather than named.
func (s *Server) browse(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		writeJSON(w, http.StatusOK, map[string]any{"path": "", "folders": browseRoots()})
		return
	}
	if !filepath.IsAbs(path) {
		writeError(w, http.StatusBadRequest, "a folder has to be given as a full path")
		return
	}
	path = filepath.Clean(path)

	entries, err := os.ReadDir(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read "+path+": "+err.Error())
		return
	}

	type folder struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	var (
		folders []folder
		media   int
	)
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			// Hidden folders are system clutter here — .Trash, .AppleDouble,
			// @eaDir — and never where somebody keeps their films.
			continue
		}
		if entry.IsDir() {
			folders = append(folders, folder{Name: name, Path: filepath.Join(path, name)})
			continue
		}
		if scan.IsMedia(name) {
			media++
		}
	}
	sort.Slice(folders, func(i, j int) bool {
		return strings.ToLower(folders[i].Name) < strings.ToLower(folders[j].Name)
	})

	parent := filepath.Dir(path)
	if parent == path {
		parent = "" // at a root: the picker offers the list of roots again
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":    path,
		"parent":  parent,
		"folders": folders,
		"media":   media,
	})
}

// browseRoots is where the picker starts: the drives on Windows, the file
// system root and the server's own home directory elsewhere.
func browseRoots() []map[string]string {
	var out []map[string]string
	add := func(name, path string) {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			out = append(out, map[string]string{"name": name, "path": path})
		}
	}
	if runtime.GOOS == "windows" {
		for letter := 'A'; letter <= 'Z'; letter++ {
			add(string(letter)+":\\", string(letter)+":\\")
		}
		return out
	}
	add("/", "/")
	if home, err := os.UserHomeDir(); err == nil {
		add(home, home)
	}
	add("/mnt", "/mnt")
	add("/media", "/media")
	return out
}

// usableName is the one validation every name here shares: present, short
// enough to display, and on one line.
func usableName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if r == '\n' || r == '\r' || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func idFrom(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "that is not an id")
		return 0, false
	}
	return id, true
}
