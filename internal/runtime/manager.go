// Package runtime manages restartable gateway runtime resources.
// 它是采集运行时的组合根：组装 engine、NATS 客户端与配置来源（engine.PlanSource）。
// 网关本身即以单一微服务形态部署、不再进一步拆分；如需接入远程配置中心，
// 替换 PlanSource 的实现即可，引擎与 worker 代码不变。
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gateway/internal/config"
	"gateway/internal/engine"
	"gateway/internal/logx"
	"gateway/internal/natsclient"

	"gorm.io/gorm"
)

// State 描述运行时的生命周期状态。
type State int32

const (
	StateIdle State = iota
	StateStarting
	StateRunning
	StateStopping
)

func (s State) String() string {
	switch s {
	case StateStarting:
		return "starting"
	case StateRunning:
		return "running"
	case StateStopping:
		return "stopping"
	default:
		return "idle"
	}
}

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
	state  State

	// configDirty 记录「重启/启动窗口内到达的配置变更」，
	// 在启动完成后再补投一次，避免变更被静默丢弃。
	configDirty bool

	restartMu  sync.Mutex
	restarting bool

	// 最近一次配置生效结果，暴露到 /api/engine/status。
	lastApplyAt  time.Time
	lastApplyErr string
	warnings     []string
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
			m.log.Error("网关运行时重启失败，运行时保持停止状态", "err", err)
			m.setApplyError(fmt.Errorf("运行时重启失败: %w", err))
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
//
// 启动/重启窗口内到达的变更会被标记为 dirty 并在启动完成后补投，
// 而不是静默丢弃（以前 eng==nil 就直接 return，用户会看到「保存成功但永不生效」）。
func (m *Manager) ConfigChanged() {
	m.mu.Lock()
	eng := m.engine
	state := m.state
	m.mu.Unlock()

	if eng == nil || state != StateRunning {
		m.mu.Lock()
		m.configDirty = true
		m.mu.Unlock()
		m.log.Info("配置变更已记录，将在运行时启动后生效", "state", state.String())
		return
	}
	if err := m.applyPlans(m.ctx, eng); err != nil {
		m.setApplyError(err)
	}
}

// ConfigChangedErr 与 ConfigChanged 相同，但把错误返回给 API，便于回传 503。
func (m *Manager) ConfigChangedErr() error {
	m.mu.Lock()
	eng := m.engine
	state := m.state
	m.mu.Unlock()

	if eng == nil || state != StateRunning {
		m.mu.Lock()
		m.configDirty = true
		m.mu.Unlock()
		return fmt.Errorf("运行时正在%s，配置已保存，将在启动后生效", state.String())
	}
	if err := m.applyPlans(m.ctx, eng); err != nil {
		m.setApplyError(err)
		return err
	}
	return nil
}

func (m *Manager) start() error {
	m.mu.Lock()
	m.state = StateStarting
	m.mu.Unlock()

	settings, err := config.EnsureSettings(m.db)
	if err != nil {
		m.mu.Lock()
		m.state = StateIdle
		m.mu.Unlock()
		return fmt.Errorf("初始化数据库配置: %w", err)
	}
	logx.Init(settings.App.LogOptions())
	m.log = logx.Module("runtime")

	runCtx, cancel := context.WithCancel(m.ctx)
	eng := engine.New(runCtx)
	var nats *natsclient.Client
	if settings.App.NATS.Enabled {
		// 接入 Token 从 gateway_provisioning 独立表读取：它在首次/重新配置时写入，
		// 与 gateway_settings 分表存放，避免经 /api/settings 回传泄漏。
		// 读取失败按未配置处理（空串随消息扇出），不阻断北向启动。
		gatewayToken := ""
		if record, err := config.LoadProvisioning(m.db); err == nil {
			gatewayToken = record.Token
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			m.log.Warn("读取接入 Token 失败，遥测将以空 token 扇出", "err", err)
		}
		nats, err = natsclient.New(runCtx, settings.App.Gateway, gatewayToken, settings.App.NATS, m.source, eng)
		if err != nil {
			// 北向扇出已不可用：告警也发不出去（走 NATS），此处只留本地日志。
			m.log.Warn("启动 NATS 客户端失败，数据扇出功能已禁用", "err", err)
		} else {
			eng.SetEventSink(nats)
			eng.SetAlarmPublisher(nats.AlarmPublisher())
		}
	}

	// 配置读取失败时绝不能用空计划去调谐：那会把「一次数据库抖动」
	// 放大成「全部链路停机」。此时保留运行时停止状态并明确报错，
	// 让 API 与 /engine/status 都能看到原因。
	if err := m.applyPlans(runCtx, eng); err != nil {
		cancel()
		if nats != nil {
			nats.Close()
		}
		m.mu.Lock()
		m.state = StateIdle
		m.mu.Unlock()
		return err
	}

	m.mu.Lock()
	m.engine, m.nats, m.cancel = eng, nats, cancel
	m.state = StateRunning
	dirty := m.configDirty
	m.configDirty = false
	m.mu.Unlock()

	if dirty {
		m.log.Info("补投启动窗口内到达的配置变更")
		if err := m.applyPlans(runCtx, eng); err != nil {
			m.setApplyError(err)
		}
	}
	return nil
}

// applyPlans 拉取配置源的全部链路/模型，构建采集计划并热重载引擎。
func (m *Manager) applyPlans(ctx context.Context, eng *engine.Engine) error {
	channels, err := m.source.LoadChannels(ctx)
	if err != nil {
		return fmt.Errorf("加载链路配置失败（保持现有运行状态）: %w", err)
	}
	models, err := m.source.LoadModels(ctx)
	if err != nil {
		return fmt.Errorf("加载设备模型失败（保持现有运行状态）: %w", err)
	}
	plans, warnings, err := engine.BuildPlans(channels, models)
	if err != nil {
		return fmt.Errorf("构建采集计划失败（保持现有运行状态）: %w", err)
	}
	for _, warning := range warnings {
		m.log.Warn("采集计划构建警告", "warn", warning)
	}
	if err := eng.Apply(plans, warnings); err != nil {
		return err
	}

	m.mu.Lock()
	m.lastApplyAt = time.Now()
	m.lastApplyErr = ""
	m.warnings = warnings
	m.mu.Unlock()
	return nil
}

func (m *Manager) setApplyError(err error) {
	m.mu.Lock()
	m.lastApplyErr = err.Error()
	m.mu.Unlock()
	m.log.Error("配置未能生效", "err", err)
}

func (m *Manager) stop() {
	m.mu.Lock()
	m.state = StateStopping
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
	m.mu.Lock()
	m.state = StateIdle
	m.mu.Unlock()
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

// HasDevice reports whether the active engine has the requested device.
func (m *Manager) HasDevice(channelIndex, deviceIndex int) bool {
	eng := m.current()
	return eng != nil && eng.HasDevice(channelIndex, deviceIndex)
}

// Connected reports whether the channel is connected (implements the NATS facade).
func (m *Manager) Connected(channelIndex int) bool {
	eng := m.current()
	return eng != nil && eng.Connected(channelIndex)
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

// Status returns the active engine status for the HTTP status endpoint,
// including runtime-level apply diagnostics.
func (m *Manager) Status() any {
	eng := m.current()
	status := RuntimeStatus{State: m.State()}
	m.mu.RLock()
	status.LastApplyErr = m.lastApplyErr
	if !m.lastApplyAt.IsZero() {
		status.LastApplyAt = m.lastApplyAt.Format(time.RFC3339)
	}
	status.Warnings = m.warnings
	status.ConfigPending = m.configDirty
	m.mu.RUnlock()

	if eng != nil {
		status.Engine = eng.Status()
	}
	return status
}

// RuntimeStatus 是 /api/engine/status 的顶层结构。
type RuntimeStatus struct {
	State         string              `json:"state"`
	LastApplyAt   string              `json:"lastApplyAt,omitempty"`
	LastApplyErr  string              `json:"lastApplyErr,omitempty"`
	ConfigPending bool                `json:"configPending"`
	Warnings      []string            `json:"warnings,omitempty"`
	Engine        engine.EngineStatus `json:"engine,omitempty"`
}

// State 返回运行时当前状态。
func (m *Manager) State() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state.String()
}

// Ready 返回运行时是否处于可服务状态（供 /readyz 使用）。
func (m *Manager) Ready() (bool, string) {
	m.mu.RLock()
	state := m.state
	eng := m.engine
	lastErr := m.lastApplyErr
	m.mu.RUnlock()

	if state != StateRunning {
		return false, fmt.Sprintf("运行时状态：%s", state.String())
	}
	if eng == nil {
		return false, "引擎未启动"
	}
	if !eng.Healthy() {
		msg := "没有已连接的链路"
		if lastErr != "" {
			msg += "；最近错误：" + lastErr
		}
		return false, msg
	}
	return true, ""
}

// ErrNotRunning 表示运行时当前不接受写操作。
var ErrNotRunning = errors.New("运行时未就绪")
