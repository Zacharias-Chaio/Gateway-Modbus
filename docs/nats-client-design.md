## NATS 客户端设计

本文档阐述网关侧 NATS 客户端的设计：配置、生命周期、连接与重连策略、降级行为等。

主题定义、消息信封与消息载体格式等**消息契约**内容，参见 [message-contract-design.md](message-contract-design.md)。

### 1. 客户端配置

NATS 配置保存在 SQLite 的 `gateway_settings` 中，通过 Web 的“网关设置”页面维护；以下结构仅表示字段与静态默认值参考。

```yaml
# 数据出口：NATS 发布配置
gateway:
  gw_id: "gw-001"               # 网关唯一 ID，用于 NATS 主题
  sn: "GW-20260908-001"         # 网关硬件序列号
  location: "A 厂区 1 号配电室"   # 网关部署位置

nats:
  enabled: false             # 总开关，默认关闭
  url: nats://127.0.0.1:4222 # 连接串，支持集群逗号分隔 / token / user:pass
  name: gateway              # 连接名，服务端监控可见

  # 发送策略
  queueSize: 4096            # 内部缓冲（待发布消息数），满则丢弃最旧并告警

  # 重连策略
  connectTimeout: 2000        # 每次建连的 TCP/TLS 握手超时（毫秒）
  reconnectWait: 2000         # 重连间隔（毫秒）
  maxReconnects: -1           # 最大重连次数，-1 表示无限重连
  retryOnFailedConnect: true  # nats.go 建连失败时允许重试
  reconnectBufSize: 8388608   # 断连期间 nats.go 内存缓冲（字节，默认 8MB），重连成功后自动补发

  # 连接保活
  pingInterval: 20000         # 心跳间隔（毫秒）：NATS 是长连接，半开连接靠心跳发现
  maxPingsOut: 3              # 连续 N 次心跳无响应 → 判定断开，触发重连

  # nats 主题前缀（主题契约详见 message-contract-design.md）
  subjectPrefix: "powerpulse.gateway"
  # 实际使用：
  #   数据发布 → {subjectPrefix}.{gatewayID}.data
  #   控制接收 → {subjectPrefix}.{gatewayID}.cmd
  #   查询接口 → {subjectPrefix}.{gatewayID}.query
```

### 2. 启动与降级行为

- 仅当 `nats.enabled: true` 时创建 NATS 客户端；默认配置不启用北向数据功能。
- 连接、订阅或订阅初始化超时失败时，网关记录警告但不退出，继续提供链路引擎与 Web 配置服务。
- 初始化失败时不会安装引擎事件出口：不发布 `.data` 数据，也不订阅 `.cmd` 或 `.query` 主题。
- 此降级实例不在进程内重新创建 NATS 客户端；NATS 服务恢复后需要重启网关。

### 3. 客户端职责与消息流

客户端自身不定义消息内容，只负责按契约收发：

- **上行发布**：引擎完成一轮设备采集后，客户端按消息契约封装 `MessageData`，发布到 `{subjectPrefix}.{gw_id}.data`；
- **下行控制**：客户端订阅 `{subjectPrefix}.{gw_id}.cmd`，收到 `MessageCmd` 后先做路由校验，再投递到引擎写队列，并通过 REQ/REP 立即回复 `MessageAck`（accepted / failure）；
- **拓扑查询**：客户端订阅 `{subjectPrefix}.{gw_id}.query`，处理 `queryTopology` 请求并返回全量拓扑。

消息信封（Header + Data）的封装/解封装逻辑、各载体结构体的字段定义，见 [message-contract-design.md](message-contract-design.md)。
