package runtime

import (
	"context"

	"gateway/internal/engine"
	"gateway/internal/store"

	"gorm.io/gorm"
)

// dbPlanSource 是 engine.PlanSource 的进程内实现：
// 从 SQLite 读取链路与设备模型，并转换为引擎自有 DTO。
// 如需接入远程配置中心，用远程实现（RPC / 消息总线）替换本类型即可，
// 引擎与 API 均不感知变化。
type dbPlanSource struct{ db *gorm.DB }

// LoadChannels 按通道索引升序返回全部链路。
func (s *dbPlanSource) LoadChannels(ctx context.Context) ([]engine.ChannelSpec, error) {
	var rows []store.Channel
	if err := s.db.WithContext(ctx).Order("channel_index asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]engine.ChannelSpec, 0, len(rows))
	for _, r := range rows {
		out = append(out, engine.ChannelSpec{
			ID: r.ID, Index: r.ChannelIndex, Name: r.Name, Type: r.Type,
			Config: []byte(r.Config), Devices: []byte(r.Devices),
		})
	}
	return out, nil
}

// LoadModels 按档案索引升序返回全部设备模型。
func (s *dbPlanSource) LoadModels(ctx context.Context) ([]engine.ModelSpec, error) {
	var rows []store.DeviceModel
	if err := s.db.WithContext(ctx).Order("profile_index asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]engine.ModelSpec, 0, len(rows))
	for _, r := range rows {
		out = append(out, engine.ModelSpec{
			ID: r.ID, ProfileIndex: r.ProfileIndex, Name: r.Name,
			Profile: []byte(r.Profile), Properties: []byte(r.Properties),
		})
	}
	return out, nil
}
