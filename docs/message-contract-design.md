## NATS 消息契约设计

本文档阐述网关与 NATS 之间的**消息契约**：主题（Subject）设计、消息信封（Header + Data）设计、各主题的消息载体格式定义。

客户端配置、连接/重连策略、启动与降级行为等**客户端设计**内容，参见 [nats-client-design.md](nats-client-design.md)。

### 1. 主题定义

主题保持极简，固定为三类：数据发布（上行）、控制接收（下行）与拓扑查询。链路、设备等元数据全部收束到消息载体，不参与 subject 路由。

| 方向 | Subject | 说明 |
|------|---------|------|
| 数据发布 | `{subjectPrefix}.{gw_id}.data` | 网关 → NATS，所有采集遥测 |
| 控制接收 | `{subjectPrefix}.{gw_id}.cmd` | NATS → 网关，写命令/控制指令 |
| 查询接口 | `{subjectPrefix}.{gw_id}.query` | NATS → 网关，查询接口 |

**示例**（`subjectPrefix: powerpulse.gateway`，`gw_id: gw-001`）：

```
powerpulse.gateway.gw-001.data
powerpulse.gateway.gw-001.cmd
powerpulse.gateway.gw-001.query
```

**设计要点**

- **网关侧固定三个主题**，启动时按 `subjectPrefix` 与 `gw_id` 生成 data、cmd、query 三个 subject；
- **多网关隔离**通过 `gw_id` 实现，A 网关收不到 B 网关的命令；
- **订阅方自过滤**：消费端收到 `.data` 后，按消息体中的 `channel_index`、`device_index` 自行路由；
- **控制响应复用 `.data`**，信封中 `type: "cmdAck"` 标识，不新增主题；
- **查询接口（拓扑发现）**：网关订阅 `.query`，收到 `type: "queryTopology"` 请求后，返回本网关的通道列表及各通道挂载的设备列表（详见 2.2.4）。不提供设备属性工程值查询，实时遥测一律走 `.data` 订阅；
- 本阶段仅使用 **Core NATS**；不引入 JetStream stream、发布确认、持久化或重放语义。

### 2. 消息格式

NATS 客户端之间的消息推荐采用 msg.Header + msg.Data 的组合：msg.Header 用于路由、类型、版本等元信息，msg.Data 用于承载业务数据。

msg.Header 的设计原则是尽量精简，避免过多嵌套，便于快速解析和路由。
msg.Data 可以是任意格式的二进制数据，通常使用 JSON 或 Protobuf 进行序列化。

#### 2.1 消息信封设计

```golang
package envelope
import (
  "fmt"
  "strconv"
  "time"

  "github.com/google/uuid"
  "github.com/nats-io/nats.go"
)
const {
  HeaderMessageID = "PP-Message-ID" // 消息唯一 ID，UUIDv4
  HeaderMessageType = "PP-Message-Type" // 消息类型，data/cmd/query
  HeaderMessageVersion = "PP-Message-Version" // 消息版本号，v1/v2
  HeaderMessageTimestamp = "PP-Message-Timestamp" // 消息时间戳，Unix 毫秒
}

type Envelope struct {
  // 元数据 (映射到 nats.Msg.Header)
  ID        string `json:"id"`        // 消息唯一 ID，UUIDv4
  Type      string `json:"type"`      // 消息类型，data/cmd/query
  Version   string `json:"version"`   // 消息版本号，v1/v2
  Timestamp time.Time  `json:"timestamp"` // 消息时间戳，Unix 毫秒
  // 实际业务负载 (映射到 nats.Msg.Data)
  Payload   []byte `json:"-"`   // 消息体，二进制数据
}

// NewEnvelope 创建一个新的消息信封
func NewEnvelope(messageType string, payload []byte) *Envelope {
	return &Envelope{
		ID:        uuid.New().String(),
		Type:      messageType,
		Version:   "v1.0",
		Timestamp: time.Now().UTC(),
		Payload:   payload,
	}
}

func (e *Envelope) ToNatsMsg(subject string) *nats.Msg {
  msg := nats.NewMsg(subject)
  if msg.Header == nil {
    msg.Header = make(nats.Header)
  }
  msg.Header.Set(HeaderMessageID, e.ID)
  msg.Header.Set(HeaderMessageType, e.Type)
  msg.Header.Set(HeaderMessageVersion, e.Version)
  msg.Header.Set(HeaderMessageTimestamp, strconv.FormatInt(e.Timestamp.UnixMilli(), 10))
  msg.Data = e.Payload
  return msg
}

func (e *Envelope) FromNatsMsg(msg *nats.Msg) (*Envelope, error) {
  if msg.Header == nil {
    return nil, fmt.Errorf("missing message headers")
  }
  ts, err := strconv.ParseInt(msg.Header.Get(HeaderMessageTimestamp), 10, 64)
  env := &Envelope{
    ID:      msg.Header.Get(HeaderMessageID),
    Type:    msg.Header.Get(HeaderMessageType),
    Version: msg.Header.Get(HeaderMessageVersion),
    Timestamp: time.UnixMilli(ts),
    Payload: msg.Data,
  }
  if env.ID == "" || env.Type == "" {
		return nil, fmt.Errorf("invalid envelope: missing required headers")
	}
  return env, nil
}
```

#### 2.2 消息载体格式定义

所有业务负载均为 **JSON 对象**，UTF-8 编码；字段统一 **snake_case**。
三个主题共享同一套路由字段（`channel_index` / `device_index`），解析入口一致。

##### 2.2.1 解析规则（通用）

网关接收侧（`.cmd` / `.query`）统一按下列顺序处理：

1. `Envelope.FromNatsMsg` 解信封，校验 Header；
2. 按主题将 `env.Payload` 解析为对应载体（`.cmd` → `MessageCmd`，`.query` → `MessageQuery`）；
3. `.cmd` 先校验路由：`channel_index` 通道不存在或 `device_index` 设备未挂载 → REQ/REP 直接回复失败响应，不再下派引擎。

---

##### 2.2.2 `{prefix}.{gw_id}.data` — 遥测上行（网关 → NATS）

方式：PUB/SUB，无需应答。信封 `PP-Message-Type: data`。

```golang
// MessageData 一次采集刷新的遥测上报。
type MessageData struct {
  GatewayID    string             `json:"gateway_id"`    // 网关 ID
  GatewaySN    string             `json:"gateway_sn"`    // 网关硬件序列号
  ChannelIndex int                `json:"channel_index"` // 通道索引，从 0 开始（对应通道ID Channel-{索引}）
  DeviceIndex  int                `json:"device_index"`  // 设备在通道挂载列表中的序号
  DeviceName   string             `json:"device_name"`   // 用户填写的设备名称
  DeviceCode   string             `json:"device_code"`   // 设备唯一编码，格式：{gateway_id}{通道索引:03d}{设备索引:03d}，如 "gw_001001001"
  CommNo       int                `json:"comm_no"`       // 设备通讯号（Modbus Unit ID）
  ModelID      string             `json:"model_id"`      // 设备模型 ID
  ModelName    string             `json:"model_name"`    // 设备模型名称
  Properties   map[string]PropVal `json:"properties"`    // 属性 ID → 属性值
}

// PropVal 单个属性值，对齐 engine.SessionEntry。
type PropVal struct {
  Index       int    `json:"index"`       // 属性索引，与物模型属性列表中的序号一致
  Name        string `json:"name"`        // 属性名称
  Unit        string `json:"unit"`        // 工程量单位
  Description string `json:"description"` // 属性值描述
  AccessMode  string `json:"access_mode"` // 读写属性（r / w / rw）
  Value       any    `json:"value"`       // 工程值；解析异常时为 null
  Timestamp   int64  `json:"timestamp"`   // 采集时间，Unix 毫秒
}
```

**说明**
- `gateway_id` 和 `gateway_sn` 标识当前网关，供订阅方识别数据来源；
- `device_code` 为设备唯一编码：**网关 ID + 通道索引（3 位补零）+ 设备索引（3 位补零）** 直接拼接，例如网关 `gw_001` 的 0 号通道 0 号设备为 `"gw_001001001"`。该字段仅供订阅方程序化索引设备，**不需要在前端展示**；
- `properties` 为 map 而非数组：key 是模型属性 ID，订阅方可稳定索引；每个值同时携带属性名称；
  需区分同通道多设备时使用 `device_index`（等价于引擎 cacheKey 的 `deviceIndex/propName` 前缀）。
- `properties` 每个属性值的 `index` 为**属性索引**，即该属性在物模型属性列表中的序号（从 0 开始），与模型编辑页展示的属性索引一致；
- **每轮采集按设备发送一条完整消息**：设备的全部属性为一个传输单位，完成该设备一轮采集后立即上送；属性值即使未变化也不得省略。
- 设备掉线时，`properties` **仅包含**内置虚拟属性 `"在线状态"`，值为 `0`；不携带该设备其他属性的历史值。
- 设备在线时，`properties` 包含全部属性，内置虚拟属性 `"在线状态"` 的值为 `1`。
- 单个属性的协议帧解析或值映射异常时，仍保留该属性键并传输 `{"value": null, "timestamp": <本轮采集时间>}`；不使用上一轮缓存值替代。

---

##### 2.2.3 `{prefix}.{gw_id}.cmd` — 控制下行（NATS → 网关，REQ/REP）

信封 `PP-Message-Type: cmd`。一次请求只允许携带一条写命令。

```golang
// MessageCmd 单条写命令请求（请求体）。
type MessageCmd struct {
  ChannelIndex int     `json:"channel_index"` // 通道索引，从 0 开始（0 为合法值）
  DeviceIndex  int     `json:"device_index"`  // 设备序号
  Name         string  `json:"name"`          // 属性名称
  Value        float64 `json:"value"`         // 工程值（与 engine.WriteCommand.RawValue 对齐）
}

// MessageAck 写命令应答（响应体，REQ/REP 的 reply 或异步 cmdAck）。
type MessageAck struct {
  RequestID    string `json:"request_id"`              // 原请求 PP-Message-ID
  ChannelIndex int    `json:"channel_index"`           // 通道索引
  DeviceIndex  int    `json:"device_index"`            // 设备序号
  Status       string `json:"status"`                  // accepted / failure / success
  Message      string `json:"message,omitempty"`       // 失败原因或补充说明
}
```

**说明**
- `Value` 为 `float64` 工程值，与 [engine.go](internal/engine/plan.go#L204) 的 `WriteCommand{DeviceIndex, PropName, RawValue}` 一一对应，worker 侧负责逆变换编码；
- REQ/REP 仅表示命令是否被网关接受：路由或参数错误返回 `status=failure`；成功投递到引擎写队列返回 `status=accepted`。
- 实际执行完成后，网关复用 `.data` 主题发布信封 `PP-Message-Type: cmdAck`，携带相同的 `request_id`；执行成功为 `status=success`，失败为 `status=failure`。

---

##### 2.2.4 `{prefix}.{gw_id}.query` — 拓扑发现（NATS → 网关，REQ/REP）

信封 `PP-Message-Type: query`。
**用途**：供其他服务模块发现本网关的拓扑——有哪些通道、每个通道挂载了哪些设备。
**不提供**设备属性工程值查询：实时遥测一律通过订阅 `.data` 主题获取。

```golang
// MessageQuery 拓扑查询请求（请求体）。
// 当前仅有 queryTopology 一种类型；Body 预留，后续可按 type 扩展其他查询。
type MessageQuery struct {
    Type string          `json:"type"`           // queryTopology
    Body json.RawMessage `json:"body,omitempty"` // 查询参数（预留）
}

// MessageQueryResp 拓扑查询响应（响应体）。
type MessageQueryResp struct {
    Type     string        `json:"type"`              // 回显请求类型
    OK       bool          `json:"ok"`                // 查询是否成功
    Message  string        `json:"message,omitempty"` // 失败原因
    Channels []ChannelInfo `json:"channels"`          // 通道列表（设备内嵌）
}

// ChannelInfo 通道信息（对齐 store.Channel + 运行状态）。
type ChannelInfo struct {
    ID           string       `json:"id"`            // 通道ID，格式 Channel-{通道索引}
    ChannelIndex int          `json:"channel_index"` // 通道索引（与 data/cmd 消息路由字段一致，从 0 开始）
    Name         string       `json:"name"`
    Type         string       `json:"type"` // Serial/Network
    Connected    bool         `json:"connected"` // 引擎在线状态
    Devices      []DeviceInfo `json:"devices"`   // 挂载设备列表
}

// DeviceInfo 挂载设备信息：标识 + 数据点表。
type DeviceInfo struct {
    Index     int         `json:"index"`     // 挂载序号（即 data/cmd 消息中的 device_index）
    Name      string      `json:"name"`      // 设备名
    Datasheet []DataEntry `json:"datasheet"` // 数据点表（属性元数据）
}

// DataEntry 单个数据点（属性）的元数据。
type DataEntry struct {
    DataIndex string `json:"data_id"`   // 数据 ID
    DataName  string `json:"data_name"` // 数据名称
    DataRW    string `json:"data_rw"`   // 数据读写权限（R/W/RW）
}
```

**说明**
- **一次请求返回全量拓扑**：通道与设备为一对多关系，设备直接内嵌在所属通道的 `devices` 数组中，调用方无需多次往返；
- `DeviceInfo.Datasheet` 来自设备模型（`store.DeviceModel.Properties`）的属性元数据列表，供调用方了解该设备暴露了哪些数据点及读写权限；模型已删除的设备 `datasheet` 置空但不省略该条目；
- 未知 `type` 返回 `ok=false, message="unknown query type"`；
- 响应中**不含任何实时值**——其他服务拿到拓扑后，按 `{channel_index, device_index, 数据名}` 自行从 `.data` 订阅流中匹配数据。
