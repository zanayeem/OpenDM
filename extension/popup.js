const DEFAULTS = { enabled: true, port: 6801, minSizeKB: 0, skipExt: '' };
const $ = id => document.getElementById(id);

chrome.storage.local.get(DEFAULTS).then(s => {
  $('enabled').checked = s.enabled;
  $('port').value = s.port;
  $('minSizeKB').value = s.minSizeKB;
  $('skipExt').value = s.skipExt;
  ping();
});

function save() {
  chrome.storage.local.set({
    enabled: $('enabled').checked,
    port: +$('port').value || 6801,
    minSizeKB: Math.max(0, +$('minSizeKB').value || 0),
    skipExt: $('skipExt').value.trim(),
  }).then(ping);
}
['enabled', 'port', 'minSizeKB', 'skipExt'].forEach(id => $(id).addEventListener('change', save));

function ping() {
  chrome.runtime.sendMessage({ type: 'ping' }, r => {
    const st = $('status');
    st.className = r && r.ok ? 'on' : 'off';
    st.querySelector('em').textContent = r && r.ok ? 'connected' : 'not running';
  });
}

$('open').onclick = () => { chrome.runtime.sendMessage({ type: 'show' }); window.close(); };
