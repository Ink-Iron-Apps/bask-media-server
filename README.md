# Bask Media Server

A media server that belongs to whoever runs it. One Go binary, one SQLite file,
no runtime to install and no account service anywhere.

It serves the library in the shape Plex clients expect, so Bask — and anything
else that speaks that dialect — works against it today. That is a presenter,
not the architecture: BMS's own API is a second presenter over the same domain,
and the Plex vocabulary never reaches storage or scanning.

## What works now

- Movie and show libraries scanned from the folder layout Bask's downloader and
  the usual renamers already produce
- Sections, items, seasons, episodes, search and Continue Watching
- Playback by direct play, with HTTP range requests so seeking works
- Playback progress, resume offsets and watched state, per user
- Local accounts and tokens; the first run prints an admin token once

## Not yet

- Transcoding (direct play only)
- Artwork and external metadata providers
- The admin web interface
- Music and photos

## Running it

```
go build ./cmd/bms
./bms -db ~/bms.db -addr 127.0.0.1:32500 \
      -section "Movies:movies:/mnt/media/Movies" \
      -section "TV Shows:shows:/mnt/media/TV Shows" \
      -scan
```

The first start prints the admin token. Point a client at the address and give
it that token.

## Resource use

Resource use is a build constraint here, not an afterthought: see
[docs/RESOURCE-BUDGET.md](docs/RESOURCE-BUDGET.md) for the budget, the rules
that keep it, and how it is proved.
