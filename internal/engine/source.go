package engine

import "context"

// ChannelSpec / ModelSpec 是采集引擎对配置数据的自有视图。
// 引擎不依赖存储层（GORM / SQLite）的具体类型；配置读取被收敛为单一边界：
// 进程内由 SQLite 实现，接入远程配置中心时仅需替换 PlanSource 实现，引擎与 worker 不变。
type ChannelSpec struct {
	ID      string // 通道ID，格式 Channel-{通道索引}，自动生成不可修改
	Index   int    // 通道索引，从 0 开始，引擎以索引定位链路 worker
	Name    string
	Type    string // Serial / Network
	Config  []byte // 链路配置 JSON（透传给 connector.ParseConfig）
	Devices []byte // 挂载设备列表 JSON：[{index, commNo, name, modelId}]
}

// ModelSpec 是一个设备模型的配置视图。
type ModelSpec struct {
	ID           string
	ProfileIndex int
	Name         string
	Profile      []byte // 档案 JSON（含 protocolType / maxRegisterCount）
	Properties   []byte // 属性数组 JSON
}

// PlanSource 提供构建采集计划所需的全部配置。
// 实现必须并发安全；进程内 SQLite 实现见 runtime.dbPlanSource。
type PlanSource interface {
	LoadChannels(ctx context.Context) ([]ChannelSpec, error)
	LoadModels(ctx context.Context) ([]ModelSpec, error)
}
