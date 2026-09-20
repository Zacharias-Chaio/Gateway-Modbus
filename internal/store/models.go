package store

import (
	"time"

	"gorm.io/datatypes"
)

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
	ID           string         `gorm:"primaryKey" json:"id"`     // 通道ID，格式 Channel-{通道索引}，自动生成
	ChannelIndex int            `gorm:"uniqueIndex" json:"channelIndex"` // 通道索引，从 0 开始，不可修改
	Name         string         `json:"name"`
	Type         string         `json:"type"` // Serial/Network
	Config       datatypes.JSON `json:"config"`
	Devices      datatypes.JSON `json:"devices"` // 挂载设备列表：[{index, commNo, modelId}]
	CreatedAt    time.Time      `json:"-"`
	UpdatedAt    time.Time      `json:"-"`
}
