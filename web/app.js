'use strict';
const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];

const state = { list: [], filter: 'all', cat: null, q: '', sel: new Set(), anchor: null, settings: null, ctxFor: null };

// ------------------------------------------------------------ api
async function api(path, body, method) {
  const opts = { method: method || (body !== undefined ? 'POST' : 'GET'), headers: {} };
  if (body !== undefined) { opts.headers['Content-Type'] = 'application/json'; opts.body = JSON.stringify(body); }
  const r = await fetch('/api/' + path, opts);
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || r.statusText);
  return data;
}
const act = (action, ids, extra = {}) => api('action', { action, ids: [...ids], ...extra }).then(refresh).catch(e => toast(e.message, 'err'));

// ------------------------------------------------------------ formatting
function fmtBytes(n) {
  if (n == null || n < 0) return '—';
  const u = ['B', 'KB', 'MB', 'GB', 'TB']; let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n : n.toFixed(n < 10 ? 2 : n < 100 ? 1 : 0)) + ' ' + u[i];
}
function fmtTime(s) {
  if (!s || !isFinite(s) || s <= 0) return '';
  s = Math.round(s);
  if (s < 60) return s + 's';
  if (s < 3600) return Math.floor(s / 60) + 'm ' + (s % 60) + 's';
  if (s < 86400) return Math.floor(s / 3600) + 'h ' + Math.floor(s % 3600 / 60) + 'm';
  return Math.floor(s / 86400) + 'd ' + Math.floor(s % 86400 / 3600) + 'h';
}
function fmtDate(iso) {
  const d = new Date(iso), now = new Date();
  const t = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  if (d.toDateString() === now.toDateString()) return 'Today ' + t;
  return d.toLocaleDateString([], { day: 'numeric', month: 'short' }) + ' ' + t;
}
const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const catColor = { Video: '#f43f5e', Music: '#a855f7', Documents: '#3b82f6', Compressed: '#f59e0b', Programs: '#22c55e', Images: '#ec4899', General: '#64748b' };
function ext(name) { const m = /\.([a-z0-9]{1,5})$/i.exec(name || ''); return m ? m[1].toUpperCase() : 'FILE'; }
const statusText = d => ({
  downloading: d.probed ? `${d.activeConns}/${d.connections} connections` : 'Connecting…',
  queued: 'Queued', paused: 'Paused', completed: 'Complete', error: 'Failed',
})[d.status];

// ------------------------------------------------------------ rendering
function visible() {
  const q = state.q.toLowerCase();
  return state.list.filter(d => {
    if (state.cat && d.category !== state.cat) return false;
    if (state.filter === 'downloading' && d.status !== 'downloading') return false;
    if (state.filter === 'queued' && d.status !== 'queued') return false;
    if (state.filter === 'unfinished' && !(d.status === 'paused' || d.status === 'error')) return false;
    if (state.filter === 'completed' && d.status !== 'completed') return false;
    if (q && !(d.fileName.toLowerCase().includes(q) || d.url.toLowerCase().includes(q))) return false;
    return true;
  }).slice().reverse(); // newest first
}

function rowHTML(d) {
  const pct = d.size > 0 ? Math.min(100, d.downloaded / d.size * 100) : (d.status === 'completed' ? 100 : 0);
  const barCls = d.status === 'completed' ? 'done' : d.status === 'error' ? 'err' : d.status === 'paused' ? 'paused'
    : (d.status === 'downloading' && d.size < 0) ? 'indet' : '';
  const sub = d.status === 'error' ? `<span class="st-error">${esc(d.error || 'Failed')}</span>` : `<span class="st-${d.status}">${statusText(d)}</span>`;
  const sizeTxt = d.size > 0 ? fmtBytes(d.size) : (d.status === 'completed' ? fmtBytes(d.downloaded) : '—');
  const pctTxt = d.size > 0 ? pct.toFixed(pct < 100 ? 1 : 0) + '%' : '';
  return `<td><div class="fname"><div class="ficon" style="background:${catColor[d.category] || catColor.General}">${ext(d.fileName)}</div>
      <div class="txt"><span class="nm" title="${esc(d.fileName)}">${esc(d.fileName)}</span><span class="sub">${sub}</span></div></div></td>
    <td>${sizeTxt}</td>
    <td><div class="prog"><div class="bar ${barCls}"><i style="width:${pct}%"></i></div>
      <div class="pl"><span>${fmtBytes(d.downloaded)}</span><span>${pctTxt}</span></div></div></td>
    <td class="${d.status === 'downloading' ? 'st-downloading' : 'muted'}">${d.status === 'downloading' ? fmtBytes(d.speed) + '/s' : ''}</td>
    <td class="muted">${d.status === 'downloading' ? fmtTime(d.eta) : ''}</td>
    <td class="muted">${fmtDate(d.addedAt)}</td>`;
}

// keyed update so rows under the mouse are never replaced (keeps clicks reliable)
const rowCache = new Map();
function syncRows(list) {
  const tbody = $('#rows');
  const keep = new Set();
  let prev = null;
  for (const d of list) {
    keep.add(d.id);
    let entry = rowCache.get(d.id);
    if (!entry) {
      const tr = document.createElement('tr');
      tr.dataset.id = d.id;
      entry = { tr, html: '' };
      rowCache.set(d.id, entry);
    }
    const html = rowHTML(d);
    if (html !== entry.html) { entry.tr.innerHTML = html; entry.html = html; }
    entry.tr.classList.toggle('sel', state.sel.has(d.id));
    const want = prev ? prev.nextSibling : tbody.firstChild;
    if (want !== entry.tr) tbody.insertBefore(entry.tr, want);
    prev = entry.tr;
  }
  for (const [id, entry] of rowCache) {
    if (!keep.has(id)) { entry.tr.remove(); rowCache.delete(id); }
  }
}

function render() {
  const rows = visible();
  syncRows(rows);
  $('#empty').hidden = rows.length > 0;

  const c = { all: 0, downloading: 0, queued: 0, unfinished: 0, completed: 0 }, cats = {};
  let speed = 0;
  for (const d of state.list) {
    c.all++;
    if (d.status === 'downloading') { c.downloading++; speed += d.speed; }
    if (d.status === 'queued') c.queued++;
    if (d.status === 'paused' || d.status === 'error') c.unfinished++;
    if (d.status === 'completed') c.completed++;
    cats[d.category] = (cats[d.category] || 0) + 1;
  }
  $$('#statusNav a').forEach(a => { $('b', a).textContent = c[a.dataset.f] || ''; });
  $$('#catNav a').forEach(a => { $('b', a).textContent = cats[a.dataset.c] || ''; });
  $('#totalSpeed').textContent = fmtBytes(speed) + '/s';
  document.title = c.downloading ? `OpenDM — ${c.downloading} active · ${fmtBytes(speed)}/s` : 'OpenDM';

  // drop selections that vanished
  for (const id of state.sel) if (!state.list.some(d => d.id === id)) state.sel.delete(id);
  updateToolbar();
  renderDetails();
}

function selected() { return state.list.filter(d => state.sel.has(d.id)); }

function updateToolbar() {
  const s = selected();
  $('[data-act=resume]').disabled = !s.some(d => d.status === 'paused' || d.status === 'error');
  $('[data-act=pause]').disabled = !s.some(d => d.status === 'downloading' || d.status === 'queued');
  $('[data-act=restart]').disabled = !s.length;
  $('[data-act=remove]').disabled = !s.length;
}

let lastDetailsKey = '';
function renderDetails() {
  const box = $('#details');
  const s = selected();
  if (s.length !== 1) { box.hidden = true; lastDetailsKey = ''; return; }
  const d = s[0];
  box.hidden = false;
  const key = [d.id, d.status, d.fileName, d.error, d.url, d.size, d.resumable, d.dir].join('|');
  if (key !== lastDetailsKey) {
    lastDetailsKey = key;
    const canPause = d.status === 'downloading' || d.status === 'queued';
    const canResume = d.status === 'paused' || d.status === 'error';
    box.innerHTML = `
      <h4><span class="nm">${esc(d.fileName)}</span>
        <div class="dacts">
          ${d.status === 'completed' ? `<button class="btn" data-d="open">Open</button>` : ''}
          <button class="btn" data-d="folder">Show in folder</button>
          ${canPause ? `<button class="btn" data-d="pause">Pause</button>` : ''}
          ${canResume ? `<button class="btn" data-d="resume">Resume</button>` : ''}
          ${d.status !== 'completed' ? `<button class="btn" data-d="seturl">Refresh address</button>` : ''}
          <button class="btn" data-d="copy">Copy link</button>
        </div></h4>
      <div class="kv">
        <span>Status</span><span id="dStatus"></span>
        <span>Size</span><span>${d.size > 0 ? fmtBytes(d.size) + ` (${d.size.toLocaleString()} bytes)` : 'Unknown'}</span>
        <span>Downloaded</span><span id="dDone"></span>
        <span>Resume support</span><span>${!d.probed ? '—' : d.resumable ? '<span class="st-completed">Yes</span>' : '<span class="st-paused">No — pausing restarts from zero</span>'}</span>
      </div>
      <div class="kv">
        <span>Save to</span><span class="wrap">${esc(d.path)}</span>
        <span>Address</span><span class="wrap">${esc(d.url)}</span>
        <span>Category</span><span>${esc(d.category)}</span>
      </div>
      ${d.status === 'error' ? `<div class="errbox">${esc(d.error)}. Try <b>Resume</b>; if the link expired use <b>Refresh address</b>.</div>` : ''}
      <div class="seg-wrap"><div class="lbl"><span>Connections &amp; segments</span><span id="dSegs"></span></div><canvas class="segs" id="segCanvas"></canvas></div>`;
  }
  $('#dStatus').innerHTML = d.status === 'downloading'
    ? `<span class="st-downloading">Downloading · ${fmtBytes(d.speed)}/s · ${fmtTime(d.eta) || '…'} left</span>`
    : `<span class="st-${d.status}">${statusText(d)}</span>`;
  $('#dDone').textContent = fmtBytes(d.downloaded) + (d.size > 0 ? ` (${(d.downloaded / d.size * 100).toFixed(1)}%)` : '');
  $('#dSegs').textContent = d.segments ? `${d.segments.length} segment${d.segments.length === 1 ? '' : 's'}` + (d.status === 'downloading' ? ` · ${d.activeConns} active` : '') : '';
  drawSegments($('#segCanvas'), d);
}

// IDM-style map of which byte ranges are already on disk
function drawSegments(cv, d) {
  const dpr = window.devicePixelRatio || 1;
  const w = cv.clientWidth, h = cv.clientHeight;
  if (cv.width !== w * dpr) { cv.width = w * dpr; cv.height = h * dpr; }
  const g = cv.getContext('2d');
  g.setTransform(dpr, 0, 0, dpr, 0, 0);
  g.clearRect(0, 0, w, h);
  const total = d.size > 0 ? d.size : 0;
  if (!total || !d.segments) {
    if (d.status === 'completed') { g.fillStyle = '#22c55e'; g.fillRect(0, 0, w, h); }
    return;
  }
  const grad = g.createLinearGradient(0, 0, w, 0);
  grad.addColorStop(0, '#6366f1'); grad.addColorStop(1, '#06b6d4');
  const fill = d.status === 'completed' ? '#22c55e' : d.status === 'error' ? '#ef4444' : d.status === 'paused' ? '#f59e0b' : grad;
  for (const [start, end, done] of d.segments) {
    const x0 = start / total * w;
    const xd = (start + done) / total * w;
    g.fillStyle = fill;
    g.fillRect(x0, 0, Math.max(xd - x0, done > 0 ? 1 : 0), h);
    // segment boundary
    if (start > 0) { g.fillStyle = 'rgba(0,0,0,.35)'; g.fillRect(x0, 0, 1, h); }
    // write head
    if (d.status === 'downloading' && end >= 0 && start + done <= end) {
      g.fillStyle = '#fff'; g.fillRect(Math.min(xd, w - 2), 3, 2, h - 6);
    }
  }
}

// ------------------------------------------------------------ polling
let failCount = 0;
async function refresh() {
  try {
    const s = await api('state');
    const prev = new Map(state.list.map(d => [d.id, d.status]));
    state.list = s.downloads || [];
    for (const d of state.list) {
      if (prev.get(d.id) && prev.get(d.id) !== 'completed' && d.status === 'completed') toast(`Finished: ${d.fileName}`, 'ok');
      if (prev.get(d.id) === 'downloading' && d.status === 'error') toast(`Failed: ${d.fileName} — ${d.error}`, 'err');
    }
    failCount = 0;
    render();
  } catch (e) {
    if (++failCount === 3) toast('Lost connection to OpenDM. Is it still running?', 'err');
  }
}
setInterval(refresh, 700);

// ------------------------------------------------------------ toast
function toast(msg, kind = '') {
  const t = document.createElement('div');
  t.className = 'toast ' + kind;
  t.textContent = msg;
  $('#toasts').appendChild(t);
  setTimeout(() => t.remove(), kind === 'err' ? 6000 : 3500);
}

// ------------------------------------------------------------ selection & table events
$('#rows').addEventListener('mousedown', e => {
  const tr = e.target.closest('tr'); if (!tr) return;
  const id = tr.dataset.id;
  if (e.button === 2 && state.sel.has(id)) return;
  if (e.shiftKey && state.anchor) {
    const ids = visible().map(d => d.id);
    const a = ids.indexOf(state.anchor), b = ids.indexOf(id);
    if (!e.ctrlKey) state.sel.clear();
    ids.slice(Math.min(a, b), Math.max(a, b) + 1).forEach(x => state.sel.add(x));
  } else if (e.ctrlKey || e.metaKey) {
    state.sel.has(id) ? state.sel.delete(id) : state.sel.add(id);
    state.anchor = id;
  } else {
    state.sel = new Set([id]); state.anchor = id;
  }
  render();
});
$('#tableWrap').addEventListener('mousedown', e => {
  if (!e.target.closest('tr') && !e.target.closest('th')) { state.sel.clear(); render(); }
});
$('#rows').addEventListener('dblclick', e => {
  const tr = e.target.closest('tr'); if (!tr) return;
  const d = state.list.find(x => x.id === tr.dataset.id);
  if (!d) return;
  if (d.status === 'completed') act('open', [d.id]);
  else if (d.status === 'downloading' || d.status === 'queued') act('pause', [d.id]);
  else act('resume', [d.id]);
});

// ------------------------------------------------------------ context menu
const ctx = $('#ctx');
function hideCtx() { ctx.hidden = true; }
document.addEventListener('mousedown', e => { if (!ctx.contains(e.target)) hideCtx(); });
window.addEventListener('blur', hideCtx);
$('#rows').addEventListener('contextmenu', e => {
  e.preventDefault();
  const s = selected(); if (!s.length) return;
  const one = s.length === 1 ? s[0] : null;
  const any = st => s.some(d => st.includes(d.status));
  ctx.innerHTML = `
    <button data-c="open" ${one && one.status === 'completed' ? '' : 'disabled'}>Open</button>
    <button data-c="folder" ${one ? '' : 'disabled'}>Show in folder</button>
    <hr>
    <button data-c="resume" ${any(['paused', 'error']) ? '' : 'disabled'}>Resume</button>
    <button data-c="pause" ${any(['downloading', 'queued']) ? '' : 'disabled'}>Pause</button>
    <button data-c="restart">Restart from zero</button>
    <button data-c="seturl" ${one && one.status !== 'completed' ? '' : 'disabled'}>Refresh download address…</button>
    <hr>
    <button data-c="copy">Copy link${s.length > 1 ? 's' : ''}</button>
    <hr>
    <button data-c="remove">Delete…</button>`;
  ctx.hidden = false;
  const r = ctx.getBoundingClientRect();
  ctx.style.left = Math.min(e.clientX, innerWidth - r.width - 8) + 'px';
  ctx.style.top = Math.min(e.clientY, innerHeight - r.height - 8) + 'px';
});
ctx.addEventListener('click', e => {
  const b = e.target.closest('button'); if (!b) return;
  hideCtx(); command(b.dataset.c);
});
$('#details').addEventListener('click', e => { const b = e.target.closest('[data-d]'); if (b) command(b.dataset.d); });

function command(c) {
  const ids = [...state.sel];
  if (!ids.length) return;
  switch (c) {
    case 'remove': return askDelete(ids);
    case 'copy':
      navigator.clipboard.writeText(selected().map(d => d.url).join('\n')).then(() => toast('Link copied'));
      return;
    case 'seturl':
      $('#newUrl').value = selected()[0]?.url || '';
      $('#dlgUrl').showModal(); $('#newUrl').select();
      return;
    default: act(c, ids);
  }
}

// ------------------------------------------------------------ toolbar
$$('[data-act]').forEach(b => b.addEventListener('click', () => command(b.dataset.act)));
$('#btnResumeAll').onclick = () => api('all/resume', {}).then(refresh);
$('#btnPauseAll').onclick = () => api('all/pause', {}).then(refresh);
$('#btnClear').onclick = () => api('all/clear-completed', {}).then(refresh);
$('#btnOpenDir').onclick = () => api('open-downloads', {});
$('#search').addEventListener('input', e => { state.q = e.target.value; render(); });
$$('#statusNav a').forEach(a => a.onclick = () => {
  $$('.sidebar a').forEach(x => x.classList.remove('on')); a.classList.add('on');
  state.filter = a.dataset.f; state.cat = null; render();
});
$$('#catNav a').forEach(a => a.onclick = () => {
  $$('.sidebar a').forEach(x => x.classList.remove('on')); a.classList.add('on');
  state.cat = a.dataset.c; state.filter = 'all'; render();
});

// ------------------------------------------------------------ delete
function askDelete(ids) {
  const s = selected();
  $('#delTitle').textContent = ids.length === 1 ? `Delete “${s[0].fileName}”?` : `Delete ${ids.length} downloads?`;
  $('#delFile').checked = false;
  $('#dlgDelete').showModal();
  $('#delOk').onclick = () => {
    $('#dlgDelete').close();
    act('remove', ids, { deleteFile: $('#delFile').checked });
    state.sel.clear();
  };
}
$('#delCancel').onclick = () => $('#dlgDelete').close();

// ------------------------------------------------------------ refresh URL
$('#urlCancel').onclick = () => $('#dlgUrl').close();
$('#urlOk').onclick = async () => {
  const ids = [...state.sel];
  try {
    await api('action', { action: 'seturl', ids, url: $('#newUrl').value.trim() });
    await api('action', { action: 'resume', ids });
    $('#dlgUrl').close(); refresh();
  } catch (e) { toast(e.message, 'err'); }
};

// ------------------------------------------------------------ add dialog
const dlgAdd = $('#dlgAdd');
let probeTimer = null, probeSeq = 0;
function parseUrls(text) {
  return text.split(/[\s]+/).map(s => s.trim()).filter(s => /^https?:\/\/\S+$/i.test(s));
}
function openAdd(prefill = '') {
  $('#addUrls').value = prefill;
  $('#addName').value = ''; $('#addDir').value = ''; $('#addRef').value = ''; $('#addCookie').value = '';
  $('#addConns').value = state.settings?.connections || 8;
  $('#probe').innerHTML = '';
  dlgAdd.showModal();
  $('#addUrls').focus();
  if (prefill) scheduleProbe();
  else if (navigator.clipboard?.readText) {
    navigator.clipboard.readText().then(t => {
      if (!$('#addUrls').value && parseUrls(t).length) { $('#addUrls').value = parseUrls(t).join('\n'); scheduleProbe(); }
    }).catch(() => {});
  }
}
function scheduleProbe() {
  clearTimeout(probeTimer);
  probeTimer = setTimeout(doProbe, 350);
}
async function doProbe() {
  const urls = parseUrls($('#addUrls').value);
  const box = $('#probe');
  $('#addName').disabled = urls.length > 1;
  if (urls.length !== 1) {
    box.innerHTML = urls.length > 1 ? `<span class="tag">${urls.length} links</span>` : '';
    return;
  }
  const seq = ++probeSeq;
  box.innerHTML = '<span>Checking link…</span>';
  try {
    const info = await api('probe', { url: urls[0], referer: $('#addRef').value, cookie: $('#addCookie').value });
    if (seq !== probeSeq) return;
    if (!$('#addName').dataset.touched) $('#addName').value = info.fileName;
    box.innerHTML = `<span class="tag">${fmtBytes(info.size)}</span><span class="tag">${esc(info.category)}</span>`
      + (info.resumable ? '<span class="tag ok">Resumable · multi-connection</span>' : '<span class="tag no">No resume support · single connection</span>');
  } catch (e) {
    if (seq !== probeSeq) return;
    box.innerHTML = `<span class="tag bad">${esc(e.message)}</span>`;
  }
}
$('#addUrls').addEventListener('input', () => { delete $('#addName').dataset.touched; scheduleProbe(); });
$('#addName').addEventListener('input', () => { $('#addName').dataset.touched = '1'; });
$('#btnAdd').onclick = () => openAdd();
$('#addCancel').onclick = () => dlgAdd.close();
$('#addBrowse').onclick = () => pick($('#addDir'));
async function submitAdd(paused) {
  const urls = parseUrls($('#addUrls').value);
  if (!urls.length) { toast('Paste at least one http:// or https:// link', 'err'); return; }
  try {
    const r = await api('add', {
      urls, paused,
      fileName: urls.length === 1 ? $('#addName').value.trim() : '',
      dir: $('#addDir').value.trim(),
      connections: +$('#addConns').value || 0,
      referer: $('#addRef').value.trim(), cookie: $('#addCookie').value.trim(),
    });
    dlgAdd.close();
    state.sel = new Set(r.ids.slice(-1));
    toast(r.ids.length > 1 ? `Added ${r.ids.length} downloads` : (paused ? 'Added to list' : 'Download started'));
    refresh();
  } catch (e) { toast(e.message, 'err'); }
}
$('#addStart').onclick = () => submitAdd(false);
$('#addLater').onclick = () => submitAdd(true);

async function pick(input) {
  try {
    const r = await api('pick-folder', { start: input.value || state.settings?.downloadDir || '' });
    if (r.path) input.value = r.path;
  } catch (e) { toast(e.message, 'err'); }
}

// ------------------------------------------------------------ settings
const dlgS = $('#dlgSettings');
async function loadSettings() {
  const r = await api('settings');
  state.settings = r.settings;
  state.platform = r.platform;
  state.dataDir = r.dataDir;
  return r;
}
async function openSettings() {
  const r = await loadSettings();
  const s = r.settings;
  $('#sDir').value = s.downloadDir; $('#sSort').checked = s.sortByCategory; $('#sNotify').checked = s.notifyOnComplete;
  $('#sAppWin').checked = s.openAppWindow; $('#sAuto').checked = s.startWithWindows;
  $('#sConns').value = s.connections; $('#sMax').value = s.maxConcurrent; $('#sLimit').value = s.speedLimitKB;
  $('#sRetries').value = s.retries; $('#sProxy').value = s.proxy; $('#sUA').value = s.userAgent; $('#sPort').value = s.port;
  $('#sAutoWrap').hidden = r.platform !== 'windows';
  $('#dataDir').textContent = r.dataDir;
  $('#extPath').textContent = '';
  switchTab('general');
  dlgS.showModal();
}
function switchTab(t) {
  $$('.tabs a').forEach(a => a.classList.toggle('on', a.dataset.tab === t));
  $$('.tab').forEach(x => x.hidden = x.dataset.tab !== t);
}
$$('.tabs a').forEach(a => a.onclick = () => switchTab(a.dataset.tab));
$('#btnSettings').onclick = openSettings;
$('#sCancel').onclick = () => dlgS.close();
$('#sBrowse').onclick = () => pick($('#sDir'));
$('#sSave').onclick = async () => {
  const body = {
    ...state.settings,
    downloadDir: $('#sDir').value.trim(), sortByCategory: $('#sSort').checked, notifyOnComplete: $('#sNotify').checked,
    openAppWindow: $('#sAppWin').checked, startWithWindows: $('#sAuto').checked,
    connections: +$('#sConns').value, maxConcurrent: +$('#sMax').value, speedLimitKB: +$('#sLimit').value,
    retries: +$('#sRetries').value, proxy: $('#sProxy').value.trim(), userAgent: $('#sUA').value.trim(), port: +$('#sPort').value,
  };
  try {
    const r = await api('settings', body, 'PUT');
    state.settings = r.settings;
    dlgS.close();
    toast(r.restartNeeded ? 'Saved. Restart OpenDM to use the new port.' : 'Settings saved', 'ok');
  } catch (e) { toast(e.message, 'err'); }
};
$('#btnExport').onclick = async () => {
  try { const r = await api('export-extension', {}); $('#extPath').textContent = r.path; }
  catch (e) { toast(e.message, 'err'); }
};
$('#btnQuit').onclick = async () => {
  await api('quit', {}).catch(() => {});
  document.body.innerHTML = '<div style="grid-column:1/-1;grid-row:1/-1;display:grid;place-items:center;color:var(--muted)">OpenDM has exited. You can close this window.</div>';
};

// ------------------------------------------------------------ keyboard, paste, drop
document.addEventListener('keydown', e => {
  if (e.target.closest('input, textarea') || document.querySelector('dialog[open]')) return;
  if (e.key === 'Delete' && state.sel.size) { e.preventDefault(); command('remove'); }
  else if (e.key === 'Enter' && state.sel.size === 1) { $('#rows tr.sel')?.dispatchEvent(new MouseEvent('dblclick', { bubbles: true })); }
  else if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'a') {
    e.preventDefault(); state.sel = new Set(visible().map(d => d.id)); render();
  } else if (e.key === 'Escape') { state.sel.clear(); render(); }
  else if (e.key === 'Insert') openAdd();
});
document.addEventListener('paste', e => {
  if (e.target.closest('input, textarea') || document.querySelector('dialog[open]')) return;
  const urls = parseUrls(e.clipboardData.getData('text'));
  if (urls.length) { e.preventDefault(); openAdd(urls.join('\n')); }
});
let dragDepth = 0;
document.addEventListener('dragenter', e => { if (e.dataTransfer.types.some(t => /uri|text/.test(t))) { dragDepth++; $('#drop').hidden = false; } });
document.addEventListener('dragleave', () => { if (--dragDepth <= 0) { dragDepth = 0; $('#drop').hidden = true; } });
document.addEventListener('dragover', e => e.preventDefault());
document.addEventListener('drop', e => {
  e.preventDefault(); dragDepth = 0; $('#drop').hidden = true;
  const urls = parseUrls(e.dataTransfer.getData('text/uri-list') || e.dataTransfer.getData('text'));
  if (urls.length) openAdd(urls.join('\n'));
});
window.addEventListener('resize', () => renderDetails());

loadSettings().catch(() => {});
refresh();
