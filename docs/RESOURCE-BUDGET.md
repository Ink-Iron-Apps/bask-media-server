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
| Admin web UI open | +0 MB server side | Static assets served from `go:embed`, no render process |

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
   in their own process group, are killed on context cancellation, and are
   reaped by an idle watchdog as well, because a killed parent is not a
   guarantee on Windows.
6. **A soft memory ceiling, not a crash.** `GOMEMLIMIT` is set from
   `--mem-limit` (default 256 MiB). Go's collector then works harder as the
   process approaches it instead of the kernel's OOM killer deciding how the
   evening ends.
7. **Few dependencies, and none that bring a runtime.** No ORM, no reflection
   framework, no Node at serve time, no embedded browser. SQLite is the pure-Go
   `modernc.org/sqlite`, which costs some speed against the cgo driver and buys
   a Windows build with no C toolchain anywhere.
8. **Images are files, not objects.** Thumbnails are generated to disk on first
   request under a concurrency semaphore, then served as files. A decoded image
   is never held past the request that made it.

## Proving it

- `go test ./...` runs `goleak` per package: goroutine leaks fail the build.
- `go test -run TestScanPeakMemory -tags bench ./internal/scan` scans a
  synthetic 100k-entry tree and asserts peak heap against the table above.
- `/debug/metrics` exposes heap, goroutines, open streams, cache occupancy and
  scan progress as plain expvar JSON; `/debug/pprof` is behind the admin token
  so a heap profile is one command away on the real library rather than a
  reproduction attempt.
- The server logs its own RSS hourly. A number in a log is how a slow leak gets
  noticed before it matters.
