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
- Posters, backdrops and summaries from TMDB, cached on disk under a byte cap
- Skip Intro and Skip Credits: markers read from a file's chapters, and
  measured from the audio a season's episodes share where there are none
- An admin web interface at `/admin`: libraries, scan and detection progress,
  accounts and tokens, served from the binary with nothing to install
- A web interface for watching at `/web`: libraries, search, Continue
  Watching, and a player with Skip Intro
- Conversion on the fly for a client that cannot play a file as it is, and
  direct play — the file itself, untouched — for every client that can

## Not yet

- Subtitles
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

Two catch-up passes are opt-in, because each one costs real time the first
time it runs and nothing afterwards:

```
# Posters, backdrops and summaries. Needs a TMDB key, from the flag or
# BMS_TMDB_API_KEY.
./bms -db ~/bms.db -enrich -tmdb-key "$TMDB_KEY"

# Skip Intro and Skip Credits. Needs ffmpeg; decodes the first and last ten
# minutes of each episode once, then compares each season's episodes.
./bms -db ~/bms.db -detect-markers
```

Both are resumable: a season or an item that has been looked at is not looked
at again, and a file that changes puts itself back in the queue.

## The admin interface

Open `http://<server>/admin` and sign in with an admin token — the one printed
on first run, or one issued from the Users page. There is no password store
here and no account service anywhere: the token is the credential, and signing
in exchanges it for a browser session that expires on its own.

From there:

- **Libraries.** Add one by browsing to its folder rather than typing a path,
  rename it, point it somewhere else, or remove it — removing a library
  forgets rows and never deletes a file. A folder that has gone away is
  reported on the row rather than quietly pruning the library on the next scan.
- **Activity.** Start a scan, a metadata pass or marker detection, watch what
  each one is doing, and stop one. Every pass commits as it goes, so stopping
  one keeps what it has already written.
- **Users.** Add accounts, make one an administrator, and issue or revoke a
  token per device. A token is shown once, because only its hash is stored.
  The last administrator cannot be deleted or demoted: there is nothing to
  recover from that but editing the database by hand.
- **Settings.** Rename the server — clients see it immediately and nothing
  else about the server changes — and save the TMDB key so the metadata pass
  can run without a restart.

It is one page, one stylesheet and one script compiled into the binary, so a
release is still a single file and there is no Node, no bundler and no
framework at serve time. Run with `-admin=false` to leave it off entirely.

## Watching in a browser

`http://<server>/web` is the same library, for watching rather than
administering. Any account signs in — not only an administrator — with the
token issued for that device, and gets a session of its own.

- Continue Watching, the libraries, and search across all of them.
- A show opens on its seasons and a season on its episodes; a film opens on
  Play, or on Resume where you left it.
- The player reports position as it goes, so stopping on a laptop and picking
  up on the television works in both directions, and each account's position
  is its own.
- Skip Intro and Skip Credits come from the markers the detector found, and
  the end of an episode offers the next one.

`-web=false` leaves this interface off.

## Direct play first, and conversion when there is no other way

The file itself is always the first answer. A direct play costs this server
nothing, keeps every frame and every channel the file has, and is what should
happen almost all of the time — so nothing is converted because of a guess.

How that is decided:

- **Clients say what they can play.** The web page asks the browser itself
  (`canPlayType`), so an MKV of H.264 and AAC is a direct play in a browser
  that opens Matroska, and is converted only where one does not. A client
  that says nothing — which is every Plex-protocol client today, Bask
  included — is taken at its word and gets the file.
- **Only what has to be converted is.** Dolby Digital inside an H.264 file
  costs the audio and leaves every frame of the picture untouched. A
  container that a client cannot open, with streams it can decode, is a
  *direct stream*: the packets are copied, nothing is re-encoded.
- **Conversion is bounded.** Three at a time by default
  (`-transcode-sessions`, and `0` turns it off), each one an ffmpeg in its own
  process group with an idle deadline: a viewer who closes a laptop stops
  costing a core within the minute, and stopping the server kills every one of
  them. See [docs/RESOURCE-BUDGET.md](docs/RESOURCE-BUDGET.md), rule 5.
- **Seeking does not wait.** A converted stream is handed out as HLS over a
  playlist written for the whole runtime, so the scrub bar is right from the
  first second, and seeking ahead moves the encoder to that point rather than
  encoding everything in between.

The player says which of the three happened — *Direct play*, *Direct stream*
or *Transcoding*, with the reason — because a server that is quietly
re-encoding everything is the thing people most want to be able to see.

With no ffmpeg, nothing is converted and a file the browser cannot decode is
reported as exactly that, with the file offered for download.

## Resource use

Resource use is a build constraint here, not an afterthought: see
[docs/RESOURCE-BUDGET.md](docs/RESOURCE-BUDGET.md) for the budget, the rules
that keep it, and how it is proved.
