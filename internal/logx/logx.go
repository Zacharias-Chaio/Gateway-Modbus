// Package logx 是网关的统一日志管理器：以标准库 log/slog 为核心，
// 将一条日志同时扇出到三路出口——终端窗口、滚动文件、前端出口接口。
//
//	业务代码 logx.Module("channel").Info(...)
//	          └─ swapHandler ─┬─ TextHandler  → 终端(os.Stdout)
//	                          ├─ JSONHandler  → 滚动文件(大小 + 每日轮转)
//	                          └─ sinkHandler  → 环形缓冲 + SSE（/api/syslog）
package logx

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options 日志管理器初始化参数。
type Options struct {
	Level       string // debug | info | warn | error（大小写不敏感）
	Console     bool   // 终端窗口输出
	File        string // 文件输出路径（空则不落文件）
	MaxSizeMB   int    // 单文件大小上限（MB），大小轮转阈值
	MaxBackups  int    // 保留历史文件份数
	MaxAgeDays  int    // 历史文件保留天数
	Compress    bool   // 历史文件 gzip 压缩
	DailyRotate bool   // 每日 00:00 轮转
	BufferSize  int    // 前端出口环形缓冲条数
}

type runtimeState struct {
	logger       *slog.Logger
	sink         *ringSink
	swap         *swapHandler
	stopRotation func()
	oldWriter    *closerHolder
}

var (
	mu      sync.RWMutex
	initMu  sync.Mutex
	runtime runtimeState
)

// Init 按配置装配三路输出并设置为全局默认日志器。可重复调用（热重载配置）。
//
// 关键点：全局只保留一个 swapHandler，Init 只替换它的内层 handler。
// 这样 engine / worker / nats 这些在构造时就捕获了 logger 的组件，
// 也会立刻用上新配置。
func Init(opt Options) {
	initMu.Lock()
	defer initMu.Unlock()

	level := ParseLevel(opt.Level)
	var handlers []slog.Handler

	// 1) 终端窗口：文本格式，便于人读。
	if opt.Console {
		handlers = append(handlers, slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	}

	// 2) 滚动文件：JSON 格式，便于后续采集/检索；大小 + 每日轮转。
	var stopRotation func()
	var newWriter *closerHolder
	if opt.File != "" {
		writer, stop := newRotator(opt)
		stopRotation = stop
		handlers = append(handlers, slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: level}))
		if cw, ok := writer.(closableWriter); ok {
			newWriter = &closerHolder{w: cw}
		}
	}

	// 3) 前端出口：复用已有缓冲，避免热重载中断已建立的 SSE 订阅。
	mu.Lock()
	s := runtime.sink
	if s == nil {
		s = newRingSink(opt.BufferSize)
	} else {
		s.resize(opt.BufferSize)
	}
	previous := runtime
	mu.Unlock()
	handlers = append(handlers, s.handler(level))

	// 4) NATS 出口：常驻 handler，writer 未安装时自身短路。
	// 放在 fanout 尾部：即便日志洪泛，也不会阻塞其余三路（各子 handler 独立拷贝）。
	handlers = append(handlers, natsHandler{})

	var inner slog.Handler
	if len(handlers) == 1 {
		inner = handlers[0]
	} else {
		inner = &fanout{handlers: handlers}
	}

	mu.Lock()
	if runtime.swap != nil {
		// 热重载：只换内层，保留已分发的 logger。
		runtime.swap.Swap(inner)
		runtime.stopRotation = stopRotation
		runtime.oldWriter = newWriter
	} else {
		swap := newSwapHandler(inner)
		runtime = runtimeState{
			logger: slog.New(swap), sink: s, swap: swap,
			stopRotation: stopRotation, oldWriter: newWriter,
		}
		slog.SetDefault(runtime.logger)
	}
	mu.Unlock()

	// 停掉旧的轮转 goroutine，并关闭旧的写入器句柄。
	// 以前只停 goroutine、从不 Close 旧的 lumberjack：每次保存设置就泄漏一个
	// 文件句柄，且新旧两个 lumberjack 同时写同一文件，轮转时互相踩踏丢日志。
	if previous.stopRotation != nil {
		previous.stopRotation()
	}
	if previous.oldWriter != nil {
		previous.oldWriter.Close()
	}
}

// Shutdown 关闭日志资源（进程退出前调用，确保缓冲落盘）。
func Shutdown() {
	initMu.Lock()
	defer initMu.Unlock()

	mu.Lock()
	prev := runtime
	mu.Unlock()

	if prev.stopRotation != nil {
		prev.stopRotation()
	}
	if prev.oldWriter != nil {
		prev.oldWriter.Close()
	}
}

// Module 返回带模块标签的“分支”日志器；name 会作为 mod 字段附加到每条日志。
// 未初始化时回退到 slog 默认器，保证任何时刻调用都安全。
func Module(name string) *slog.Logger {
	mu.RLock()
	logger := runtime.logger
	mu.RUnlock()
	if logger == nil {
		return slog.Default().With("mod", name)
	}
	return logger.With("mod", name)
}

// ParseLevel 把级别字符串解析为 slog.Level，未知值回退 Info。
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// RequestLogLevel 决定一条 HTTP 访问日志的级别：
// 轮询类只读接口（realtime / comm-monitor / syslog / engine status / system-info）降到 Debug，
// 否则一个打开的页面就能以 2Hz 的速度刷满日志文件。
func RequestLogLevel(method, path string, status int) slog.Level {
	if status >= 500 {
		return slog.LevelError
	}
	if status >= 400 {
		return slog.LevelWarn
	}
	if method == http.MethodGet {
		switch {
		case strings.HasPrefix(path, "/api/realtime"),
			strings.HasPrefix(path, "/api/comm-monitor"),
			strings.HasPrefix(path, "/api/syslog"),
			strings.HasPrefix(path, "/api/engine/status"),
			strings.HasPrefix(path, "/api/system-info"),
			strings.HasPrefix(path, "/healthz"),
			strings.HasPrefix(path, "/readyz"):
			return slog.LevelDebug
		}
	}
	return slog.LevelInfo
}

// 保持 time 引用（部分构建下用于未来扩展的时间窗采样）。
var _ = time.Now

// NATSWriter 是日志第四路出口（NATS 扇出）的接收方。
//
// 依赖倒置：logx 不能 import natsclient（natsclient 依赖 logx），由 runtime 组合根
// 在 NATS 客户端创建成功后通过 SetNATSWriter 注入实现。nil 表示出口未启用。
type NATSWriter interface {
	// WriteLog 必须非阻塞；实现方自行做级别过滤与限频（契约见 docs/powerpulse-log-standard.md）。
	WriteLog(level slog.Level, mod, msg string, attrs map[string]any, at time.Time)
}

var natsWriter atomic.Pointer[NATSWriter]

// SetNATSWriter 安装/移除 NATS 日志出口。传 nil 即停用（runtime 停止 NATS 客户端时调用）。
// 不加锁：指针读写原子，且出口侧自身必须容忍并发调用。
func SetNATSWriter(w NATSWriter) {
	if w == nil {
		natsWriter.Store(nil)
		return
	}
	natsWriter.Store(&w)
}

// natsHandler 把每条日志转交 NATS 出口。它常驻 handler 列表（Init 每次都装配），
// 每条日志时动态查 writer——writer 为 nil 时零开销短路。这样 logx 热重载
// 与 NATS 客户端重建（runtime restart）两个生命周期互不干扰。
type natsHandler struct{}

func (natsHandler) Enabled(context.Context, slog.Level) bool { return true }

func (natsHandler) Handle(_ context.Context, r slog.Record) error {
	// 注意：原子指针本身可能为 nil（出口从未安装/已移除），
	// 必须先判指针再解引用——写成 *natsWriter.Load() 会在 nil 上直接 panic
	// （零值或 SetNATSWriter(nil) 后 Load 返回 nil 指针，不是 nil 接口）。
	p := natsWriter.Load()
	if p == nil || *p == nil {
		return nil
	}
	w := *p
	// 标量化 attrs：契约规定值仅标量（error→字符串、非标量 fmt.Sprint）。
	attrs := make(map[string]any, r.NumAttrs())
	mod := ""
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "mod" {
			mod = a.Value.String()
			return true
		}
		v := a.Value.Any()
		switch v.(type) {
		case string, int, int8, int16, int32, int64,
			uint, uint8, uint16, uint32, uint64,
			float32, float64, bool, nil:
			attrs[a.Key] = v
		case error:
			attrs[a.Key] = v.(error).Error()
		case time.Duration:
			attrs[a.Key] = v.(time.Duration).String()
		default:
			attrs[a.Key] = fmt.Sprint(v)
		}
		return true
	})
	w.WriteLog(r.Level, mod, r.Message, attrs, r.Time)
	return nil
}

func (natsHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return natsHandler{} }
func (natsHandler) WithGroup(string) slog.Handler            { return natsHandler{} }
