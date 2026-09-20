/* ══════════════ 报文信息 · Communication monitor ══════════════ */
let logEvents = [];
let logNextSeq = 0;
let logTimer = null;
let logLoading = false;

function renderLogSelectors() {
  // 链路下拉按通道索引（从 0 开始）取值，标签展示自动生成的通道ID（Channel-{索引}）。
  fillSelect(document.getElementById('log-channel'), state.channels, c => c.channelIndex, c => `${c.id || channelIdFromIndex(c.channelIndex)} · ${c.name}`, true);
  onLogChannelChange();
  logStartPolling();
}

function onLogChannelChange() {
  const ch = state.channels.find(c => String(c.channelIndex) === document.getElementById('log-channel').value);
  const devices = [{ index: -1, commNo: '', name: '', model: null }].concat(channelDevices(ch));
  fillSelect(document.getElementById('log-device'), devices,
    d => d.index, d => d.index < 0 ? '全部设备' : `#${d.commNo} · ${d.name || (d.model.profile && d.model.profile.name) || '未命名设备'}`, false);
  resetCommLog();
}

function onLogDeviceChange() {
  resetCommLog();
}

function resetCommLog() {
  logEvents = [];
  logNextSeq = 0;
  renderCommLog();
  fetchCommLog(true);
}

function logStartPolling() {
  logStopPolling();
  logTimer = setInterval(() => fetchCommLog(false), 2000);
}

function logStopPolling() {
  if (logTimer) { clearInterval(logTimer); logTimer = null; }
}

function logSelection() {
  const channel = document.getElementById('log-channel');
  const device = document.getElementById('log-device');
  if (!channel || !device || channel.value === '') return null;
  // 通道索引从 0 开始（0 是有效值），用 -1 表示无效。
  const channelIndex = toNum(channel.value, -1);
  if (channelIndex < 0) return null;
  return { channelIndex, deviceIndex: toNum(device.value, -1) };
}

async function fetchCommLog(force) {
  const sel = logSelection();
  if (!sel || logLoading) return;
  logLoading = true;
  const selectionKey = sel.channelIndex + '/' + sel.deviceIndex;
  try {
    let url = '/comm-monitor?channelIndex=' + encodeURIComponent(sel.channelIndex) + '&limit=200';
    if (sel.deviceIndex >= 0) url += '&deviceIndex=' + encodeURIComponent(sel.deviceIndex);
    if (!force && logNextSeq) url += '&afterSeq=' + encodeURIComponent(logNextSeq);
    const data = await apiGet(url);
    const active = logSelection();
    if (!active || active.channelIndex + '/' + active.deviceIndex !== selectionKey) return;
    if (force) logEvents = data.events || [];
    else logEvents = logEvents.concat(data.events || []);
    if (logEvents.length > 200) logEvents = logEvents.slice(-200);
    logNextSeq = data.nextSeq || 0;
    renderCommLog(data.stats);
  } catch (e) {
    renderCommLog(null, '暂无通讯监控数据（链路未运行或尚未建立连接）。');
  } finally {
    logLoading = false;
  }
}

function renderCommLog(stats, emptyMessage) {
  const errorRate = document.getElementById('log-err');
  if (errorRate) errorRate.textContent = stats ? Number(stats.errorRate || 0).toFixed(2) + '%' : '—';
  const consoleEl = document.getElementById('log-console');
  if (!consoleEl) return;
  if (!logEvents.length) {
    consoleEl.textContent = emptyMessage || '等待通讯报文…';
    return;
  }
  consoleEl.textContent = logEvents.map(formatCommEvent).join('\n');
  consoleEl.scrollTop = consoleEl.scrollHeight;
}

function formatCommEvent(event) {
  const time = event.time ? formatLogTime(event.time) : '—';
  const device = '#' + event.unitId;
  const operation = event.operation === 'write' ? '写入' : '读取';
  if (event.direction === 'ERR') return `${time} ${device} ERR ${operation} 第${event.attempt}次: ${event.error || '通讯失败'}`;
  return `${time} ${device} ${event.direction}: ${event.hex || ''}`;
}

function formatLogTime(value) {
  const date = new Date(value);
  const pad = (number, width = 2) => String(number).padStart(width, '0');
  return `${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}.${pad(date.getMilliseconds(), 3)}`;
}
