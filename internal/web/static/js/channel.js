/* ══════════════ 链路配置 · Channels ══════════════ */
function emptyChannel() {
  return { id: '', channelIndex: null, name:'', type:'', reconnectRetries:'0', resendRetries:'3', pollInterval:'500', serialName:'', baudRate:'9600', dataBits:'8', parity:'None', stopBits:'1',
    deviceIp:'', devicePort:'', devices: [] };
}
// 通道索引分配：从 0 开始取最小未占用值（与档案索引规则一致），后端保存时再次校验分配。
function nextChannelIndex() {
  let n = 0;
  while (state.channels.some(c => toNum(c.channelIndex, -1) === n)) n++;
  return n;
}
// 通道ID 生成规则：Channel-[通道索引]，自动生成、不可修改。
function channelIdFromIndex(n) { return 'Channel-' + n; }
function onChannelTypeChange() {
  const t = document.getElementById('ch-type').value;
  document.getElementById('ch-grp-serial').classList.toggle('d-none', t !== 'Serial');
  document.getElementById('ch-grp-network').classList.toggle('d-none', t !== 'Network');
  fillHardwareSelects();
}
function hwOptionsHtml(category, selected) {
  const map = (state.hardware && state.hardware[category]) || {};
  const keys = Object.keys(map).sort();
  let html = '<option value="">请选择端口</option>';
  let matched = false;
  for (const k of keys) {
    const sel = String(k) === String(selected) ? 'selected' : '';
    if (sel) matched = true;
    html += `<option value="${escapeHtml(k)}" ${sel}>${escapeHtml(k)} · ${escapeHtml(map[k])}</option>`;
  }
  if (selected && !matched) html += `<option value="${escapeHtml(selected)}" selected>${escapeHtml(selected)}</option>`;
  return html;
}
function fillHardwareSelects() {
  const c = state.channel || {};
  const s = document.getElementById('ch-serialName');
  if (s) s.innerHTML = hwOptionsHtml('Serial', c.serialName);
}
function hwNode(category, key) {
  if (!key) return '';
  const map = (state.hardware && state.hardware[category]) || {};
  return Object.prototype.hasOwnProperty.call(map, key) ? map[key] : key;
}
// 反向查找：由设备节点值反查丝印标签 key（用于从后端重新加载时还原下拉值）
function hwKey(category, node) {
  if (!node) return '';
  const map = (state.hardware && state.hardware[category]) || {};
  for (const k of Object.keys(map)) { if (String(map[k]) === String(node)) return k; }
  return node;
}
function nextCommNo(devices) {
  const used = (devices || []).map(d => Number(d.commNo)).filter(n => Number.isInteger(n) && n > 0);
  let n = 1; while (used.includes(n)) n++; return n;
}
function chDeviceModelOptions(selectedId) {
  return ['<option value="">请选择设备模板</option>'].concat(state.models.map(m => {
    const p = m.profile || {};
    const sel = String(m.id) === String(selectedId) ? 'selected' : '';
    return `<option value="${escapeHtml(m.id)}" ${sel}>${escapeHtml(p.name || '未命名设备模型')}</option>`;
  })).join('');
}
function readChannelDeviceRows() {
  return Array.from(document.querySelectorAll('#ch-device-rows tr')).map(tr => {
    const commEl = tr.querySelector('.ch-comm-input');
    const nameEl = tr.querySelector('.ch-name-input');
    const selEl = tr.querySelector('select');
    const raw = commEl ? commEl.value.trim() : '';
    return { commNo: raw === '' ? '' : Number(raw), name: nameEl ? nameEl.value.trim() : '', modelId: selEl ? selEl.value : '' };
  });
}
function renderChannelDeviceTable() {
  const wrap = document.getElementById('ch-device-table-wrap');
  const tbody = document.getElementById('ch-device-rows');
  const empty = document.getElementById('ch-device-empty');
  const noModels = document.getElementById('ch-device-nomodels');
  if (!wrap || !tbody) return;
  const devices = (state.channel && state.channel.devices) || [];
  if (!state.models.length) {
    wrap.classList.add('d-none'); empty.classList.add('d-none'); noModels.classList.remove('d-none'); tbody.innerHTML = '';
    return;
  }
  noModels.classList.add('d-none');
  if (!devices.length) {
    wrap.classList.add('d-none'); empty.classList.remove('d-none'); tbody.innerHTML = ''; return;
  }
  empty.classList.add('d-none'); wrap.classList.remove('d-none');
  tbody.innerHTML = devices.map((d, i) => `
    <tr>
      <td><span class="ch-device-idx">${i}</span></td>
      <td><input type="number" min="1" max="247" step="1" class="form-control form-control-sm ch-comm-input" value="${escapeHtml(d.commNo ?? '')}" oninput="onCommNoInput()"></td>
      <td><input type="text" maxlength="64" placeholder="请输入设备名称" class="form-control form-control-sm ch-name-input" value="${escapeHtml(d.name ?? '')}"></td>
      <td><select class="form-select form-select-sm" oninput="onCommNoInput()">${chDeviceModelOptions(d.modelId)}</select></td>
      <td class="text-end"><button type="button" class="btn btn-outline-danger btn-sm" onclick="chRemoveDevice(${i})" title="移除"><i class="bi bi-trash"></i></button></td>
    </tr>`).join('');
  validateChannelDevices(false);
}
function chAddDevice() {
  if (!state.channel) return;
  if (!state.models.length) { alert('暂无设备模型，请先在「设备模型」中创建。'); return; }
  state.channel.devices = readChannelDeviceRows();
  state.channel.devices.push({ commNo: nextCommNo(state.channel.devices), name: '', modelId: state.models[0].id });
  renderChannelDeviceTable();
}
function chRemoveDevice(i) {
  if (!state.channel) return;
  state.channel.devices = readChannelDeviceRows();
  state.channel.devices.splice(i, 1);
  renderChannelDeviceTable();
}
function onCommNoInput() { validateChannelDevices(false); }
function validateChannelDevices(report = true) {
  const rows = Array.from(document.querySelectorAll('#ch-device-rows tr'));
  let ok = true, firstBad = null;
  const seen = {};
  rows.forEach(tr => {
    const commEl = tr.querySelector('.ch-comm-input');
    const nameEl = tr.querySelector('.ch-name-input');
    const selEl = tr.querySelector('select');
    commEl.classList.remove('is-invalid');
    nameEl.classList.remove('is-invalid');
    selEl.classList.remove('is-invalid');
    const v = commEl.value.trim();
    const n = Number(v);
    if (v === '' || !Number.isInteger(n) || n < 1 || n > 247) { commEl.classList.add('is-invalid'); ok = false; firstBad = firstBad || commEl; }
    if (!nameEl.value.trim()) { nameEl.classList.add('is-invalid'); ok = false; firstBad = firstBad || nameEl; }
    if (!selEl.value) { selEl.classList.add('is-invalid'); ok = false; firstBad = firstBad || selEl; }
  });
  rows.forEach(tr => {
    const commEl = tr.querySelector('.ch-comm-input');
    const v = commEl.value.trim();
    if (v === '' || !Number.isInteger(Number(v))) return;
    if (seen[v]) { commEl.classList.add('is-invalid'); seen[v].classList.add('is-invalid'); ok = false; firstBad = firstBad || commEl; }
    else seen[v] = commEl;
  });
  if (!ok && report) {
    if (firstBad) firstBad.focus();
    alert('请检查设备配置：设备名称必填、通讯号需为 1 到 247 的整数、同一链路内不可重复，且每行需选择设备模板。');
  }
  return ok;
}
// 生成链路占用的硬件资源唯一键：串口以端口名唯一，网络以 IP+端口 唯一。
// 返回 null 表示当前配置缺少可比较的资源标识，不参与冲突判断。
function channelConflictKey(c) {
  if (!c) return null;
  if (c.type === 'Serial') {
    const s = (c.serialName || '').trim();
    return s ? { kind: 'Serial', key: s.toLowerCase(), label: `串口 ${s}` } : null;
  }
  if (c.type === 'Network') {
    const ip = (c.deviceIp || '').trim();
    const port = String(c.devicePort ?? '').trim();
    return (ip && port) ? { kind: 'Network', key: `${ip.toLowerCase()}:${port}`, label: `${ip}:${port}` } : null;
  }
  return null;
}
// 冲突检测：同一串口端口，或同一网络 IP+端口，不能被多个链路共用。
function validateChannelConflict(report = true) {
  const key = channelConflictKey(state.channel);
  if (!key) return true; // 缺少可比较的唯一标识时不拦截
  for (let i = 0; i < state.channels.length; i++) {
    if (i === channelEditIndex) continue; // 跳过正在编辑的链路自身
    const other = channelConflictKey(state.channels[i]);
    if (other && other.kind === key.kind && other.key === key.key) {
      if (report) alert(`链路配置冲突：${key.label} 已被链路「${state.channels[i].name || '未命名链路'}」占用，同一串口端口或网络 IP+端口 不能被多个链路共用。`);
      return false;
    }
  }
  return true;
}
/* ── 链路配置向导（1·链路配置 2·设备配置 3·预览） ── */
function newChannel() {
  state.channel = emptyChannel();
  // 通道索引从 0 开始自动分配；通道ID 按 Channel-[通道索引] 生成，二者均不可修改。
  state.channel.channelIndex = nextChannelIndex();
  state.channel.id = channelIdFromIndex(state.channel.channelIndex);
  channelEditIndex = -1;
  enterChannelWizard();
}
function editChannel(idx) {
  state.channel = deepCopy(state.channels[idx]);
  channelEditIndex = idx;
  enterChannelWizard();
}
function enterChannelWizard() {
  document.getElementById('channel-landing').classList.add('d-none');
  document.getElementById('channel-wizard').classList.remove('d-none');
  document.getElementById('ch-wizard-title').textContent = state.channel.name || '新建链路';
  document.getElementById('form-channel').classList.remove('was-validated');
  chCurrentStep = 1;
  fillChannelForm();
  chRenderStep();
}
function showChannelLanding() {
  document.getElementById('channel-wizard').classList.add('d-none');
  document.getElementById('channel-landing').classList.remove('d-none');
}
function backToChannelList() {
  // 返回列表只放弃当前编辑缓冲，不持久化；只有保存按钮才会写入数据库
  state.channel = null;
  channelEditIndex = -1;
  showChannelLanding();
  renderChannelList();
}
function fillChannelForm() {
  const c = state.channel || emptyChannel();
  // 通道索引 / 通道ID 为自动生成的只读展示，不参与表单回读。
  setVal('ch-index', c.channelIndex); setVal('ch-id', c.id || (c.channelIndex == null ? '' : channelIdFromIndex(c.channelIndex)));
  setVal('ch-name', c.name); setVal('ch-type', c.type);
  setVal('ch-reconnectRetries', c.reconnectRetries); setVal('ch-resendRetries', c.resendRetries); setVal('ch-pollInterval', c.pollInterval);
  setVal('ch-baud', c.baudRate); setVal('ch-dataBits', c.dataBits);
  setVal('ch-parity', c.parity); setVal('ch-stopBits', c.stopBits);
  setVal('ch-deviceIp', c.deviceIp); setVal('ch-devicePort', c.devicePort);
  renderChannelDeviceTable();
  onChannelTypeChange();
}
function syncChannelFromForm() {
  if (!state.channel) return;
  const c = state.channel;
  c.name = val('ch-name'); c.type = val('ch-type');
  c.reconnectRetries = val('ch-reconnectRetries'); c.resendRetries = val('ch-resendRetries'); c.pollInterval = val('ch-pollInterval');
  c.serialName = val('ch-serialName'); c.baudRate = val('ch-baud'); c.dataBits = val('ch-dataBits'); c.parity = val('ch-parity'); c.stopBits = val('ch-stopBits');
  c.deviceIp = val('ch-deviceIp'); c.devicePort = val('ch-devicePort');
  c.devices = readChannelDeviceRows();
}
function validateChannelStep1() {
  const form = document.getElementById('form-channel');
  form.classList.add('was-validated');
  return form.checkValidity();
}
function chGoStep(n) {
  if (n < 1 || n > CH_TOTAL_STEPS) return;
  if (n > 1 && chCurrentStep === 1 && !validateChannelStep1()) return;
  syncChannelFromForm();
  if (n > 1 && chCurrentStep === 1 && !validateChannelConflict()) return;
  if (n >= 3 && !validateChannelDevices()) { chCurrentStep = 2; chRenderStep(); return; }
  chCurrentStep = n;
  chRenderStep();
}
function chNextStep() { chGoStep(chCurrentStep + 1); }
function chPrevStep() { chGoStep(chCurrentStep - 1); }
function chRenderStep() {
  for (let i = 1; i <= CH_TOTAL_STEPS; i++) {
    document.getElementById('ch-step-' + i).classList.toggle('d-none', i !== chCurrentStep);
    const si = document.getElementById('ch-si-' + i);
    si.classList.toggle('active', i === chCurrentStep);
    si.classList.toggle('completed', i < chCurrentStep);
  }
  document.getElementById('ch-btn-prev').disabled = chCurrentStep === 1;
  document.getElementById('ch-btn-next').disabled = chCurrentStep === CH_TOTAL_STEPS;
  document.getElementById('ch-step-counter').textContent = `第 ${chCurrentStep} 步 / 共 ${CH_TOTAL_STEPS} 步`;
  if (chCurrentStep === 3) chRenderPreview();
}
function chRenderPreview() {
  syncChannelFromForm();
  document.getElementById('ch-pre-config').textContent = JSON.stringify(buildChannelConfig(state.channel), null, 2);
  document.getElementById('ch-sum-name').textContent = state.channel.name || '—';
  document.getElementById('ch-sum-type').textContent = CHANNEL_TYPE_LABEL[state.channel.type] || '—';
  document.getElementById('ch-sum-models').textContent = (state.channel.devices || []).length;
}
function saveChannel() {
  if (!validateChannelStep1()) { chGoStep(1); return; }
  syncChannelFromForm();
  if (!validateChannelConflict()) { chGoStep(1); return; }
  if (!validateChannelDevices()) { chGoStep(2); return; }
  const payload = channelToPayload(state.channel);
  const wasNew = !state.channel.id;
  apiPost('/channels', payload)
    .then(saved => {
      // 新建时回填后端分配的通道索引与通道ID（Channel-{索引}），二者此后不可修改。
      if (saved && saved.id) state.channel.id = String(saved.id);
      if (saved && saved.channelIndex != null) state.channel.channelIndex = toNum(saved.channelIndex, state.channel.channelIndex);
      const ch = deepCopy(state.channel);
      if (channelEditIndex >= 0) state.channels[channelEditIndex] = ch;
      else { state.channels.push(ch); channelEditIndex = state.channels.length - 1; }
      alert(wasNew ? '链路已创建并保存到数据库' : '链路已更新');
      backToChannelList();
    })
    .catch(e => alert('保存失败：' + e.message));
}
function buildChannelConfig(c) {
  const devices = (c.devices || []).map((d, i) => {
    const m = state.models.find(x => String(x.id) === String(d.modelId));
    return { index: i, commNo: toNum(d.commNo, null), name: d.name || '', modelId: d.modelId || null, modelName: (m && m.profile && m.profile.name) || null };
  });
  const base = { id: c.id || '', channelIndex: toNum(c.channelIndex, null), name: c.name, type: c.type,
    reconnectRetries: toNum(c.reconnectRetries, null), resendRetries: toNum(c.resendRetries, null), pollInterval: toNum(c.pollInterval, null),
    devices };
  if (c.type === 'Serial') Object.assign(base, { serialName: hwNode('Serial', c.serialName), baudRate: toNum(c.baudRate, null), dataBits: toNum(c.dataBits, null), parity: c.parity, stopBits: c.stopBits });
  if (c.type === 'Network') Object.assign(base, { deviceIp: c.deviceIp, devicePort: c.devicePort === '' ? null : toNum(c.devicePort, null) });
  return base;
}
function deleteChannel(idx) {
  if (!confirm('确定删除该链路？')) return;
  const c = state.channels[idx];
  if (!c) return;
  apiDelete('/channels/' + encodeURIComponent(String(c.id || '')))
    .then(() => {
      state.channels.splice(idx, 1);
      if (channelEditIndex === idx) { state.channel = null; channelEditIndex = -1; }
      renderChannelList();
    })
    .catch(e => alert('删除失败：' + e.message));
}
function channelConfigTags(c) {
  if (c.type === 'Serial') return [c.serialName ? `串口：${c.serialName}` : '', `${c.baudRate} bps`, `${c.dataBits}${PARITY_LABEL[c.parity] || c.parity}${c.stopBits}`];
  if (c.type === 'Network') return [(c.deviceIp ? c.deviceIp : '') + (c.devicePort ? ':' + c.devicePort : '')];
  return [];
}
function renderChannelList() {
  const wrap = document.getElementById('channel-cards');
  document.getElementById('channel-count').textContent = state.channels.length;
  document.getElementById('channel-empty').classList.toggle('d-none', state.channels.length > 0);
  wrap.innerHTML = state.channels.map((c, i) => {
    const tags = channelConfigTags(c).filter(Boolean).map(t => `<span class="mc-tag">${escapeHtml(t)}</span>`).join('');
    const devices = c.devices || [];
    const mounted = devices.length
      ? devices.map(d => { const m = state.models.find(x => String(x.id) === String(d.modelId)); const nm = (m && m.profile && m.profile.name) || '未知模板'; return `<span class="mc-tag iface">#${escapeHtml(String(d.commNo))} · ${escapeHtml(d.name || nm)}</span>`; }).join('')
      : '<span class="text-muted small">未挂载设备</span>';
    return `
      <div class="col-md-6 col-xl-4">
        <div class="model-card">
          <div class="model-card-top">
            <div class="model-card-icon"><i class="bi bi-${CHANNEL_TYPE_ICON[c.type] || 'diagram-3'}"></i></div>
            <div class="min-w-0">
              <div class="model-card-title">${escapeHtml(c.name || '未命名链路')}</div>
              <div class="model-card-sub">通道ID：${escapeHtml(c.id || (c.channelIndex == null ? '—' : channelIdFromIndex(c.channelIndex)))} · ${escapeHtml(CHANNEL_TYPE_LABEL[c.type] || '未设置类型')}</div>
            </div>
          </div>
          <div class="model-card-tags">${tags}</div>
          <div><div class="preview-label mb-1"><i class="bi bi-cpu me-1"></i>挂载设备（${devices.length}）</div><div class="ch-mounted">${mounted}</div></div>
          <div class="model-card-actions mt-auto">
            <button class="btn btn-primary btn-sm flex-grow-1" onclick="editChannel(${i})"><i class="bi bi-gear me-1"></i>配置</button>
            <button class="btn btn-outline-danger btn-sm" onclick="deleteChannel(${i})" title="删除"><i class="bi bi-trash"></i></button>
          </div>
        </div>
      </div>`;
  }).join('');
}

