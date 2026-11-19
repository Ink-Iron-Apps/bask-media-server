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

  const audio = [];
  if (can('audio/mp4; codecs="mp4a.40.2"')) audio.push('aac');
  if (can('audio/mpeg')) audio.push('mp3');
  if (can('audio/mp4; codecs="ac-3"')) audio.push('ac3');
  if (can('audio/mp4; codecs="ec-3"')) audio.push('eac3');
  if (can('audio/webm; codecs="opus"') || can('audio/ogg; codecs="opus"')) audio.push('opus');
  if (can('audio/webm; codecs="vorbis"') || can('audio/ogg; codecs="vorbis"')) audio.push('vorbis');
  if (can('audio/flac') || can('audio/x-flac')) audio.push('flac');

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
  const startMs = Math.max(0, Math.round(offsetMs || 0));

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
  };

  const mode = document.getElementById('player-mode');
  mode.textContent = answer.mode === 'direct' ? answer.label : answer.label + ' · ' + answer.why;
  mode.className = answer.mode === 'direct' ? 'muted' : 'converting';

  if (answer.mode === 'unsupported') {
    cannotPlay(answer.why);
    return;
  }

  await attach(answer, startMs);

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

  // Tell the server it can stop converting. The watchdog would notice
  // eventually; saying so frees a core now.
  if (playing.session) {
    fetch(API + '/play/' + playing.session + '/stop', {
      method: 'POST',
      headers: { 'X-BMS-Web': '1' },
      credentials: 'same-origin',
      keepalive: true,
    }).catch(() => { /* the watchdog is the backstop */ });
  }
  detachHls();
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
