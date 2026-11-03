# PowerPulse 平台告警消息契约标准

本文档定义 PowerPulse 平台**跨模块通用**的告警/事件消息契约：统一主题命名空间、消息信封、消息结构与语义规则。所有 PowerPulse 模块（物模型 thingsmodel、网关 gateway 及未来模块）发布告警与事件消息时**必须遵循本标准**。

- 各模块自身业务契约（查询、控制、数据发布等）见各自模块的契约文档，如 [thingsmodel-message-contract-design.md](thingsmodel-message-contract-design.md)。
- 信封复用网关侧 `PP-Message-*` 契约，见 [gateway-message-contract-design.md](gateway-message-contract-design.md)。

## 1. 设计原则

- **一个全局告警命名空间**：`powerpulse.alarm`，模块为二级命名空间；
- **消息自描述**：来源（`source`）、类别（`category`）、生命周期（`kind`）、严重度（`level`）全部在负载中，消费方无需额外上下文即可路由、分级、落库；
- **订阅一次收全量**：`powerpulse.alarm.>`；按模块订阅：`powerpulse.alarm.thingsmodel`；
- **模块内不再细分主题**：设备告警/软件告警/系统事件走同一主题，靠 `category` 区分；
- 本阶段仅使用 **Core NATS**：无持久化、无重放、无跨模块顺序保证；消费方自行落库与排序。

## 2. 主题定义

| Subject | 发布方 | 说明 |
|---------|--------|------|
| `powerpulse.alarm.thingsmodel` | 物模型模块 | 设备告警沿触发、控制终态等 |
| `powerpulse.alarm.gateway` | 网关模块 | 链路断连/恢复、写命令失败终态、panic 隔离等 |
| `powerpulse.alarm.{module}` | 未来模块 | 新模块接入需登记 `{module}` 名称 |

**规则**

- `{module}` 为全局唯一的模块名（小写），新增模块须在本表登记，避免主题冲突；
- `{module}` 与消息内 `source.module` 必须一致（主题用于路由，负载用于自描述与审计）。

## 3. 消息信封

复用 `PP-Message-*` 四个 Header，`PP-Message-Type` 固定为 `alarm`：

| Header | 说明 |
|--------|------|
| `PP-Message-ID` | 消息唯一 ID，UUIDv4（与负载 `alarm_id` 一致，作为消费方幂等键） |
| `PP-Message-Type` | 固定 `alarm` |
| `PP-Message-Version` | 消息版本号，当前 `v1.0` |
| `PP-Message-Timestamp` | 消息时间戳，Unix 毫秒 |

## 4. 消息结构

```golang
// AlarmMessage 平台通用告警/事件消息。
type AlarmMessage struct {
    AlarmID   string      `json:"alarm_id"`            // UUIDv4，消费方幂等键
    Source    AlarmSource `json:"source"`              // 发布方来源
    Category  string      `json:"category"`            // device | software | system
    Kind      string      `json:"kind"`                // trigger | recover | event
    Level     int         `json:"level"`               // 0 提示 / 1 一般 / 2 严重 / 3 紧急
    Timestamp int64       `json:"timestamp"`           // Unix 毫秒
    Count     int         `json:"count,omitempty"`     // 节流聚合计数（聚合消息 >1）

    Device   *AlarmDevice   `json:"device,omitempty"`   // category=device 时必填
    Software *AlarmSoftware `json:"software,omitempty"` // category=software 时必填
    System   *AlarmSystem   `json:"system,omitempty"`   // category=system 时必填
}

// AlarmSource 发布方来源。
type AlarmSource struct {
    Module   string `json:"module"`   // 模块名，与主题二级命名空间一致：thingsmodel / gateway / ...
    Instance string `json:"instance"` // 模块内实例标识；单例模块与 module 相同
}

// AlarmDevice 设备告警明细：由设备监测点位规则触发。
type AlarmDevice struct {
    DeviceID   string    `json:"device_id"`     // 设备实例 ID（模块内唯一）
    DeviceName string    `json:"device_name"`
    EventID    string    `json:"event_id,omitempty"` // 告警条目 ID（发布方模块内稳定，如 event_N）
    EventKey   string    `json:"event_key"`     // 告警规则 key
    EventName  string    `json:"event_name"`
    Point      string    `json:"point"`       // 监测属性 key
    PointName  string    `json:"point_name"`  // 监测属性名称
    Value      any       `json:"value"`       // 触发值
    Rule       AlarmRule `json:"rule"`        // 判定依据
}

// AlarmRule 告警判定规则。
type AlarmRule struct {
    Type      string  `json:"type"`      // equal | upper | lower
    Threshold float64 `json:"threshold"` // 阈值
    Time      int     `json:"time"`      // 持续时间（毫秒），0 表示立即触发
}

// AlarmSoftware 软件告警明细：模块自身行为异常。
type AlarmSoftware struct {
    Code      string         `json:"code"`              // 标准化事件码，见第 6 节
    Component string         `json:"component"`         // 模块内组件，如 runtime / north / south / store
    Message   string         `json:"message,omitempty"` // 人读信息
    Context   map[string]any `json:"context,omitempty"` // 按事件码约定的上下文
}

// AlarmSystem 系统告警明细：跨模块/平台级事件。
type AlarmSystem struct {
    Code      string         `json:"code"`
    Component string         `json:"component"`
    Message   string         `json:"message,omitempty"`
    Context   map[string]any `json:"context,omitempty"`
}
```

## 5. 字段语义

### 5.1 category（类别）

| 取值 | 含义 | 典型场景 | kind 取值 |
|------|------|----------|-----------|
| `device` | 设备告警：设备监测点位规则匹配产生 | 温度过高、通讯故障 | trigger / recover |
| `software` | 软件告警：模块自身行为异常 | 控制下发失败、连接异常 | event（暂无状态型） |
| `system` | 系统告警：跨模块/平台级事件 | 预留 | 预留 |

新增 category 属于**标准级变更**，需评审修订本文档；模块不得私有扩展 category。

### 5.2 kind（生命周期）

| 取值 | 含义 | 约定 |
|------|------|------|
| `trigger` | 状态型告警**触发沿** | 必须与后续 `recover` 配对（同一告警定位键） |
| `recover` | 状态型告警**恢复沿** | 定位键与对应 trigger 一致 |
| `event` | 一次性事件，无恢复沿 | 控制终态、异常通知等 |

状态型告警的**实时状态**不属于本主题职责，由各模块的数据/状态主题承载；本主题只发布业务关注点（触发/恢复沿与一次性事件）。

### 5.3 level（严重度）

| 取值 | 含义 |
|------|------|
| 0 | 提示（信息性通知） |
| 1 | 一般 |
| 2 | 严重（如控制失败终态、设备告警） |
| 3 | 紧急 |

### 5.4 count（聚合计数）

节流聚合消息（见第 7 节）携带窗口内合并的条数；非聚合消息省略。

## 6. 事件码标准

事件码全局唯一，命名规范：`{module}.{domain}.{event}`。

已登记事件码：

| code | 模块 | category | 含义 | level | context 约定 |
|------|------|----------|------|-------|--------------|
| `thingsmodel.cmd.result` | thingsmodel | software | 服务控制执行**失败**终态（成功静默） | 2 | `request_id`、`device_id`、`service_key`、`service_id`、`value`、`message` |
| `thingsmodel.cmd.frequent_failure` | thingsmodel | software | 高频控制失败聚合告警 | 2 | `device_id`、`service_key`、`window_ms` |
| `thingsmodel.model.changed` | thingsmodel | software | 模型定义变更提示（设备创建/更新/删除/启停） | 0 | `device_id`、`action`（created/updated/deleted/disabled） |
| `gateway.link.disconnected` | gateway | software | 链路连接失败**触发沿**（重连成功发 recover） | 2 | `channel_index`、`channel_name`、`target`、`err` |
| `gateway.link.recovered` | gateway | software | 链路重连成功**恢复沿** | 1 | `channel_index`、`channel_name`、`target` |
| `gateway.link.degraded` | gateway | software | 连接重试达上限转入慢速探测 | 2 | `channel_index`、`channel_name`、`attempts`、`max` |
| `gateway.write.failed` | gateway | software | 写命令执行**失败**终态（成功静默） | 2 | `request_id`、`channel_index`、`device_index`、`prop`、`message` |
| `gateway.engine.panic` | gateway | software | 链路 goroutine panic 已隔离 | 3 | `channel_name`、`panic` |
| `gateway.nats.unavailable` | gateway | software | NATS 客户端启动失败，北向扇出禁用 | 1 | `err` |
| `gateway.queue.overflow` | gateway | software | 北向发布队列已满，丢弃最旧事件 | 1 | `queue_size` |

新增事件码须在本表登记（code、category、level、context 约定）。

## 7. 可靠性与节流约定

- **幂等**：消费方以 `alarm_id` 去重；网络重发/多订阅场景下消息可能重复；
- **闭环类消息必达不聚合**：携带请求关联语义（如 `request_id`）的事件必须一请求一消息，禁止聚合，否则消费方闭环断裂；发布方可按业务约定**仅发布失败类终态（成功静默）**以降低提示性噪音；
- **高频状态告警聚合**：发布方按 `(category, code, 定位键)` 维度做时间窗聚合，达到阈值发布一条并携带 `count`，每窗口最多一条；
- **顺序**：跨模块、跨消息无顺序保证，消费方按 `timestamp` 排序、按定位键合并状态。

## 8. 模块接入清单

1. 在第 2 节登记模块名（`{module}`）；
2. 在第 6 节登记事件码（如有 software/system 类消息）；
3. 实现沿语义（状态型告警 trigger/recover 配对）与节流；
4. `source.module` 与主题一致、`source.instance` 填模块内实例标识；
5. 消息结构通过本标准结构体序列化，禁止私有字段（新增字段须修订标准）。
