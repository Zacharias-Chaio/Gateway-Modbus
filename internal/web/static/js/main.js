/* ══════════════ 首次配置（网关名 + Token） ══════════════ */
// 接入配置可以跳过：它是提醒，不是门。跳过后软件完全可用，
// 但每次打开页面都会再次弹出，直到完成配置。
// 网关名会回显，Token 不会——后端任何响应都不回传 Token。

function showSetupOverlay(message) {
  const overlay = document.getElementById('setup-overlay');
  if (!overlay) return;
  overlay.style.display = 'flex';
  const err = document.getElementById('setup-err');
  if (err) {
    err.textContent = message || '';
    err.classList.toggle('d-none', !message);
  }
}

function hideSetupOverlay() {
  const overlay = document.getElementById('setup-overlay');
  if (overlay) overlay.style.display = 'none';
}

function setSetupError(message) {
  const err = document.getElementById('setup-err');
  if (!err) return;
  err.textContent = message || '';
  err.classList.toggle('d-none', !message);
}

// skipSetup 跳过接入配置：仅收起本次遮罩，软件正常使用；
// 状态仍以服务端为准，下次打开页面会再次提醒。
function skipSetup() {
  hideSetupOverlay();
  init();
}

// submitSetup 保存接入配置，成功后重新载入页面。
async function submitSetup() {
  const gwId = val('setup-gwid');
  const token = document.getElementById('setup-token').value.trim();
  const button = document.getElementById('setup-submit');

  if (!gwId) { setSetupError('请填写网关名'); document.getElementById('setup-gwid').focus(); return; }
  if (!token) { setSetupError('请填写 Token'); document.getElementById('setup-token').focus(); return; }

  button.disabled = true;
  try {
    await apiPost('/provisioning', { gwId: gwId, token: token });
    document.getElementById('setup-token').value = '';
    window.location.reload();
  } catch (error) {
    setSetupError(error.message || '保存失败');
    button.disabled = false;
  }
}

// refreshSetupOverlay 读取后端状态：未完成配置则显示遮罩提醒。
async function refreshSetupOverlay() {
  try {
    const info = await apiGet('/provisioning');
    state.provisioning = info;
    if (info && info.configured) {
      hideSetupOverlay();
      return true;
    }
    document.getElementById('setup-gwid').value = (info && info.gwId) || '';
    showSetupOverlay('');
    return false;
  } catch (error) {
    showSetupOverlay('无法读取网关接入配置：' + (error.message || '未知错误'));
    return false;
  }
}

function init() {
  propModal = new bootstrap.Modal(document.getElementById('propModal'));
  Promise.all([loadModels(), loadSettings(), loadSystemInfo()]).then(() => loadChannels()).then(() => {
    switchSection('device');
    showLanding();
    renderModelList();
    renderChannelList();
  });
}

document.addEventListener('DOMContentLoaded', async () => {
  const ready = await refreshSetupOverlay();
  if (ready) init();
});
