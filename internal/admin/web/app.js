// The admin interface.
//
// Plain modules-free JavaScript against the browser's own APIs: the server
// ships one binary with no build step, so this file is what runs, exactly as
// it is written. Everything on screen is built as DOM nodes with textContent
// rather than assembled as HTML strings, so a film called
// "<img onerror=...>.mkv" is a film with an odd name and not an injection.

'use strict';

const API = '/admin/api';

/* ---------- plumbing ---------- */

async function api(method, path, body) {
  const headers = { 'X-BMS-Admin': '1' };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  const res = await fetch(API + path, {
    method,
    headers,
    credentials: 'same-origin',
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 401) {
    showSignIn();
    throw new Error('signed out');
  }
  if (res.status === 204) return null;
  let data = {};
  try { data = await res.json(); } catch (e) { /* an empty body is fine */ }
  if (!res.ok) throw new Error(data.error || 'that did not work (' + res.status + ')');
  return data;
}

// upload sends raw bytes rather than JSON, which is what an image is.
//
// Its own function rather than a flag on api(): the body is not encoded, the
// content type comes from the file, and conflating the two would mean a
// JSON.stringify of a Blob on the next careless call.
async function upload(path, file) {
  const res = await fetch(API + path, {
    method: 'POST',
    headers: { 'X-BMS-Admin': '1', 'Content-Type': file.type || 'application/octet-stream' },
    credentials: 'same-origin',
    body: file,
  });
  if (res.status === 401) {
    showSignIn();
    throw new Error('signed out');
  }
  let data = {};
  try { data = await res.json(); } catch (e) { /* an empty body is fine */ }
  if (!res.ok) throw new Error(data.error || 'that did not work (' + res.status + ')');
  return data;
}

// fill replaces a parent's contents, dropping the nothings.
//
// replaceChildren turns a null into the text "null" on the page, which is
// exactly what a conditional section that is not showing evaluates to.
function fill(parent, ...nodes) {
  // Flattened like el()'s children, because replaceChildren turns an array
  // into the string "[object HTMLButtonElement]" rather than appending what
  // is in it — a list that silently renders as text instead of buttons.
  parent.replaceChildren(...nodes.flat().filter((node) => node != null && node !== false));
}

// count renders a number with its noun, singular where it should be.
function count(n, singular, plural) {
  return num(n) + ' ' + (n === 1 ? singular : (plural || singular + 's'));
}

function el(tag, props, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(props || {})) {
    if (key === 'text') node.textContent = value;
    else if (key === 'on') for (const [ev, fn] of Object.entries(value)) node.addEventListener(ev, fn);
    else if (value === true) node.setAttribute(key, '');
    else if (value !== false && value != null) node.setAttribute(key, value);
  }
  for (const child of children.flat()) {
    if (child == null || child === false) continue;
    node.append(typeof child === 'string' ? document.createTextNode(child) : child);
  }
  return node;
}

let toastTimer = null;
function toast(message, bad) {
  const box = document.getElementById('toast');
  box.textContent = message;
  box.classList.toggle('bad', !!bad);
  box.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { box.hidden = true; }, bad ? 7000 : 3500);
}

function failed(err) {
  if (err && err.message === 'signed out') return;
  toast(err && err.message ? err.message : String(err), true);
}

/* ---------- formatting ---------- */

const nf = new Intl.NumberFormat();
const num = (n) => nf.format(n || 0);

function size(bytes) {
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  let value = bytes || 0, i = 0;
  while (value >= 1024 && i < units.length - 1) { value /= 1024; i++; }
  return (i === 0 ? value : value.toFixed(value < 10 ? 1 : 0)) + ' ' + units[i];
}

function duration(seconds) {
  seconds = Math.max(0, Math.round(seconds));
  const d = Math.floor(seconds / 86400);
  const h = Math.floor((seconds % 86400) / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  if (d) return d + 'd ' + h + 'h';
  if (h) return h + 'h ' + m + 'm';
  if (m) return m + 'm ' + (seconds % 60) + 's';
  if (seconds === 0) return 'less than a second';
  return seconds + 's';
}

// set reports whether a time came back at all. Go writes a time that was
// never set as the year 1, not as nothing, so every reader has to know that.
function set(iso) { return !!iso && !iso.startsWith('0001-01-01'); }

function when(iso) {
  if (!set(iso)) return 'never';
  const then = new Date(iso);
  const secs = (Date.now() - then.getTime()) / 1000;
  if (secs < 60) return 'just now';
  if (secs < 86400 * 2) return duration(secs) + ' ago';
  return then.toLocaleDateString();
}

/* ---------- sign-in ---------- */

function showSignIn() {
  stopPolling();
  document.getElementById('app').hidden = true;
  document.getElementById('signin').hidden = false;
  document.getElementById('token').focus();
}

function showApp() {
  document.getElementById('signin').hidden = true;
  document.getElementById('app').hidden = false;
}

document.getElementById('signin-form').addEventListener('submit', async (event) => {
  event.preventDefault();
  const box = document.getElementById('signin-error');
  box.hidden = true;
  const input = document.getElementById('token');
  try {
    const me = await api('POST', '/session', { token: input.value });
    input.value = '';
    showApp();
    identify(me);
    render();
  } catch (err) {
    box.textContent = err.message;
    box.hidden = false;
  }
});

document.getElementById('signout').addEventListener('click', async () => {
  try { await api('DELETE', '/session'); } catch (err) { /* signing out is final either way */ }
  showSignIn();
});

function identify(me) {
  document.getElementById('who').textContent = me && me.username ? me.username : '';
}

/* ---------- routing ---------- */

const views = {};
let polling = null;

function stopPolling() {
  if (polling) { clearInterval(polling); polling = null; }
}

function poll(fn) {
  stopPolling();
  // Two seconds while something is running, and nothing at all when it is
  // not: a status page that polls an idle server forever is a laptop fan.
  polling = setInterval(() => { fn().catch(() => stopPolling()); }, 2000);
}

function currentView() {
  const name = (location.hash || '#/overview').replace('#/', '');
  return views[name] ? name : 'overview';
}

async function render() {
  const name = currentView();
  for (const link of document.querySelectorAll('.side a')) {
    link.classList.toggle('on', link.dataset.view === name);
  }
  stopPolling();
  const main = document.getElementById('main');
  fill(main,el('p', { class: 'muted', text: 'Loading…' }));
  try {
    await views[name](main);
  } catch (err) {
    fill(main,el('p', { class: 'error', text: err.message }));
    failed(err);
  }
}

window.addEventListener('hashchange', render);

/* ---------- shared pieces ---------- */

function jobBadge(job) {
  return el('span', { class: 'badge ' + job.state, text: job.state });
}

function jobProgress(job) {
  // Known total: a <progress>, which carries its value as an attribute and so
  // needs no inline style — the page's content security policy forbids those.
  // Unknown total: a bar that animates from the stylesheet, because a scan
  // does not know how many files it will find until it has found them.
  if (job.total > 0) {
    return el('progress', {
      class: 'bar',
      value: Math.min(100, Math.round((job.done / job.total) * 100)),
      max: 100,
      'aria-label': 'progress',
    });
  }
  return el('div', { class: 'bar indeterminate' }, el('i'));
}

function jobLine(job, onCancel) {
  const title = job.kind === 'scan' ? 'Scanning ' + (job.target || 'the library')
    : job.kind === 'enrich' ? 'Fetching metadata'
      : 'Detecting markers';
  const parts = [
    el('div', { class: 'row' },
      el('strong', { text: title }),
      jobBadge(job),
      el('span', { class: 'muted', text: job.note || '' }),
      job.state === 'running' && onCancel
        ? el('button', { class: 'danger', on: { click: () => onCancel(job) }, text: 'Stop' })
        : null),
  ];
  if (job.state === 'running') parts.push(jobProgress(job));
  if (job.error) parts.push(el('p', { class: 'error', text: job.error }));
  return el('div', { class: 'card' }, parts);
}

async function cancelJob(job) {
  try {
    await api('POST', '/jobs/' + job.id + '/cancel');
    toast('Stopping. What it has already written is kept.');
  } catch (err) { failed(err); }
}

async function startJob(kind) {
  try {
    await api('POST', '/jobs', { kind });
    toast('Started.');
    render();
  } catch (err) { failed(err); }
}

// passButtons starts a pass. Every button is always pressable: a pass that
// cannot run says so in words — no ffmpeg, no key — and a greyed-out button
// with no explanation is the thing that sends somebody to the logs.
function passButtons() {
  return el('div', { class: 'row' },
    el('button', { on: { click: () => startJob('scan') }, text: 'Scan libraries', title: 'Walk every library folder and write what is there' }),
    el('button', { on: { click: () => startJob('enrich') }, text: 'Fetch metadata', title: 'Posters, backdrops and summaries, from TMDB' }),
    el('button', { on: { click: () => startJob('markers') }, text: 'Detect intros', title: "Compare each season's audio to find intros and credits" }));
}

/* ---------- now playing ---------- */

// playingCard is who is watching what, with the one button that stops them.
//
// Drawn only when something is playing. A card reading "nothing is playing"
// is a card an operator learns to ignore, and this one has to be noticed.
function playingCard(streams, redraw) {
  if (!streams.length) return null;
  return el('div', { class: 'card' },
    el('h2', { text: 'Playing now' }),
    el('table', {}, el('tbody', {}, streams.map((stream) => el('tr', {},
      el('td', {},
        el('strong', { text: stream.title }),
        stream.subtitle ? el('div', { class: 'muted', text: stream.subtitle }) : null,
        el('div', { class: 'muted mono', text: watchedBy(stream) })),
      el('td', {},
        el('span', { class: 'badge ' + (stream.mode === 'direct' ? 'done' : 'running'), text: modeLabel(stream.mode) }),
        stream.why ? el('div', { class: 'muted', text: stream.why }) : null),
      el('td', { class: 'muted', text: howFar(stream) }),
      el('td', { class: 'right' },
        el('button', {
          class: 'danger',
          text: 'Stop',
          on: { click: () => stopStream(stream, redraw) },
        }))))))); 
}

// watchedBy is the line saying who, on what, from where.
function watchedBy(stream) {
  const parts = [stream.user || 'someone', stream.client || stream.surface];
  if (stream.device && stream.device !== stream.client) parts.push(stream.device);
  if (stream.address) parts.push(stream.address);
  return parts.join('  ·  ');
}

function modeLabel(mode) {
  if (mode === 'transcode') return 'Transcoding';
  if (mode === 'remux') return 'Direct stream';
  return 'Direct play';
}

// howFar is where they are in it, and how long they have been there.
function howFar(stream) {
  const at = Math.round((stream.positionMs || 0) / 1000);
  const of = Math.round((stream.durationMs || 0) / 1000);
  const where = of > 0 ? duration(at) + ' of ' + duration(of) : duration(at);
  const state = stream.paused ? 'paused' : 'watching for ' + duration(stream.forSeconds || 0);
  return where + '  ·  ' + state;
}

// stopStream ends somebody's stream, with a sentence saying why.
//
// The sentence is asked for rather than optional: a Plex client shows it, and
// a film that stops with no explanation is a household asking what broke.
async function stopStream(stream, redraw) {
  const reason = prompt('Stop "' + stream.title + '"?\n\nWhat should the client be told?',
    'An administrator stopped this stream.');
  if (reason === null) return;
  try {
    await api('POST', '/playing/terminate', { key: stream.key, reason: reason });
    toast('Stopped.');
    await redraw();
  } catch (err) { failed(err); }
}

/* ---------- overview ---------- */

views.overview = async function (main) {
  const draw = async () => {
    // Both at once: the page is one view of what this server is doing, and a
    // second round trip would show the two halves a moment apart.
    const [o, now] = await Promise.all([api('GET', '/overview'), api('GET', '/playing')]);
    const streams = (now && now.playing) || [];
    document.getElementById('brand-name').textContent = o.name || 'Bask';
    const s = o.stats || {};
    const running = (o.jobs || []).filter((j) => j.state === 'running');

    // Only what this server actually holds: a household with no photo
    // library should not be told it has none, over and over.
    const tiles = [
      ['Movies', num(s.movies)],
      ['Shows', num(s.shows)],
      ['Episodes', num(s.episodes)],
      s.tracks ? ['Tracks', num(s.tracks)] : null,
      s.albums ? ['Albums', num(s.albums)] : null,
      s.photos ? ['Photographs', num(s.photos)] : null,
      ['Files', num(s.files)],
      ['On disk', size(s.bytes)],
      ['Skip markers', num(s.markedItems)],
    ].filter(Boolean).map(([k, v]) => el('div', { class: 'tile' }, el('div', { class: 'n', text: v }), el('div', { class: 'k', text: k })));

    const facts = [
      ['Version', o.version],
      ['Uptime', duration(o.uptime)],
      ['Heap', o.heapMB + ' MB'],
      ['Goroutines', num(o.goroutines)],
      ['Database', o.databaseMB + ' MB'],
      ['Artwork', o.artworkMB == null ? 'not cached' : size(o.artworkMB * 1048576) + ' of ' + size(o.artworkCapMB * 1048576)],
      ['Playing now', count(o.playing || 0, 'stream')],
      ['Converting now', o.transcoding == null ? 'no ffmpeg' : num(o.transcoding) + ' stream' + (o.transcoding === 1 ? '' : 's')],
      ['Machine id', o.machineId],
    ];

    const todo = [];
    if (s.needsMetadata > 0) todo.push(count(s.needsMetadata, 'item') + ' with no metadata yet');
    if (s.needsMarkers > 0) todo.push(count(s.needsMarkers, 'season') + ' not checked for intros yet');
    if (!o.canScan) todo.push('No ffprobe, so durations and chapters are missing');
    if (!o.canEnrich) todo.push('No TMDB key, so posters and summaries cannot be fetched');
    if (!o.canDetect) todo.push('No ffmpeg, so intros cannot be detected from audio');

    fill(main,
      el('div', { class: 'head' }, el('div', {}, el('h1', { text: o.name || 'Bask Media Server' }),
        el('p', { class: 'muted', text: 'One binary, one database file.' })), passButtons()),
      el('div', { class: 'tiles' }, tiles),
      playingCard(streams, draw),
      running.length
        ? el('div', {}, el('h2', { text: 'Running now' }), running.map((j) => jobLine(j, cancelJob)))
        : null,
      todo.length
        ? el('div', { class: 'card' }, el('h2', { text: 'Worth doing' }),
          el('ul', {}, todo.map((t) => el('li', { text: t }))))
        : null,
      el('div', { class: 'card' }, el('h2', { text: 'This server' }),
        el('table', {}, el('tbody', {}, facts.map(([k, v]) =>
          el('tr', {}, el('th', { text: k }), el('td', { class: 'mono', text: String(v) })))))),
    );
    // Polled while anything is happening, which now includes somebody
    // watching: a position that does not move is a status page nobody trusts.
    const busy = running.length > 0 || streams.length > 0;
    if (busy && !polling) poll(draw);
    if (!busy) stopPolling();
  };
  await draw();
};

/* ---------- libraries ---------- */

views.libraries = async function (main) {
  const sections = (await api('GET', '/sections')) || [];

  const rows = sections.map((sec) => {
    const counts = contents(sec);
    return el('tr', {},
      el('td', {},
        el('strong', { text: sec.title }),
        el('div', { class: 'muted mono', text: (sec.roots || []).join('  ·  ') }),
        (sec.missing || []).length
          ? el('div', { class: 'error', text: 'Not reachable: ' + sec.missing.join(', ') })
          : null),
      el('td', {}, el('span', { class: 'badge', text: sec.kind })),
      el('td', {}, counts, el('div', { class: 'muted', text: count(sec.files, 'file') + ' · ' + size(sec.bytes) })),
      el('td', { class: 'muted' },
        el('div', { text: when(sec.scannedAt) }),
        sec.settings && sec.settings.scanEvery
          ? el('div', { class: 'badge', text: scanEveryLabel(sec.settings.scanEvery) })
          : null),
      el('td', { class: 'right' },
        el('div', { class: 'row actions' },
          el('button', {
            text: 'Scan',
            on: {
              click: async () => {
                try {
                  await api('POST', '/sections/' + sec.id + '/scan');
                  toast('Scanning ' + sec.title + '.');
                  location.hash = '#/jobs';
                } catch (err) { failed(err); }
              },
            },
          }),
          el('button', { text: 'Edit', on: { click: () => editSection(sec) } }),
          el('button', {
            class: 'danger',
            text: 'Remove',
            on: {
              click: async () => {
                if (!confirm('Remove "' + sec.title + '" from this server?\n\nNo files on disk are deleted. The library and everything scanned into it are forgotten.')) return;
                try {
                  await api('DELETE', '/sections/' + sec.id);
                  toast('Removed. No files were touched.');
                  render();
                } catch (err) { failed(err); }
              },
            },
          }))));
  });

  fill(main,
    el('div', { class: 'head' }, el('h1', { text: 'Libraries' })),
    sections.length
      ? el('div', { class: 'card' }, el('table', {},
        el('thead', {}, el('tr', {}, ['Library', 'Holds', 'Contents', 'Last scanned', ''].map(
          (h, i) => el('th', { class: i === 4 ? 'right' : '', text: h })))),
        el('tbody', {}, rows)))
      : el('div', { class: 'card' }, el('p', { class: 'muted', text: 'No libraries yet. Add the folder your films, shows, music or photographs are in.' })),
    addSectionCard(),
  );
};

// scanEveryLabel is the menu's own wording for an interval, so the listing
// and the editor never describe the same number differently.
function scanEveryLabel(minutes) {
  const found = SCAN_INTERVALS.find(([value]) => value === minutes);
  return found ? found[1] : 'Every ' + minutes + ' minutes';
}

// contents is the one line that says how big a library is, in the words of
// whatever it holds.
function contents(sec) {
  switch (sec.kind) {
    case 'shows':
      return count(sec.items, 'show') + ' · ' + count(sec.leaves, 'episode');
    case 'music':
      return count(sec.items, 'artist') + ' · ' + count(sec.leaves, 'track');
    case 'photos':
      return count(sec.items, 'album') + ' · ' + count(sec.leaves, 'photograph');
    default:
      return count(sec.items, 'movie');
  }
}

function addSectionCard() {
  const title = el('input', { id: 'new-title', placeholder: 'Films' });
  const kind = el('select', { id: 'new-kind' },
    el('option', { value: 'movies', text: 'Movies' }),
    el('option', { value: 'shows', text: 'TV shows' }),
    el('option', { value: 'music', text: 'Music' }),
    el('option', { value: 'photos', text: 'Photos' }));
  const root = el('input', { id: 'new-root', placeholder: '/mnt/media/Movies', spellcheck: 'false' });
  const picker = el('div', {});

  const add = el('button', {
    class: 'primary',
    text: 'Add library',
    on: {
      click: async () => {
        try {
          await api('POST', '/sections', { title: title.value, kind: kind.value, roots: [root.value] });
          toast('Added. Scan it when you are ready.');
          render();
        } catch (err) { failed(err); }
      },
    },
  });

  return el('div', { class: 'card' },
    el('h2', { text: 'Add a library' }),
    el('label', { for: 'new-title', text: 'Name' }), title,
    el('label', { for: 'new-kind', text: 'What it holds' }), kind,
    el('label', { for: 'new-root', text: 'Folder' }), root,
    el('div', { class: 'row' },
      el('button', { text: 'Browse…', on: { click: () => browseInto(picker, root) } }),
      add),
    picker);
}

// browseInto renders a folder picker that writes into an input.
//
// Typing a path blind is where library setup usually goes wrong — a drive
// letter that moved, a trailing space, a share that is not mounted — so the
// server lists what it can actually see and the path comes from a click.
async function browseInto(container, input, path) {
  try {
    const listing = await api('GET', '/browse' + (path ? '?path=' + encodeURIComponent(path) : ''));
    const list = el('div', { class: 'picker' });
    if (listing.parent !== undefined) {
      list.append(el('button', {
        text: listing.parent ? '↑ ' + listing.parent : '↑ all drives',
        on: { click: () => browseInto(container, input, listing.parent) },
      }));
    }
    for (const folder of listing.folders || []) {
      list.append(el('button', {
        text: folder.name,
        on: { click: () => browseInto(container, input, folder.path) },
      }));
    }
    const here = listing.path
      ? el('div', { class: 'row' },
        el('span', { class: 'mono', text: listing.path }),
        el('span', { class: 'muted', text: count(listing.media, 'media file') + ' here' }),
        el('button', {
          class: 'primary',
          text: 'Use this folder',
          on: {
            click: () => {
              input.value = listing.path;
              fill(container,);
            },
          },
        }))
      : el('p', { class: 'muted', text: 'Pick a drive to start from.' });
    fill(container,here, list);
  } catch (err) { failed(err); }
}

// SCAN_INTERVALS are the choices offered for scanning a library without
// being asked.
//
// A menu rather than a number box: the useful answers are a handful, the
// server refuses anything under a quarter of an hour anyway, and a box
// invites somebody to type 1 and wonder why it was refused.
const SCAN_INTERVALS = [
  [0, 'Only when asked'],
  [15, 'Every 15 minutes'],
  [30, 'Every half hour'],
  [60, 'Every hour'],
  [180, 'Every 3 hours'],
  [360, 'Every 6 hours'],
  [720, 'Every 12 hours'],
  [1440, 'Once a day'],
];

function editSection(sec) {
  const title = el('input', { value: sec.title });
  const roots = el('input', { value: (sec.roots || []).join(' | '), spellcheck: 'false' });
  const picker = el('div', {});

  const settings = sec.settings || {};
  const every = el('select', {}, SCAN_INTERVALS.map(([minutes, label]) =>
    el('option', {
      value: String(minutes), text: label,
      selected: (settings.scanEvery || 0) === minutes,
    })));

  // Tri-state on the wire, two states here: these are "on unless somebody
  // said otherwise", so the box is ticked when the setting is absent and
  // sending false is the only thing that turns one off.
  const toggle = (id, label, value, note) => {
    const box = el('input', { type: 'checkbox', id: id + '-' + sec.id });
    box.checked = value !== false;
    return {
      box,
      row: el('div', {},
        el('div', { class: 'row' }, box, el('label', { for: id + '-' + sec.id, text: label })),
        note ? el('p', { class: 'muted', text: note }) : null),
    };
  };

  const enrich = toggle('enrich', 'Fetch titles, summaries and artwork', settings.enrich,
    'Off for a library no catalogue knows — home videos, recordings, anything whose names are not a catalogue\u2019s names.');
  const people = toggle('people', 'Fetch cast and crew', settings.people,
    'The expensive half: a request per title on top of the match. Never asked for music or photographs.');
  const markers = toggle('markers', 'Look for intros and credits', settings.markers,
    'Reads the audio of every episode, which is the slowest thing this server does. Off for a library of films \u2014 there is nothing to skip into.');

  const nextLine = sec.nextScan
    ? el('p', { class: 'muted', text: 'Next scan ' + when(sec.nextScan) + '.' })
    : null;

  const card = el('div', { class: 'card' },
    el('h2', { text: 'Edit ' + sec.title }),
    el('label', { text: 'Name' }), title,
    el('label', { text: 'Folders, separated by |' }), roots,

    el('h3', { text: 'Scanning' }),
    el('label', { text: 'Scan this library' }), every,
    el('p', { class: 'muted', text: 'A rescan of a library nothing changed in costs a stat per file and no writes, so a schedule is about how soon a new film appears rather than how much work the server does. Folders are not watched: a library on a network share produces no change events at all, so a watch would work only on the setups that need it least.' }),
    nextLine,

    el('h3', { text: 'What to do with it' }),
    enrich.row,
    people.row,
    markers.row,

    el('div', { class: 'row' },
      el('button', { text: 'Browse…', on: { click: () => browseInto(picker, roots) } }),
      el('button', {
        class: 'primary',
        text: 'Save',
        on: {
          click: async () => {
            try {
              await api('PATCH', '/sections/' + sec.id, {
                title: title.value,
                roots: roots.value.split('|').map((r) => r.trim()).filter(Boolean),
                // Only the switches that are off are sent. An absent setting
                // is the default, so a library left alone stores nothing and
                // reads back the same as one that predates these controls.
                settings: {
                  scanEvery: Number(every.value) || 0,
                  ...(enrich.box.checked ? {} : { enrich: false }),
                  ...(people.box.checked ? {} : { people: false }),
                  ...(markers.box.checked ? {} : { markers: false }),
                },
              });
              toast('Saved. Scan it to pick up what changed.');
              render();
            } catch (err) { failed(err); }
          },
        },
      }),
      el('button', { text: 'Cancel', on: { click: () => render() } })),
    picker);

  const main = document.getElementById('main');
  fill(main,el('div', { class: 'head' }, el('h1', { text: 'Libraries' })), card);
}

/* ---------- metadata ---------- */

// The metadata view: find something, correct what the provider got wrong.
//
// A search box rather than a browser, because this is the page somebody opens
// having already seen the wrong poster on their television — they know what
// they are looking for, and a tree of every library is in the way of it.
views.metadata = async function (main) {
  const box = el('input', { placeholder: 'A title', spellcheck: 'false' });
  const results = el('div', {});
  const bulk = el('div', {});

  // What is ticked, by id, kept across a redraw of the rows so somebody can
  // search twice and edit both sets of results together.
  const picked = new Map();

  const drawBulk = () => {
    fill(bulk, picked.size ? bulkCard(picked, () => { picked.clear(); search(); }) : null);
  };

  const search = async () => {
    const term = box.value.trim();
    if (term.length < 2) {
      fill(results, el('p', { class: 'muted', text: 'Two letters or more.' }));
      return;
    }
    try {
      const found = (await api('GET', '/items?q=' + encodeURIComponent(term))) || [];
      fill(results, found.length
        ? el('div', {},
          el('div', { class: 'row' },
            el('button', {
              text: 'Select all ' + found.length,
              on: {
                click: () => {
                  found.forEach((item) => picked.set(item.id, item));
                  search();
                },
              },
            }),
            picked.size
              ? el('button', { text: 'Clear selection', on: { click: () => { picked.clear(); search(); } } })
              : null),
          el('table', {}, el('tbody', {}, found.map((item) => {
            const tick = el('input', { type: 'checkbox', id: 'pick-' + item.id });
            tick.checked = picked.has(item.id);
            tick.addEventListener('change', () => {
              if (tick.checked) picked.set(item.id, item);
              else picked.delete(item.id);
              drawBulk();
            });
            return el('tr', {},
              el('td', { class: 'tick' }, tick),
              el('td', {}, el('strong', { text: item.title + (item.year ? ' (' + item.year + ')' : '') }),
                el('div', { class: 'muted', text: [item.kind, item.library].filter(Boolean).join('  ·  ') })),
              el('td', { class: 'right' }, el('button', { text: 'Edit', on: { click: () => editItem(item.id) } })));
          }))))
        : el('p', { class: 'muted', text: 'Nothing by that name.' }));
      drawBulk();
    } catch (err) { failed(err); }
  };

  box.addEventListener('keydown', (event) => { if (event.key === 'Enter') search(); });

  fill(main,
    el('div', { class: 'head' }, el('div', {}, el('h1', { text: 'Metadata' }),
      el('p', { class: 'muted', text: 'Correct a title, or pick the right entry when the provider chose the wrong one. Tick several to change the same thing about all of them.' }))),
    el('div', { class: 'card' },
      el('label', { text: 'Find something' }),
      el('div', { class: 'row' }, box, el('button', { class: 'primary', text: 'Search', on: { click: search } })),
      results),
    bulk);
  box.focus();
};

// bulkCard is one change applied to everything ticked.
//
// Words are added and taken away rather than replaced, which is the whole
// point of editing a search result: "also label these Christmas". Replacing
// would mean every other label on every one of them was lost to a field
// somebody left empty — and replacing is right there in the one-at-a-time
// editor, where the page can show what is already on it.
//
// No title, no year, no summary. Forty items with one title is forty items
// nobody can tell apart, so those are the editor's job and this does not
// offer them.
function bulkCard(picked, done) {
  const commas = (box) => box.value.split(',').map((word) => word.trim()).filter(Boolean);

  const fields = {};
  const pair = (key, label) => {
    fields['add' + key] = el('input', { placeholder: 'add…', spellcheck: 'false' });
    fields['remove' + key] = el('input', { placeholder: 'remove…', spellcheck: 'false' });
    return el('div', {},
      el('label', { text: label }),
      el('div', { class: 'row' }, fields['add' + key], fields['remove' + key]));
  };

  const studio = el('input', { placeholder: 'leave empty to change nothing', spellcheck: 'false' });
  const rating = el('input', { placeholder: 'leave empty to change nothing', spellcheck: 'false' });
  const lock = el('select', {},
    el('option', { value: '', text: 'Leave as they are' }),
    el('option', { value: 'yes', text: 'Lock, so a metadata pass leaves them alone' }),
    el('option', { value: 'no', text: 'Unlock, so the next pass can fetch for them' }));

  const titles = [...picked.values()].slice(0, 6).map((item) => item.title);
  const andMore = picked.size > titles.length ? ' and ' + (picked.size - titles.length) + ' more' : '';

  return el('div', { class: 'card' },
    el('h2', { text: count(picked.size, 'item') + ' selected' }),
    el('p', { class: 'muted', text: titles.join(', ') + andMore }),

    pair('Genres', 'Genres'),
    pair('Tags', 'Tags'),
    pair('Labels', 'Labels'),
    pair('Moods', 'Moods'),
    pair('Styles', 'Styles'),
    el('p', { class: 'muted', text: 'Separated by commas. A word in both boxes is removed \u2014 predictable beats clever.' }),

    el('label', { text: 'Studio' }), studio,
    el('label', { text: 'Certificate' }), rating,
    el('p', { class: 'muted', text: 'A certificate here sets both the label a client shows and the age a rating ceiling compares against, so the two cannot disagree.' }),
    el('label', { text: 'Metadata passes' }), lock,

    el('div', { class: 'row' },
      el('button', {
        class: 'primary',
        text: 'Apply to ' + count(picked.size, 'item'),
        on: {
          click: async () => {
            const body = { items: [...picked.keys()] };
            for (const [key, input] of Object.entries(fields)) {
              const words = commas(input);
              if (words.length) body[key] = words;
            }
            if (studio.value.trim()) body.studio = studio.value.trim();
            if (rating.value.trim()) body.rating = rating.value.trim();
            if (lock.value) body.locked = lock.value === 'yes';

            if (Object.keys(body).length === 1) { toast('Nothing to change.', true); return; }
            if (!confirm('Change ' + count(picked.size, 'item') + '?\n\nThis applies to all of them at once.')) return;
            try {
              const answer = await api('POST', '/items/bulk', body);
              toast((answer && answer.note) || 'Done.');
              done();
            } catch (err) { failed(err); }
          },
        },
      }),
      el('button', { text: 'Cancel', on: { click: done } })));
}

// editItem is the editor for one item: the fields, and the provider's other
// entries.
async function editItem(id) {
  const main = document.getElementById('main');
  let detail;
  try {
    detail = await api('GET', '/items/' + id);
  } catch (err) { failed(err); return; }

  const title = el('input', { value: detail.title, spellcheck: 'false' });
  const year = el('input', { value: detail.year || '', type: 'number' });
  const summary = el('textarea', { rows: '4' });
  summary.value = detail.summary || '';
  // Words as one line of comma-separated values each, rather than rows of
  // removable chips: a list is three or four words, typing is faster than
  // clicking, and the field reads the same way it is stored.
  //
  // Five kinds, kept apart because they are written by different people. A
  // genre comes from a catalogue or a file's own tags; a tag and a label are
  // somebody's here, which is what makes them worth having. Mood and style
  // are music's, and a music client draws them in their own rows.
  const words = (name) => el('input', {
    value: (detail[name] || []).join(', '), spellcheck: 'false',
  });
  const genres = words('genres');
  const tags = words('tags');
  const labels = words('labels');
  const moods = words('moods');
  const styles = words('styles');
  const commas = (box) => box.value.split(',').map((word) => word.trim()).filter(Boolean);
  const locked = el('input', { type: 'checkbox', id: 'locked-' + id });
  locked.checked = !!detail.locked;

  const matches = el('div', {});
  const term = el('input', { value: detail.title, spellcheck: 'false' });

  // What the filename said, where it is no longer what is shown: the one
  // fact that explains why a rescan did not put the old title back.
  const scanned = (detail.scannedTitle && detail.scannedTitle !== detail.title)
    ? el('p', { class: 'muted', text: 'The file is named "' + detail.scannedTitle
      + (detail.scannedYear ? ' (' + detail.scannedYear + ')' : '') + '", which is still how a scan finds it.' })
    : null;

  const save = async () => {
    try {
      await api('PATCH', '/items/' + id, {
        title: title.value,
        year: Number(year.value) || 0,
        summary: summary.value,
        genres: commas(genres),
        tags: commas(tags),
        labels: commas(labels),
        moods: commas(moods),
        styles: commas(styles),
        locked: locked.checked,
      });
      toast(locked.checked ? 'Saved and locked, so a metadata pass will leave it alone.' : 'Saved.');
      editItem(id);
    } catch (err) { failed(err); }
  };

  const searchProvider = async () => {
    fill(matches, el('p', { class: 'muted', text: 'Asking the provider…' }));
    try {
      const answer = await api('GET', '/items/' + id + '/candidates?q=' + encodeURIComponent(term.value.trim()));
      const found = (answer && answer.candidates) || [];
      fill(matches, found.length
        ? el('div', {}, found.map((candidate) => el('div', { class: 'row match' },
          candidate.posterUrl ? el('img', { src: candidate.posterUrl, alt: '', class: 'thumb' }) : null,
          el('div', { class: 'grow' },
            el('strong', { text: candidate.title + (candidate.year ? ' (' + candidate.year + ')' : '') }),
            candidate.current ? el('span', { class: 'badge', text: 'current' }) : null,
            el('div', { class: 'muted', text: (candidate.summary || '').slice(0, 200) })),
          el('button', {
            text: 'Use this',
            on: { click: () => useMatch(id, candidate) },
          }))))
        : el('p', { class: 'muted', text: 'The provider has nothing by that name.' }));
    } catch (err) { failed(err); }
  };

  fill(main,
    el('div', { class: 'head' }, el('div', {}, el('h1', { text: detail.title }),
      el('p', { class: 'muted', text: [detail.kind, detail.library].filter(Boolean).join('  ·  ') })),
      el('button', { text: 'Back', on: { click: () => render() } })),

    el('div', { class: 'card' },
      el('h2', { text: 'Details' }),
      scanned,
      el('label', { text: 'Title' }), title,
      el('label', { text: 'Year' }), year,
      el('label', { text: 'Summary' }), summary,
      el('label', { text: 'Genres' }), genres,
      el('p', { class: 'muted', text: 'Separated by commas. Saved as typed, so a metadata pass will not add to them until this is unlocked.' }),
      el('label', { text: 'Tags' }), tags,
      el('label', { text: 'Labels' }), labels,
      el('p', { class: 'muted', text: 'Tags and labels are yours \u2014 nothing fetches or overwrites them. Plex clients read both as labels, because their wire has one field for a word somebody attached; Jellyfin reads both as tags.' }),
      el('label', { text: 'Moods' }), moods,
      el('label', { text: 'Styles' }), styles,
      el('p', { class: 'muted', text: 'What a music client shows beside an album. Melancholy is a mood, not a genre.' }),
      el('div', { class: 'row' },
        locked,
        el('label', { for: 'locked-' + id, text: 'Leave this alone when fetching metadata' })),
      el('div', { class: 'row' },
        el('button', { class: 'primary', text: 'Save', on: { click: save } }),
        el('button', {
          text: 'Back to the filename',
          on: {
            click: async () => {
              if (!confirm('Put the file\'s own title back?\n\nIt is unlocked as well, so the next metadata pass will match it again.')) return;
              try {
                await api('POST', '/items/' + id + '/revert');
                toast('Reverted.');
                editItem(id);
              } catch (err) { failed(err); }
            },
          },
        })),
      (detail.files || []).length
        ? el('div', { class: 'muted mono files' }, (detail.files || []).map((path) => el('div', { text: path })))
        : null),

    placementCard(id, detail),

    artworkCard(id, detail),

    subtitleCard(id, detail),

    el('div', { class: 'card' },
      el('h2', { text: 'Which one is it?' }),
      detail.canRematch
        ? el('div', {},
          el('p', { class: 'muted', text: 'Search the provider and pick the right entry. The summary and the artwork are replaced with that entry\'s.' }),
          el('div', { class: 'row' }, term,
            el('button', { text: 'Search the provider', on: { click: searchProvider } })),
          matches)
        : el('p', { class: 'muted', text: detail.why || 'This item cannot be matched to a provider.' })));
}

// placementCard is where a special goes in the order a show is watched.
//
// Only for a special, because a regular episode's place is its season and
// its number, and offering to move one would be offering to lie about which
// episode it is.
//
// An answer here beats both TheTVDB and the air date, and it does not lock
// the item: the title and the artwork are still a metadata pass's to fetch,
// which is why this is its own control and not the lock tick above.
function placementCard(id, detail) {
  if (!detail.special) return null;
  const place = detail.placement || { afterSeason: -1, beforeSeason: -1, beforeEpisode: -1 };

  // Minus one is "nobody has said", drawn as an empty field: zero is a real
  // answer, so a blank is the only way to mean unset.
  const num = (value) => el('input', {
    type: 'number', min: '0', class: 'short',
    value: value >= 0 ? String(value) : '',
  });
  const after = num(place.afterSeason);
  const beforeSeason = num(place.beforeSeason);
  const beforeEpisode = num(place.beforeEpisode);
  const read = (box) => (box.value.trim() === '' ? -1 : Number(box.value));

  const whereNow = () => {
    if (place.afterSeason >= 0) return 'After season ' + place.afterSeason + '.';
    if (place.beforeSeason >= 0) {
      return place.beforeEpisode >= 0
        ? 'Before S' + place.beforeSeason + 'E' + place.beforeEpisode + '.'
        : 'At the start of season ' + place.beforeSeason + '.';
    }
    return detail.aired
      ? 'Nobody has placed it, so it sits by when it aired (' + detail.aired + ').'
      : 'Nobody has placed it and it has no air date, so it sits at the end.';
  };

  return el('div', { class: 'card' },
    el('h2', { text: 'Where this special goes' }),
    el('p', { class: 'muted', text: whereNow() + (place.byHand ? ' Set by hand.' : '') }),
    el('p', { class: 'muted', text: 'A Christmas episode that went out between two series belongs between them, not before the pilot. The air date usually gets this right on its own; this is for the ones it does not.' }),

    el('label', { text: 'After season' }), after,
    el('p', { class: 'muted', text: 'Leave the others empty.' }),
    el('label', { text: 'Or before season' }), beforeSeason,
    el('label', { text: 'and before episode' }), beforeEpisode,
    el('p', { class: 'muted', text: 'An episode number needs its season. Leave it empty for the start of that season.' }),

    el('div', { class: 'row' },
      el('button', {
        class: 'primary',
        text: 'Save placement',
        on: {
          click: async () => {
            try {
              const answer = await api('PUT', '/items/' + id + '/placement', {
                afterSeason: read(after),
                beforeSeason: read(beforeSeason),
                beforeEpisode: read(beforeEpisode),
              });
              toast((answer && answer.note) || 'Saved.');
              editItem(id);
            } catch (err) { failed(err); }
          },
        },
      }),
      el('button', {
        text: 'Let the air date decide',
        on: {
          click: async () => {
            try {
              await api('PUT', '/items/' + id + '/placement',
                { afterSeason: -1, beforeSeason: -1, beforeEpisode: -1 });
              toast('Cleared. It will sit by when it aired.');
              editItem(id);
            } catch (err) { failed(err); }
          },
        },
      })));
}

// artworkCard is where the picture on an item is chosen.
//
// Three ways in, because the services work three different ways: a grid of
// what TMDB and TheTVDB have, a box for a pasted address — which is the only
// way ThePosterDB can be used, since it has no search API — and a file off
// this machine. Whichever is used, the picture is fetched into the server's
// own cache, so a shelf does not go blank when somebody else reorganises a
// CDN.
//
// The grid is not fetched with the page. Opening an editor would otherwise
// cost two requests to a third party every time, for a chooser most visits
// never open.
function artworkCard(id, detail) {
  const shown = el('div', { class: 'art-now row' },
    el('figure', {},
      el('img', { src: '/admin/api/items/' + id + '/image?kind=poster&w=140&v=' + Date.now(), alt: '' }),
      el('figcaption', { class: 'muted', text: 'Poster' })),
    el('figure', {},
      el('img', { src: '/admin/api/items/' + id + '/image?kind=art&w=260&v=' + Date.now(), alt: '' }),
      el('figcaption', { class: 'muted', text: 'Backdrop' })));

  const grid = el('div', {});
  const link = el('input', { placeholder: 'https://theposterdb.com/poster/…', spellcheck: 'false' });
  const linkKind = el('select', {},
    el('option', { value: 'poster', text: 'as the poster' }),
    el('option', { value: 'backdrop', text: 'as the backdrop' }));
  const file = el('input', { type: 'file', accept: 'image/jpeg,image/png' });
  const fileKind = el('select', {},
    el('option', { value: 'poster', text: 'as the poster' }),
    el('option', { value: 'backdrop', text: 'as the backdrop' }));

  // One place that applies a choice, so the three ways in agree about what
  // happens afterwards: a word, and the editor redrawn so the pictures at
  // the top are the new ones.
  const applied = (answer) => {
    toast((answer && answer.note) || 'Saved.');
    editItem(id);
  };

  const browse = async () => {
    fill(grid, el('p', { class: 'muted', text: 'Asking the artwork services…' }));
    try {
      const answer = await api('GET', '/items/' + id + '/artwork');
      const art = (answer && answer.art) || [];
      const notes = [];
      if (answer.why) notes.push(answer.why);
      (answer.failed || []).forEach((line) => notes.push(line));
      if (!art.length && !notes.length) {
        notes.push('None of ' + (answer.sources || []).join(', ') + ' has a picture of this.');
      }

      fill(grid,
        ...notes.map((line) => el('p', { class: 'muted', text: line })),
        art.length ? el('div', { class: 'art-grid' }, art.map((one) => {
          const label = [
            one.source,
            one.language ? one.language : 'no text',
            one.width ? one.width + '×' + one.height : '',
          ].filter(Boolean).join(' · ');
          return el('button', {
            class: 'art' + (one.kind === 'backdrop' ? ' wide' : ''),
            title: 'Use this ' + one.kind,
            on: {
              click: async () => {
                try {
                  applied(await api('POST', '/items/' + id + '/artwork', { kind: one.kind, url: one.url }));
                } catch (err) { failed(err); }
              },
            },
          },
          el('img', { src: one.thumbUrl || one.url, alt: '', loading: 'lazy' }),
          el('span', { class: 'muted', text: label }));
        })) : null);
    } catch (err) { failed(err); fill(grid, el('p', { class: 'muted', text: err.message })); }
  };

  return el('div', { class: 'card' },
    el('h2', { text: 'Artwork' }),
    shown,
    el('p', { class: 'muted', text: 'Whatever is chosen is copied into this server’s own cache, and choosing one locks the item so a metadata pass will not replace it.' }),

    el('div', { class: 'row' },
      el('button', { text: 'Show what the services have', on: { click: browse } })),
    grid,

    el('h3', { text: 'From a link' }),
    el('p', { class: 'muted', text: 'ThePosterDB has no search API and asks not to be crawled, so there is nothing to browse there — open the poster you want and paste its address. Any other image address works too.' }),
    el('div', { class: 'row' }, link, linkKind,
      el('button', {
        text: 'Use it',
        on: {
          click: async () => {
            if (!link.value.trim()) { toast('Paste an address first.', true); return; }
            try {
              applied(await api('POST', '/items/' + id + '/artwork',
                { kind: linkKind.value, url: link.value.trim() }));
            } catch (err) { failed(err); }
          },
        },
      })),

    el('h3', { text: 'From this machine' }),
    el('div', { class: 'row' }, file, fileKind,
      el('button', {
        text: 'Upload it',
        on: {
          click: async () => {
            const chosen = file.files && file.files[0];
            if (!chosen) { toast('Pick a picture first.', true); return; }
            try {
              applied(await upload('/items/' + id + '/artwork/upload?kind=' + fileKind.value, chosen));
            } catch (err) { failed(err); }
          },
        },
      })));
}

// subtitleCard finds a subtitle for a film that has none.
//
// Only for the things that have a file: a series or a season has nothing to
// match a subtitle against, and the hash of the file is what makes a match
// worth having.
function subtitleCard(id, detail) {
  if (!(detail.files || []).length) return null;

  const language = el('input', { value: 'en', spellcheck: 'false', class: 'short' });
  const forced = el('input', { type: 'checkbox', id: 'sub-forced-' + id });
  const hearing = el('input', { type: 'checkbox', id: 'sub-sdh-' + id });
  const results = el('div', {});

  const search = async () => {
    fill(results, el('p', { class: 'muted', text: 'Asking OpenSubtitles…' }));
    try {
      const answer = await api('GET', '/items/' + id + '/subtitles?language='
        + encodeURIComponent(language.value.trim() || 'en')
        + (forced.checked ? '&forced=1' : '') + (hearing.checked ? '&hearingImpaired=1' : ''));
      const found = (answer && answer.subtitles) || [];
      fill(results, found.length
        ? el('table', {}, el('tbody', {}, found.map((offer) => el('tr', {},
          el('td', {},
            el('strong', { text: offer.release || offer.name || 'A subtitle' }),
            el('div', { class: 'muted', text: [
              offer.exact ? 'Timed against this exact file' : 'For another release',
              offer.language,
              offer.forced ? 'forced' : '',
              offer.hearingImpaired ? 'SDH' : '',
              offer.downloads ? count(offer.downloads, 'download') : '',
            ].filter(Boolean).join('  ·  ') })),
          el('td', { class: 'right' }, el('button', {
            class: offer.exact ? 'primary' : '',
            text: 'Use this',
            on: { click: () => useSubtitle(id, offer, language.value.trim() || 'en', forced.checked, hearing.checked) },
          }))))))
        : el('p', { class: 'muted', text: 'Nothing on offer for that.' }));
    } catch (err) { failed(err); }
  };

  return el('div', { class: 'card' },
    el('h2', { text: 'Subtitles' }),
    el('p', { class: 'muted', text: 'A subtitle timed against this exact file cannot drift; one for another release can be a minute out by the end. The file is written beside the film, so the next scan finds it.' }),
    el('div', { class: 'row' },
      el('label', { text: 'Language' }), language,
      forced, el('label', { for: 'sub-forced-' + id, text: 'forced' }),
      hearing, el('label', { for: 'sub-sdh-' + id, text: 'for the hard of hearing' }),
      el('button', { text: 'Search', on: { click: search } })),
    results);
}

// useSubtitle fetches the one somebody picked.
async function useSubtitle(id, offer, language, forced, hearing) {
  try {
    const saved = await api('POST', '/items/' + id + '/subtitles', {
      handle: offer.handle, language, forced, hearingImpaired: hearing,
    });
    toast('Saved beside the film as ' + (saved.path || 'a sidecar') + '.');
  } catch (err) { failed(err); }
}

// useMatch points the item at the entry somebody chose.
async function useMatch(id, candidate) {
  if (!confirm('Use "' + candidate.title + (candidate.year ? ' (' + candidate.year + ')' : '') + '"?')) return;
  try {
    const answer = await api('POST', '/items/' + id + '/match', { providerId: candidate.providerId });
    toast(answer && answer.note ? answer.note : 'Matched to ' + candidate.title + '.');
    editItem(id);
  } catch (err) { failed(err); }
}

/* ---------- activity ---------- */

views.jobs = async function (main) {
  const draw = async () => {
    const list = (await api('GET', '/jobs')) || [];
    fill(main,
      el('div', { class: 'head' }, el('h1', { text: 'Activity' }), passButtons()),
      list.length
        ? el('div', {}, list.map((job) => el('div', {},
          jobLine(job, cancelJob),
          el('p', {
            class: 'muted',
            text: when(job.started) + (set(job.finished)
              ? ' · ran for ' + duration((new Date(job.finished) - new Date(job.started)) / 1000)
              : ''),
          }))))
        : el('div', { class: 'card' }, el('p', { class: 'muted', text: 'Nothing has run since the server started.' })),
    );
    if (list.some((j) => j.state === 'running')) { if (!polling) poll(draw); } else stopPolling();
  };
  await draw();
};

/* ---------- collections ---------- */

// A collection is the library's grouping, so it is made here rather than by a
// viewer. Playlists and favourites are deliberately not in this interface:
// they belong to an account, and an administrator reaching into somebody's
// playlists is the wrong shape even on a server they own.
views.collections = async function (main) {
  const all = (await api('GET', '/collections')) || [];
  const title = el('input', { id: 'new-collection', placeholder: 'The Three Colours Trilogy' });

  const rows = all.map((c) => el('tr', {},
    el('td', {}, el('strong', { text: c.title }),
      c.rules ? el('span', { class: 'badge', text: ' by rules' }) : null,
      c.unreadable ? el('span', { class: 'badge failed', text: ' rules unreadable' }) : null,
      c.summary ? el('div', { class: 'muted', text: c.summary }) : null),
    el('td', { class: 'muted', text: count(c.held, 'item') }),
    el('td', { class: 'right' }, el('div', { class: 'row actions' },
      el('button', { text: 'Edit', on: { click: () => editCollection(c.id) } }),
      el('button', {
        class: 'danger',
        text: 'Delete',
        on: {
          click: async () => {
            if (!confirm('Delete "' + c.title + '"?\n\nThe grouping goes. The films stay exactly where they are.')) return;
            try {
              await api('DELETE', '/collections/' + c.id);
              toast('Deleted.');
              render();
            } catch (err) { failed(err); }
          },
        },
      })))));

  fill(main,
    el('div', { class: 'head' }, el('div', {}, el('h1', { text: 'Collections' }),
      el('p', { class: 'muted', text: 'Groupings the whole household sees: a trilogy, a director, a Christmas shelf. Pick what goes in by hand, or give it rules and let it fill itself. A collection holding nothing a device may see is hidden from that device entirely.' }))),
    el('div', { class: 'card' }, all.length
      ? el('table', {}, el('tbody', {}, rows))
      : el('p', { class: 'muted', text: 'No collections yet.' })),
    el('div', { class: 'card' },
      el('h2', { text: 'Add a collection' }),
      el('label', { for: 'new-collection', text: 'Name' }), title,
      el('div', { class: 'row' }, el('button', {
        class: 'primary',
        text: 'Create',
        on: {
          click: async () => {
            try {
              const made = await api('POST', '/collections', { title: title.value });
              editCollection(made.id);
            } catch (err) { failed(err); }
          },
        },
      }))));
};

// editCollection edits one collection, which is two editors sharing a name
// and a Save button.
//
// A collection is either picked by hand or filled by rules, never both, so
// this shows one or the other with a way across. Nothing is saved until Save,
// including the change from one kind to the other: a half-built rule set is
// somebody mid-thought rather than an instruction.
async function editCollection(id) {
  try {
    const [collection, vocabulary, libraries] = await Promise.all([
      api('GET', '/collections/' + id),
      api('GET', '/collections/fields'),
      api('GET', '/sections'),
    ]);

    // A copy, so abandoning the page abandons the edit.
    let rules = collection.rules ? JSON.parse(JSON.stringify(collection.rules)) : null;
    const chosen = rules ? [] : (collection.items || []).slice();
    const name = el('input', { value: collection.title });
    const summary = el('input', { value: collection.summary || '' });

    const label = (item) =>
      item.title + (item.year ? ' (' + item.year + ')' : '') + ' · ' + item.library;

    const main = document.getElementById('main');

    const save = async () => {
      try {
        const body = { title: name.value, summary: summary.value, rules: rules };
        // Only a hand-picked collection sends a list. A smart one has no
        // stored membership, and sending one would be refused.
        if (!rules) body.items = chosen.map((item) => item.id);
        await api('PATCH', '/collections/' + id, body);
        toast('Saved.');
        render();
      } catch (err) { failed(err); }
    };

    const draw = () => {
      fill(main,
        el('div', { class: 'head' }, el('div', {}, el('h1', { text: collection.title }),
          el('p', { class: 'muted', text: rules
            ? 'This one fills itself: everything in the library that answers the rules below, in the order they name.'
            : 'The order here is the order a client shows them in.' })),
          el('button', { text: 'Back to collections', on: { click: () => render() } })),
        el('div', { class: 'card' },
          el('label', { text: 'Name' }), name,
          el('label', { text: 'Summary' }), summary),
        collection.unreadable && !rules
          ? el('div', { class: 'card' },
              el('h2', { text: 'These rules could not be read' }),
              el('p', { class: 'muted', text: 'Something stored rules this build of the server cannot parse, so the collection holds nothing. Build them again below, or pick films by hand instead.' }))
          : null,
        rules ? rulesCard(rules, vocabulary, libraries, label, () => { rules = null; draw(); })
              : handCard(chosen, label, () => { rules = { match: 'all', rules: [], sort: 'title' }; draw(); }),
        el('div', { class: 'card' },
          el('div', { class: 'row' }, el('button', { class: 'primary', text: 'Save', on: { click: save } }))));
    };
    draw();
  } catch (err) { failed(err); }
}

// handCard is the original picker: what is in it, in order, and a search to
// add more.
function handCard(chosen, label, turnSmart) {
  const list = el('tbody', {});
  const search = el('input', { placeholder: 'Search the library' });
  const results = el('div', { class: 'row wrap' });

  const drawChosen = () => {
    fill(list, chosen.map((item, index) => el('tr', {},
      el('td', { class: 'muted', text: String(index + 1) }),
      el('td', { text: label(item) }),
      el('td', { class: 'right' }, el('div', { class: 'row actions' },
        el('button', {
          text: '↑',
          on: { click: () => { if (index > 0) { chosen.splice(index - 1, 0, chosen.splice(index, 1)[0]); drawChosen(); } } },
        }),
        el('button', {
          text: '↓',
          on: { click: () => { if (index < chosen.length - 1) { chosen.splice(index + 1, 0, chosen.splice(index, 1)[0]); drawChosen(); } } },
        }),
        el('button', {
          class: 'danger',
          text: 'Remove',
          on: { click: () => { chosen.splice(index, 1); drawChosen(); } },
        }))))));
    if (chosen.length === 0) {
      fill(list, el('tr', {}, el('td', { class: 'muted', text: 'Nothing in it yet.' })));
    }
  };
  drawChosen();

  let searching = null;
  search.addEventListener('input', () => {
    const term = search.value.trim();
    if (!term) { fill(results); return; }
    clearTimeout(searching);
    searching = setTimeout(async () => {
      try {
        const found = (await api('GET', '/items?q=' + encodeURIComponent(term))) || [];
        fill(results, found.map((item) => el('button', {
          text: label(item),
          on: {
            click: () => {
              if (chosen.some((already) => already.id === item.id)) {
                toast('Already in it.');
                return;
              }
              chosen.push(item);
              drawChosen();
            },
          },
        })));
        if (found.length === 0) fill(results, el('span', { class: 'muted', text: 'Nothing matched.' }));
      } catch (err) { failed(err); }
    }, 200);
  });

  return [
    el('div', { class: 'card' },
      el('h2', { text: 'In this collection' }),
      el('table', {}, list)),
    el('div', { class: 'card' },
      el('h2', { text: 'Add something' }),
      el('label', { text: 'Search' }), search,
      results),
    el('div', { class: 'card' },
      el('h2', { text: 'Or let it fill itself' }),
      el('p', { class: 'muted', text: 'Rules instead of a list: everything unwatched in one library, everything added this month, every film over two hours. A collection filled by rules keeps no list of its own, so whatever is picked here goes when you save.' }),
      el('div', { class: 'row' }, el('button', { text: 'Use rules instead', on: { click: turnSmart } }))),
  ];
}

/* ---------- the rule builder ---------- */

// What each comparison is called. The server owns which ones a field allows;
// what they are called in English is this page's business, and the words
// differ by what the field holds — "is before" reads wrong about a number of
// days ago.
const OP_WORDS = {
  is: 'is', isnot: 'is not', contains: 'contains', starts: 'starts with',
  before: 'is before', after: 'is after', atleast: 'is at least', atmost: 'is at most',
  within: 'within the last', yes: 'all the way through', no: 'not at all',
  partly: 'part of the way through',
};
const DAY_WORDS = { within: 'within the last', before: 'longer ago than' };
const KIND_WORDS = {
  movie: 'Film', show: 'Series', album: 'Album', artist: 'Artist', photoalbum: 'Photo album',
};
const SORT_WORDS = {
  title: 'Title', year: 'Year', added: 'When it was added', duration: 'Length',
};
const UNITS = { days: 'days', minutes: 'minutes' };

function opWord(spec, op) {
  if (spec.value === 'days' && DAY_WORDS[op]) return DAY_WORDS[op];
  return OP_WORDS[op] || op;
}

// rulesCard is the question: the conditions, how to read them, the order, and
// what the library says the answer is right now.
function rulesCard(rules, vocabulary, libraries, label, turnHand) {
  const fields = vocabulary.fields || [];
  const specOf = (field) => fields.find((f) => f.field === field) || fields[0];
  const body = el('div', {});
  const answer = el('div', {});

  let pending = null;
  const ask = () => {
    clearTimeout(pending);
    pending = setTimeout(async () => {
      try {
        const got = await api('POST', '/collections/preview', rules);
        const held = got.held || 0;
        fill(answer,
          el('p', {}, el('strong', {
            text: held === 1 ? '1 item matches' : count(held, 'item') + ' match',
          }), rules.limit ? el('span', { class: 'muted', text: ' · capped at ' + rules.limit } ) : null),
          (got.items || []).length
            ? el('table', {}, el('tbody', {}, (got.items || []).map((item) =>
                el('tr', {}, el('td', { text: label(item) })))))
            : el('p', { class: 'muted', text: 'Nothing in the library answers this yet.' }));
      } catch (err) {
        // Shown here rather than as a toast: this fires on every keystroke,
        // and a half-typed rule is not worth shouting about.
        fill(answer, el('p', { class: 'muted', text: err.message }));
      }
    }, 250);
  };

  const value = (rule, spec) => {
    switch (spec.value) {
      case 'none':
        return null;
      case 'library': {
        const pick = el('select', {}, libraries.map((lib) =>
          el('option', { value: String(lib.id), text: lib.title, selected: String(lib.id) === String(rule.value) })));
        if (!rule.value && libraries.length) { rule.value = String(libraries[0].id); pick.value = rule.value; }
        pick.addEventListener('change', () => { rule.value = pick.value; ask(); });
        return pick;
      }
      case 'choice': {
        const pick = el('select', {}, (spec.choices || []).map((choice) =>
          el('option', { value: choice, text: KIND_WORDS[choice] || choice, selected: choice === rule.value })));
        if (!rule.value && (spec.choices || []).length) { rule.value = spec.choices[0]; pick.value = rule.value; }
        pick.addEventListener('change', () => { rule.value = pick.value; ask(); });
        return pick;
      }
      case 'person':
      case 'genre': {
        // A text box with the library's own genres offered beside it, not a
        // menu of them: a rule may name a genre nothing carries yet, and the
        // shelf then fills the day something does.
        const listID = 'genres-' + Math.random().toString(36).slice(2, 8);
        const box = el('input', {
          value: rule.value || '', list: listID, spellcheck: 'false',
          placeholder: (spec.choices || []).length ? spec.choices[0] : (spec.value === 'person' ? 'A name' : 'Comedy'),
        });
        box.addEventListener('input', () => { rule.value = box.value; ask(); });
        return [box, el('datalist', { id: listID }, (spec.choices || []).map((choice) =>
          el('option', { value: choice })))];
      }
      case 'number': case 'days': case 'minutes': {
        const box = el('input', { type: 'number', min: '0', value: rule.value || '' });
        box.addEventListener('input', () => { rule.value = box.value; ask(); });
        return UNITS[spec.value] ? [box, el('span', { class: 'muted', text: UNITS[spec.value] })] : box;
      }
      default: {
        const box = el('input', { value: rule.value || '', placeholder: spec.field === 'certificate' ? 'PG, 15, or empty for unrated' : '' });
        box.addEventListener('input', () => { rule.value = box.value; ask(); });
        return box;
      }
    }
  };

  const draw = () => {
    const rows = (rules.rules || []).map((rule, index) => {
      const spec = specOf(rule.field);
      const field = el('select', {}, fields.map((f) =>
        el('option', { value: f.field, text: f.label, selected: f.field === rule.field })));
      field.addEventListener('change', () => {
        const next = specOf(field.value);
        rule.field = next.field;
        rule.op = next.ops[0];
        rule.value = '';
        draw();
      });

      const op = el('select', {}, (spec.ops || []).map((o) =>
        el('option', { value: o, text: opWord(spec, o), selected: o === rule.op })));
      op.addEventListener('change', () => { rule.op = op.value; draw(); });

      return el('tr', {},
        el('td', {}, field),
        el('td', {}, op),
        el('td', {}, value(rule, spec)),
        el('td', { class: 'right' }, el('button', {
          class: 'danger',
          text: 'Remove',
          on: { click: () => { rules.rules.splice(index, 1); draw(); } },
        })));
    });

    const match = el('select', {}, (vocabulary.matches || ['all', 'any']).map((m) =>
      el('option', { value: m, text: m === 'any' ? 'any one of these' : 'all of these', selected: m === (rules.match || 'all') })));
    match.addEventListener('change', () => { rules.match = match.value; draw(); });

    const sort = el('select', {}, (vocabulary.sorts || []).map((s) =>
      el('option', { value: s, text: SORT_WORDS[s] || s, selected: s === (rules.sort || 'title') })));
    sort.addEventListener('change', () => { rules.sort = sort.value; draw(); });

    const backwards = el('input', { type: 'checkbox' });
    backwards.checked = !!rules.desc;
    backwards.addEventListener('change', () => { rules.desc = backwards.checked; draw(); });

    const cap = el('input', { type: 'number', min: '0', value: rules.limit ? String(rules.limit) : '' });
    cap.addEventListener('input', () => { rules.limit = Number(cap.value) || 0; ask(); });

    fill(body,
      el('div', { class: 'row' },
        el('span', { class: 'muted', text: 'An item belongs here when it answers' }), match),
      rows.length
        ? el('table', {}, el('tbody', {}, rows))
        : el('p', { class: 'muted', text: 'No rules yet, which means the whole library. Add one, or set a limit and an order to make a shelf of it.' }),
      el('div', { class: 'row' }, el('button', {
        text: 'Add a rule',
        on: {
          click: () => {
            const first = fields[0];
            rules.rules = rules.rules || [];
            rules.rules.push({ field: first.field, op: first.ops[0], value: '' });
            draw();
          },
        },
      })),
      el('div', { class: 'row' },
        el('span', { class: 'muted', text: 'In order of' }), sort,
        el('label', { class: 'row' }, backwards, el('span', { text: 'backwards' })),
        el('span', { class: 'muted', text: 'keeping at most' }), cap,
        el('span', { class: 'muted', text: '(blank for all of them)' })));
    ask();
  };
  draw();

  return [
    el('div', { class: 'card' },
      el('h2', { text: 'Rules' }),
      body),
    el('div', { class: 'card' },
      el('h2', { text: 'What that matches now' }),
      el('p', { class: 'muted', text: 'The whole library, as you see it: a viewer sees only the part of this their libraries and age rating allow. Watched and unwatched are answered for your own account, and every account answers them for itself — so a collection of unwatched films genuinely holds different films for different people.' }),
      answer),
    el('div', { class: 'card' },
      el('h2', { text: 'Or pick films by hand' }),
      el('p', { class: 'muted', text: 'The rules go and the collection becomes an ordinary list, empty to start with.' }),
      el('div', { class: 'row' }, el('button', { text: 'Pick by hand instead', on: { click: turnHand } }))),
  ];
}

/* ---------- users ---------- */

views.users = async function (main) {
  const users = (await api('GET', '/users')) || [];
  const libraries = (await api('GET', '/sections')) || [];

  const rows = users.flatMap((user) => [el('tr', {},
    el('td', {}, el('strong', { text: user.username }), user.self ? el('span', { class: 'muted', text: ' (you)' }) : null),
    el('td', {},
      user.admin ? el('span', { class: 'badge done', text: 'administrator' }) : el('span', { class: 'badge', text: 'viewer' }),
      // An invited account arrives here able to reach nothing, so saying so
      // in the row is what tells an administrator there is something to do.
      user.active === false ? el('span', { class: 'badge failed', text: 'waiting' }) : null,
      user.fromCloud ? el('span', { class: 'badge', text: 'invited' }) : null),
    el('td', { class: 'muted', text: user.fromCloud ? 'a Bask account' : count(user.tokens, 'token') }),
    el('td', { class: 'muted', text: when(user.createdAt) }),
    el('td', { class: 'right' }, el('div', { class: 'row actions' },
      // An invited account holds no token: the account service signs for it
      // on each request, so there is nothing here to list or revoke.
      user.fromCloud ? null : el('button', { text: 'Tokens', on: { click: () => showTokens(user) } }),
      el('button', { text: 'Limits', on: { click: () => editLimits(user) } }),
      el('button', {
        class: user.active === false ? 'primary' : '',
        text: user.active === false ? 'Allow' : 'Suspend',
        on: {
          click: async () => {
            if (user.active !== false
              && !confirm('Suspend "' + user.username + '"?\n\nEvery credential they hold stops working at once. Their watched state and resume positions are kept, and allowing them again puts everything back.')) return;
            try {
              await api('PATCH', '/users/' + user.id, { active: user.active === false });
              toast(user.active === false ? 'Allowed.' : 'Suspended.');
              render();
            } catch (err) { failed(err); }
          },
        },
      }),
      el('button', {
        text: user.admin ? 'Make viewer' : 'Make administrator',
        on: {
          click: async () => {
            try {
              await api('PATCH', '/users/' + user.id, { admin: !user.admin });
              render();
            } catch (err) { failed(err); }
          },
        },
      }),
      el('button', {
        class: 'danger',
        text: 'Delete',
        on: {
          click: async () => {
            if (!confirm('Delete "' + user.username + '"?\n\nTheir tokens stop working and their watched state and resume positions are deleted.')) return;
            try {
              await api('DELETE', '/users/' + user.id);
              toast('Deleted.');
              render();
            } catch (err) { failed(err); }
          },
        },
      })))),
    // An administrator administers the server rather than browsing it, so
    // narrowing one would be a setting with no effect and is not offered.
    libraries.length && !user.admin
      ? el('tr', {}, el('td', { colspan: '5' },
          libraryPicker(user.sections, libraries, '/users/' + user.id, 'account', () => render())))
      : null,
  ]);

  const name = el('input', { id: 'new-user', placeholder: 'kitchen-tv' });
  const admin = el('select', { id: 'new-user-admin' },
    el('option', { value: 'viewer', text: 'Viewer' }),
    el('option', { value: 'admin', text: 'Administrator' }));

  fill(main,
    el('div', { class: 'head' }, el('div', {}, el('h1', { text: 'Users' }),
      el('p', { class: 'muted', text: 'Accounts live on this server. The libraries ticked under an account are the only ones it can reach, and a device can be narrowed further under Tokens but never past this. '
        + 'An account marked invited comes from a Bask account and is vouched for by the account service; what it may watch is still decided here.' }))),
    el('div', { class: 'card' }, el('table', {},
      el('thead', {}, el('tr', {}, ['Account', 'Can', 'Credentials', 'Added', ''].map(
        (h, i) => el('th', { class: i === 4 ? 'right' : '', text: h })))),
      el('tbody', {}, rows))),
    el('div', { class: 'card' },
      el('h2', { text: 'Add an account' }),
      el('label', { for: 'new-user', text: 'Name' }), name,
      el('label', { for: 'new-user-admin', text: 'Role' }), admin,
      el('div', { class: 'row' }, el('button', {
        class: 'primary',
        text: 'Create and issue a token',
        on: {
          click: async () => {
            try {
              const made = await api('POST', '/users', { username: name.value, admin: admin.value === 'admin' });
              showSecret(made.token, 'Give this to the client that will sign in as ' + name.value + '.');
            } catch (err) { failed(err); }
          },
        },
      }))),
    el('div', { id: 'secret' }));
};

// showSecret displays a token once, because once is all the server can do: it
// stores only the hash, so this value cannot be shown again.
function showSecret(token, note) {
  const box = document.getElementById('secret') || document.getElementById('main');
  fill(box,el('div', { class: 'card' },
    el('h2', { text: 'This token is shown once' }),
    el('p', { class: 'muted', text: note }),
    el('div', { class: 'secret mono', text: token }),
    el('div', { class: 'row' },
      el('button', {
        text: 'Copy',
        on: {
          click: async () => {
            try {
              await navigator.clipboard.writeText(token);
              toast('Copied.');
            } catch (err) { toast('Select it and copy it by hand.', true); }
          },
        },
      }),
      el('button', { text: 'Done', on: { click: () => render() } }))));
  box.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
}

// editLimits is what an account may watch, and how much of the server it may
// take.
//
// All of it on the account rather than on the device, because every one of
// these is about a person: a child is a child on the television and on the
// tablet.
function editLimits(user) {
  const ceiling = el('select', { id: 'ceiling-' + user.id },
    el('option', { value: '-1', text: 'No limit' }),
    ...[0, 7, 8, 12, 13, 14, 15, 16, 17, 18].map((age) => el('option', {
      value: String(age),
      text: age === 0 ? 'Rated for everyone' : 'Rated ' + age + ' and under',
    })));
  ceiling.value = String(user.ceiling);

  const unrated = el('input', { type: 'checkbox', id: 'unrated-' + user.id });
  unrated.checked = !!user.allowUnrated;

  const streams = el('input', { type: 'number', min: '0', value: String(user.maxStreams || 0), class: 'short' });
  const bitrate = el('input', {
    type: 'number', min: '0', class: 'short',
    value: String(Math.round((user.maxBitRate || 0) / 1000000)),
  });
  const pin = el('input', { type: 'password', id: 'pin-' + user.id, placeholder: user.hasPin ? 'set' : 'not set', class: 'short' });

  const card = el('div', { class: 'card' },
    el('h2', { text: 'What ' + user.username + ' may watch' }),
    el('label', { for: 'ceiling-' + user.id, text: 'Highest rating' }), ceiling,
    el('div', { class: 'row' }, unrated,
      el('label', { for: 'unrated-' + user.id, text: 'Also show anything nobody has rated' })),
    el('p', { class: 'muted', text: 'Ratings come from the metadata pass, so an item it has not reached yet has none. Leaving the box ticked keeps a half-enriched library watchable; unticking it is what makes a restricted account predictable.' }),

    el('h2', { text: 'How much of the server' }),
    el('div', { class: 'row' }, el('label', { text: 'Streams at once' }), streams,
      el('span', { class: 'muted', text: '0 is no limit' })),
    el('div', { class: 'row' }, el('label', { text: 'Megabits per second' }), bitrate,
      el('span', { class: 'muted', text: '0 is no limit' })),
    el('p', { class: 'muted', text: 'A bandwidth limit converts a stream down to fit rather than refusing it. A stream limit refuses: there is no smaller version of a third film at once.' }),

    el('h2', { text: 'PIN' }),
    el('label', { for: 'pin-' + user.id, text: 'Four digits or more' }), pin,
    el('p', { class: 'muted', text: 'Typed in the watching interface to lift the rating limit for a few hours. Leave the box empty to keep the PIN that is set; clear it with the button.' }),

    el('div', { class: 'row' },
      el('button', {
        class: 'primary',
        text: 'Save',
        on: {
          click: async () => {
            const body = {
              ceiling: Number(ceiling.value),
              allowUnrated: unrated.checked,
              maxStreams: Math.max(0, Number(streams.value) || 0),
              maxBitRate: Math.max(0, Math.round((Number(bitrate.value) || 0) * 1000000)),
            };
            if (pin.value.trim() !== '') body.pin = pin.value.trim();
            try {
              await api('PATCH', '/users/' + user.id, body);
              toast('Saved.');
              render();
            } catch (err) { failed(err); }
          },
        },
      }),
      user.hasPin
        ? el('button', {
          class: 'danger',
          text: 'Clear the PIN',
          on: {
            click: async () => {
              try {
                await api('PATCH', '/users/' + user.id, { pin: '' });
                toast('Cleared. The limit can no longer be lifted from the watching interface.');
                render();
              } catch (err) { failed(err); }
            },
          },
        })
        : null,
      el('button', { text: 'Cancel', on: { click: () => render() } })));

  const main = document.getElementById('main');
  fill(main, el('div', { class: 'head' }, el('h1', { text: 'Users' })), card);
}

// libraryPicker is the row of checkboxes that decides which libraries a
// credential or an account may see.
//
// Every box ticked is stored as no restriction at all, so a library added
// later is visible rather than quietly missing. Saving is explicit: a
// half-ticked set of boxes is somebody mid-thought, not an instruction.
function libraryPicker(granted, libraries, path, noun, onSaved) {
  const chosen = new Set(granted || []);
  const all = chosen.size === 0;
  const key = path.replace(/[^a-z0-9]+/gi, '-');
  const boxes = libraries.map((section) => {
    const box = el('input', { type: 'checkbox', id: 'pick' + key + '-' + section.id });
    box.checked = all || chosen.has(section.id);
    return { section, box };
  });

  const summary = el('span', { class: 'muted' });
  const describe = () => {
    const ticked = boxes.filter((b) => b.box.checked);
    summary.textContent = ticked.length === libraries.length
      ? 'every library'
      : ticked.length === 0
        ? 'nothing ticked yet'
        : ticked.map((b) => b.section.title).join(', ');
  };
  describe();
  for (const b of boxes) b.box.addEventListener('change', describe);

  return el('div', { class: 'row wrap' },
    boxes.map((b) => el('label', { class: 'check', for: b.box.id }, b.box, b.section.title)),
    el('button', {
      text: 'Save libraries',
      on: {
        click: async () => {
          const ticked = boxes.filter((b) => b.box.checked).map((b) => b.section.id);
          if (ticked.length === 0) {
            toast('Tick at least one library. To stop this ' + noun + ' reaching anything, remove it.', true);
            return;
          }
          try {
            await api('PATCH', path, { sections: ticked });
            toast(ticked.length === libraries.length ? 'This ' + noun + ' sees every library.' : 'Saved.');
            onSaved();
          } catch (err) { failed(err); }
        },
      },
    }),
    summary);
}

async function showTokens(user) {
  try {
    const tokens = (await api('GET', '/users/' + user.id + '/tokens')) || [];
    const sections = (await api('GET', '/sections')) || [];
    const label = el('input', { placeholder: 'living room' });

    const rows = tokens.flatMap((t) => [
      el('tr', {},
        el('td', {}, el('strong', { text: t.label || '—' }),
          t.device ? el('div', { class: 'muted', text: 'last signed in as ' + t.device }) : null),
        el('td', { class: 'muted', text: 'added ' + when(t.createdAt) }),
        el('td', { class: 'muted', text: 'last used ' + when(t.lastSeenAt) }),
        el('td', { class: 'right' }, el('button', {
          class: 'danger',
          text: 'Revoke',
          on: {
            click: async () => {
              if (!confirm('Revoke this token?\n\nThe client holding it is signed out the next time it asks for anything.')) return;
              try {
                await api('DELETE', '/tokens/' + t.id);
                toast('Revoked.');
                showTokens(user);
              } catch (err) { failed(err); }
            },
          },
        }))),
      sections.length
        ? el('tr', {}, el('td', { colspan: '4' },
            libraryPicker(t.sections, sections, '/tokens/' + t.id, 'device', () => showTokens(user))))
        : null,
    ]);

    const main = document.getElementById('main');
    fill(main,
      el('div', { class: 'head' }, el('div', {}, el('h1', { text: user.username }),
        el('p', { class: 'muted', text: 'One token per device is worth the minute it costs: revoking one then signs out one television, and the libraries ticked under a token are the only ones that device can reach.' })),
        el('button', { text: 'Back to users', on: { click: () => render() } })),
      el('div', { class: 'card' }, tokens.length
        ? el('table', {}, el('tbody', {}, rows))
        : el('p', { class: 'muted', text: 'No tokens. This account cannot sign in until it has one.' })),
      el('div', { class: 'card' },
        el('h2', { text: 'Issue a token' }),
        el('label', { text: 'What is it for' }), label,
        el('div', { class: 'row' }, el('button', {
          class: 'primary',
          text: 'Issue',
          on: {
            click: async () => {
              try {
                const made = await api('POST', '/users/' + user.id + '/tokens', { label: label.value });
                showSecret(made.token, 'Give this to ' + (label.value || 'the client') + '.');
              } catch (err) { failed(err); }
            },
          },
        }))),
      el('div', { id: 'secret' }));
  } catch (err) { failed(err); }
}

/* ---------- history ---------- */

// The activity log: who watched what, when, and for how long.
//
// Administrator only, like everything else here, and worth saying why: Now
// Playing shows what is happening while it happens, which anybody in the
// house could see by walking into the room. This answers what somebody
// watched three Tuesdays ago, which they could not — so it sits behind the
// same sign-in as the accounts, and has a button that forgets it.
let historyWindow = 30;
let historyStart = 0;

views.history = async function (main) {
  const draw = async () => {
    const page = await api('GET', '/history?days=' + historyWindow + '&start=' + historyStart);
    const watches = (page && page.watches) || [];
    const size = (page && page.size) || 50;
    const total = (page && page.total) || 0;

    const windows = [[7, 'Last week'], [30, 'Last month'], [90, 'Last three months'], [0, 'Everything']];
    const pick = el('select', { id: 'history-window' },
      windows.map(([days, label]) => el('option', { value: days, text: label, selected: days === historyWindow })));
    pick.addEventListener('change', async () => {
      historyWindow = Number(pick.value);
      historyStart = 0;
      await draw();
    });

    const rows = watches.map((watch) => el('tr', {},
      el('th', {},
        el('div', { text: watch.title + (watch.subtitle ? ' — ' + watch.subtitle : '') }),
        el('div', { class: 'muted', text: watch.user + ' · ' + (watch.device || watch.client || 'somewhere') })),
      el('td', {},
        el('div', { text: duration(watch.watched) }),
        watch.paused ? el('div', { class: 'muted', text: duration(watch.paused) + ' paused' }) : null,
        watch.length && watch.position
          ? el('div', { class: 'muted', text: 'got to ' + duration(watch.position) + ' of ' + duration(watch.length) })
          : null),
      el('td', {},
        el('div', { class: 'muted', text: when(watch.started) }),
        el('div', { class: 'muted', text: new Date(watch.started).toLocaleString() })),
      el('td', {},
        watch.mode ? el('span', { class: 'badge ' + (watch.mode === 'direct' ? 'done' : 'running'), text: watch.mode }) : null,
        watch.surface ? el('div', { class: 'muted', text: watch.surface }) : null,
        watch.reason ? el('div', { class: 'muted', text: watch.reason }) : null,
        !watch.complete ? el('div', { class: 'muted', text: 'the server never saw this stop' }) : null)));

    const older = el('button', {
      text: 'Older',
      disabled: historyStart + size >= total,
      on: { click: async () => { historyStart += size; await draw(); } },
    });
    const newer = el('button', {
      text: 'Newer',
      disabled: historyStart === 0,
      on: { click: async () => { historyStart = Math.max(0, historyStart - size); await draw(); } },
    });

    fill(main,
      el('div', { class: 'head' }, el('h1', { text: 'History' }), el('div', { class: 'row' }, pick)),
      totalsCard(page),
      el('div', { class: 'card' },
        el('h2', { text: count(total, 'watch', 'watches') }),
        rows.length
          ? el('table', {}, el('tbody', {}, rows))
          : el('p', { class: 'muted', text: 'Nothing has been watched in this window.' }),
        total > size
          ? el('div', { class: 'row' }, newer, older,
            el('span', { class: 'muted', text: (historyStart + 1) + '–' + Math.min(historyStart + size, total) + ' of ' + total }))
          : null),
      forgetCard(draw));
  };
  await draw();
};

// totalsCard is the two questions a page of rows cannot answer: who watches
// the most of this server, and what gets watched on it.
function totalsCard(page) {
  const viewers = (page && page.viewers) || [];
  const titles = (page && page.titles) || [];
  if (!viewers.length && !titles.length) return null;

  const table = (rows) => el('table', {}, el('tbody', {}, rows.map((row) => el('tr', {},
    el('th', { text: row.name }),
    el('td', { class: 'muted', text: count(row.plays, 'play') }),
    el('td', { class: 'muted', text: duration(row.watched) })))));

  return el('div', { class: 'card' },
    el('h2', { text: 'In this window' }),
    el('h3', { text: 'By viewer' }),
    viewers.length ? table(viewers) : el('p', { class: 'muted', text: 'Nobody.' }),
    el('h3', { text: 'Most watched' }),
    titles.length ? table(titles) : el('p', { class: 'muted', text: 'Nothing.' }));
}

// forgetCard is the delete. A record of who watched what is the most personal
// thing this server holds, and getting rid of it should not mean opening the
// database.
function forgetCard(draw) {
  const forget = async (olderThan, confirmation) => {
    if (!confirm(confirmation)) return;
    try {
      const gone = await api('DELETE', '/history' + (olderThan ? '?olderThan=' + olderThan : ''));
      toast('Forgot ' + count(gone.removed, 'watch', 'watches') + '.');
      historyStart = 0;
      await draw();
    } catch (err) { failed(err); }
  };

  return el('div', { class: 'card' },
    el('h2', { text: 'Forgetting' }),
    el('p', { class: 'muted', text: 'The log is kept until somebody says otherwise. A row is small — a few '
      + 'hundred bytes — so a household watching ten things a day writes under a megabyte a year.' }),
    el('div', { class: 'row' },
      el('button', {
        text: 'Forget anything older than 90 days',
        on: { click: () => forget(90, 'Forget everything watched more than 90 days ago?\n\nThis cannot be undone.') },
      }),
      el('button', {
        class: 'danger',
        text: 'Forget everything',
        on: { click: () => forget(0, 'Forget the whole activity log?\n\nThis cannot be undone. Watched state, resume positions and playlists are not affected.') },
      })));
}

/* ---------- settings ---------- */

views.settings = async function (main) {
  const [settings, backups, hooks, account] = await Promise.all([
    api('GET', '/settings'), api('GET', '/backups'), api('GET', '/webhooks'), api('GET', '/cloud'),
  ]);
  const name = el('input', { id: 'server-name', value: settings.name || '' });
  const key = el('input', { id: 'tmdb-key', type: 'password', placeholder: settings.tmdbKeyStored ? 'stored' : 'not set', spellcheck: 'false' });
  const tvdbKey = el('input', {
    id: 'tvdb-key', type: 'password',
    placeholder: settings.tvdbKeyStored ? 'stored' : 'not set', spellcheck: 'false',
  });
  // The same choices a library's scan offers, so the two schedules are
  // described in one vocabulary.
  const passEvery = (id, value) => el('select', { id: id }, SCAN_INTERVALS.map(([minutes, label]) =>
    el('option', {
      value: String(minutes),
      text: minutes === 0 ? 'Only when asked' : label,
      selected: (value || 0) === minutes,
    })));
  const enrichEvery = passEvery('enrich-every', settings.enrichEvery);
  const markersEvery = passEvery('markers-every', settings.markersEvery);

  const tvdbPin = el('input', {
    id: 'tvdb-pin', type: 'password',
    placeholder: settings.tvdbPinStored ? 'stored' : 'only for a subscriber key', spellcheck: 'false',
  });
  const subtitleKey = el('input', {
    id: 'subtitle-key', type: 'password',
    placeholder: settings.subtitleKeyStored ? 'stored' : 'not set', spellcheck: 'false',
  });

  const tools = [
    ['ffprobe', settings.ffprobe, 'Durations and chapter markers during a scan'],
    ['ffmpeg', settings.ffmpeg, 'Reading audio, which is how intros are detected'],
  ].map(([tool, present, why]) => el('tr', {},
    el('th', { text: tool }),
    el('td', {}, el('span', { class: 'badge ' + (present ? 'done' : 'failed'), text: present ? 'found' : 'not found' })),
    el('td', { class: 'muted', text: why })));

  fill(main,
    el('div', { class: 'head' }, el('h1', { text: 'Settings' })),
    el('div', { class: 'card' },
      el('h2', { text: 'This server' }),
      el('label', { for: 'server-name', text: 'Name clients show' }), name,
      el('p', { class: 'muted', text: 'Changing this is immediate. Clients see the new name the next time they ask, and nothing else about the server changes: it keeps its identity, so no client forgets what it knew.' }),
      el('div', { class: 'row' }, el('button', {
        class: 'primary',
        text: 'Save',
        on: {
          click: async () => {
            try {
              await api('PATCH', '/settings', { name: name.value });
              document.getElementById('brand-name').textContent = name.value;
              toast('Saved.');
            } catch (err) { failed(err); }
          },
        },
      }))),
    el('div', { class: 'card' },
      el('h2', { text: 'Metadata' }),
      el('label', { for: 'tmdb-key', text: 'TMDB API key or read token' }), key,
      el('p', {
        class: 'muted',
        text: settings.tmdbKeyStored
          ? 'A key is stored. Typing a new one replaces it; the one stored is never sent back to this page.'
          : (settings.tmdbKeyElsewhere
            ? 'A key is coming from the command line or the environment. One saved here would be used instead.'
            : 'Without a key the library is browsable and playable and simply has no posters or summaries.'),
      }),
      el('div', { class: 'row' },
        el('button', {
          class: 'primary',
          text: 'Save key',
          on: {
            click: async () => {
              try {
                await api('PATCH', '/settings', { tmdbKey: key.value });
                key.value = '';
                toast('Saved. Fetch metadata to use it.');
                render();
              } catch (err) { failed(err); }
            },
          },
        }),
        settings.tmdbKeyStored
          ? el('button', {
            class: 'danger',
            text: 'Forget the stored key',
            on: {
              click: async () => {
                try {
                  await api('PATCH', '/settings', { tmdbKey: '' });
                  toast('Forgotten.');
                  render();
                } catch (err) { failed(err); }
              },
            },
          })
          : null)),
    el('div', { class: 'card' },
      el('h2', { text: 'Passes' }),
      el('p', { class: 'muted', text: 'A library that scans itself finds a new film and then leaves it with no poster until somebody presses a button. These are the passes that finish the job, and they run only when something is actually waiting \u2014 an empty queue starts nothing.' }),
      el('label', { for: 'enrich-every', text: 'Fetch metadata' }), enrichEvery,
      el('label', { for: 'markers-every', text: 'Find intros and credits' }), markersEvery,
      el('p', { class: 'muted', text: 'Each library decides whether these touch it at all, in its own settings.' }),
      el('div', { class: 'row' },
        el('button', {
          class: 'primary',
          text: 'Save',
          on: {
            click: async () => {
              try {
                await api('PATCH', '/settings', {
                  enrichEvery: Number(enrichEvery.value) || 0,
                  markersEvery: Number(markersEvery.value) || 0,
                });
                toast('Saved.');
                render();
              } catch (err) { failed(err); }
            },
          },
        }))),
    el('div', { class: 'card' },
      el('h2', { text: 'Artwork' }),
      el('p', { class: 'muted', text: 'TMDB\u2019s pictures come with the key above. TheTVDB is a second source for the artwork chooser only \u2014 it is never used to match a title, because a library matched against two services would need a rule for which one wins.' }),
      el('label', { for: 'tvdb-key', text: 'TheTVDB API key' }), tvdbKey,
      el('label', { for: 'tvdb-pin', text: 'Subscriber PIN' }), tvdbPin,
      el('p', {
        class: 'muted',
        text: settings.tvdbKeyStored
          ? 'A key is stored, so TheTVDB appears in the artwork chooser. The PIN is only wanted by a user-supported key.'
          : 'Without a key, the chooser offers TMDB and the link box. ThePosterDB needs no key: it has no search API, so a poster is used by pasting its address.',
      }),
      el('div', { class: 'row' },
        el('button', {
          class: 'primary',
          text: 'Save key',
          on: {
            click: async () => {
              try {
                const patch = {};
                if (tvdbKey.value) patch.tvdbKey = tvdbKey.value;
                if (tvdbPin.value) patch.tvdbPin = tvdbPin.value;
                if (!Object.keys(patch).length) { toast('Nothing typed.', true); return; }
                await api('PATCH', '/settings', patch);
                tvdbKey.value = '';
                tvdbPin.value = '';
                toast('Saved.');
                render();
              } catch (err) { failed(err); }
            },
          },
        }),
        settings.tvdbKeyStored
          ? el('button', {
            class: 'danger',
            text: 'Forget the stored key',
            on: {
              click: async () => {
                try {
                  await api('PATCH', '/settings', { tvdbKey: '', tvdbPin: '' });
                  toast('Forgotten. TheTVDB is no longer offered.');
                  render();
                } catch (err) { failed(err); }
              },
            },
          })
          : null)),
    el('div', { class: 'card' },
      el('h2', { text: 'Subtitles' }),
      el('label', { for: 'subtitle-key', text: 'OpenSubtitles API key' }), subtitleKey,
      el('p', {
        class: 'muted',
        text: settings.subtitleKeyStored
          ? 'A key is stored, so clients can search for subtitles a file does not have. Clearing it turns searching off.'
          : 'Without a key, this server never searches for subtitles and nothing about this library leaves the machine. With one, a search sends the title and the file\'s hash.',
      }),
      el('div', { class: 'row' },
        el('button', {
          class: 'primary',
          text: 'Save key',
          on: {
            click: async () => {
              try {
                await api('PATCH', '/settings', { subtitleKey: subtitleKey.value });
                subtitleKey.value = '';
                toast('Saved.');
                render();
              } catch (err) { failed(err); }
            },
          },
        }),
        settings.subtitleKeyStored
          ? el('button', {
            class: 'danger',
            text: 'Forget the stored key',
            on: {
              click: async () => {
                try {
                  await api('PATCH', '/settings', { subtitleKey: '' });
                  toast('Forgotten. Subtitle searching is off.');
                  render();
                } catch (err) { failed(err); }
              },
            },
          })
          : null)),
    el('div', { class: 'card' },
      el('h2', { text: 'Tools found on this machine' }),
      el('table', {}, el('tbody', {}, tools)),
      el('p', { class: 'muted', text: 'Both are optional. Without them the library still scans, browses and plays; it has no durations, chapters or detected intros.' })),
    accountServiceCard(account),
    webhookCard(hooks),
    backupCard(backups),
    el('div', { class: 'card' },
      el('h2', { text: 'This sign-in' }),
      el('p', { class: 'muted', text: 'A browser session lasts ' + settings.sessionDays + ' days and is separate from the tokens clients hold, so signing out here signs nothing else out.' })));
};

// accountServiceCard is claiming this server with a Bask account, and letting
// it go again.
//
// Optional, and the card says so: a server nobody claims serves its library
// exactly as it always has. What claiming buys is being able to invite
// somebody without making them an account here, and what it never buys is the
// service deciding anything -- which libraries a guest reaches is set on the
// Users page, on this machine.
function accountServiceCard(account) {
  const data = account || {};
  if (data.supported === false) {
    return el('div', { class: 'card' },
      el('h2', { text: 'Bask account' }),
      el('p', { class: 'muted', text: 'This server was built without account-service support, so it cannot be claimed. Local accounts and the tokens clients hold work as they always have.' }));
  }

  if (data.linked && data.link) {
    const addresses = el('textarea', {
      id: 'cloud-addresses', rows: '3', spellcheck: 'false',
      placeholder: 'https://bask.example.com\nhttp://192.168.1.10:32400',
    });
    return el('div', { class: 'card' },
      el('h2', { text: 'Bask account' }),
      el('table', {}, el('tbody', {},
        el('tr', {}, el('th', { text: 'Service' }), el('td', { class: 'mono', text: data.link.issuer })),
        el('tr', {}, el('th', { text: 'This server' }), el('td', { class: 'mono', text: data.link.serverId })),
        el('tr', {}, el('th', { text: 'Claimed' }), el('td', { class: 'muted', text: when(data.link.claimedAt) })))),
      data.waiting
        ? el('p', {}, el('a', { href: '#/users',
          text: data.waiting === 1
            ? 'One account is waiting to be allowed →'
            : data.waiting + ' accounts are waiting to be allowed →' }))
        : el('p', { class: 'muted', text: 'Nobody is waiting. An invited account appears on the Users page the first time it asks for something, and reaches nothing until you allow it.' }),
      el('h3', { text: 'Where this server can be reached' }),
      el('label', { for: 'cloud-addresses', text: 'One address per line' }), addresses,
      el('p', { class: 'muted', text: 'The service hands these to clients and never connects to them, so it never learns whether they work. A client tries them in order. Nothing here is proxied: reaching this server is still your arrangement -- a tunnel, a tailnet, a forwarded port.' }),
      el('div', { class: 'row' },
        el('button', {
          class: 'primary',
          text: 'Publish addresses',
          on: {
            click: async () => {
              const lines = addresses.value.split('\n').map((line) => line.trim()).filter(Boolean);
              if (!lines.length) { toast('That needs at least one address.', true); return; }
              try {
                await api('PUT', '/cloud/addresses', { addresses: lines });
                toast('Published.');
              } catch (err) { failed(err); }
            },
          },
        }),
        el('button', {
          class: 'danger',
          text: 'Unlink this server',
          on: {
            click: async () => {
              if (!confirm('Unlink this server?\n\nInvited accounts stop being able to sign in. The accounts themselves stay, with whatever libraries you granted them, so claiming again does not undo your work.')) return;
              try {
                await api('DELETE', '/cloud');
                toast('Unlinked.');
                render();
              } catch (err) { failed(err); }
            },
          },
        })));
  }

  const issuer = el('input', { id: 'cloud-issuer', value: 'https://account.baskmedia.app', spellcheck: 'false' });
  const token = el('input', { id: 'cloud-claim', type: 'password', placeholder: 'claim-...', spellcheck: 'false' });
  const name = el('input', { id: 'cloud-name', placeholder: 'the name your account will show' });
  return el('div', { class: 'card' },
    el('h2', { text: 'Bask account' }),
    el('p', { class: 'muted', text: 'Claiming this server lets you invite somebody without giving them an account on this machine. It is optional, and nothing changes for the clients you already have.' }),
    el('label', { for: 'cloud-issuer', text: 'Account service' }), issuer,
    el('label', { for: 'cloud-claim', text: 'Claim token' }), token,
    el('label', { for: 'cloud-name', text: 'Name for this server' }), name,
    el('p', { class: 'muted', text: 'Sign in to the account service in a browser and ask it for a claim token, then paste it here. It is good for four minutes and once only, so it is expired by the time it reaches anybody\'s scrollback -- and you never type your account password into this server.' }),
    el('div', { class: 'row' }, el('button', {
      class: 'primary',
      text: 'Claim this server',
      on: {
        click: async () => {
          if (!token.value.trim()) { toast('That needs a claim token.', true); return; }
          try {
            await api('POST', '/cloud/claim', {
              issuer: issuer.value.trim(),
              token: token.value.trim(),
              name: name.value.trim(),
            });
            token.value = '';
            toast('Claimed.');
            render();
          } catch (err) { failed(err); }
        },
      },
    })));
}

// webhookCard is where this server tells somewhere else what happened.
//
// The page never shows a token or a secret, only whether one is set: this is
// the screen most likely to be shared, screenshotted or screen-recorded
// during a support conversation, and those are credentials for somebody
// else's service.
function webhookCard(data) {
  const hooks = (data && data.webhooks) || [];
  const families = (data && data.families) || ['playback', 'library', 'job'];

  const rows = hooks.map((hook) => {
    const subscribed = hook.events || [];
    const boxes = [];
    const saveEvents = async () => {
      const wanted = [];
      for (const [family, node] of boxes) if (node.checked) wanted.push(family + '.*');
      try {
        // Everything ticked is stored as everything rather than as three
        // families, so a family added in a later version still reaches a
        // destination that asked for the lot.
        await api('PATCH', '/webhooks/' + hook.id, {
          events: wanted.length === families.length ? [] : wanted,
        });
        toast('Saved.');
      } catch (err) { failed(err); render(); }
    };
    for (const family of families) {
      const box = el('input', { type: 'checkbox' });
      box.checked = subscribed.length === 0 || subscribed.indexOf(family + '.*') >= 0;
      box.addEventListener('change', saveEvents);
      boxes.push([family, box]);
    }

    const result = hook.lastAt
      ? el('span', { class: 'badge ' + (hook.lastError ? 'failed' : 'done'),
        text: hook.lastError ? (hook.lastStatus ? String(hook.lastStatus) : 'failed') : String(hook.lastStatus) })
      : el('span', { class: 'muted', text: 'never sent' });

    return el('tr', {},
      el('th', {},
        el('div', { class: 'mono', text: hook.url }),
        el('div', { class: 'muted', text: hook.format === 'ntfy' ? 'one line of text, for a notification service' : 'a JSON object' }
        )),
      el('td', {}, el('div', { class: 'row' }, boxes.map(([family, box]) =>
        el('label', { class: 'muted' }, box, ' ' + family)))),
      el('td', {},
        result,
        hook.lastAt ? el('div', { class: 'muted', text: when(hook.lastAt) }) : null,
        hook.lastError ? el('div', { class: 'muted', text: hook.lastError }) : null,
        hook.failures > 1 ? el('div', { class: 'muted', text: hook.failures + ' failures in a row' }) : null,
        hook.hasSecret ? el('div', { class: 'muted', text: 'signed' }) : null,
        hook.hasToken ? el('div', { class: 'muted', text: 'sends a token' }) : null),
      el('td', { class: 'right' }, el('div', { class: 'row actions' },
        el('button', {
          text: 'Test',
          on: {
            click: async () => {
              try {
                const sent = await api('POST', '/webhooks/' + hook.id + '/test', {});
                if (sent.delivered) toast('Delivered, and it answered ' + sent.status + '.');
                else toast('Not delivered: ' + (sent.error || 'no answer') + '.', true);
                render();
              } catch (err) { failed(err); }
            },
          },
        }),
        el('button', {
          text: hook.enabled ? 'Switch off' : 'Switch on',
          on: {
            click: async () => {
              try {
                await api('PATCH', '/webhooks/' + hook.id, { enabled: !hook.enabled });
                toast(hook.enabled ? 'Switched off. Nothing is sent to it.' : 'Switched on.');
                render();
              } catch (err) { failed(err); }
            },
          },
        }),
        el('button', {
          class: 'danger',
          text: 'Remove',
          on: {
            click: async () => {
              if (!confirm('Remove ' + hook.url + '?\n\nIts address, token and secret are forgotten.')) return;
              try {
                await api('DELETE', '/webhooks/' + hook.id);
                toast('Removed.');
                render();
              } catch (err) { failed(err); }
            },
          },
        }))));
  });

  const url = el('input', { id: 'hook-url', placeholder: 'http://127.0.0.1:7810/media', spellcheck: 'false' });
  const format = el('select', { id: 'hook-format' },
    el('option', { value: 'json', text: 'JSON, for something that parses it' }),
    el('option', { value: 'ntfy', text: 'One line of text, for ntfy' }));
  const token = el('input', { id: 'hook-token', type: 'password', placeholder: 'only if the receiver wants one', spellcheck: 'false' });
  const secret = el('input', { id: 'hook-secret', type: 'password', placeholder: 'only if you want the body signed', spellcheck: 'false' });

  return el('div', { class: 'card' },
    el('h2', { text: 'Webhooks' }),
    el('p', { class: 'muted', text: 'This server posts to these addresses when something happens: a play, a pause, '
      + 'a stop, a new item in the library, a background pass that finished or failed. Nothing is sent about where '
      + 'anybody is up to — a position report every ten seconds is not news.' }),
    rows.length
      ? el('table', {}, el('tbody', {}, rows))
      : el('p', { class: 'muted', text: 'Nothing is told anything yet.' }),
    data && data.sending === false
      ? el('p', { class: 'muted', text: 'This server was built without a notifier, so nothing is actually sent.' })
      : null,
    el('h3', { text: 'Add one' }),
    el('label', { for: 'hook-url', text: 'Address' }), url,
    el('label', { for: 'hook-format', text: 'Shape' }), format,
    el('label', { for: 'hook-token', text: 'Bearer token' }), token,
    el('label', { for: 'hook-secret', text: 'Signing secret' }), secret,
    el('p', { class: 'muted', text: 'A new destination hears about everything; untick what it should not. '
      + 'A secret is how a receiver on the open internet can tell a real event from anybody who learned the address: '
      + 'the body arrives with an X-Bask-Signature header, which is HMAC-SHA256 of the body under that secret.' }),
    el('div', { class: 'row' }, el('button', {
      class: 'primary',
      text: 'Add',
      on: {
        click: async () => {
          if (!url.value.trim()) { toast('That needs an address.', true); return; }
          try {
            await api('POST', '/webhooks', {
              url: url.value.trim(),
              format: format.value,
              token: token.value,
              secret: secret.value,
            });
            url.value = ''; token.value = ''; secret.value = '';
            toast('Added. Press Test to see whether it answers.');
            render();
          } catch (err) { failed(err); }
        },
      },
    })));
}

// backupCard is the database's copies: what there is, and one button.
//
// The database is the one thing on this server that cannot be rebuilt — the
// media is on disk and a scan finds it again, while watched state, resume
// positions, playlists and tokens exist nowhere else.
function backupCard(backups) {
  const list = (backups && backups.backups) || [];
  const rows = list.map((backup) => el('tr', {},
    el('th', { class: 'mono', text: backup.name }),
    el('td', { class: 'muted', text: when(backup.taken) }),
    el('td', { class: 'muted', text: backup.sizeMB + ' MB' }),
    el('td', { class: 'right' }, el('div', { class: 'row actions' },
      el('a', { class: 'button', href: '/admin/api/backups/' + encodeURIComponent(backup.name), text: 'Download' }),
      el('button', {
        class: 'danger',
        text: 'Remove',
        on: {
          click: async () => {
            if (!confirm('Remove ' + backup.name + '?\n\nThe database itself is untouched. This deletes one copy of it.')) return;
            try {
              await api('DELETE', '/backups/' + encodeURIComponent(backup.name));
              toast('Removed.');
              render();
            } catch (err) { failed(err); }
          },
        },
      })))));

  return el('div', { class: 'card' },
    el('h2', { text: 'Backups' }),
    el('p', { class: 'muted', text: 'Copies of the database, written while the server runs. '
      + 'They go in ' + ((backups && backups.directory) || 'a folder beside the database')
      + ', and the newest ' + ((backups && backups.keep) || 7) + ' are kept.' }),
    el('div', { class: 'row' }, el('button', {
      class: 'primary',
      text: 'Back up now',
      on: {
        click: async () => {
          try {
            await api('POST', '/backups');
            toast('Backing up.');
            location.hash = '#/jobs';
          } catch (err) { failed(err); }
        },
      },
    })),
    rows.length
      ? el('table', {}, el('tbody', {}, rows))
      : el('p', { class: 'muted', text: 'Nothing has been backed up yet.' }),
    el('p', { class: 'muted', text: 'A copy on the same disk protects against a mistake, not against the disk. Download one somewhere else now and then.' }));
}

/* ---------- start ---------- */

(async function () {
  try {
    const me = await api('GET', '/session');
    showApp();
    identify(me);
    render();
  } catch (err) {
    // A 401 has already shown the sign-in form; anything else is a server
    // that cannot answer, and the form is still the right place to wait.
    showSignIn();
  }
})();
