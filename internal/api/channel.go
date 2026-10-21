package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"gateway/internal/engine/connector"
	"gateway/internal/engine/converter"
	"gateway/internal/engine/converter/modbus"
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

	// 配置字段级校验：这是全仓唯一的通道配置契约。
	cfg, err := connector.ParseConfig(c.Type, c.Config)
	if err != nil {
		fail(w, http.StatusBadRequest, "链路配置无效: "+err.Error())
		return
	}

	// 挂载设备结构校验：模型必须存在、通讯号必须合法且在本链路内唯一、
	// 设备序号必须唯一。以前这些全部推迟到运行期，且以 warn 形式跳过，
	// 用户在界面上看到的是「保存成功、但设备一直没数据」。
	mounts, err := decodeMounts(c.Devices)
	if err != nil {
		fail(w, http.StatusBadRequest, "设备列表无效: "+err.Error())
		return
	}
	if err := s.validateMounts(mounts); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	// 冲突检测：同一串口端口或网络 IP+端口不能被多个链路共用。
	if key := cfg.ResourceKey(); key != "" {
		var others []store.Channel
		if err := s.DB.Where("id <> ?", c.ID).Find(&others).Error; err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, o := range others {
			otherCfg, err := connector.ParseConfig(o.Type, o.Config)
			if err != nil {
				continue
			}
			if otherCfg.ResourceKey() == key {
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
		warnings := s.notifyConfigChanged()
		okWithWarnings(w, c, warnings)
		return
	}
	// 更新：主键已存在才覆盖，避免伪造主键插入脏数据
	var exist store.Channel
	err = s.DB.First(&exist, "id = ?", c.ID).Error
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
	warnings := s.notifyConfigChanged()
	okWithWarnings(w, c, warnings)
}

// decodeMounts 解析挂载设备列表。
func decodeMounts(raw []byte) ([]store.DeviceMount, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var mounts []store.DeviceMount
	if err := json.Unmarshal(raw, &mounts); err != nil {
		return nil, err
	}
	return mounts, nil
}

// validateMounts 校验挂载设备的结构完整性。
func (s *Server) validateMounts(mounts []store.DeviceMount) error {
	if len(mounts) == 0 {
		return nil
	}
	seenIndex := make(map[int]bool, len(mounts))
	seenComm := make(map[int]bool, len(mounts))

	var modelIDs []string
	for _, m := range mounts {
		modelIDs = append(modelIDs, m.ModelID)
	}
	existing := make(map[string]bool, len(modelIDs))
	if len(modelIDs) > 0 {
		var rows []store.DeviceModel
		if err := s.DB.Select("id").Where("id IN ?", modelIDs).Find(&rows).Error; err != nil {
			return fmt.Errorf("校验设备模型失败: %w", err)
		}
		for _, row := range rows {
			existing[row.ID] = true
		}
	}

	for i, m := range mounts {
		label := fmt.Sprintf("设备 #%d", m.Index)
		if m.Name != "" {
			label = fmt.Sprintf("设备 #%d（%s）", m.Index, m.Name)
		}
		if m.Index < 0 {
			return fmt.Errorf("%s: 设备序号不能为负", label)
		}
		if seenIndex[m.Index] {
			return fmt.Errorf("%s: 设备序号重复", label)
		}
		seenIndex[m.Index] = true

		if m.CommNo < 1 || m.CommNo > 247 {
			return fmt.Errorf("%s: 通讯号 %d 越界（合法范围 1~247）", label, m.CommNo)
		}
		if seenComm[m.CommNo] {
			return fmt.Errorf("%s: 通讯号 %d 在本链路内重复，多个设备使用同一从站地址会互相串话", label, m.CommNo)
		}
		seenComm[m.CommNo] = true

		if strings.TrimSpace(m.ModelID) == "" {
			return fmt.Errorf("%s: 必须选择设备模型", label)
		}
		if !existing[m.ModelID] {
			return fmt.Errorf("%s: 设备模型 %q 不存在", label, m.ModelID)
		}
		_ = i
	}
	return nil
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
	warnings := s.notifyConfigChanged()
	okWithWarnings(w, map[string]string{"id": id}, warnings)
}

// channelResourceKey 兼容旧调用：委托给 connector 的统一配置契约。
func channelResourceKey(typ string, config []byte) string {
	cfg, err := connector.ParseConfig(typ, config)
	if err != nil {
		return ""
	}
	return cfg.ResourceKey()
}

// ProtocolCapabilities 返回已注册协议的元信息，供前端动态渲染下拉框，
// 避免「后端支持的协议」与「前端写死的选项」长期脱节。
func (s *Server) ProtocolCapabilities(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Name          string `json:"name"`
		InterfaceType string `json:"interfaceType"`
		SupportsBits  bool   `json:"supportsBits"`
		MaxReadRegs   int    `json:"maxReadRegisters"`
		MaxReadBits   int    `json:"maxReadBits"`
	}
	names := converter.Names()
	out := make([]item, 0, len(names))
	for _, name := range names {
		spec, okSpec := converter.Lookup(name)
		if !okSpec {
			continue
		}
		out = append(out, item{
			Name: string(name), InterfaceType: spec.InterfaceType, SupportsBits: spec.SupportsBits,
			MaxReadRegs: modbus.MaxReadRegisters, MaxReadBits: modbus.MaxReadBits,
		})
	}
	ok(w, out)
}
