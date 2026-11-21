# Resource budget

BMS is meant to sit on a machine that is also a desktop. A media server that
quietly eats a gigabyte is a media server that gets uninstalled, so the budget
below is a build constraint, not an aspiration, and the tests enforce the parts
that can be enforced.

## Targets

| State | Resident set | Notes |
|---|---|---|
| Idle, library loaded | < 60 MB | HTTP listener, SQLite, nothing else awake |
| Scanning 100k files | < 150 MB peak | Bounded workers, batched writes, no library-wide maps |
| Four concurrent direct plays | < 120 MB | Kernel does the work; we hold 32 KiB per stream |
| Detecting markers in a season | < 100 MB peak | One ffmpeg per worker, plus ~19 KiB of fingerprint per end of per episode |
| Admin web UI open | +0 MB server side | Static assets served from `go:embed`, no render process |
| Watching in a browser | Same as any direct play | The page is embedded; the video is `http.ServeContent` over the same 32 KiB buffer |
| One stream being converted | One ffmpeg, ~50 MB RSS, plus a core | Bounded by `-transcode-sessions` (3 by default); segments are files on disk, never buffers here |
| Background passes remembered | < 10 KB | The job history is capped at twenty finished jobs and holds counters, not output |
| Scanning 40k photographs | < 150 MB peak | The front of each file is read, never decoded: dimensions from the format's header, EXIF capped at 1 MiB |
| Scanning a music library | < 150 MB peak | One ffprobe per worker for the tags, plus at most one cover read per album folder, capped at 8 MiB |
| Making a photo thumbnail | < 170 MB per decode, bounded | One decoded bitmap at a time per resize slot (two by default), capped at 40 megapixels; the turn is applied to the thumbnail, not the original |
| Listening in a browser | Same as any direct play | An audio file is `http.ServeContent` over the same 32 KiB buffer |

Peak means peak: a scan that allocates 400 MB for a moment and settles back to
60 MB has failed this budget, because the machine had to find 400 MB.

## Rules that keep it there

1. **Nothing reads a whole file.** Media is served with `http.ServeContent`, so
   ranges, seeks and `If-Range` are the kernel's problem and ours is a 32 KiB
   copy buffer. No `io.ReadAll` on anything that came from a disk or a socket.
2. **No in-memory library.** SQLite is the only store of record. Queries stream
   with `sql.Rows` and are paged by the same `X-Plex-Container-Start` and
   `X-Plex-Container-Size` headers the clients already send, so a request for a
   30,000-item section never materialises 30,000 structs.
3. **Caches are bounded or absent.** A map with no eviction is the standard Go
   memory leak. Any cache states its cap in items and bytes at construction and
   is measured in the metrics endpoint; a cache that cannot state its cap does
   not get merged.
4. **Every goroutine has an owner and a context.** Tickers are stopped,
   channels are closed by their writers, and `goleak` runs in every package's
   `TestMain`, so a leaked goroutine fails CI rather than accumulating on a
   machine nobody is watching.
5. **Every subprocess dies with its request.** An abandoned ffmpeg is the
   biggest real-world resource leak in this class of software: the client walks
   away, the transcode keeps burning a core and filling a disk. Transcodes run
   in their own process group and are killed as a group — so a child of ffmpeg
   dies too — on three separate triggers: the client saying it has stopped, an
   idle watchdog that needs no cooperation from anybody, and the server
   shutting down. The watchdog is the one that matters, because a client that
   closes a laptop sends nothing at all, and because a killed parent is not a
   guarantee on Windows. `internal/transcode`'s tests assert all three by
   starting a process that spawns a child and checking both are gone.
6. **A soft memory ceiling, not a crash.** `GOMEMLIMIT` is set from
   `--mem-limit` (default 256 MiB). Go's collector then works harder as the
   process approaches it instead of the kernel's OOM killer deciding how the
   evening ends.
7. **Few dependencies, and none that bring a runtime.** No ORM, no reflection
   framework, no Node at serve time, no embedded browser. SQLite is the pure-Go
   `modernc.org/sqlite`, which costs some speed against the cgo driver and buys
   a Windows build with no C toolchain anywhere.
8. **Fingerprints are measured, bounded and cached on disk.** Marker detection
   reduces each end of a file to a 32-bit hash per 128 ms — about 19 KiB for
   ten minutes, against the ~100 MB those ten minutes are as PCM — and never
   holds the audio: ffmpeg is read in 32 KiB chunks and each frame is folded
   into a hash and dropped. One season is the working set, capped at 60
   episodes (~2.3 MB of fingerprints); the cache lives in SQLite, is keyed on
   each file's size and mtime, and is swept when a file no longer matches,
   which bounds it at two rows per file on disk and nothing in memory.
9. **Images are files, not objects.** Thumbnails are generated to disk on first
   request under a concurrency semaphore, then served as files. A decoded image
   is never held past the request that made it.
10. **A photo library is read, never decoded, and never copied.** A scan takes
    a photograph's dimensions from the format's header and its time and
    rotation from EXIF, both from the first megabyte of the file — so forty
    thousand photographs cost forty thousand short reads rather than forty
    thousand decodes. The originals stay in the library: only thumbnails are
    cached, which is both what keeps the artwork cache's cap meaningful and
    what stops a photo library costing twice its size on disk. The one place a
    photograph is decoded is making a thumbnail, which goes through the same
    semaphore and the same 40-megapixel refusal as a poster, and applies the
    EXIF turn to the finished thumbnail rather than to the original — fifty
    megabytes saved per rotated picture, on the picture every phone takes.
11. **A cover is read once per album, not once per track.** Both places a cover
    can be — a folder image, or the picture inside the files — are resolved per
    album folder and remembered for the length of the scan, so a sixty-track
    box set reads one picture. The parsers are bounded: at most 8 MiB of
    picture, and a length field claiming more is a corrupt file rather than an
    allocation to make.

12. **A negotiation probes one file, and only when the answer depends on it.**
    A client that sent no device profile and no bit rate ceiling is told about
    the file without ffprobe being run at all, and a request naming one media
    source has the others described from the database rather than measured. So
    a home screen full of items costs no subprocesses, and opening something
    costs one short probe of one file's header — not one per version, and not
    one per row on a shelf.
13. **A converted stream is addressable without the server remembering it.**
    What to encode travels in the query string of the playlist URL, clamped
    where it is read back, so there is no per-negotiation state to hold or
    expire. The only state is the ffmpeg itself, which is bounded by rule 5:
    capped in number, in its own process group, with an idle deadline, killed
    as a group, and freed early when a client says it has stopped.

14. **A restriction costs one predicate, not a second query path.**
    Narrowing a credential to some of the libraries hands the handlers a copy
    of the database handle carrying a set of section ids, and every query that
    returns a library, an item or a file appends `AND section_id IN (…)` to
    the statement it was already running. No second set of queries to keep in
    step with the first, no post-filtering of a page that would make a count
    wrong, and nothing cached per credential: the sets are a handful of
    integers read once per request alongside the token.

## Proving it

- `go test ./...` runs `goleak` per package: goroutine leaks fail the build.
- `go test -run TestScanPeakMemory -tags bench ./internal/scan` scans a
  synthetic 100k-entry tree and asserts peak heap against the table above.
- The admin interface's status page reports heap, goroutines, artwork cache
  occupancy against its cap, database size and what every background pass is
  doing, read from the same process rather than from a second accounting of
  it. A machine-readable `/debug/metrics` and a token-gated `/debug/pprof`
  are still to come; until they are, the page and the hourly log line are how
  a leak gets noticed.
- The server logs its own RSS hourly. A number in a log is how a slow leak gets
  noticed before it matters.
