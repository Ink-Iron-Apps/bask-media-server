// The viewing interface.
//
// Written against the browser's own APIs, compiled into the server binary,
// and deliberately small: it browses the library, plays what this browser can
// decode, and says so plainly when it cannot, because the server does not
// transcode and a spinner that never resolves is a worse answer than a
// sentence.

'use strict';

const API = '/web/api';

/* ---------- plumbing ---------- */

async function api(method, path, body) {
  const headers = { 'X-BMS-Web': '1' };
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

function fill(parent, ...nodes) {
  // Flattened like el()'s children, because replaceChildren turns an array
  // into the string "[object HTMLButtonElement]" rather than appending what
  // is in it — a list that silently renders as text instead of buttons.
  parent.replaceChildren(...nodes.flat().filter((node) => node != null && node !== false));
}

let toastTimer = null;
function toast(message, bad) {
  const box = document.getElementById('toast');
  box.textContent = message;
  box.classList.toggle('bad', !!bad);
  box.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { box.hidden = true; }, bad ? 7000 : 3000);
}

function failed(err) {
  if (err && err.message === 'signed out') return;
  toast(err && err.message ? err.message : String(err), true);
}

/* ---------- formatting ---------- */

function clock(ms) {
  const total = Math.round((ms || 0) / 1000);
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  if (h) return h + ':' + String(m).padStart(2, '0') + ':' + String(s).padStart(2, '0');
  return m + ':' + String(s).padStart(2, '0');
}

function runtime(ms) {
  const minutes = Math.round((ms || 0) / 60000);
  if (!minutes) return '';
  if (minutes < 60) return minutes + ' min';
  return Math.floor(minutes / 60) + 'h ' + (minutes % 60) + 'm';
}

/* ---------- sign-in ---------- */

function showSignIn() {
  closePlayer();
  document.getElementById('app').hidden = true;
  document.getElementById('signin').hidden = false;
  document.getElementById('token').focus();
}

function showApp(me) {
  document.getElementById('signin').hidden = true;
  document.getElementById('app').hidden = false;
  document.getElementById('who').textContent = me && me.username ? me.username : '';
  // The way back to administration, for the account that has it.
  document.getElementById('to-admin').hidden = !(me && me.admin);
}

document.getElementById('signin-form').addEventListener('submit', async (event) => {
  event.preventDefault();
  const box = document.getElementById('signin-error');
  box.hidden = true;
  const input = document.getElementById('token');
  try {
    const me = await api('POST', '/session', { token: input.value });
    input.value = '';
    showApp(me);
    render();
  } catch (err) {
    box.textContent = err.message;
    box.hidden = false;
  }
});

document.getElementById('signout').addEventListener('click', async () => {
  try { await api('DELETE', '/session'); } catch (err) { /* either way, the page is done */ }
  showSignIn();
});

document.getElementById('search-form').addEventListener('submit', (event) => {
  event.preventDefault();
  const query = document.getElementById('search').value.trim();
  location.hash = query ? '#/search/' + encodeURIComponent(query) : '#/';
});

/* ---------- posters ---------- */

// square names the things that are not film posters: a cover, an artist, an
// album of photographs. Asking for the wrong shape is not only ugly — the
// server renders and caches whatever size is asked for, so a poster-shaped
// request for a square cover is a second rendition of the same picture.
function square(kind) {
  return kind === 'artist' || kind === 'album' || kind === 'track' ||
    kind === 'photoalbum' || kind === 'photo';
}

function poster(item, width) {
  const box = el('div', { class: 'poster' + (square(item.kind) ? ' square' : '') });
  if (item.poster) {
    box.append(el('img', {
      src: '/web/image/' + item.id + '?w=' + width + '&h=' +
        (square(item.kind) ? width : Math.round(width * 1.5)),
      alt: '', loading: 'lazy',
    }));
  } else {
    box.append(el('div', { class: 'letter', text: (item.title || '?').slice(0, 1).toUpperCase() }));
  }
  if (item.watched) box.append(el('span', { class: 'badge-watched', text: 'watched' }));
  if (item.offsetMs > 0 && item.durationMs > 0) {
    // A <progress> rather than a div with a width: the page's content
    // security policy forbids inline styles, and an element that carries its
    // own value needs none.
    box.append(el('progress', {
      class: 'progress',
      value: Math.round(Math.min(100, (item.offsetMs / item.durationMs) * 100)),
      max: 100,
      'aria-label': 'how far through',
    }));
  }
  return box;
}

function card(item) {
  if (item.kind === 'track') {
    // A track is played, not opened: there is no page for one, and a search
    // that found it should start it rather than offer a dead end.
    return el('button', { class: 'card', on: { click: () => playTracks([item], 0, null) } },
      poster(item, 300),
      el('div', { class: 'title', text: item.title }),
      el('div', { class: 'sub', text: item.subtitle || '' }));
  }
  return el('a', { class: 'card', href: '#/item/' + item.id },
    poster(item, 300),
    el('div', { class: 'title', text: item.title }),
    el('div', { class: 'sub', text: item.subtitle || (item.year ? String(item.year) : '') }));
}

function grid(items) {
  return el('div', { class: 'grid' }, items.map(card));
}

/* ---------- views ---------- */

const main = () => document.getElementById('main');

async function render() {
  const hash = location.hash || '#/';
  markLibrary(hash);
  try {
    if (hash.startsWith('#/library/')) return await libraryView(Number(hash.slice(10)));
    if (hash.startsWith('#/item/')) return await itemView(Number(hash.slice(7)));
    if (hash.startsWith('#/search/')) return await searchView(decodeURIComponent(hash.slice(9)));
    if (hash.startsWith('#/playlist/')) return await playlistView(Number(hash.slice(11)));
    if (hash.startsWith('#/collection/')) return await collectionView(Number(hash.slice(13)));
    if (hash === '#/favourites') return await favouritesView();
    return await homeView();
  } catch (err) {
    fill(main(), el('p', { class: 'error', text: err.message }));
    failed(err);
  }
}

function markLibrary(hash) {
  for (const link of document.querySelectorAll('#libraries a')) {
    link.classList.toggle('on', link.getAttribute('href') === hash);
  }
}

async function homeView() {
  const home = await api('GET', '/home');
  document.getElementById('brand-name').textContent = home.name || 'Bask';
  document.getElementById('signin-name').textContent = home.name || 'Bask';

  fill(document.getElementById('libraries'), ...(home.sections || []).map((section) =>
    el('a', { href: '#/library/' + section.id, text: section.title })));
  markLibrary(location.hash || '#/');

  const resume = home.continue || [];

  // Asked for together rather than one after another: three small queries
  // that do not depend on each other should not be three round trips.
  const [starred, lists, groups] = await Promise.all([
    api('GET', '/favourites?size=12').catch(() => ({ items: [] })),
    api('GET', '/playlists').catch(() => ({ playlists: [] })),
    api('GET', '/collections').catch(() => ({ collections: [] })),
  ]);
  const favourites = starred.items || [];
  const playlists = lists.playlists || [];
  const collections = groups.collections || [];

  fill(main(),
    resume.length ? el('h2', { text: 'Where you left off' }) : null,
    resume.length ? grid(resume) : null,

    favourites.length ? el('h2', {},
      'Favourites',
      el('a', { class: 'more', href: '#/favourites', text: 'All' })) : null,
    favourites.length ? grid(favourites) : null,

    playlists.length ? el('h2', { text: 'Playlists' }) : null,
    playlists.length ? el('div', { class: 'row' }, playlists.map((list) =>
      el('a', {
        class: 'button',
        href: '#/playlist/' + list.id,
        text: list.title + ' · ' + list.entries + (list.entries === 1 ? ' item' : ' items'),
      }))) : null,

    collections.length ? el('h2', { text: 'Collections' }) : null,
    collections.length ? el('div', { class: 'row' }, collections.map((c) =>
      el('a', { class: 'button', href: '#/collection/' + c.id, text: c.title }))) : null,

    el('h2', { text: 'Libraries' }),
    (home.sections || []).length
      ? el('div', { class: 'row' }, (home.sections || []).map((section) =>
        el('a', { class: 'button', href: '#/library/' + section.id, text: section.title })))
      : el('p', { class: 'empty', text: 'No libraries yet. Whoever runs this server adds them in the admin interface.' }));
}

async function favouritesView() {
  const page = await api('GET', '/favourites?size=200');
  const items = page.items || [];
  fill(main(),
    el('h1', { text: 'Favourites' }),
    items.length ? grid(items)
      : el('p', { class: 'empty', text: 'Nothing starred yet. The star is on a film or an episode page.' }));
}

async function collectionView(id) {
  const page = await api('GET', '/collections/' + id + '?size=200');
  const items = page.items || [];
  fill(main(),
    el('h1', { text: page.collection.title }),
    page.collection.summary ? el('p', { class: 'summary', text: page.collection.summary }) : null,
    items.length ? grid(items) : el('p', { class: 'empty', text: 'Nothing in it.' }));
}

// playlistView is the one list that is ordered and editable, so it is a list
// of rows rather than a grid: a row has somewhere to put a remove button and
// a grid of posters does not.
async function playlistView(id) {
  const page = await api('GET', '/playlists/' + id + '?size=500');
  const lines = page.lines || [];

  const rows = lines.map((line, index) => el('div', { class: 'episode-row' },
    el('button', {
      class: 'episode',
      on: { click: () => { location.hash = '#/item/' + line.item.id; } },
    }, el('span', { class: 'n', text: String(index + 1) }),
      el('span', { class: 'title', text: line.item.title }),
      line.item.subtitle ? el('span', { class: 'sub', text: line.item.subtitle }) : null),
    el('button', {
      class: 'ghost',
      title: 'Remove from this playlist',
      text: '✕',
      on: {
        click: async () => {
          try {
            await api('DELETE', '/playlists/' + id + '/items/' + line.entry);
            render();
          } catch (err) { failed(err); }
        },
      },
    })));

  fill(main(),
    el('h1', { text: page.playlist.title }),
    el('p', { class: 'meta', text: page.playlist.entries + (page.playlist.entries === 1 ? ' item' : ' items') }),
    lines.length ? el('div', { class: 'episodes' }, rows)
      : el('p', { class: 'empty', text: 'Nothing in it yet.' }),
    el('div', { class: 'row actions' }, el('button', {
      class: 'ghost',
      text: 'Delete this playlist',
      on: {
        click: async () => {
          if (!confirm('Delete "' + page.playlist.title + '"?\n\nThe list goes. Nothing in your library is touched.')) return;
          try {
            await api('DELETE', '/playlists/' + id);
            location.hash = '#/';
          } catch (err) { failed(err); }
        },
      },
    })));
}

// starButton is the one control that appears on every playable page.
function starButton(item) {
  let on = !!item.favourite;
  const button = el('button', {
    class: 'ghost',
    title: 'Favourite',
    text: on ? '★' : '☆',
  });
  button.addEventListener('click', async () => {
    try {
      const reply = await api(on ? 'DELETE' : 'POST', '/favourites/' + item.id);
      on = !!reply.favourite;
      button.textContent = on ? '★' : '☆';
    } catch (err) { failed(err); }
  });
  return button;
}

// addToPlaylistButton asks which list, offering to make one.
function addToPlaylistButton(item) {
  return el('button', {
    class: 'ghost',
    text: 'Add to playlist',
    on: {
      click: async () => {
        try {
          const lists = (await api('GET', '/playlists')).playlists || [];
          const names = lists.map((list, i) => (i + 1) + '. ' + list.title).join('\n');
          const answer = prompt(
            (lists.length ? 'Which playlist?\n' + names + '\n\n' : '') +
            'Type a number, or a name for a new one.');
          if (answer === null || answer.trim() === '') return;

          const picked = lists[Number(answer.trim()) - 1];
          if (picked) {
            await api('POST', '/playlists/' + picked.id + '/items', { items: [item.id] });
            toast('Added to ' + picked.title + '.');
            return;
          }
          const made = await api('POST', '/playlists', { title: answer.trim(), items: [item.id] });
          toast('Added to ' + made.title + '.');
        } catch (err) { failed(err); }
      },
    },
  });
}

// holds is what one of these libraries is full of, said properly.
function holds(kind, n) {
  const word = kind === 'shows' ? 'show'
    : kind === 'music' ? 'artist'
      : kind === 'photos' ? 'album'
        : 'film';
  if (word === 'film') return n + (n === 1 ? ' film' : ' films');
  return n + ' ' + word + (n === 1 ? '' : 's');
}

async function libraryView(id) {
  const page = await api('GET', '/sections/' + id + '?size=120');
  fill(main(),
    el('h1', { text: page.title }),
    el('p', { class: 'muted', text: holds(page.kind, page.total) }),
    page.items.length ? grid(page.items) : el('p', { class: 'empty', text: 'Nothing here yet. It may not have been scanned.' }));

  // More of the library, a page at a time, because the server pages and so
  // should the scrolling.
  if (page.items.length < page.total) {
    const more = el('button', {
      text: 'Show more',
      on: {
        click: async () => {
          more.disabled = true;
          try {
            const next = await api('GET', '/sections/' + id + '?size=120&start=' + document.querySelectorAll('.grid .card').length);
            document.querySelector('.grid').append(...next.items.map(card));
            if (document.querySelectorAll('.grid .card').length >= next.total) more.remove();
            else more.disabled = false;
          } catch (err) { failed(err); more.disabled = false; }
        },
      },
    });
    main().append(el('div', { class: 'row more' }, more));
  }
}

async function searchView(query) {
  document.getElementById('search').value = query;
  const found = await api('GET', '/search?q=' + encodeURIComponent(query));
  fill(main(),
    el('h1', { text: 'Search' }),
    el('p', { class: 'muted', text: found.items.length + ' for “' + query + '”' }),
    found.items.length ? grid(found.items) : el('p', { class: 'empty', text: 'Nothing matched.' }));
}

async function itemView(id) {
  const detail = await api('GET', '/items/' + id);
  const item = detail.item;
  const children = detail.children || [];
  const parts = detail.parts || [];

  // Music and photographs are browsed differently enough that sharing the
  // film hero would mean a page of hedged conditionals: an album is a list of
  // tracks with one cover, and a photo album is a wall of pictures.
  if (item.kind === 'album') return albumView(detail);
  if (item.kind === 'artist') return artistView(detail);
  if (item.kind === 'photoalbum') return photoAlbumView(detail);
  if (item.kind === 'photo') return photoView(detail);

  const hero = el('div', { class: 'hero' + (item.art ? ' with-art' : '') });
  if (item.art) hero.append(el('img', { src: '/web/image/' + item.id + '?kind=art&w=1280', alt: '' }));

  const meta = [item.subtitle, item.year ? String(item.year) : '', runtime(item.durationMs)]
    .filter(Boolean).join(' · ');

  const actions = el('div', { class: 'row actions' });
  if (parts.length) {
    const resuming = item.offsetMs > 5000 && !item.watched;
    actions.append(el('button', {
      class: 'primary',
      text: resuming ? 'Resume at ' + clock(item.offsetMs) : 'Play',
      on: { click: () => play(detail, resuming ? item.offsetMs : 0) },
    }));
    if (resuming) {
      actions.append(el('button', { text: 'Play from the start', on: { click: () => play(detail, 0) } }));
    }
  }
  if (detail.parent) {
    actions.append(el('a', { class: 'button', href: '#/item/' + detail.parent.id, text: '‹ ' + detail.parent.title }));
  }
  actions.append(starButton(item));
  if (parts.length) actions.append(addToPlaylistButton(item));

  // The trailer goes beside Play, because that is what somebody deciding
  // whether to watch something is looking for; everything else goes in a
  // shelf below.
  const bonus = detail.extras || [];
  const trailer = bonus.find((extra) => extra.sort === 'trailer');
  if (trailer) {
    actions.append(el('button', { text: 'Trailer', on: { click: () => playExtra(trailer.item.id) } }));
  }

  hero.append(el('div', { class: 'over' },
    el('h1', { text: item.title }),
    meta ? el('div', { class: 'meta', text: meta }) : null,
    item.summary ? el('p', { class: 'summary', text: item.summary }) : null,
    actions));

  const episodes = children.filter((child) => child.kind === 'episode');
  fill(main(),
    hero,
    extrasShelf(bonus),
    children.length && !episodes.length ? el('h2', { text: 'Seasons' }) : null,
    children.length && !episodes.length ? grid(children) : null,
    episodes.length ? el('h2', { text: episodes.length + (episodes.length === 1 ? ' episode' : ' episodes') }) : null,
    episodes.length ? el('div', { class: 'episodes' }, episodes.map((episode) => el('button', {
      class: 'episode',
      on: { click: () => location.hash = '#/item/' + episode.id },
    },
    el('span', { class: 'n', text: 'E' + episode.index }),
    el('span', { class: 'body' },
      el('div', { text: episode.title }),
      el('div', { class: 'sum', text: episode.summary || '' })),
    el('span', { class: 'muted', text: episode.watched ? 'watched' : (episode.offsetMs ? clock(episode.offsetMs) : runtime(episode.durationMs)) })))) : null);
}

// extrasShelf is the trailers, featurettes and deleted scenes under an item.
//
// Labelled by sort rather than grouped under headings: a film has three or
// four of these, and three headings over one row each is more page furniture
// than content.
function extrasShelf(bonus) {
  if (!bonus.length) return null;
  return el('div', {},
    el('h2', { text: bonus.length === 1 ? 'Extra' : 'Extras' }),
    el('div', { class: 'episodes' }, bonus.map((extra) => el('button', {
      class: 'episode',
      on: { click: () => playExtra(extra.item.id) },
    },
    el('span', { class: 'n', text: '▶' }),
    el('span', { class: 'body' },
      el('div', { text: extra.item.title }),
      el('div', { class: 'sum', text: extra.label })),
    el('span', { class: 'muted', text: runtime(extra.item.durationMs) })))));
}

// playExtra plays one, by asking for it the way anything else is asked for:
// an extra is an item with a file, so the ordinary player handles it.
async function playExtra(id) {
  try {
    const detail = await api('GET', '/items/' + id);
    play(detail, 0);
  } catch (err) { failed(err); }
}

/* ---------- music ---------- */

async function artistView(detail) {
  const item = detail.item;
  const albums = (detail.children || []).filter((child) => child.kind === 'album');

  fill(main(),
    el('div', { class: 'head' },
      el('h1', { text: item.title }),
      el('p', { class: 'muted', text: count(albums.length, 'album') })),
    albums.length ? grid(albums) : el('p', { class: 'empty', text: 'Nothing here yet.' }));
}

async function albumView(detail) {
  const item = detail.item;
  const tracks = (detail.children || []).filter((child) => child.kind === 'track');
  const total = tracks.reduce((sum, track) => sum + (track.durationMs || 0), 0);

  const meta = [item.subtitle, item.year ? String(item.year) : '',
    count(tracks.length, 'track'), runtime(total)].filter(Boolean).join(' · ');

  const head = el('div', { class: 'album-head' },
    poster(item, 300),
    el('div', { class: 'about' },
      el('h1', { text: item.title }),
      el('div', { class: 'meta', text: meta }),
      el('div', { class: 'row actions' },
        tracks.length
          ? el('button', { class: 'primary', text: 'Play album', on: { click: () => playTracks(tracks, 0, item) } })
          : null,
        detail.parent
          ? el('a', { class: 'button', href: '#/item/' + detail.parent.id, text: '‹ ' + detail.parent.title })
          : null)));

  // Discs only where there is more than one, because "Disc 1" above an album
  // that has one disc is a heading that says nothing.
  const discs = [...new Set(tracks.map((track) => track.disc || 0))];
  const rows = [];
  for (const disc of discs) {
    if (discs.length > 1) rows.push(el('h2', { class: 'disc', text: 'Disc ' + (disc || 1) }));
    for (const track of tracks.filter((t) => (t.disc || 0) === disc)) {
      rows.push(trackRow(track, tracks, item));
    }
  }

  fill(main(), head, tracks.length ? el('div', { class: 'tracks' }, rows) : el('p', { class: 'empty', text: 'No tracks.' }));
  markPlaying();
}

function trackRow(track, queue, album) {
  const row = el('button', {
    class: 'track',
    'data-track': track.id,
    on: { click: () => playTracks(queue, queue.indexOf(track), album) },
  },
  el('span', { class: 'n', text: track.index ? String(track.index) : '·' }),
  el('span', { class: 'body' },
    el('div', { text: track.title }),
    // Who plays it, and only where that is not the album's own artist: on a
    // compilation this is the only thing that tells the rows apart, and on
    // every other album it would repeat the title at the top of the page.
    track.performer ? el('div', { class: 'sum', text: track.performer }) : null),
  el('span', { class: 'muted mono', text: clock(track.durationMs) }));
  return row;
}

// markPlaying highlights the row that is sounding, so a list of twenty tracks
// says which one you are hearing.
function markPlaying() {
  const id = queue.tracks.length ? String(queue.tracks[queue.at].id) : '';
  for (const row of document.querySelectorAll('.track')) {
    row.classList.toggle('on', row.dataset.track === id);
  }
}

/* ---------- the audio player ---------- */

const audio = document.getElementById('audio');
const audioBox = document.getElementById('audio-player');
const queue = { tracks: [], at: 0, album: null, session: null, timer: null, hls: null };

async function playTracks(tracks, index, album) {
  if (!tracks.length) return;
  queue.tracks = tracks;
  queue.at = Math.max(0, Math.min(index, tracks.length - 1));
  queue.album = album || queue.album;
  await startTrack(0);
}

// startTrack plays whatever the queue is pointing at. offsetMs resumes.
async function startTrack(offsetMs) {
  const track = queue.tracks[queue.at];
  if (!track) return closeAudio();

  stopReporting();
  detachAudioHls();
  audioBox.hidden = false;
  document.getElementById('audio-title').textContent = track.title;
  document.getElementById('audio-sub').textContent =
    track.subtitle || (queue.album ? queue.album.title : '');
  document.getElementById('audio-mode').textContent = '';

  const art = document.getElementById('audio-cover');
  const cover = queue.album && queue.album.poster ? queue.album : (track.poster ? track : null);
  if (cover) art.src = '/web/image/' + cover.id + '?w=120&h=120';
  else art.removeAttribute('src');
  document.getElementById('audio-art').href =
    '#/item/' + (queue.album ? queue.album.id : track.id);

  let answer;
  try {
    answer = await api('POST', '/play', {
      itemId: track.id,
      startMs: Math.round(offsetMs || 0),
      capabilities: capabilities(),
    });
  } catch (err) {
    failed(err);
    return;
  }
  queue.session = answer.session || null;

  const mode = document.getElementById('audio-mode');
  mode.textContent = answer.mode === 'direct' ? '' : answer.label;
  mode.className = answer.mode === 'direct' ? 'muted' : 'converting';

  if (answer.mode === 'unsupported') {
    toast('This browser cannot play ' + track.title + ': ' + answer.why, true);
    return nextTrack();
  }

  if (answer.hls && !audio.canPlayType('application/vnd.apple.mpegurl')) {
    try {
      const Hls = await loadHls();
      if (!Hls || !Hls.isSupported()) throw new Error('this browser cannot play a converted stream');
      const hls = new Hls({ maxBufferLength: 30, enableWorker: true });
      queue.hls = hls;
      hls.on(Hls.Events.MANIFEST_PARSED, () => audio.play().catch(() => {}));
      hls.on(Hls.Events.ERROR, (event, data) => {
        if (data && data.fatal) toast('The converted stream stopped.', true);
      });
      hls.loadSource(answer.url);
      hls.attachMedia(audio);
    } catch (err) {
      failed(err);
      return;
    }
  } else {
    audio.src = answer.url;
    if (offsetMs > 0) {
      audio.addEventListener('loadedmetadata', function once() {
        audio.removeEventListener('loadedmetadata', once);
        try { audio.currentTime = offsetMs / 1000; } catch (err) { /* already there */ }
      });
    }
    audio.play().catch(() => { /* a browser that wants a click first */ });
  }

  document.getElementById('audio-toggle').textContent = 'Pause';
  // Every ten seconds, the same as the video player: a track that was half
  // heard resumes where it was, on this browser or on the television.
  queue.timer = setInterval(reportAudio, 10000);
  markPlaying();
}

function nextTrack() {
  reportAudio();
  if (queue.at + 1 >= queue.tracks.length) return closeAudio();
  queue.at++;
  return startTrack(0);
}

function previousTrack() {
  // The first few seconds of a track mean "the one before"; after that they
  // mean "start this one again", which is what every player has always done.
  if (audio.currentTime > 5) {
    audio.currentTime = 0;
    return;
  }
  reportAudio();
  if (queue.at === 0) {
    audio.currentTime = 0;
    return;
  }
  queue.at--;
  startTrack(0);
}

function reportAudio() {
  const track = queue.tracks[queue.at];
  if (!track || !audio.duration || Number.isNaN(audio.duration)) return;
  fetch(API + '/progress', {
    method: 'POST',
    headers: { 'X-BMS-Web': '1', 'Content-Type': 'application/json' },
    credentials: 'same-origin',
    body: JSON.stringify({
      itemId: track.id,
      offsetMs: Math.round(audio.currentTime * 1000),
      durationMs: Math.round(audio.duration * 1000),
    }),
    keepalive: true,
  }).catch(() => { /* a lost report costs a few seconds of position */ });
}

function stopReporting() {
  clearInterval(queue.timer);
  queue.timer = null;
}

function detachAudioHls() {
  if (queue.hls) {
    try { queue.hls.destroy(); } catch (err) { /* already gone */ }
    queue.hls = null;
  }
}

function closeAudio() {
  reportAudio();
  stopReporting();
  // Sent whether or not anything was being converted: the encoder is only
  // half of what a stop ends, and the other half is the line on the status
  // page saying somebody is listening to this.
  const track = queue.tracks[queue.at];
  if (track) {
    fetch(API + '/play/' + (queue.session || 'direct') + '/stop?itemId=' + track.id, {
      method: 'POST',
      headers: { 'X-BMS-Web': '1' },
      credentials: 'same-origin',
      keepalive: true,
    }).catch(() => { /* the watchdog is the backstop */ });
  }
  queue.session = null;
  detachAudioHls();
  audio.pause();
  audio.removeAttribute('src');
  audio.load();
  queue.tracks = [];
  audioBox.hidden = true;
  markPlaying();
}

document.getElementById('audio-next').addEventListener('click', nextTrack);
document.getElementById('audio-prev').addEventListener('click', previousTrack);
document.getElementById('audio-close').addEventListener('click', closeAudio);
document.getElementById('audio-toggle').addEventListener('click', () => {
  if (audio.paused) audio.play().catch(() => {});
  else audio.pause();
});
audio.addEventListener('play', () => { document.getElementById('audio-toggle').textContent = 'Pause'; });
audio.addEventListener('pause', () => {
  document.getElementById('audio-toggle').textContent = 'Play';
  reportAudio();
});
audio.addEventListener('ended', nextTrack);
audio.addEventListener('timeupdate', () => {
  const bar = document.getElementById('audio-progress');
  if (audio.duration && !Number.isNaN(audio.duration)) {
    bar.value = Math.round((audio.currentTime / audio.duration) * 100);
  }
  document.getElementById('audio-time').textContent =
    clock(audio.currentTime * 1000) + (audio.duration ? ' / ' + clock(audio.duration * 1000) : '');
});
audio.addEventListener('error', () => {
  const track = queue.tracks[queue.at];
  if (track) toast('Could not play ' + track.title + '.', true);
});
window.addEventListener('beforeunload', () => { if (queue.tracks.length) reportAudio(); });

/* ---------- photographs ---------- */

async function photoAlbumView(detail) {
  const item = detail.item;
  const children = detail.children || [];
  const albums = children.filter((child) => child.kind === 'photoalbum');
  const pictures = children.filter((child) => child.kind === 'photo');

  const meta = [
    albums.length ? count(albums.length, 'album') : '',
    pictures.length ? count(pictures.length, 'photograph') : '',
    span(pictures),
  ].filter(Boolean).join(' · ');

  fill(main(),
    el('div', { class: 'head' },
      el('h1', { text: item.title }),
      meta ? el('p', { class: 'muted', text: meta }) : null,
      detail.parent
        ? el('a', { class: 'button', href: '#/item/' + detail.parent.id, text: '‹ ' + detail.parent.title })
        : null),
    albums.length ? grid(albums) : null,
    pictures.length ? wall(pictures) : null,
    !albums.length && !pictures.length
      ? el('p', { class: 'empty', text: 'Nothing here yet. It may not have been scanned.' })
      : null);
}

// span is the stretch of time an album covers, which is the one fact about a
// folder of photographs that is not already in its name.
function span(pictures) {
  const dates = pictures.map((photo) => photo.takenAt).filter(Boolean).sort();
  if (!dates.length) return '';
  const first = dates[0].slice(0, 4);
  const last = dates[dates.length - 1].slice(0, 4);
  return first === last ? first : first + '–' + last;
}

// wall is the grid of thumbnails. Every picture is a button rather than a
// link, because opening one is opening the viewer over this page and not
// going anywhere else.
function wall(pictures) {
  return el('div', { class: 'wall' }, pictures.map((photo, index) =>
    el('button', {
      class: 'shot',
      title: photo.title,
      on: { click: () => openLightbox(pictures, index) },
    },
    el('img', { src: '/web/image/' + photo.id + '?w=400&h=400', alt: photo.title, loading: 'lazy' }))));
}

async function photoView(detail) {
  // One photograph on its own — reached from a link rather than from its
  // album — opens the viewer over the album it is in, so that the arrows
  // still work.
  const item = detail.item;
  let pictures = [item];
  let at = 0;
  if (detail.parent) {
    try {
      const album = await api('GET', '/items/' + detail.parent.id);
      const found = (album.children || []).filter((child) => child.kind === 'photo');
      const index = found.findIndex((child) => child.id === item.id);
      if (index >= 0) {
        pictures = found;
        at = index;
      }
    } catch (err) { /* the one picture is enough */ }
  }

  fill(main(),
    el('div', { class: 'head' },
      el('h1', { text: item.title }),
      detail.parent
        ? el('a', { class: 'button', href: '#/item/' + detail.parent.id, text: '‹ ' + detail.parent.title })
        : null));
  openLightbox(pictures, at);
}

const lightbox = document.getElementById('lightbox');
let viewing = null; // { pictures, at }

function openLightbox(pictures, index) {
  viewing = { pictures, at: index };
  lightbox.hidden = false;
  showPhoto();
}

function showPhoto() {
  if (!viewing) return;
  const photo = viewing.pictures[viewing.at];
  if (!photo) return closeLightbox();

  // A size rather than the original: the server renders and caches it once,
  // and a phone on a slow link is not sent a twelve-megapixel file to shrink
  // in its own layer. The original is one click away, under Download.
  document.getElementById('lightbox-img').src = '/web/image/' + photo.id + '?w=2000&h=2000';
  document.getElementById('lightbox-img').alt = photo.title;
  document.getElementById('lightbox-title').textContent = photo.title;
  document.getElementById('lightbox-when').textContent = photo.takenAt || '';
  document.getElementById('lightbox-count').textContent =
    (viewing.at + 1) + ' / ' + viewing.pictures.length;
  const file = document.getElementById('lightbox-file');
  file.href = '/web/image/' + photo.id;
  file.setAttribute('download', photo.title);

  // The next one and the one before are fetched now, so that moving through
  // an album is instant rather than a wait per picture.
  for (const near of [viewing.at + 1, viewing.at - 1]) {
    const other = viewing.pictures[near];
    if (other) new Image().src = '/web/image/' + other.id + '?w=2000&h=2000';
  }
}

function stepPhoto(by) {
  if (!viewing) return;
  const at = viewing.at + by;
  if (at < 0 || at >= viewing.pictures.length) return;
  viewing.at = at;
  showPhoto();
}

function closeLightbox() {
  viewing = null;
  lightbox.hidden = true;
  document.getElementById('lightbox-img').removeAttribute('src');
}

document.getElementById('lightbox-back').addEventListener('click', closeLightbox);
document.getElementById('lightbox-next').addEventListener('click', () => stepPhoto(1));
document.getElementById('lightbox-prev').addEventListener('click', () => stepPhoto(-1));
document.addEventListener('keydown', (event) => {
  if (lightbox.hidden) return;
  if (event.key === 'Escape') closeLightbox();
  if (event.key === 'ArrowRight') stepPhoto(1);
  if (event.key === 'ArrowLeft') stepPhoto(-1);
});

// count is "1 album" or "12 albums", which is said in enough places to be
// worth one function.
function count(n, word) {
  return n + ' ' + word + (n === 1 ? '' : 's');
}

/* ---------- what this browser can actually play ---------- */

// capabilities asks the browser rather than telling it.
//
// A list of formats written on the server is a guess about software that
// updates every six weeks, and both ways of being wrong cost something: claim
// too much and the viewer gets a black screen, claim too little and the
// server re-encodes a file that would have played untouched. canPlayType is
// the browser's own answer, so direct play happens wherever it is possible.
function capabilities() {
  const probe = document.createElement('video');
  const can = (type) => {
    const answer = probe.canPlayType(type);
    return answer === 'probably' || answer === 'maybe';
  };

  const containers = [];
  if (can('video/mp4')) containers.push('mp4', 'm4v', 'mov');
  if (can('video/webm')) containers.push('webm');
  if (can('video/x-matroska; codecs="avc1.640029"')) containers.push('mkv', 'webm');

  const video = [];
  if (can('video/mp4; codecs="avc1.640029"')) video.push('h264');
  if (can('video/mp4; codecs="hev1.1.6.L93.B0"') || can('video/mp4; codecs="hvc1.1.6.L93.B0"')) video.push('hevc');
  if (can('video/webm; codecs="vp8"')) video.push('vp8');
  if (can('video/webm; codecs="vp9"') || can('video/mp4; codecs="vp09.00.10.08"')) video.push('vp9');
  if (can('video/mp4; codecs="av01.0.05M.08"')) video.push('av1');

  // Music is asked about separately, through an audio element: a browser
  // answers differently for the two — Chrome plays FLAC in an <audio> and
  // says no to it in a <video> — and a wrong answer here is either a track
  // that will not play or a server re-encoding a file that would have.
  const sound = document.createElement('audio');
  const hear = (type) => {
    const answer = sound.canPlayType(type);
    return answer === 'probably' || answer === 'maybe';
  };
  if (hear('audio/mpeg')) containers.push('mp3');
  if (hear('audio/flac') || hear('audio/x-flac')) containers.push('flac');
  if (hear('audio/mp4')) containers.push('m4a', 'm4b', 'aac', 'alac');
  if (hear('audio/ogg; codecs="vorbis"')) containers.push('ogg', 'oga');
  if (hear('audio/ogg; codecs="opus"')) containers.push('opus', 'ogg');
  if (hear('audio/wav') || hear('audio/wave') || hear('audio/x-wav')) containers.push('wav');
  if (hear('audio/aiff') || hear('audio/x-aiff')) containers.push('aiff', 'aif');

  const audio = [];
  if (hear('audio/wav') || hear('audio/x-wav')) {
    // What ffprobe calls the contents of a WAV, which is the codec name the
    // server compares against.
    audio.push('pcm_s16le', 'pcm_s24le', 'pcm_f32le', 'pcm_u8');
  }
  if (hear('audio/mp4; codecs="alac"')) audio.push('alac');
  if (can('audio/mp4; codecs="mp4a.40.2"')) audio.push('aac');
  if (can('audio/mpeg')) audio.push('mp3');
  if (can('audio/mp4; codecs="ac-3"')) audio.push('ac3');
  if (can('audio/mp4; codecs="ec-3"')) audio.push('eac3');
  if (can('audio/webm; codecs="opus"') || can('audio/ogg; codecs="opus"')) audio.push('opus');
  if (can('audio/webm; codecs="vorbis"') || can('audio/ogg; codecs="vorbis"')) audio.push('vorbis');
  if (can('audio/flac') || can('audio/x-flac') || hear('audio/flac')) audio.push('flac');

  return { containers, video, audio };
}

// loadHls fetches the one vendored script, and only when a stream actually
// needs it: most playback is direct and pays nothing for this.
let hlsLoading = null;
function loadHls() {
  if (window.Hls) return Promise.resolve(window.Hls);
  if (!hlsLoading) {
    hlsLoading = new Promise((resolve, reject) => {
      const tag = document.createElement('script');
      tag.src = '/web/vendor/hls.light.min.js';
      tag.addEventListener('load', () => resolve(window.Hls));
      tag.addEventListener('error', () => {
        hlsLoading = null;
        reject(new Error('could not load the streaming player'));
      });
      document.head.append(tag);
    });
  }
  return hlsLoading;
}

/* ---------- subtitles ---------- */

// subtitlesFor hangs the text tracks off the video element.
//
// A <track> is the browser's own subtitle machinery: it parses the WebVTT,
// draws the cues, positions them over the video and keeps them in time with
// the player, including through a seek. Nothing here has to re-implement any
// of that — the server's job is to hand over WebVTT, and this one is to say
// which ones exist.
function subtitlesFor(detail) {
  for (const track of [...video.querySelectorAll('track')]) track.remove();

  const available = (detail.subtitles || []).filter((sub) => sub.text);
  for (const sub of available) {
    video.append(el('track', {
      kind: sub.hearing ? 'captions' : 'subtitles',
      label: sub.label,
      srclang: sub.language || 'und',
      src: sub.url,
    }));
  }

  const picked = available.findIndex((sub) => sub.default);
  showSubtitle(picked);
  renderSubtitleMenu(detail, available, picked);
}

// showSubtitle turns one on and the rest off. Index -1 is off.
function showSubtitle(index) {
  const tracks = video.textTracks;
  for (let i = 0; i < tracks.length; i++) {
    tracks[i].mode = i === index ? 'showing' : 'disabled';
  }
}

function renderSubtitleMenu(detail, available, picked) {
  const box = document.getElementById('player-subs');
  const pictures = (detail.subtitles || []).filter((sub) => !sub.text);
  if (!available.length && !pictures.length) {
    box.replaceChildren();
    return;
  }

  const burning = (playing && playing.burn) || 0;
  const menu = el('div', { class: 'menu', hidden: true });
  const choose = (index, label) => el('button', {
    text: label,
    on: {
      click: () => {
        menu.hidden = true;
        if (burning) {
          // Burned in, so turning it off means asking for the stream again
          // without it: the subtitle is in the picture, not beside it.
          burnSubtitle(detail, 0);
          return;
        }
        showSubtitle(index);
        button.textContent = index < 0 ? 'Subtitles' : label;
      },
    },
  });

  const current = burning
    ? (pictures.find((sub) => sub.id === burning) || {}).label || 'Subtitles'
    : (picked < 0 ? 'Subtitles' : available[picked].label);

  const button = el('button', {
    class: 'ghost',
    text: current,
    on: { click: () => { menu.hidden = !menu.hidden; } },
  });

  fill(menu,
    choose(-1, 'Off'),
    available.map((sub, index) => choose(index, sub.label)),
    // A picture subtitle is a sequence of images, which a browser has no way
    // to draw. The server can draw it into the video instead — which costs a
    // conversion, so it is offered rather than done quietly.
    pictures.map((sub) => el('button', {
      class: sub.id === burning ? 'on' : '',
      title: 'A picture subtitle. The server draws it into the video, which means converting the stream.',
      text: sub.label + ' (burn in)',
      on: {
        click: () => {
          menu.hidden = true;
          burnSubtitle(detail, sub.id);
        },
      },
    })));

  fill(box, button, menu);
}

// burnSubtitle restarts the stream with a subtitle drawn into the picture, or
// without one, keeping the viewer where they were.
async function burnSubtitle(detail, subtitleId) {
  const at = Math.round((video.currentTime || 0) * 1000);
  if (playing && playing.session) {
    // Free the encoder that is drawing the other one rather than leaving it
    // to the idle deadline.
    fetch(API + '/play/' + playing.session + '/stop?itemId=' + detail.item.id, {
      method: 'POST',
      headers: { 'X-BMS-Web': '1' },
      credentials: 'same-origin',
    }).catch(() => { /* the watchdog is the backstop */ });
  }
  toast(subtitleId ? 'Drawing that subtitle into the picture.' : 'Subtitles off.');
  await play(detail, at, subtitleId);
}

/* ---------- the player ---------- */

const video = document.getElementById('video');
const playerBox = document.getElementById('player');
let playing = null;      // { item, parts, markers, next }
let reportTimer = null;

async function play(detail, offsetMs, burnSubtitleId) {
  const part = (detail.parts || [])[0];
  if (!part) {
    toast('There is no file for this one.', true);
    return;
  }
  const startMs = Math.max(0, Math.round(offsetMs || 0));

  // Two things playing at once is nobody's intention.
  if (!audio.paused) audio.pause();

  document.getElementById('player-title').textContent =
    (detail.item.subtitle ? detail.item.subtitle + ' · ' : '') + detail.item.title;
  document.getElementById('player-error').hidden = true;
  document.getElementById('player-skip').replaceChildren();
  document.getElementById('player-mode').textContent = '';
  playerBox.hidden = false;

  let answer;
  try {
    // The server decides: it knows what is in the file, this page knows what
    // the browser can decode, and the answer is a URL either way.
    answer = await api('POST', '/play', {
      itemId: detail.item.id,
      partId: part.id,
      startMs,
      // A subtitle the browser cannot draw is drawn by the server, which
      // makes this a conversion whatever the file is.
      burnSubtitleId: burnSubtitleId || 0,
      capabilities: capabilities(),
    });
  } catch (err) {
    failed(err);
    closePlayer();
    return;
  }

  playing = {
    detail, part, startMs,
    markers: detail.markers || [],
    next: null,
    session: answer.session || null,
    mode: answer.mode,
    burn: burnSubtitleId || 0,
  };

  const mode = document.getElementById('player-mode');
  mode.textContent = answer.mode === 'direct' ? answer.label : answer.label + ' · ' + answer.why;
  mode.className = answer.mode === 'direct' ? 'muted' : 'converting';

  if (answer.mode === 'unsupported') {
    cannotPlay(answer.why);
    return;
  }

  await attach(answer, startMs);
  subtitlesFor(detail);

  clearInterval(reportTimer);
  // Every ten seconds, which is often enough that closing a laptop loses
  // nothing worth noticing and rare enough to be invisible to the server.
  reportTimer = setInterval(report, 10000);

  // What comes next, so the end of an episode offers it rather than stopping.
  if (detail.item.kind === 'episode' && detail.parent) {
    try {
      const season = await api('GET', '/items/' + detail.parent.id);
      const list = (season.children || []).filter((child) => child.kind === 'episode');
      const at = list.findIndex((child) => child.id === detail.item.id);
      if (at >= 0 && at + 1 < list.length) playing.next = list[at + 1];
    } catch (err) { /* no next episode is not an error */ }
  }
}

// attach points the video element at whatever the server offered: the file
// itself, or a stream being made while it plays.
async function attach(answer, startMs) {
  detachHls();
  const resumeAt = startMs / 1000;

  const seekOnce = () => {
    video.addEventListener('loadedmetadata', function once() {
      video.removeEventListener('loadedmetadata', once);
      if (resumeAt > 0 && (!video.duration || resumeAt < video.duration - 5)) {
        try { video.currentTime = resumeAt; } catch (err) { /* a stream that starts there already */ }
      }
      video.play().catch(() => { /* a browser that wants a click first is fine */ });
    });
  };

  if (!answer.hls) {
    seekOnce();
    video.src = answer.url;
    return;
  }

  // Safari plays HLS itself; everything else needs the vendored player.
  if (video.canPlayType('application/vnd.apple.mpegurl')) {
    seekOnce();
    video.src = answer.url;
    return;
  }
  try {
    const Hls = await loadHls();
    if (!Hls || !Hls.isSupported()) throw new Error('this browser cannot play a converted stream');
    const hls = new Hls({ maxBufferLength: 30, enableWorker: true });
    playing.hls = hls;
    hls.on(Hls.Events.MANIFEST_PARSED, () => {
      if (resumeAt > 0) {
        try { video.currentTime = resumeAt; } catch (err) { /* already there */ }
      }
      video.play().catch(() => { /* a click will start it */ });
    });
    hls.on(Hls.Events.ERROR, (event, data) => {
      if (!data || !data.fatal) return;
      // A segment that is still being made answers "ask again", and hls.js
      // retries by itself; anything fatal has already exhausted that.
      cannotPlay('the converted stream stopped: ' + (data.details || 'unknown error'));
    });
    hls.loadSource(answer.url);
    hls.attachMedia(video);
  } catch (err) {
    cannotPlay(err.message);
  }
}

function detachHls() {
  if (playing && playing.hls) {
    try { playing.hls.destroy(); } catch (err) { /* already gone */ }
    playing.hls = null;
  }
}

// cannotPlay is the honest end of the road: this browser cannot show this
// file and the server cannot convert it, so say which and offer the file.
function cannotPlay(why) {
  const box = document.getElementById('player-error');
  const container = playing && playing.part ? (playing.part.container || 'this file').toUpperCase() : 'this file';
  fill(box,
    el('h2', { class: 'tight', text: 'This browser cannot play it' }),
    el('p', { text: why ? why.charAt(0).toUpperCase() + why.slice(1) + '.' : 'The file is ' + container + '.' }),
    el('p', { class: 'muted', text: 'The Bask app plays it directly, and so does anything else that can decode it.' }),
    el('div', { class: 'row' },
      el('button', { text: 'Back', on: { click: () => { closePlayer(); render(); } } }),
      playing && playing.part
        ? el('a', { class: 'button', href: '/web/stream/' + playing.part.id, download: '', text: 'Download the file' })
        : null));
  box.hidden = false;
}

function report() {
  if (!playing || !video.duration || Number.isNaN(video.duration)) return;
  const body = {
    itemId: playing.detail.item.id,
    offsetMs: Math.round(video.currentTime * 1000),
    durationMs: Math.round(video.duration * 1000),
  };
  // keepalive, so the last report still goes out while the page is going away.
  fetch(API + '/progress', {
    method: 'POST',
    headers: { 'X-BMS-Web': '1', 'Content-Type': 'application/json' },
    credentials: 'same-origin',
    body: JSON.stringify(body),
    keepalive: true,
  }).catch(() => { /* a lost report costs a few seconds of position */ });
}

function closePlayer() {
  if (!playing) {
    playerBox.hidden = true;
    return;
  }
  report();
  clearInterval(reportTimer);
  reportTimer = null;

  // Tell the server the stream is over. The watchdog would notice eventually;
  // saying so frees a core now, and takes this viewer off the status page at
  // the moment they stopped watching rather than a minute later.
  fetch(API + '/play/' + (playing.session || 'direct') + '/stop?itemId=' + playing.detail.item.id, {
    method: 'POST',
    headers: { 'X-BMS-Web': '1' },
    credentials: 'same-origin',
    keepalive: true,
  }).catch(() => { /* the watchdog is the backstop */ });
  detachHls();
  fill(document.getElementById('player-subs'));
  video.pause();
  video.removeAttribute('src');
  video.load();
  playing = null;
  playerBox.hidden = true;
}

document.getElementById('player-back').addEventListener('click', () => {
  closePlayer();
  render();
});

video.addEventListener('pause', report);

video.addEventListener('timeupdate', () => {
  if (!playing) return;
  const at = video.currentTime * 1000;
  const skip = document.getElementById('player-skip');
  const marker = playing.markers.find((m) => at >= m.startMs && at < m.endMs - 1000);

  if (marker && !(marker.final && playing.next)) {
    if (skip.dataset.for !== String(marker.startMs)) {
      skip.dataset.for = String(marker.startMs);
      fill(skip, el('button', {
        class: 'primary',
        text: marker.type === 'intro' ? 'Skip intro' : 'Skip credits',
        on: { click: () => { video.currentTime = marker.endMs / 1000; } },
      }));
    }
    return;
  }
  if (marker && marker.final && playing.next) {
    if (skip.dataset.for !== 'next') {
      skip.dataset.for = 'next';
      fill(skip, el('button', { class: 'primary', text: 'Next episode', on: { click: playNext } }));
    }
    return;
  }
  if (skip.dataset.for) {
    skip.dataset.for = '';
    skip.replaceChildren();
  }
});

video.addEventListener('ended', () => {
  report();
  if (playing && playing.next) playNext();
  else { closePlayer(); render(); }
});

async function playNext() {
  if (!playing || !playing.next) return;
  const nextId = playing.next.id;
  report();
  try {
    const detail = await api('GET', '/items/' + nextId);
    location.hash = '#/item/' + nextId;
    await play(detail, 0);
  } catch (err) { failed(err); }
}

// A file the browser cannot read has to say so rather than spin. With a
// converted stream this fires only when the conversion itself failed, which
// cannotPlay reports in the same place.
video.addEventListener('error', () => {
  if (!playing) return;
  const container = (playing.part.container || 'this file').toUpperCase();
  cannotPlay('the file is ' + container + ' and this browser could not decode it');
});

document.addEventListener('keydown', (event) => {
  if (playerBox.hidden) return;
  if (event.key === 'Escape') { closePlayer(); render(); }
});

window.addEventListener('beforeunload', () => { if (playing) report(); });
window.addEventListener('hashchange', () => {
  // A photograph open over an album belongs to the page that opened it.
  if (!lightbox.hidden) closeLightbox();
  if (playerBox.hidden) render();
});

/* ---------- start ---------- */

(async function () {
  try {
    const me = await api('GET', '/session');
    showApp(me);
    render();
  } catch (err) {
    showSignIn();
  }
})();
