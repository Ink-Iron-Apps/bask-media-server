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

/* ---------- overview ---------- */

views.overview = async function (main) {
  const draw = async () => {
    const o = await api('GET', '/overview');
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
    if (running.length && !polling) poll(draw);
    if (!running.length) stopPolling();
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
      el('td', { class: 'muted', text: when(sec.scannedAt) }),
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

function editSection(sec) {
  const title = el('input', { value: sec.title });
  const roots = el('input', { value: (sec.roots || []).join(' | '), spellcheck: 'false' });
  const picker = el('div', {});

  const card = el('div', { class: 'card' },
    el('h2', { text: 'Edit ' + sec.title }),
    el('label', { text: 'Name' }), title,
    el('label', { text: 'Folders, separated by |' }), roots,
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

/* ---------- users ---------- */

views.users = async function (main) {
  const users = (await api('GET', '/users')) || [];

  const rows = users.map((user) => el('tr', {},
    el('td', {}, el('strong', { text: user.username }), user.self ? el('span', { class: 'muted', text: ' (you)' }) : null),
    el('td', {}, user.admin ? el('span', { class: 'badge done', text: 'administrator' }) : el('span', { class: 'badge', text: 'viewer' })),
    el('td', { class: 'muted', text: count(user.tokens, 'token') }),
    el('td', { class: 'muted', text: when(user.createdAt) }),
    el('td', { class: 'right' }, el('div', { class: 'row actions' },
      el('button', { text: 'Tokens', on: { click: () => showTokens(user) } }),
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
      })))));

  const name = el('input', { id: 'new-user', placeholder: 'kitchen-tv' });
  const admin = el('select', { id: 'new-user-admin' },
    el('option', { value: 'viewer', text: 'Viewer' }),
    el('option', { value: 'admin', text: 'Administrator' }));

  fill(main,
    el('div', { class: 'head' }, el('div', {}, el('h1', { text: 'Users' }),
      el('p', { class: 'muted', text: 'Accounts live on this server. There is no account service anywhere else.' }))),
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

async function showTokens(user) {
  try {
    const tokens = (await api('GET', '/users/' + user.id + '/tokens')) || [];
    const label = el('input', { placeholder: 'living room' });

    const rows = tokens.map((t) => el('tr', {},
      el('td', { text: t.label || '—' }),
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
      }))));

    const main = document.getElementById('main');
    fill(main,
      el('div', { class: 'head' }, el('div', {}, el('h1', { text: user.username }),
        el('p', { class: 'muted', text: 'One token per device is worth the minute it costs: revoking one then signs out one television.' })),
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

/* ---------- settings ---------- */

views.settings = async function (main) {
  const settings = await api('GET', '/settings');
  const name = el('input', { id: 'server-name', value: settings.name || '' });
  const key = el('input', { id: 'tmdb-key', type: 'password', placeholder: settings.tmdbKeyStored ? 'stored' : 'not set', spellcheck: 'false' });

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
      el('h2', { text: 'Tools found on this machine' }),
      el('table', {}, el('tbody', {}, tools)),
      el('p', { class: 'muted', text: 'Both are optional. Without them the library still scans, browses and plays; it has no durations, chapters or detected intros.' })),
    el('div', { class: 'card' },
      el('h2', { text: 'This sign-in' }),
      el('p', { class: 'muted', text: 'A browser session lasts ' + settings.sessionDays + ' days and is separate from the tokens clients hold, so signing out here signs nothing else out.' })));
};

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
