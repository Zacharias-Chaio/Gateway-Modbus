package logx

import (
	"context"
	"io"
	"log/slog"
	"sync"
)

// swapHandler 是一个「永远转发」的 slog.Handler：它本身不实现任何格式化，
// 只把记录转交给内部可原子替换的真实 handler。
//
// 为什么需要它：engine / worker / natsclient 都在构造时调用 Module("x") 拿到
// 一个固定的 *slog.Logger。如果 Init 每次都重建 handler 并替换全局默认器，
// 这些长生命周期组件手里的旧 logger 仍然指向旧 handler——保存日志设置后
// 想临时开 debug 会「看起来没反应」，而且新旧两个 lumberjack 实例会同时写
// 同一个文件，轮转时互相踩踏。
//
// 用 swapHandler 包一层后，Init 只需替换内层 handler，所有已分发的 logger
// 立刻生效。
type swapHandler struct {
	mu   sync.RWMutex
	curr slog.Handler
}

func newSwapHandler(h slog.Handler) *swapHandler {
	return &swapHandler{curr: h}
}

// Swap 替换内层 handler，并返回旧的 handler 供调用方延迟关闭底层资源。
func (s *swapHandler) Swap(h slog.Handler) slog.Handler {
	s.mu.Lock()
	old := s.curr
	s.curr = h
	s.mu.Unlock()
	return old
}

// current 返回当前内层 handler。
func (s *swapHandler) current() slog.Handler {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.curr
}

func (s *swapHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if h := s.current(); h != nil {
		return h.Enabled(ctx, level)
	}
	return false
}

func (s *swapHandler) Handle(ctx context.Context, r slog.Record) error {
	if h := s.current(); h != nil {
		return h.Handle(ctx, r.Clone())
	}
	return nil
}

// WithAttrs 返回的派生 handler 内部包着一个 swapHandler，
// 这样 Module("engine") 产生的分支在 Init 之后依然能看到新配置。
func (s *swapHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &derivedHandler{parent: s, attrs: attrs}
}

func (s *swapHandler) WithGroup(name string) slog.Handler {
	if h := s.current(); h != nil {
		return h.WithGroup(name)
	}
	return s
}

// derivedHandler 是「延迟绑定」的派生 handler：每次写日志时
// 才向父 swapHandler 取当前配置。
type derivedHandler struct {
	parent *swapHandler
	attrs  []slog.Attr
}

func (d *derivedHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return d.parent.Enabled(ctx, level)
}

func (d *derivedHandler) Handle(ctx context.Context, r slog.Record) error {
	h := d.parent.current()
	if h == nil {
		return nil
	}
	return h.WithAttrs(d.attrs).Handle(ctx, r.Clone())
}

func (d *derivedHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(d.attrs)+len(attrs))
	merged = append(merged, d.attrs...)
	merged = append(merged, attrs...)
	return &derivedHandler{parent: d.parent, attrs: merged}
}

func (d *derivedHandler) WithGroup(name string) slog.Handler {
	h := d.parent.current()
	if h == nil {
		return d
	}
	return h.WithAttrs(d.attrs).WithGroup(name)
}

// closableWriter 是可关闭的 io.Writer（lumberjack.Logger 满足）。
type closableWriter interface {
	io.Writer
	Close() error
}

// closerHolder 记录需要在下次 Init 时关闭的旧写入器。
type closerHolder struct {
	once sync.Once
	w    closableWriter
}

func (c *closerHolder) Close() {
	if c == nil || c.w == nil {
		return
	}
	c.once.Do(func() {
		_ = c.w.Close()
	})
}

// atomicLevel 由 slog.LevelVar 取代（标准库已提供并发安全的热更新级别）。
// 保留占位说明，避免误以为需要自建实现。

// fanout 是一个把单条记录分发到多个子 handler 的 slog.Handler。
type fanout struct {
	handlers []slog.Handler
}

func (f *fanout) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range f.handlers {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (f *fanout) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range f.handlers {
		if h.Enabled(ctx, r.Level) {
			// 每个子 handler 拿到独立副本，避免 Attrs 迭代相互影响。
			_ = h.Handle(ctx, r.Clone())
		}
	}
	return nil
}

func (f *fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		next[i] = h.WithAttrs(attrs)
	}
	return &fanout{handlers: next}
}

func (f *fanout) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		next[i] = h.WithGroup(name)
	}
	return &fanout{handlers: next}
}

// 确保实现接口。
var (
	_ slog.Handler = (*fanout)(nil)
	_ slog.Handler = (*swapHandler)(nil)
	_ slog.Handler = (*derivedHandler)(nil)
)
