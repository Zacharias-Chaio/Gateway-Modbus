package api

import (
	"fmt"
	"net/http"
	"strconv"
)

// CommunicationMonitor returns current-session packets and statistics for one
// device, or for every device on a link when deviceIndex is omitted.
func (s *Server) CommunicationMonitor(w http.ResponseWriter, r *http.Request) {
	channelIndex, err := nonNegativeQueryInt(r, "channelIndex")
	if err != nil {
		fail(w, http.StatusBadRequest, "channelIndex 必须是不小于 0 的整数")
		return
	}

	deviceIndex := -1
	if raw := r.URL.Query().Get("deviceIndex"); raw != "" {
		deviceIndex, err = strconv.Atoi(raw)
		if err != nil || deviceIndex < 0 {
			fail(w, http.StatusBadRequest, "deviceIndex 必须是不小于 0 的整数")
			return
		}
	}

	afterSeq := uint64(0)
	if raw := r.URL.Query().Get("afterSeq"); raw != "" {
		afterSeq, err = strconv.ParseUint(raw, 10, 64)
		if err != nil {
			fail(w, http.StatusBadRequest, "afterSeq 必须是无符号整数")
			return
		}
	}

	limit := 200
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 1000 {
			fail(w, http.StatusBadRequest, "limit 必须介于 1 和 1000")
			return
		}
	}

	if s.Engine == nil {
		fail(w, http.StatusServiceUnavailable, "引擎未启动，无法获取通讯监控数据")
		return
	}
	snapshot, found := s.Engine.CommunicationSnapshot(channelIndex, deviceIndex, afterSeq, limit)
	if !found {
		fail(w, http.StatusNotFound, "链路未运行或不存在")
		return
	}
	ok(w, snapshot)
}

// nonNegativeQueryInt 解析不小于 0 的整数查询参数（通道索引从 0 开始，0 合法）。
func nonNegativeQueryInt(r *http.Request, key string) (int, error) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return 0, fmt.Errorf("缺少 %s", key)
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("%s 必须是不小于 0 的整数", key)
	}
	return v, nil
}
