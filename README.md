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
- Libraries per account and per device: an account is given some of the
  libraries, and one of its devices can be narrowed further still
- Favourites, playlists and collections, on both APIs and in the browser
- Two cuts of a film as one film with two versions, named so a client can
  offer the choice
- Trailers and extras: the `Trailers/`, `Featurettes/` and `Deleted Scenes/`
  folders and the `-trailer` filenames, offered beside what they belong to on
  all three surfaces instead of becoming rows in the library
- Hardware encoding where the machine has it: VAAPI, NVENC, QSV,
  VideoToolbox or AMF, proved on the machine before it is used
- Now Playing in the admin interface: who is watching what, on which device,
  how far in, and whether anything is being converted — with a button that
  stops a stream and tells the client why
- Posters, backdrops and summaries from TMDB, cached on disk under a byte cap
- Fixing what the provider got wrong: correct a title, year or summary by
  hand, or pick the right entry out of the provider's list. A corrected item
  is left alone by later metadata passes, and the filename is still what a
  scan recognises it by
- Skip Intro and Skip Credits: markers read from a file's chapters, and
  measured from the audio a season's episodes share where there are none
- An admin web interface at `/admin`: libraries, scan and detection progress,
  accounts and tokens, served from the binary with nothing to install
- A web interface for watching at `/web`: libraries, search, Continue
  Watching, and a player with Skip Intro
- Conversion on the fly for a client that cannot play a file as it is, and
  direct play — the file itself, untouched — for every client that can
- Subtitles: the sidecar files beside the media and the streams inside it,
  with language, forced and SDH read from where they are written
- Music libraries: artists, albums and tracks from the files' own tags, with
  covers from the folder or from inside the files, and an album that plays in
  the browser while the library is browsed
- Photo libraries: the folder tree as albums, EXIF for when a photograph was
  taken and which way up it goes, and thumbnails made from the originals
  where they are
- A Jellyfin-compatible API, so a client can be pointed straight at this
  server by address — no account service in the middle

## Not yet

- Nothing from the list this project set out with. What comes next is whatever
  the household asks for.

## Running it

```
go build ./cmd/bms
./bms -db ~/bms.db -addr 127.0.0.1:32500 \
      -section "Movies:movies:/mnt/media/Movies" \
      -section "TV Shows:shows:/mnt/media/TV Shows" \
      -section "Music:music:/mnt/media/Music" \
      -section "Photos:photos:/mnt/media/Pictures" \
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
- **Libraries per device.** Under each token is a row of libraries, and the
  ones ticked are the only ones that credential can reach. Ticking all of them
  is stored as no restriction at all, so a library added next month is visible
  to that device rather than quietly missing from it. Beside the label a human
  typed is what the client last called itself, which is how a list of
  credentials reads as a list of devices.
- **Settings.** Rename the server — clients see it immediately and nothing
  else about the server changes — and save the TMDB key so the metadata pass
  can run without a restart.

It is one page, one stylesheet and one script compiled into the binary, so a
release is still a single file and there is no Node, no bundler and no
framework at serve time. Run with `-admin=false` to leave it off entirely.

## Who sees which libraries

Two settings, and a request has to pass both.

**An account** is given some of the libraries, which is the Plex-shaped
question: this person gets the films and the music, that person does not get
the documentaries. Set under Users.

**A device** can be narrowed further, because a token is BMS's per-device
credential — the thing you can revoke for one television without touching
anybody else. Set under that account's Tokens. No second notion of a device is
invented, and nothing depends on the device id a client sends, which the
client writes itself and which changes when somebody reinstalls the app.

The two are combined rather than ranked: a device reaches a library only if
its account allows it *and* its own grants allow it. So a token cannot be used
to widen an account, and two grants that overlap nowhere — the account allowed
the films, the television allowed the music — mean that television reaches
neither. Contradictory instructions resolve to less access, never more.

Either setting left at every library is stored as no restriction at all, so a
library added next month is visible rather than quietly missing.

A restriction applies to every answer, not only the list of libraries:

- The libraries the client is told about, on both APIs.
- Anything addressed by id afterwards. A hidden film is a 404 for its
  metadata, its poster, its playback negotiation, its direct stream and its
  converted stream, and a Plex client's request for the file by part id is a
  404 too — that one never passes through the item it belongs to, so it is the
  door that would otherwise be left open.
- Search, the home screen rows, Next Up and Continue Watching, which are built
  across every library at once and are the other place a hidden library leaks.
- The web interface, which signs in with a token and inherits its restriction,
  so opening a browser on that television shows the same libraries the app on
  it shows.

What it does not do is hide anything from the admin interface, which
administers the server rather than browsing a library.

Two things worth knowing before using it:

- **One token per device.** A token shared between a phone and a television
  cannot tell them apart. Issue each device its own — the admin interface has
  always suggested it, and this is what makes it pay.
- **It is a setting, not a deletion.** Ticking the library back on makes
  everything reachable again, immediately, including a session already open.

Administrators are not narrowed: an administrator administers the server
rather than browsing it, so the setting is not offered for one.

## Watching in a browser

`http://<server>/web` is the same library, for watching rather than
administering. Any account signs in — not only an administrator — with the
token issued for that device, and gets a session of its own.

- Where you left off, the libraries, and search across all of them.
- A show opens on its seasons and a season on its episodes; a film opens on
  Play, or on Resume where you left it.
- The player reports position as it goes, so stopping on a laptop and picking
  up on the television works in both directions, and each account's position
  is its own.
- Skip Intro and Skip Credits come from the markers the detector found, and
  the end of an episode offers the next one.
- An album plays in a bar along the bottom, and a photograph opens over the
  album it is in, with the arrow keys moving through it.

`-web=false` leaves this interface off.

## Subtitles

Both kinds, found by scanning rather than by being told:

- **Beside the file.** `Film (2002).en.srt`, `Film (2002).eng.forced.srt`,
  `Film (2002) - English.srt` — the conventions the renamers in use actually
  write. The language, "forced" and SDH are read out of the name; anything
  else after the title becomes the track's own label, so a commentary track
  is labelled rather than mislabelled.
- **Inside the file.** Every subtitle stream the container declares, with the
  language and the dispositions it carries, read by the same probe that
  measures the file during a scan.

A client is told about both in the shape Plex clients expect: a `Stream` on
the `Part`, with the sidecars carrying a key and the embedded ones not, which
is how a client tells them apart. The text is served from that key in the
format asked for — `.srt` for Bask, `.vtt` for a browser — and an embedded
stream is pulled out of the container on request, which Plex itself refuses
to do.

In the browser they are `<track>` elements, so the browser parses, positions
and times the cues, and the player has a menu to choose one or turn them off.
PGS and VobSub are pictures of words rather than text: they are listed, marked
as pictures, and left to a player that can draw them, which is what a direct
play in Bask does.

## Music

A music library is scanned the other way round from a film one. A film says
what it is in its path, because the tool that fetched it wrote the name there;
a music file carries tags written by whoever published it, and those beat any
reading of `01 - track.mp3`. So the tags decide, the path fills the gaps, and
a file with neither is still filed — under "Unknown Artist", where somebody
can find it — rather than dropped.

- **Artists, albums and tracks**, from `album_artist`, `album`, `track` and
  `disc`. The album artist is what keeps a compilation together instead of
  scattering it across twenty artists, and the performer is kept per track,
  which is the only thing that makes a row of *Now 42* readable.
- **Both multi-disc layouts**: a folder per disc (`CD1`, `Disc 2`) and a
  filename that carries both (`2-05 Closing Time.mp3`). The disc is part of a
  track's identity, because the two discs of an album both have a track 1 and
  nothing else about them differs.
- **Covers from either place.** A `cover.jpg`, `folder.jpg` or the one picture
  in a folder of music is the album's; where there is none, the picture inside
  the files themselves is read — ID3's `APIC`, FLAC's picture block, an MP4
  `covr` atom — because anything ripped from a streaming service has the cover
  only there, and a wall of blank squares is the wrong answer when the picture
  has been there all along. No ffmpeg is needed for any of it.
- **In the browser**, an album plays in a bar along the bottom while the
  library is still being browsed, which is the whole difference between
  listening and watching. It reports position like the video player, so a
  half-heard track resumes where it was.
- **For a client**, tracks are Plex `Track` elements under `album` under
  `artist`, with the album and the artist on every row so a music client needs
  no second request to draw a list.

Playback is the same decision as for video: the browser says what it can
decode and the file is sent untouched wherever it can be. FLAC, MP3, AAC, Opus
and Vorbis play as they are in a current browser; the formats MP4 cannot carry
— WMA, Monkey's Audio, WavPack — are re-encoded, which for audio costs a
fraction of a core.

## Photographs

The folder tree *is* the metadata. People file photographs in folders that
mean something — a year, a holiday, a person — and anything cleverer would be
a guess over the top of an answer somebody already gave. Every level of the
tree is an album, as deep as it goes, and a loose file at the top of the
library is put in an album named after the library.

- **When it was taken** comes from EXIF `DateTimeOriginal`, falling back to the
  file's mtime, and an album is ordered by it: a folder of holiday pictures
  belongs in the order they happened, not the order of their filenames.
- **Which way up** comes from EXIF `Orientation`, and is applied when a
  thumbnail is made. Without it every photograph taken on an upright phone
  arrives on its side.
- **Nothing is decoded during a scan.** The dimensions come out of the
  format's header and the rest out of the EXIF segment, so a camera roll of
  forty thousand files costs a read of the front of each one.
- **Originals stay where they are.** Only thumbnails are cached, which is what
  keeps the cache's byte cap meaningful: a copy of a photo library would
  double what it costs on disk for pictures that are already on the disk.
- **HEIC and AVIF** are listed and served as they are. Go cannot decode them,
  so they have no dimensions and no thumbnail of our making — a phone and a
  current browser display them directly, and a library that had silently
  omitted half a camera roll would be the worse answer.

Thumbnail caches are skipped by name (`@eaDir`, `.thumbnails` and the hidden
folders), or a library would gain a second copy of itself at a quarter of the
size.

## Two APIs, one library

BMS answers in two vocabularies over the same rows.

The **Plex-compatible API** is at the root, and it is what Bask's existing
client speaks. Its weakness is not the protocol: it is that a Plex client
finds servers through plex.tv, which a server belonging to the person running
it is never going to be on.

The **Jellyfin-compatible API** is the way in that does not need anybody's
account service. A client is told an address and a user, which is the shape a
self-hosted server actually has — so this is what makes BMS reachable from
Bask, from Findroid, from a browser, from anything that speaks it.

```
http://<server>:32500          the address a client is given
Username                       the account's name
Password                       the token issued for that device
```

The password field carries the token, which is deliberate: BMS already has a
credential per device that can be revoked on its own, and inventing a second
secret for the same account would make one of them a lie. Everything else is
Jellyfin's own shape — `/System/Info/Public` to find the server,
`/Users/AuthenticateByName` to sign in, `/Users/{id}/Views` for the
libraries, `BaseItemDto` rows with `RunTimeTicks` and `UserData` — so a
client that has never heard of BMS needs no special case.

What it carries: films, shows, seasons and episodes; artists, albums and
tracks; photo albums and photographs with their dimensions and dates;
posters, covers and thumbnails; subtitles as external streams in the format
asked for; the file itself with range requests; a converted stream for a
client that cannot take the file; and progress, resume, next up and recently
added, per account.

What it does not: Jellyfin's own library management, its plugins, its user
administration, and Live TV. This is the read path a client needs to browse
and play, which is what a server has to offer.

`-jellyfin=false` turns it off.

## Direct play first, and conversion when there is no other way

The file itself is always the first answer. A direct play costs this server
nothing, keeps every frame and every channel the file has, and is what should
happen almost all of the time — so nothing is converted because of a guess.

How that is decided:

- **Clients say what they can play.** The web page asks the browser itself
  (`canPlayType`), so an MKV of H.264 and AAC is a direct play in a browser
  that opens Matroska, and is converted only where one does not. A Jellyfin
  client sends a device profile — its containers, its codecs, and a ceiling
  on the bit rate — to `/Items/{id}/PlaybackInfo`, and is answered with the
  file or with a stream. A client that says nothing — which is every
  Plex-protocol client today — is taken at its word and gets the file.
- **A bit rate ceiling is a request too.** A quality preset is how most
  clients ask for a conversion: nothing is wrong with the codecs, the link is
  just smaller than the file. The picture comes down to the size that ceiling
  pays for rather than spending the whole budget on a 1080p frame, and the
  client is told why in the names the protocol uses
  (`ContainerBitrateExceedsLimit`, `VideoCodecNotSupported`).
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
- **The plan travels in the URL.** What to encode and how is in the query
  string of the playlist a client is given, so there is nothing to expire
  between the negotiation and the first segment, and the values are clamped
  where they are read back. A client that says it has stopped
  (`DELETE /Videos/ActiveEncodings`) frees the core then rather than at the
  idle deadline.

The player says which of the three happened — *Direct play*, *Direct stream*
or *Transcoding*, with the reason — because a server that is quietly
re-encoding everything is the thing people most want to be able to see.

With no ffmpeg, nothing is converted and a file the browser cannot decode is
reported as exactly that, with the file offered for download.

## Resource use

Resource use is a build constraint here, not an afterthought: see
[docs/RESOURCE-BUDGET.md](docs/RESOURCE-BUDGET.md) for the budget, the rules
that keep it, and how it is proved.
