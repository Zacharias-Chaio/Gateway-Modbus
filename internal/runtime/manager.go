// Package runtime manages restartable gateway runtime resources.
// 它是采集运行时的组合根：组装 engine、NATS 客户端与配置来源（engine.PlanSource）。
// 网关本身即以单一微服务形态部署、不再进一步拆分；如需接入远程配置中心，
// 替换 PlanSource 的实现即可，引擎与 worker 代码不变。
package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"gateway/internal/config"
	"gateway/internal/engine"
	"gateway/internal/logx"
	"gateway/internal/natsclient"

	"gorm.io/gorm"
)

// Manager owns the restartable engine and NATS client while the HTTP process remains running.
type Manager struct {
	ctx    context.Context
	db     *gorm.DB // 仅用于读写网关设置；链路/模型一律经 PlanSource 获取
	source engine.PlanSource
	log    *slog.Logger

	mu     sync.RWMutex
	engine *engine.Engine
	nats   *natsclient.Client
	cancel context.CancelFunc

	restartMu  sync.Mutex
	restarting bool
}

// New starts the first runtime instance.
func New(ctx context.Context, db *gorm.DB) (*Manager, error) {
	m := &Manager{ctx: ctx, db: db, source: &dbPlanSource{db: db}, log: logx.Module("runtime")}
	if err := m.start(); err != nil {
		return nil, err
	}
	return m, nil
}

// Restart schedules an in-process restart and returns false while one is already underway.
func (m *Manager) Restart() bool {
	m.restartMu.Lock()
	defer m.restartMu.Unlock()
	if m.restarting {
		return false
	}
	m.restarting = true
	go func() {
		defer func() {
			m.restartMu.Lock()
			m.restarting = false
			m.restartMu.Unlock()
		}()
		m.stop()
		if err := m.start(); err != nil {
			m.log.Error("网关运行时重启失败", "err", err)
			return
		}
		m.log.Info("网关运行时重启完成")
	}()
	return true
}

// Stop releases runtime resources during process shutdown.
func (m *Manager) Stop() { m.stop() }

// ConfigChanged 在模型 / 链路配置保存后由 API 层调用：
// 从 PlanSource 拉取最新配置并交给活跃引擎差量热重载。
// 运行时不存在（正在重启）时静默跳过。
func (m *Manager) ConfigChanged() {
	m.mu.RLock()
	eng := m.engine
	m.mu.RUnlock()
	if eng == nil {
		return
	}
	m.applyPlans(m.ctx, eng)
}

func (m *Manager) start() error {
	settings, err := config.EnsureSettings(m.db)
	if err != nil {
		return fmt.Errorf("初始化数据库配置: %w", err)
	}
	logx.Init(settings.App.LogOptions())
	m.log = logx.Module("runtime")

	runCtx, cancel := context.WithCancel(m.ctx)
	eng := engine.New(runCtx)
	var nats *natsclient.Client
	if settings.App.NATS.Enabled {
		nats, err = natsclient.New(runCtx, settings.App.Gateway, settings.App.NATS, m.source, eng)
		if err != nil {
			m.log.Warn("启动 NATS 客户端失败，数据扇出功能已禁用", "err", err)
		} else {
			eng.SetEventSink(nats)
		}
	}

	m.applyPlans(runCtx, eng)

	m.mu.Lock()
	m.engine, m.nats, m.cancel = eng, nats, cancel
	m.mu.Unlock()
	return nil
}

// applyPlans 拉取配置源的全部链路/模型，构建采集计划并热重载引擎。
func (m *Manager) applyPlans(ctx context.Context, eng *engine.Engine) {
	channels, err := m.source.LoadChannels(ctx)
	if err != nil {
		m.log.Warn("加载链路配置失败", "err", err)
	}
	models, err := m.source.LoadModels(ctx)
	if err != nil {
		m.log.Warn("加载设备模型失败", "err", err)
	}
	plans, warnings := engine.BuildPlans(channels, models)
	for _, warning := range warnings {
		m.log.Warn("采集计划构建警告", "warn", warning)
	}
	eng.Apply(plans)
}

func (m *Manager) stop() {
	m.mu.Lock()
	eng, nats, cancel := m.engine, m.nats, m.cancel
	m.engine, m.nats, m.cancel = nil, nil, nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if eng != nil {
		eng.Stop()
	}
	if nats != nil {
		nats.Close()
	}
}

func (m *Manager) current() *engine.Engine {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.engine
}

// Submit delegates a command to the active engine.
func (m *Manager) Submit(channelIndex int, command engine.WriteCommand) bool {
	eng := m.current()
	return eng != nil && eng.Submit(channelIndex, command)
}

// Values returns a real-time value snapshot from the active engine.
func (m *Manager) Values(channelIndex int) map[string]engine.SessionEntry {
	if eng := m.current(); eng != nil {
		return eng.Values(channelIndex)
	}
	return map[string]engine.SessionEntry{}
}

// CommunicationSnapshot returns communication data from the active engine.
func (m *Manager) CommunicationSnapshot(channelIndex, deviceIndex int, afterSeq uint64, limit int) (engine.CommunicationSnapshot, bool) {
	if eng := m.current(); eng != nil {
		return eng.CommunicationSnapshot(channelIndex, deviceIndex, afterSeq, limit)
	}
	return engine.CommunicationSnapshot{}, false
}

// Status returns the active engine status for the HTTP status endpoint.
func (m *Manager) Status() any {
	if eng := m.current(); eng != nil {
		return eng.Status()
	}
	return []any{}
}
