# 模块设计参考

> 本文档面向 **新增模块的设计与开发**，从现有 `Gateway` 项目中提炼可复用的设计原则、协作契约与代码骨架。新增任何业务模块（如告警规则、用户管理、协议扩展等）时，应以此作为设计基线，避免重复决策与不一致实现。

## 文档定位

| 文档 | 关注点 |
|------|--------|
| **本文档** | 模块设计原则、协作契约、可直接套用的代码骨架 |
| [web-design-guide.md](file:///f:/Code/Gateway/docs/web-design-guide.md) | 前端视觉与组件规范 |
| [data-models.md](file:///f:/Code/Gateway/docs/data-models.md) | 现有数据字段详解 |

---

## 一、架构总览

```
┌──────────────────── HTTP Server (chi v5) ────────────────────┐
│  /api/*  ─► api.Server ─► store (GORM/SQLite)              │
│                         ─► engine (链路引擎，热重载)         │
│                         ─► logx  (统一日志 fanout)          │
│  /*      ─► embed.FS   ─► 静态 HTML/JS/CSS 单页应用         │
└─────────────────────────────────────────────────────────────┘
```

### 分层职责

| 层 | 目录 | 职责 | 对外暴露 |
|----|------|------|----------|
| 入口 | `main.go` | 装配、信号监听、优雅退出 | — |
| 配置 | `internal/config` | 静态默认值与数据库设置读写 | `config.Settings` |
| 路由 | `internal/web` | chi 路由、静态分发、请求日志 | `Router(...)` |
| API | `internal/api` | REST 处理器、引擎回调 | `api.Server` |
| 存储 | `internal/store` | GORM 模型与迁移 | `store.Open(...)` |
| 引擎 | `internal/engine` | 链路 worker、采集循环、热重载 | `engine.Engine` |
| 连接器 | `internal/engine/connector` | 串口/TCP 抽象 | `Driver` 接口 |
| 协议 | `internal/engine/converter` | 帧组装/解析 | `FrameIO` 接口 |
| 日志 | `internal/logx` | slog fanout 三路输出 | `logx.Module(name)` |
| 前端 | `internal/web/static` | 单页 UI | 嵌入式静态资源 |

### 参考实现位置

| 关注点 | 参考文件 |
|--------|----------|
| 装配与优雅退出 | [main.go](file:///f:/Code/Gateway/main.go) |
| 路由组装 | [internal/web/router.go](file:///f:/Code/Gateway/internal/web/router.go) |
| REST 处理器模板 | [internal/api/](file:///f:/Code/Gateway/internal/api) |
| 引擎 facade | [internal/api/api.go](file:///f:/Code/Gateway/internal/api/api.go) |
| 存储模型 | [internal/store/models.go](file:///f:/Code/Gateway/internal/store/models.go) |
| 引擎差量调谐 | [internal/engine/engine.go](file:///f:/Code/Gateway/internal/engine/engine.go) |
| 日志 fanout | [internal/logx/logx.go](file:///f:/Code/Gateway/internal/logx/logx.go) |
| 前端 API 客户端 | [internal/web/static/js/api.js](file:///f:/Code/Gateway/internal/web/static/js/api.js) |

---

## 二、核心设计原则

1. **单体二进制**：前后端同源部署，静态资源通过 `//go:embed` 编入二进制，无独立前端服务。
2. **接口化解耦**：跨层调用一律走接口（如 `EngineFacade`），便于测试桩接与未启用场景的兜底。
3. **热重载优先**：配置变更触发差量调谐而非全量重启，基于配置指纹避免抖动。
4. **JSON Blob 灵活 schema**：业务字段用 `datatypes.JSON` 存储，扩展字段无需改表。
5. **回退兜底**：配置缺失、引擎未启用、硬件 YAML 解析失败等场景均提供默认值与静默降级。
6. **并发安全收敛在引擎**：API 层无锁；worker 独占自己的 Driver/Converter，引擎用 `sync.Mutex` 保护 worker map。
7. **统一响应封装**：所有业务接口返回 `{code, data}` / `{code, msg}`，HTTP 状态码与业务码一致。

---

## 三、后端模块设计规范

### 3.1 目录与文件命名

```
internal/
├── api/
│   ├── api.go              Server 结构 + EngineFacade 接口 + ok/fail 工具
│   ├── {module}.go         每个业务模块一个文件：ListX/SaveX/DeleteX 三件套
│   └── ...
├── store/
│   ├── db.go               Open + AutoMigrate
│   └── models.go           所有 GORM 模型集中
└── ...
```

**约定**：
- 每个 REST 资源对应 `internal/api/{module}.go` 一个文件
- 实现 `List / Save / Delete` 三件套（按需）
- GORM 模型统一放 `store/models.go`，新增表在 `store.Open` 的 `AutoMigrate` 中追加

### 3.2 路由注册

在 [internal/web/router.go](file:///f:/Code/Gateway/internal/web/router.go) 的 `r.Route("/api", ...)` 中追加：

```go
r.Route("/api", func(r chi.Router) {
    // ... 现有路由
    r.Get("/alerts", s.ListAlerts)        // 列表
    r.Post("/alerts", s.SaveAlert)        // 创建/更新
    r.Delete("/alerts/{id}", s.DeleteAlert)  // 删除
})
```

**约定**：
- 资源名复数（`/alerts` 不是 `/alert`）
- 路径参数用 `{id}`
- REST 动词：GET 列表、POST upsert、DELETE 删除

### 3.3 响应封装契约

统一使用 [api.go](file:///f:/Code/Gateway/internal/api/api.go) 中的 `ok` / `fail`：

```go
// 成功
ok(w, data)                    // → { "code": 0, "data": <any> }

// 失败
fail(w, http.StatusBadRequest, "参数错误")
fail(w, http.StatusNotFound, "资源不存在")
fail(w, http.StatusConflict, "资源冲突")
fail(w, http.StatusServiceUnavailable, "引擎未就绪")
```

**状态码选用**：

| 场景 | 状态码 |
|------|--------|
| 成功 | 200 |
| 参数错误 / JSON 解析失败 | 400 |
| 资源不存在 | 404 |
| 业务冲突（如硬件资源占用） | 409 |
| 引擎未启动 / 队列满 | 503 |
| 内部错误 | 500 |

### 3.4 存储模型设计

#### 基础模型模板

```go
type Alert struct {
    ID        string         `gorm:"primaryKey" json:"id"`     // UUID（业务可生成）
    // 或 int 自增：`gorm:"primaryKey" json:"id"`，前端 id==0 视为新建
    Name      string         `json:"name"`
    Rule      datatypes.JSON `json:"rule"`      // 灵活 schema 字段
    Enabled   bool           `json:"enabled"`
    CreatedAt time.Time      `json:"-"`         // 不暴露前端
    UpdatedAt time.Time      `json:"-"`
}
```

#### 设计规范

- **主键策略**：
  - 业务可生成 ID：业务可在前端生成的实体（如设备模型，规则 `Profile-{档案索引}`）
  - 规则生成 ID：通道索引从 0 开始、取最小未占用值，通道ID 按规则 `Channel-{通道索引}` 由服务端合成，二者均不可修改（参考 [channel.go](file:///f:/Code/Gateway/internal/api/channel.go)）
  - **关键**：更新时先 `First` 查存在性再 `Save`，避免伪造主键插入脏数据；通道索引 / 通道ID 更新时保留库中原值
- **时间字段**：`CreatedAt/UpdatedAt` 标 `json:"-"`，避免泄露与前端误改；更新前先读出原 `CreatedAt` 保留
- **JSON Blob**：扩展性强的字段（如配置、规则、属性表）用 `datatypes.JSON`，字段扩展无需改表
- **列表查询**：统一 `Order("id asc")` 或 `Order("xxx_index asc")`

#### upsert 模板

```go
func (s *Server) SaveAlert(w http.ResponseWriter, r *http.Request) {
    var a store.Alert
    if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
        fail(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
        return
    }
    if a.ID == "" {
        fail(w, http.StatusBadRequest, "缺少 ID")
        return
    }
    // 保留原始 CreatedAt
    var exist store.Alert
    if err := s.DB.First(&exist, "id = ?", a.ID).Error; err == nil {
        a.CreatedAt = exist.CreatedAt
    } else if !errors.Is(err, gorm.ErrRecordNotFound) {
        fail(w, http.StatusInternalServerError, err.Error())
        return
    }
    if err := s.DB.Save(&a).Error; err != nil {
        fail(w, http.StatusInternalServerError, err.Error())
        return
    }
    s.reloadEngine()   // 如需触发引擎重载
    ok(w, a)
}
```

#### 冲突检测模板

参考 [channel.go](file:///f:/Code/Gateway/internal/api/channel.go) 的 `channelResourceKey`：

```go
// 检测同一资源不能被多个记录占用（如串口/IP+端口/唯一编号）
if key := resourceKey(a); key != "" {
    var others []store.Alert
    s.DB.Where("id <> ?", a.ID).Find(&others)
    for _, o := range others {
        if resourceKey(o) == key {
            fail(w, http.StatusConflict, "资源被「"+o.Name+"」占用")
            return
        }
    }
}
```

### 3.5 引擎协作规范

#### Facade 接口

API 层只通过 `EngineFacade`（[api.go](file:///f:/Code/Gateway/internal/api/api.go)）调用引擎：

```go
type EngineFacade interface {
    Apply(plans []engine.ChannelPlan, models []store.DeviceModel)  // 差量调谐
    Submit(channelID int, cmd engine.WriteCommand) bool              // 投递写命令
    Values(channelID int) map[string]engine.SessionEntry             // 读取实时值
}
```

**规范**：
- 新增引擎能力时，**扩展 facade 接口**而非让 API 直接依赖 `engine.Engine`
- facade 允许为 nil（未启用引擎的场景），调用前判断 `if s.Engine == nil`

#### 热重载触发点

任何影响引擎运行的配置变更接口，末尾调用 `s.reloadEngine()`：

```go
func (s *Server) reloadEngine() {
    if s.Engine == nil { return }
    var channels []store.Channel
    s.DB.Order("id asc").Find(&channels)
    var models []store.DeviceModel
    s.DB.Order("profile_index asc").Find(&models)
    plans, warnings := engine.BuildPlans(channels, models)
    for _, msg := range warnings {
        logx.Module("api").Warn("采集计划警告", "warning", msg)
    }
    s.Engine.Apply(plans, models)
}
```

#### 差量调谐（指纹策略）

参考 [engine.go](file:///f:/Code/Gateway/internal/engine/engine.go) 的 `planFingerprint`：

| 期望态 | 当前态 | 动作 |
|--------|--------|------|
| 有 | 无 | 启动 |
| 无 | 有 | 停止 |
| 有 | 有，指纹相同 | 保持不变 |
| 有 | 有，指纹变化 | 重启（先停后启） |

**指纹设计要点**：
- 覆盖所有影响运行结果的字段（类型/配置/协议/分组/属性元数据）
- **排除** Legacy 兼容字段与显示字段，避免转换后抖动触发无谓重启
- 浮点数用 `math.Float64bits` 写入哈希，保证精确匹配

#### 实时数据读取

```
GET /api/realtime?device={channelID}/{deviceIndex}
   └─► s.Engine.Values(channelID)       // worker 内存缓存快照
        └─► 按 "deviceIndex/" 前缀过滤
```

**缓存键约定**：`"deviceIndex/propName"`，API 层做前缀过滤

#### 写命令下发

```
POST /api/set { channelId, deviceIndex, propName, value }
   └─► s.Engine.Submit(channelID, cmd)
        └─► worker.writeCh (cap=32) 非阻塞投递
             └─► collectLoop 写优先消费 → execWrite → Send
```

**规范**：
- 投递队列满返回 `false` → API 返回 503
- 写命令在 worker 侧做逆变换：`raw = (engineering - delta) / coef`
- 写优先：`collectLoop` 每 tick 先非阻塞检查 `writeCh`

### 3.6 日志接入规范

#### 获取 logger

```go
log := logx.Module("alert")   // mod 字段自动附加
log.Info("创建告警", "id", a.ID, "name", a.Name)
log.Warn("冲突", "key", key)
log.Error("保存失败", "err", err)
```

#### 三路 fanout

参考 [logx.go](file:///f:/Code/Gateway/internal/logx/logx.go)：
- **终端**：文本格式，便于人读
- **文件**：JSON 格式 + 大小/每日轮转（lumberjack）
- **前端出口**：环形缓冲 + SSE 推送

**规范**：
- 所有日志通过 `logx.Module(name)` 获取，**不直接用** `log.Println` 或 `fmt.Println`
- 模块名 (`mod`) 短小稳定，前端可据此过滤
- 关键操作带上下文字段（`channel`, `device`, `id` 等）

#### 前端日志出口

| 端点 | 模式 | 用途 |
|------|------|------|
| `GET /api/syslog` | JSON 快照 | 拉取最近 N 条 |
| `GET /api/syslog/stream` | SSE | 实时推送 |

SSE 规范（参考 [sink.go](file:///f:/Code/Gateway/internal/logx/sink.go)）：
- 先补发当前快照，再订阅增量
- 25s 心跳维持连接
- `X-Accel-Buffering: no` 禁用 nginx 缓冲
- 订阅者消费不过来则丢条，**绝不阻塞日志主流程**

---

## 四、前端模块设计规范

### 4.1 文件组织

```
internal/web/static/
├── index.html              单页入口
├── css/app.css             统一样式（设计令牌见 web-design-guide.md）
└── js/
    ├── constants.js         常量 + 全局 state
    ├── helpers.js           工具函数 + switchSection
    ├── api.js               API 客户端 + 持久化转换
    ├── {module}.js          业务模块独立文件
    └── main.js              init / 启动
```

**加载顺序**（在 `index.html` 末尾按依赖追加）：
```html
<script src="/js/constants.js"></script>
<script src="/js/helpers.js"></script>
<!-- 依赖 constants/helpers 的模块 -->
<script src="/js/{module}.js"></script>
<script src="/js/main.js"></script>   <!-- main 永远最后 -->
```

### 4.2 全局 state 聚合

所有运行时数据统一放 [constants.js](file:///f:/Code/Gateway/internal/web/static/js/constants.js) 的 `state` 对象：

```js
const state = {
  models: [],
  hardware: {},
  channels: [],
  // 新增模块的数据：
  alerts: [],
  editingAlertId: null,
};
```

**规范**：
- 单一 state 作为前端唯一数据源，避免多模块数据不一致
- 不引入 store 框架，直接读写 `state`
- 编辑态临时变量（如 `editingId`）也放 `state`

### 4.3 区块接入约定

每个一级区块在 `index.html` 新增：

```html
<section id="section-alert" class="app-section d-none">
  <div class="section-header">
    <h4><i class="bi bi-bell me-2" style="color:var(--primary)"></i>告警规则</h4>
    <div class="sub">管理设备告警规则与通知策略</div>
  </div>
  <!-- 区块内容 -->
</section>
```

侧栏新增按钮：

```html
<button class="sidebar-item" id="nav-alert" onclick="switchSection('alert')">
  <i class="bi bi-bell"></i><span>告警规则</span>
</button>
```

`switchSection`（[helpers.js](file:///f:/Code/Gateway/internal/web/static/js/helpers.js)）按需追加初始化：

```js
function switchSection(key) {
  document.querySelectorAll('.app-section').forEach(s => s.classList.add('d-none'));
  document.getElementById('section-' + key).classList.remove('d-none');
  document.querySelectorAll('.sidebar-item').forEach(i => i.classList.remove('active'));
  document.getElementById('nav-' + key).classList.add('active');
  // 按需追加：
  if (key === 'alert') renderAlertList();
}
```

**命名约定**：
- 区块 ID：`section-{key}`
- 侧栏按钮 ID：`nav-{key}`
- `key` 短小、单数、与路由 `/api/{key}s` 对应

### 4.4 API 客户端使用规范

统一走 [api.js](file:///f:/Code/Gateway/internal/web/static/js/api.js) 的 `apiGet / apiPost / apiDelete`：

```js
// 列表
const rows = await apiGet('/alerts');
state.alerts = rows || [];

// 新建/更新
await apiPost('/alerts', { id, name, rule, enabled });

// 删除
await apiDelete('/alerts/' + id);
```

**规范**：
- **不直接** `fetch`，所有请求走 `apiReq`（已封装错误处理与解包）
- 成功时 `apiReq` 返回 `data` 字段，失败时 `throw Error(msg)`
- 错误处理用 `try / catch`，错误展示用 `alert` 或 toast

### 4.5 启动流程接入

在 [main.js](file:///f:/Code/Gateway/internal/web/static/js/main.js) 的 `init()` 中按依赖顺序加载数据：

```js
function init() {
  Promise.all([loadModels(), loadHardware()])
    .then(() => loadChannels())
    .then(() => loadAlerts())      // 新增模块的初始化
    .then(() => {
      switchSection('device');
      renderModelList();
      renderChannelList();
      renderAlertList();
    });
}
```

### 4.6 视觉规范

遵循 [web-design-guide.md](file:///f:/Code/Gateway/docs/web-design-guide.md)，核心要点：
- 主题色用 `var(--primary)` 等 CSS 变量，**不硬编码** 颜色
- 表单字段用 `fw-semibold` 标签 + 必填红星 + `invalid-feedback`
- 列表页用 `.model-card` 形态
- 多步流程用 `.stepper` + `.wizard-card`
- 状态标签用 `.badge-r/.badge-w/.badge-rw` 等语义化配色

---

## 五、数据契约规范

### 5.1 命名约定

**全链路 camelCase**：JSON tag、前端 state、前端表单 ID 全部一致。

| 层 | 示例 |
|----|------|
| 存储 GORM tag | `json:"profileIndex"` `json:"serialName"` |
| 前端 state | `state.profile.profileIndex` |
| 前端 DOM ID | `pf-profileIndex`（前缀-字段名） |
| 引擎 Config JSON tag | `json:"serialName"`（Go 字段 PascalCase） |

**DOM ID 前缀约定**：
- `pf-` = profile（设备档案）
- `ch-` = channel（链路）
- `pm-` = property modal（属性弹窗）
- `rt-` = realtime（实时数据）
- `log-` = log（日志）
- 新模块自取前缀，如 `al-` = alert

### 5.2 数据流转换

前端表单 ↔ 后端存储有三层转换，**全部收敛在前端**：

```text
前端 state (扁平)                后端 store (嵌套 JSON)
─────────────────               ──────────────────────
state.profile.{字段}  ──toPayload──►  { id, profile, properties }
state.channels[i]     ──toPayload──►  { id, name, type, config, devices }
                       ◄──fromRow───   (反向：拆 config 到扁平字段供表单回填)
```

**规范**：
- 后端永远存储完整 JSON blob
- 前端在提交/接收时做扁平化 ↔ 嵌套的转换
- 转换函数集中放在 [api.js](file:///f:/Code/Gateway/internal/web/static/js/api.js)，命名 `xxxFromRow` / `xxxToPayload`
- **后端不跟随表单字段变动**，扩展字段只在 blob 内部加

### 5.3 硬件配置协作

数据库 `gateway_settings` 中的丝印 ↔ 节点映射：

```yaml
Serial:
  COM1: /dev/ttyS1      # key=丝印标签, value=真实设备节点
```

**协作流程**：
1. 后端 `/api/settings` 返回应用与硬件设置，`/api/hardware` 返回其硬件映射 `{ Serial: {COM1:"/dev/ttyS1",...}, ... }`
2. 新数据库由 `config.DefaultSettings()` 写入静态默认映射；前端 `loadSettings` 存入 `state.hardware`
3. 渲染下拉框：`option value=真实节点, 文本=丝印标签`
4. 用户选择丝印 → 提交时 value 自动为真实节点

**规范**：硬件配置由数据库持久化，通过“网关设置”页面修改；前端通过丝印降低认知负担，导出 JSON 用真实节点保证可执行。

---

## 六、模块设计模板

### 6.1 后端模板：`internal/api/{module}.go`

```go
package api

import (
    "encoding/json"
    "errors"
    "net/http"
    "strconv"

    "gateway/internal/logx"
    "gateway/internal/store"

    "github.com/go-chi/chi/v5"
    "gorm.io/gorm"
)

// List{Module}s 返回全部资源。
func (s *Server) List{Module}s(w http.ResponseWriter, r *http.Request) {
    var list []store.{Module}
    if err := s.DB.Order("id asc").Find(&list).Error; err != nil {
        fail(w, http.StatusInternalServerError, err.Error())
        return
    }
    ok(w, list)
}

// Save{Module} 创建或更新资源。
// id==0（自增）或 id==""（UUID）视为新建。
func (s *Server) Save{Module}(w http.ResponseWriter, r *http.Request) {
    var m store.{Module}
    if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
        fail(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
        return
    }
    // 必填校验
    if m.ID == "" {
        fail(w, http.StatusBadRequest, "缺少 ID")
        return
    }
    // 冲突检测（按需）
    // ...
    // 保留 CreatedAt
    var exist store.{Module}
    if err := s.DB.First(&exist, "id = ?", m.ID).Error; err == nil {
        m.CreatedAt = exist.CreatedAt
    } else if !errors.Is(err, gorm.ErrRecordNotFound) {
        fail(w, http.StatusInternalServerError, err.Error())
        return
    }
    if err := s.DB.Save(&m).Error; err != nil {
        fail(w, http.StatusInternalServerError, err.Error())
        return
    }
    // 如需触发引擎重载
    s.reloadEngine()
    logx.Module("{module}").Info("保存", "id", m.ID)
    ok(w, m)
}

// Delete{Module} 删除指定资源。
func (s *Server) Delete{Module}(w http.ResponseWriter, r *http.Request) {
    id := chi.URLParam(r, "id")
    if id == "" {
        fail(w, http.StatusBadRequest, "无效的 ID")
        return
    }
    res := s.DB.Delete(&store.{Module}{}, "id = ?", id)
    if res.Error != nil {
        fail(w, http.StatusInternalServerError, res.Error.Error())
        return
    }
    if res.RowsAffected == 0 {
        fail(w, http.StatusNotFound, "资源不存在: id="+id)
        return
    }
    s.reloadEngine()
    ok(w, map[string]string{"id": id})
}
```

### 6.2 后端模板：存储模型

```go
// 在 internal/store/models.go 追加
type {Module} struct {
    ID        string         `gorm:"primaryKey" json:"id"`
    Name      string         `json:"name"`
    Config    datatypes.JSON `json:"config"`     // 灵活 schema 字段
    Enabled   bool           `json:"enabled"`
    CreatedAt time.Time      `json:"-"`
    UpdatedAt time.Time      `json:"-"`
}
```

```go
// 在 internal/store/db.go 的 Open 中追加 AutoMigrate
if err := db.AutoMigrate(&DeviceModel{}, &Channel{}, &{Module}{}); err != nil {
    return nil, err
}
```

### 6.3 后端模板：路由注册

```go
// 在 internal/web/router.go 的 r.Route("/api", ...) 中追加
r.Get("/{module}s", s.List{Module}s)
r.Post("/{module}s", s.Save{Module})
r.Delete("/{module}s/{id}", s.Delete{Module})
```

### 6.4 前端模板：`internal/web/static/js/{module}.js`

```js
/* ══════════════ {Module} ══════════════ */

async function load{Module}s() {
  try {
    const rows = await apiGet('/{module}s');
    state.{module}s = rows || [];
  } catch (e) { state.{module}s = []; }
}

function render{Module}List() {
  const wrap = document.getElementById('{module}-cards');
  if (!wrap) return;
  wrap.innerHTML = state.{module}s.map(m => `
    <div class="col-md-6 col-lg-4">
      <div class="model-card">
        <div class="model-card-top">
          <div class="model-card-icon"><i class="bi bi-bell"></i></div>
          <div class="min-w-0">
            <div class="model-card-title">${escapeHtml(m.name)}</div>
            <div class="model-card-sub">${escapeHtml(m.id)}</div>
          </div>
        </div>
        <div class="model-card-actions">
          <button class="btn btn-outline-secondary btn-sm" onclick="edit{Module}('${escapeHtml(m.id)}')">
            <i class="bi bi-pencil me-1"></i>编辑
          </button>
          <button class="btn btn-outline-danger btn-sm" onclick="delete{Module}('${escapeHtml(m.id)}')">
            <i class="bi bi-trash"></i>
          </button>
        </div>
      </div>
    </div>
  `).join('');
}

async function save{Module}() {
  const payload = { /* 从表单收集 */ };
  try {
    await apiPost('/{module}s', payload);
    await load{Module}s();
    render{Module}List();
  } catch (e) {
    alert('保存失败：' + e.message);
  }
}

async function delete{Module}(id) {
  if (!confirm('确认删除？')) return;
  try {
    await apiDelete('/{module}s/' + id);
    await load{Module}s();
    render{Module}List();
  } catch (e) {
    alert('删除失败：' + e.message);
  }
}
```

### 6.5 前端模板：HTML 区块

```html
<!-- 在 index.html 的 .main-inner 中追加 -->
<section id="section-{module}" class="app-section d-none">
  <div class="section-header">
    <h4><i class="bi bi-bell me-2" style="color:var(--primary)"></i>{模块名}</h4>
    <div class="sub">{模块描述}</div>
  </div>
  <div class="landing-toolbar mt-3">
    <span class="count-pill">已配置 <span id="{module}-count" class="fw-semibold">0</span> 个</span>
    <button class="btn btn-primary" onclick="new{Module}()">
      <i class="bi bi-plus-lg me-1"></i>新建
    </button>
  </div>
  <div class="row g-3 mt-1" id="{module}-cards"></div>
</section>
```

### 6.6 前端模板：侧栏按钮

```html
<!-- 在 .sidebar-nav 中追加 -->
<button class="sidebar-item" id="nav-{module}" onclick="switchSection('{module}')">
  <i class="bi bi-bell"></i><span>{模块名}</span>
</button>
```

---

## 七、设计 Checklist

新增模块时按以下清单逐项核对：

### 7.1 后端

- [ ] 在 `internal/store/models.go` 定义 GORM 模型，时间字段标 `json:"-"`
- [ ] 在 `store.Open` 的 `AutoMigrate` 中追加新表
- [ ] 在 `internal/api/{module}.go` 实现 List/Save/Delete（按需）
- [ ] 保存前先 `First` 查存在性，保留原 `CreatedAt`
- [ ] 业务冲突返回 409，参数错误 400，不存在 404，引擎未就绪 503
- [ ] 响应统一走 `ok(w, data)` / `fail(w, code, msg)`
- [ ] 影响引擎运行的接口末尾调用 `s.reloadEngine()`
- [ ] 引擎能力扩展通过 `EngineFacade` 接口，不直接依赖 `engine.Engine`
- [ ] 日志通过 `logx.Module("xxx")` 获取，不直接用 `log` / `fmt`
- [ ] 在 `internal/web/router.go` 注册路由，资源名复数

### 7.2 前端

- [ ] 在 `js/{module}.js` 实现模块逻辑
- [ ] 在 `index.html` 末尾按依赖顺序加载脚本
- [ ] 在 `index.html` 新增 `<section class="app-section d-none" id="section-{key}">`
- [ ] 在侧栏新增 `<button class="sidebar-item" id="nav-{key}">`
- [ ] 在 `switchSection` 中按需追加初始化与轮询启停
- [ ] 常量与状态追加到 `constants.js` 的 `state` 对象
- [ ] API 调用统一走 `apiGet / apiPost / apiDelete`
- [ ] 字段命名全 camelCase，DOM ID 用 `{前缀}-{字段名}`
- [ ] 视觉风格遵循 [web-design-guide.md](file:///f:/Code/Gateway/docs/web-design-guide.md)

### 7.3 数据契约

- [ ] JSON 字段命名 camelCase，与 GORM tag 一致
- [ ] 灵活 schema 字段用 `datatypes.JSON`，避免改表
- [ ] 前端提交时做扁平化 ↔ 嵌套转换，后端只存完整 blob
- [ ] 转换函数集中 `api.js`，命名 `xxxFromRow` / `xxxToPayload`
- [ ] 列表查询统一 `Order("id asc")` 或 `Order("xxx_index asc")`
- [ ] 主键策略：UUID 用于业务可生成实体，自增用于链路类实体

---

## 八、关键依赖版本

| 依赖 | 版本 | 用途 |
|------|------|------|
| Go | 1.23 | 标准库 `log/slog` 作为日志核心 |
| go-chi/chi/v5 | v5.1.0 | HTTP 路由 |
| gorm.io/gorm | v1.25.12 | ORM |
| gorm.io/datatypes | v1.2.1 | JSON 字段类型 |
| glebarez/sqlite | v1.11.0 | 纯 Go SQLite（无 CGO） |
| gopkg.in/yaml.v3 | v3.0.1（间接） | `go.bug.st/serial` 测试依赖；网关不使用 YAML 配置 |
| gopkg.in/natefinch/lumberjack.v2 | v2.2.1 | 日志滚动 |
| go.bug.st/serial | v1.6.2 | 串口通信 |
| Bootstrap | 5.3.2 | 前端 CSS（CDN） |
| Bootstrap Icons | 1.11.3 | 图标（CDN） |

**约束**：不引入额外 UI 框架或构建工具，自定义样式收敛到 `app.css`，自定义脚本收敛到 `static/js/`。
