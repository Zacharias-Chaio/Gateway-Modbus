/* ══════════════ 实时数据 · Realtime ══════════════ */

// rtValues 缓存从引擎获取的实时值，key = 属性 ID（属性名允许重复，不能作键）
let rtValues = {};
let rtTimer = null;
let rtLoading = false;

// channelDevices 返回链路上的设备列表（附带模型信息）。
// 使用设备在链路中的序号（index）作为标识，与引擎 worker 的设备序号一致。
function channelDevices(ch) {
  if (!ch) return [];
  return (ch.devices || []).map((d, i) => {
    const m = state.models.find(x => String(x.id) === String(d.modelId));
    return m ? { index: i, commNo: d.commNo, name: d.name || '', model: m } : null;
  }).filter(Boolean);
}

// rtSelection 返回当前选中的 { channelIndex, deviceIndex } 或 null。
// 通道索引从 0 开始（0 是有效值），不能用真值判断是否存在。
function rtSelection() {
  const ch = document.getElementById('rt-channel');
  const dev = document.getElementById('rt-device');
  if (!ch || !dev || ch.value === '') return null;
  const channelIndex = toNum(ch.value, -1);
  const deviceIndex = toNum(dev.value, 0);
  if (channelIndex < 0) return null;
  return { channelIndex, deviceIndex };
}

// channelDeviceModel 根据下拉选择查找设备模型。
function channelDeviceModel() {
  const ch = state.channels.find(c => String(c.channelIndex) === document.getElementById('rt-channel').value);
  if (!ch) return null;
  const devIdx = toNum(document.getElementById('rt-device').value, -1);
  const d = (ch.devices || [])[devIdx];
  return d ? state.models.find(m => String(m.id) === String(d.modelId)) : null;
}

function fillSelect(sel, items, getVal, getLabel, keepVal) {
  const prev = keepVal ? sel.value : '';
  sel.innerHTML = items.length ? items.map(it => `<option value="${escapeHtml(getVal(it))}">${escapeHtml(getLabel(it))}</option>`).join('')
    : '<option value="">无可用项</option>';
  if (prev && items.some(it => String(getVal(it)) === prev)) sel.value = prev;
}

// ── 自动刷新控制 ──
function rtStartPolling() {
  rtStopPolling();
  rtTimer = setInterval(() => {
    // 页面不可见时暂停轮询，避免 N 个后台标签线性放大后端压力
    if (document.hidden) return;
    fetchRtValues();
  }, 2000);
}
function rtStopPolling() {
  if (rtTimer) { clearInterval(rtTimer); rtTimer = null; }
}
// 页面重新可见时立即刷新一次，避免数据看起来「停住了」
document.addEventListener('visibilitychange', () => {
  if (!document.hidden && rtTimer) fetchRtValues();
});

// renderRealtime 由 switchSection('realtime') 触发：填充下拉、启动轮询。
// 通道以下拉顺序（通道索引）定位，标签展示自动生成的通道ID（Channel-{索引}）。
function renderRealtime() {
  fillSelect(document.getElementById('rt-channel'), state.channels, c => c.channelIndex, c => `${c.id || channelIdFromIndex(c.channelIndex)} · ${c.name}`, true);
  onRtChannelChange();
  rtStartPolling();
}

function onRtChannelChange() {
  const ch = state.channels.find(c => String(c.channelIndex) === document.getElementById('rt-channel').value);
  fillSelect(document.getElementById('rt-device'), channelDevices(ch),
    d => d.index, d => `#${d.commNo} · ${d.name || (d.model.profile && d.model.profile.name) || '未命名设备'}`, true);
  rtValues = {};
  renderRtTable();
  fetchRtValues();
}

// fetchRtValues 从后端引擎获取实时缓存值并更新表格。
async function fetchRtValues() {
  const sel = rtSelection();
  if (!sel) return;
  const tb = document.getElementById('rt-tbody');
  if (!tb || rtLoading) return;
  rtLoading = true;
  try {
    const data = await apiGet('/realtime?device=' + encodeURIComponent(sel.channelIndex + '/' + sel.deviceIndex));
    rtValues = (data && data.values) || {};
    // 逐属性更新 DOM
    const m = channelDeviceModel();
    if (m) {
      for (const p of (m.properties || [])) {
        const cell = document.getElementById('rtval-' + p.id);
        if (!cell) continue;
        // 实时值以属性 ID 为键（属性名允许重复，不能作为键）
        const entry = rtValues[p.id];
        const hasVal = entry !== undefined && entry !== null && entry.value !== undefined && entry.value !== null;
        cell.textContent = hasVal ? formatRtValue(entry.value, p.dataType) : '—';
        cell.classList.toggle('text-muted', !hasVal);
        // 更新该属性的采集时间
        const tsCell = document.getElementById('rtts-' + p.id);
        if (tsCell) {
          tsCell.textContent = hasVal ? new Date(entry.ts).toLocaleTimeString() : '—';
          tsCell.classList.toggle('text-muted', !hasVal);
        }
      }
    }
    // 更新状态提示：区分「链路重启中」「链路未运行」「真的没有数据」
    const hint = document.getElementById('rt-hint');
    if (hint) {
      const hasData = Object.keys(rtValues).length > 0;
      const note = data && data.note;
      if (hasData) {
        hint.textContent = '采集中 · 最后更新 ' + new Date((data && data.timestamp || 0) * 1000).toLocaleTimeString();
        hint.classList.remove('text-warning');
      } else if (note) {
        hint.textContent = note;
        hint.classList.add('text-warning');
      } else {
        hint.textContent = '无采集数据（设备未响应或属性未映射成功，可查看通讯监控）';
        hint.classList.add('text-warning');
      }
    }
  } catch (e) {
    const hint = document.getElementById('rt-hint');
    if (hint) {
      hint.textContent = '无法获取实时数据：' + e.message + (e.message.indexOf('HTTP') >= 0 ? '（引擎可能正在重启）' : '');
      hint.classList.add('text-warning');
    }
  } finally {
    rtLoading = false;
  }
}

// formatRtValue 按数据类型格式化显示。
function formatRtValue(v, dataType) {
  if (typeof v === 'number') {
    if (dataType === 'bool') return String(v);
    if (dataType === 'float') return v.toFixed(2);
    return String(v);
  }
  return String(v);
}

// renderRtTable 渲染属性表格骨架（值列先占位 —，由 fetchRtValues 填充）。
function renderRtTable() {
  const m = channelDeviceModel();
  const empty = document.getElementById('rt-empty'), wrap = document.getElementById('rt-wrap'), tb = document.getElementById('rt-tbody');
  if (!m || !m.properties.length) {
    wrap.classList.add('d-none'); empty.classList.remove('d-none');
    empty.querySelector('div').textContent = m ? '该设备暂无属性' : '请选择通道与设备以查看实时属性数值。';
    return;
  }
  empty.classList.add('d-none'); wrap.classList.remove('d-none');
  tb.innerHTML = m.properties.map((p, i) => {
    const writable = p.accessMode === 'w' || p.accessMode === 'rw';
    const setCell = writable
      ? `<div class="d-flex gap-1">
           <input type="text" class="form-control form-control-sm rt-set-input" id="set-${escapeHtml(p.id)}" placeholder="设定值">
           <button class="btn btn-sm btn-primary text-nowrap" onclick="setRtValue('${escapeHtml(p.id)}')"><i class="bi bi-upload"></i> 下发</button>
         </div>`
      : '<span class="text-muted">只读</span>';
    return `<tr>
      <td class="text-muted">${i}</td><td><code>${escapeHtml(p.id)}</code></td><td>${escapeHtml(p.name)}</td>
      <td class="fw-semibold text-muted" id="rtval-${escapeHtml(p.id)}">—</td>
      <td>${escapeHtml(p.unit || '—')}</td>
      <td><span class="badge badge-${escapeHtml(p.accessMode)}">${escapeHtml((p.accessMode||'').toUpperCase())}</span></td>
      <td class="text-nowrap">${setCell}</td>
      <td class="text-muted text-nowrap" id="rtts-${escapeHtml(p.id)}">—</td>
    </tr>`;
  }).join('');
}

// setRtValue 通过引擎下发写命令到后端。
async function setRtValue(propId) {
  const sel = rtSelection();
  if (!sel) { alert('请先选择通道与设备'); return; }
  const m = channelDeviceModel();
  if (!m) return;
  const p = (m.properties || []).find(x => x.id === propId);
  if (!p) return;
  const input = document.getElementById('set-' + propId);
  const v = input ? input.value.trim() : '';
  if (v === '') { alert('请输入设定值'); return; }
  const numVal = Number(v);
  if (isNaN(numVal)) { alert('设定值必须是数字'); return; }
  input.disabled = true;
  try {
    // waitMs=5000：同步等待写命令在链路上的最终执行结果（含设备异常码）。
    // 之前这里拿不到任何回执，只能靠「值有没有变化」反推。
    // 用属性 ID 下发（属性名可重复，按名字会打到错误的属性上）
    const res = await apiPost('/set', {
      channelIndex: sel.channelIndex,
      deviceIndex: sel.deviceIndex,
      propId: p.id,
      propName: p.name,
      value: numVal,
      waitMs: 5000
    });
    const status = res && res.status;
    if (status === 'ok') {
      alert(`下发成功：${p.name} = ${v}` + (res.applied > 1 ? `（分 ${res.applied} 帧写入）` : ''));
      input.value = '';
    } else {
      alert(`已下发，等待结果超时：${p.name} = ${v}\n命令仍在链路队列中执行，请查看通讯监控确认结果。`);
    }
    setTimeout(fetchRtValues, 300);
  } catch (e) {
    alert('下发失败：' + e.message);
  } finally {
    input.disabled = false;
  }
}

