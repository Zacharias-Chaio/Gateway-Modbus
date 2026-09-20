/* ══════════════ Wizard navigation ══════════════ */
function goStep(n) {
  if (n < 1 || n > TOTAL_STEPS) return;
  if (n > 1 && currentStep === 1 && !validateProfile()) return;
  syncProfileFromForm();
  currentStep = n;
  renderStep();
}
function nextStep() { goStep(currentStep + 1); }
function prevStep() { goStep(currentStep - 1); }
function renderStep() {
  for (let i = 1; i <= TOTAL_STEPS; i++) {
    document.getElementById('step-' + i).classList.toggle('d-none', i !== currentStep);
    const si = document.getElementById('si-' + i);
    si.classList.toggle('active', i === currentStep);
    si.classList.toggle('completed', i < currentStep);
  }
  document.getElementById('btn-prev').disabled = currentStep === 1;
  document.getElementById('btn-next').disabled = currentStep === TOTAL_STEPS;
  document.getElementById('step-counter').textContent = `第 ${currentStep} 步 / 共 ${TOTAL_STEPS} 步`;
  if (currentStep === TOTAL_STEPS) renderPreview();
}

/* ══════════════ Step 1 · Profile ══════════════ */
function onInterfaceChange() { rebuildProtocolOptions(''); }
function rebuildProtocolOptions(selected) {
  const iface = document.getElementById('pf-interface').value;
  const sel = document.getElementById('pf-protocol');
  const protos = PROTOCOLS_BY_INTERFACE[iface] || [];
  sel.innerHTML = '<option value="">' + (protos.length ? '请选择协议类型' : '请先选择接口类型') + '</option>'
    + protos.map(p => `<option value="${escapeHtml(p)}">${escapeHtml(p)}</option>`).join('');
  if (selected && protos.includes(selected)) sel.value = selected;
}
function validateProfile() {
  const form = document.getElementById('form-profile');
  form.classList.add('was-validated');
  return form.checkValidity();
}
function syncProfileFromForm() {
  state.profile.profileIndex = toNum(val('pf-profileIndex'), null);
  state.profile.profileId = val('pf-profileId');
  state.profile.name = val('pf-name');
  state.profile.manufacturer = val('pf-manufacturer');
  state.profile.description = val('pf-description');
  state.profile.deviceType = val('pf-deviceType');
  state.profile.deviceModel = val('pf-deviceModel');
  state.profile.ratedPower = toNum(val('pf-ratedPower'), null);
  state.profile.interfaceType = val('pf-interface');
  state.profile.protocolType = val('pf-protocol');
  state.profile.protocolVersion = val('pf-version');
  state.profile.maxRegisterCount = toNum(val('pf-maxRegs'), 100);
}
function fillProfileForm() {
  setVal('pf-profileIndex', state.profile.profileIndex);
  setVal('pf-profileId', state.profile.profileId);
  setVal('pf-name', state.profile.name);
  setVal('pf-manufacturer', state.profile.manufacturer);
  setVal('pf-description', state.profile.description);
  setVal('pf-deviceType', state.profile.deviceType);
  setVal('pf-deviceModel', state.profile.deviceModel);
  setVal('pf-ratedPower', state.profile.ratedPower);
  setVal('pf-interface', state.profile.interfaceType);
  rebuildProtocolOptions(state.profile.protocolType);
  setVal('pf-version', state.profile.protocolVersion);
  setVal('pf-maxRegs', state.profile.maxRegisterCount ?? 100);
}

/* ══════════════ Project import / export (整体) ══════════════ */
/* ══════════════ Device model landing (multi-model) ══════════════ */
const IFACE_LABEL = { Serial:'串口', Network:'网络' };
function ifaceLabel(v) { return IFACE_LABEL[v] || v || '—'; }
function emptyProfile() { return { profileIndex:null, profileId:'', name:'', manufacturer:'', description:'', deviceType:'', deviceModel:'', ratedPower:null, interfaceType:'', protocolType:'', protocolVersion:'', maxRegisterCount: 100 }; }
function nextProfileIndex() { let n = 0; while (state.models.some(m => m.profile && String(m.profile.profileIndex) === String(n))) n++; return n; }
// 属性ID 规则：Property-[属性索引]，自动生成、不可修改。
// 索引 0 由默认在线属性（online，保持原有规则）占用，新属性从 1 起取最小未占用值。
function nextPropIndex() { let n = 1; while (state.properties.some(p => p.id === 'Property-' + n)) n++; return n; }
// 寄存器地址 = 基址 + 偏移，十进制/十六进制双显（如 10/000AH）
function regAddrDisplay(p) {
  if (p.registerBase == null || p.registerOffset == null) return '—';
  const addr = p.registerBase + p.registerOffset;
  return `${addr}/${addr.toString(16).toUpperCase().padStart(4, '0')}H`;
}
function loadModelIntoBuffer(m) {
  state.profile = deepCopy(m.profile);
  state.properties = deepCopy(m.properties);
}
function saveBufferIntoModel() {
  syncProfileFromForm();
  const m = state.models.find(x => x.id === state.editingId);
  if (!m) return;
  m.profile = deepCopy(state.profile);
  m.properties = deepCopy(state.properties);
}
function newModel() {
  const prof = emptyProfile();
  prof.profileIndex = nextProfileIndex();
  // 档案ID 规则：Profile-[档案索引号]，自动生成，不可修改
  const modelId = 'Profile-' + prof.profileIndex;
  prof.profileId = modelId;
  const m = {
    id: modelId,
    profile: prof,
    properties: [
      { id:'online', name:'在线状态', description:'{"offline":0,"online":1}', dataType:'bool', unit:'', accessMode:'r', startBit:0, endBit:0, deltaValue:0, coefficient:1, readFunctionCode:null, writeFunctionCode:null, registerBase:null, registerOffset:null, byteOrder:'' }
    ]
  };
  state.models.push(m);
  state.editingId = m.id;
  loadModelIntoBuffer(m);
  enterWizard();
}
function editModel(id) {
  const m = state.models.find(x => x.id === id);
  if (!m) return;
  state.editingId = id;
  loadModelIntoBuffer(m);
  enterWizard();
}
function deleteModel(id) {
  if (!confirm('确定删除该设备模型？此操作不可恢复。')) return;
  const m = state.models.find(x => x.id === id);
  state.models = state.models.filter(m => m.id !== id);
  if (state.editingId === id) state.editingId = null;
  if (m && m.profile && m.profile.profileId) apiDelete('/models/' + encodeURIComponent(m.profile.profileId)).catch(() => {});
  renderModelList();
}
function enterWizard() {
  document.getElementById('device-landing').classList.add('d-none');
  document.getElementById('device-wizard').classList.remove('d-none');
  document.getElementById('wizard-title').textContent = state.profile.name || '新建设备模型';
  currentStep = 1;
  fillProfileForm();
  renderProps();
  renderStep();
}
function showLanding() {
  document.getElementById('device-wizard').classList.add('d-none');
  document.getElementById('device-landing').classList.remove('d-none');
}
function backToList() {
  saveBufferIntoModel();
  const m = state.models.find(x => x.id === state.editingId);
  // discard a freshly-created model left completely empty
  if (m && !m.profile.name && !m.properties.length) {
    state.models = state.models.filter(x => x.id !== state.editingId);
  }
  state.editingId = null;
  showLanding();
  renderModelList();
}
function renderModelList() {
  const wrap = document.getElementById('model-cards');
  document.getElementById('model-count').textContent = state.models.length;
  document.getElementById('model-empty').classList.toggle('d-none', state.models.length > 0);
  wrap.innerHTML = state.models.map(m => {
    const p = m.profile || {};
    const tags = [
      `<span class="mc-tag iface">${escapeHtml(ifaceLabel(p.interfaceType))}</span>`,
      p.protocolType ? `<span class="mc-tag">${escapeHtml(p.protocolType)}</span>` : '',
      p.protocolVersion ? `<span class="mc-tag">${escapeHtml(p.protocolVersion)}</span>` : ''
    ].join('');
    const desc = p.description ? `<div class="model-card-desc">${escapeHtml(p.description)}</div>` : '';
    return `
      <div class="col-md-6 col-xl-4">
        <div class="model-card">
          <div class="model-card-top">
            <div class="model-card-icon"><i class="bi bi-cpu"></i></div>
            <div class="min-w-0">
              <div class="model-card-title">${escapeHtml(p.name || '未命名设备模型')}</div>
              <div class="model-card-sub">${escapeHtml(p.manufacturer || '未填写厂商')}</div>
            </div>
          </div>
          <div class="model-card-tags">${tags}</div>
          ${desc}
          <div class="model-card-stats">
            <div class="mc-stat"><div class="num">${m.properties.length}</div><div class="lbl">属性</div></div>
          </div>
          <div class="model-card-actions">
            <button class="btn btn-primary btn-sm flex-grow-1" onclick="editModel('${m.id}')"><i class="bi bi-gear me-1"></i>配置</button>
            <button class="btn btn-outline-danger btn-sm" onclick="deleteModel('${m.id}')" title="删除"><i class="bi bi-trash"></i></button>
          </div>
        </div>
      </div>`;
  }).join('');
}

function importProject(input) {
  const file = input.files[0];
  if (!file) return;
  const reader = new FileReader();
  reader.onload = e => {
    try {
      const data = JSON.parse(e.target.result);
      if ((state.properties.length) && !confirm('导入将覆盖当前配置，是否继续？')) { input.value = ''; return; }
      state.profile = Object.assign(emptyProfile(), data.profile || {});
      state.properties = Array.isArray(data.properties) ? data.properties : [];
      fillProfileForm();
      renderProps();
      alert('配置导入成功');
    } catch (err) {
      alert('导入失败：JSON 解析错误 - ' + err.message);
    }
    input.value = '';
  };
  reader.readAsText(file);
}

/* ══════════════ Step 2 · Properties ══════════════ */
function openPropModal(idx = -1) {
  propEditIndex = idx;
  const form = document.getElementById('form-prop');
  form.classList.remove('was-validated');
  const propIdx = idx >= 0 ? idx : nextPropIndex();
  const p = idx >= 0 ? state.properties[idx]
    : { id: 'Property-' + propIdx, name:'', description:'', dataType:'', unit:'', accessMode:'', startBit:0, endBit:0, deltaValue:0, coefficient:1, readFunctionCode:null, writeFunctionCode:null, registerBase:null, registerOffset:null, registerCount:1, byteOrder:'' };
  setVal('pm-index', propIdx);
  setVal('pm-id', p.id); setVal('pm-name', p.name); setVal('pm-desc', p.description);
  setVal('pm-dataType', p.dataType); setVal('pm-unit', p.unit); setVal('pm-access', p.accessMode);
  setVal('pm-startbit', p.startBit ?? 0);
  setVal('pm-endbit', p.endBit ?? 0);
  setVal('pm-base', p.deltaValue ?? 0); setVal('pm-coef', p.coefficient);
  setVal('pm-readfunc', p.readFunctionCode ?? ''); setVal('pm-writefunc', p.writeFunctionCode ?? '');
  setVal('pm-regbase', p.registerBase != null ? p.registerBase : ''); setVal('pm-regoffset', p.registerOffset != null ? p.registerOffset : '');
  setVal('pm-regcount', p.registerCount != null ? p.registerCount : ''); setVal('pm-byteorder', p.byteOrder);
  // 属性ID 自动生成（在线点保持 online），一律不可修改
  document.getElementById('pm-id').readOnly = true;
  document.getElementById('propModalTitle').textContent = idx >= 0 ? '编辑属性' : '添加属性';
  propModal.show();
}
function saveProp() {
  const form = document.getElementById('form-prop');
  form.classList.add('was-validated');
  if (!form.checkValidity()) return;
  const id = val('pm-id');
  if (!/^[A-Za-z_][A-Za-z0-9_-]*$/.test(id)) { alert('属性ID 格式非法（自动生成，无需填写）'); return; }
  const dup = state.properties.findIndex(p => p.id === id);
  if (dup >= 0 && dup !== propEditIndex) { alert('属性ID 已存在：' + id); return; }
  const dataType = val('pm-dataType');
  const startBit = toNum(val('pm-startbit'), 0);
  const endBit = toNum(val('pm-endbit'), 0);
  if (endBit < startBit) { alert('终止位不能小于起始位'); return; }
  const accessMode = val('pm-access');
  const readFC = toNum(val('pm-readfunc'), null);
  const writeFC = toNum(val('pm-writefunc'), null);
  const registerBase = toNum(parseRegAddr(val('pm-regbase')), null);
  const registerOffset = toNum(parseRegAddr(val('pm-regoffset')), null);
  // 协议映射为必选配置；在线点（online）为虚拟属性、不映射寄存器，豁免校验。
  if (!isLocked(propEditIndex)) {
    if ((accessMode === 'r' || accessMode === 'rw') && readFC === null) { alert('协议映射必选：可读属性必须选择读功能码'); return; }
    if ((accessMode === 'w' || accessMode === 'rw') && writeFC === null) { alert('协议映射必选：可写属性必须选择写功能码'); return; }
    if (registerBase === null) { alert('协议映射必选：寄存器基址必填'); return; }
    if (registerOffset === null) { alert('协议映射必选：寄存器偏移必填（可为 0）'); return; }
  }
  // 设计约定：寄存器数量与位区间（数据长度）无强制对应关系——
  //   寄存器数量 → 读取跨度（从设备读多少个寄存器）；
  //   位区间 → 解析规则（读回数据中取多少位换算工程值，如 1 寄存器 16 位只取前 8 位）。
  // 仅要求位区间落在实际读取的数据宽度内。
  const regCount = toNum(val('pm-regcount'), null);
  if (!isLocked(propEditIndex) && regCount === null) { alert('协议映射必选：寄存器数量必填（1~125）'); return; }
  if (regCount !== null && (!Number.isInteger(regCount) || regCount < 1 || regCount > 125)) { alert('寄存器数量需为 1~125 的整数'); return; }
  const dataWidth = (regCount !== null ? regCount : Math.floor(endBit / 16) + 1) * 16; // 可读取的数据位数
  if (endBit >= dataWidth) { alert(`终止位 ${endBit} 超出可读取的数据宽度：当前配置可读 ${dataWidth} 位，请增大寄存器数量或缩小位区间`); return; }
  const p = {
    id, name: val('pm-name'), description: val('pm-desc'), dataType, unit: val('pm-unit'),
    accessMode,
    startBit, endBit,
    deltaValue: toNum(val('pm-base'), 0),
    coefficient: toNum(val('pm-coef'), 1),
    readFunctionCode: readFC,
    writeFunctionCode: writeFC,
    registerBase,
    registerOffset,
    registerCount: regCount,
    byteOrder: val('pm-byteorder')
  };
  if (propEditIndex >= 0) state.properties[propEditIndex] = p; else state.properties.push(p);
  propModal.hide();
  renderProps();
}
function deleteProp(idx) {
  if (isLocked(idx)) { alert('默认属性不可删除'); return; }
  if (!confirm('确定删除该属性？')) return;
  state.properties.splice(idx, 1);
  renderProps();
}
function renderProps() {
  const tb = document.getElementById('props-tbody');
  const empty = document.getElementById('props-empty');
  const wrap = document.getElementById('props-table-wrap');
  if (!state.properties.length) { tb.innerHTML = ''; empty.classList.remove('d-none'); wrap.classList.add('d-none'); return; }
  empty.classList.add('d-none'); wrap.classList.remove('d-none');
  tb.innerHTML = state.properties.map((p, i) => {
    const bitRange = (p.startBit != null && p.endBit != null) ? `${p.startBit}~${p.endBit}` : '—';
    return `<tr>
      <td>${i}</td>
      <td><code>${escapeHtml(p.id)}</code></td>
      <td>${escapeHtml(p.name)}</td>
      <td>${escapeHtml(dataTypeLabel(p.dataType))}</td>
      <td>${escapeHtml(String(p.deltaValue ?? 0))}</td>
      <td>${escapeHtml(String(p.coefficient ?? '1'))}</td>
      <td>${escapeHtml(p.unit || '—')}</td>
      <td><span class="badge badge-${escapeHtml(p.accessMode)}">${escapeHtml((p.accessMode || '').toUpperCase())}</span></td>
      <td>${escapeHtml(regAddrDisplay(p))}</td>
      <td>${escapeHtml(bitRange)}</td>
      <td class="text-nowrap">
        ${isLocked(i) ? '<span class="text-muted" title="默认属性不可删除"><i class="bi bi-lock"></i></span>' : `<button class="btn btn-sm btn-link p-0 me-2" onclick="openPropModal(${i})" title="编辑"><i class="bi bi-pencil"></i></button>
        <button class="btn btn-sm btn-link p-0 text-danger" onclick="deleteProp(${i})" title="删除"><i class="bi bi-trash"></i></button>`}
      </td>
    </tr>`;
  }).join('');
}

/* ─── CSV ─── */
function csvCell(v) { const s = (v === null || v === undefined) ? '' : String(v); return '"' + s.replace(/"/g, '""') + '"'; }
function exportPropsCsv() {
  if (!state.properties.length) { alert('暂无属性可导出'); return; }
  const lines = [CSV_HEADERS.map(csvCell).join(',')];
  state.properties.forEach(p => {
    lines.push([p.id, p.name, p.description, DT_LABEL[p.dataType] || p.dataType, p.startBit ?? '', p.endBit ?? '', ACCESS_LABEL[p.accessMode] || p.accessMode,
      p.deltaValue ?? 0, p.coefficient, p.unit, p.readFunctionCode ?? '', p.writeFunctionCode ?? '', p.registerBase ?? '', p.registerOffset ?? '', p.registerCount ?? '', p.byteOrder
    ].map(csvCell).join(','));
  });
  downloadBlob(new Blob(['\uFEFF' + lines.join('\r\n')], { type: 'text/csv;charset=utf-8' }), sanitize(state.profile.name) + '-properties.csv');
}
function parseCsv(text) {
  const rows = []; let row = []; let field = ''; let i = 0; let inQuotes = false;
  text = text.replace(/^\uFEFF/, '');
  while (i < text.length) {
    const c = text[i];
    if (inQuotes) {
      if (c === '"') { if (text[i + 1] === '"') { field += '"'; i += 2; continue; } inQuotes = false; i++; continue; }
      field += c; i++; continue;
    }
    if (c === '"') { inQuotes = true; i++; continue; }
    if (c === ',') { row.push(field); field = ''; i++; continue; }
    if (c === '\r') { i++; continue; }
    if (c === '\n') { row.push(field); rows.push(row); row = []; field = ''; i++; continue; }
    field += c; i++;
  }
  if (field.length > 0 || row.length > 0) { row.push(field); rows.push(row); }
  return rows;
}
function importPropsCsv(input) {
  const file = input.files[0];
  if (!file) return;
  const reader = new FileReader();
  reader.onload = e => {
    try {
      const rows = parseCsv(e.target.result).filter(r => r.some(c => c.trim() !== ''));
      if (rows.length < 2) throw new Error('CSV 内容为空或缺少数据行');
      const headers = rows[0].map(h => h.trim());
      const fieldIdx = {};
      headers.forEach((h, i) => { const f = CSV_FIELD_MAP[h] || CSV_FIELD_MAP[h.toLowerCase()]; if (f) fieldIdx[f] = i; });
      ['id', 'name', 'dataType', 'accessMode'].forEach(req => { if (!(req in fieldIdx)) throw new Error('缺少必需列：' + req); });
      const imported = [];
      const seen = new Set();
      for (let r = 1; r < rows.length; r++) {
        const row = rows[r];
        const get = f => (f in fieldIdx) ? (row[fieldIdx[f]] ?? '').trim() : '';
        const id = get('id');
        if (!id) throw new Error(`第 ${r + 1} 行：属性ID 不能为空`);
        if (!/^[A-Za-z_][A-Za-z0-9_-]*$/.test(id)) throw new Error(`第 ${r + 1} 行：属性ID 格式非法（${id}）`);
        if (seen.has(id)) throw new Error('CSV 中存在重复属性ID：' + id);
        seen.add(id);
        const dtRaw = get('dataType');
        const dt = DT_LABEL[dtRaw] ? dtRaw : (DT_FROM_LABEL[dtRaw] || dtRaw);
        if (!DT_LABEL[dt]) throw new Error(`第 ${r + 1} 行：数据类型非法（${dtRaw}）`);
        const accRaw = get('accessMode');
        const acc = ACCESS_FROM_LABEL[accRaw] || accRaw;
        if (!ACCESS_LABEL[acc]) throw new Error(`第 ${r + 1} 行：读写属性非法（${accRaw}）`);
        // 协议映射为必选配置；虚拟属性 online 豁免
        if (id !== 'online') {
          const rfc = toNum(get('readFunctionCode'), null);
          const wfc = toNum(get('writeFunctionCode'), null);
          const rb = toNum(get('registerBase'), null);
          const ro = toNum(get('registerOffset'), null);
          const rc = toNum(get('registerCount'), null);
          if ((acc === 'r' || acc === 'rw') && rfc === null) throw new Error(`第 ${r + 1} 行：读功能码必填`);
          if ((acc === 'w' || acc === 'rw') && wfc === null) throw new Error(`第 ${r + 1} 行：写功能码必填`);
          if (rb === null) throw new Error(`第 ${r + 1} 行：寄存器基址必填`);
          if (ro === null) throw new Error(`第 ${r + 1} 行：寄存器偏移必填（可为 0）`);
          if (rc === null || !Number.isInteger(rc) || rc < 1 || rc > 125) throw new Error(`第 ${r + 1} 行：寄存器数量必填（1~125）`);
        }
        imported.push({
          id, name: get('name'), description: get('description'), dataType: dt, unit: get('unit'), accessMode: acc,
          startBit: toNum(get('startBit'), 0), endBit: toNum(get('endBit'), 0),
          deltaValue: toNum(get('deltaValue'), 0), coefficient: toNum(get('coefficient'), 1),
          readFunctionCode: toNum(get('readFunctionCode'), null), writeFunctionCode: toNum(get('writeFunctionCode'), null),
          registerBase: toNum(get('registerBase'), null), registerOffset: toNum(get('registerOffset'), null), registerCount: toNum(get('registerCount'), null),
          byteOrder: get('byteOrder')
        });
      }
      if (state.properties.length && !confirm(`将导入 ${imported.length} 条属性并覆盖当前 ${state.properties.length} 条，是否继续？`)) { input.value = ''; return; }
      state.properties = imported;
      renderProps();
      alert(`成功导入 ${imported.length} 条属性`);
    } catch (err) {
      alert('CSV 导入失败：' + err.message);
    }
    input.value = '';
  };
  reader.readAsText(file);
}

/* ══════════════ Shared helpers ══════════════ */
function isLocked(idx) { return idx === 0; }

/* ══════════════ Step 3 · Preview & Export ══════════════ */
function buildCollectorConfig() {
  syncProfileFromForm();
  return {
    profile: {
      profileIndex: state.profile.profileIndex,
      profileId: state.profile.profileId || '',
      name: state.profile.name,
      manufacturer: state.profile.manufacturer || '',
      description: state.profile.description || '',
      deviceType: state.profile.deviceType || '',
      deviceModel: state.profile.deviceModel || '',
      ratedPower: state.profile.ratedPower,
      interfaceType: state.profile.interfaceType,
      protocolType: state.profile.protocolType,
      protocolVersion: state.profile.protocolVersion || '',
      maxRegisterCount: state.profile.maxRegisterCount
    },
    properties: state.properties.map((p, i) => ({
      index: i,
      id: p.id,
      name: p.name,
      description: p.description || '',
      dataType: p.dataType,
      unit: p.unit || '',
      accessMode: p.accessMode,
      startBit: p.startBit ?? 0,
      endBit: p.endBit ?? 0,
      deltaValue: p.deltaValue ?? 0,
      coefficient: p.coefficient,
      readFunctionCode: p.readFunctionCode,
      writeFunctionCode: p.writeFunctionCode,
      registerBase: p.registerBase,
      registerOffset: p.registerOffset,
      registerCount: p.registerCount ?? null,
      byteOrder: p.byteOrder || ''
    }))
  };
}
function renderPreview() {
  document.getElementById('pre-collector').textContent = JSON.stringify(buildCollectorConfig(), null, 2);
  document.getElementById('sum-name').textContent = state.profile.name || '—';
  document.getElementById('sum-props').textContent = state.properties.length;
}
function saveDeviceModel() {
  if (!validateProfile()) { goStep(1); return; }
  syncProfileFromForm();
  saveBufferIntoModel();
  const m = state.models.find(x => x.id === state.editingId);
  if (!m) { alert('未找到当前设备模型'); return; }
  apiPost('/models', modelToPayload(m))
    .then(() => { alert('设备模型已保存到数据库'); backToList(); })
    .catch(e => alert('保存失败：' + e.message));
}
function exportDeviceModel() {
  if (!validateProfile()) { goStep(1); return; }
  downloadJson(buildCollectorConfig(), sanitize(state.profile.name) + '.json');
}
