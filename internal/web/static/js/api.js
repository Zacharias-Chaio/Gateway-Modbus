/* ══════════════ API 客户端 ══════════════ */
const API = '/api';
// ApiError 携带后端返回的结构化信息，便于页面把错误定位到具体字段/行。
// 后端校验失败时返回 422 + issues:[{index, propId, propName, field, message}]，
// 只取 msg 会丢掉定位能力，用户又得自己去数第几行。
class ApiError extends Error {
  constructor(message, status, payload) {
    super(message);
    this.name = 'ApiError';
    this.status = status || 0;
    this.code = (payload && payload.code) || 0;
    this.issues = (payload && payload.issues) || [];
    this.warnings = (payload && payload.warnings) || [];
  }
  get hasIssues() { return this.issues.length > 0; }
}
async function apiReq(path, opts) {
  const r = await fetch(API + path, opts);
  const j = await r.json().catch(() => ({}));
  if (!r.ok || j.code) throw new ApiError(j.msg || ('HTTP ' + r.status), r.status, j);
  return j.data;
}
function apiGet(p) { return apiReq(p); }
function apiPost(p, body) { return apiReq(p, { method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify(body) }); }
function apiDelete(p) { return apiReq(p, { method:'DELETE' }); }
function modelToPayload(m) {
  const id = m.profile.profileId || m.id;
  return { id, profileIndex: toNum(m.profile.profileIndex, 0), name: m.profile.name || '', profile: m.profile, properties: m.properties };
}
async function loadModels() {
  try {
    const rows = await apiGet('/models');
    state.models = (rows || []).map(r => ({ id: (r.profile && r.profile.profileId) || r.id, profile: r.profile || {}, properties: r.properties || [] }));
  } catch (e) { state.models = []; }
}
async function loadSettings() {
  try {
    const settings = await apiGet('/settings');
    if (!settings || !settings.app || !settings.hardware) throw new Error('网关设置数据不完整');
    state.settings = settings;
    state.hardware = settings.hardware;
  } catch (e) {
    state.settings = null;
    state.hardware = {};
  }
}
async function loadSystemInfo() {
  try {
    state.systemInfo = await apiGet('/system-info');
  } catch (e) {
    state.systemInfo = null;
  }
}
// 接入配置状态：网关名用于设置页回显，Token 后端不回传。
async function loadProvisioning() {
  try {
    state.provisioning = await apiGet('/provisioning');
  } catch (e) {
    state.provisioning = null;
  }
}
/* ── 链路持久化（/api/channels）── */
// 通道索引从 0 开始、通道ID 按规则 Channel-[通道索引] 生成，均由后端分配且不可修改。
function channelFromRow(r) {
  return { id: String(r.id || ''), channelIndex: toNum(r.channelIndex, null), name: r.name || '', type: r.type || '',
    reconnectRetries: r.config && r.config.reconnectRetries, resendRetries: r.config && r.config.resendRetries, pollInterval: r.config && r.config.pollInterval,
    frameInterval: r.config && r.config.frameInterval, frameTimeoutMs: r.config && r.config.frameTimeoutMs,
    serialName: hwKey('Serial', r.config && r.config.serialName), baudRate: r.config && r.config.baudRate, dataBits: r.config && r.config.dataBits, parity: r.config && r.config.parity, stopBits: r.config && r.config.stopBits,
    deviceIp: r.config && r.config.deviceIp, devicePort: r.config && r.config.devicePort,
    devices: (r.devices || []).map((d, i) => ({ index: d.index != null ? d.index : i, commNo: String(d.commNo), name: d.name || '', modelId: String(d.modelId) })) };
}
function channelToPayload(c) {
  const config = buildChannelConfig(c);
  // id 为空串表示新建：通道索引与通道ID 由服务端生成后回填。
  return { id: c.id || '', name: c.name || '', type: c.type || '', config: config, devices: config.devices || [] };
}
async function loadChannels() {
  try {
    const rows = await apiGet('/channels');
    state.channels = (rows || []).map(channelFromRow);
  } catch (e) { state.channels = []; }
}

/* ══════════════ Download helpers ══════════════ */
function downloadBlob(blob, filename) {
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url; a.download = filename;
  document.body.appendChild(a); a.click(); a.remove();
  URL.revokeObjectURL(url);
}
function downloadJson(obj, filename) { downloadBlob(new Blob([JSON.stringify(obj, null, 2)], { type: 'application/json' }), filename); }

