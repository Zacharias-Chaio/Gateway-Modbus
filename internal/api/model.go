package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"gateway/internal/engine/converter"
	"gateway/internal/store"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"
)

// validateModelProfile 落实业务收窄：协议必须是已注册协议，
// 接口类型必须与协议声明一致（留空视为未指定，向后兼容）。
// 非法值在保存阶段拦截，而不是等引擎构建采集计划时静默跳过。
func validateModelProfile(m *store.DeviceModel) error {
	var profile struct {
		InterfaceType string `json:"interfaceType"`
		ProtocolType  string `json:"protocolType"`
	}
	if len(m.Profile) > 0 {
		if err := json.Unmarshal(m.Profile, &profile); err != nil {
			return fmt.Errorf("模型 Profile 解析失败: %w", err)
		}
	}

	// 协议白名单来自注册表，而不是在 API 层硬编码字符串——
	// 以前这里和 converter.New、connector.NewDriver 各有一份 switch，
	// 三者可以互相矛盾（白名单允许但构造器不支持，或反之）。
	if profile.ProtocolType == "" {
		return fmt.Errorf("模型 Profile 缺少 protocolType")
	}
	if !converter.Exists(profile.ProtocolType) {
		names := make([]string, 0, 2)
		for _, n := range converter.Names() {
			names = append(names, string(n))
		}
		return fmt.Errorf("协议 %q 不支持（可用：%s）", profile.ProtocolType, strings.Join(names, " / "))
	}

	iface := profile.InterfaceType
	switch iface {
	case "", "Serial", "Network":
	default:
		return fmt.Errorf("接口类型只支持 Serial / Network，当前为 %q", iface)
	}
	if want := converter.InterfaceTypeOf(profile.ProtocolType); iface != "" && want != "" && iface != want {
		return fmt.Errorf("协议 %q 要求接口类型 %s，当前为 %q", profile.ProtocolType, want, iface)
	}
	return nil
}

// validateModelProperties 在保存阶段校验属性数组，而不是等到运行期。
//
// 与 engine.buildDevicePlan 共用 converter.ValidateProps，保证
// 「保存时通过」与「运行时可执行」不会出现分歧。
// 唯一性以属性 ID 为准；属性名允许重复（仅作展示）。
func validateModelProperties(properties []byte) error {
	if len(properties) == 0 {
		return nil
	}
	var props []converter.PropMeta
	if err := json.Unmarshal(properties, &props); err != nil {
		return fmt.Errorf("属性数组解析失败: %w", err)
	}
	return converter.ValidateProps(props)
}

// ListModels 返回全部设备模型。
func (s *Server) ListModels(w http.ResponseWriter, r *http.Request) {
	var list []store.DeviceModel
	if err := s.DB.Order("profile_index asc").Find(&list).Error; err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ok(w, list)
}

// SaveModel 创建或更新设备模型（按 ID upsert）。
func (s *Server) SaveModel(w http.ResponseWriter, r *http.Request) {
	var m store.DeviceModel
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		fail(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
		return
	}
	if m.ID == "" {
		fail(w, http.StatusBadRequest, "缺少设备模型 ID")
		return
	}
	if err := validateModelProfile(&m); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateModelProperties(m.Properties); err != nil {
		failValidation(w, err)
		return
	}
	// 若已存在则保留原始创建时间，避免 Save 全字段更新把 CreatedAt 写成零值。
	var exist store.DeviceModel
	if err := s.DB.First(&exist, "id = ?", m.ID).Error; err == nil {
		m.CreatedAt = exist.CreatedAt
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.DB.Save(&m).Error; err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	warnings := s.notifyConfigChanged()
	okWithWarnings(w, m, warnings)
}

// DeleteModel 删除指定设备模型。
//
// 被链路引用时返回 409 并列出引用者：以前可以直接删掉正在被使用的模型，
// 引擎热重载后那些设备会静默失效（且触发设备索引重排的连锁问题）。
// 确实要删时带 ?force=true 二次确认。
func (s *Server) DeleteModel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		fail(w, http.StatusBadRequest, "无效的设备模型 ID")
		return
	}

	if !isTruthy(r.URL.Query().Get("force")) {
		used, err := s.channelsUsingModel(id)
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		if len(used) > 0 {
			fail(w, http.StatusConflict, fmt.Sprintf(
				"设备模型正被 %d 条链路引用（%s），请先解除挂载或带 force=true 强制删除",
				len(used), strings.Join(used, "、")))
			return
		}
	}

	res := s.DB.Delete(&store.DeviceModel{}, "id = ?", id)
	if res.Error != nil {
		fail(w, http.StatusInternalServerError, res.Error.Error())
		return
	}
	if res.RowsAffected == 0 {
		fail(w, http.StatusNotFound, "设备模型不存在: id="+id)
		return
	}
	warnings := s.notifyConfigChanged()
	okWithWarnings(w, map[string]string{"id": id}, warnings)
}

// channelsUsingModel 返回引用了指定模型 ID 的链路名称。
func (s *Server) channelsUsingModel(modelID string) ([]string, error) {
	var channels []store.Channel
	if err := s.DB.Find(&channels).Error; err != nil {
		return nil, err
	}
	var used []string
	for _, ch := range channels {
		mounts, err := decodeMounts(ch.Devices)
		if err != nil {
			continue
		}
		for _, m := range mounts {
			if m.ModelID == modelID {
				used = append(used, ch.Name)
				break
			}
		}
	}
	return used, nil
}

func isTruthy(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "y":
		return true
	}
	return false
}
