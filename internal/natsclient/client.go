package natsclient

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gateway/internal/config"
	"gateway/internal/engine"
	"gateway/internal/engine/converter"
	"gateway/internal/logx"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

// CommandTarget 是北向客户端需要的写命令能力。
// 收窄为接口后，runtime.Manager 可以在重启窗口内返回明确的失败，
// 而不是让 NATS 侧持有一个构造时的引擎指针、写到已被 stop 的引擎上。
type CommandTarget interface {
	Submit(channelIndex int, cmd engine.WriteCommand) bool
	HasDevice(channelIndex, deviceIndex int) bool
	Connected(channelIndex int) bool
}

// Client 是 Core NATS 北向客户端。
type Client struct {
	config  config.NATS
	gateway config.Gateway
	token   string
	source  engine.PlanSource
	target  CommandTarget
	log     *slog.Logger

	data  string
	cmd   string
	query string

	conn *nats.Conn

	events       chan any
	eventsMu     sync.Mutex
	eventsClosed bool

	// 日志扇出队列：与遥测队列隔离——日志洪泛不得挤占遥测；满时丢最旧（诊断流尽力而为）。
	logQueue     chan logMessage
	logThreshold slog.Level

	done     chan struct{}
	stopOnce sync.Once
}

// source 提供链路与设备模型配置（拓扑查询用），北向客户端不直接依赖存储层。
// gatewayToken 是网关对北向平台的接入凭据，随每条遥测上行消息扇出；
// 未配置时为空串，字段仍会出现在消息中（gateway_token: ""）。
func New(ctx context.Context, gateway config.Gateway, gatewayToken string, cfg config.NATS, source engine.PlanSource, target CommandTarget) (*Client, error) {
	gwID, err := config.ValidateGWID(gateway.GWID)
	if err != nil {
		return nil, err
	}
	if cfg.SubjectPrefix == "" {
		return nil, fmt.Errorf("nats.subjectPrefix 不能为空")
	}
	prefix := strings.TrimSuffix(cfg.SubjectPrefix, ".") + "." + gwID
	client := &Client{
		config: cfg, gateway: gateway, token: gatewayToken, source: source, target: target, log: logx.Module("nats"),
		data: prefix + ".data", cmd: prefix + ".cmd", query: prefix + ".query",
		events: make(chan any, queueSize(cfg.QueueSize)), done: make(chan struct{}),
		logQueue:     make(chan logMessage, 1024),
		logThreshold: logx.ParseLevel(cfg.LogLevel),
	}
	opts := []nats.Option{
		nats.Name(cfg.Name),
		nats.Timeout(milliseconds(cfg.ConnectTimeout, 2*time.Second)),
		nats.ReconnectWait(milliseconds(cfg.ReconnectWait, 2*time.Second)),
		nats.MaxReconnects(cfg.MaxReconnects),
		nats.RetryOnFailedConnect(cfg.RetryOnFailedConnect),
		nats.ReconnectBufSize(cfg.ReconnectBufSize),
		nats.PingInterval(milliseconds(cfg.PingInterval, 20*time.Second)),
		nats.MaxPingsOutstanding(cfg.MaxPingsOut),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) { client.log.Warn("NATS 已断开", "err", err) }),
		nats.ReconnectHandler(func(conn *nats.Conn) { client.log.Info("NATS 已重连", "url", conn.ConnectedUrl()) }),
		nats.ClosedHandler(func(conn *nats.Conn) { client.log.Warn("NATS 已关闭", "err", conn.LastError()) }),
	}
	// 支持用户名密码（明文 TLS 终结由反向代理负责，避免在嵌入式设备上维护证书）。
	if cfg.Username != "" {
		opts = append(opts, nats.UserInfo(cfg.Username, cfg.Password))
	}
	conn, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("连接 NATS: %w", err)
	}
	client.conn = conn
	if _, err := conn.Subscribe(client.cmd, client.handleCommand); err != nil {
		conn.Close()
		return nil, fmt.Errorf("订阅命令主题: %w", err)
	}
	if _, err := conn.Subscribe(client.query, client.handleQuery); err != nil {
		conn.Close()
		return nil, fmt.Errorf("订阅查询主题: %w", err)
	}
	if err := conn.Flush(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("初始化 NATS 订阅: %w", err)
	}
	go client.publishLoop(ctx)
	if cfg.LogEnabled {
		// 日志扇出：注册为 logx 第四路出口。必须放在 publishLoop 之后：
		// 写入方（全局 logger）随时可能投递，goroutine 未起就会丢日志。
		logx.SetNATSWriter(client)
		go client.logLoop(ctx)
		client.log.Info("NATS 日志扇出已启用", "subject", platformLogSubject, "level", client.logThreshold.String())
	}
	client.log.Info("NATS 客户端已启动", "data", client.data, "cmd", client.cmd, "query", client.query)
	return client, nil
}

// PublishTelemetry implements engine.EventSink. It never blocks a worker.
func (c *Client) PublishTelemetry(event engine.TelemetryEvent) {
	c.enqueue(event)
}

// PublishWriteResult implements engine.EventSink. It never blocks a worker.
func (c *Client) PublishWriteResult(event engine.WriteResultEvent) {
	c.enqueue(event)
}

func (c *Client) enqueue(event any) {
	c.eventsMu.Lock()
	defer c.eventsMu.Unlock()
	if c.eventsClosed {
		return
	}

	select {
	case c.events <- event:
		return
	default:
	}

	// 保留最新遥测：队列满时淘汰一条已排队的旧事件后重试入队。
	select {
	case <-c.events:
		c.log.Warn("NATS 发布队列已满，丢弃最旧事件")
	default:
	}
	c.events <- event
}

// Close stops publishing and drains subscriptions before closing the NATS connection.
func (c *Client) Close() {
	c.stopOnce.Do(func() {
		// 先摘除日志出口：Close 之后队列里的事件不再有意义，避免循环写已关闭通道。
		logx.SetNATSWriter(nil)
		c.eventsMu.Lock()
		c.eventsClosed = true
		close(c.events)
		c.eventsMu.Unlock()
		<-c.done
		if err := c.conn.Drain(); err != nil {
			c.log.Warn("NATS Drain 失败", "err", err)
			c.conn.Close()
		}
	})
}

func (c *Client) publishLoop(ctx context.Context) {
	defer close(c.done)
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-c.events:
			if !ok {
				return
			}
			c.publishEvent(event)
		}
	}
}

func (c *Client) publishEvent(event any) {
	switch e := event.(type) {
	case engine.TelemetryEvent:
		c.publishTelemetry(e)
	case engine.WriteResultEvent:
		c.publishWriteResult(e)
	}
}

func (c *Client) publishTelemetry(event engine.TelemetryEvent) {
	payload := messageData{
		GatewayID:    c.gateway.GWID,
		GatewaySN:    c.gateway.SN,
		GatewayToken: c.token,
		DeviceCode:   deviceCode(c.gateway.GWID, event.ChannelIndex, event.DeviceIndex),
		ChannelIndex: event.ChannelIndex,
		DeviceIndex:  event.DeviceIndex,
		DeviceName:   event.DeviceName,
		CommNo:       event.CommNo,
		ModelID:      event.ModelID,
		ModelName:    event.ModelName,
		Properties:   make(map[string]propVal, len(event.Properties)),
	}
	for key, p := range event.Properties {
		payload.Properties[key] = propVal{
			Index: p.Index, Name: p.Name, Unit: p.Unit, Description: p.Description,
			AccessMode: p.AccessMode, Value: p.Value, Timestamp: p.Timestamp.UnixMilli(),
		}
	}
	// 契约：遥测上行固定为 PP-Message-Type: data（见 messageTypeData 注释）。
	c.publish(messageTypeData, payload)
}

func (c *Client) publishWriteResult(event engine.WriteResultEvent) {
	// 契约：写命令异步终态复用 .data 主题，类型为 cmdAck。
	c.publish(messageTypeCmdAck, messageAck{
		RequestID: event.RequestID, ChannelIndex: event.ChannelIndex, DeviceIndex: event.DeviceIndex,
		Status: statusOf(event.OK), Message: event.Error,
	})
}

func (c *Client) publish(messageType string, payload any) {
	if c.conn == nil {
		return
	}
	data, err := json.Marshal(payload)
	if err != nil {
		c.log.Warn("NATS 消息编码失败", "type", messageType, "err", err)
		return
	}
	if err := c.conn.PublishMsg(newEnvelope(messageType, data).toMsg(c.data)); err != nil {
		c.log.Warn("NATS 发布失败", "type", messageType, "err", err)
	}
}

func (c *Client) handleCommand(msg *nats.Msg) {
	env, err := envelopeFromMsg(msg)
	if err != nil || env.Type != messageTypeCmd {
		c.respondAck(msg, messageAck{Status: "failure", Message: "无效控制消息"})
		return
	}
	var command messageCmd
	// 通道索引从 0 开始，0 是合法值；仅负数或缺参视为非法。
	if err := json.Unmarshal(env.Payload, &command); err != nil || command.Name == "" || command.ChannelIndex < 0 {
		c.respondAck(msg, messageAck{RequestID: env.ID, ChannelIndex: command.ChannelIndex, DeviceIndex: command.DeviceIndex, Status: "failure", Message: "无效控制参数"})
		return
	}
	if !c.target.HasDevice(command.ChannelIndex, command.DeviceIndex) {
		c.respondAck(msg, messageAck{RequestID: env.ID, ChannelIndex: command.ChannelIndex, DeviceIndex: command.DeviceIndex, Status: "failure", Message: "通道或设备不存在"})
		return
	}
	accepted := c.target.Submit(command.ChannelIndex, engine.WriteCommand{
		RequestID: env.ID, DeviceIndex: command.DeviceIndex, PropName: command.Name, RawValue: command.Value,
	})
	status, message := "accepted", ""
	if !accepted {
		status, message = "failure", "写命令队列已满或链路未就绪"
	}
	c.respondAck(msg, messageAck{RequestID: env.ID, ChannelIndex: command.ChannelIndex, DeviceIndex: command.DeviceIndex, Status: status, Message: message})
}

func (c *Client) respondAck(msg *nats.Msg, response messageAck) {
	if msg.Reply == "" {
		return
	}
	data, err := json.Marshal(response)
	if err == nil {
		_ = c.conn.PublishMsg(newEnvelope(messageTypeCmdAck, data).toMsg(msg.Reply))
	}
}

func (c *Client) handleQuery(msg *nats.Msg) {
	env, err := envelopeFromMsg(msg)
	if err != nil || env.Type != messageTypeQuery {
		c.respondQuery(msg, messageQueryResp{OK: false, Message: "无效查询消息"})
		return
	}
	var query messageQuery
	if err := json.Unmarshal(env.Payload, &query); err != nil || query.Type != "queryTopology" {
		c.respondQuery(msg, messageQueryResp{Type: query.Type, OK: false, Message: "unknown query type"})
		return
	}
	channels, err := c.topology()
	if err != nil {
		c.respondQuery(msg, messageQueryResp{Type: query.Type, OK: false, Message: err.Error()})
		return
	}
	c.respondQuery(msg, messageQueryResp{Type: query.Type, OK: true, Channels: channels})
}

func (c *Client) respondQuery(msg *nats.Msg, response messageQueryResp) {
	if msg.Reply == "" {
		return
	}
	data, err := json.Marshal(response)
	if err != nil {
		return
	}
	_ = c.conn.PublishMsg(newEnvelope("queryResp", data).toMsg(msg.Reply))
}

func (c *Client) topology() ([]channelInfo, error) {
	// 使用带超时的 ctx，避免 DB 查询风暴时无法反压。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	channels, err := c.source.LoadChannels(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取链路: %w", err)
	}
	models, err := c.source.LoadModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取设备模型: %w", err)
	}
	modelMap := make(map[string]engine.ModelSpec, len(models))
	for _, model := range models {
		modelMap[model.ID] = model
	}
	result := make([]channelInfo, 0, len(channels))
	for _, channel := range channels {
		var mounts []engine.DeviceMount
		if err := json.Unmarshal(channel.Devices, &mounts); err != nil {
			c.log.Warn("链路设备列表解析失败，拓扑中跳过该链路", "channel", channel.Name, "err", err)
			continue
		}
		info := channelInfo{ID: channel.ID, ChannelIndex: channel.Index, Name: channel.Name, Type: channel.Type, Connected: c.target.Connected(channel.Index)}
		for _, mount := range mounts {
			device := deviceInfo{Index: mount.Index, Name: mount.Name, CommNo: mount.CommNo}
			if model, ok := modelMap[mount.ModelID]; ok {
				var properties []converter.PropMeta
				if err := json.Unmarshal(model.Properties, &properties); err != nil {
					c.log.Warn("设备模型属性解析失败", "model", model.Name, "err", err)
					continue
				}
				device.Datasheet = make([]dataEntry, 0, len(properties))
				for _, property := range properties {
					device.Datasheet = append(device.Datasheet, dataEntry{DataIndex: property.PropID, DataName: property.Name, DataRW: property.AccessMode})
				}
			}
			info.Devices = append(info.Devices, device)
		}
		result = append(result, info)
	}
	return result, nil
}

func statusOf(ok bool) string {
	if ok {
		return "success"
	}
	return "failure"
}

func milliseconds(value int, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return time.Duration(value) * time.Millisecond
}

func queueSize(value int) int {
	if value <= 0 {
		return 1
	}
	return value
}

// WriteLog 实现 logx.NATSWriter（日志第四路出口）。
//
// 非阻塞：满队列丢最旧并计一条溢出告警（queue.overflow，经告警主题，
// 不再循环进日志队列）。级别过滤在这里做——logx 侧只标量化不过滤，
// 保证本地/文件/前端三路不受平台阈值影响。
func (c *Client) WriteLog(level slog.Level, mod, msg string, attrs map[string]any, at time.Time) {
	if level < c.logThreshold {
		return
	}
	m := logMessage{
		LogID:  uuid.NewString(),
		Source: logSource{Module: "gateway", Instance: c.gateway.GWID},
		Level:  levelName(level),
		Mod:    mod, Msg: msg, Attrs: attrs,
		Timestamp: at.UnixMilli(),
	}
	select {
	case c.logQueue <- m:
	default:
		// 丢最旧、留最新：诊断流尽力而为语义。
		select {
		case <-c.logQueue:
		default:
		}
		c.PublishAlarm(alarmMessage{
			AlarmID:  uuid.NewString(),
			Source:   alarmSource{Module: "gateway", Instance: c.gateway.GWID},
			Category: "software", Kind: "event", Level: 1,
			Timestamp: time.Now().UnixMilli(),
			Software: &alarmSoftware{
				Code: "gateway.queue.overflow", Component: "nats",
				Message: "日志发布队列已满，丢弃最旧日志",
				Context: map[string]any{"queue_size": len(c.logQueue)},
			},
		})
		select {
		case c.logQueue <- m:
		default:
		}
	}
}

// logLoop 是日志扇出的专用发布 goroutine：与遥测 publishLoop 并行，
// 互不阻塞。ctx 取消即退出，丢弃未发布残量。
func (c *Client) logLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-c.logQueue:
			data, err := json.Marshal(m)
			if err != nil {
				continue
			}
			if err := c.conn.PublishMsg(newEnvelope(messageTypeLog, data).toMsg(platformLogSubject)); err != nil {
				// 日志发布失败只进本地日志，不再回灌 NATS 队列（避免反馈循环）。
				c.log.Warn("日志扇出发布失败", "err", err)
			}
		}
	}
}

// PublishAlarm 发布一条平台告警（契约见 docs/powerpulse-alarm-standard.md）。
//
// 同步直发而非走队列：告警量被沿语义+聚合控制在极低水平，且闭环类消息
// （携带 request_id）必须尽力必达，不允许被日志/遥测的丢最旧策略牺牲。
// PublishMsg 在断连期间写入 nats.go 重连缓冲，非阻塞。
func (c *Client) PublishAlarm(msg alarmMessage) {
	if msg.Software == nil {
		return
	}
	msg.Source = alarmSource{Module: "gateway", Instance: c.gateway.GWID}
	if msg.Timestamp == 0 {
		msg.Timestamp = time.Now().UnixMilli()
	}
	data, err := json.Marshal(msg)
	if err != nil {
		c.log.Warn("告警消息编码失败", "code", msg.Software.Code, "err", err)
		return
	}
	env := newEnvelope(messageTypeAlarm, data)
	msg.AlarmID = env.ID // 契约：PP-Message-ID 与 alarm_id 一致
	data, err = json.Marshal(msg)
	if err != nil {
		c.log.Warn("告警消息编码失败", "code", msg.Software.Code, "err", err)
		return
	}
	if err := c.conn.PublishMsg(env.toMsg(platformAlarmSubject)); err != nil {
		c.log.Warn("告警发布失败", "code", msg.Software.Code, "err", err)
	}
}

// AlarmPublisher 暴露给 engine/runtime 的最小告警出口。
// 返回的函数直接调用 PublishAlarm，便于 engine 侧以闭包形式持有。
func (c *Client) AlarmPublisher() func(code, component, message string, level int, kind string, context map[string]any) {
	return func(code, component, message string, level int, kind string, context map[string]any) {
		c.PublishAlarm(alarmMessage{
			AlarmID:  uuid.NewString(),
			Source:   alarmSource{Module: "gateway", Instance: c.gateway.GWID},
			Category: "software", Kind: kind, Level: level,
			Timestamp: time.Now().UnixMilli(),
			Software: &alarmSoftware{
				Code: code, Component: component, Message: message, Context: context,
			},
		})
	}
}

// levelName 把 slog 级别转为契约规定的小写字符串。
func levelName(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "error"
	case level >= slog.LevelWarn:
		return "warn"
	case level >= slog.LevelInfo:
		return "info"
	default:
		return "debug"
	}
}
