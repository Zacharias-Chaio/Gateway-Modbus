# 数据模型文档

本文档描述网关的两类核心配置数据——**设备模型**与**链路通道**，涵盖前端表单结构、JSON 字段、数据库存储以及引擎内部的协议映射，便于开发与对接。

---

## 一、设备模型（DeviceModel）

设备模型是设备的"模板"，定义设备的档案信息与一组属性的协议映射。一个模型可被多条链路挂载复用。

### 1.1 数据库存储

存储于 SQLite `data/config.db`，表 `device_models`，GORM 模型 `store.DeviceModel`：

| 字段 | 类型 | GORM 标签 | 说明 |
|------|------|-----------|------|
| `ID` | `string` | `primaryKey` | 模型 ID，即 `profile.profileId`，格式 `Profile-{档案索引}`，创建时前端自动生成 |
| `ProfileIndex` | `int` | — | 档案索引，从 0 自增，用于排序 |
| `Name` | `string` | — | 模型名称 |
| `Profile` | `datatypes.JSON` | — | 档案信息（设备/协议元数据），见 §1.2 |
| `Properties` | `datatypes.JSON` | — | 属性数组，见 §1.3 |
| `CreatedAt` | `time.Time` | `json:"-"` | 创建时间（不返回前端） |
| `UpdatedAt` | `time.Time` | `json:"-"` | 更新时间（不返回前端） |

> `Profile` 与 `Properties` 整体以 JSON Blob 存储，结构灵活，无需改表结构即可扩展字段。

### 1.2 Profile（档案信息）

前端 `emptyProfile()` 定义的完整字段：

| 字段 | 类型 | 说明 |
|------|------|------|
| `profileIndex` | `int` | 档案索引（从 0 自增） |
| `profileId` | `string` | 档案 ID，格式 `Profile-{profileIndex}`，自动生成、不可修改（与 `DeviceModel.ID` 一致） |
| `name` | `string` | 模型名称 |
| `manufacturer` | `string` | 厂商 |
| `description` | `string` | 描述 |
| `deviceType` | `string` | 设备类型 |
| `deviceModel` | `string` | 设备型号 |
| `ratedPower` | `number\|null` | 额定功率 |
| `interfaceType` | `string` | 接口类型：`Serial` / `Network`（CAN 已移除） |
| `protocolType` | `string` | 协议类型：仅 `Modbus RTU` / `Modbus TCP`（保存时白名单校验） |
| `protocolVersion` | `string` | 协议版本 |
| `maxRegisterCount` | `int` | 单次读取最大寄存器数（默认 125，即 Modbus 上限） |

### 1.3 Properties（属性数组）

每个属性描述一个采集/控制量的 Modbus 映射。**协议映射为必选配置**（读/写功能码按读写属性必选，基址、偏移、数量必填）；默认虚拟属性 `在线状态`（`online`）不映射实际寄存器，豁免该校验。

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | `string` | 属性 ID，自动生成、不可修改：新建属性为 `Property-{属性索引}`（索引从 1 起，取最小未占用值）；默认在线点保持 `online` |
| `name` | `string` | 属性名称（实时数据展示用，也是缓存键） |
| `description` | `string` | 描述。枚举类属性统一为 JSON 格式字符串（值到含义的映射），如 `{"offline":0,"online":1}` |
| `dataType` | `string` | 数据类型：`bool` / `int` / `float` / `string` |
| `unit` | `string` | 单位 |
| `accessMode` | `string` | 访问模式：`r`（只读）/ `w`（只写）/ `rw`（读写） |
| `startBit` | `int` | 起始位（bit 0 = 最低位），定义数据长度的位区间起点 |
| `endBit` | `int` | 终止位（含两端）。位区间（数据长度）定义解析时取多少位换算工程值，与寄存器数量无强制对应（如 1 寄存器 16 位可只取前 8 位） |
| `deltaValue` | `number` | 偏移量（可正负），`engineering = raw × coef + deltaValue` |
| `coefficient` | `number` | 系数（`engineering = raw × coef + deltaValue`） |
| `readFunctionCode` | `int\|null` | 读功能码：3(保持寄存器)/4(输入寄存器)/1(线圈)/2(离散量)。可读属性（r/rw）必选；null 仅用于虚拟属性或纯写属性 |
| `writeFunctionCode` | `int\|null` | 写功能码：5(写单线圈)/6(写单寄存器)/15(写多线圈)/16(写多寄存器)。可写属性（w/rw）必选；纯读属性可为 null |
| `registerBase` | `int` | 寄存器基址（分组依据），必填 |
| `registerOffset` | `int` | 相对基址的偏移（实际地址 = `registerBase + registerOffset`），必填（可为 0） |
| `registerCount` | `int` | 寄存器数量（1~125），决定读取跨度，必填，与位区间无强制对应；参与协议映射的属性缺失该字段时，构建采集计划直接报错 |
| `byteOrder` | `string` | 字节序（如 `ABCD` / `DCBA` / `BADC` / `CDAB`） |

#### 寄存器寻址与分组

```
实际地址 = registerBase + registerOffset
```

引擎按 `(readFunctionCode, registerBase)` 分组，同一组内的属性合并为一次读请求，请求的寄存器数量为组内最远地址覆盖范围。

#### 数据长度与位提取

`startBit` / `endBit` 定义了属性在寄存器区间的**位级定位**（bit 0 = 最低位）：

- 设计约定：寄存器数量（读取跨度）与位区间（数据长度）无强制对应——位区间定义解析时取数据中的多少位换算工程值；仅要求位区间落在实际读取的数据宽度内
- 寄存器数量为必填（1~125），由前端表单与 CSV 导入校验，引擎构建采集计划时兜底校验
- `string` 类型不走位提取，按字节级处理，长度由 `registerCount` 指定

示例：

| 场景 | startBit | endBit | 寄存器数 | 说明 |
|------|----------|--------|---------|------|
| 单 bit 标志 | 3 | 3 | 1 | 提取第 3 位 |
| 16 位整数 | 0 | 15 | 1 | 全部 16 位 |
| 32 位浮点 | 0 | 31 | 2 | 全部 32 位（2 寄存器） |
| 位段提取 | 4 | 7 | 1 | 提取 bit4~bit7 |

#### 虚拟属性：在线状态

模型创建时默认带一个特殊属性：

```json
{
  "id": "online", "name": "在线状态", "description": "{\"offline\":0,\"online\":1}",
  "dataType": "bool", "accessMode": "r",
  "startBit": 0, "endBit": 0,
  "readFunctionCode": null, "writeFunctionCode": null,
  "registerBase": null, "registerOffset": null
}
```

- `readFunctionCode=null` → 不参与 Modbus 读取，`BuildGroups` 自动跳过。
- 由 worker 根据通信结果写入：轮询成功=1，失败/断线=0。
- 前端实时值直接显示 `0` / `1`。

### 1.4 REST 接口

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/models` | 列出全部模型（按 `profile_index asc`） |
| `POST` | `/api/models` | 新建/更新模型（upsert），成功后触发**引擎热重载** |
| `DELETE` | `/api/models/{id}` | 删除模型，成功后触发**引擎热重载** |

---

## 二、链路通道（Channel）

链路通道描述一条物理通信链路及其挂载的设备。

### 2.1 数据库存储

存储于 SQLite `data/config.db`，表 `channels`，GORM 模型 `store.Channel`：

| 字段 | 类型 | GORM 标签 | 说明 |
|------|------|-----------|------|
| `ID` | `string` | `primaryKey` | 通道 ID，格式 `Channel-{通道索引}`，自动生成、不可修改（服务端按 `store.ChannelIDFromIndex` 合成） |
| `ChannelIndex` | `int` | `uniqueIndex` | 通道索引，从 0 开始，新建时取最小未占用值，不可修改 |
| `Name` | `string` | — | 链路名称 |
| `Type` | `string` | — | 链路类型：`Serial` / `Network`（CAN 已移除，保存时白名单校验拦截） |
| `Config` | `datatypes.JSON` | — | 通信参数（见 §2.2） |
| `Devices` | `datatypes.JSON` | — | 挂载设备列表（见 §2.3） |
| `CreatedAt` | `time.Time` | `json:"-"` | 创建时间 |
| `UpdatedAt` | `time.Time` | `json:"-"` | 更新时间 |

> **通道索引与通道ID 规则**：通道索引从 0 开始，新建链路时由服务端分配最小未占用值（删除后空出的索引可被复用）；通道ID 按固定规则 `Channel-{通道索引}` 自动生成，二者均不可修改。历史「整数自增主键」库在启动时自动迁移：旧行按 id 升序重编号为索引 0,1,2…，主键改写为 `Channel-{索引}`，其余字段原样保留。引擎、实时数据与报文监控均以通道索引定位链路。

### 2.2 Config（通信参数）

通用字段（所有链路类型）：

| 字段 | 类型 | 说明 |
|------|------|------|
| `frameInterval` | `int\|null` | 帧间隔（毫秒），半双工总线发送后等待响应 |
| `reconnectRetries` | `int\|null` | 连接失败重试次数 |
| `resendRetries` | `int\|null` | 单帧重发次数 |
| `pollInterval` | `int\|null` | 轮询间隔（毫秒），0 表示默认 500ms |

按链路类型的附加字段：

**Serial（串口）**

| 字段 | 类型 | 说明 |
|------|------|------|
| `serialName` | `string` | 串口节点（如 `/dev/ttyS1`），从数据库接口映射转换 |
| `baudRate` | `int` | 波特率 |
| `dataBits` | `int` | 数据位（7/8） |
| `parity` | `string` | 校验：`None` / `Even` / `Odd` |
| `stopBits` | `string` | 停止位：`1` / `1.5` / `2` |

**Network（网络/TCP）**

| 字段 | 类型 | 说明 |
|------|------|------|
| `deviceIp` | `string` | 设备 IP |
| `devicePort` | `int` | 设备端口（如 502） |

### 2.3 Devices（挂载设备列表）

数组，每项描述一个挂载在链路上的从站设备：

| 字段 | 类型 | 说明 |
|------|------|------|
| `index` | `int` | 在链路内的序号（从 0 起） |
| `commNo` | `int` | 通信地址 / 从站号（Modbus Unit ID） |
| `name` | `string` | 设备名称（用户填写，展示/日志用） |
| `modelId` | `string` | 引用的设备模型 ID |
| `modelName` | `string` | 模型名称（冗余，仅展示用） |

### 2.4 接口映射

`gateway_settings` 记录同时保存应用配置和硬件映射；其中 `app.gateway.gw_id` 是网关唯一 ID，`app.gateway.sn` 是可选的网关硬件序列号，`app.gateway.location` 是可选的部署位置描述。

硬件映射定义面板丝印标签与实际设备节点的关系。首次创建数据库时，静态默认值来自 `internal/config/config.go`：

```yaml
Serial:        # 串口
  COM1: /dev/ttyS1
  COM2: /dev/ttyS2
```

前端配置时选择丝印标签（如 `COM1`），保存时由 `buildChannelConfig` 自动替换为真实节点（如 `/dev/ttyS1`）。通过 `GET /api/hardware` 接口从数据库读取。

### 2.5 REST 接口

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/channels` | 列出全部链路（按 `channel_index asc`） |
| `POST` | `/api/channels` | 新建/更新链路（含冲突检测），成功后触发**引擎热重载**。`id` 为空串视为新建：服务端分配通道索引并生成 `Channel-{索引}` 通道ID；更新时通道索引与通道ID 保留库中原值（不可修改） |
| `DELETE` | `/api/channels/{id}` | 删除链路（`id` 为字符串通道ID，如 `Channel-0`），成功后触发**引擎热重载** |
| `GET` | `/api/realtime?device={channelIndex}/{deviceIndex}` | 该设备的全部缓存实时值（通道索引从 0 开始） |
| `POST` | `/api/set` | 下发写值：`{channelIndex, deviceIndex, propName, value}` |
| `GET` | `/api/comm-monitor?channelIndex=&deviceIndex=&afterSeq=&limit=` | 通讯报文 + 会话统计（`limit` ≤ 1000） |
| `GET` | `/api/hardware` | 读取数据库中的接口映射 |
| `GET` | `/api/settings` | 读取完整网关设置 |
| `POST` | `/api/settings` | 保存完整网关设置 |
| `GET` | `/api/system-info` | 读取操作系统、系统时间和 Gateway 版本 |
| `POST` | `/api/restart` | 进程内重启运行资源（引擎和 NATS），HTTP 服务保持运行 |

#### 链路冲突检测

保存链路时，按链路类型计算资源唯一键：
- Serial → `serialName`
- Network → `deviceIp:devicePort`

若与其他链路冲突，返回 `409 Conflict`。

---

## 三、引擎内部映射（engine/converter）

设备模型的属性 JSON 在引擎层解析为 `PropMeta`，用于构建采集分组：

```go
type PropMeta struct {
    Name         string  // 属性名
    PropID       string  // 属性 ID
    DataType     string  // bool / int / float / string
    StartBit     int     // 起始位（bit 0 = 最低位）
    EndBit       int     // 终止位（含），须落在 RegisterCount × 16 位宽度内
    Offset       int     // 相对基址偏移
    RegisterBase int     // 寄存器基址
    RegisterCount int    // 寄存器数量（1~125，必填，由 buildDevicePlan 校验）
    ReadFC       int     // 读功能码
    WriteFC      int     // 写功能码
    Coefficient  float64 // 系数
    DeltaValue   float64 // 偏移量（可正负）
    ByteOrder    string  // 字节序
    AccessMode   string  // r / w / rw
}
```

JSON 字段名与前端属性定义的映射：

| PropMeta 字段 | JSON 字段（前端/存储） |
|---------------|------------------------|
| `StartBit` | `startBit` |
| `EndBit` | `endBit` |
| `Offset` | `registerOffset` |
| `RegisterBase` | `registerBase` |
| `RegisterCount` | `registerCount` |
| `ReadFC` | `readFunctionCode` |
| `WriteFC` | `writeFunctionCode` |
| `DeltaValue` | `deltaValue` |

### 采集分组规则

`BuildGroups` 将属性按 `(ReadFC, RegisterBase)` 分桶：
- 跳过 `accessMode` 不含 `r` 的属性
- 跳过 `ReadFC <= 0` 的属性（如虚拟属性"在线状态"）
- 同组 `Quantity = max(offset + registerCount)`，寄存器数量必填（1~125）
- 一次读请求获取组内所有属性，响应按 `offset × 2` 切片解析

### 工程量变换

```
读取：engineering = raw × coefficient + deltaValue
写入：raw = (engineering - deltaValue) / coefficient
```

> 位提取在工程量变换**之前**执行：先从寄存器字节中按 `[startBit, endBit]` 提取位段，再做系数与偏移量变换。

---

## 四、热重载机制

设备模型与链路通道的增删改均会触发**引擎热重载**（`reloadEngine`）：

1. 拉取全量 `channels` + `models`
2. `BuildPlans(channels, models)` 构建采集计划
3. `Engine.Apply(plans, models)` 差量应用

差量判断基于**配置指纹**（`fingerprint = sha1(type + config)`）：
- 指纹未变的链路保持运行，不受影响
- 指纹变化或新增的链路重启
- 被删除的链路停止

> 设备模型的热重载在近期补齐——此前模型变更后需重启或改链路才生效，现已修复。
