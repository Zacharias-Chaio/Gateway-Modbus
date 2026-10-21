package converter

import (
	"fmt"
	"sort"

	"gateway/internal/engine/converter/modbus"
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
// 地址空间（AddrSpace）：这是 FC01/02 与 FC03/04 的本质差异。
//
//	寄存器空间（FC03/04）：1 个单位 = 1 个寄存器 = 2 字节。
//	位空间（FC01/02）    ：1 个单位 = 1 个位，响应数据按 ceil(qty/8) 字节打包。
//
// 此前分组层把两者都按「1 单位 = 2 字节」硬编码（×2），导致任何一个
// 线圈/离散量属性都因为「响应字节数不足」而被每轮静默跳过——既不采集、
// 也不报错，遥测里恒为 null。
//
// 分组规则：
//  1. 按 (readFC, registerBase) 分桶 —— 同一基址 + 同一功能码的属性合并为一次读请求。
//  2. 组内 Quantity = max(registerOffset + 单位宽度)，覆盖所有成员的最远地址。
//  3. 组内 ByteOffset/ByteLen 按地址空间计算，用于从响应数据中切片各属性。

// AddrSpace 表示属性所在的 Modbus 地址空间。
// 直接复用 modbus 包的枚举，避免两套语义漂移。
type AddrSpace = modbus.AddrSpace

const (
	// AddrSpaceRegister 寄存器空间（FC 03/04）：1 单位 = 2 字节。
	AddrSpaceRegister = modbus.AddrSpaceRegister
	// AddrSpaceBit 位空间（FC 01/02）：1 单位 = 1 位，按字节打包。
	AddrSpaceBit = modbus.AddrSpaceBit
)

// AddrSpaceOf 由读功能码推导地址空间。
func AddrSpaceOf(readFC int) AddrSpace { return modbus.AddrSpaceOf(readFC) }

// PropMeta 描述单个属性的协议映射元数据（来自设备模型 JSON）。
type PropMeta struct {
	Index        int     `json:"index"` // 属性索引（模型属性列表中的序号，与物模型保持一致）
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
	Signed       *bool   `json:"signed,omitempty"` // 是否有符号；nil 表示按位区间自动推断

	// RegisterCount 寄存器数量（读取跨度），必填 1~125；由 buildDevicePlan 统一校验。
	RegisterCount int `json:"registerCount"`
}

// AddrSpace 返回该属性所属的地址空间（由读功能码推导；只写属性按写功能码推导）。
func (p PropMeta) AddrSpace() AddrSpace {
	if p.ReadFC > 0 {
		return AddrSpaceOf(p.ReadFC)
	}
	return AddrSpaceOf(p.WriteFC)
}

// bitSpan 返回该属性在位空间下占用的位数。
// 位空间里 registerCount 表示「位数量」；若未填（0），退化为位区间宽度。
func (p PropMeta) bitSpan() int {
	if p.RegisterCount > 0 {
		return p.RegisterCount
	}
	return p.EndBit - p.StartBit + 1
}

// ReadByteRange 返回该属性相对所属分组起始地址的字节偏移与字节长度。
//
//	寄存器空间：偏移 = offset×2，长度 = regCount×2。
//	位空间    ：起始位 = offset，长度 = bitSpan；首字节下标 = offset/8，
//	             占用字节数 = ceil((offset%8 + bitSpan)/8)。
//
// 位空间的偏移必须是相对「分组起始地址」，而不是绝对地址——因为设备返回的
// 是打包后的字节流，而不是每个位单独一个字节。
func (p PropMeta) ReadByteRange(offsetInGroup int) (byteOffset, byteLen int) {
	if p.AddrSpace() == AddrSpaceBit {
		span := p.bitSpan()
		if span <= 0 {
			span = 1
		}
		start := offsetInGroup
		if start < 0 {
			start = 0
		}
		byteOffset = start / 8
		endBit := start%8 + span // 从分组起点算起的位游标终点
		byteLen = (endBit + 7) / 8
		if byteLen < 1 {
			byteLen = 1
		}
		return byteOffset, byteLen
	}
	return offsetInGroup * 2, p.RegisterCount * 2
}

// IsBitSpace 判断该属性是否位于位地址空间（FC01/02）。
func (p PropMeta) IsBitSpace() bool { return p.AddrSpace() == AddrSpaceBit }

// RegGroup 是一次读请求对应的寄存器组。
type RegGroup struct {
	ReadFC    int        // Modbus 功能码
	StartAddr int        // 实际请求的起始地址（= registerBase）
	Quantity  int        // 请求的数量（寄存器数或位数，取决于 AddrSpace）
	AddrSpace AddrSpace  // 地址空间，决定响应帧的字节布局
	Members   []GroupMember
}

// GroupMember 是组内单个属性的定位信息。
type GroupMember struct {
	Prop       PropMeta
	ByteOffset int // 在响应数据中的字节偏移
	ByteLen    int // 该属性的字节长度
}

// BuildGroups 将设备的属性列表按 (readFC, registerBase) 分组。
// 只处理可读属性（accessMode 含 r）。
// maxRegs 限制单个读请求的最大数量（寄存器空间上限 125，位空间上限 2000）；
// 同一分组的覆盖范围超过该值时自动拆分为多个连续子请求。
// maxRegs <= 0 时按地址空间取协议默认上限。
//
// 寄存器区间横跨段边界的属性无法并入任何连续段：以自身起点单独成组，
// 保证仍然被采集（此前会被整组静默丢弃）。
func BuildGroups(props []PropMeta, maxRegs int) []RegGroup {
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

	// 2. 对每个 bucket 计算覆盖范围；超过上限则拆分为多段子请求
	groups := make([]RegGroup, 0, len(buckets))
	for k, members := range buckets {
		space := AddrSpaceOf(k.fc)
		limit := maxRegs
		if limit <= 0 {
			limit = protocolUnitLimit(space)
		}
		if max := protocolUnitLimit(space); limit > max {
			limit = max
		}

		maxEnd := 0
		for _, m := range members {
			if end := m.Offset + unitSpan(m, space); end > maxEnd {
				maxEnd = end
			}
		}

		// 按段拆分：每段最多覆盖 limit 个单位。
		// captured 记录已并入某段的属性，跨界属性留给步骤 2b 的独立分组。
		captured := make([]bool, len(members))
		for start := 0; start < maxEnd; start += limit {
			end := start + limit
			if end > maxEnd {
				end = maxEnd
			}
			segLen := end - start

			var segMembers []GroupMember
			for i, m := range members {
				mStart := m.Offset
				mEnd := m.Offset + unitSpan(m, space)
				// 属性必须完全落在当前段内
				if mStart >= start && mEnd <= end {
					captured[i] = true
					byteOffset, byteLen := m.ReadByteRange(mStart - start)
					segMembers = append(segMembers, GroupMember{
						Prop:       m,
						ByteOffset: byteOffset,
						ByteLen:    byteLen,
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
				AddrSpace: space,
				Members:   segMembers,
			})
		}

		// 2b. 跨段属性（区间横跨段边界，或自身宽度超过 limit）
		// 以自身起点独立成组，从响应帧偏移 0 处切片。
		for i, m := range members {
			if captured[i] {
				continue
			}
			span := unitSpan(m, space)
			byteOffset, byteLen := m.ReadByteRange(0)
			groups = append(groups, RegGroup{
				ReadFC:    k.fc,
				StartAddr: k.base + m.Offset,
				Quantity:  span,
				AddrSpace: space,
				Members: []GroupMember{{
					Prop:       m,
					ByteOffset: byteOffset,
					ByteLen:    byteLen,
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

// unitSpan 返回属性在其地址空间中占用的地址单位数。
func unitSpan(p PropMeta, space AddrSpace) int {
	if space == AddrSpaceBit {
		return p.bitSpan()
	}
	return p.RegisterCount
}

// protocolUnitLimit 返回该地址空间的协议单次读数量上限。
func protocolUnitLimit(space AddrSpace) int {
	if space == AddrSpaceBit {
		return 2000
	}
	return DefaultMaxRegs
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

// canRead 判断属性是否可读（大小写不敏感，避免 "RW" 这类配置静默失效）。
func canRead(access string) bool {
	switch normalizeAccess(access) {
	case "r", "rw", "wr":
		return true
	}
	return false
}

// canWrite 判断属性是否可写（大小写不敏感）。
func canWrite(access string) bool {
	switch normalizeAccess(access) {
	case "w", "rw", "wr":
		return true
	}
	return false
}

// normalizeAccess 归一化访问模式：去空格、转小写。
func normalizeAccess(access string) string {
	out := make([]rune, 0, len(access))
	for _, r := range access {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		out = append(out, r)
	}
	return string(out)
}

// NormalizeAccess 对外暴露访问模式归一化（构建期校验使用）。
func NormalizeAccess(access string) string { return normalizeAccess(access) }
