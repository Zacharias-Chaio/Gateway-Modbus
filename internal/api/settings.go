package api

import (
	"encoding/json"
	"net/http"

	"gateway/internal/config"
)

// GetSettings returns all persisted application and hardware settings.
func (s *Server) GetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := config.LoadSettings(s.DB)
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取网关设置失败: "+err.Error())
		return
	}
	ok(w, settings)
}

// SaveSettings persists all settings categories. Logging options apply immediately;
// gateway identity and NATS connection options take effect after a runtime restart.
func (s *Server) SaveSettings(w http.ResponseWriter, r *http.Request) {
	var settings config.Settings
	if err := json.NewDecoder(r.Body).Decode(&settings); err != nil {
		fail(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
		return
	}

	// 变更前先读出旧值，用于判断是否需要重建运行时。
	before, beforeErr := config.LoadSettings(s.DB)

	if err := config.SaveSettings(s.DB, settings); err != nil {
		fail(w, http.StatusBadRequest, "保存网关设置失败: "+err.Error())
		return
	}
	applyLogSettings(settings.App)

	// 网关身份 / NATS 连接参数只在运行时启动时生效。
	// 以前这些变更静默地「保存成功但不生效」，运维无从得知；
	// 现在明确回传 warnings，并在确实影响连接时提示需要重启运行时。
	var warnings []string
	if beforeErr == nil {
		if before.App.Gateway != settings.App.Gateway {
			warnings = append(warnings, "网关标识已变更，需点击「重启软件」后生效（NATS 主题与设备编码会随之改变）")
		}
		if before.App.NATS != settings.App.NATS {
			warnings = append(warnings, "NATS 连接参数已变更，需点击「重启软件」后生效")
		}
	}
	okWithWarnings(w, settings, warnings)
}
