package connector

import (
	"context"
	"errors"
	"io"
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
		return err
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
		return 0, io.ErrClosedPipe
	}
	return port.Write(p)
}

func (d *serialDriver) Receive(p []byte, timeout time.Duration) (int, error) {
	d.mu.Lock()
	port := d.port
	d.mu.Unlock()
	if port == nil {
		return 0, io.ErrClosedPipe
	}
	if timeout > 0 {
		_ = port.SetReadTimeout(timeout)
	} else {
		_ = port.SetReadTimeout(serial.NoTimeout)
	}
	return port.Read(p)
}

// Refresh 清空串口收发缓冲，用于错帧或帧不同步后的复位。
func (d *serialDriver) Refresh() error {
	d.mu.Lock()
	port := d.port
	d.mu.Unlock()
	if port == nil {
		return io.ErrClosedPipe
	}
	if err := port.ResetInputBuffer(); err != nil {
		return err
	}
	return port.ResetOutputBuffer()
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

// toParity 把配置中的校验位字符串映射为 serial 库枚举。
func toParity(s string) (serial.Parity, error) {
	switch s {
	case "", "None":
		return serial.NoParity, nil
	case "Even":
		return serial.EvenParity, nil
	case "Odd":
		return serial.OddParity, nil
	default:
		return serial.NoParity, errors.New("无效的校验位: " + s)
	}
}

// toStopBits 把配置中的停止位字符串映射为 serial 库枚举。
func toStopBits(s string) (serial.StopBits, error) {
	switch s {
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
