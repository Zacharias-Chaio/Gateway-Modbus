package api

import (
	"encoding/json"
	"net/http"
	"runtime"
	"time"

	"gateway/internal/buildinfo"
	"gateway/internal/config"
	"gateway/internal/engine"
	"gateway/internal/logx"

	"gorm.io/gorm"
)

// Server 持有数据库句柄，挂载所有 REST 接口。
type Server struct {
	DB *gorm.DB
	// Engine 负责链路的运行与热重载；链路配置变更后回调其 Apply。
	// 允许为 nil（例如未启用引擎的场景）。
	Engine EngineFacade
}

// EngineFacade 汇总引擎对 API 层暴露的全部能力，
// 由运行时管理器实现。接口化以避免 api 直接依赖引擎内部细节。
type EngineFacade interface {
	// Submit 投递一条写命令（按通道索引定位链路）。
	Submit(channelIndex int, cmd engine.WriteCommand) bool
	// HasDevice 判断设备是否存在且可采集。
	HasDevice(channelIndex, deviceIndex int) bool
	// Values 返回指定链路（按通道索引）的实时值快照。
	Values(channelIndex int) map[string]engine.SessionEntry
	// CommunicationSnapshot returns communication packets and per-session statistics.
	CommunicationSnapshot(channelIndex, deviceIndex int, afterSeq uint64, limit int) (engine.CommunicationSnapshot, bool)
}

// RuntimeFacade exposes the restartable runtime resources used by the HTTP API.
type RuntimeFacade interface {
	EngineFacade
	Restart() bool
	Status() any
	// ConfigChanged 通知配置（模型 / 链路）已保存，由运行时拉取最新配置并热重载引擎。
	ConfigChanged()
	// ConfigChangedErr 与 ConfigChanged 相同，但把应用失败的原因回传给调用方。
	ConfigChangedErr() error
	// State 返回运行时状态（idle/starting/running/stopping）。
	State() string
	// Ready 返回运行时是否可服务及不可服务的原因。
	Ready() (bool, string)
}

func New(db *gorm.DB) *Server {
	return &Server{DB: db}
}

func applyLogSettings(app config.App) {
	logx.Init(app.LogOptions())
}

// SystemInfo is read-only runtime metadata shown in the gateway settings UI.
type SystemInfo struct {
	OperatingSystem string `json:"operatingSystem"`
	SystemTime      string `json:"systemTime"`
	GatewayVersion  string `json:"gatewayVersion"`
}

// GetSystemInfo returns the current host operating system, time, and build version.
func (s *Server) GetSystemInfo(w http.ResponseWriter, r *http.Request) {
	ok(w, SystemInfo{
		OperatingSystem: runtime.GOOS,
		SystemTime:      time.Now().Format(time.RFC3339),
		GatewayVersion:  buildinfo.Version,
	})
}

// Restart requests an in-process runtime restart without terminating the gateway process.
func (s *Server) Restart(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.Engine.(RuntimeFacade)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "运行时重启不可用")
		return
	}
	if !rt.Restart() {
		fail(w, http.StatusConflict, "软件正在重启，请稍候")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"code": 0, "data": map[string]string{"status": "restarting"}})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func ok(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "data": data})
}
func fail(w http.ResponseWriter, c int, m string) {
	writeJSON(w, c, map[string]any{"code": c, "msg": m})
}

// okWithWarnings 返回成功但带告警（如「保存成功，但有设备被跳过」）。
// 以前 BuildPlans 的 warnings 只进日志，用户看到 200 就以为一切正常。
func okWithWarnings(w http.ResponseWriter, data any, warnings []string) {
	payload := map[string]any{"code": 0, "data": data}
	if len(warnings) > 0 {
		payload["warnings"] = warnings
	}
	writeJSON(w, http.StatusOK, payload)
}

// notifyConfigChanged 通知配置变更，并把「保存成功但未生效」如实回传。
func (s *Server) notifyConfigChanged() []string {
	rt, okRT := s.Engine.(RuntimeFacade)
	if !okRT {
		return nil
	}
	if err := rt.ConfigChangedErr(); err != nil {
		logx.Module("api").Warn("配置已保存但未能立即生效", "err", err)
		return []string{err.Error()}
	}
	return nil
}

// Health 是存活探针：进程能响应即为存活。
func (s *Server) Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().Format(time.RFC3339)})
}

// Ready 是就绪探针：引擎存在且至少一条链路已连接才算就绪。
// 端口能监听但采集全挂时不应报告健康——systemd 只看进程存活是看不出来的。
func (s *Server) Ready(w http.ResponseWriter, r *http.Request) {
	rt, okRT := s.Engine.(RuntimeFacade)
	if !okRT {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "reason": "运行时未启用"})
		return
	}
	if ready, reason := rt.Ready(); !ready {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "reason": reason})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}
