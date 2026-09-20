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
	ChannelIndex int                `json:"channel_index"` // 通道索引，从 0 开始（与通道ID Channel-{索引} 对应）
	DeviceIndex  int                `json:"device_index"`
	DeviceName   string             `json:"device_name"`
	CommNo       int                `json:"comm_no"`
	ModelID      string             `json:"model_id"`
	ModelName    string             `json:"model_name"`
	Properties   map[string]propVal `json:"properties"`
}

type propVal struct {
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
	ID           string       `json:"id"`           // 通道ID，格式 Channel-{通道索引}
	ChannelIndex int          `json:"channel_index"` // 通道索引（与 data/cmd 消息路由字段一致，从 0 开始）
	Name         string       `json:"name"`
	Type         string       `json:"type"`
	Connected    bool         `json:"connected"` // 引擎在线状态
	Devices      []deviceInfo `json:"devices"`   // 挂载设备列表
}

type deviceInfo struct {
	Index     int         `json:"index"`
	Name      string      `json:"name"`
	Datasheet []dataEntry `json:"datasheet"`
}

type dataEntry struct {
	DataIndex string `json:"data_id"`
	DataName  string `json:"data_name"`
	DataRW    string `json:"data_rw"`
}
