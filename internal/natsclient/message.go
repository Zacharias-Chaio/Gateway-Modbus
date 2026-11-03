package natsclient

import (
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

const (
	headerMessageID        = "PP-Message-ID"
	headerMessageType      = "PP-Message-Type"
	headerMessageVersion   = "PP-Message-Version"
	headerMessageTimestamp = "PP-Message-Timestamp"
	messageVersion         = "v1.0"
)

// 消息类型字面量：与 docs/message-contract-design.md 的契约一致，
// 也是平台侧（ThingsModel south.go）switch 匹配的取值。
// 以前这里散落着 "telemetry"/"cmdResult" 等魔法字符串：网关发布
// telemetry 而平台只认 data，遥测被整流丢弃却无任何告警。
const (
	messageTypeData   = "data"   // 遥测上行（网关 → NATS）
	messageTypeCmd    = "cmd"    // 控制下行（NATS → 网关，REQ/REP）
	messageTypeCmdAck = "cmdAck" // 控制执行结果（网关复用 .data 主题异步回报）
	messageTypeQuery  = "query"  // 拓扑查询（NATS → 网关，REQ/REP）
	messageTypeLog    = "log"    // 平台日志扇出（契约见 docs/powerpulse-log-standard.md）
	messageTypeAlarm  = "alarm"  // 平台告警（契约见 docs/powerpulse-alarm-standard.md）
)

// 平台横切主题：powerpulse.log / powerpulse.alarm 命名空间是平台级契约，
// 独立于网关业务主题前缀 subjectPrefix（后者只用于遥测/控制/查询）。
const (
	platformLogSubject   = "powerpulse.log.gateway"
	platformAlarmSubject = "powerpulse.alarm.gateway"
)

type envelope struct {
	ID        string
	Type      string
	Version   string
	Timestamp time.Time
	Payload   []byte
}

func newEnvelope(messageType string, payload []byte) envelope {
	return envelope{
		ID: uuid.NewString(), Type: messageType, Version: messageVersion,
		Timestamp: time.Now().UTC(), Payload: payload,
	}
}

func envelopeFromMsg(msg *nats.Msg) (envelope, error) {
	if msg.Header == nil {
		return envelope{}, fmt.Errorf("缺少消息头")
	}
	timestamp, err := strconv.ParseInt(msg.Header.Get(headerMessageTimestamp), 10, 64)
	if err != nil {
		return envelope{}, fmt.Errorf("无效消息时间戳: %w", err)
	}
	env := envelope{
		ID: msg.Header.Get(headerMessageID), Type: msg.Header.Get(headerMessageType),
		Version: msg.Header.Get(headerMessageVersion), Timestamp: time.UnixMilli(timestamp), Payload: msg.Data,
	}
	if env.ID == "" || env.Type == "" || env.Version == "" {
		return envelope{}, fmt.Errorf("消息头缺少必填字段")
	}
	return env, nil
}

func (e envelope) toMsg(subject string) *nats.Msg {
	msg := nats.NewMsg(subject)
	msg.Header.Set(headerMessageID, e.ID)
	msg.Header.Set(headerMessageType, e.Type)
	msg.Header.Set(headerMessageVersion, e.Version)
	msg.Header.Set(headerMessageTimestamp, strconv.FormatInt(e.Timestamp.UnixMilli(), 10))
	msg.Data = e.Payload
	return msg
}

type messageData struct {
	GatewayID    string             `json:"gateway_id"`
	GatewaySN    string             `json:"gateway_sn"`
	DeviceCode   string             `json:"device_code"`   // 设备唯一编码：{gateway_id}{通道索引:03d}{设备索引:03d}，如 gw_001001001
	ChannelIndex int                `json:"channel_index"` // 通道索引，从 0 开始（与通道ID Channel-{索引} 对应）
	DeviceIndex  int                `json:"device_index"`
	DeviceName   string             `json:"device_name"`
	CommNo       int                `json:"comm_no"`
	ModelID      string             `json:"model_id"`
	ModelName    string             `json:"model_name"`
	Properties   map[string]propVal `json:"properties"`
}

// deviceCode 生成设备唯一编码：{gateway_id}{通道索引:03d}{设备索引:03d}，如 gw_001001001。
//
// 索引超过 999 时定宽会被突破（编码串变长导致平台侧解析串位）。
// 这里显式截断到 3 位并打日志是不可接受的静默错误，因此改为：
// 超过 999 时改用分隔符形式，让平台侧能明确区分而不是拿到一个错位的串。
func deviceCode(gatewayID string, channelIndex, deviceIndex int) string {
	if channelIndex < 0 || channelIndex > 999 || deviceIndex < 0 || deviceIndex > 999 {
		return fmt.Sprintf("%s:%d:%d", gatewayID, channelIndex, deviceIndex)
	}
	return fmt.Sprintf("%s%03d%03d", gatewayID, channelIndex, deviceIndex)
}

type propVal struct {
	Index       int    `json:"index"` // 属性索引，与物模型属性列表中的序号一致
	Name        string `json:"name"`
	Unit        string `json:"unit"`
	Description string `json:"description"`
	AccessMode  string `json:"access_mode"`
	Value       any    `json:"value"`
	Timestamp   int64  `json:"timestamp"`
}

type messageCmd struct {
	ChannelIndex int     `json:"channel_index"` // 通道索引，从 0 开始
	DeviceIndex  int     `json:"device_index"`
	Name         string  `json:"name"`
	Value        float64 `json:"value"`
}

type messageAck struct {
	RequestID    string `json:"request_id"`
	ChannelIndex int    `json:"channel_index"`
	DeviceIndex  int    `json:"device_index"`
	Status       string `json:"status"`
	Message      string `json:"message,omitempty"`
}

type messageQuery struct {
	Type string `json:"type"`
}

type messageQueryResp struct {
	Type     string        `json:"type"`
	OK       bool          `json:"ok"`
	Message  string        `json:"message,omitempty"`
	Channels []channelInfo `json:"channels"`
}

// ChannelInfo 通道信息（对齐 store.Channel + 运行状态）。
type channelInfo struct {
	ID           string       `json:"id"`            // 通道ID，格式 Channel-{通道索引}
	ChannelIndex int          `json:"channel_index"` // 通道索引（与 data/cmd 消息路由字段一致，从 0 开始）
	Name         string       `json:"name"`
	Type         string       `json:"type"`
	Connected    bool         `json:"connected"` // 引擎在线状态
	Devices      []deviceInfo `json:"devices"`   // 挂载设备列表
}

type deviceInfo struct {
	Index     int         `json:"index"` // 设备序号，等于通道挂载列表中的 index，全局稳定
	Name      string      `json:"name"`
	CommNo    int         `json:"comm_no,omitempty"` // Modbus 从站地址，便于平台侧交叉核对
	Datasheet []dataEntry `json:"datasheet"`
}

type dataEntry struct {
	DataIndex string `json:"data_id"`
	DataName  string `json:"data_name"`
	DataRW    string `json:"data_rw"`
}

// logMessage 平台通用日志消息（契约见 docs/powerpulse-log-standard.md 第 4 节）。
type logMessage struct {
	LogID     string         `json:"log_id"`
	Source    logSource      `json:"source"`
	Level     string         `json:"level"` // debug | info | warn | error（小写）
	Mod       string         `json:"mod"`
	Msg       string         `json:"msg"`
	Attrs     map[string]any `json:"attrs,omitempty"`
	Timestamp int64          `json:"timestamp"` // Unix 毫秒
}

type logSource struct {
	Module   string `json:"module"`   // 固定 gateway，与主题二级命名空间一致
	Instance string `json:"instance"` // gw_id，消费方按此过滤单网关日志
}

// alarmEnvelope 与 alarmMessage 平台通用告警消息（契约见 docs/powerpulse-alarm-standard.md 第 4 节）。
// 结构体字段与标准保持一致；发布时 PP-Message-ID 与 alarm_id 填同一 UUID。
type alarmMessage struct {
	AlarmID   string         `json:"alarm_id"`
	Source    alarmSource    `json:"source"`
	Category  string         `json:"category"` // software（网关本期仅软件告警）
	Kind      string         `json:"kind"`     // trigger | recover | event
	Level     int            `json:"level"`    // 0 提示 / 1 一般 / 2 严重 / 3 紧急
	Timestamp int64          `json:"timestamp"`
	Count     int            `json:"count,omitempty"`
	Software  *alarmSoftware `json:"software,omitempty"`
}

type alarmSource struct {
	Module   string `json:"module"`   // 固定 gateway
	Instance string `json:"instance"` // gw_id
}

type alarmSoftware struct {
	Code      string         `json:"code"`      // 标准化事件码，见契约第 6 节
	Component string         `json:"component"` // engine / nats / runtime / store
	Message   string         `json:"message,omitempty"`
	Context   map[string]any `json:"context,omitempty"`
}
