# PowerPulse 平台日志消息契约标准

本文档定义 PowerPulse 平台**跨模块通用**的日志消息契约：统一主题命名空间、消息信封、消息结构与语义规则。所有 PowerPulse 模块（网关 gateway、物模型 thingsmodel 及未来模块）将运行日志扇出到 NATS 时**必须遵循本标准**。

- 各模块自身业务契约（查询、控制、数据发布等）见各自模块的契约文档，如 [message-contract-design.md](message-contract-design.md)；
- 信封复用网关侧 `PP-Message-*` 契约，见 [message-contract-design.md](message-contract-design.md)；
- 满足告警语义的异常（状态沿、可聚合、需运维介入）**必须**走 [powerpulse-alarm-standard.md](powerpulse-alarm-standard.md)，不得混入日志流；日志流只承载诊断信息流。

## 1. 设计原则

- **一个全局日志命名空间**：`powerpulse.log`，模块为二级命名空间；
- **消息自描述**：来源（`source`）、级别（`level`）、模块内分支（`mod`）、结构化上下文（`attrs`）全部在负载中，消费方无需额外上下文即可路由、分级、落库；
- **订阅一次收全量**：`powerpulse.log.>`；按模块订阅：`powerpulse.log.gateway`；
- **模块内不再细分主题**：终端/文件/前端/NATS 各路日志出口的内容一致性由发布方保证，消费方按 `level` 与 `mod` 自行过滤；
- **尽力而为语义**：本阶段仅使用 Core NATS，日志流无持久化、无重放、无送达确认；消费方不得将日志流作为审计或计费依据；
- **洪泛治理是发布方义务**：发送侧必须做级别过滤与源头限频（见第 6 节），不得把重试循环、轮询噪音原样扇出。

## 2. 主题定义

| Subject | 发布方 | 说明 |
|---------|--------|------|
| `powerpulse.log.gateway` | 网关模块 | 全量网关日志流 |
| `powerpulse.log.thingsmodel` | 物模型模块（预留） | — |
| `powerpulse.log.{module}` | 未来模块 | 新模块接入需登记 `{module}` 名称 |

**规则**

- `{module}` 为全局唯一的模块名（小写），新增模块须在本表登记，避免主题冲突；
- `{module}` 与消息内 `source.module` 必须一致（主题用于路由，负载用于自描述与审计）；
- **主题不按实例细分**：多实例（如多网关）通过消息内 `source.instance` 区分，与告警标准第 2 节规则保持一致；
- `powerpulse.log` 命名空间是平台横切流，**独立于各模块的业务主题前缀**（如网关遥测的 `powerpulse.gateway.{gw_id}.data`），后者的前缀属于模块业务配置。

## 3. 消息信封

复用 `PP-Message-*` 四个 Header，`PP-Message-Type` 固定为 `log`：

| Header | 说明 |
|--------|------|
| `PP-Message-ID` | 消息唯一 ID，UUIDv4（与负载 `log_id` 一致，作为消费方幂等键） |
| `PP-Message-Type` | 固定 `log` |
| `PP-Message-Version` | 消息版本号，当前 `v1.0` |
| `PP-Message-Timestamp` | 消息时间戳，Unix 毫秒 |

## 4. 消息结构

```golang
// LogMessage 平台通用日志消息。
type LogMessage struct {
    LogID     string         `json:"log_id"`              // UUIDv4，消费方幂等键
    Source    LogSource      `json:"source"`               // 发布方来源
    Level     string         `json:"level"`                // debug | info | warn | error（小写）
    Mod       string         `json:"mod"`                  // 模块内分支标识（见 5.2）
    Msg       string         `json:"msg"`                  // 人读消息文本
    Attrs     map[string]any `json:"attrs,omitempty"`      // 结构化上下文，值仅标量（见 5.4）
    Timestamp int64          `json:"timestamp"`            // 日志产生时间，Unix 毫秒
}

// LogSource 发布方来源。
type LogSource struct {
    Module   string `json:"module"`   // 模块名，与主题二级命名空间一致：gateway / thingsmodel / ...
    Instance string `json:"instance"` // 模块内实例标识；网关为 gw_id，单例模块与 module 相同
}
```

## 5. 字段语义

### 5.1 level（级别）

小写规范化，与各模块日志配置枚举一致（区别于本地文件 JSON 的大写 slog 级名）：

| 取值 | 含义 | 典型场景 |
|------|------|----------|
| `debug` | 调试诊断 | 原始报文 hex、轮询成功明细 |
| `info` | 生命周期 | 服务启动、链路连接、配置生效 |
| `warn` | 可恢复异常 | 连接失败重试、发布失败、队列丢弃 |
| `error` | 致命/隔离级 | panic、运行时重启失败、数据库不可用 |

发送侧可按主题配置独立级别阈值（与本地日志级别解耦）：本地日志面向现场排障，平台汇聚面向集中监控，两者的信噪比需求不同。

### 5.2 mod（模块内分支）

模块内稳定的分支标识，`{module}` 内全局唯一。已登记分支：

| module | mod | 说明 |
|--------|-----|------|
| gateway | `main` | 进程入口、生命周期 |
| gateway | `http` | HTTP 访问日志 |
| gateway | `engine` | 采集引擎与链路 worker |
| gateway | `nats` | NATS 北向客户端 |
| gateway | `runtime` | 可重启运行时 |
| gateway | `api` | REST API |
| gateway | `store` | 配置库 |
| gateway | `gorm` | ORM 日志转发 |

新增 mod 须在模块契约文档登记。跨模块不得复用同名 mod 表达不同含义。

### 5.3 source.instance

网关模块为 `gw_id`（与遥测主题中的网关标识一致）；消费方按 instance 过滤单网关日志。单例模块 `instance` 与 `module` 相同。

### 5.4 attrs（结构化上下文）

- key 命名采用 **snake_case**，与消息契约整体风格一致；
- **值仅允许标量**（string / number / bool / null）；
  - error → 字符串化（`.Error()`）；
  - duration → 字符串（如 `"3s"`）；
  - 其余非标量结构 → `fmt.Sprint` 字符串化；
- 消费方不得依赖嵌套结构或特定 key 的存在；发布方新增 key 不属于契约变更；
- 敏感信息（密码、凭据）禁止进入 attrs。

### 5.5 与告警流的分工

| 维度 | 日志流 `powerpulse.log` | 告警流 `powerpulse.alarm` |
|------|------------------------|--------------------------|
| 定位 | 诊断信息流 | 业务关注点（沿与终态） |
| 消息数 | 全量（受级别过滤） | 极少量（沿语义+聚合） |
| 结构化程度 | 人读为主、attrs 辅助 | 强结构（事件码、context 约定） |
| 消费方 | 日志检索/排障 | 告警面板/通知 |

同一异常可以同时出现在两路（日志一条诊断 + 告警一条事件），但**告警语义的消息必须走告警流**——日志流不承载告警职责，消费方不得从日志流中推断告警状态。

## 6. 级别与节流约定

- **级别过滤**：发送侧按主题配置的级别阈值过滤（如默认 `warn`），低于阈值的消息不发布；
- **闭环类不进日志流聚合**：与请求关联的日志（如写命令回执）保持一请求一消息，聚合会破坏消费方闭环；
- **高频噪音源头限频**：重试循环、轮询失败类日志必须在发布方实现「首次 + 周期摘要 + 恢复」模式（如网关断连重试：首次完整、此后每 N 次一条摘要、恢复一条），禁止把每次重试原样扇出；
- **幂等**：消费方以 `log_id` 去重；网络重发场景下消息可能重复；
- **顺序**：无顺序保证，消费方按 `timestamp` 排序。

## 7. 模块接入清单

1. 在第 2 节登记模块名（`{module}`）；
2. 在第 5.2 节登记 mod 分支表；
3. 实现发送侧级别过滤与源头限频；
4. `source.module` 与主题一致、`source.instance` 填模块内实例标识；
5. 消息结构通过本标准结构体序列化，禁止私有字段（新增字段须修订标准）。
