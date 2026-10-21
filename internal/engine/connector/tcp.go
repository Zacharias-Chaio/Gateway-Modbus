package connector

import (
	"context"
	"errors"
	"io"
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
		return 0, ErrPortClosed
	}
	n, err := conn.Write(p)
	if err != nil {
		return n, normalizeNetErr(err)
	}
	return n, nil
}

func (d *tcpDriver) Receive(p []byte, timeout time.Duration) (int, error) {
	d.mu.Lock()
	conn := d.conn
	d.mu.Unlock()
	if conn == nil {
		return 0, ErrPortClosed
	}
	if timeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(timeout))
	} else {
		_ = conn.SetReadDeadline(time.Time{})
	}
	n, err := conn.Read(p)
	if err != nil {
		return n, normalizeNetErr(err)
	}
	return n, nil
}

// Refresh 对 TCP 链路而言无独立缓冲可清，读掉可能残留的旧数据即可。
func (d *tcpDriver) Refresh() error {
	d.mu.Lock()
	conn := d.conn
	d.mu.Unlock()
	if conn == nil {
		return ErrPortClosed
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

// normalizeNetErr 把「对端关闭/连接重置」归一化为 ErrPortClosed，
// 同时保留原错误链（%w）让上层仍可 errors.Is/As 判断。
// 读超时（deadline exceeded）不属于断线，必须原样返回。
func normalizeNetErr(err error) error {
	if err == nil {
		return nil
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return err // 读超时：正常控制流
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
		return errors.Join(ErrPortClosed, err)
	}
	return err
}
