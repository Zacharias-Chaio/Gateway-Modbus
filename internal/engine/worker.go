package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
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
}

// sessionEntry 是单个属性的实时值缓存条目（内部使用，API 层看 SessionEntry）。
type sessionEntry = SessionEntry

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

	mu        sync.Mutex
	connected bool
	lastErr   string

	// session 值缓存：key = "deviceIndex/propName"
	sess sync.RWMutex
	data map[string]sessionEntry

	monitor *commMonitor
	sink    EventSink
}

// newWorker 构造 worker，此时尚未启动 goroutine。
func newWorker(index int, name, fp string, cfg connector.Config, drv connector.Driver, plan ChannelPlan, sink EventSink) *worker {
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
		data:      make(map[string]sessionEntry),
		monitor:   newCommMonitor(index, defaultCommEventCapacity),
		sink:      sink,
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

// publishTelemetry 构造当前设备的一轮遥测快照，交给外部数据出口。
func (w *worker) publishTelemetry(deviceIndex int, online bool) {
	if w.sink == nil || deviceIndex < 0 || deviceIndex >= len(w.plan.Devices) {
		return
	}
	dev := &w.plan.Devices[deviceIndex]
	now := time.Now()
	properties := make(map[string]TelemetryProperty)
	if online {
		w.sess.RLock()
		for _, prop := range dev.Props {
			if prop.PropID == "" {
				w.log.Warn("属性缺少 ID，跳过遥测发布", "channel", w.name, "device", dev.DisplayName(), "prop", prop.Name)
				continue
			}
			if value, ok := w.data[cacheKey(deviceIndex, prop.Name)]; ok {
				properties[prop.PropID] = TelemetryProperty{Name: prop.Name, Unit: prop.Unit, Description: prop.Description, AccessMode: prop.AccessMode, Value: value.Value, Timestamp: value.Timestamp}
			} else {
				properties[prop.PropID] = TelemetryProperty{Name: prop.Name, Unit: prop.Unit, Description: prop.Description, AccessMode: prop.AccessMode, Value: nil, Timestamp: now}
			}
		}
		w.sess.RUnlock()
		w.addOnlineTelemetry(properties, dev, int64(1), now)
	} else {
		w.addOnlineTelemetry(properties, dev, int64(0), now)
	}
	w.sink.PublishTelemetry(TelemetryEvent{
		ChannelIndex: w.index, DeviceIndex: deviceIndex, DeviceName: dev.DisplayName(),
		CommNo: int(dev.UnitID), ModelID: dev.ModelID, ModelName: dev.ModelName,
		Online: online, Properties: properties, Timestamp: now,
	})
}

func (w *worker) addOnlineTelemetry(properties map[string]TelemetryProperty, dev *DevicePlan, value int64, timestamp time.Time) {
	for _, prop := range dev.Props {
		if prop.Name != OnlinePropName {
			continue
		}
		if prop.PropID == "" {
			w.log.Warn("在线状态属性缺少 ID，跳过遥测发布", "channel", w.name, "device", dev.DisplayName())
			return
		}
		properties[prop.PropID] = TelemetryProperty{Name: prop.Name, Unit: prop.Unit, Description: prop.Description, AccessMode: prop.AccessMode, Value: value, Timestamp: timestamp}
		return
	}
}

// publishWriteResult 将异步写命令的最终结果交给外部数据出口。
func (w *worker) publishWriteResult(cmd WriteCommand, err error) {
	if w.sink == nil || cmd.RequestID == "" {
		return
	}
	event := WriteResultEvent{
		RequestID: cmd.RequestID, ChannelIndex: w.index, DeviceIndex: cmd.DeviceIndex,
		OK: err == nil, Timestamp: time.Now(),
	}
	if err != nil {
		event.Error = err.Error()
	}
	w.sink.PublishWriteResult(event)
}

// errLinkStopped 表示 worker 已退出，队列中的写命令不会被执行。
var errLinkStopped = errors.New("链路已停止，写命令未执行")

// drainPendingWrites 在 worker 退出前排空写命令队列：
// 对带 RequestID 的命令（消息总线来源）发布失败回执，避免调用方永远等不到 cmdAck。
// worker 退出前已从引擎活动表移除，不再有新命令入队；仅存在极小的在途投递窗口。
func (w *worker) drainPendingWrites() {
	for {
		select {
		case cmd := <-w.writeCh:
			if cmd.RequestID == "" {
				continue // HTTP 来源命令无回执通道，直接丢弃
			}
			w.publishWriteResult(cmd, errLinkStopped)
		default:
			return
		}
	}
}

// run 是链路主循环：连接（失败后按 reconnectRetries 策略重连）→ 采集循环 → ctx 取消则关闭退出。
func (w *worker) run(ctx context.Context) {
	defer close(w.done)
	defer func() {
		if err := w.drv.Close(); err != nil {
			w.log.Warn("关闭链路失败", "channel", w.name, "err", err)
		}
		w.setConnected(false, "")
	}()
	// LIFO：drain 最先执行，保证 stop() 等到 done 关闭时回执已全部发出。
	defer w.drainPendingWrites()

	const reconnectInterval = 3 * time.Second
	connectAttempts := 0 // 已尝试的连接次数（用于 reconnectRetries 判断）

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := w.drv.Open(ctx); err != nil {
			connectAttempts++
			w.setConnected(false, err.Error())
			w.markAllOffline()
			w.publishAllOffline()

			// reconnectRetries == 0 表示无限重连；> 0 表示达到次数后放弃链路。
			maxRetry := w.cfg.ReconnectRetries
			if maxRetry > 0 && connectAttempts > maxRetry {
				w.log.Error("链路连接失败次数已达上限，停止重连",
					"channel", w.name, "target", w.cfg.Target(),
					"attempts", connectAttempts, "max", maxRetry)
				w.publishOfflineUntilStopped(ctx)
				return
			}

			w.log.Warn("链路连接失败，稍后重试",
				"channel", w.name, "target", w.cfg.Target(), "err", err,
				"attempt", connectAttempts, "retryIn", reconnectInterval.String())
			if !w.waitReconnect(ctx, reconnectInterval) {
				return
			}
			continue
		}

		// 连接成功，重置计数
		connectAttempts = 0
		w.setConnected(true, "")
		w.monitor.reset(time.Now())
		w.log.Info("链路已连接，开始采集",
			"channel", w.name, "type", w.cfg.Type, "target", w.cfg.Target(),
			"devices", len(w.plan.Devices), "pollInterval", w.pollDuration().String())

		// 连接成功后进入采集循环；返回非 nil 表示需重连。
		if w.collectLoop(ctx) {
			w.setConnected(false, "")
			w.markAllOffline()
			w.publishAllOffline()
			if !w.waitReconnect(ctx, reconnectInterval) {
				return
			}
			continue
		}
		return // ctx 取消，正常退出
	}
}

// collectLoop 是采集主循环，在连接已建立的前提下运行。
// 返回 true 表示链路异常需要重连，false 表示 ctx 取消正常退出。
func (w *worker) collectLoop(ctx context.Context) bool {
	pollInterval := w.pollDuration()

	// 逐设备轮询：一个 tick 轮询一个设备。
	devIdx := 0
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		default:
		}

		// ── 写优先级检查：非阻塞查看是否有写命令待执行 ──
		select {
		case cmd := <-w.writeCh:
			if err := w.execWrite(ctx, cmd); err != nil {
				w.log.Warn("写命令执行失败", "channel", w.name, "prop", cmd.PropName, "err", err)
				w.publishWriteResult(cmd, err)
				if isLinkError(err) {
					return true // 需重连
				}
			} else {
				w.publishWriteResult(cmd, nil)
			}
			continue // 写完成后立即检查下一条写命令，保证写优先
		default:
			// 无写命令，继续轮询
		}

		if len(w.plan.Devices) > 0 {
			dev := &w.plan.Devices[devIdx%len(w.plan.Devices)]
			if err := w.pollDevice(ctx, dev); err != nil {
				w.log.Warn("设备轮询失败", "channel", w.name, "device", dev.DisplayName(), "err", err)
				w.setOnline(dev.Index, false)
				w.publishTelemetry(dev.Index, false)
				if isLinkError(err) {
					return true
				}
			} else {
				w.setOnline(dev.Index, true)
				w.publishTelemetry(dev.Index, true)
			}
			devIdx++
		}

		// 等待下一个 tick 或 ctx 取消
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

// pollDuration 返回轮询间隔，默认 500ms。
func (w *worker) pollDuration() time.Duration {
	if w.cfg.PollInterval > 0 {
		return time.Duration(w.cfg.PollInterval) * time.Millisecond
	}
	return 500 * time.Millisecond
}

// pollDevice 轮询单个设备：遍历其所有寄存器分组，逐组发送读请求。
func (w *worker) pollDevice(ctx context.Context, dev *DevicePlan) error {
	for gi := range dev.Groups {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		if err := w.pollOne(ctx, dev, gi); err != nil {
			return err
		}
		if !sleepCtx(ctx, frameInterval(w.cfg.FrameInterval)) {
			return ctx.Err()
		}
	}
	return nil
}

// pollOne 执行一次寄存器组读取（含重发逻辑）。
// resendRetries 控制单帧发送失败后的重试次数（0 = 不重试，发一次即返回错误）。
func (w *worker) pollOne(ctx context.Context, dev *DevicePlan, gi int) error {
	g := dev.Groups[gi]
	deviceIndex := dev.Index
	transactionStarted := time.Now()

	// 组装读请求
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

		// 发送
		w.logTX(dev, "read", attempt, req)
		if _, err := w.drv.Send(req); err != nil {
			lastErr = err
			w.log.Debug("读请求发送失败",
				"channel", w.name, "device", dev.DisplayName(),
				"attempt", attempt, "err", err)
			continue
		}

		// 帧间隔（半双工总线发送后需等待响应）
		if !sleepCtx(ctx, frameInterval(w.cfg.FrameInterval)) {
			return ctx.Err()
		}

		// 读取响应（渐进式帧读取）
		raw, err := w.readFrame(ctx, dev, byte(g.ReadFC), g.Quantity, tid, "read", attempt, time.Now())
		if err != nil {
			lastErr = err
			// 链路层错误（连接断开）无需重发，直接返回触发重连
			if isLinkError(err) {
				w.monitor.complete(deviceIndex, dev.UnitID, "read", attempt, err, time.Since(transactionStarted))
				return err
			}
			w.log.Debug("读响应解析失败",
				"channel", w.name, "device", dev.DisplayName(),
				"attempt", attempt, "err", err)
			continue
		}

		// 解析各属性值并写入 session 缓存
		for _, m := range g.Members {
			if len(raw) < m.ByteOffset+m.ByteLen {
				continue
			}
			chunk := raw[m.ByteOffset : m.ByteOffset+m.ByteLen]
			val, err := converter.MapRegisters(chunk, m.Prop)
			if err != nil {
				continue
			}
			key := cacheKey(deviceIndex, m.Prop.Name)
			w.sess.Lock()
			w.data[key] = sessionEntry{Value: val, Timestamp: time.Now()}
			w.sess.Unlock()

			w.log.Debug("采集成功",
				"channel", w.name, "device", dev.DisplayName(),
				"prop", m.Prop.Name, "value", val)
		}
		w.monitor.complete(deviceIndex, dev.UnitID, "read", attempt, nil, time.Since(transactionStarted))
		return nil
	}

	// 所有重发均失败
	if lastErr != nil {
		w.monitor.complete(deviceIndex, dev.UnitID, "read", maxAttempts, lastErr, time.Since(transactionStarted))
		w.log.Warn("读请求重发耗尽",
			"channel", w.name, "device", dev.DisplayName(),
			"attempts", maxAttempts, "lastErr", lastErr)
		return lastErr
	}
	err = errors.New("读请求失败")
	w.monitor.complete(deviceIndex, dev.UnitID, "read", maxAttempts, err, time.Since(transactionStarted))
	return err
}

// readFrame 渐进式读取一帧响应。
// RTU 按固定长度读，TCP 先读 MBAP 头再按 length 字段读完整帧。
func (w *worker) readFrame(ctx context.Context, dev *DevicePlan, fc byte, quantity int, tid uint16, operation string, attempt int, sentAt time.Time) ([]byte, error) {
	const (
		readTimeout  = 1500 * time.Millisecond // 单次 Receive 超时
		maxWaitTotal = 3000 * time.Millisecond // 整帧最大等待时间
	)

	deadline := time.Now().Add(maxWaitTotal)
	var buf []byte

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		tmp := make([]byte, 256)
		n, err := w.drv.Receive(tmp, readTimeout)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}

		// 尝试解码
		data, err := dev.Conv.DecodeRead(buf, tid, dev.UnitID, fc, quantity)
		if err == nil {
			w.logRX(dev, operation, attempt, buf, time.Since(sentAt))
			return data, nil
		}
		if !converter.IsShortFrame(err) {
			// 异常码或校验失败
			return nil, err
		}
		// ErrShortFrame → 继续累积字节
	}
	return nil, errors.New("读取响应帧超时")
}

// execWrite 执行一条写命令（含重发逻辑）。
// resendRetries 控制单帧发送失败后的重试次数（0 = 不重试）。
func (w *worker) execWrite(ctx context.Context, cmd WriteCommand) error {
	if cmd.DeviceIndex < 0 || cmd.DeviceIndex >= len(w.plan.Devices) {
		return errors.New("设备序号越界")
	}
	dev := &w.plan.Devices[cmd.DeviceIndex]
	transactionStarted := time.Now()

	// 查找属性
	prop, err := converter.FindWriteProp(dev.Props, cmd.PropName)
	if err != nil {
		return err
	}

	// 逆变换：engineering = raw×coef+delta → raw = (engineering - delta) / coef
	coef := prop.Coefficient
	if coef == 0 {
		coef = 1
	}
	rawVal := (cmd.RawValue - prop.DeltaValue) / coef

	// 编码 PDU
	pdu, err := converter.EncodeValuePDU(prop, rawVal, prop.RegisterBase+prop.Offset)
	if err != nil {
		return err
	}

	// 组帧
	frame, tid, err := dev.Conv.EncodeWrite(dev.UnitID, pdu)
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

		// 发送
		w.logTX(dev, "write", attempt, frame)
		if _, err := w.drv.Send(frame); err != nil {
			lastErr = err
			continue
		}
		if !sleepCtx(ctx, frameInterval(w.cfg.FrameInterval)) {
			return ctx.Err()
		}
		sentAt := time.Now()

		// 读响应（渐进式）
		buf := make([]byte, 0, 32)
		const writeRespTimeout = 1500 * time.Millisecond
		deadline := time.Now().Add(3000 * time.Millisecond)

		writeOK := false
		for time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			tmp := make([]byte, 64)
			n, err := w.drv.Receive(tmp, writeRespTimeout)
			if err != nil {
				lastErr = err
				break // 内层循环，尝试下一轮重发
			}
			if n > 0 {
				buf = append(buf, tmp[:n]...)
			}
			err = dev.Conv.DecodeWrite(buf, tid, dev.UnitID, byte(prop.WriteFC),
				prop.RegisterBase+prop.Offset, prop.RegisterCount)
			if err == nil {
				w.logRX(dev, "write", attempt, buf, time.Since(sentAt))
				w.monitor.complete(cmd.DeviceIndex, dev.UnitID, "write", attempt, nil, time.Since(transactionStarted))
				return nil
			}
			if !converter.IsShortFrame(err) {
				lastErr = err
				break
			}
			writeOK = true // 继续累积
		}
		if !writeOK && lastErr == nil {
			lastErr = errors.New("写响应帧超时")
		}
	}
	if lastErr != nil {
		w.monitor.complete(cmd.DeviceIndex, dev.UnitID, "write", maxAttempts, lastErr, time.Since(transactionStarted))
		return lastErr
	}
	err = errors.New("写命令失败")
	w.monitor.complete(cmd.DeviceIndex, dev.UnitID, "write", maxAttempts, err, time.Since(transactionStarted))
	return err
}

func (w *worker) setConnected(v bool, errMsg string) {
	w.mu.Lock()
	w.connected = v
	w.lastErr = errMsg
	w.mu.Unlock()
}

// state 返回 worker 当前状态快照。
func (w *worker) state() workerState {
	w.mu.Lock()
	connected, lastErr := w.connected, w.lastErr
	w.mu.Unlock()
	return workerState{
		ChannelIndex: w.index,
		Name:         w.name,
		Type:         w.cfg.Type,
		Target:       w.cfg.Target(),
		Connected:    connected,
		LastError:    lastErr,
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
	w.log.Debug("TX 发送报文",
		"channel", w.name, "device", dev.DisplayName(),
		"hex", fmt.Sprintf("% x", p), "len", len(p))
}

// logRX 记录接收报文（Debug 级），便于通信排障。
func (w *worker) logRX(dev *DevicePlan, operation string, attempt int, p []byte, latency time.Duration) {
	w.monitor.rx(dev.Index, dev.UnitID, operation, attempt, p, latency)
	w.log.Debug("RX 接收报文",
		"channel", w.name, "device", dev.DisplayName(),
		"hex", fmt.Sprintf("% x", p), "len", len(p))
}

// OnlinePropName 是设备在线状态的虚拟属性名（模型默认属性，不映射实际寄存器）。
const OnlinePropName = "在线状态"

// hasOnlineProp 判断设备模型的属性表中是否包含"在线状态"虚拟属性。
func hasOnlineProp(dev *DevicePlan) bool {
	for _, p := range dev.Props {
		if p.Name == OnlinePropName {
			return true
		}
	}
	return false
}

// setOnline 更新指定设备的在线状态缓存。
// online=true → 值 1（报文交互正常）；online=false → 值 0（链路断开或设备无响应）。
func (w *worker) setOnline(devIdx int, online bool) {
	dev := &w.plan.Devices[devIdx]
	if !hasOnlineProp(dev) {
		return
	}
	val := int64(0)
	if online {
		val = 1
	}
	w.sess.Lock()
	w.data[cacheKey(devIdx, OnlinePropName)] = sessionEntry{Value: val, Timestamp: time.Now()}
	w.sess.Unlock()
}

// markAllOffline 将该链路下所有包含"在线状态"属性的设备标记为离线（0）。
func (w *worker) markAllOffline() {
	for i := range w.plan.Devices {
		w.setOnline(i, false)
	}
}

// publishAllOffline sends an offline-only telemetry snapshot for every device.
func (w *worker) publishAllOffline() {
	for i := range w.plan.Devices {
		w.publishTelemetry(i, false)
	}
}

// waitReconnect emits offline telemetry at the polling cadence while waiting to retry.
func (w *worker) waitReconnect(ctx context.Context, retryInterval time.Duration) bool {
	retry := time.NewTimer(retryInterval)
	defer retry.Stop()
	ticker := time.NewTicker(w.pollDuration())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-retry.C:
			return true
		case <-ticker.C:
			w.publishAllOffline()
		}
	}
}

// publishOfflineUntilStopped keeps the offline state visible after reconnect attempts are exhausted.
func (w *worker) publishOfflineUntilStopped(ctx context.Context) {
	ticker := time.NewTicker(w.pollDuration())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.publishAllOffline()
		}
	}
}

// isLinkError 判断错误是否需要重连（底层连接断开）。
func isLinkError(err error) bool {
	if err == nil {
		return false
	}
	// 1) 上下文取消：不视作链路错误（正常退出）。
	if errors.Is(err, context.Canceled) {
		return false
	}
	// 2) 标准 io.EOF / net.ErrClosed：典型的对端关闭。
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	// 3) net.Error：连接重置类立即重连；超时类（设备无响应但 TCP 可能仍存活）
	//    不视作链路错误，避免频繁重连——若连接真的半开，后续 Send 会触发
	//    connection reset / EOF 再重连。
	var ne net.Error
	if errors.As(err, &ne) {
		return !ne.Timeout()
	}
	// 4) 关键字兜底：覆盖各平台底层错误文案变体（如 Windows
	//    "wsasend: An existing connection was forcibly closed by the remote host."）。
	msg := strings.ToLower(err.Error())
	for _, kw := range linkErrKeywords {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// linkErrKeywords 是触发重连的错误消息关键字（统一小写匹配）。
// 注：io.EOF 由 errors.Is 精确捕获，此处不再用 "eof" 子串避免误伤。
var linkErrKeywords = []string{
	"connection reset by peer",
	"broken pipe",
	"connection forcibly closed",
	"wsasend",
	"wsaeconnreset",
	"connection refused",
	"connection aborted",
	"no connection could be made",
	"use of closed network connection",
}

// sleepCtx 在 ctx 可取消的前提下睡眠 d；被取消返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func frameInterval(milliseconds int) time.Duration {
	if milliseconds <= 0 {
		return 0
	}
	return time.Duration(milliseconds) * time.Millisecond
}
