// Package natsclient adapts engine events to the gateway's Core NATS contract.
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

	"github.com/nats-io/nats.go"
)

// Client publishes engine events and serves the cmd and query subjects.
type Client struct {
	config       config.NATS
	gateway      config.Gateway
	source       engine.PlanSource
	engine       *engine.Engine
	log          *slog.Logger
	conn         *nats.Conn
	data         string
	cmd          string
	query        string
	events       chan any
	eventsMu     sync.Mutex
	eventsClosed bool
	stopOnce     sync.Once
	done         chan struct{}
}

// New connects to NATS and starts the event publisher and request subscriptions.
// source 提供链路与设备模型配置（拓扑查询用），北向客户端不直接依赖存储层。
func New(ctx context.Context, gateway config.Gateway, cfg config.NATS, source engine.PlanSource, eng *engine.Engine) (*Client, error) {
	if gateway.GWID == "" || strings.Contains(gateway.GWID, ".") {
		return nil, fmt.Errorf("无效 gateway.gw_id")
	}
	if cfg.SubjectPrefix == "" {
		return nil, fmt.Errorf("nats.subjectPrefix 不能为空")
	}
	prefix := strings.TrimSuffix(cfg.SubjectPrefix, ".") + "." + gateway.GWID
	client := &Client{
		config: cfg, gateway: gateway, source: source, engine: eng, log: logx.Module("nats"),
		data: prefix + ".data", cmd: prefix + ".cmd", query: prefix + ".query",
		events: make(chan any, queueSize(cfg.QueueSize)), done: make(chan struct{}),
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
	var messageType string
	var payload any
	switch value := event.(type) {
	case engine.TelemetryEvent:
		messageType = "data"
		properties := make(map[string]propVal, len(value.Properties))
		for id, prop := range value.Properties {
			properties[id] = propVal{Index: prop.Index, Name: prop.Name, Unit: prop.Unit, Description: prop.Description, AccessMode: prop.AccessMode, Value: prop.Value, Timestamp: prop.Timestamp.UnixMilli()}
		}
		payload = messageData{GatewayID: c.gateway.GWID, GatewaySN: c.gateway.SN,
			DeviceCode:   deviceCode(c.gateway.GWID, value.ChannelIndex, value.DeviceIndex),
			ChannelIndex: value.ChannelIndex, DeviceIndex: value.DeviceIndex,
			DeviceName: value.DeviceName, CommNo: value.CommNo, ModelID: value.ModelID,
			ModelName: value.ModelName, Properties: properties}
	case engine.WriteResultEvent:
		messageType = "cmdAck"
		status := "success"
		if !value.OK {
			status = "failure"
		}
		payload = messageAck{RequestID: value.RequestID, ChannelIndex: value.ChannelIndex,
			DeviceIndex: value.DeviceIndex, Status: status, Message: value.Error}
	default:
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
	if err != nil || env.Type != "cmd" {
		c.respondAck(msg, messageAck{Status: "failure", Message: "无效控制消息"})
		return
	}
	var command messageCmd
	// 通道索引从 0 开始，0 是合法值；仅负数或缺参视为非法。
	if err := json.Unmarshal(env.Payload, &command); err != nil || command.Name == "" || command.ChannelIndex < 0 {
		c.respondAck(msg, messageAck{RequestID: env.ID, ChannelIndex: command.ChannelIndex, DeviceIndex: command.DeviceIndex, Status: "failure", Message: "无效控制参数"})
		return
	}
	if !c.engine.HasDevice(command.ChannelIndex, command.DeviceIndex) {
		c.respondAck(msg, messageAck{RequestID: env.ID, ChannelIndex: command.ChannelIndex, DeviceIndex: command.DeviceIndex, Status: "failure", Message: "通道或设备不存在"})
		return
	}
	accepted := c.engine.Submit(command.ChannelIndex, engine.WriteCommand{
		RequestID: env.ID, DeviceIndex: command.DeviceIndex, PropName: command.Name, RawValue: command.Value,
	})
	status, message := "accepted", ""
	if !accepted {
		status, message = "failure", "写命令队列已满"
	}
	c.respondAck(msg, messageAck{RequestID: env.ID, ChannelIndex: command.ChannelIndex, DeviceIndex: command.DeviceIndex, Status: status, Message: message})
}

func (c *Client) respondAck(msg *nats.Msg, response messageAck) {
	if msg.Reply == "" {
		return
	}
	data, err := json.Marshal(response)
	if err == nil {
		_ = c.conn.PublishMsg(newEnvelope("cmdAck", data).toMsg(msg.Reply))
	}
}

func (c *Client) handleQuery(msg *nats.Msg) {
	env, err := envelopeFromMsg(msg)
	if err != nil || env.Type != "query" {
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
	if err == nil {
		_ = c.conn.PublishMsg(newEnvelope("queryResp", data).toMsg(msg.Reply))
	}
}

func (c *Client) topology() ([]channelInfo, error) {
	channels, err := c.source.LoadChannels(context.Background())
	if err != nil {
		return nil, fmt.Errorf("读取链路: %w", err)
	}
	models, err := c.source.LoadModels(context.Background())
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
		_ = json.Unmarshal(channel.Devices, &mounts)
		info := channelInfo{ID: channel.ID, ChannelIndex: channel.Index, Name: channel.Name, Type: channel.Type, Connected: c.engine.Connected(channel.Index)}
		for _, mount := range mounts {
			device := deviceInfo{Index: mount.Index, Name: mount.Name}
			if model, ok := modelMap[mount.ModelID]; ok {
				var properties []converter.PropMeta
				if json.Unmarshal(model.Properties, &properties) == nil {
					device.Datasheet = make([]dataEntry, 0, len(properties))
					for _, property := range properties {
						device.Datasheet = append(device.Datasheet, dataEntry{DataIndex: property.PropID, DataName: property.Name, DataRW: property.AccessMode})
					}
				}
			}
			info.Devices = append(info.Devices, device)
		}
		result = append(result, info)
	}
	return result, nil
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
