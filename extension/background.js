// OpenDM browser integration (Manifest V3).
// Catches new browser downloads, hands them to the OpenDM app running on
// 127.0.0.1, and cancels the browser's own copy. If OpenDM isn't running the
// browser simply keeps downloading as normal.

const DEFAULTS = { enabled: true, port: 6801, minSizeKB: 0, skipExt: '' };

async function settings() {
  return { ...DEFAULTS, ...(await chrome.storage.local.get(DEFAULTS)) };
}

async function cookieHeader(url) {
  try {
    const list = await chrome.cookies.getAll({ url });
    return list.map(c => `${c.name}=${c.value}`).join('; ');
  } catch {
    return '';
  }
}

async function sendToApp(payload, port) {
  const r = await fetch(`http://127.0.0.1:${port}/api/add`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  });
  if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || r.statusText);
  return r.json();
}

function isOwnUrl(url, port) {
  try {
    const u = new URL(url);
    return (u.hostname === '127.0.0.1' || u.hostname === 'localhost') && +u.port === +port;
  } catch { return true; }
}

function baseName(p) {
  return (p || '').split(/[\\/]/).pop();
}

async function buildPayload(url, referrer, fileName) {
  return {
    url,
    referer: referrer || '',
    cookie: await cookieHeader(url),
    userAgent: navigator.userAgent,
    fileName: fileName || '',
  };
}

// Track ids we've already handled so retries/updates don't double-send.
const handled = new Set();

chrome.downloads.onCreated.addListener(async item => {
  const s = await settings();
  if (!s.enabled || handled.has(item.id)) return;
  const url = item.finalUrl || item.url;
  if (!/^https?:/i.test(url) || isOwnUrl(url, s.port)) return;
  if (item.state && item.state !== 'in_progress') return;
  if (s.minSizeKB > 0 && item.totalBytes > 0 && item.totalBytes < s.minSizeKB * 1024) return;
  if (s.skipExt) {
    const ext = (baseName(item.filename) || new URL(url).pathname).split('.').pop().toLowerCase();
    const skip = s.skipExt.split(/[\s,;]+/).map(x => x.replace(/^\./, '').toLowerCase()).filter(Boolean);
    if (ext && skip.includes(ext)) return;
  }
  handled.add(item.id);
  try {
    await sendToApp(await buildPayload(url, item.referrer, baseName(item.filename)), s.port);
    // OpenDM accepted it — stop the browser's own copy
    try { await chrome.downloads.cancel(item.id); } catch {}
    try { await chrome.downloads.erase({ id: item.id }); } catch {}
    flashBadge('✓', '#22c55e');
  } catch (e) {
    // app not running or refused: let the browser carry on
    handled.delete(item.id);
    flashBadge('!', '#ef4444');
  }
});

function flashBadge(text, color) {
  chrome.action.setBadgeBackgroundColor({ color });
  chrome.action.setBadgeText({ text });
  setTimeout(() => chrome.action.setBadgeText({ text: '' }), 2500);
}

// ---- right-click menu
function setupMenus() {
  chrome.contextMenus.removeAll(() => {
    chrome.contextMenus.create({ id: 'opendm-link', title: 'Download with OpenDM', contexts: ['link'] });
    chrome.contextMenus.create({ id: 'opendm-media', title: 'Download media with OpenDM', contexts: ['image', 'video', 'audio'] });
    chrome.contextMenus.create({ id: 'opendm-page', title: 'Download this page with OpenDM', contexts: ['page'] });
  });
}
chrome.runtime.onInstalled.addListener(setupMenus);
chrome.runtime.onStartup.addListener(setupMenus);

chrome.contextMenus.onClicked.addListener(async (info, tab) => {
  const s = await settings();
  const url = info.menuItemId === 'opendm-link' ? info.linkUrl
    : info.menuItemId === 'opendm-media' ? info.srcUrl : info.pageUrl;
  if (!url || !/^https?:/i.test(url)) { flashBadge('!', '#ef4444'); return; }
  try {
    await sendToApp(await buildPayload(url, info.pageUrl || tab?.url), s.port);
    flashBadge('✓', '#22c55e');
  } catch {
    flashBadge('!', '#ef4444');
  }
});

// ---- popup messages
chrome.runtime.onMessage.addListener((msg, _sender, reply) => {
  if (msg?.type === 'ping') {
    settings().then(s => fetch(`http://127.0.0.1:${s.port}/api/ping`)
      .then(r => r.json()).then(j => reply({ ok: j.app === 'OpenDM', version: j.version }))
      .catch(() => reply({ ok: false })));
    return true;
  }
  if (msg?.type === 'show') {
    settings().then(s => fetch(`http://127.0.0.1:${s.port}/api/show`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}',
    }).catch(() => {}));
  }
});
