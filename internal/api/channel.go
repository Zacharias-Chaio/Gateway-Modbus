package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"gateway/internal/engine/connector"
	"gateway/internal/store"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"
)

// ListChannels 返回全部链路（按通道索引升序）。
func (s *Server) ListChannels(w http.ResponseWriter, r *http.Request) {
	var list []store.Channel
	if err := s.DB.Order("channel_index asc").Find(&list).Error; err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ok(w, list)
}

// nextChannelIndex 返回最小未占用的通道索引（从 0 开始）。
// 删除链路后空出的索引会被新建链路复用，与前端 nextProfileIndex 规则一致。
func nextChannelIndex(db *gorm.DB) (int, error) {
	var indexes []int
	if err := db.Model(&store.Channel{}).Pluck("channel_index", &indexes).Error; err != nil {
		return 0, err
	}
	used := make(map[int]bool, len(indexes))
	for _, i := range indexes {
		used[i] = true
	}
	n := 0
	for used[n] {
		n++
	}
	return n, nil
}

// SaveChannel 创建或更新链路，并回传下发配置 JSON。
// 通道索引与通道ID 均由服务端生成且不可修改：
//   - id 为空视为新建：分配最小未占用索引，并按 Channel-{索引} 生成通道ID；
//   - id 非空视为更新：通道索引与通道ID 保留库中原值，忽略客户端传入的改动。
func (s *Server) SaveChannel(w http.ResponseWriter, r *http.Request) {
	var c store.Channel
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		fail(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
		return
	}
	// 链路类型收窄：采集网关只支持串口与网络（Modbus RTU / TCP）。
	if c.Type != connector.TypeSerial && c.Type != connector.TypeNetwork {
		fail(w, http.StatusBadRequest, "链路类型只支持 Serial / Network，当前为 "+c.Type)
		return
	}
	// 冲突检测：同一串口端口或网络 IP+端口不能被多个链路共用。
	if key := channelResourceKey(c.Type, c.Config); key != "" {
		var others []store.Channel
		if err := s.DB.Where("id <> ?", c.ID).Find(&others).Error; err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, o := range others {
			if channelResourceKey(o.Type, o.Config) == key {
				fail(w, http.StatusConflict, "链路配置冲突：串口端口或网络 IP+端口已被链路「"+o.Name+"」占用，不能被多个链路共用")
				return
			}
		}
	}
	if c.ID == "" {
		// 明确新建：通道索引与通道ID 由服务端分配，客户端不可指定
		index, err := nextChannelIndex(s.DB)
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		c.ChannelIndex = index
		c.ID = store.ChannelIDFromIndex(index)
		if err := s.DB.Create(&c).Error; err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.notifyConfigChanged()
		ok(w, c)
		return
	}
	// 更新：主键已存在才覆盖，避免伪造主键插入脏数据
	var exist store.Channel
	err := s.DB.First(&exist, "id = ?", c.ID).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			fail(w, http.StatusNotFound, "链路不存在: id="+c.ID)
			return
		}
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	c.ChannelIndex = exist.ChannelIndex // 通道索引不可修改，保留库中原值
	c.CreatedAt = exist.CreatedAt        // 保留原始创建时间，避免 Save 写入零值
	if err := s.DB.Save(&c).Error; err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notifyConfigChanged()
	ok(w, c)
}

// DeleteChannel 删除指定链路。
func (s *Server) DeleteChannel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		fail(w, http.StatusBadRequest, "无效的链路 ID")
		return
	}
	res := s.DB.Delete(&store.Channel{}, "id = ?", id)
	if res.Error != nil {
		fail(w, http.StatusInternalServerError, res.Error.Error())
		return
	}
	if res.RowsAffected == 0 {
		fail(w, http.StatusNotFound, "链路不存在: id="+id)
		return
	}
	s.notifyConfigChanged()
	ok(w, map[string]string{"id": id})
}

// notifyConfigChanged 通知运行时配置已变更：由 runtime 从 PlanSource
// 拉取全量链路 / 模型并触发引擎差量热重载。API 层不直接操作引擎。
func (s *Server) notifyConfigChanged() {
	if runtime, ok := s.Engine.(RuntimeFacade); ok {
		runtime.ConfigChanged()
	}
}

// channelResourceKey 提取链路占用的硬件资源唯一键：串口以端口名唯一，
// 网络以 IP+端口唯一。返回空串表示无可比较的资源占用，不参与冲突判断。
func channelResourceKey(typ string, config []byte) string {
	if len(config) == 0 {
		return ""
	}
	var cfg struct {
		SerialName string          `json:"serialName"`
		DeviceIP   string          `json:"deviceIp"`
		DevicePort json.RawMessage `json:"devicePort"`
	}
	if err := json.Unmarshal(config, &cfg); err != nil {
		return ""
	}
	switch typ {
	case "Serial":
		if s := strings.TrimSpace(cfg.SerialName); s != "" {
			return "Serial|" + strings.ToLower(s)
		}
	case "Network":
		ip := strings.TrimSpace(cfg.DeviceIP)
		port := strings.TrimSpace(string(cfg.DevicePort))
		if ip != "" && port != "" && port != "null" {
			return "Network|" + strings.ToLower(ip) + ":" + port
		}
	}
	return ""
}
