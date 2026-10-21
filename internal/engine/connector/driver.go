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

// ErrPortClosed 表示串口/网络连接已失效，需要重连。
//
// 以前判断「是否需要重连」要把各平台底层错误文案拿去匹配关键字，
// 串口拔线返回的文本（如 "The device does not recognize the command"）
// 不在任何关键字里，于是 worker 会对着失效句柄一直空转，
// 直到人工重启进程。现在驱动层负责把致命 I/O 错误归一化成本错误，
// isLinkError 用 errors.Is 精确判断即可。
var ErrPortClosed = errors.New("链路已断开")

// 链路类型常量，与前端 / store 中的 Channel.Type 保持一致。
const (
	TypeSerial  = "Serial"
	TypeNetwork = "Network"
)

// 链路参数的校验边界与默认值。
const (
	DefaultPollIntervalMs       = 500
	DefaultReconnectIntervalMs  = 3000
	DefaultReadTimeoutMs        = 1500
	DefaultFrameTimeoutMs       = 3000
	MinPollIntervalMs           = 10
	MaxPollIntervalMs           = 3_600_000
	MaxFrameIntervalMs          = 60_000
	MaxRetries                  = 100
	DefaultWriteResponseTimeout = 1500
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
//
// 注意：本结构体与其 JSON tag 是**全仓唯一的通道配置契约**。
// plan.go 从它取 pollInterval，api 层从它做资源冲突检测，
// connector 从它构造驱动；以前这三处各有一份匿名结构体，
// 新增一个字段要改三个 Go 文件加一个前端 JS 文件，漏改不报错，
// 只会「某个功能默默不生效」。
type Config struct {
	Type string `json:"type"` // Serial / Network

	// 通用重试 / 节流参数。
	FrameInterval    int `json:"frameInterval"`    // 帧间隔（毫秒）
	ReconnectRetries int `json:"reconnectRetries"` // 连接失败重试次数，0=无限
	ResendRetries    int `json:"resendRetries"`    // 单帧重发次数
	PollInterval     int `json:"pollInterval"`     // 轮询间隔（毫秒），0 表示使用默认值

	// 帧超时（毫秒），0 表示使用默认值。1200bps 下 125 寄存器的响应帧约需 2.1s，
	// 硬编码的 3s 总超时会让低速率长帧必然超时。
	ReadTimeoutMs  int `json:"readTimeoutMs"`
	FrameTimeoutMs int `json:"frameTimeoutMs"`

	// 串口参数。
	SerialName string `json:"serialName"` // 设备节点，如 /dev/ttyS1
	BaudRate   int    `json:"baudRate"`
	DataBits   int    `json:"dataBits"`
	Parity     string `json:"parity"`   // None / Even / Odd
	StopBits   string `json:"stopBits"` // "1" / "1.5" / "2"

	// 网络参数。
	DeviceIP   string `json:"deviceIp"`
	DevicePort int    `json:"devicePort"`
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

// PollDuration 返回生效的轮询间隔。
func (c Config) PollDuration() time.Duration {
	ms := c.PollInterval
	if ms < MinPollIntervalMs {
		ms = DefaultPollIntervalMs
	}
	return time.Duration(ms) * time.Millisecond
}

// ReadTimeout 返回生效的单次读超时。
func (c Config) ReadTimeout() time.Duration {
	if c.ReadTimeoutMs > 0 {
		return time.Duration(c.ReadTimeoutMs) * time.Millisecond
	}
	return DefaultReadTimeoutMs * time.Millisecond
}

// FrameTimeout 返回生效的整帧最大等待时间。
func (c Config) FrameTimeout() time.Duration {
	if c.FrameTimeoutMs > 0 {
		return time.Duration(c.FrameTimeoutMs) * time.Millisecond
	}
	return DefaultFrameTimeoutMs * time.Millisecond
}

// FrameGap 返回帧间隔。
func (c Config) FrameGap() time.Duration {
	if c.FrameInterval <= 0 {
		return 0
	}
	if c.FrameInterval > MaxFrameIntervalMs {
		return MaxFrameIntervalMs * time.Millisecond
	}
	return time.Duration(c.FrameInterval) * time.Millisecond
}

// Validate 校验通道配置的内部一致性。
func (c Config) Validate() error {
	if c.PollInterval < 0 || c.PollInterval > MaxPollIntervalMs {
		return errors.New("pollInterval 超出允许范围（0~3600000 毫秒）")
	}
	if c.FrameInterval < 0 || c.FrameInterval > MaxFrameIntervalMs {
		return errors.New("frameInterval 超出允许范围（0~60000 毫秒）")
	}
	if c.ReconnectRetries < 0 || c.ReconnectRetries > MaxRetries {
		return errors.New("reconnectRetries 超出允许范围（0~100）")
	}
	if c.ResendRetries < 0 || c.ResendRetries > MaxRetries {
		return errors.New("resendRetries 超出允许范围（0~100）")
	}
	if c.ReadTimeoutMs < 0 || c.FrameTimeoutMs < 0 {
		return errors.New("帧超时不能为负数")
	}
	switch c.Type {
	case TypeSerial:
		if strings.TrimSpace(c.SerialName) == "" {
			return errors.New("串口链路缺少设备节点")
		}
		if c.BaudRate <= 0 {
			return errors.New("串口波特率无效")
		}
		if _, err := toParity(c.Parity); err != nil {
			return err
		}
		if _, err := toStopBits(c.StopBits); err != nil {
			return err
		}
	case TypeNetwork:
		if strings.TrimSpace(c.DeviceIP) == "" {
			return errors.New("网络链路缺少设备 IP")
		}
		if c.DevicePort <= 0 || c.DevicePort > 65535 {
			return errors.New("网络链路端口无效（1~65535）")
		}
	default:
		return ErrNotSupported
	}
	return nil
}

// ResourceKey 返回链路占用的硬件资源唯一键：串口以端口名唯一，
// 网络以 IP+端口唯一。返回空串表示无可比较的资源占用。
//
// 放在 connector 而不是 api，是为了让「配置契约」只有一个归属地。
func (c Config) ResourceKey() string {
	switch c.Type {
	case TypeSerial:
		if s := strings.TrimSpace(c.SerialName); s != "" {
			return "Serial|" + strings.ToLower(s)
		}
	case TypeNetwork:
		ip := strings.TrimSpace(c.DeviceIP)
		if ip != "" && c.DevicePort > 0 {
			return "Network|" + strings.ToLower(ip) + ":" + strconv.Itoa(c.DevicePort)
		}
	}
	return ""
}

// NewDriver 按 Config.Type 构造对应的驱动实现。
func NewDriver(cfg Config) (Driver, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
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
// 入参为裸类型，connector 不依赖存储层。
func ParseConfig(chType string, config []byte) (Config, error) {
	cfg := Config{Type: chType}
	if len(config) > 0 {
		// stopBits 历史上可能是数字（1 / 1.5 / 2）或字符串，先用中间结构接收。
		// 注意：Config.StopBits 是 string，直接 Unmarshal 数字会失败，
		// 所以必须先剔除该字段再解析其余内容。
		var probe struct {
			StopBits json.RawMessage `json:"stopBits"`
		}
		if err := json.Unmarshal(config, &probe); err != nil {
			return cfg, err
		}
		var generic map[string]json.RawMessage
		if err := json.Unmarshal(config, &generic); err != nil {
			return cfg, err
		}
		delete(generic, "stopBits")
		rest, err := json.Marshal(generic)
		if err != nil {
			return cfg, err
		}
		if err := json.Unmarshal(rest, &cfg); err != nil {
			return cfg, err
		}
		cfg.Type = chType // 以链路表上的类型为准，忽略 Config 内的副本
		cfg.StopBits = decodeStopBits(probe.StopBits)
	}
	return cfg, cfg.Validate()
}

// decodeStopBits 把 JSON 中的任意类型停止位归一化为字符串。
func decodeStopBits(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "1"
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return normalizeStopBits(s)
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		if f == float64(int64(f)) {
			return strconv.FormatInt(int64(f), 10)
		}
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return "1"
}

// normalizeStopBits 归一化停止位字符串（JSON 中可能是数字或字符串）。
func normalizeStopBits(v string) string {
	s := strings.TrimSpace(v)
	if s == "" {
		return "1"
	}
	return s
}

// NormalizeStopBitsFromAny 把 JSON 中的任意类型停止位归一化为字符串。
// 保留给历史配置（stopBits 曾被写成数字）。
func NormalizeStopBitsFromAny(v any) string {
	switch x := v.(type) {
	case string:
		return normalizeStopBits(x)
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return "1"
}
