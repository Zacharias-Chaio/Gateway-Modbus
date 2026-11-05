package api

import (
	"encoding/json"
	"net/http"

	"gateway/internal/config"
	"gateway/internal/logx"
)

// 本文件实现网关的首次配置接口，对应前端首次打开时的「网关名 + Token」表单。
//
// 这不是登录，也不是鉴权：网关没有用户、没有会话、没有角色。
// 它只负责把物模型模块分配的接入凭据写进数据库一次。

// provisioningPayload 是 POST /api/provisioning 的请求体。
type provisioningPayload struct {
	GWID  string `json:"gwId"`
	Token string `json:"token"`
}

// GetProvisioning 返回首次配置状态与当前网关名。
//
// 响应体里没有 Token 字段，且是有意为之：Token 是网关对平台的接入凭据，
// 一旦能经 HTTP 读出，任何能打开这个页面的人都拿得到它。
func (s *Server) GetProvisioning(w http.ResponseWriter, r *http.Request) {
	state, err := config.ProvisioningStatus(s.DB)
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取接入配置失败: "+err.Error())
		return
	}
	ok(w, state)
}

// SaveProvisioning 写入网关名与 Token，完成后网关才进入可用状态。
func (s *Server) SaveProvisioning(w http.ResponseWriter, r *http.Request) {
	var payload provisioningPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		fail(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
		return
	}

	if err := config.SaveProvisioning(s.DB, payload.GWID, payload.Token); err != nil {
		// 这里刻意不把 err 原样回显给前端：校验错误里可能带上 Token 片段。
		// 校验失败原因由 config 层给出，且已保证不含凭据内容。
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	// 记录的是网关名，不是 Token——日志会被落盘、扇出到平台，凭据不能进去。
	logx.Module("api").Info("网关接入配置完成", "gw_id", payload.GWID)

	state, err := config.ProvisioningStatus(s.DB)
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取接入配置失败: "+err.Error())
		return
	}

	warnings := s.provisioningWarnings()
	okWithWarnings(w, state, warnings)
}

// provisioningWarnings 提示需要重启运行时才能让新网关名生效。
//
// 网关名会拼进 NATS 主题，而 NATS 客户端在运行时启动时读取它；
// 只在启用 NATS 时提示，否则是在制造无谓的打扰。
func (s *Server) provisioningWarnings() []string {
	settings, err := config.LoadSettings(s.DB)
	if err != nil || !settings.App.NATS.Enabled {
		return nil
	}
	return []string{"网关名已变更，需点击「软件重启」后生效（NATS 主题与设备编码会随之改变）"}
}
