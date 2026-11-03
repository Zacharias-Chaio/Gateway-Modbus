package connector

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"syscall"
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
// 同时保留原错误链（Join）让上层仍可 errors.Is/As 判断。
// 读超时（deadline exceeded）不属于断线，必须原样返回。
//
// ⚠ Windows 陷阱（本函数曾经因此漏判，worker 对死连接无限空转）：
//   - syscall.ECONNRESET 等 POSIX 名在 Windows 上是**虚构值**
//     （APPLICATION_ERROR+iota，约 1<<29+35），不等于真实的
//     WSAECONNRESET(10054)。errors.Is 永远匹配不上。
//   - 真实值是 syscall.WSAECONNRESET / WSAECONNABORTED，
//     必须用 errors.As 取出 syscall.Errno 后直接比较。
//   - os.SyscallError（"wsasend: ..."）的 Unwrap 会露出内层 Errno，
//     所以 errors.As 能穿透。
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
	if isConnReset(err) {
		return errors.Join(ErrPortClosed, err)
	}
	return err
}

// isConnReset 判断是否为连接被对端重置/中止类错误（跨平台）。
//
// Linux/POSIX：ECONNRESET(104)/ECONNABORTED(103)/ENETRESET(102)
// Windows：WSAECONNRESET(10054)/WSAECONNABORTED(10053)
// 两套值都用 syscall.Errno 数值直接比较（errors.As 穿透 os.SyscallError
// 的 Unwrap 链取出 Errno），无需构建标签，三个目标平台统一编译。
func isConnReset(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case syscall.ECONNRESET, syscall.ECONNABORTED, syscall.ENETRESET:
		// POSIX 真实值：Linux 构建下三个名字即真实错误码；
		// Windows 构建下这些名字是虚构值，但 WSA* 分支兜住了 Windows。
		return true
	case 10054, 10053: // WSAECONNRESET / WSAECONNABORTED：Windows 真实值
		return true
	}
	// TCP 重传超时（Linux ETIMEDOUT / Windows WSAETIMEDOUT=10060）也是断线：
	// 重传耗尽说明链路已死，重发无意义。注意不能走 net.Error.Timeout()
	// 判定——Windows 上 Errno.Timeout() 对 ETIMEDOUT 返回 true，会与
	// 读 deadline 超时混淆；读超时在前面的 net.Error 分支已被放行，
	// 能走到这里的 ETIMEDOUT 只可能是写/重传超时。
	if errors.Is(err, syscall.ETIMEDOUT) || errno == 10060 {
		return true
	}
	return false
}
