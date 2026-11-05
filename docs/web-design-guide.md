# Web 前端设计风格指南

本文档从 `internal/web/static/` 现有实现中提炼，作为其他模块（新增页面、独立子模块、二次开发）统一遵循的设计参考，确保视觉与交互一致性。

参考实现：
- 页面结构：[index.html](file:///f:/Code/Gateway/internal/web/static/index.html)
- 样式定义：[app.css](file:///f:/Code/Gateway/internal/web/static/css/app.css)
- 常量与状态：[constants.js](file:///f:/Code/Gateway/internal/web/static/js/constants.js)

---

## 一、技术栈

| 依赖 | 版本 | 用途 |
|------|------|------|
| Bootstrap | 5.3.2 | 栅格、表单、Modal、按钮基础样式 |
| Bootstrap Icons | 1.11.3 | 全量图标（`bi bi-*`） |
| 原生 CSS 变量 | — | 主题色、阴影、圆角统一管理 |
| 原生 JavaScript | ES6+ | 无构建工具，无框架依赖 |

**约束**：不引入额外的 UI 框架或构建工具，所有自定义样式统一收敛到 `app.css`，所有自定义脚本收敛到 `static/js/` 下分模块组织。

---

## 二、设计令牌（Design Tokens）

所有视觉变量统一定义在 `:root`，禁止硬编码颜色/阴影/圆角值。

### 2.1 颜色

```css
:root {
  --primary:        #4f46e5;  /* 主品牌色（Indigo 600），用于强调、激活、主按钮 */
  --primary-light:  #eef2ff;  /* 主色浅底（Indigo 50），用于 hover、icon 背板、info banner */
  --primary-hover:  #4338ca;  /* 主色 hover 加深 */
  --success:        #059669;  /* 成功（Emerald 600），用于完成态、LED 在线 */
  --warning:        #d97706;  /* 警告（Amber 600） */
  --danger:         #dc2626;  /* 危险（Red 600），必填星标、错误提示 */
  --muted-color:     #6b7280;  /* 次要文本（Gray 500） */
  --border-color:   #e5e7eb;  /* 分隔线/边框（Gray 200） */
  --bg:             #f8fafc;  /* 全局背景（Slate 50） */
  --card-shadow: 0 4px 24px rgba(0,0,0,.06);  /* 卡片通用阴影 */
}
```

### 2.2 文本颜色梯度

| 用途 | 颜色 | 来源 |
|------|------|------|
| 标题主文本 | `#111827` | 灰阶 900 |
| 正文 | `#1f2937` | 灰阶 800（`body`） |
| 次级/标签 | `#374151` | 灰阶 700 |
| 辅助/占位 | `var(--muted-color)` `#6b7280` | 灰阶 500 |
| 禁用/极弱 | `#9ca3af` | 灰阶 400 |

### 2.3 字体

```css
font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, 'Microsoft YaHei', sans-serif;
```

- 等宽字体（代码/日志）：`'Consolas', 'Monaco', 'Courier New', monospace`
- 中文优先级：Microsoft YaHei 兜底，标题不使用衬线字体

### 2.4 圆角

| 元素 | 圆角 |
|------|------|
| 卡片/面板/Modal | `14–16px` |
| 圆形图标背板/品牌图标 | `10–12px` |
| 表单输入/按钮 | 跟随 Bootstrap 默认（`0.375rem`） |
| 标签/Pill/Badge | `20px`（全圆角） |
| 圆形元素（step-circle、nav-arrow、品牌 icon 大圆） | `50%` / `12px` |

### 2.5 阴影

- 卡片/面板：`var(--card-shadow)` = `0 4px 24px rgba(0,0,0,.06)`
- 激活态额外光晕：`0 0 0 4px rgba(79,70,229,.15)`（主色 15% 透明度外环）
- 浮起按钮：`0 2px 8px rgba(0,0,0,.05)`
- Hover 抬升：`0 8px 24px rgba(0,0,0,.08)` + `translateY(-2px)`

---

## 三、布局结构

### 3.1 总体骨架：Sidebar + Main

```html
<div class="app-shell">
  <aside class="sidebar"> … </aside>
  <main class="app-main">
    <div class="main-inner"> … </div>
  </main>
</div>
```

- `.app-shell`：`display:flex; min-height:100vh`
- `.sidebar`：固定宽度 **232px**，`position:sticky; top:0; height:100vh`，白底右分隔线
- `.app-main`：`flex:1; min-width:0; padding:26px 30px 48px`
- `.main-inner`：`max-width:1080px; margin:0 auto`（内容居中，宽度统一）

### 3.2 侧边栏结构

```
┌─ sidebar-brand ─────────────┐
│ [icon]  品牌主标题           │
│         品牌副标题（英文）    │
├─────────────────────────────┤
│ sidebar-section-title       │  ← 11px 大写英文，灰色分隔标题
│ ▸ sidebar-item (active)     │
│ ▸ sidebar-item              │  ← 11px padding, 10px radius
│ ▸ sidebar-item              │
├─────────────────────────────┤
│ sidebar-footer (版本信息)    │  ← margin-top:auto 推到底部
└─────────────────────────────┘
```

**关键样式**：
- 普通项：`color:#475569; padding:11px 14px; border-radius:10px`
- Hover：背景变 `--primary-light`，文本变 `--primary`
- Active：背景 `--primary`，白字，`box-shadow:0 4px 12px rgba(79,70,229,.25)`
- 选中项右侧可携带 `item-badge`（`background:rgba(255,255,255,.2); color:#fff`）

### 3.3 区块切换约定

每个一级区块为 `<section class="app-section" id="section-{key}">`，默认全部 `d-none`，通过 `switchSection(key)` 切换显隐并同步高亮 `nav-{key}`。新增模块遵循同样的 `section-xxx` / `nav-xxx` 命名。

---

## 四、通用组件模式

### 4.1 区块头部（Section Header）

每个区块统一头部：标题（带主色图标）+ 副描述。

```html
<div class="section-header">
  <h4><i class="bi bi-cpu me-2" style="color:var(--primary)"></i>设备模型</h4>
  <div class="sub">管理南向采集设备模型，配置设备档案与属性...</div>
</div>
```

- `h4`：`font-weight:700; color:#111827`
- `.sub`：`color:var(--muted-color); font-size:13.5px`
- 标题图标统一使用主色 `color:var(--primary)`

### 4.2 工具条（Landing Toolbar）

列表页顶部右上方操作区：

```html
<div class="landing-toolbar">
  <span class="count-pill">已配置 <span class="fw-semibold">N</span> 个...</span>
  <button class="btn btn-primary"><i class="bi bi-plus-lg me-1"></i>新建...</button>
</div>
```

- 工具条 `display:flex; justify-content:space-between; flex-wrap:wrap; gap:12px`
- 数量胶囊 `count-pill`：`border-radius:20px; padding:4px 12px; font-size:12.5px`，白底+灰边框
- 主操作按钮始终带 `bi-plus-lg` 前置图标

### 4.3 卡片（Model Card）

列表项卡片统一形态：

```html
<div class="model-card">
  <div class="model-card-top">
    <div class="model-card-icon"><i class="bi bi-cpu"></i></div>
    <div class="min-w-0">
      <div class="model-card-title">标题</div>
      <div class="model-card-sub">副标题</div>
    </div>
  </div>
  <div class="model-card-tags">
    <span class="mc-tag iface">串口</span>  <!-- 主色调标签 -->
    <span class="mc-tag">Modbus RTU</span>   <!-- 普通灰标签 -->
  </div>
  <div class="model-card-desc">描述文本</div>
  <div class="model-card-stats">
    <div class="mc-stat"><div class="num">12</div><div class="lbl">属性</div></div>
  </div>
  <div class="model-card-actions">...</div>
</div>
```

**关键样式**：
- 卡片：白底 + `1px solid var(--border-color)` + `border-radius:14px; padding:18px`
- Hover：阴影加深 + `translateY(-2px)` + 边框变 `#c7d2fe`（主色浅紫）
- 图标背板：`44×44px`，`border-radius:11px`，背景 `--primary-light`，图标主色
- 标签 `.mc-tag`：`border-radius:20px; padding:3px 9px; background:#f3f4f6`
- 主色调标签 `.mc-tag.iface`：`background:var(--primary-light); color:var(--primary)`
- 统计块 `.mc-stat`：浅灰底 + 灰边框 + 圆角，数字主色加粗 20px

### 4.4 步进器（Stepper）

向导流程统一使用三步圆形步进器：

```html
<div class="stepper" id="stepper">
  <div class="step-item active">
    <div class="step-circle">1</div>
    <div class="step-label">档案配置</div>
  </div>
  ...
</div>
```

- 圆圈 `42×42px`，`border-radius:50%; border:2px solid var(--border-color)`
- 激活态：`border-color/background:var(--primary)` + `box-shadow:0 0 0 4px rgba(79,70,229,.15)`
- 完成态：`border-color/background:var(--success)` 白字
- 连接线：`height:2px; background:var(--border-color)` 横贯步骤之间
- 标签：`12.5px`，激活态主色加粗，完成态 success 色

### 4.5 向导卡片（Wizard Card）

```html
<div class="wizard-wrapper">
  <button class="nav-arrow" id="btn-prev">&#8249;</button>
  <div class="wizard-card">
    <div class="step-heading">
      <div class="step-icon"><i class="bi bi-card-text"></i></div>
      <div class="flex-grow-1">
        <h5 class="fw-bold mb-0">步骤标题</h5>
        <div class="text-muted small">副描述</div>
      </div>
      <div class="d-flex gap-2">...操作按钮</div>
    </div>
    <!-- 步骤内容 -->
  </div>
  <button class="nav-arrow" id="btn-next">&#8250;</button>
</div>
<div class="step-counter">第 N 步 / 共 3 步</div>
```

- 卡片：`border-radius:16px; padding:32px 36px 26px; min-height:480px`
- 步骤头部 `step-icon`：`48×48px` 圆角 12px，浅紫底 + 主色图标
- 左右翻页按钮 `nav-arrow`：`44×44px` 圆形，hover 时变主色描边与浅紫底
- 步骤计数器：底部居中，`13px` 灰色文本

### 4.6 信息横幅（Info Banner）

步骤内提示性信息条：

```html
<div class="info-banner">
  <i class="bi bi-info-circle-fill me-2" style="color:var(--primary)"></i>
  <span class="text-muted">说明文本，可含 <span class="fw-semibold">加粗关键词</span>。</span>
</div>
```

- 背景 `#f0f4ff`，边框 `1px solid #c7d2fe`，圆角 `10px`，内边距 `12px 16px`
- 字号 `13.5px`，主色图标 + 灰色文本，关键词加粗

### 4.7 表单分区（Form Section Divider）

```html
<div class="form-section-divider"><span><i class="bi bi-folder2-open me-1"></i>档案信息</span></div>
```

- 文字 `12.5px; font-weight:600; color:var(--muted-color)`
- `::after` 自适应填充一条 `1px` 灰色横线

### 4.8 表单字段约定

```html
<div class="col-md-6">
  <label class="form-label fw-semibold">字段名 <span class="text-danger">*</span></label>
  <input type="text" class="form-control" id="xx-yyy" required placeholder="提示文本">
  <div class="invalid-feedback">校验失败提示</div>
</div>
```

- `label` 统一 `fw-semibold`，必填项追加 `*` 红色星标
- `id` 命名：`{区块前缀}-{字段名}`（如 `pf-name`、`ch-deviceIp`、`pm-dataType`）
- 校验失败使用 Bootstrap `invalid-feedback` + `was-validated`/`is-invalid` 类
- 自动生成/只读字段加 `readonly`，`placeholder="自动生成"`
- 输入框单位用 `input-group` + `input-group-text`（如 `kW`）

### 4.9 数据面板（Data Panel）

实时数据/日志监控区块统一容器：

```html
<div class="data-panel">
  <div class="filter-bar">
    <div class="fb-item">
      <label>通道</label>
      <select class="form-select">...</select>
    </div>
    <button class="btn btn-outline-primary"><i class="bi bi-arrow-clockwise me-1"></i>刷新</button>
    <span class="ms-auto text-muted small">辅助提示</span>
  </div>
</div>
```

- `.data-panel`：白底 + `border-radius:14px` + `var(--card-shadow)` + `padding:18px 20px`
- `.filter-bar`：`display:flex; flex-wrap:wrap; gap:14px; align-items:flex-end`
- `.fb-item`：`min-width:200px`，`label` 12.5px 加粗灰色

### 4.10 数据表格

```html
<div class="table-responsive">
  <table class="table table-hover align-middle">
    <thead><tr><th>...</th></tr></thead>
    <tbody id="xxx-tbody"></tbody>
  </table>
</div>
```

- 表头 `12.5px; font-weight:600; color:#374151; white-space:nowrap`
- 单元格 `13px`
- Hover 行底色 `#f8f9ff`（极浅紫）
- `code` 元素：主色 `12.5px`
- 操作列固定宽度并右对齐

### 4.11 徽标（Badge）

读写属性/状态标识使用语义化浅底深字配色：

| 类 | 背景 | 字色 | 用途 |
|----|------|------|------|
| `.badge-r`  | `#fef3c7` | `#92400e` | 只读 R（琥珀） |
| `.badge-w`  | `#d1fae5` | `#065f46` | 只写 W（翠绿） |
| `.badge-rw` | `#dbeafe` | `#1e40af` | 读写 RW（浅蓝） |
| `.mc-tag`  | `#f3f4f6` | `#4b5563` | 中性标签 |
| `.mc-tag.iface` | `var(--primary-light)` | `var(--primary)` | 主色调标签 |

### 4.12 代码/日志预览

```html
<pre class="code-preview">...</pre>      <!-- JSON 配置预览 -->
<pre class="log-console">...</pre>          <!-- 通讯日志 -->
```

通用样式：
- 背景 `#1e1e2e`（Catppuccin Mocha 深底）
- 字色 `#cdd6f4`
- 字体：Consolas / Monaco / Courier New，`12.5px`，`line-height:1.7`
- 圆角 `10–12px`，内边距 `16–20px`

日志着色规则（用于通讯报文）：
- `.log-rx`：`#89dceb`（青色，接收）
- `.log-tx`：`#a6e3a1`（绿色，发送）
- `.log-ts`：`#6c7086`（灰色，时间戳前缀）

### 4.13 摘要卡（Summary Card）

预览步骤的指标卡：

```html
<div class="summary-card">
  <div class="val">12</div>
  <div class="lbl">属性数量</div>
</div>
```

- 边框 + `border-radius:12px; padding:18px 20px`
- `.val`：`26px; font-weight:700; color:var(--primary)`
- `.lbl`：`12.5px; color:var(--muted-color)`

### 4.14 空状态（Empty State）

```html
<div class="empty-state">
  <i class="bi bi-inboxes"></i>
  <div>暂无数据，点击右上角「新建 XXX」开始配置。</div>
</div>
```

- `text-align:center; padding:44px 20px; color:var(--muted-color)`
- 图标 `38px`，`opacity:.4`，下方 `12px` 间距

### 4.15 误码率指标盒

```html
<div class="err-box">
  <div class="err-val" id="log-err">0.00%</div>
  <div class="err-lbl">误码率</div>
</div>
```

- 背景 `var(--primary-light)` + 主色描边 `1px solid #c7d2fe` + `border-radius:12px`
- `.err-val`：`24px` 加粗主色
- `.err-lbl`：`11.5px` 灰色

### 4.16 在线指示灯（LED）

```html
<span class="led led-on"></span>
```

- `9×9px` 圆点，背景 `var(--success)`，`box-shadow:0 0 6px var(--success)` 发光

### 4.17 Modal

属性编辑统一使用 Bootstrap Modal（`modal-xl modal-dialog-scrollable`），遵循 Bootstrap 头/体/脚结构，主按钮主色 + `bi-check-lg` 图标，取消按钮 `btn-light`。

---

## 五、按钮规范

| 场景 | 类 | 图标约定 |
|------|----|---------|
| 主操作（新建/保存/确认） | `btn btn-primary` | 前置 `bi-plus-lg` / `bi-check-lg` / `bi-database-check` |
| 成功保存 | `btn btn-success` | `bi-database-check` |
| 次要操作（导入/导出/刷新） | `btn btn-outline-secondary` 或 `btn btn-outline-primary` | `bi-box-arrow-in-down` / `bi-download` / `bi-arrow-clockwise` |
| 返回 | `btn btn-outline-secondary btn-sm` | `bi-arrow-left` |
| 模态框取消 | `btn btn-light` | — |

按钮内文字与图标之间统一 `me-1` 间距；纯小尺寸按钮统一 `btn-sm`。

---

## 六、图标规范

- 全部使用 **Bootstrap Icons**（`bi bi-*`）
- 标题图标：`me-2` + 主色 `color:var(--primary)`
- 按钮前置图标：`me-1`
- 分区/小标题图标：`me-1`，跟随分区标题
- 业务模块固定图标映射（见 `constants.js` 的 `CHANNEL_TYPE_ICON`）：
  - Serial → `bi-usb-symbol`
  - Network → `bi-ethernet`
- 一级导航图标：`bi-cpu`（设备）/`bi-diagram-3`（链路）/`bi-activity`（实时）/`bi-journal-text`（报文信息）
- 网关设置导航图标：`bi-gear`
- 品牌图标：`bi-hdd-network-fill`

---

## 七、响应式

```css
@media (max-width: 880px) {
  .sidebar { width: 64px; }              /* 侧栏收成图标条 */
  .sidebar-brand .brand-text,
  .sidebar-brand .brand-sub,
  .sidebar-item span,
  .sidebar-item .item-badge,
  .sidebar-section-title,
  .sidebar-footer { display: none; }     /* 隐藏文字，仅留图标 */
  .sidebar-item { justify-content: center; padding: 12px; }
  .step-label { display: none; }          /* 步进器隐藏文字 */
  .app-main { padding: 18px 14px 36px; }
  .wizard-card { padding: 20px 16px; }
  .nav-arrow { width: 34px; min-width: 34px; height: 34px; font-size: 22px; }
}
```

新增模块在窄屏下应遵循同样的折叠策略：侧栏仅留图标、卡片内边距收窄、隐藏次要标签。

---

## 八、接入配置提醒（Provisioning Overlay）

```html
<div id="setup-overlay" style="position:fixed;inset:0;background:var(--bg);z-index:2000;
     display:flex;align-items:center;justify-content:center;">
  <div style="position:relative;background:#fff;border-radius:16px;box-shadow:var(--card-shadow);
       padding:36px 40px;width:420px;">
    <!-- 右上角绝对定位的「跳过」按钮（btn btn-sm btn-link text-muted，position:absolute;top:14px;right:16px） -->
    <div class="text-center mb-4">
      <div class="brand-icon" style="width:48px;height:48px;border-radius:12px;
           background:var(--primary);color:#fff;display:inline-flex;
           align-items:center;justify-content:center;font-size:24px;">
        <i class="bi bi-hdd-network-fill"></i>
      </div>
      <h5 class="fw-bold mt-3 mb-0">网关接入配置</h5>
      <div class="text-muted small">建议填写网关名与接入 Token；也可以先跳过，稍后在网关设置中配置</div>
    </div>
    <!-- 网关名输入 + 说明 / Token 输入 / 错误提示 / 保存并进入（w-100 整行） -->
  </div>
</div>
```

- 全屏覆盖，背景使用 `var(--bg)`，`z-index:2000`
- 卡片宽 **420px**（比常规卡片宽，因含字段说明），圆角 16px，使用 `var(--card-shadow)`
- 品牌图标采用主色实心方块 + 白字图标
- 错误提示 `text-danger small` + `d-none` 切换；字段错误就地提示，不用 `alert`
- 网关名输入框 `placeholder="gw_0001"`，下方说明「须以 `gw_` 开头」
- Token 输入框 `type="password"`，不回显
- 「跳过」按钮绝对定位于卡片右上角（`position:absolute;top:14px;right:16px`，`btn btn-sm btn-link text-muted` 低调样式）；主按钮「保存并进入」`w-100` 整行占满。跳过仅收起本次遮罩，软件正常可用
- 提交成功后清空 Token 输入框再 `window.location.reload()`，避免凭据留在 DOM 中
- 遮罩显隐以 `GET /api/provisioning` 的 `configured` 为准，**不用本地标记**：
  跳过不落任何状态，未配置期间每次打开页面都会再次提醒；完成配置后不再弹出
- 业务接口**不做任何拦截**：配置是提醒，不是门

### 重新配置弹窗

设置页「接入配置」卡片只读展示网关名与 Token 配置状态，「重新配置」打开独立的
居中 Modal（`#reprovisionModal`，`modal-dialog-centered`），表单与首屏遮罩一致
（网关名 + Token + 就地错误提示 + 取消/保存）。不复用首屏遮罩：用户正在设置页操作，
全屏遮罩会打断上下文。保存成功后关闭弹窗、局部刷新设置页，不整页重载。

### 这不是登录

网关**没有**用户登录体系，也**没有**接口鉴权：没有用户表、会话、角色，也不存在凭据校验。
本节描述的是一个**可跳过的提醒**——网关名与 Token 是否已写入数据库：

- 不承担任何访问控制职责，**不得**被视为安全边界；
- 不存在「登出」；
- 访问控制边界是网络本身（见 README「部署前提：仅限受控网络」）。

设置页「接入配置」卡片只读展示网关名与 Token 配置状态，提供「重新配置」按钮重填
（Token 不回显，更换凭据必须重填，不能只读展示后编辑）。


---

## 九、新增模块清单（Checklist）

新增页面/子模块时，请按以下清单核对一致性：

- [ ] 主题色、阴影、圆角全部引用 `:root` 变量，不硬编码
- [ ] 一级区块包裹为 `<section class="app-section" id="section-xxx">`，并在侧栏新增对应 `nav-xxx`
- [ ] 区块头部使用 `.section-header` + 主色图标 + `.sub` 副描述
- [ ] 列表页采用 `.model-card` 形态，统一 Hover 抬升与主色调边框
- [ ] 表单字段使用 `fw-semibold` 标签 + 必填红星 + Bootstrap `invalid-feedback`
- [ ] 多步流程采用 `.stepper` + `.wizard-card` + `.nav-arrow`
- [ ] 提示性信息条使用 `.info-banner`（主色浅底）
- [ ] 表格统一 `table table-hover align-middle` + `table-responsive` 包裹
- [ ] 状态/读写标识使用 `.badge-r/.badge-w/.badge-rw` 等语义化浅底深字
- [ ] 代码/日志预览使用 `.code-preview` / `.log-console` 深色面板
- [ ] 图标全部使用 Bootstrap Icons，按钮前置图标加 `me-1`
- [ ] 窄屏（≤880px）下隐藏侧栏文字，卡片内边距收窄
- [ ] JS 状态收敛到 `state` 对象（见 `constants.js`），新增常量同样定义在此文件或同目录新文件
