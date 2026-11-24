package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/markers"
)

// Found is one thing a scan discovered on disk, in the scanner's terms.
//
// Deliberately flat: the scanner knows a path, what it parsed out of it, and
// where it belongs in a hierarchy by name. Turning that into rows — creating
// the show before the season before the episode — is this package's job,
// because only this package knows the identity index that makes it idempotent.
type Found struct {
	SectionID int64
	Kind      domain.Kind

	// Show and Season place an episode. Empty for a movie.
	Show       string
	ShowYear   int
	SeasonNum  int
	EpisodeNum int

	// Artist, Album, Disc and TrackNum place a track, and Performer is who
	// plays on it when that differs from the album's artist — which is the
	// useful half of a row on a compilation.
	Artist    string
	Performer string
	Album     string
	Disc      int
	TrackNum  int

	// AlbumPoster and ArtistPoster are artwork keys for the containers above
	// a track: a folder image beside the album, or the cover that was inside
	// the file. Applied only where the container has none, so one scan's
	// answer is not overwritten by the next file in the same folder.
	AlbumPoster  string
	ArtistPoster string

	// Albums places a photograph: the chain of album names from the library
	// root down to the folder it is in, outermost first.
	Albums []string

	// TakenAt is when a photograph was taken, which is what orders an album.
	TakenAt time.Time

	// Width, Height and Orientation describe a photograph's pixels and the
	// turn its EXIF asks for.
	Width       int
	Height      int
	Orientation int

	Title    string
	Year     int
	Duration time.Duration

	Path       string
	Size       int64
	Container  string
	ModifiedAt time.Time

	// Extra marks this file as something that belongs to another item: a
	// trailer, a deleted scene. Non-empty means Kind is KindExtra, and the
	// parent is named by Movie and MovieYear, or by Show and the season and
	// episode numbers where it belongs to television.
	Extra domain.ExtraKind

	// Movie and MovieYear name the film an extra belongs to. Empty for
	// everything that is not an extra of a film.
	Movie     string
	MovieYear int

	// ParentTitle is the title of the episode an extra belongs to, where the
	// extra's name said which one. The episode's own title rather than the
	// extra's, because that is what the episode's identity is built on — so
	// an extra scanned before the episode's own file lands on the same row
	// the episode will, rather than beside it.
	ParentTitle string

	// Edition is which cut of a film this file is, where the name says so.
	// Two editions are one item with two files, so this is what lets a
	// client offer the choice rather than showing two identical rows.
	Edition string

	// Chapters the file declares, if ffprobe found any. Written as scanned
	// facts and read again by marker detection, which is why a library with
	// labelled chapters gets Skip Intro out of a plain scan.
	Chapters []domain.Chapter

	// Genres are what the file's own tags say this is about, as a music file
	// carries them. Nil means the scan had nothing to say — no probe, or no
	// tag — and must not clear what is already stored; a film's genres come
	// from a provider instead, so this is empty for one.
	Genres []string

	// Subtitles are the streams inside the file and the sidecar files beside
	// it, as the scan found them. Nil means nothing was looked for — a scan
	// with no probe — and an empty slice means nothing was found, which are
	// different answers: the first must not delete what is already known.
	Subtitles []domain.Subtitle
}

// Writer applies scan results in batches inside one transaction.
//
// A scan of a large library is tens of thousands of tiny writes, and one
// transaction per file would fsync per file: the difference is minutes versus
// hours. One transaction for the whole scan would instead hold a write lock
// for the duration and grow the WAL without bound, so the batch is the middle
// ground — commit every BatchSize items, keep the WAL small, let readers in
// between batches.
type Writer struct {
	db    *DB
	batch int

	tx      *sql.Tx
	pending int
	parents map[string]int64 // show/season cache, reset per batch — see note in Add
	now     time.Time
	seen    map[string]struct{}
}

// NewWriter starts a batched writer. batch of 0 means the default of 500,
// which keeps a batch's transaction well under a megabyte of WAL for typical
// rows.
func (d *DB) NewWriter(batch int) *Writer {
	if batch <= 0 {
		batch = 500
	}
	return &Writer{
		db:      d,
		batch:   batch,
		parents: make(map[string]int64),
		now:     time.Now().UTC(),
		seen:    make(map[string]struct{}),
	}
}

// Add records one discovery, opening a transaction if none is open and
// committing when the batch is full.
//
// The parent cache is keyed by section/show/season rather than by row id, and
// it is intentionally not cleared between batches: a library's shows are a
// small set — a few hundred at most — while its episodes are the many, so this
// is a bounded cache by the shape of the data, not by hope. A library with
// 50,000 distinct show names would be the exception that needs an LRU, and the
// metrics endpoint reports the occupancy so that case is visible rather than
// theoretical.
func (w *Writer) Add(ctx context.Context, f Found) error {
	if err := w.begin(ctx); err != nil {
		return err
	}
	if err := w.add(ctx, f); err != nil {
		return err
	}
	w.pending++
	if w.pending >= w.batch {
		return w.Commit(ctx)
	}
	return nil
}

// AddAll records a batch of discoveries in one transaction and commits it.
//
// This is what a scan uses, and the difference from calling [Writer.Add] in a
// loop is the only thing that matters about it: the transaction is open for
// the writes and nothing else.
//
// Add leaves a transaction open between calls, which is correct when the
// caller has the next row ready and catastrophic when it does not. A scan
// probes its files on worker goroutines and reads them off a channel, so a
// loop over Add holds SQLite's single write lock while it waits for ffprobe —
// for a batch of 500 files that is minutes, during which every other writer
// in the process fails with SQLITE_BUSY once the busy timeout expires. On a
// real library that meant the admin interface could not even create a
// session: signing in is a write.
//
// So the caller gathers a batch first and hands it over whole. The lock is
// then held for a few hundred inserts with no I/O of any kind between them.
func (w *Writer) AddAll(ctx context.Context, found []Found) error {
	if len(found) == 0 {
		return nil
	}
	if err := w.begin(ctx); err != nil {
		return err
	}
	for _, f := range found {
		if err := w.add(ctx, f); err != nil {
			return err
		}
		w.pending++
	}
	return w.Commit(ctx)
}

// add writes one discovery inside whatever transaction is already open.
func (w *Writer) add(ctx context.Context, f Found) error {
	spec := itemSpec{
		sectionID: f.SectionID,
		kind:      f.Kind,
		title:     f.Title,
		year:      f.Year,
		duration:  f.Duration,
	}
	switch f.Kind {
	case domain.KindEpisode:
		showID, err := w.ensureContainer(ctx, itemSpec{
			sectionID: f.SectionID, kind: domain.KindShow, title: f.Show, year: f.ShowYear,
		})
		if err != nil {
			return err
		}
		seasonID, err := w.ensureContainer(ctx, itemSpec{
			sectionID: f.SectionID, kind: domain.KindSeason, parentID: &showID,
			title: fmt.Sprintf("Season %d", f.SeasonNum), index: f.SeasonNum,
		})
		if err != nil {
			return err
		}
		spec.parentID, spec.index = &seasonID, f.EpisodeNum
	case domain.KindTrack:
		// An artist with no picture of their own shows one of their covers,
		// which is what every music client does when an agent has no artist
		// art: a library whose top level is a wall of blank squares is a
		// worse answer than one that borrows from the album below it.
		artistPoster := f.ArtistPoster
		if artistPoster == "" {
			artistPoster = f.AlbumPoster
		}
		artistID, err := w.ensureContainer(ctx, itemSpec{
			sectionID: f.SectionID, kind: domain.KindArtist, title: f.Artist,
			poster: artistPoster,
		})
		if err != nil {
			return err
		}
		albumID, err := w.ensureContainer(ctx, itemSpec{
			sectionID: f.SectionID, kind: domain.KindAlbum, parentID: &artistID,
			title: f.Album, year: f.Year, poster: f.AlbumPoster,
		})
		if err != nil {
			return err
		}
		// A track's own year is the album's; carrying it twice would make two
		// rows of one track when a file disagrees with its neighbours.
		spec.parentID, spec.index, spec.disc, spec.year = &albumID, f.TrackNum, f.Disc, 0
		spec.performer = f.Performer
	case domain.KindPhoto:
		// The folder tree, one album per level, which is the hierarchy
		// somebody already built by filing their photographs.
		var parent *int64
		for _, name := range f.Albums {
			id, err := w.ensureContainer(ctx, itemSpec{
				sectionID: f.SectionID, kind: domain.KindPhotoAlbum, parentID: parent, title: name,
			})
			if err != nil {
				return err
			}
			parent = &id
		}
		spec.parentID, spec.takenAt = parent, f.TakenAt
	case domain.KindMovie:
		// No parent.
	case domain.KindExtra:
		parentID, err := w.extraParent(ctx, f)
		if err != nil {
			return err
		}
		spec.parentID, spec.extra = parentID, f.Extra
		// Never a year of its own: an extra's year would put it in the
		// identity index beside a differently-dated copy of itself, and
		// nobody is looking for the release year of a featurette.
		spec.year = 0
	default:
		return fmt.Errorf("scanner produced an unplayable kind: %s", f.Kind)
	}

	itemID, err := w.upsertItem(ctx, spec)
	if err != nil {
		return err
	}
	parentID := spec.parentID
	partID, changed, err := w.upsertPart(ctx, itemID, f)
	if err != nil {
		return err
	}
	// A season whose files changed is new evidence about where its intro is,
	// and a newly added episode has no marker at all yet, so the audio pass
	// has to look at this season again. Cleared here rather than in the pass
	// because this is the only place that knows a file moved.
	if changed && parentID != nil && f.Kind == domain.KindEpisode {
		if _, err := w.tx.ExecContext(ctx, `UPDATE items SET markers_at = 0 WHERE id = ?`, *parentID); err != nil {
			return fmt.Errorf("reopen season for detection: %w", err)
		}
	}
	if err := w.applyGenres(ctx, itemID, spec, f); err != nil {
		return err
	}
	if err := w.applyChapters(ctx, itemID, f); err != nil {
		return err
	}
	if err := w.applySubtitles(ctx, itemID, partID, f); err != nil {
		return err
	}

	w.seen[f.Path] = struct{}{}
	return nil
}

// applyGenres records what the file's tags say this is about, and gives the
// containers above it the same words where they have none.
//
// Skipped entirely when the scan had nothing to say, for the reason on
// [Found.Genres]: a scan without a probe must not erase a library's genres.
//
// The track gets exactly its tag's genres; the album and the artist above it
// get them only if they are empty. Replacing a container's genres per track
// would leave an album filed under whichever track the walk reached last,
// and a compilation under whichever genre its final song happened to be.
func (w *Writer) applyGenres(ctx context.Context, itemID int64, spec itemSpec, f Found) error {
	if len(f.Genres) == 0 {
		return nil
	}
	if err := replaceGenresTx(ctx, w.tx, itemID, f.Genres); err != nil {
		return err
	}
	if spec.parentID == nil {
		return nil
	}
	if err := addGenresTx(ctx, w.tx, *spec.parentID, f.Genres); err != nil {
		return err
	}
	// And the level above that, which is the artist over an album. Read
	// rather than remembered, because the writer's parent cache holds ids
	// and not the chain between them.
	var grandparent sql.NullInt64
	if err := w.tx.QueryRowContext(ctx,
		`SELECT parent_id FROM items WHERE id = ?`, *spec.parentID).Scan(&grandparent); err != nil {
		return fmt.Errorf("read the level above: %w", err)
	}
	if !grandparent.Valid {
		return nil
	}
	return addGenresTx(ctx, w.tx, grandparent.Int64, f.Genres)
}

// applyChapters stores what the file said and what that implies.
//
// Only when the probe actually returned chapters: a scan without ffprobe, or
// of a file that declares none, must not delete the chapters an earlier scan
// recorded. A file whose chapters really went away comes back as a changed
// file, and the next probe that reads it replaces them.
func (w *Writer) applyChapters(ctx context.Context, itemID int64, f Found) error {
	if len(f.Chapters) == 0 {
		return nil
	}
	chapters := make([]domain.Chapter, len(f.Chapters))
	copy(chapters, f.Chapters)
	for i := range chapters {
		chapters[i].ItemID = itemID
	}
	if err := replaceChaptersTx(ctx, w.tx, itemID, chapters); err != nil {
		return err
	}
	return replaceMarkersTx(ctx, w.tx, itemID, domain.SourceChapter, markers.FromChapters(chapters))
}

// applySubtitles records what this file's subtitles are now.
//
// Skipped entirely when the scan looked for none — no probe, so no embedded
// streams, and the walk did not list the folder — because an empty answer
// from a scan that did not ask would delete sidecars that are still there.
func (w *Writer) applySubtitles(ctx context.Context, itemID, partID int64, f Found) error {
	if f.Subtitles == nil {
		return nil
	}
	subs := make([]domain.Subtitle, len(f.Subtitles))
	copy(subs, f.Subtitles)
	for i := range subs {
		subs[i].ItemID, subs[i].PartID = itemID, partID
	}
	return replaceSubtitlesTx(ctx, w.tx, itemID, partID, subs)
}

// Commit flushes the open batch, if any.
func (w *Writer) Commit(ctx context.Context) error {
	if w.tx == nil {
		return nil
	}
	tx := w.tx
	w.tx, w.pending = nil, 0
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit batch: %w", err)
	}
	return nil
}

// Rollback abandons the open batch. Safe to call after Commit.
func (w *Writer) Rollback() {
	if w.tx == nil {
		return
	}
	w.tx.Rollback()
	w.tx, w.pending = nil, 0
}

// Seen reports the paths this writer has accepted, which is what a sweep for
// vanished files compares against.
func (w *Writer) Seen() map[string]struct{} { return w.seen }

func (w *Writer) begin(ctx context.Context) error {
	if w.tx != nil {
		return nil
	}
	tx, err := w.db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin batch: %w", err)
	}
	w.tx = tx
	return nil
}

// itemSpec is one row as the scanner's discovery implies it.
//
// A struct rather than nine positional arguments: the kinds differ in which
// of these they set — an episode has an index, a track an index and a disc, a
// photograph a time and neither — and a call site that names its fields is
// one that cannot quietly pass a disc number as a year.
type itemSpec struct {
	sectionID int64
	kind      domain.Kind
	parentID  *int64
	title     string
	year      int
	index     int
	disc      int
	duration  time.Duration
	takenAt   time.Time
	performer string
	extra     domain.ExtraKind

	// poster is artwork for a container the scan found rather than fetched —
	// an album's cover. Applied only when the row has none, so the first file
	// in a folder decides and the rest cost nothing.
	poster string
}

// ensureContainer finds or creates a show, season, artist, album or photo
// album and returns its id.
func (w *Writer) ensureContainer(ctx context.Context, spec itemSpec) (int64, error) {
	cacheKey := fmt.Sprintf("%d/%s/%v/%s/%d/%d/%d",
		spec.sectionID, spec.kind, derefID(spec.parentID), spec.title, spec.year, spec.index, spec.disc)
	if id, ok := w.parents[cacheKey]; ok {
		if spec.poster != "" {
			if err := w.applyPoster(ctx, id, spec.poster); err != nil {
				return 0, err
			}
		}
		return id, nil
	}
	id, err := w.upsertItem(ctx, spec)
	if err != nil {
		return 0, err
	}
	w.parents[cacheKey] = id
	return id, nil
}

// extraParent is the item an extra belongs to, created if the scan has not
// reached it yet.
//
// Order is not guaranteed: a walk can reach "Trailers/teaser.mkv" before the
// film's own file, so the parent is ensured the same way a season is. The
// identity index means the film's own file then lands on this very row rather
// than beside it.
//
// An extra whose parent cannot be named is refused rather than filed
// somewhere arbitrary: a trailer attached to the wrong film is worse than one
// that was skipped and logged.
func (w *Writer) extraParent(ctx context.Context, f Found) (*int64, error) {
	switch {
	case f.Show != "" && f.SeasonNum > 0 && f.EpisodeNum > 0:
		showID, err := w.ensureContainer(ctx, itemSpec{
			sectionID: f.SectionID, kind: domain.KindShow, title: f.Show, year: f.ShowYear,
		})
		if err != nil {
			return nil, err
		}
		seasonID, err := w.ensureContainer(ctx, itemSpec{
			sectionID: f.SectionID, kind: domain.KindSeason, parentID: &showID,
			title: fmt.Sprintf("Season %d", f.SeasonNum), index: f.SeasonNum,
		})
		if err != nil {
			return nil, err
		}
		// The episode, by the identity its own file will produce: the name
		// of an extra like "S01E01 - Part 1-deleted.mkv" reads as the
		// episode it belongs to, so the row is ensured rather than waited
		// for — a walk reaches the extra before the episode as often as not,
		// because a hyphen sorts before a dot.
		if f.ParentTitle != "" {
			episodeID, err := w.ensureContainer(ctx, itemSpec{
				sectionID: f.SectionID, kind: domain.KindEpisode, parentID: &seasonID,
				title: f.ParentTitle, index: f.EpisodeNum,
			})
			if err != nil {
				return nil, err
			}
			return &episodeID, nil
		}
		// Nothing in the name said which episode, so the number is all there
		// is. An episode that has not been scanned leaves the extra on the
		// series, which is where somebody would look for it anyway.
		episodeID, found, err := w.episodeNumbered(ctx, seasonID, f.EpisodeNum)
		if err != nil {
			return nil, err
		}
		if found {
			return &episodeID, nil
		}
		return &showID, nil
	case f.Show != "":
		showID, err := w.ensureContainer(ctx, itemSpec{
			sectionID: f.SectionID, kind: domain.KindShow, title: f.Show, year: f.ShowYear,
		})
		if err != nil {
			return nil, err
		}
		return &showID, nil
	case f.Movie != "":
		movieID, err := w.ensureContainer(ctx, itemSpec{
			sectionID: f.SectionID, kind: domain.KindMovie, title: f.Movie, year: f.MovieYear,
		})
		if err != nil {
			return nil, err
		}
		return &movieID, nil
	}
	return nil, fmt.Errorf("an extra with nothing to belong to: %s", f.Path)
}

// episodeNumbered is the episode at this number in this season, if the scan
// has written it.
func (w *Writer) episodeNumbered(ctx context.Context, seasonID int64, number int) (int64, bool, error) {
	var id int64
	err := w.tx.QueryRowContext(ctx,
		`SELECT id FROM items WHERE kind = 'episode' AND parent_id = ? AND idx = ? AND extra = ''
		 ORDER BY id LIMIT 1`, seasonID, number).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("look up episode: %w", err)
	}
	return id, true, nil
}

// applyPoster gives a container artwork it does not already have.
func (w *Writer) applyPoster(ctx context.Context, itemID int64, key string) error {
	if key == "" {
		return nil
	}
	if _, err := w.tx.ExecContext(ctx,
		`UPDATE items SET poster_key = ?, updated_at = ? WHERE id = ? AND poster_key = ''`,
		key, millis(w.now), itemID); err != nil {
		return fmt.Errorf("set poster: %w", err)
	}
	return nil
}

// upsertItem is idempotent on the identity index, so a rescan of an unchanged
// library produces no new rows and no churn in updated_at.
func (w *Writer) upsertItem(ctx context.Context, spec itemSpec) (int64, error) {
	var parent any
	if spec.parentID != nil {
		parent = *spec.parentID
	}

	var id int64
	// Found by what the scan found, not by what is displayed: an item whose
	// title was corrected in the interface is still this file's item, and
	// looking it up by the corrected title would insert a second row.
	err := w.tx.QueryRowContext(ctx,
		`SELECT id FROM items WHERE section_id = ? AND kind = ? AND IFNULL(parent_id, 0) = IFNULL(?, 0)
		   AND scanned_title = ? AND scanned_year = ? AND idx = ? AND disc = ?`,
		spec.sectionID, string(spec.kind), parent, spec.title, spec.year, spec.index, spec.disc).Scan(&id)
	switch {
	case err == nil:
		if spec.duration > 0 {
			if _, err := w.tx.ExecContext(ctx,
				`UPDATE items SET duration_ms = ?, updated_at = ? WHERE id = ? AND duration_ms <> ?`,
				spec.duration.Milliseconds(), millis(w.now), id, spec.duration.Milliseconds()); err != nil {
				return 0, fmt.Errorf("update duration: %w", err)
			}
		}
		if !spec.takenAt.IsZero() {
			if _, err := w.tx.ExecContext(ctx,
				`UPDATE items SET taken_at = ? WHERE id = ? AND taken_at <> ?`,
				millis(spec.takenAt), id, millis(spec.takenAt)); err != nil {
				return 0, fmt.Errorf("update taken at: %w", err)
			}
		}
		if spec.performer != "" {
			if _, err := w.tx.ExecContext(ctx,
				`UPDATE items SET performer = ? WHERE id = ? AND performer <> ?`,
				spec.performer, id, spec.performer); err != nil {
				return 0, fmt.Errorf("update performer: %w", err)
			}
		}
		if spec.extra != "" {
			// A file moved from "Featurettes" to "Trailers" is the same
			// extra, resorted. Written here so a rescan picks that up rather
			// than leaving it filed where it used to be.
			if _, err := w.tx.ExecContext(ctx,
				`UPDATE items SET extra = ? WHERE id = ? AND extra <> ?`,
				string(spec.extra), id, string(spec.extra)); err != nil {
				return 0, fmt.Errorf("update extra: %w", err)
			}
		}
		if err := w.applyPoster(ctx, id, spec.poster); err != nil {
			return 0, err
		}
		return id, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("look up item: %w", err)
	}

	res, err := w.tx.ExecContext(ctx,
		`INSERT INTO items (section_id, kind, parent_id, title, sort_title, year, idx, disc,
		    duration_ms, added_at, updated_at, taken_at, poster_key, performer,
		    scanned_title, scanned_year, extra)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		spec.sectionID, string(spec.kind), parent, spec.title, SortTitle(spec.title),
		spec.year, spec.index, spec.disc,
		spec.duration.Milliseconds(), millis(w.now), millis(w.now), millis(spec.takenAt),
		spec.poster, spec.performer, spec.title, spec.year, string(spec.extra))
	if err != nil {
		return 0, fmt.Errorf("insert item: %w", err)
	}
	id, err = res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("insert item: %w", err)
	}
	return id, nil
}

// upsertPart records the file, and leaves the row untouched when nothing about
// it has changed. That is what makes a rescan cost a stat per file.
//
// Reports whether the row actually moved, which is how the scan knows a season
// is worth detecting markers for again.
func (w *Writer) upsertPart(ctx context.Context, itemID int64, f Found) (int64, bool, error) {
	res, err := w.tx.ExecContext(ctx,
		`INSERT INTO parts (item_id, path, size, container, modified_at, width, height, orientation, edition)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET
		   item_id = excluded.item_id,
		   size = excluded.size,
		   container = excluded.container,
		   modified_at = excluded.modified_at,
		   width = excluded.width,
		   height = excluded.height,
		   orientation = excluded.orientation,
		   edition = excluded.edition
		 WHERE parts.size <> excluded.size OR parts.modified_at <> excluded.modified_at
		    OR parts.item_id <> excluded.item_id OR parts.width <> excluded.width
		    OR parts.height <> excluded.height OR parts.orientation <> excluded.orientation
		    OR parts.edition <> excluded.edition`,
		itemID, f.Path, f.Size, f.Container, millis(f.ModifiedAt), f.Width, f.Height, f.Orientation, f.Edition)
	if err != nil {
		return 0, false, fmt.Errorf("upsert part: %w", err)
	}
	affected, err := res.RowsAffected()
	changed := err != nil || affected > 0 // a driver that cannot say assumes it changed

	// The id is read back rather than taken from the insert, because an
	// unchanged row is not written at all and has no last insert id: the path
	// is the identity here, which is what the unique index on it is for.
	var partID int64
	if err := w.tx.QueryRowContext(ctx, `SELECT id FROM parts WHERE path = ?`, f.Path).Scan(&partID); err != nil {
		return 0, changed, fmt.Errorf("read part id: %w", err)
	}
	return partID, changed, nil
}

// PrunePaths removes parts whose files are no longer on disk, and then any
// item left with no parts and no children.
//
// Deletes in bounded chunks: a library that lost a whole drive would otherwise
// build one statement with 40,000 placeholders.
func (d *DB) PrunePaths(ctx context.Context, sectionID int64, keep map[string]struct{}) (int, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT p.id, p.path FROM parts p JOIN items i ON i.id = p.item_id WHERE i.section_id = ?`, sectionID)
	if err != nil {
		return 0, fmt.Errorf("list paths: %w", err)
	}

	var stale []int64
	for rows.Next() {
		var (
			id   int64
			path string
		)
		if err := rows.Scan(&id, &path); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan path: %w", err)
		}
		if _, ok := keep[path]; !ok {
			stale = append(stale, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(stale) == 0 {
		return 0, nil
	}

	const chunk = 200
	for start := 0; start < len(stale); start += chunk {
		end := min(start+chunk, len(stale))
		batch := stale[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		args := make([]any, 0, len(batch))
		for _, id := range batch {
			args = append(args, id)
		}
		if _, err := d.sql.ExecContext(ctx, `DELETE FROM parts WHERE id IN (`+placeholders+`)`, args...); err != nil {
			return 0, fmt.Errorf("delete parts: %w", err)
		}
	}

	// Everything that is a file with no file left, then everything that
	// gathers things with nothing left to gather. Deepest first, and repeated
	// until a pass changes nothing: shows are two levels deep, music is two,
	// and a tree of photo albums is as deep as somebody's folders go.
	if _, err := d.sql.ExecContext(ctx,
		`DELETE FROM items WHERE section_id = ? AND kind IN ('movie','episode','track','photo')
		   AND NOT EXISTS (SELECT 1 FROM parts WHERE parts.item_id = items.id)`, sectionID); err != nil {
		return 0, fmt.Errorf("prune leaves: %w", err)
	}
	// Bounded so a cycle in parent_id — which the schema should make
	// impossible — cannot turn tidying up into a loop that never ends.
	for range maxTreeDepth {
		res, err := d.sql.ExecContext(ctx,
			`DELETE FROM items WHERE section_id = ? AND kind IN ('season','show','album','artist','photoalbum')
			   AND NOT EXISTS (SELECT 1 FROM items AS child WHERE child.parent_id = items.id)`, sectionID)
		if err != nil {
			return 0, fmt.Errorf("prune containers: %w", err)
		}
		if affected, err := res.RowsAffected(); err == nil && affected == 0 {
			break
		}
	}
	return len(stale), nil
}

// maxTreeDepth bounds how deep a library's own hierarchy is taken to go. Two
// is television and music; the rest is headroom for a photo library filed by
// year, month, day and event.
const maxTreeDepth = 16

// SortTitle is Title with a leading article dropped and case folded, so "The
// Martian" files under M where a viewer looks for it.
func SortTitle(title string) string {
	lower := strings.ToLower(strings.TrimSpace(title))
	for _, article := range []string{"the ", "a ", "an "} {
		if strings.HasPrefix(lower, article) {
			return strings.TrimSpace(lower[len(article):])
		}
	}
	return lower
}

func derefID(id *int64) int64 {
	if id == nil {
		return 0
	}
	return *id
}
