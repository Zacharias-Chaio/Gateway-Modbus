package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"gateway/internal/engine/connector"
	"gateway/internal/engine/converter"
	"gateway/internal/logx"
)

// workerState 描述一条链路 worker 的运行状态。
type workerState struct {
	ChannelIndex int    `json:"channelIndex"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	Target       string `json:"target"`
	Connected    bool   `json:"connected"`
	LastError    string `json:"lastError,omitempty"`

	// 以下为运维可见性字段：让「实际刷新周期」与「设备级健康」不再只能靠猜。
	State             string        `json:"state"` // connecting / online / reconnecting / fatal
	ReconnectAttempts int           `json:"reconnectAttempts"`
	EffectivePollMs   int64         `json:"effectivePollMs"`         // 每个设备实际轮询一轮的毫秒数（估算）
	LastPollAt        time.Time     `json:"lastPollAt,omitempty"`    // 最近一次成功轮询时间
	LastSuccessAt     time.Time     `json:"lastSuccessAt,omitempty"` // 最近一次成功通讯时间
	Devices           []deviceState `json:"devices"`
}

// deviceState 是单设备的运行状态。
type deviceState struct {
	Index      int       `json:"index"`
	Name       string    `json:"name"`
	Valid      bool      `json:"valid"`
	Error      string    `json:"error,omitempty"`
	Online     bool      `json:"online"`
	ConsecFail int       `json:"consecutiveFailures"`
	LastPollMs int64     `json:"lastPollMs,omitempty"`
	LastPollAt time.Time `json:"lastPollAt,omitempty"`
}

// worker 承载一条链路：独占一个 Driver，在自己的 goroutine 中管理连接生命周期
// 与协议轮询、写命令下发。
type worker struct {
	index int // 通道索引（从 0 开始）
	name  string
	fp    string // 配置指纹，用于热重载时判断是否需要重启
	cfg   connector.Config
	drv   connector.Driver
	plan  ChannelPlan

	log    *slog.Logger
	cancel context.CancelFunc
	done   chan struct{}

	// 写命令优先级队列（非阻塞写入，worker 侧优先消费）。
	writeCh chan WriteCommand

	mu          sync.Mutex
	connected   bool
	lastErr     string
	phase       string
	reconnects  int
	lastSuccess time.Time
	devStates   map[int]*deviceState

	// session 值缓存：key = "deviceIndex/propName"
	sess sync.RWMutex
	data map[string]SessionEntry

	monitor *commMonitor
	sink    EventSink

	// 属性缺失 ID 的告警只打一次，避免 2Hz × N 属性刷屏。
	warnedNoPropID sync.Map
}

// newWorker 构造 worker，此时尚未启动 goroutine。
func newWorker(index int, name, fp string, cfg connector.Config, drv connector.Driver, plan ChannelPlan, sink EventSink) *worker {
	devStates := make(map[int]*deviceState, len(plan.Devices))
	for i := range plan.Devices {
		d := &plan.Devices[i]
		devStates[d.Index] = &deviceState{Index: d.Index, Name: d.DisplayName(), Valid: d.Valid, Error: d.ErrMsg}
	}
	return &worker{
		index:     index,
		name:      name,
		fp:        fp,
		cfg:       cfg,
		drv:       drv,
		plan:      plan,
		log:       logx.Module("engine"),
		done:      make(chan struct{}),
		writeCh:   make(chan WriteCommand, 32),
		data:      make(map[string]SessionEntry),
		devStates: devStates,
		monitor:   newCommMonitor(index, defaultCommEventCapacity),
		sink:      sink,
		phase:     "idle",
	}
}

// start 启动链路 goroutine，用 parent 派生的 ctx 控制生命周期。
func (w *worker) start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	w.cancel = cancel
	go w.run(ctx)
}

// stop 请求停止并等待 goroutine 退出。关闭底层连接以立即唤醒阻塞的 Receive。
func (w *worker) stop() {
	if w.cancel != nil {
		w.cancel()
	}
	if err := w.drv.Close(); err != nil {
		w.log.Warn("停止链路时关闭连接失败", "channel", w.name, "err", err)
	}
	<-w.done
}

// tryWrite 尝试非阻塞地投递一条写命令。
// 队列满时返回 false，调用方应自行处理超时。
func (w *worker) tryWrite(cmd WriteCommand) bool {
	select {
	case w.writeCh <- cmd:
		return true
	default:
		return false
	}
}

// getValues 返回所有缓存值快照（供 API realtime 查询）。
func (w *worker) getValues() map[string]SessionEntry {
	w.sess.RLock()
	defer w.sess.RUnlock()
	out := make(map[string]SessionEntry, len(w.data))
	for k, v := range w.data {
		out[k] = v
	}
	return out
}

// deviceByIndex 按设备序号定位设备计划（序号即配置中的挂载序号，不做数组下标假设）。
func (w *worker) deviceByIndex(index int) *DevicePlan {
	for i := range w.plan.Devices {
		if w.plan.Devices[i].Index == index {
			return &w.plan.Devices[i]
		}
	}
	return nil
}

// publishTelemetry 构造当前设备的一轮遥测快照，交给外部数据出口。
func (w *worker) publishTelemetry(dev *DevicePlan, online bool) {
	if w.sink == nil || dev == nil {
		return
	}
	now := time.Now()
	properties := make(map[string]TelemetryProperty)

	if online {
		w.sess.RLock()
		for _, prop := range dev.Props {
			if prop.PropID == "" {
				// 构建期已校验，这里只是兜底；同一条告警只打一次。
				if _, loaded := w.warnedNoPropID.LoadOrStore(w.name+"|"+prop.Name, struct{}{}); !loaded {
					w.log.Warn("属性缺少 ID，跳过遥测发布", "channel", w.name, "device", dev.DisplayName(), "prop", prop.Name)
				}
				continue
			}
			if value, ok := w.data[cacheKey(dev.Index, prop.PropID)]; ok {
				properties[prop.PropID] = TelemetryProperty{Index: prop.Index, Name: prop.Name, Unit: prop.Unit, Description: prop.Description, AccessMode: prop.AccessMode, Value: value.Value, Timestamp: value.Timestamp}
			} else {
				properties[prop.PropID] = TelemetryProperty{Index: prop.Index, Name: prop.Name, Unit: prop.Unit, Description: prop.Description, AccessMode: prop.AccessMode, Value: nil, Timestamp: now}
			}
		}
		w.sess.RUnlock()
		w.addOnlineTelemetry(properties, dev, int64(1), now)
	} else {
		w.addOnlineTelemetry(properties, dev, int64(0), now)
	}

	w.sink.PublishTelemetry(TelemetryEvent{
		ChannelIndex: w.index, DeviceIndex: dev.Index, DeviceName: dev.DisplayName(),
		CommNo: int(dev.UnitID), ModelID: dev.ModelID, ModelName: dev.ModelName,
		Online: online, Properties: properties, Timestamp: now,
	})
}

// addOnlineTelemetry 把设备在线状态写入本轮遥测快照。
// 在线点按属性 ID 识别（内置在线点 ID 为 "online"），属性名仅作兜底兼容——
// 属性名允许重复、也允许用户改名，不能作为唯一依据。
func (w *worker) addOnlineTelemetry(properties map[string]TelemetryProperty, dev *DevicePlan, value int64, timestamp time.Time) {
	for _, prop := range dev.Props {
		if prop.PropID != OnlinePropID && prop.Name != OnlinePropName {
			continue
		}
		if prop.PropID == "" {
			if _, loaded := w.warnedNoPropID.LoadOrStore(w.name+"|online", struct{}{}); !loaded {
				w.log.Warn("在线状态属性缺少 ID，跳过遥测发布", "channel", w.name, "device", dev.DisplayName())
			}
			return
		}
		properties[prop.PropID] = TelemetryProperty{Index: prop.Index, Name: prop.Name, Unit: prop.Unit, Description: prop.Description, AccessMode: prop.AccessMode, Value: value, Timestamp: timestamp}
		return
	}
}

// publishWriteResult 将异步写命令的最终结果交给外部数据出口。
func (w *worker) publishWriteResult(cmd WriteCommand, result WriteResult) {
	// HTTP 入口：通过 ResultCh 回传（非阻塞）。
	if cmd.ResultCh != nil {
		select {
		case cmd.ResultCh <- result:
		default:
		}
	}
	// 消息总线入口：通过 RequestID 回执。
	if w.sink == nil || cmd.RequestID == "" {
		return
	}
	event := WriteResultEvent{
		RequestID: cmd.RequestID, ChannelIndex: w.index, DeviceIndex: cmd.DeviceIndex,
		OK: result.OK, Timestamp: time.Now(),
	}
	if !result.OK {
		event.Error = result.Err
	}
	w.sink.PublishWriteResult(event)
}

// errLinkStopped 表示 worker 已退出，队列中的写命令不会被执行。
var errLinkStopped = errors.New("链路已停止，写命令未执行")

// drainPendingWrites 在 worker 退出前排空写命令队列：
// 对带 RequestID / ResultCh 的命令回执失败，避免调用方永远等不到结果。
func (w *worker) drainPendingWrites() {
	for {
		select {
		case cmd := <-w.writeCh:
			w.publishWriteResult(cmd, WriteResult{OK: false, Err: errLinkStopped.Error()})
		default:
			return
		}
	}
}

// run 是链路主循环：连接（失败后按 reconnectRetries 策略重连）→ 采集循环 → ctx 取消则关闭退出。
func (w *worker) run(ctx context.Context) {
	defer close(w.done)
	// 兜底 recover：编码/映射层若仍有 panic，不能让整个网关进程崩溃。
	// 此前 worker goroutine 内没有任何 recover，HTTP 侧的 Recoverer 管不到它。
	defer func() {
		if r := recover(); r != nil {
			w.log.Error("链路 goroutine panic，已隔离", "channel", w.name, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	defer func() {
		if err := w.drv.Close(); err != nil {
			w.log.Warn("关闭链路失败", "channel", w.name, "err", err)
		}
		w.setConnected(false, "")
		w.setPhase("stopped")
	}()
	// LIFO：drain 最先执行，保证 stop() 等到 done 关闭时回执已全部发出。
	defer w.drainPendingWrites()

	connectAttempts := 0 // 已尝试的连接次数

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		w.setPhase("connecting")
		if err := w.drv.Open(ctx); err != nil {
			connectAttempts++
			w.setConnected(false, err.Error())
			w.setReconnects(connectAttempts)
			w.markAllOffline()
			w.publishAllOffline(false)

			// reconnectRetries == 0 表示无限重连；> 0 表示达到次数后转为慢速探测。
			maxRetry := w.cfg.ReconnectRetries
			if maxRetry > 0 && connectAttempts > maxRetry {
				w.log.Error("链路连接失败次数已达上限，转入慢速探测",
					"channel", w.name, "target", w.cfg.Target(),
					"attempts", connectAttempts, "max", maxRetry)
				w.setPhase("fatal")
				// 慢速探测：不再永久放弃，而是低频重试 + 低频心跳，
				// 避免「10 条链路全断时每 500ms 向 NATS 洪泛离线报文」。
				if !w.slowProbe(ctx) {
					return
				}
				connectAttempts = 0
				continue
			}

			retryIn := time.Duration(connector.DefaultReconnectIntervalMs) * time.Millisecond
			w.log.Warn("链路连接失败，稍后重试",
				"channel", w.name, "target", w.cfg.Target(), "err", err,
				"attempt", connectAttempts, "retryIn", retryIn.String())
			w.setPhase("reconnecting")
			if !w.waitReconnect(ctx, retryIn) {
				return
			}
			continue
		}

		// 连接成功，重置计数
		connectAttempts = 0
		w.setConnected(true, "")
		w.setReconnects(0)
		w.setPhase("online")
		w.monitor.reset(time.Now())
		w.log.Info("链路已连接，开始采集",
			"channel", w.name, "type", w.cfg.Type, "target", w.cfg.Target(),
			"devices", len(w.plan.Devices), "pollInterval", w.cfg.PollDuration().String())

		// 连接成功后进入采集循环；返回非 nil 表示需重连。
		if err := w.collectLoop(ctx); err != nil {
			w.setConnected(false, err.Error())
			w.setPhase("reconnecting")
			w.markAllOffline()
			w.publishAllOffline(false)
			if !w.waitReconnect(ctx, time.Duration(connector.DefaultReconnectIntervalMs)*time.Millisecond) {
				return
			}
			continue
		}
		return // ctx 取消，正常退出
	}
}

// collectLoop 是采集主循环，在连接已建立的前提下运行。
//
// 调度语义：每个设备独立计时，到点就轮询，互不阻塞。
// 此前是「一个 tick 只轮询一个设备」，于是 N 个设备时单个设备的实际周期
// 变成 N×pollInterval——README 说的「500ms 刷新」在 10 设备链路上其实是 5s。
func (w *worker) collectLoop(ctx context.Context) error {
	pollInterval := w.cfg.PollDuration()

	// 只调度可采集的设备
	targets := make([]*DevicePlan, 0, len(w.plan.Devices))
	for i := range w.plan.Devices {
		if w.plan.Devices[i].Valid {
			targets = append(targets, &w.plan.Devices[i])
		}
	}
	if len(targets) == 0 {
		// 把每个无效设备的具体原因一并打出来。以前只报「全部配置无效」，
		// 真正的原因（模型缺失 / 属性重名 / 地址越界…）藏在另一条告警里，
		// 运维只看这两行日志无从下手。
		reasons := make([]string, 0, len(w.plan.Devices))
		for i := range w.plan.Devices {
			d := &w.plan.Devices[i]
			reason := d.ErrMsg
			if reason == "" {
				reason = "未知原因"
			}
			reasons = append(reasons, fmt.Sprintf("#%d(%s): %s", d.Index, d.DisplayName(), reason))
		}
		w.log.Warn("链路没有可采集的设备（全部挂载项配置无效），等待配置修复",
			"channel", w.name, "devices", len(w.plan.Devices), "reasons", strings.Join(reasons, "；"))
		// 保持在线状态但空转：配置修好后热重载会重建 worker。
		<-ctx.Done()
		return nil
	}

	nextDue := make(map[int]time.Time, len(targets))
	now := time.Now()
	for _, dev := range targets {
		nextDue[dev.Index] = now
	}

	// 用较细的调度粒度（不超过 50ms），避免多设备时引用单一大 ticker 造成抖动。
	granularity := pollInterval
	if granularity > 50*time.Millisecond {
		granularity = 50 * time.Millisecond
	}
	if granularity < 5*time.Millisecond {
		granularity = 5 * time.Millisecond
	}
	ticker := time.NewTicker(granularity)
	defer ticker.Stop()

	for {
		// 写命令优先：一次排空所有待执行写命令
		if stop, err := w.drainWrites(ctx); stop {
			return err
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		// 找出所有到期的设备，按 nextDue 升序执行
		now = time.Now()
		due := make([]*DevicePlan, 0, len(targets))
		for _, dev := range targets {
			if !now.Before(nextDue[dev.Index]) {
				due = append(due, dev)
			}
		}
		sort.Slice(due, func(i, j int) bool {
			return nextDue[due[i].Index].Before(nextDue[due[j].Index])
		})

		for _, dev := range due {
			if err := w.pollDevice(ctx, dev); err != nil {
				w.log.Warn("设备轮询失败", "channel", w.name, "device", dev.DisplayName(), "err", err)
				w.setDeviceOffline(dev, err)
				w.publishTelemetry(dev, false)
				if isLinkError(err) {
					return err
				}
			} else {
				w.setDeviceOnline(dev)
				w.publishTelemetry(dev, true)
			}
			// 半双工总线：设备之间也要留帧间隔
			if !sleepCtx(ctx, w.cfg.FrameGap()) {
				return nil
			}
		}

		// 重新排期：成功的按 pollInterval，失败的做退避（避免坏设备打满总线）
		now = time.Now()
		for _, dev := range due {
			nextDue[dev.Index] = now.Add(w.nextDelay(dev, pollInterval))
		}
	}
}

// nextDelay 返回该设备下一轮轮询的延迟。连续失败时指数退避，上限 30s。
func (w *worker) nextDelay(dev *DevicePlan, base time.Duration) time.Duration {
	w.mu.Lock()
	st := w.devStates[dev.Index]
	fails := 0
	if st != nil {
		fails = st.ConsecFail
	}
	w.mu.Unlock()

	if fails <= 0 {
		return base
	}
	delay := base
	for i := 1; i < fails && delay < 30*time.Second; i++ {
		delay *= 2
	}
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return delay
}

// drainWrites 排空写命令队列。返回 stop=true 表示需要退出采集循环。
func (w *worker) drainWrites(ctx context.Context) (bool, error) {
	for {
		select {
		case cmd := <-w.writeCh:
			res := w.execWrite(ctx, cmd)
			if !res.OK {
				w.log.Warn("写命令执行失败", "channel", w.name, "prop", cmd.PropName, "err", res.Err)
			}
			w.publishWriteResult(cmd, res)
			if isLinkError(errors.New(res.Err)) {
				return true, errors.New(res.Err)
			}
		default:
			return false, nil
		}
	}
}

// pollDuration 返回轮询间隔。
func (w *worker) pollDuration() time.Duration { return w.cfg.PollDuration() }

// pollDevice 轮询单个设备：遍历其所有寄存器分组，逐组发送读请求。
//
// 单组失败不再中止整设备：以前某组基址非法（导致异常码）会让后续所有分组
// 都不再被采集，一个配置错误就静默掉一半属性。
func (w *worker) pollDevice(ctx context.Context, dev *DevicePlan) error {
	if !dev.Valid {
		return nil // 占位设备：跳过，但保留序号
	}
	var firstErr error
	for gi := range dev.Groups {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		if err := w.pollOne(ctx, dev, gi); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			// 链路断开：立刻返回触发重连
			if isLinkError(err) {
				return err
			}
			// 确定性错误（如异常码 02 非法地址）：继续轮询其它分组
			continue
		}
		if !sleepCtx(ctx, w.cfg.FrameGap()) {
			return nil
		}
	}
	return firstErr
}

// pollOne 执行一次寄存器/位组读取（含重发与错误分类逻辑）。
func (w *worker) pollOne(ctx context.Context, dev *DevicePlan, gi int) error {
	g := dev.Groups[gi]
	deviceIndex := dev.Index
	transactionStarted := time.Now()

	req, tid, err := dev.Conv.EncodeRead(dev.UnitID, g.ReadFC, g.StartAddr, g.Quantity)
	if err != nil {
		return err
	}

	maxAttempts := w.cfg.ResendRetries + 1 // resendRetries=0 → 仅发 1 次
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		w.logTX(dev, "read", attempt, req)
		if _, err := w.drv.Send(req); err != nil {
			lastErr = err
			if isLinkError(err) {
				w.monitor.complete(deviceIndex, dev.UnitID, "read", attempt, err, time.Since(transactionStarted))
				return err
			}
			w.log.Debug("读请求发送失败", "channel", w.name, "device", dev.DisplayName(), "attempt", attempt, "err", err)
			continue
		}

		if !sleepCtx(ctx, w.cfg.FrameGap()) {
			return ctx.Err()
		}

		raw, err := w.readFrame(ctx, dev, byte(g.ReadFC), g.Quantity, tid, "read", attempt, time.Now())
		if err != nil {
			lastErr = err
			if isLinkError(err) {
				w.monitor.complete(deviceIndex, dev.UnitID, "read", attempt, err, time.Since(transactionStarted))
				return err
			}
			// 确定性错误（异常码）或非法配置：重发无意义，立即返回
			if converter.IsPermanent(err) {
				w.monitor.complete(deviceIndex, dev.UnitID, "read", attempt, err, time.Since(transactionStarted))
				return err
			}
			// 错帧后残留字节会污染下一轮：先清空链路缓冲再重发
			if converter.NeedsResync(err) {
				if rerr := w.drv.Refresh(); rerr != nil && isLinkError(rerr) {
					w.monitor.complete(deviceIndex, dev.UnitID, "read", attempt, rerr, time.Since(transactionStarted))
					return rerr
				}
			}
			w.log.Debug("读响应解析失败", "channel", w.name, "device", dev.DisplayName(), "attempt", attempt, "err", err)
			continue
		}

		// 解析各属性值并写入 session 缓存
		w.applyMembers(dev, g, raw)

		w.monitor.complete(deviceIndex, dev.UnitID, "read", attempt, nil, time.Since(transactionStarted))
		return nil
	}

	if lastErr != nil {
		w.monitor.complete(deviceIndex, dev.UnitID, "read", maxAttempts, lastErr, time.Since(transactionStarted))
		w.log.Warn("读请求重发耗尽",
			"channel", w.name, "device", dev.DisplayName(),
			"startAddr", g.StartAddr, "quantity", g.Quantity, "fc", g.ReadFC,
			"attempts", maxAttempts, "lastErr", lastErr)
		return lastErr
	}
	err = errors.New("读请求失败")
	w.monitor.complete(deviceIndex, dev.UnitID, "read", maxAttempts, err, time.Since(transactionStarted))
	return err
}

// applyMembers 把响应裸字节映射为各属性的工程值并写入缓存。
//
// 映射失败不再静默 continue：以前字节序/位区间配错时，属性既不入缓存、
// 也不产生任何错误与日志，界面上只是「一直没数据」，而通讯正确率显示 100%。
func (w *worker) applyMembers(dev *DevicePlan, g converter.RegGroup, raw []byte) {
	for _, m := range g.Members {
		if m.ByteOffset < 0 || len(raw) < m.ByteOffset+m.ByteLen {
			w.logMapError(dev, g, m, fmt.Errorf("响应字节不足：需要 %d~%d，实际 %d", m.ByteOffset, m.ByteOffset+m.ByteLen, len(raw)))
			continue
		}
		chunk := raw[m.ByteOffset : m.ByteOffset+m.ByteLen]
		// 位空间需要知道属性起始位在切片内的偏移
		bitInChunk := 0
		if m.Prop.IsBitSpace() {
			bitInChunk = 0 // ReadByteRange 已按分组起点对齐，切片首字节即属性起始字节
		}
		val, err := converter.MapRegistersAt(chunk, m.Prop, bitInChunk)
		if err != nil {
			w.logMapError(dev, g, m, err)
			continue
		}
		// 缓存键用属性 ID 而不是属性名：属性名允许重复（现场常见同名点位
		// 分布在不同数据块），用名字做键会让同名属性互相覆盖数据。
		key := cacheKey(dev.Index, m.Prop.CacheKey())
		w.sess.Lock()
		w.data[key] = SessionEntry{Value: val, Timestamp: time.Now()}
		w.sess.Unlock()

		w.log.Debug("采集成功", "channel", w.name, "device", dev.DisplayName(), "prop", m.Prop.Name, "value", val)
	}
}

// logMapError 记录映射失败（限频），并把失败计入通讯监控。
func (w *worker) logMapError(dev *DevicePlan, g converter.RegGroup, m converter.GroupMember, err error) {
	w.monitor.complete(dev.Index, dev.UnitID, "read", 1, fmt.Errorf("属性 %q 映射失败: %w", m.Prop.Name, err), 0)
	if _, loaded := w.warnedNoPropID.LoadOrStore("map|"+w.name+"|"+m.Prop.Name, struct{}{}); !loaded {
		w.log.Warn("属性映射失败（该属性将不产生数据）",
			"channel", w.name, "device", dev.DisplayName(), "prop", m.Prop.Name,
			"fc", g.ReadFC, "byteOffset", m.ByteOffset, "byteLen", m.ByteLen,
			"dataType", m.Prop.DataType, "byteOrder", m.Prop.ByteOrder, "err", err)
	}
}

// receiveFrame 是读帧的通用实现，readFrame 与写响应共用。
//
// 以前读与写各写了一份渐进式累积逻辑，结果两份行为已经分叉
// （写路径的超时判定条件是反的）。
func (w *worker) receiveFrame(ctx context.Context, tmpSize int, decode func(buf []byte) (bool, error)) ([]byte, error) {
	deadline := time.Now().Add(w.cfg.FrameTimeout())
	readTimeout := w.cfg.ReadTimeout()
	var buf []byte

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return buf, ctx.Err()
		default:
		}

		tmp := make([]byte, tmpSize)
		n, err := w.drv.Receive(tmp, readTimeout)
		if err != nil {
			if isTimeout(err) {
				continue
			}
			return buf, err
		}
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		} else {
			continue
		}

		done, derr := decode(buf)
		if derr != nil {
			if converter.IsShortFrame(derr) {
				continue // 继续累积
			}
			return buf, derr
		}
		if done {
			return buf, nil
		}
	}
	return buf, errFrameTimeout
}

var errFrameTimeout = errors.New("响应帧超时")

// readFrame 渐进式读取一帧读响应。
func (w *worker) readFrame(ctx context.Context, dev *DevicePlan, fc byte, quantity int, tid uint16, operation string, attempt int, sentAt time.Time) ([]byte, error) {
	var data []byte
	buf, err := w.receiveFrame(ctx, 256, func(b []byte) (bool, error) {
		d, derr := dev.Conv.DecodeRead(b, tid, dev.UnitID, fc, quantity)
		if derr != nil {
			return false, derr
		}
		data = d
		return true, nil
	})
	if err != nil {
		if errors.Is(err, errFrameTimeout) {
			return nil, fmt.Errorf("%w（FC=0x%02X qty=%d，%s 内未收齐）", err, fc, quantity, w.cfg.FrameTimeout())
		}
		return nil, err
	}
	w.logRX(dev, operation, attempt, buf, time.Since(sentAt))
	return data, nil
}

// execWrite 执行一条写命令（含重发与多帧拆分）。
//
// 返回 WriteResult 而不是 error：需要把「写了几帧」也带回给调用方，
// 便于 FC06 逐寄存器拆分写入时观察实际效果。
func (w *worker) execWrite(ctx context.Context, cmd WriteCommand) WriteResult {
	dev := w.deviceByIndex(cmd.DeviceIndex)
	if dev == nil || !dev.Valid {
		return WriteResult{OK: false, Err: "设备序号不存在或不可采集"}
	}
	transactionStarted := time.Now()

	prop, err := converter.FindWriteProp(dev.Props, cmd.PropName)
	if err != nil {
		return WriteResult{OK: false, Err: err.Error()}
	}

	// 逆变换：engineering = raw×coef+delta → raw = (engineering - delta) / coef
	coef := prop.Coefficient
	if coef == 0 {
		coef = 1
	}
	rawVal := (cmd.RawValue - prop.DeltaValue) / coef

	startAddr := prop.RegisterBase + prop.Offset
	plan, err := converter.BuildWritePlan(prop, rawVal, startAddr)
	if err != nil {
		return WriteResult{OK: false, Err: err.Error()}
	}

	applied := 0
	for _, frame := range plan.Frames {
		if err := w.writeFrame(ctx, dev, prop, plan.Mode, frame, transactionStarted); err != nil {
			return WriteResult{OK: false, Err: err.Error(), Applied: applied}
		}
		applied++
		// 多帧写入（FC06 拆分）之间保留帧间隔
		if len(plan.Frames) > 1 {
			if !sleepCtx(ctx, w.cfg.FrameGap()) {
				return WriteResult{OK: false, Err: ctx.Err().Error(), Applied: applied}
			}
		}
	}
	return WriteResult{OK: true, Applied: applied}
}

// writeFrame 发送一帧写请求并校验响应。
func (w *worker) writeFrame(ctx context.Context, dev *DevicePlan, prop converter.PropMeta, mode string, frame converter.WriteFrame, transactionStarted time.Time) error {
	encoded, tid, err := dev.Conv.EncodeWrite(dev.UnitID, frame.PDU)
	if err != nil {
		return err
	}

	maxAttempts := w.cfg.ResendRetries + 1
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		w.logTX(dev, "write", attempt, encoded)
		if _, err := w.drv.Send(encoded); err != nil {
			lastErr = err
			if isLinkError(err) {
				return err
			}
			continue
		}
		if !sleepCtx(ctx, w.cfg.FrameGap()) {
			return ctx.Err()
		}
		sentAt := time.Now()

		expectAddr := frame.StartAddr
		quantity := frame.Quantity
		_, err = w.receiveFrame(ctx, 64, func(b []byte) (bool, error) {
			derr := dev.Conv.DecodeWrite(b, tid, dev.UnitID, frame.FC, expectAddr, quantity)
			if derr != nil {
				return false, derr
			}
			return true, nil
		})
		if err == nil {
			w.logRX(dev, "write", attempt, encoded, time.Since(sentAt))
			w.monitor.complete(dev.Index, dev.UnitID, "write", attempt, nil, time.Since(transactionStarted))
			return nil
		}

		lastErr = err
		if isLinkError(err) {
			w.monitor.complete(dev.Index, dev.UnitID, "write", attempt, err, time.Since(transactionStarted))
			return err
		}
		if converter.IsPermanent(err) {
			w.monitor.complete(dev.Index, dev.UnitID, "write", attempt, err, time.Since(transactionStarted))
			return err
		}
		if converter.NeedsResync(err) {
			if rerr := w.drv.Refresh(); rerr != nil && isLinkError(rerr) {
				return rerr
			}
		}
	}

	w.monitor.complete(dev.Index, dev.UnitID, "write", maxAttempts, lastErr, time.Since(transactionStarted))
	if lastErr == nil {
		lastErr = errors.New("写命令失败")
	}
	return lastErr
}

func (w *worker) setConnected(v bool, errMsg string) {
	w.mu.Lock()
	w.connected = v
	w.lastErr = errMsg
	if v {
		w.lastSuccess = time.Now()
	}
	w.mu.Unlock()
}

func (w *worker) setPhase(phase string) {
	w.mu.Lock()
	w.phase = phase
	w.mu.Unlock()
}

func (w *worker) setReconnects(n int) {
	w.mu.Lock()
	w.reconnects = n
	w.mu.Unlock()
}

// setDeviceOnline 更新设备在线状态与统计。
func (w *worker) setDeviceOnline(dev *DevicePlan) {
	now := time.Now()
	w.mu.Lock()
	st := w.devStates[dev.Index]
	if st != nil {
		st.Online = true
		st.ConsecFail = 0
		st.LastPollMs = 0
		st.LastPollAt = now
	}
	w.lastSuccess = now
	w.connected = true
	w.mu.Unlock()
	w.setOnlineValue(dev, true)
}

// setDeviceOffline 更新设备离线状态与统计。
func (w *worker) setDeviceOffline(dev *DevicePlan, err error) {
	w.mu.Lock()
	st := w.devStates[dev.Index]
	if st != nil {
		st.Online = false
		st.ConsecFail++
		st.LastPollAt = time.Now()
		if err != nil {
			st.Error = err.Error()
		}
	}
	w.mu.Unlock()
	w.setOnlineValue(dev, false)
}

// state 返回 worker 当前状态快照。
func (w *worker) state() workerState {
	w.mu.Lock()
	connected, lastErr, phase, reconnects, lastSuccess := w.connected, w.lastErr, w.phase, w.reconnects, w.lastSuccess
	devices := make([]deviceState, 0, len(w.devStates))
	for _, st := range w.devStates {
		devices = append(devices, *st)
	}
	w.mu.Unlock()

	sort.Slice(devices, func(i, j int) bool { return devices[i].Index < devices[j].Index })

	var effective int64
	if len(devices) > 0 {
		effective = w.cfg.PollDuration().Milliseconds() * int64(len(devices))
	}
	return workerState{
		ChannelIndex:      w.index,
		Name:              w.name,
		Type:              w.cfg.Type,
		Target:            w.cfg.Target(),
		Connected:         connected,
		LastError:         lastErr,
		State:             phase,
		ReconnectAttempts: reconnects,
		EffectivePollMs:   effective,
		LastSuccessAt:     lastSuccess,
		Devices:           devices,
	}
}

// ─── 辅助函数 ────────────────────────────────────────────────

// cacheKey 生成 session 缓存键。
func cacheKey(devIdx int, propName string) string {
	return fmt.Sprintf("%d/%s", devIdx, propName)
}

// logTX 记录发送报文（Debug 级），便于通信排障。
func (w *worker) logTX(dev *DevicePlan, operation string, attempt int, p []byte) {
	w.monitor.tx(dev.Index, dev.UnitID, operation, attempt, p)
	w.log.Debug("TX 发送报文", "channel", w.name, "device", dev.DisplayName(), "hex", fmt.Sprintf("% x", p), "len", len(p))
}

// logRX 记录接收报文（Debug 级），便于通信排障。
func (w *worker) logRX(dev *DevicePlan, operation string, attempt int, p []byte, latency time.Duration) {
	w.monitor.rx(dev.Index, dev.UnitID, operation, attempt, p, latency)
	w.log.Debug("RX 接收报文", "channel", w.name, "device", dev.DisplayName(), "hex", fmt.Sprintf("% x", p), "len", len(p))
}

// OnlinePropName 是设备在线状态的虚拟属性名（模型默认属性的显示名）。
const OnlinePropName = "在线状态"

// OnlinePropID 是设备在线状态的虚拟属性 ID（内置在线点，唯一标识）。
const OnlinePropID = "online"

// hasOnlineProp 判断设备模型的属性表中是否包含"在线状态"虚拟属性。
// 以属性 ID 优先、属性名兜底，兼容历史模型。
func hasOnlineProp(dev *DevicePlan) bool {
	for _, p := range dev.Props {
		if p.PropID == OnlinePropID || p.Name == OnlinePropName {
			return true
		}
	}
	return false
}

// setOnlineValue 更新指定设备的在线状态缓存。
func (w *worker) setOnlineValue(dev *DevicePlan, online bool) {
	key, ok := onlineCacheKey(dev)
	if !ok {
		return
	}
	val := int64(0)
	if online {
		val = 1
	}
	w.sess.Lock()
	w.data[key] = SessionEntry{Value: val, Timestamp: time.Now()}
	w.sess.Unlock()
}

// onlineCacheKey 返回该设备在线点的缓存键（优先按属性 ID）。
func onlineCacheKey(dev *DevicePlan) (string, bool) {
	for _, p := range dev.Props {
		if p.PropID == OnlinePropID || p.Name == OnlinePropName {
			return cacheKey(dev.Index, p.CacheKey()), true
		}
	}
	return "", false
}

// markAllOffline 将该链路下所有包含"在线状态"属性的设备标记为离线（0）。
func (w *worker) markAllOffline() {
	for i := range w.plan.Devices {
		w.setOnlineValue(&w.plan.Devices[i], false)
	}
}

// publishAllOffline 为每个设备发送离线遥测。
// announce=true 表示「状态翻转通知」，false 表示低频心跳。
func (w *worker) publishAllOffline(announce bool) {
	for i := range w.plan.Devices {
		if !w.plan.Devices[i].Valid {
			continue
		}
		w.publishTelemetry(&w.plan.Devices[i], false)
	}
}

// waitReconnect 等待重连间隔，期间以低频心跳维持离线可见性。
// 以前这里按轮询周期（500ms）给每设备发离线报文，10 条链路全断时
// 北向会被离线消息打满。现在固定 30s 一次心跳。
func (w *worker) waitReconnect(ctx context.Context, retryInterval time.Duration) bool {
	retry := time.NewTimer(retryInterval)
	defer retry.Stop()
	// 先立即发一次状态翻转通知
	w.publishAllOffline(true)

	heartbeat := time.NewTicker(offlineHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-retry.C:
			return true
		case <-heartbeat.C:
			w.publishAllOffline(false)
		}
	}
}

// offlineHeartbeatInterval 是离线遥测的心跳间隔。
const offlineHeartbeatInterval = 30 * time.Second

// slowProbe 用于「重连次数耗尽」后的降级运行：低频探测 + 低频心跳。
// 返回 true 表示探测成功（应重新进入正常连接流程）。
func (w *worker) slowProbe(ctx context.Context) bool {
	probe := time.NewTicker(reconnectProbeInterval)
	defer probe.Stop()
	heartbeat := time.NewTicker(offlineHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-probe.C:
			if err := w.drv.Open(ctx); err == nil {
				w.log.Info("慢速探测成功，链路恢复", "channel", w.name, "target", w.cfg.Target())
				w.setConnected(true, "")
				return true
			}
			w.setConnected(false, "慢速探测失败")
		case <-heartbeat.C:
			w.publishAllOffline(false)
		}
	}
}

// reconnectProbeInterval 是放弃快速重连后的探测间隔。
const reconnectProbeInterval = 30 * time.Second

// isLinkError 判断错误是否需要重连（底层连接断开）。
func isLinkError(err error) bool {
	if err == nil {
		return false
	}
	// 上下文取消：不视作链路错误（正常退出）。
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// 驱动层已归一化的断线错误（串口拔线、TCP 对端关闭）。
	if errors.Is(err, connector.ErrPortClosed) {
		return true
	}
	// 超时不当作链路错误：设备无响应但 TCP 连接可能仍存活。
	if isTimeout(err) {
		return false
	}
	return false
}

// isTimeout 判断是否为超时类错误。
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	type timeout interface{ Timeout() bool }
	var te timeout
	if errors.As(err, &te) {
		return te.Timeout()
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// sleepCtx 在 ctx 可取消的前提下睡眠 d；被取消返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
