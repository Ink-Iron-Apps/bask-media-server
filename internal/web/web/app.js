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
  parent.replaceChildren(...nodes.filter((node) => node != null && node !== false));
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

function poster(item, width) {
  const box = el('div', { class: 'poster' });
  if (item.poster) {
    box.append(el('img', {
      src: '/web/image/' + item.id + '?w=' + width + '&h=' + Math.round(width * 1.5),
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
  const where = item.kind === 'episode' || item.kind === 'movie'
    ? '#/item/' + item.id
    : '#/item/' + item.id;
  return el('a', { class: 'card', href: where },
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
  fill(main(),
    resume.length ? el('h2', { text: 'Continue watching' }) : null,
    resume.length ? grid(resume) : null,
    el('h2', { text: 'Libraries' }),
    (home.sections || []).length
      ? el('div', { class: 'row' }, (home.sections || []).map((section) =>
        el('a', { class: 'button', href: '#/library/' + section.id, text: section.title })))
      : el('p', { class: 'empty', text: 'No libraries yet. Whoever runs this server adds them in the admin interface.' }));
}

async function libraryView(id) {
  const page = await api('GET', '/sections/' + id + '?size=120');
  fill(main(),
    el('h1', { text: page.title }),
    el('p', { class: 'muted', text: page.total + (page.kind === 'shows' ? ' shows' : ' films') }),
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

  hero.append(el('div', { class: 'over' },
    el('h1', { text: item.title }),
    meta ? el('div', { class: 'meta', text: meta }) : null,
    item.summary ? el('p', { class: 'summary', text: item.summary }) : null,
    actions));

  const episodes = children.filter((child) => child.kind === 'episode');
  fill(main(),
    hero,
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

/* ---------- the player ---------- */

const video = document.getElementById('video');
const playerBox = document.getElementById('player');
let playing = null;      // { item, parts, markers, next }
let reportTimer = null;

async function play(detail, offsetMs) {
  const part = (detail.parts || [])[0];
  if (!part) {
    toast('There is no file for this one.', true);
    return;
  }
  playing = { detail, part, markers: detail.markers || [], next: null };

  document.getElementById('player-title').textContent =
    (detail.item.subtitle ? detail.item.subtitle + ' · ' : '') + detail.item.title;
  document.getElementById('player-error').hidden = true;
  document.getElementById('player-skip').replaceChildren();
  playerBox.hidden = false;

  video.src = '/web/stream/' + part.id;
  video.currentTime = 0;
  const startAt = (offsetMs || 0) / 1000;
  video.addEventListener('loadedmetadata', function once() {
    video.removeEventListener('loadedmetadata', once);
    if (startAt > 0 && startAt < video.duration - 5) video.currentTime = startAt;
    video.play().catch(() => { /* a browser that wants a click first is fine */ });
  });

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

// The browser decodes, this server does not transcode, and a file the browser
// cannot read has to say so rather than spin.
video.addEventListener('error', () => {
  if (!playing) return;
  const box = document.getElementById('player-error');
  const container = (playing.part.container || 'this file').toUpperCase();
  fill(box,
    el('h2', { class: 'tight', text: 'This browser cannot play it' }),
    el('p', {
      text: 'The file is ' + container + ' and nothing is being converted: this server sends the file as it is. '
        + 'A player that can decode it plays it directly — the Bask app, or anything else on this machine.',
    }),
    el('div', { class: 'row' },
      el('button', { text: 'Back', on: { click: () => { closePlayer(); render(); } } }),
      el('a', { class: 'button', href: '/web/stream/' + playing.part.id, download: '', text: 'Download the file' })));
  box.hidden = false;
});

document.addEventListener('keydown', (event) => {
  if (playerBox.hidden) return;
  if (event.key === 'Escape') { closePlayer(); render(); }
});

window.addEventListener('beforeunload', () => { if (playing) report(); });
window.addEventListener('hashchange', () => { if (playerBox.hidden) render(); });

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
