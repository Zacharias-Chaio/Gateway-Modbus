package converter

import (
	"fmt"
	"sort"
)

// DefaultMaxRegs 是 Modbus 协议单次读请求的寄存器数上限。
const DefaultMaxRegs = 125

// 本文件实现「寄存器分组」策略。
//
// 地址模型：实际寄存器地址 = registerBase + registerOffset
//
//   - registerBase：寄存器基址，是分组的核心依据。相同 (readFC, registerBase)
//     的属性归入同一读请求，一次 EncodeRead 从 base 开始连续读取。
//   - registerOffset：相对于基址的偏移量（寄存器个数，非字节），用于从响应帧中
//     定位该属性的起始位置：字节偏移 = registerOffset × 2。
//   - startBit / endBit：在该属性寄存器区间的位级定位（bit 0 = 最低位），
//     约束 endBit 落在 registerCount × 16 位宽度内。例如 startBit=0,endBit=0 →
//     提取最低 1 位；startBit=0,endBit=31 → 全部 32 位。
//   - registerCount：寄存器数量（读取跨度），必填 1~125，由 buildDevicePlan 校验。
//     设计约定：寄存器数量与位区间（数据长度）无强制对应——位区间只定义
//     解析时取数据中的多少位换算工程值（如 1 寄存器 16 位可只取前 8 位），
//     读取跨度由 registerCount 独立指定。
//   - string 类型不走位提取，忽略 startBit/endBit，按字节级处理。
//
// 分组规则：
//  1. 按 (readFC, registerBase) 分桶 —— 同一基址 + 同一功能码的属性合并为一次读请求。
//  2. 组内 Quantity = max(registerOffset + regCount)，覆盖所有成员的最远地址。
//  3. 组内 ByteOffset = registerOffset × 2，用于从响应数据中切片各属性。

// PropMeta 描述单个属性的协议映射元数据（来自设备模型 JSON）。
type PropMeta struct {
	Name         string  `json:"name"`
	PropID       string  `json:"id"`
	DataType     string  `json:"dataType"`
	StartBit     int     `json:"startBit"`       // 起始位（bit 0 = 最低位）
	EndBit       int     `json:"endBit"`         // 终止位（含），须落在 registerCount × 16 位宽度内
	Offset       int     `json:"registerOffset"` // 相对基址的寄存器偏移，实际地址 = registerBase + registerOffset
	RegisterBase int     `json:"registerBase"`   // 寄存器基址（分组的依据）
	ReadFC       int     `json:"readFunctionCode"`
	WriteFC      int     `json:"writeFunctionCode"`
	Coefficient  float64 `json:"coefficient"`
	DeltaValue   float64 `json:"deltaValue"` // 偏移量（可正负），工程值 = 原始值 × coefficient + deltaValue
	ByteOrder    string  `json:"byteOrder"`
	AccessMode   string  `json:"accessMode"`  // r / w / rw
	Unit         string  `json:"unit"`        // 工程量单位（遥测上报用）
	Description  string  `json:"description"` // 属性值描述（遥测上报用）

	// RegisterCount 寄存器数量（读取跨度），必填 1~125；由 buildDevicePlan 统一校验。
	RegisterCount int `json:"registerCount"`
}

// RegGroup 是一次读请求对应的寄存器组。
type RegGroup struct {
	ReadFC    int // Modbus 功能码
	StartAddr int // 实际请求的起始地址（= registerBase）
	Quantity  int // 请求的寄存器数量
	Members   []GroupMember
}

// GroupMember 是组内单个属性的定位信息。
type GroupMember struct {
	Prop       PropMeta
	ByteOffset int // 在响应数据中的字节偏移（= offset * 2）
	ByteLen    int // 该属性的字节长度（= regCount * 2）
}

// BuildGroups 将设备的属性列表按 (readFC, registerBase) 分组。
// 只处理可读属性（accessMode 含 r）。
// maxRegs 限制单个读请求的最大寄存器数（Modbus 协议上限 125）；
// 同一分组的覆盖范围超过该值时自动拆分为多个连续子请求，每个子请求的
// Quantity <= maxRegs。maxRegs <= 0 时取默认上限 125。
//
// 寄存器区间横跨段边界的属性无法并入任何连续段：以自身起点单独成组，
// 保证仍然被采集（此前会被整组静默丢弃）。单个属性宽度本身超过 maxRegs 时，
// 独立请求会超出协议上限、被设备以异常码拒绝——这是通讯监控中可见的错误，
// 优于静默不采集。
func BuildGroups(props []PropMeta, maxRegs int) []RegGroup {
	if maxRegs <= 0 {
		maxRegs = DefaultMaxRegs
	}

	// 1. 按分组键索引
	type key struct {
		fc   int
		base int
	}
	buckets := make(map[key][]PropMeta)

	for _, p := range props {
		if !canRead(p.AccessMode) {
			continue
		}
		// 跳过未配置读功能码的属性 —— FC=0 不是合法 Modbus 读码，会产生无效请求。
		if p.ReadFC <= 0 {
			continue
		}
		k := key{fc: p.ReadFC, base: p.RegisterBase}
		buckets[k] = append(buckets[k], p)
	}

	// 2. 对每个 bucket 计算覆盖范围；超过 maxRegs 则拆分为多段子请求
	groups := make([]RegGroup, 0, len(buckets))
	for k, members := range buckets {
		maxEnd := 0
		for _, m := range members {
			end := m.Offset + m.RegisterCount
			if end > maxEnd {
				maxEnd = end
			}
		}

		// 按段拆分：每段最多覆盖 maxRegs 个寄存器。
		// captured 记录已并入某段的属性，跨界属性留给步骤 2b 的独立分组。
		captured := make([]bool, len(members))
		for start := 0; start < maxEnd; start += maxRegs {
			end := start + maxRegs
			if end > maxEnd {
				end = maxEnd
			}
			segLen := end - start

			var segMembers []GroupMember
			for i, m := range members {
				mStart := m.Offset // 属性在原始 bucket 中的寄存器偏移
				mEnd := m.Offset + m.RegisterCount
				// 属性必须完全落在当前段内
				if mStart >= start && mEnd <= end {
					captured[i] = true
					segMembers = append(segMembers, GroupMember{
						Prop:       m,
						ByteOffset: (m.Offset - start) * 2, // 相对段起始的字节偏移
						ByteLen:    m.RegisterCount * 2,
					})
				}
			}
			if len(segMembers) == 0 {
				continue
			}
			groups = append(groups, RegGroup{
				ReadFC:    k.fc,
				StartAddr: k.base + start,
				Quantity:  segLen,
				Members:   segMembers,
			})
		}

		// 2b. 跨段属性（寄存器区间横跨 maxRegs 段边界，或自身宽度超过 maxRegs）
		// 以自身起点独立成组，从响应帧偏移 0 处切片。
		for i, m := range members {
			if captured[i] {
				continue
			}
			groups = append(groups, RegGroup{
				ReadFC:    k.fc,
				StartAddr: k.base + m.Offset,
				Quantity:  m.RegisterCount,
				Members: []GroupMember{{
					Prop:       m,
					ByteOffset: 0,
					ByteLen:    m.RegisterCount * 2,
				}},
			})
		}
	}

	// 3. 稳定排序：按功能码、起始地址，保证每次重建 plan 时顺序一致
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].ReadFC != groups[j].ReadFC {
			return groups[i].ReadFC < groups[j].ReadFC
		}
		return groups[i].StartAddr < groups[j].StartAddr
	})

	return groups
}

// FindWriteProp 在属性列表中查找指定属性名的可写属性。
// 返回属性元数据和 nil；未找到返回零值和错误。
func FindWriteProp(props []PropMeta, propName string) (PropMeta, error) {
	for _, p := range props {
		if p.Name == propName && canWrite(p.AccessMode) {
			return p, nil
		}
	}
	return PropMeta{}, fmt.Errorf("属性 %q 不可写或不存在", propName)
}

// canRead 判断属性是否可读。
func canRead(access string) bool {
	switch access {
	case "r", "rw":
		return true
	}
	return false
}

// canWrite 判断属性是否可写。
func canWrite(access string) bool {
	switch access {
	case "w", "rw":
		return true
	}
	return false
}
