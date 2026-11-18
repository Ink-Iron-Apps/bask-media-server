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
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/ffprobe"
	"github.com/inknironapps/bask-media-server/internal/plexapi"
	"github.com/inknironapps/bask-media-server/internal/scan"
	"github.com/inknironapps/bask-media-server/internal/store"
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
		return errors.New(`want "Title:movies|shows:/path"`)
	}
	s.specs = append(s.specs, value)
	return nil
}

func main() {
	var (
		dbPath   = flag.String("db", "bms.db", "path to the library database")
		addr     = flag.String("addr", "127.0.0.1:32500", "address to listen on")
		name     = flag.String("name", "Bask Media Server", "name clients show for this server")
		memLimit = flag.Int64("mem-limit", 256, "soft memory ceiling in MiB; the collector works harder near it")
		scanNow  = flag.Bool("scan", false, "scan every section before serving")
		probe    = flag.Bool("probe", true, "measure durations with ffprobe during a scan")
		sections sectionFlag
	)
	flag.Var(&sections, "section", `add a library on first run, as "Title:movies|shows:/path" (repeatable)`)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	// A soft ceiling rather than a hard one: Go's collector becomes more
	// aggressive as the process approaches it, which is a slower server rather
	// than a dead one. See docs/RESOURCE-BUDGET.md.
	if *memLimit > 0 {
		debug.SetMemoryLimit(*memLimit * 1 << 20)
	}

	if err := run(context.Background(), log, runConfig{
		dbPath:   *dbPath,
		addr:     *addr,
		name:     *name,
		scanNow:  *scanNow,
		probe:    *probe,
		sections: sections.specs,
	}); err != nil {
		log.Error("bms stopped", "error", err)
		os.Exit(1)
	}
}

type runConfig struct {
	dbPath   string
	addr     string
	name     string
	scanNow  bool
	probe    bool
	sections []string
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

	if cfg.scanNow {
		scanner := &scan.Scanner{DB: db, Log: log}
		if cfg.probe {
			prober, err := ffprobe.New("")
			if err != nil {
				log.Warn("no ffprobe; durations will be missing until one is installed", "error", err)
			} else {
				scanner.Prober = prober
			}
		}
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

	api := &plexapi.Server{DB: db, Log: log, Name: cfg.name, MachineID: machineID, Version: version}
	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout on purpose: a response here can be a two-hour film
		// streaming at the speed the client plays it, and a write deadline
		// would cut it off mid-sentence.
		IdleTimeout: 2 * time.Minute,
	}

	errs := make(chan error, 1)
	go func() {
		log.Info("serving", "addr", cfg.addr, "machineIdentifier", machineID, "version", version)
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
		if kind != domain.SectionMovies && kind != domain.SectionShows {
			return fmt.Errorf("section %q: kind must be movies or shows", title)
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
