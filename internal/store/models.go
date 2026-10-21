package store

import (
	"time"

	"gorm.io/datatypes"
)

// SchemaVersion 是当前配置 blob 的 schema 版本。
//
// 以前 Channel.Config / DeviceModel.Profile / Properties 都是无版本号的裸 JSON，
// 改字段语义（例如设备序号规则）时，库里的存量配置会被静默按新规则解读。
// 现在每个 blob 都带 schemaVersion，加载时按版本走迁移链，未知高版本直接拒绝。
const SchemaVersion = 2

// DeviceModel 设备模型：档案 + 属性整体以 JSON 存储。
type DeviceModel struct {
	ID           string         `gorm:"primaryKey" json:"id"` // profileId，格式 Profile-{档案索引}，前端生成
	ProfileIndex int            `json:"profileIndex"`         // 档案索引，从 0 自增
	Name         string         `json:"name"`
	Profile      datatypes.JSON `json:"profile"`    // 档案/设备/协议信息
	Properties   datatypes.JSON `json:"properties"` // 属性数组
	CreatedAt    time.Time      `json:"-"`
	UpdatedAt    time.Time      `json:"-"`
}

// Channel 链路通道：通信参数 + 挂载设备列表。
// 通道索引从 0 开始、由服务端分配且不可修改；通道ID 按固定规则
// Channel-{通道索引} 自动生成，同样不可修改（见 db.go 的 ChannelIDFromIndex）。
type Channel struct {
	ID           string         `gorm:"primaryKey" json:"id"`            // 通道ID，格式 Channel-{通道索引}，自动生成
	ChannelIndex int            `gorm:"uniqueIndex" json:"channelIndex"` // 通道索引，从 0 开始，不可修改
	Name         string         `json:"name"`
	Type         string         `json:"type"` // Serial/Network
	Config       datatypes.JSON `json:"config"`
	Devices      datatypes.JSON `json:"devices"` // 挂载设备列表：[{index, commNo, modelId}]
	CreatedAt    time.Time      `json:"-"`
	UpdatedAt    time.Time      `json:"-"`
}

// DeviceMount 是挂载设备列表的存储视图。
//
// Index 是设备的稳定业务标识：北向 device_index、实时缓存键、写命令定位
// 全部以它为准。服务端生成后不得因为「中间某个设备配置失效」而重排。
type DeviceMount struct {
	Index   int    `json:"index"`
	CommNo  int    `json:"commNo"`
	Name    string `json:"name"`
	ModelID string `json:"modelId"`

	// SchemaVersion 标记该挂载项的配置版本，便于后续演进。
	SchemaVersion int `json:"schemaVersion,omitempty"`
}
