/* ══════════════ 网关设置 ══════════════ */
const SETTINGS_CARDS = [
  { icon: 'router', title: '网关信息', subtitle: '标识并描述当前网关实例', fields: [
    { path: 'gateway.gw_id', label: '网关 ID', hint: '仅允许gw_xxx格式；重启后生效。' },
    { path: 'gateway.sn', label: '网关 SN', hint: '网关硬件序列号，用于设备追溯。' },
    { path: 'gateway.location', label: '位置信息', hint: '例如：A 厂区 1 号配电室。' }
  ] },
  { icon: 'journal-text', title: '日志设置', subtitle: '终端、文件与前端日志出口', fields: [
    { path: 'log.level', label: '日志级别', type: 'select', options: [['debug', 'Debug'], ['info', 'Info'], ['warn', 'Warn'], ['error', 'Error']], hint: '保存后立即生效。' },
    { path: 'log.console', label: '终端输出', type: 'boolean', hint: '输出文本日志到标准输出' },
    { path: 'log.file', label: '日志文件路径', hint: '留空则不写入文件' },
    { path: 'log.maxSizeMB', label: '单文件上限 (M)', type: 'number', min: 0 },
    { path: 'log.maxBackups', label: '历史文件份数', type: 'number', min: 0 },
    { path: 'log.maxAgeDays', label: '保留天数 (day)', type: 'number', min: 0 },
    { path: 'log.compress', label: '压缩归档', type: 'boolean', hint: '历史日志使用 gzip 压缩' },
    { path: 'log.dailyRotate', label: '每日轮转', type: 'boolean', hint: '每天 00:00 创建新日志文件' },
    { path: 'log.bufferSize', label: '前端缓冲条数', type: 'number', min: 0, hint: '系统日志 SSE 的最近记录数量' }
  ] },
  { icon: 'broadcast-pin', title: 'NATS 客户端', subtitle: '遥测发布、控制命令与拓扑查询', fields: [
    { path: 'nats.enabled', label: '启用 NATS', type: 'boolean', hint: '开启后在服务启动时连接 NATS' },
    { path: 'nats.url', label: '服务地址', hint: '例如 nats://127.0.0.1:4222' },
    { path: 'nats.name', label: '连接名称' },
    { path: 'nats.subjectPrefix', label: '主题前缀', hint: '例如 powerpulse.gateway' },
    { path: 'nats.queueSize', label: '发布队列长度', type: 'number', min: 1 },
    { path: 'nats.connectTimeout', label: '连接超时 (ms)', type: 'number', min: 0 },
    { path: 'nats.reconnectWait', label: '重连间隔 (ms)', type: 'number', min: 0 },
    { path: 'nats.maxReconnects', label: '最大重连次数', type: 'number', min: -1, hint: '-1 表示无限重连' },
    { path: 'nats.retryOnFailedConnect', label: '失败后重试', type: 'boolean', hint: '允许 nats.go 在连接失败后重试' },
    { path: 'nats.reconnectBufSize', label: '断连缓冲 (bytes)', type: 'number', min: 0 },
    { path: 'nats.pingInterval', label: '心跳间隔 (ms)', type: 'number', min: 0 },
    { path: 'nats.maxPingsOut', label: '最大未响应心跳数', type: 'number', min: 0 }
  ] }
];

const HARDWARE_CATEGORY_META = {
  Serial: { label: '串口接口', icon: 'usb-symbol' }
};

function fieldID(path) { return 'setting-' + path.replaceAll('.', '-'); }
function getPath(object, path) { return path.split('.').reduce((value, key) => value && value[key], object); }
function setPath(object, path, value) {
  const keys = path.split('.');
  const last = keys.pop();
  const target = keys.reduce((value, key) => value[key] || (value[key] = {}), object);
  target[last] = value;
}

function renderField(field, app) {
  const id = fieldID(field.path);
  const value = getPath(app, field.path);
  const hint = field.hint ? `<div class="form-text">${escapeHtml(field.hint)}</div>` : '';
  if (field.type === 'boolean') return `<div class="settings-field"><label for="${id}">${escapeHtml(field.label)}</label><div class="settings-switch"><span>${escapeHtml(field.hint || '')}</span><div class="form-check form-switch m-0"><input id="${id}" class="form-check-input" type="checkbox"${value ? ' checked' : ''}></div></div></div>`;
  if (field.type === 'select') return `<div class="settings-field"><label for="${id}">${escapeHtml(field.label)}</label><select id="${id}" class="form-select">${field.options.map(([optionValue, optionLabel]) => `<option value="${escapeHtml(optionValue)}"${optionValue === value ? ' selected' : ''}>${escapeHtml(optionLabel)}</option>`).join('')}</select>${hint}</div>`;
  const type = field.type === 'number' ? 'number' : 'text';
  const min = field.min !== undefined ? ` min="${field.min}"` : '';
  return `<div class="settings-field"><label for="${id}">${escapeHtml(field.label)}</label><input id="${id}" type="${type}" class="form-control" value="${escapeHtml(value)}"${min}>${hint}</div>`;
}

function settingsCard(icon, title, subtitle, body) {
  return `<article class="settings-card"><div class="settings-card-head"><i class="bi bi-${icon}"></i><div><div class="settings-card-title">${title}</div><div class="settings-card-sub">${subtitle}</div></div></div>${body}</article>`;
}

function systemInfoCard() {
  return settingsCard('info-circle', '软件信息', '当前网关运行环境', `<dl class="settings-static">
    <div><dt>操作系统</dt><dd id="system-operating-system">-</dd></div>
    <div><dt>系统时间</dt><dd id="system-time">-</dd></div>
    <div><dt>Gateway 版本</dt><dd id="system-gateway-version">-</dd></div>
  </dl>`);
}

function softwareSettingsCard() {
  return settingsCard('arrow-repeat', '软件设置', '运行时维护操作', `<div class="settings-restart">
    <p>重新加载网关运行资源、采集链路和 NATS 客户端，HTTP 服务保持可用。</p>
    <button id="gateway-restart" class="btn btn-outline-danger w-100" type="button" onclick="restartGateway()"><i class="bi bi-arrow-clockwise me-1"></i>软件重启</button>
  </div>`);
}

async function restartGateway() {
  if (!confirm('确认软件重启？采集链路会短暂中断。')) return;
  const button = document.getElementById('gateway-restart');
  button.disabled = true;
  button.innerHTML = '<span class="spinner-border spinner-border-sm me-1" aria-hidden="true"></span>正在重启';
  try {
    await apiPost('/restart');
    setTimeout(() => window.location.reload(), 1500);
  } catch (error) {
    button.disabled = false;
    button.innerHTML = '<i class="bi bi-arrow-clockwise me-1"></i>软件重启';
    alert('软件重启失败：' + error.message);
  }
}

function updateSystemInfo() {
  const info = state.systemInfo;
  const operatingSystem = document.getElementById('system-operating-system');
  const systemTime = document.getElementById('system-time');
  const gatewayVersion = document.getElementById('system-gateway-version');
  if (!operatingSystem || !systemTime || !gatewayVersion) return;
  operatingSystem.textContent = info ? info.operatingSystem : '不可用';
  systemTime.textContent = info ? new Date(info.systemTime).toLocaleString() : '不可用';
  gatewayVersion.textContent = info ? info.gatewayVersion : '不可用';
}

function renderSettings() {
  const target = document.getElementById('settings-cards');
  const save = document.getElementById('settings-save');
  if (!target) return;
  if (!state.settings) {
    target.innerHTML = '<div class="empty-state"><i class="bi bi-exclamation-circle"></i><div>无法读取网关设置，请恢复服务连接后重试。</div></div>';
    if (save) save.disabled = true;
    return;
  }
  if (save) save.disabled = false;
  const cards = SETTINGS_CARDS.map(card => settingsCard(card.icon, card.title, card.subtitle, card.fields.map(field => renderField(field, state.settings.app)).join('')));
  const columns = [
    [cards[0], systemInfoCard(), renderHardwareCard()],
    [cards[1], softwareSettingsCard()],
    [cards[2]]
  ];
  target.innerHTML = columns.map(column => `<div class="settings-column">${column.join('')}</div>`).join('');
  updateSystemInfo();
}

function renderHardwareCard() {
  const hardware = state.settings.hardware;
  const categories = [...new Set([...Object.keys(HARDWARE_CATEGORY_META), ...Object.keys(hardware)])];
  const groups = categories.map(category => {
    const entries = hardware[category] || {};
    const meta = HARDWARE_CATEGORY_META[category] || { label: category, icon: 'diagram-3' };
    const rows = Object.entries(entries).map(([label, node]) => hardwareRow(category, label, node)).join('');
    return `<div class="settings-hardware-group"><div class="settings-hardware-title"><span><i class="bi bi-${meta.icon} me-1"></i>${escapeHtml(meta.label)}</span><button class="btn btn-outline-secondary btn-sm" type="button" data-category="${escapeHtml(category)}" onclick="addHardwareRow(this)" title="添加接口"><i class="bi bi-plus-lg"></i></button></div><div class="settings-hardware-rows">${rows}</div></div>`;
  }).join('');
  return settingsCard('motherboard', '接口映射', '面板丝印与实际设备节点', groups);
}

function hardwareRow(category, label = '', node = '') {
  return `<div class="settings-hardware-row" data-category="${escapeHtml(category)}"><input class="form-control form-control-sm hardware-label" value="${escapeHtml(label)}" placeholder="丝印，如 COM1"><input class="form-control form-control-sm hardware-node" value="${escapeHtml(node)}" placeholder="设备节点"><button class="btn btn-outline-danger btn-sm" type="button" onclick="removeHardwareRow(this)" title="删除接口"><i class="bi bi-trash"></i></button></div>`;
}

function addHardwareRow(button) {
  const target = button.closest('.settings-hardware-group').querySelector('.settings-hardware-rows');
  if (target) target.insertAdjacentHTML('beforeend', hardwareRow(button.dataset.category));
}

function removeHardwareRow(button) {
  button.closest('.settings-hardware-row').remove();
}

function collectHardwareSettings() {
  const hardware = Object.fromEntries(Object.keys(state.settings.hardware)
    .map(category => [category, {}]));
  document.querySelectorAll('.settings-hardware-row').forEach(row => {
    const category = row.dataset.category;
    const label = row.querySelector('.hardware-label').value.trim();
    const node = row.querySelector('.hardware-node').value.trim();
    if (!label && !node) return;
    if (!label || !node) throw new Error('硬件接口的丝印和设备节点必须同时填写');
    if (!hardware[category]) hardware[category] = {};
    if (hardware[category][label]) throw new Error('硬件丝印不能重复：' + label);
    hardware[category][label] = node;
  });
  return hardware;
}

async function saveSettings() {
  try {
    if (!state.settings) throw new Error('设置尚未加载，无法保存');
    const app = deepCopy(state.settings.app);
    SETTINGS_CARDS.forEach(card => card.fields.forEach(field => {
      const input = document.getElementById(fieldID(field.path));
      let value = field.type === 'boolean' ? input.checked : input.value.trim();
      if (field.type === 'number') {
        value = Number(value);
        if (!Number.isFinite(value)) throw new Error(`${field.label} 必须是数字`);
      }
      setPath(app, field.path, value);
    }));
    const payload = { app, hardware: collectHardwareSettings() };
    const saved = await apiPost('/settings', payload);
    state.settings = saved;
    state.hardware = saved.hardware;
    renderSettings();
    alert('设置已保存。日志设置已生效，网关信息和 NATS 设置将在重启服务后生效。');
  } catch (error) {
    alert('保存设置失败：' + error.message);
  }
}