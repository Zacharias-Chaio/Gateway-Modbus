package engine

import (
	"sync"
	"time"
)

// EventSink 接收引擎产生的事件。实现方不得阻塞采集 worker。
type EventSink interface {
	PublishTelemetry(TelemetryEvent)
	PublishWriteResult(WriteResultEvent)
}

// TelemetryEvent 表示一个设备完成一轮采集后的完整遥测快照。
type TelemetryEvent struct {
	ChannelIndex int
	DeviceIndex  int
	DeviceName   string
	CommNo       int
	ModelID      string
	ModelName    string
	Online       bool
	Properties   map[string]TelemetryProperty
	Timestamp    time.Time
}

// TelemetryProperty is a single property in a telemetry snapshot, keyed by ID.
type TelemetryProperty struct {
	Index       int // 属性索引，与物模型属性列表中的序号一致
	Name        string
	Unit        string
	Description string
	AccessMode  string
	Value       any
	Timestamp   time.Time
}

// WriteResultEvent 表示一条写命令的最终执行结果。
type WriteResultEvent struct {
	RequestID    string
	ChannelIndex int
	DeviceIndex  int
	OK           bool
	Error        string
	Timestamp    time.Time
}

// AlarmPublisher 发布一条软件告警（契约见 docs/powerpulse-alarm-standard.md）。
// kind: trigger | recover | event；level: 0 提示 / 1 一般 / 2 严重 / 3 紧急。
// 实现方必须非阻塞。nil 安全：Alarmer 包装后调用点无需判空。
type AlarmPublisher func(code, component, message string, level int, kind string, context map[string]any)

// Publish 是 nil 安全的发布入口。
func (p AlarmPublisher) Publish(code, component, message string, level int, kind string, context map[string]any) {
	if p == nil {
		return
	}
	p(code, component, message, level, kind, context)
}

// 标准事件码与级别（与 docs/powerpulse-alarm-standard.md 第 6 节登记一致）。
const (
	alarmCodeLinkDisconnected = "gateway.link.disconnected"
	alarmCodeLinkRecovered    = "gateway.link.recovered"
	alarmCodeLinkDegraded     = "gateway.link.degraded"
	alarmCodeWriteFailed      = "gateway.write.failed"
	alarmCodeEnginePanic      = "gateway.engine.panic"
	alarmCodeNatsUnavailable  = "gateway.nats.unavailable"
)

// alarmKind 沿类型字面量。
const (
	alarmKindTrigger = "trigger"
	alarmKindRecover = "recover"
	alarmKindEvent   = "event"
)

// linkTracker 管理单链路断连告警的沿语义（契约 5.2/7 节）：
//   - 首次断连 → trigger（level 2）
//   - 持续断连且距上次广播超过窗口 → 聚合 event（携带 count）
//   - 恢复 → recover（level 1）；未触发过 trigger 时不发明造恢复沿
//
// 并发安全：worker 主循环单线程调用，但 stop/Status 并存，仍持锁保护。
type linkTracker struct {
	mu          sync.Mutex
	publisher   AlarmPublisher
	index       int    // 通道索引（context 中的 channel_index）
	name        string // 通道名称
	target      string // 连接目标
	active      bool   // 当前是否处于已广播 trigger 的断连状态
	broadcastAt time.Time
	window      time.Duration // 聚合广播窗口
	failures    int           // 窗口内累计失败次数（聚合 count）
}

func newLinkTracker(publisher AlarmPublisher, index int, name, target string) *linkTracker {
	return &linkTracker{
		publisher: publisher, index: index, name: name, target: target,
		window: 10 * time.Minute,
	}
}

// onDisconnect 记录一次连接失败，按沿语义决定是否发布。
func (t *linkTracker) onDisconnect(err string, attempt int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failures++
	now := time.Now()
	if !t.active {
		t.active = true
		t.broadcastAt = now
		t.publisher.Publish(alarmCodeLinkDisconnected, "engine",
			"链路连接失败", 2, alarmKindTrigger,
			map[string]any{
				"channel_index": t.index, "channel_name": t.name,
				"target": t.target, "err": err, "attempt": attempt,
			})
		return
	}
	if now.Sub(t.broadcastAt) >= t.window {
		t.broadcastAt = now
		count := t.failures
		t.publisher.Publish(alarmCodeLinkDisconnected, "engine",
			"链路持续断连", 2, alarmKindEvent,
			map[string]any{
				"channel_index": t.index, "channel_name": t.name,
				"target": t.target, "count": count,
			})
	}
}

// onRecover 连接成功：active 时发 recover 沿。
func (t *linkTracker) onRecover() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.active {
		return
	}
	t.active = false
	t.failures = 0
	t.publisher.Publish(alarmCodeLinkRecovered, "engine",
		"链路重连成功", 1, alarmKindRecover,
		map[string]any{
			"channel_index": t.index, "channel_name": t.name, "target": t.target,
		})
}
