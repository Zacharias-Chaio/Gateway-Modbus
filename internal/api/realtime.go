package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gateway/internal/engine"
)

// Realtime 返回设备属性的实时采集值。
// 查询参数 device 格式为 "channelIndex/deviceIndex"（如 "0/1"，通道索引从 0 开始），
// 返回该链路设备下的所有缓存属性。
//
// 可选参数 channelIndex：只返回该通道的值（减少大链路下的整表复制）。
// 响应中始终带 engineState 字段，让前端能区分「没有数据」与「链路正在重启」。
func (s *Server) Realtime(w http.ResponseWriter, r *http.Request) {
	device := r.URL.Query().Get("device")
	state := s.runtimeState()

	if device == "" {
		ok(w, map[string]any{
			"device": "", "timestamp": time.Now().Unix(), "values": map[string]any{},
			"engineState": state,
		})
		return
	}

	channelIndex, devIdx := parseDeviceKey(device)
	if channelIndex < 0 {
		fail(w, http.StatusBadRequest, "device 参数格式应为 channelIndex/deviceIndex")
		return
	}

	if s.Engine == nil {
		ok(w, map[string]any{
			"device": device, "timestamp": time.Now().Unix(), "values": map[string]any{},
			"engineState": state, "note": "引擎未启动",
		})
		return
	}

	all := s.Engine.Values(channelIndex)
	if all == nil {
		// 链路不存在 / 未运行：明确告知，而不是返回一个空 map 让前端以为「还没有数据」。
		ok(w, map[string]any{
			"device": device, "timestamp": time.Now().Unix(), "values": map[string]any{},
			"engineState": state, "note": "链路未运行或不存在",
		})
		return
	}

	// 缓存键的形如 "deviceIndex/propID"（属性 ID 才是唯一标识，属性名允许重复）。
	// 返回值同样以属性 ID 为键，前端按 id 取值。
	values := make(map[string]any)
	prefix := fmt.Sprintf("%d/", devIdx)
	for k, v := range all {
		if strings.HasPrefix(k, prefix) {
			propKey := k[len(prefix):]
			values[propKey] = map[string]any{
				"value": v.Value,
				"ts":    v.Timestamp.UnixMilli(),
			}
		}
	}
	ok(w, map[string]any{
		"device":      device,
		"timestamp":   time.Now().Unix(),
		"values":      values,
		"engineState": state,
	})
}

// runtimeState 返回运行时的可读状态（供前端渲染「重启中」等提示）。
func (s *Server) runtimeState() string {
	if rt, okRT := s.Engine.(RuntimeFacade); okRT {
		return rt.State()
	}
	return "unknown"
}

// SetValue 接收对可写属性的设定值，通过引擎下发写命令。
//
// 请求体 JSON: { "channelIndex": 0, "deviceIndex": 0, "propId": "Property-46", "value": 50.0, "waitMs": 3000 }
//
// propId 是推荐字段（属性唯一标识）；propName 仅为兼容旧调用方保留——
// 属性名允许重复，按名字定位可能命中错误的属性。
//
// 返回 202 + requestId；若客户端传了 waitMs（上限 10s），则同步等待执行结果后
// 返回 200 + 结果。以前 HTTP 入口完全没有回执，界面点「下发」只能靠
// 「值有没有变化」反推，而写读可能不同源、刷新还有延迟。
func (s *Server) SetValue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ChannelIndex *int    `json:"channelIndex"`
		DeviceIndex  int     `json:"deviceIndex"`
		PropID       string  `json:"propId"`
		PropName     string  `json:"propName"`
		Value        float64 `json:"value"`
		WaitMs       int     `json:"waitMs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
		return
	}
	if req.ChannelIndex == nil || *req.ChannelIndex < 0 {
		fail(w, http.StatusBadRequest, "缺少 channelIndex")
		return
	}
	// 优先使用属性 ID 定位；仅在未提供时退回属性名。
	propKey := strings.TrimSpace(req.PropID)
	if propKey == "" {
		propKey = strings.TrimSpace(req.PropName)
	}
	if propKey == "" {
		fail(w, http.StatusBadRequest, "缺少 propId（属性 ID）")
		return
	}
	if s.Engine == nil {
		fail(w, http.StatusServiceUnavailable, "引擎未启动，无法下发写命令")
		return
	}

	// 设备存在性校验：以前越界索引会被接受，错误无人接收。
	if !s.Engine.HasDevice(*req.ChannelIndex, req.DeviceIndex) {
		fail(w, http.StatusBadRequest, fmt.Sprintf("通道 %d 设备 %d 不存在或不可采集", *req.ChannelIndex, req.DeviceIndex))
		return
	}

	requestID := fmt.Sprintf("http-%d", time.Now().UnixNano())
	waitMs := req.WaitMs
	if waitMs < 0 {
		waitMs = 0
	}
	if waitMs > 10000 {
		waitMs = 10000
	}

	cmd := engine.WriteCommand{
		RequestID:   requestID,
		DeviceIndex: req.DeviceIndex,
		PropName:    propKey,
		RawValue:    req.Value,
	}

	var resultCh chan engine.WriteResult
	if waitMs > 0 {
		resultCh = make(chan engine.WriteResult, 1)
		cmd.ResultCh = resultCh
	}

	if !s.Engine.Submit(*req.ChannelIndex, cmd) {
		fail(w, http.StatusServiceUnavailable, "写命令投递失败：链路不存在或队列已满")
		return
	}

	if resultCh == nil {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"code": 0,
			"data": map[string]any{"status": "accepted", "requestId": requestID},
		})
		return
	}

	select {
	case res := <-resultCh:
		if !res.OK {
			fail(w, http.StatusBadGateway, "写命令执行失败: "+res.Err)
			return
		}
		ok(w, map[string]any{"status": "ok", "requestId": requestID, "applied": res.Applied})
	case <-time.After(time.Duration(waitMs) * time.Millisecond):
		// 超时不代表失败：命令仍在链路队列里执行，回传 requestId 便于继续追踪。
		writeJSON(w, http.StatusAccepted, map[string]any{
			"code": 0,
			"data": map[string]any{"status": "pending", "requestId": requestID,
				"note": "等待超时，命令仍在链路队列中执行，可通过通讯监控观察结果"},
		})
	case <-r.Context().Done():
		return
	}
}

// parseDeviceKey 解析 "channelIndex/deviceIndex" 格式。
// 通道索引与设备序号均从 0 开始；解析失败或为负时返回 -1。
func parseDeviceKey(s string) (channelIndex, devIdx int) {
	channel, device, hasDevice := strings.Cut(s, "/")
	idx, err := strconv.Atoi(channel)
	if err != nil || idx < 0 {
		return -1, 0
	}
	if !hasDevice {
		return idx, 0
	}
	d, err := strconv.Atoi(device)
	if err != nil || d < 0 {
		return -1, 0
	}
	return idx, d
}
