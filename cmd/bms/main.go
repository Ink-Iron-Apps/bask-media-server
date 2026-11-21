// Command bms is the Bask Media Server.
//
// One binary, one SQLite file, no runtime to install. It scans the folders it
// is pointed at and serves them in the shape Plex clients expect, which is why
// Bask and anything else that speaks that dialect can use it today.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/inknironapps/bask-media-server/internal/admin"
	"github.com/inknironapps/bask-media-server/internal/artwork"
	"github.com/inknironapps/bask-media-server/internal/detect"
	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/enrich"
	"github.com/inknironapps/bask-media-server/internal/ffprobe"
	"github.com/inknironapps/bask-media-server/internal/fingerprint"
	"github.com/inknironapps/bask-media-server/internal/jellyfin"
	"github.com/inknironapps/bask-media-server/internal/jobs"
	"github.com/inknironapps/bask-media-server/internal/plexapi"
	"github.com/inknironapps/bask-media-server/internal/scan"
	"github.com/inknironapps/bask-media-server/internal/store"
	"github.com/inknironapps/bask-media-server/internal/subtitles"
	"github.com/inknironapps/bask-media-server/internal/transcode"
	"github.com/inknironapps/bask-media-server/internal/web"
)

// version is stamped by the build; a client shows it and the update path will
// compare it, so it is not cosmetic.
var version = "0.1.0-dev"

type sectionFlag struct {
	specs []string
}

func (s *sectionFlag) String() string { return strings.Join(s.specs, ",") }

func (s *sectionFlag) Set(value string) error {
	if strings.Count(value, ":") < 2 {
		return errors.New(`want "Title:movies|shows|music|photos:/path"`)
	}
	s.specs = append(s.specs, value)
	return nil
}

func main() {
	var (
		dbPath      = flag.String("db", "bms.db", "path to the library database")
		addr        = flag.String("addr", "127.0.0.1:32500", "address to listen on")
		name        = flag.String("name", "Bask Media Server", "name clients show for this server")
		memLimit    = flag.Int64("mem-limit", 256, "soft memory ceiling in MiB; the collector works harder near it")
		scanNow     = flag.Bool("scan", false, "scan every section before serving")
		probe       = flag.Bool("probe", true, "measure durations with ffprobe during a scan")
		enrichNow   = flag.Bool("enrich", false, "ask the metadata provider about anything it has not seen")
		detectNow   = flag.Bool("detect-markers", false, "find intro and credits markers by comparing each season's audio")
		ffmpegPath  = flag.String("ffmpeg", "", "path to ffmpeg, used for marker detection (default: found on PATH)")
		detectJobs  = flag.Int("detect-workers", 0, "how many files to fingerprint at once (default: half the CPUs)")
		adminUI     = flag.Bool("admin", true, "serve the admin web interface at /admin")
		webUI       = flag.Bool("web", true, "serve the viewing web interface at /web")
		jellyfinAPI = flag.Bool("jellyfin", true, "answer the Jellyfin API, so clients can be pointed straight at this server")
		transcodes  = flag.Int("transcode-sessions", 3, "how many files may be converted at once for clients that cannot play them; 0 turns conversion off")
		accel       = flag.String("hwaccel", "auto", "hardware encoder to convert with: auto, off, or one of vaapi, nvenc, qsv, videotoolbox, amf. Anything other than off falls back to the processor if the hardware does not work")
		artworkDir  = flag.String("artwork", "", "where to cache posters and backdrops (default: beside the database)")
		artworkMB   = flag.Int64("artwork-cache-mb", 512, "cap on the artwork cache in MiB")
		tmdbKey     = flag.String("tmdb-key", "", "TMDB API key or v4 read token; falls back to BMS_TMDB_API_KEY")
		sections    sectionFlag
	)
	flag.Var(&sections, "section", `add a library on first run, as "Title:movies|shows|music|photos:/path" (repeatable)`)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	// A soft ceiling rather than a hard one: Go's collector becomes more
	// aggressive as the process approaches it, which is a slower server rather
	// than a dead one. See docs/RESOURCE-BUDGET.md.
	if *memLimit > 0 {
		debug.SetMemoryLimit(*memLimit * 1 << 20)
	}

	key := *tmdbKey
	if key == "" {
		key = os.Getenv("BMS_TMDB_API_KEY")
	}

	if err := run(context.Background(), log, runConfig{
		dbPath:     *dbPath,
		addr:       *addr,
		name:       *name,
		scanNow:    *scanNow,
		probe:      *probe,
		enrichNow:  *enrichNow,
		detectNow:  *detectNow,
		adminUI:    *adminUI,
		webUI:      *webUI,
		jellyfin:   *jellyfinAPI,
		transcodes: *transcodes,
		accel:      *accel,
		ffmpegPath: *ffmpegPath,
		detectJobs: *detectJobs,
		artworkDir: *artworkDir,
		artworkMB:  *artworkMB,
		tmdbKey:    key,
		sections:   sections.specs,
	}); err != nil {
		log.Error("bms stopped", "error", err)
		os.Exit(1)
	}
}

type runConfig struct {
	dbPath     string
	addr       string
	name       string
	scanNow    bool
	probe      bool
	enrichNow  bool
	detectNow  bool
	adminUI    bool
	webUI      bool
	jellyfin   bool
	transcodes int
	accel      string
	ffmpegPath string
	detectJobs int
	artworkDir string
	artworkMB  int64
	tmdbKey    string
	sections   []string
}

func run(ctx context.Context, log *slog.Logger, cfg runConfig) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	machineID, err := machineIdentity(ctx, db)
	if err != nil {
		return err
	}

	// First run: one admin account and one token, printed once. There is no
	// account server to fall back on, by design, so the token is how the first
	// client gets in.
	users, err := db.CountUsers(ctx)
	if err != nil {
		return err
	}
	if users == 0 {
		user, err := db.CreateUser(ctx, "admin", true)
		if err != nil {
			return err
		}
		token, err := db.IssueToken(ctx, user.ID, "first run")
		if err != nil {
			return err
		}
		log.Info("created the admin account; this token is shown once", "token", token)
	}

	if err := addSections(ctx, db, log, cfg.sections); err != nil {
		return err
	}

	// Artwork is a cache beside the database by default, so one path is the
	// whole of this server's state and a backup is a copy of one directory.
	artworkDir := cfg.artworkDir
	if artworkDir == "" {
		artworkDir = filepath.Join(filepath.Dir(cfg.dbPath), "artwork")
	}
	images, err := artwork.New(artworkDir, cfg.artworkMB<<20, 2)
	if err != nil {
		return err
	}

	// The outside tools, found once at startup and shared by the command-line
	// passes and the admin interface alike. Each one is optional: a library
	// with no ffprobe has no durations, a library with no ffmpeg has no
	// detected intros, and both are browsable and playable regardless.
	var prober scan.Prober
	if cfg.probe {
		found, err := ffprobe.New("")
		if err != nil {
			log.Warn("no ffprobe; durations and chapters will be missing until one is installed", "error", err)
		} else {
			prober = found
		}
	}
	var printer detect.Printer
	if found, err := fingerprint.New(cfg.ffmpegPath); err != nil {
		log.Warn("no ffmpeg; intros cannot be detected from audio until one is installed", "error", err)
	} else {
		printer = found
	}

	// Conversion for clients that cannot play a file as it is. Optional in
	// every direction: no ffmpeg, or -transcode-sessions 0, and every client
	// gets the file itself — which is what almost all of them want anyway.
	var transcoder *transcode.Manager
	if cfg.transcodes > 0 {
		made, err := transcode.New(transcode.Options{
			FFmpeg:      cfg.ffmpegPath,
			Root:        filepath.Join(filepath.Dir(cfg.dbPath), "transcodes"),
			MaxSessions: cfg.transcodes,
			Accel:       cfg.accel,
			Log:         log,
		})
		if err != nil {
			log.Warn("no ffmpeg, so nothing can be converted for a client that cannot play it", "error", err)
		} else {
			transcoder = made
			defer transcoder.Shutdown()
		}
	}

	runner := &jobs.Runner{
		DB:       db,
		Log:      log,
		Prober:   prober,
		Printer:  printer,
		Provider: jobs.TMDBProvider(db, cfg.tmdbKey, 4),
		Artwork:  images,
		Workers:  cfg.detectJobs,
	}
	// Nothing started here outlives the process: rule 4 of the resource
	// budget, and the reason a job is cancellable in the first place.
	defer runner.Shutdown()

	if cfg.scanNow {
		scanner := &scan.Scanner{DB: db, Prober: prober, Log: log, Artwork: images}
		list, err := db.Sections(ctx)
		if err != nil {
			return err
		}
		for _, section := range list {
			if _, err := scanner.Section(ctx, section); err != nil {
				return fmt.Errorf("scan %s: %w", section.Title, err)
			}
		}
	}

	if cfg.enrichNow {
		provider, err := runner.Provider(ctx)
		if err != nil {
			log.Warn("nothing can be enriched", "error", err)
		} else {
			enricher := &enrich.Enricher{DB: db, Provider: provider, Artwork: images, Log: log}
			if _, err := enricher.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
		}
	}

	// Markers after metadata, and before serving: both are catch-up passes
	// over whatever the scan just found, and a client that connects while one
	// is running simply sees fewer markers than it will later.
	if cfg.detectNow {
		if printer == nil {
			log.Warn("no ffmpeg, so no markers can be detected; install one or pass -ffmpeg")
		} else {
			detector := &detect.Detector{DB: db, Printer: printer, Log: log, Workers: cfg.detectJobs}
			if _, err := detector.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			if swept, err := db.SweepFingerprints(ctx); err != nil {
				log.Warn("could not sweep stale fingerprints", "error", err)
			} else if swept > 0 {
				log.Info("dropped fingerprints for files that have changed", "count", swept)
			}
		}
	}

	// One converter, shared: it holds a path and a timeout, and the work it
	// does is per request.
	converter := &subtitles.Converter{FFmpeg: ffmpegPath(cfg.ffmpegPath)}

	api := &plexapi.Server{
		Store: db, Log: log, Name: cfg.name, MachineID: machineID, Version: version,
		Artwork: images, Subtitles: converter,
	}

	// One listener for every surface. The Plex-compatible API owns the root,
	// because that is where clients look for it; the interfaces are mounted
	// under /admin and /web, where Go's mux redirects the bare path to the
	// directory form for a browser by itself.
	handler := http.Handler(api.Handler())
	if cfg.adminUI || cfg.webUI || cfg.jellyfin {
		root := http.NewServeMux()
		if cfg.jellyfin {
			// A second presenter over the same library, for the clients that
			// are told an address rather than an account — which is every
			// client a server like this one can actually be reached by.
			emby := &jellyfin.Server{
				Store: db, Log: log, Name: cfg.name, MachineID: machineID, Version: version,
				Artwork: images, Subtitles: converter,
			}
			if transcoder != nil {
				emby.Transcoder = transcoder
			}
			if prober != nil {
				// Together these are what lets a client ask for a smaller
				// version of a file: one to measure it, one to convert it.
				emby.Prober = prober
			}
			handler := emby.Handler()
			for _, prefix := range jellyfin.Prefixes() {
				root.Handle(prefix, handler)
				// Clients are not consistent about the case of the fixed
				// segments, and the mux matches a path exactly: the lower-case
				// spelling has to reach the same place, where the handler's own
				// normaliser puts it right.
				if lower := strings.ToLower(prefix); lower != prefix {
					root.Handle(lower, handler)
				}
			}
		}
		if cfg.adminUI {
			panel := &admin.Server{
				DB:        db,
				Jobs:      runner,
				Log:       log,
				Name:      cfg.name,
				MachineID: machineID,
				Version:   version,
				Artwork:   images,
				Started:   time.Now(),
			}
			if transcoder != nil {
				panel.Transcoding = transcoder.Sessions
			}
			root.Handle("/admin/", panel.Handler())
		}
		if cfg.webUI {
			viewing := &web.Server{
				Store:   db,
				Log:     log,
				Name:    cfg.name,
				Artwork: images,
			}
			if prober != nil {
				viewing.Prober = prober
			}
			if transcoder != nil {
				viewing.Transcoder = transcoder
			}
			viewing.Subtitles = converter
			root.Handle("/web/", viewing.Handler())
		}
		root.Handle("/", api.Handler())
		handler = root
	}

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout on purpose: a response here can be a two-hour film
		// streaming at the speed the client plays it, and a write deadline
		// would cut it off mid-sentence.
		IdleTimeout: 2 * time.Minute,
	}

	errs := make(chan error, 1)
	go func() {
		log.Info("serving", "addr", cfg.addr, "machineIdentifier", machineID, "version", version,
			"admin", cfg.adminUI, "web", cfg.webUI)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	go reportMemory(ctx, log)

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// ffmpegPath resolves the converter's binary: what was asked for, or
// whatever is on PATH, or nothing — in which case the conversions that need
// no ffmpeg still work and the rest report that they cannot.
func ffmpegPath(configured string) string {
	if configured != "" {
		return configured
	}
	found, err := exec.LookPath("ffmpeg")
	if err != nil {
		return ""
	}
	return found
}

// machineIdentity returns this server's permanent id, creating it once.
//
// Clients key everything they remember on this value, so regenerating it would
// silently orphan every setting and downloaded file on every device.
func machineIdentity(ctx context.Context, db *store.DB) (string, error) {
	const key = "machine_identifier"
	existing, err := db.ServerValue(ctx, key)
	if err != nil {
		return "", err
	}
	if existing != "" {
		return existing, nil
	}

	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate machine identifier: %w", err)
	}
	id := hex.EncodeToString(raw)
	if err := db.SetServerValue(ctx, key, id); err != nil {
		return "", err
	}
	return id, nil
}

func addSections(ctx context.Context, db *store.DB, log *slog.Logger, specs []string) error {
	if len(specs) == 0 {
		return nil
	}
	existing, err := db.Sections(ctx)
	if err != nil {
		return err
	}
	have := make(map[string]bool, len(existing))
	for _, s := range existing {
		have[s.Title] = true
	}

	for _, spec := range specs {
		parts := strings.SplitN(spec, ":", 3)
		title, kind, root := parts[0], domain.SectionKind(parts[1]), parts[2]
		if !kind.Valid() {
			return fmt.Errorf("section %q: kind must be movies, shows, music or photos", title)
		}
		if have[title] {
			continue
		}
		if _, err := db.CreateSection(ctx, title, kind, []string{root}); err != nil {
			return err
		}
		log.Info("added section", "title", title, "kind", string(kind), "root", root)
	}
	return nil
}

// reportMemory logs resident memory hourly.
//
// A number in a log is how a slow leak gets noticed before the machine starts
// swapping, and it costs one line an hour.
func reportMemory(ctx context.Context, log *slog.Logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var stats runtime.MemStats
			runtime.ReadMemStats(&stats)
			log.Info("memory",
				"heapMB", stats.HeapAlloc>>20,
				"sysMB", stats.Sys>>20,
				"goroutines", runtime.NumGoroutine())
		}
	}
}
