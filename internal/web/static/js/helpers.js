/* ══════════════ Small helpers ══════════════ */
function val(id) { return document.getElementById(id).value.trim(); }
function setVal(id, v) { document.getElementById(id).value = (v === null || v === undefined) ? '' : v; }
function escapeHtml(s) { return String(s ?? '').replace(/[&<>"']/g, c => ({ '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;' }[c])); }
function toNum(v, def) { if (v === '' || v === null || v === undefined) return def; const n = Number(v); return isNaN(n) ? def : n; }
function dataTypeLabel(v) { return DT_LABEL[v] || v || '—'; }
function parseRegAddr(v) {
  if (v === '' || v === null || v === undefined) return '';
  const s = String(v).trim();
  if (/^0[xX][0-9A-Fa-f]+$/.test(s)) return parseInt(s, 16).toString(10);
  if (/^-?\d+$/.test(s)) return parseInt(s, 10).toString(10);
  return s; // 无法解析则原样返回
}
function normRegField(id) {
  const el = document.getElementById(id);
  if (!el) return;
  el.value = parseRegAddr(el.value);
}
function sanitize(name) { return (String(name || '').replace(/[\\/:*?"<>|]/g, '_').trim()) || 'device'; }

/* ══════════════ Deep copy ══════════════ */
function deepCopy(obj) { return JSON.parse(JSON.stringify(obj)); }

/* ══════════════ Sidebar ══════════════ */
function switchSection(key) {
  document.querySelectorAll('.app-section').forEach(s => s.classList.add('d-none'));
  document.getElementById('section-' + key).classList.remove('d-none');
  document.querySelectorAll('.sidebar-item').forEach(i => i.classList.remove('active'));
  document.getElementById('nav-' + key).classList.add('active');
  if (key === 'realtime') renderRealtime(); else rtStopPolling();
  if (key === 'log') renderLogSelectors(); else logStopPolling();
  if (key === 'settings') {
    renderSettings();
    loadSystemInfo().then(updateSystemInfo);
  }
}

