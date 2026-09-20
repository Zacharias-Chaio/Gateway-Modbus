// Package connector 封装链路连接器：通过统一 Driver 接口屏蔽串口 / 网络的物理差异。
package connector

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// ErrNotSupported 表示该链路类型的驱动尚未实现。
var ErrNotSupported = errors.New("该链路类型暂不支持")

// 链路类型常量，与前端 / store 中的 Channel.Type 保持一致。
const (
	TypeSerial  = "Serial"
	TypeNetwork = "Network"
)

// Driver 是屏蔽 serial / tcp 差异的统一链路抽象。
// 一个 Driver 实例对应一条物理链路，由所属的 worker goroutine 独占使用，
// 因此实现本身无需保证并发安全。
type Driver interface {
	// Open 建立底层连接（打开串口 / 拨号 TCP）。
	// ctx 用于取消尚在进行中的连接动作。
	Open(ctx context.Context) error
	// Send 发送一帧字节，返回实际写入的字节数。
	Send(p []byte) (int, error)
	// Receive 读取字节到 p，最多阻塞 timeout；超时返回 os.ErrDeadlineExceeded 或等价错误。
	Receive(p []byte, timeout time.Duration) (int, error)
	// Refresh 刷新链路：清空底层收发缓冲，用于错帧后复位。
	Refresh() error
	// Close 关闭底层连接，可重复调用。
	Close() error
}

// Config 是从 store.Channel.Config（JSON）解析出的驱动参数，
// 使 engine 与前端 / 存储的字段命名解耦。
type Config struct {
	Type string // Serial / Network

	// 通用重试 / 节流参数。
	FrameInterval    int // 帧间隔（毫秒）
	ReconnectRetries int // 连接失败重试次数
	ResendRetries    int // 单帧重发次数
	PollInterval     int // 轮询间隔（毫秒），0 表示使用默认值

	// 串口参数。
	SerialName string // 设备节点，如 /dev/ttyS1
	BaudRate   int
	DataBits   int
	Parity     string // None / Even / Odd
	StopBits   string // "1" / "1.5" / "2"

	// 网络参数。
	DeviceIP   string
	DevicePort int
}

// Target 返回链路的目标地址描述，用于日志与 Info。
func (c Config) Target() string {
	switch c.Type {
	case TypeSerial:
		return c.SerialName
	case TypeNetwork:
		if c.DeviceIP == "" {
			return ""
		}
		return c.DeviceIP + ":" + strconv.Itoa(c.DevicePort)
	}
	return ""
}

// NewDriver 按 Config.Type 构造对应的驱动实现。
func NewDriver(cfg Config) (Driver, error) {
	switch cfg.Type {
	case TypeSerial:
		return newSerialDriver(cfg)
	case TypeNetwork:
		return newTCPDriver(cfg)
	default:
		return nil, ErrNotSupported
	}
}

// ParseConfig 把链路类型与配置 JSON 解析成驱动可用的 Config。
// Config 的 JSON 字段命名与前端 buildChannelConfig 保持一致；
// 入参为裸类型，connector 不依赖存储层。
func ParseConfig(chType string, config []byte) (Config, error) {
	cfg := Config{Type: chType}
	if len(config) == 0 {
		return cfg, nil
	}
	var raw struct {
		FrameInterval    *int   `json:"frameInterval"`
		ReconnectRetries *int   `json:"reconnectRetries"`
		ResendRetries    *int   `json:"resendRetries"`
		PollInterval     *int   `json:"pollInterval"`
		SerialName       string `json:"serialName"`
		BaudRate         *int   `json:"baudRate"`
		DataBits         *int   `json:"dataBits"`
		Parity           string `json:"parity"`
		StopBits         any    `json:"stopBits"`
		DeviceIP         string `json:"deviceIp"`
		DevicePort       *int   `json:"devicePort"`
	}
	if err := json.Unmarshal(config, &raw); err != nil {
		return cfg, err
	}
	cfg.FrameInterval = deref(raw.FrameInterval)
	cfg.ReconnectRetries = deref(raw.ReconnectRetries)
	cfg.ResendRetries = deref(raw.ResendRetries)
	cfg.PollInterval = deref(raw.PollInterval)
	cfg.SerialName = raw.SerialName
	cfg.BaudRate = deref(raw.BaudRate)
	cfg.DataBits = deref(raw.DataBits)
	cfg.Parity = raw.Parity
	cfg.StopBits = stopBitsString(raw.StopBits)
	cfg.DeviceIP = raw.DeviceIP
	cfg.DevicePort = deref(raw.DevicePort)
	return cfg, nil
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// stopBitsString 把 JSON 中可能为数字或字符串的 stopBits 归一化为字符串。
func stopBitsString(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return ""
}
