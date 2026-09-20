package connector

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync"
	"time"
)

// tcpDriver 基于标准库 net.Conn 实现网络（TCP）链路。
type tcpDriver struct {
	addr        string
	dialTimeout time.Duration

	mu   sync.Mutex
	conn net.Conn
}

// newTCPDriver 依据配置构造 TCP 驱动；地址非法时返回错误。
func newTCPDriver(cfg Config) (Driver, error) {
	if cfg.DeviceIP == "" {
		return nil, errors.New("网络链路缺少设备 IP")
	}
	if cfg.DevicePort <= 0 || cfg.DevicePort > 65535 {
		return nil, errors.New("网络链路端口无效")
	}
	return &tcpDriver{
		addr:        net.JoinHostPort(cfg.DeviceIP, strconv.Itoa(cfg.DevicePort)),
		dialTimeout: 5 * time.Second,
	}, nil
}

func (d *tcpDriver) Open(ctx context.Context) error {
	dialer := net.Dialer{Timeout: d.dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", d.addr)
	if err != nil {
		return err
	}
	// 启用 TCP keepalive：无应用层报文交互时也能感知半开连接（网线拔出 / 对端掉电），
	// 由内核在探测失败后让阻塞的 Read 返回错误，触发 worker 重连。
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetKeepAlive(true)
		_ = tcp.SetKeepAlivePeriod(30 * time.Second)
	}
	d.mu.Lock()
	// 若并发下已有连接，先关旧的，避免泄漏。
	if d.conn != nil {
		_ = d.conn.Close()
	}
	d.conn = conn
	d.mu.Unlock()
	return nil
}

func (d *tcpDriver) Send(p []byte) (int, error) {
	d.mu.Lock()
	conn := d.conn
	d.mu.Unlock()
	if conn == nil {
		return 0, net.ErrClosed
	}
	return conn.Write(p)
}

func (d *tcpDriver) Receive(p []byte, timeout time.Duration) (int, error) {
	d.mu.Lock()
	conn := d.conn
	d.mu.Unlock()
	if conn == nil {
		return 0, net.ErrClosed
	}
	if timeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(timeout))
	} else {
		_ = conn.SetReadDeadline(time.Time{})
	}
	return conn.Read(p)
}

// Refresh 对 TCP 链路而言无独立缓冲可清，读掉可能残留的旧数据即可。
func (d *tcpDriver) Refresh() error {
	d.mu.Lock()
	conn := d.conn
	d.mu.Unlock()
	if conn == nil {
		return net.ErrClosed
	}
	// 非阻塞地排空当前可读的残留字节。
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
	buf := make([]byte, 512)
	for {
		if _, err := conn.Read(buf); err != nil {
			break
		}
	}
	_ = conn.SetReadDeadline(time.Time{})
	return nil
}

func (d *tcpDriver) Close() error {
	d.mu.Lock()
	conn := d.conn
	d.conn = nil
	d.mu.Unlock()
	if conn == nil {
		return nil
	}
	return conn.Close()
}
