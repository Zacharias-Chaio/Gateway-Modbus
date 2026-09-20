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
func (s *Server) Realtime(w http.ResponseWriter, r *http.Request) {
	device := r.URL.Query().Get("device")
	if device == "" {
		ok(w, map[string]any{"device": "", "timestamp": time.Now().Unix(), "values": map[string]any{}})
		return
	}

	// 尝试从引擎获取实时缓存值
	if s.Engine != nil {
		channelIndex, devIdx := parseDeviceKey(device)
		if channelIndex >= 0 {
			all := s.Engine.Values(channelIndex)
			values := make(map[string]any)
			prefix := fmt.Sprintf("%d/", devIdx)
			for k, v := range all {
				if len(k) > len(prefix) && k[:len(prefix)] == prefix {
					propName := k[len(prefix):]
					values[propName] = map[string]any{
						"value": v.Value,
						"ts":    v.Timestamp.UnixMilli(),
					}
				}
			}
			ok(w, map[string]any{
				"device":    device,
				"timestamp": time.Now().Unix(),
				"values":    values,
			})
			return
		}
	}

	// 引擎未启用或无缓存数据，返回空
	ok(w, map[string]any{
		"device":    device,
		"timestamp": time.Now().Unix(),
		"values":    map[string]any{},
		"note":      "无采集数据（引擎未启动或设备未连接）",
	})
}

// SetValue 接收对可写属性的设定值，通过引擎下发写命令。
// 请求体 JSON: { "channelIndex": 0, "deviceIndex": 0, "propName": "频率", "value": 50.0 }
// 通道索引从 0 开始，用指针区分「未提供」与合法的 0。
func (s *Server) SetValue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ChannelIndex *int    `json:"channelIndex"`
		DeviceIndex  int     `json:"deviceIndex"`
		PropName     string  `json:"propName"`
		Value        float64 `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
		return
	}
	if req.ChannelIndex == nil || *req.ChannelIndex < 0 || req.PropName == "" {
		fail(w, http.StatusBadRequest, "缺少 channelIndex 或 propName")
		return
	}

	if s.Engine == nil {
		fail(w, http.StatusServiceUnavailable, "引擎未启动，无法下发写命令")
		return
	}

	cmd := engine.WriteCommand{
		DeviceIndex: req.DeviceIndex,
		PropName:    req.PropName,
		RawValue:    req.Value,
	}
	if !s.Engine.Submit(*req.ChannelIndex, cmd) {
		fail(w, http.StatusServiceUnavailable, "写命令投递失败：链路不存在或队列已满")
		return
	}
	ok(w, map[string]string{"status": "accepted"})
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
