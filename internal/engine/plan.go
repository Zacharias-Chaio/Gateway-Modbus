package engine

import (
	"encoding/json"
	"fmt"

	"gateway/internal/engine/converter"
)

// 本文件定义采集计划的领域模型：将 PlanSource 提供的扁平配置（ChannelSpec + ModelSpec）
// 转化为 engine 可直接执行的分层结构（ChannelPlan → DevicePlan → RegGroup）。
//
// 数据流向:
//
//	ChannelSpec ──┐
//	               ├─ BuildPlans ─► ChannelPlan
//	ModelSpec ───┘                   │
//	                                   ├─ Devices[]: DevicePlan
//	                                   │     ├─ Index（链路内序号：缓存键 / 写命令 / 遥测定位）
//	                                   │     ├─ UnitID (从站地址)
//	                                   │     ├─ Conv (协议转换器)
//	                                   │     └─ Groups[]: RegGroup (寄存器分组)
//	                                   │           └─ Members[]: GroupMember (属性定位)
//	                                   └─ PollMs (轮询间隔)

// DeviceMount 描述挂载在链路上的单个设备（从 ChannelSpec.Devices JSON 解析）。
type DeviceMount struct {
	Index   int    `json:"index"`   // 设备序号
	CommNo  int    `json:"commNo"`  // 从站地址 / 单元 ID
	Name    string `json:"name"`    // 设备名称（用户填写）
	ModelID string `json:"modelId"` // 对应 DeviceModel.ID
}

// DevicePlan 是一个设备的采集执行计划。
type DevicePlan struct {
	Index     int                  // 设备在链路内的序号（缓存键、写命令与遥测定位使用）
	UnitID    byte                 // 从站地址
	Name      string               // 设备名称（用户填写，日志/展示用）
	ModelID   string               // 设备模型 ID
	ModelName string               // 模型名称（日志用）
	Protocol  string               // 协议类型（"Modbus RTU" / "Modbus TCP"）
	Conv      converter.FrameIO    // 协议转换器
	Groups    []converter.RegGroup // 寄存器分组
	Props     []converter.PropMeta // 完整属性表（写操作查找用）
}

// DisplayName 返回用于日志/展示的设备名称：优先用户填写的设备名，否则退回模型名。
func (d *DevicePlan) DisplayName() string {
	if d.Name != "" {
		return d.Name
	}
	return d.ModelName
}

// ChannelPlan 是一条链路的完整采集计划。
type ChannelPlan struct {
	ChannelIndex int    // 通道索引（从 0 开始），worker 的定位标识
	ChannelName  string // 链路名称
	ChannelType  string // 链路类型（Serial / Network）
	Config       []byte // 链路配置 JSON（透传给 connector.ParseConfig）
	Devices      []DevicePlan
	PollMs       int // 轮询间隔（毫秒），0 表示默认
}

// BuildPlans 将配置源提供的链路 + 设备模型转化为引擎可执行的采集计划。
// 无法解析的设备/模型会被跳过并记录原因（返回的 warnings 仅用于日志）。
func BuildPlans(channels []ChannelSpec, models []ModelSpec) ([]ChannelPlan, []string) {
	modelMap := make(map[string]ModelSpec, len(models))
	for _, m := range models {
		modelMap[m.ID] = m
	}

	plans := make([]ChannelPlan, 0, len(channels))
	var warnings []string

	for _, ch := range channels {
		plan, warns := buildChannelPlan(ch, modelMap)
		warnings = append(warnings, warns...)
		if plan == nil {
			continue
		}
		plans = append(plans, *plan)
	}
	return plans, warnings
}

// buildChannelPlan 构建单条链路的采集计划。
func buildChannelPlan(ch ChannelSpec, modelMap map[string]ModelSpec) (*ChannelPlan, []string) {
	var warns []string

	// 解析链路挂载的设备列表。
	var mounts []DeviceMount
	if len(ch.Devices) > 0 {
		if err := json.Unmarshal(ch.Devices, &mounts); err != nil {
			return nil, []string{fmt.Sprintf("链路 %q 设备列表解析失败: %v", ch.Name, err)}
		}
	}

	plan := &ChannelPlan{
		ChannelIndex: ch.Index,
		ChannelName:  ch.Name,
		ChannelType:  ch.Type,
		Config:       ch.Config,
	}

	// 从 Config JSON 中提取 pollInterval。
	var cfgRaw struct {
		PollInterval int `json:"pollInterval"`
	}
	_ = json.Unmarshal(ch.Config, &cfgRaw)
	plan.PollMs = cfgRaw.PollInterval

	for _, mt := range mounts {
		model, ok := modelMap[mt.ModelID]
		if !ok {
			warns = append(warns, fmt.Sprintf("链路 %q 设备 %d: 模型 %q 不存在，跳过", ch.Name, mt.Index, mt.ModelID))
			continue
		}

		dp, err := buildDevicePlan(model, mt.CommNo)
		if err != nil {
			warns = append(warns, fmt.Sprintf("链路 %q 设备 %d (%s): %v", ch.Name, mt.Index, model.Name, err))
			continue
		}
		dp.Name = mt.Name
		dp.ModelID = mt.ModelID
		dp.Index = len(plan.Devices) // 链路内序号，作为缓存键与 API 定位标识
		plan.Devices = append(plan.Devices, *dp)
	}

	if len(plan.Devices) == 0 {
		return nil, warns
	}
	return plan, warns
}

// buildDevicePlan 从设备模型构建单个设备的采集计划。
func buildDevicePlan(model ModelSpec, commNo int) (*DevicePlan, error) {
	if commNo < 1 || commNo > 247 {
		return nil, fmt.Errorf("模型 %q 的 Modbus 通讯号必须介于 1 和 247", model.Name)
	}

	// 解析协议信息与最大寄存器数。
	var profile struct {
		ProtocolType     string `json:"protocolType"`
		MaxRegisterCount *int   `json:"maxRegisterCount"`
	}
	if err := json.Unmarshal(model.Profile, &profile); err != nil {
		return nil, fmt.Errorf("模型 %q Profile 解析失败: %w", model.Name, err)
	}
	proto := profile.ProtocolType
	if proto == "" {
		return nil, fmt.Errorf("模型 %q 未定义协议类型", model.Name)
	}
	maxRegs := 0
	if profile.MaxRegisterCount != nil && *profile.MaxRegisterCount > 0 {
		maxRegs = *profile.MaxRegisterCount
	}

	// 创建协议转换器。
	conv, err := converter.New(proto)
	if err != nil {
		return nil, fmt.Errorf("模型 %q 协议 %q 不支持: %w", model.Name, proto, err)
	}

	// 解析属性列表为 PropMeta。
	var rawProps []converter.PropMeta
	if err := json.Unmarshal(model.Properties, &rawProps); err != nil {
		return nil, fmt.Errorf("模型 %q 属性列表解析失败: %w", model.Name, err)
	}

	// 补全默认值并校验协议映射：
	//   - coefficient 为 0 时按 1 处理（避免工程值变换与写值逆变换失真）；
	//   - 寄存器数量为必填（1~125），参与协议映射（配置了读或写功能码）的属性
	//     缺失时直接报错；虚拟属性（如"在线状态"，功能码均为 0）豁免。
	for i := range rawProps {
		p := &rawProps[i]
		p.Index = i // 属性索引 = 模型属性列表中的序号，与物模型保持一致
		if p.Coefficient == 0 {
			p.Coefficient = 1
		}
		if p.ReadFC > 0 || p.WriteFC > 0 {
			if p.RegisterCount < 1 || p.RegisterCount > 125 {
				return nil, fmt.Errorf("模型 %q 属性 %q: 寄存器数量必填（1~125）", model.Name, p.Name)
			}
		}
	}

	// 构建寄存器分组（受 maxRegisterCount 约束自动拆分）。
	groups := converter.BuildGroups(rawProps, maxRegs)

	return &DevicePlan{
		UnitID:    byte(commNo),
		ModelID:   model.ID,
		ModelName: model.Name,
		Protocol:  proto,
		Conv:      conv,
		Groups:    groups,
		Props:     rawProps,
	}, nil
}

// ─── 写命令 ──────────────────────────────────────────────────

// WriteCommand 是一条设备写指令。
type WriteCommand struct {
	RequestID   string  // 外部请求 ID；为空表示非消息总线命令
	DeviceIndex int     // 设备在 ChannelPlan.Devices 中的序号
	PropName    string  // 属性名
	RawValue    float64 // 工程值（尚未逆变换，worker 侧做逆变换后编码）
}

// WriteResult 是写命令的执行结果。
type WriteResult struct {
	OK  bool
	Err string
}
