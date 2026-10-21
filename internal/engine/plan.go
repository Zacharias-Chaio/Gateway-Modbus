package engine

import (
	"encoding/json"
	"fmt"
	"sort"

	"gateway/internal/engine/connector"
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
	Index   int    `json:"index"`   // 设备序号（权威标识，运行期不得重排）
	CommNo  int    `json:"commNo"`  // 从站地址 / 单元 ID
	Name    string `json:"name"`    // 设备名称（用户填写）
	ModelID string `json:"modelId"` // 对应 DeviceModel.ID
}

// DevicePlan 是一个设备的采集执行计划。
type DevicePlan struct {
	Index     int                  // 设备序号：**等于配置中的挂载序号**，不得重排
	UnitID    byte                 // 从站地址
	Name      string               // 设备名称（用户填写，日志/展示用）
	ModelID   string               // 设备模型 ID
	ModelName string               // 模型名称（日志用）
	Protocol  string               // 协议类型（"Modbus RTU" / "Modbus TCP"）
	Conv      converter.FrameIO    // 协议转换器
	Groups    []converter.RegGroup // 寄存器分组
	Props     []converter.PropMeta // 完整属性表（写操作查找用）

	// Valid=false 表示该挂载位因为配置问题无法采集（模型缺失/校验失败）。
	// 保留占位是刻意的：若把无效设备从切片里剔除，后续设备的 Index 会整体前移，
	// 北向 device_index 便会指向错误的物理设备。
	Valid  bool
	ErrMsg string
}

// DisplayName 返回用于日志/展示的设备名称：优先用户填写的设备名，否则退回模型名。
func (d *DevicePlan) DisplayName() string {
	if d.Name != "" {
		return d.Name
	}
	if d.ModelName != "" {
		return d.ModelName
	}
	return fmt.Sprintf("设备#%d", d.Index)
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
//
// 返回值：
//   - plans：可执行计划（设备切片中保留无效设备的占位）
//   - warnings：非致命问题（仅日志/接口回传）
//   - err：致命问题。调用方必须放弃本次调谐，保持现有链路继续运行，
//     绝不能用空计划去「收敛」——那会把一次配置读取抖动放大成全链路停机。
func BuildPlans(channels []ChannelSpec, models []ModelSpec) ([]ChannelPlan, []string, error) {
	modelMap := make(map[string]ModelSpec, len(models))
	for _, m := range models {
		modelMap[m.ID] = m
	}

	plans := make([]ChannelPlan, 0, len(channels))
	var warnings []string

	for _, ch := range channels {
		plan, warns, err := buildChannelPlan(ch, modelMap)
		warnings = append(warnings, warns...)
		if err != nil {
			return nil, warnings, fmt.Errorf("链路 %q: %w", ch.Name, err)
		}
		if plan == nil {
			continue
		}
		plans = append(plans, *plan)
	}
	return plans, warnings, nil
}

// buildChannelPlan 构建单条链路的采集计划。
func buildChannelPlan(ch ChannelSpec, modelMap map[string]ModelSpec) (*ChannelPlan, []string, error) {
	var warns []string

	var mounts []DeviceMount
	if len(ch.Devices) > 0 {
		if err := json.Unmarshal(ch.Devices, &mounts); err != nil {
			return nil, nil, fmt.Errorf("设备列表解析失败: %w", err)
		}
	}

	cfg, err := connector.ParseConfig(ch.Type, ch.Config)
	if err != nil {
		return nil, nil, fmt.Errorf("链路配置解析失败: %w", err)
	}

	plan := &ChannelPlan{
		ChannelIndex: ch.Index,
		ChannelName:  ch.Name,
		ChannelType:  ch.Type,
		Config:       ch.Config,
		PollMs:       cfg.PollInterval,
	}

	// 设备序号必须以配置里的 index 为准。历史上这里用 len(plan.Devices) 重排，
	// 只要中间有一个设备被跳过（模型被删、commNo 越界、属性校验失败），
	// 后面所有设备的序号都会前移，北向 device_index 随之指向错误的物理设备。
	seenIndex := make(map[int]bool, len(mounts))
	seenComm := make(map[int]bool, len(mounts))

	for _, mt := range mounts {
		if mt.Index < 0 {
			warns = append(warns, fmt.Sprintf("设备 %q: 序号 %d 非法，已忽略", mt.Name, mt.Index))
			continue
		}
		if seenIndex[mt.Index] {
			warns = append(warns, fmt.Sprintf("设备 %q: 序号 %d 重复，已忽略", mt.Name, mt.Index))
			continue
		}
		seenIndex[mt.Index] = true

		dp := &DevicePlan{Index: mt.Index, Name: mt.Name, ModelID: mt.ModelID, Valid: false}

		model, ok := modelMap[mt.ModelID]
		if !ok {
			dp.ErrMsg = fmt.Sprintf("设备模型 %q 不存在", mt.ModelID)
			warns = append(warns, fmt.Sprintf("链路 %q 设备 #%d %s", ch.Name, mt.Index, dp.ErrMsg))
			plan.Devices = append(plan.Devices, *dp)
			continue
		}
		dp.ModelName = model.Name

		if mt.CommNo < 1 || mt.CommNo > 247 {
			dp.ErrMsg = fmt.Sprintf("通讯号 %d 越界（合法范围 1~247）", mt.CommNo)
			warns = append(warns, fmt.Sprintf("链路 %q 设备 #%d: %s", ch.Name, mt.Index, dp.ErrMsg))
			plan.Devices = append(plan.Devices, *dp)
			continue
		}
		if seenComm[mt.CommNo] {
			dp.ErrMsg = fmt.Sprintf("通讯号 %d 在本链路内重复", mt.CommNo)
			warns = append(warns, fmt.Sprintf("链路 %q 设备 #%d: %s", ch.Name, mt.Index, dp.ErrMsg))
			plan.Devices = append(plan.Devices, *dp)
			continue
		}
		seenComm[mt.CommNo] = true

		built, err := buildDevicePlan(model, mt.CommNo)
		if err != nil {
			dp.ErrMsg = err.Error()
			warns = append(warns, fmt.Sprintf("链路 %q 设备 #%d (%s): %v", ch.Name, mt.Index, model.Name, err))
			plan.Devices = append(plan.Devices, *dp)
			continue
		}
		built.Index = mt.Index
		built.Name = mt.Name
		built.ModelID = mt.ModelID
		built.Valid = true
		plan.Devices = append(plan.Devices, *built)
	}

	// 按序号升序，保证缓存键、写命令定位与遍历顺序稳定。
	sort.SliceStable(plan.Devices, func(i, j int) bool {
		return plan.Devices[i].Index < plan.Devices[j].Index
	})

	if len(plan.Devices) == 0 {
		return nil, warns, nil
	}
	return plan, warns, nil
}

// buildDevicePlan 从设备模型构建单个设备的采集计划。
func buildDevicePlan(model ModelSpec, commNo int) (*DevicePlan, error) {
	if commNo < 1 || commNo > 247 {
		return nil, fmt.Errorf("模型 %q 的 Modbus 通讯号必须介于 1 和 247", model.Name)
	}

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

	conv, err := converter.New(proto)
	if err != nil {
		return nil, fmt.Errorf("模型 %q 协议 %q 不支持: %w", model.Name, proto, err)
	}

	var rawProps []converter.PropMeta
	if err := json.Unmarshal(model.Properties, &rawProps); err != nil {
		return nil, fmt.Errorf("模型 %q 属性列表解析失败: %w", model.Name, err)
	}

	if err := converter.ValidatePropsFor(rawProps, converter.SupportsBitsOf(proto)); err != nil {
		return nil, fmt.Errorf("模型 %q: %w", model.Name, err)
	}

	for i := range rawProps {
		rawProps[i].Index = i // 属性索引 = 模型属性列表中的序号，与物模型保持一致
	}

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
	DeviceIndex int     // 设备序号（等于配置中的挂载序号）
	PropName    string  // 属性名
	RawValue    float64 // 工程值（尚未逆变换，worker 侧做逆变换后编码）
	// ResultCh 用于 HTTP 入口拿到执行结果；nil 表示调用方不需要回执。
	// 发送必须非阻塞，channel 由调用方保证至少能接收 1 条。
	ResultCh chan<- WriteResult
}

// WriteResult 是写命令的执行结果。
type WriteResult struct {
	OK      bool   `json:"ok"`
	Err     string `json:"err,omitempty"`
	Applied int    `json:"applied"` // 实际发出的写帧数（FC06 拆帧时 >1）
}
