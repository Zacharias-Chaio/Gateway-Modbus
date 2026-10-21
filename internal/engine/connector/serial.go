package connector

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial"
)

// serialDriver 基于 go.bug.st/serial（纯 Go，无 CGO，可交叉编译）实现串口链路。
type serialDriver struct {
	name string
	mode *serial.Mode

	mu   sync.Mutex
	port serial.Port
}

// newSerialDriver 依据配置构造串口驱动并校验参数。
func newSerialDriver(cfg Config) (Driver, error) {
	if cfg.SerialName == "" {
		return nil, errors.New("串口链路缺少设备节点")
	}
	if cfg.BaudRate <= 0 {
		return nil, errors.New("串口波特率无效")
	}
	parity, err := toParity(cfg.Parity)
	if err != nil {
		return nil, err
	}
	stop, err := toStopBits(cfg.StopBits)
	if err != nil {
		return nil, err
	}
	dataBits := cfg.DataBits
	if dataBits == 0 {
		dataBits = 8
	}
	return &serialDriver{
		name: cfg.SerialName,
		mode: &serial.Mode{
			BaudRate: cfg.BaudRate,
			DataBits: dataBits,
			Parity:   parity,
			StopBits: stop,
		},
	}, nil
}

func (d *serialDriver) Open(_ context.Context) error {
	port, err := serial.Open(d.name, d.mode)
	if err != nil {
		return normalizeSerialErr(err)
	}
	d.mu.Lock()
	if d.port != nil {
		_ = d.port.Close()
	}
	d.port = port
	d.mu.Unlock()
	return nil
}

func (d *serialDriver) Send(p []byte) (int, error) {
	d.mu.Lock()
	port := d.port
	d.mu.Unlock()
	if port == nil {
		return 0, ErrPortClosed
	}
	n, err := port.Write(p)
	if err != nil {
		return n, normalizeSerialErr(err)
	}
	return n, nil
}

func (d *serialDriver) Receive(p []byte, timeout time.Duration) (int, error) {
	d.mu.Lock()
	port := d.port
	d.mu.Unlock()
	if port == nil {
		return 0, ErrPortClosed
	}
	if timeout > 0 {
		_ = port.SetReadTimeout(timeout)
	} else {
		_ = port.SetReadTimeout(serial.NoTimeout)
	}
	n, err := port.Read(p)
	if err != nil {
		return n, normalizeSerialErr(err)
	}
	return n, nil
}

// Refresh 清空串口收发缓冲，用于错帧或帧不同步后的复位。
func (d *serialDriver) Refresh() error {
	d.mu.Lock()
	port := d.port
	d.mu.Unlock()
	if port == nil {
		return ErrPortClosed
	}
	if err := port.ResetInputBuffer(); err != nil {
		return err
	}
	if err := port.ResetOutputBuffer(); err != nil {
		return err
	}
	return nil
}

func (d *serialDriver) Close() error {
	d.mu.Lock()
	port := d.port
	d.port = nil
	d.mu.Unlock()
	if port == nil {
		return nil
	}
	return port.Close()
}

// normalizeSerialErr 把串口层的致命 I/O 错误归一化为 ErrPortClosed。
//
// 这是「串口掉线后能否自愈」的关键：go.bug.st/serial 在 USB 拔出、
// 设备节点消失时返回的错误文本因平台而异（且不属于 net.Error），
// 上层靠关键字匹配永远会漏。这里在错误产生的源头统一标注。
func normalizeSerialErr(err error) error {
	if err == nil {
		return nil
	}
	// 读超时是正常控制流，不能当作断线。
	// go.bug.st/serial 在 SetReadTimeout 到期后返回 nil error + n==0，
	// 部分平台/版本会返回 os.ErrDeadlineExceeded，这里一并放行。
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return err
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		return ErrPortClosed
	}
	msg := strings.ToLower(err.Error())
	for _, kw := range serialFatalKeywords {
		if strings.Contains(msg, kw) {
			return ErrPortClosed
		}
	}
	return err
}

// serialFatalKeywords 是判定「串口已不可用」的错误文案片段（统一小写）。
var serialFatalKeywords = []string{
	"the device does not recognize the command",
	"device not configured",
	"port is closed",
	"serial port closed",
	"input/output error",
	"the system cannot find the file specified",
	"no such file or directory",
	"permission denied",
	"access is denied",
	"the handle is invalid",
	"broken pipe",
}

// toParity 把配置中的校验位字符串映射为 serial 库枚举。
func toParity(s string) (serial.Parity, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "none", "n":
		return serial.NoParity, nil
	case "even", "e":
		return serial.EvenParity, nil
	case "odd", "o":
		return serial.OddParity, nil
	default:
		return serial.NoParity, errors.New("无效的校验位: " + s)
	}
}

// toStopBits 把配置中的停止位字符串映射为 serial 库枚举。
func toStopBits(s string) (serial.StopBits, error) {
	switch strings.TrimSpace(s) {
	case "", "1":
		return serial.OneStopBit, nil
	case "1.5":
		return serial.OnePointFiveStopBits, nil
	case "2":
		return serial.TwoStopBits, nil
	default:
		return serial.OneStopBit, errors.New("无效的停止位: " + s)
	}
}
