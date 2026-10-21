package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"log/slog"
	"math"
	"sort"
	"sync"
	"time"

	"gateway/internal/engine/connector"
	"gateway/internal/logx"
)

// State 描述引擎的生命周期状态。
type State int32

const (
	StateStopped State = iota
	StateRunning
	StateStoppedFinal // 已 Stop，不再接受 Apply
)

func (s State) String() string {
	switch s {
	case StateRunning:
		return "running"
	case StateStoppedFinal:
		return "stopped"
	default:
		return "idle"
	}
}

// Engine 是链路的 supervisor：按配置差量启停 worker，支持热重载。
// 对外方法均为并发安全。
type Engine struct {
	ctx context.Context
	log *slog.Logger

	mu      sync.Mutex
	applyMu sync.Mutex      // 串行化调谐，避免新旧 worker 在重启期间重叠运行
	workers map[int]*worker // key: 通道索引（从 0 开始）
	sink    EventSink
	closed  bool // Stop 后置位，拒绝再启动 worker（避免僵尸 goroutine）

	// 调谐结果的可观测性：让「配置保存了但没生效」对运维可见。
	lastApplyAt  time.Time
	lastApplyErr string
	lastWarnings []string

	// useTestDriver 是测试钩子：让链路改用 driverFactory 构造驱动，
	// 从而在不触碰真实串口/网络的前提下验证启停、调度与错误处理。
	useTestDriver bool
}

// driverFactory 是驱动构造的间接点，默认委托给 connector.NewDriver。
// 单测通过它注入假驱动；生产路径始终使用默认值。
var driverFactory = func(cfg connector.Config) (connector.Driver, error) {
	return connector.NewDriver(cfg)
}

// SetDriverFactory 仅供测试/集成场景替换驱动构造。
func SetDriverFactory(f func(cfg connector.Config) (connector.Driver, error)) {
	if f == nil {
		driverFactory = func(cfg connector.Config) (connector.Driver, error) {
			return connector.NewDriver(cfg)
		}
		return
	}
	driverFactory = f
}

// New 创建 Engine。ctx 作为所有 worker 的父上下文，取消即触发全部链路关闭。
func New(ctx context.Context) *Engine {
	if ctx == nil {
		ctx = context.Background()
	}
	return &Engine{
		ctx:     ctx,
		log:     logx.Module("engine"),
		workers: make(map[int]*worker),
	}
}

// SetEventSink 设置采集与写入事件的非阻塞接收方。
// 应在 Apply 启动 worker 前调用。
func (e *Engine) SetEventSink(sink EventSink) {
	e.mu.Lock()
	e.sink = sink
	e.mu.Unlock()
}

// ApplyResult 汇总一次调谐的结果。
type ApplyResult struct {
	Started  []string `json:"started"`
	Stopped  []string `json:"stopped"`
	Restart  []string `json:"restarted"`
	Failed   []string `json:"failed"`
	Warnings []string `json:"warnings,omitempty"`
}

// Apply 以目标 plans 为期望状态，对运行中的 worker 做差量调谐：
//   - 期望有、当前无        → 启动
//   - 期望无、当前有        → 停止
//   - 两者都有但配置指纹变化 → 重启（先停后启）
//
// 可重复调用，用于配置保存 / 删除后的热重载。
// 若引擎已 Stop，则拒绝执行并返回错误（否则会在已取消的 ctx 上创建
// 立刻退出的僵尸 worker，它们仍占着 workers map 直到下一次 Apply）。
func (e *Engine) Apply(plans []ChannelPlan, warnings []string) error {
	e.applyMu.Lock()
	defer e.applyMu.Unlock()

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return fmt.Errorf("引擎已停止，忽略本次热重载")
	}
	e.mu.Unlock()

	desired := make(map[int]ChannelPlan, len(plans))
	for _, p := range plans {
		desired[p.ChannelIndex] = p
	}

	result := ApplyResult{Warnings: warnings}

	e.mu.Lock()
	toStop := make([]*worker, 0)
	for index, w := range e.workers {
		p, keep := desired[index]
		if !keep {
			e.log.Info("停止链路（已删除）", "channel", w.name, "index", index)
			toStop = append(toStop, w)
			delete(e.workers, index)
			result.Stopped = append(result.Stopped, w.name)
			continue
		}
		if planFingerprint(p) != w.fp {
			e.log.Info("重启链路（配置变更）", "channel", w.name, "index", index)
			toStop = append(toStop, w)
			delete(e.workers, index)
			result.Restart = append(result.Restart, w.name)
		}
	}
	e.mu.Unlock()

	for _, w := range toStop {
		w.stop()
	}

	e.mu.Lock()
	for index, p := range desired {
		if _, running := e.workers[index]; running {
			continue
		}
		if err := e.startChannel(p); err != nil {
			result.Failed = append(result.Failed, fmt.Sprintf("%s: %v", p.ChannelName, err))
			continue
		}
		result.Started = append(result.Started, p.ChannelName)
	}
	e.lastApplyAt = time.Now()
	e.lastApplyErr = ""
	if len(result.Failed) > 0 {
		e.lastApplyErr = fmt.Sprintf("%d 条链路启动失败", len(result.Failed))
	}
	e.lastWarnings = warnings
	e.mu.Unlock()

	return nil
}

// startChannel 解析配置、构造驱动并启动一个 worker。调用方须持有 e.mu。
func (e *Engine) startChannel(p ChannelPlan) error {
	cfg, err := connector.ParseConfig(p.ChannelType, p.Config)
	if err != nil {
		e.log.Error("链路配置解析失败，跳过", "channel", p.ChannelName, "index", p.ChannelIndex, "err", err)
		return fmt.Errorf("链路配置解析失败: %w", err)
	}
	drv, err := driverFactory(cfg)
	if err != nil {
		e.log.Error("链路驱动创建失败，跳过", "channel", p.ChannelName, "index", p.ChannelIndex, "type", p.ChannelType, "err", err)
		return fmt.Errorf("链路驱动创建失败: %w", err)
	}
	w := newWorker(p.ChannelIndex, p.ChannelName, planFingerprint(p), cfg, drv, p, e.sink)
	w.start(e.ctx)
	e.workers[p.ChannelIndex] = w

	valid := 0
	for i := range p.Devices {
		if p.Devices[i].Valid {
			valid++
		}
	}
	e.log.Info("启动链路", "channel", p.ChannelName, "index", p.ChannelIndex,
		"id", fmt.Sprintf("Channel-%d", p.ChannelIndex), "type", p.ChannelType,
		"target", cfg.Target(), "devices", len(p.Devices), "validDevices", valid,
		"pollInterval", cfg.PollDuration().String())
	return nil
}

// Submit 向指定链路投递一条写命令（非阻塞）。
// 通道索引不存在或队列已满时返回 false。
func (e *Engine) Submit(channelIndex int, cmd WriteCommand) bool {
	w := e.workerOf(channelIndex)
	if w == nil {
		return false
	}
	return w.tryWrite(cmd)
}

// HasDevice reports whether an active channel has a *collectable* mounted device.
func (e *Engine) HasDevice(channelIndex, deviceIndex int) bool {
	w := e.workerOf(channelIndex)
	if w == nil {
		return false
	}
	for i := range w.plan.Devices {
		if w.plan.Devices[i].Index == deviceIndex {
			return w.plan.Devices[i].Valid
		}
	}
	return false
}

// Connected reports whether an active channel is currently connected.
func (e *Engine) Connected(channelIndex int) bool {
	w := e.workerOf(channelIndex)
	return w != nil && w.state().Connected
}

// SessionEntry 是单个属性的实时值缓存条目（API 可见）。
type SessionEntry struct {
	Value     any       `json:"value"`     // 工程值
	Timestamp time.Time `json:"timestamp"` // 最后更新时间
}

// Values 返回指定链路的所有缓存实时值快照。
// 通道索引不存在返回 nil。
func (e *Engine) Values(channelIndex int) map[string]SessionEntry {
	w := e.workerOf(channelIndex)
	if w == nil {
		return nil
	}
	return w.getValues()
}

// CommunicationSnapshot returns the current communication-monitor session for
// a device, or for every device when deviceIndex is -1. The boolean is false
// when the channel has no active worker.
func (e *Engine) CommunicationSnapshot(channelIndex, deviceIndex int, afterSeq uint64, limit int) (CommunicationSnapshot, bool) {
	w := e.workerOf(channelIndex)
	if w == nil {
		return CommunicationSnapshot{}, false
	}
	return w.monitor.snapshot(deviceIndex, afterSeq, limit), true
}

// Stop 停止所有链路并等待其 goroutine 退出。
func (e *Engine) Stop() {
	e.applyMu.Lock()
	defer e.applyMu.Unlock()

	e.mu.Lock()
	e.closed = true
	workers := make([]*worker, 0, len(e.workers))
	for id, w := range e.workers {
		workers = append(workers, w)
		delete(e.workers, id)
	}
	e.mu.Unlock()
	for _, w := range workers {
		w.stop()
	}
	e.log.Info("引擎已停止，所有链路关闭")
}

// EngineStatus 是 /api/engine/status 的返回结构。
type EngineStatus struct {
	State        string       `json:"state"`
	Channels     []workerState `json:"channels"`
	LastApplyAt  string       `json:"lastApplyAt,omitempty"`
	LastApplyErr string       `json:"lastApplyErr,omitempty"`
	Warnings     []string     `json:"warnings,omitempty"`
}

// Status 返回全部链路的运行状态快照，按通道索引升序。
func (e *Engine) Status() EngineStatus {
	e.mu.Lock()
	out := make([]workerState, 0, len(e.workers))
	for _, w := range e.workers {
		out = append(out, w.state())
	}
	status := EngineStatus{
		State:        e.state().String(),
		LastApplyErr: e.lastApplyErr,
		Warnings:     e.lastWarnings,
	}
	if !e.lastApplyAt.IsZero() {
		status.LastApplyAt = e.lastApplyAt.Format(time.RFC3339)
	}
	e.mu.Unlock()

	sort.Slice(out, func(i, j int) bool { return out[i].ChannelIndex < out[j].ChannelIndex })
	status.Channels = out
	return status
}

func (e *Engine) state() State {
	if e.closed {
		return StateStoppedFinal
	}
	return StateRunning
}

// Healthy 返回引擎整体是否处于「可服务」状态：至少一条链路已连接。
// 供 /readyz 探针使用——端口能监听但引擎全挂时不应报告健康。
func (e *Engine) Healthy() bool {
	e.mu.Lock()
	workers := make([]*worker, 0, len(e.workers))
	for _, w := range e.workers {
		workers = append(workers, w)
	}
	e.mu.Unlock()
	for _, w := range workers {
		if w.state().Connected {
			return true
		}
	}
	return false
}

// workerOf 返回指定通道的 worker（无则 nil）。调用方不得持有返回值的锁。
func (e *Engine) workerOf(channelIndex int) *worker {
	e.mu.Lock()
	w := e.workers[channelIndex]
	e.mu.Unlock()
	return w
}

// planFingerprint 计算采集计划的配置指纹：
// 链路类型 + 传输配置 + 每个设备的序号/协议/分组/属性元数据摘要。
// 指纹不变则热重载时无需重启，避免无谓抖动。
func planFingerprint(p ChannelPlan) string {
	h := sha256.New()
	writeStr(h, p.ChannelType)
	writeStr(h, p.ChannelName)

	// 轮询间隔
	writeInt(h, p.PollMs)

	// 传输层配置（IP/端口/串口参数/超时等），变化时必须重启
	h.Write(p.Config)
	h.Write([]byte{0})

	// 设备列表：必须包含 Index 与 Valid，否则「模型缺失」这类变更不会触发重启，
	// 旧 worker 会继续按旧计划采集，而 API 已按新配置返回拓扑。
	for _, dev := range p.Devices {
		writeInt(h, dev.Index)
		writeStr(h, dev.Protocol)
		h.Write([]byte{dev.UnitID})
		writeStr(h, dev.DisplayName())
		writeStr(h, dev.ModelID)
		if dev.Valid {
			h.Write([]byte{1})
		} else {
			h.Write([]byte{0})
			writeStr(h, dev.ErrMsg)
		}

		// 分组指纹：功能码 + 起始地址 + 数量 + 地址空间
		for _, g := range dev.Groups {
			h.Write([]byte{byte(g.ReadFC)})
			writeUint16(h, g.StartAddr)
			writeUint16(h, g.Quantity)
			h.Write([]byte{byte(g.AddrSpace)})
		}
		h.Write([]byte{0xFF})

		// 属性元数据指纹：只哈希影响采集/写操作结果的字段。
		for _, prop := range dev.Props {
			writeStr(h, prop.Name)
			writeStr(h, prop.PropID)
			writeStr(h, prop.DataType)
			writeUint16(h, prop.StartBit)
			writeUint16(h, prop.EndBit)
			writeUint16(h, prop.Offset)
			writeUint16(h, prop.RegisterBase)
			writeUint16(h, prop.RegisterCount)
			h.Write([]byte{byte(prop.ReadFC), byte(prop.WriteFC)})
			int64Bits(prop.Coefficient, h)
			int64Bits(prop.DeltaValue, h)
			writeStr(h, prop.ByteOrder)
			writeStr(h, prop.AccessMode)
			writeStr(h, prop.Unit)
			if prop.Signed != nil && *prop.Signed {
				h.Write([]byte{1})
			} else {
				h.Write([]byte{0})
			}
			h.Write([]byte{0xFD})
		}
		h.Write([]byte{0xFE})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeStr(h hash.Hash, s string) {
	h.Write([]byte(s))
	h.Write([]byte{0})
}

func writeInt(h hash.Hash, v int) {
	h.Write([]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

func writeUint16(h hash.Hash, v int) {
	h.Write([]byte{byte(v >> 8), byte(v)})
}

// int64Bits 将 float64 的 IEEE 754 二进制表示写入指纹哈希，
// 保证浮点值的精确匹配（避免字符串格式化的精度丢失）。
func int64Bits(f float64, h hash.Hash) {
	bits := math.Float64bits(f)
	for i := 7; i >= 0; i-- {
		h.Write([]byte{byte(bits >> (i * 8))})
	}
}
