package converter

import (
	"fmt"
	"strings"

	"gateway/internal/engine/converter/modbus"
)

// OnlinePropName 是设备在线状态的虚拟属性名（不映射实际寄存器）。
const OnlinePropName = "在线状态"

// isVirtualProp 判断属性是否为「虚拟属性」——不映射任何寄存器/位，
// 由网关自行填充值（如在线状态）。
//
// 判定必须严格：只凭「读写功能码都为 0」是不够的，否则一个漏配功能码的
// 普通属性会被静默当成虚拟属性，永远不采集也不报错。
func isVirtualProp(p *PropMeta) bool {
	if p.ReadFC != 0 || p.WriteFC != 0 {
		return false
	}
	return p.Name == OnlinePropName || p.AccessMode == ""
}

// ValidateProps 校验属性配置，允许位地址空间（等价于 ValidatePropsFor(props, true)）。
//
// 供不知道协议能力的调用方（如 API 保存阶段的通用校验）使用。
func ValidateProps(props []PropMeta) error {
	return ValidatePropsFor(props, true)
}

// ValidatePropsFor 集中校验属性配置。API 保存与引擎构建计划共用同一套规则，
// 保证「保存时通过」与「运行时可执行」不会出现分歧。
//
// supportsBits 表示协议是否支持位地址空间（FC01/02）。不支持时，
// 配置了位功能码的属性会被明确拒绝——否则它会「保存成功但永远采不到值」。
//
// 以前这些校验散落在运行期且大多是静默跳过：属性 ID 缺失要等到每轮
// 遥测发布才打一条 Warn（2Hz × 20 属性 = 40 条/秒），字节序写错只会在读取时
// 返回一个被 continue 吞掉的错误，访问模式写成 "RW" 会让属性永久不采集且毫无提示。
func ValidatePropsFor(props []PropMeta, supportsBits bool) error {
	seenName := make(map[string]bool, len(props))
	seenID := make(map[string]bool, len(props))

	for i := range props {
		p := &props[i]
		label := fmt.Sprintf("属性 #%d", i)
		if p.Name != "" {
			label = fmt.Sprintf("属性 %q", p.Name)
		}

		if strings.TrimSpace(p.Name) == "" {
			return fmt.Errorf("属性 #%d: 属性名不能为空", i)
		}
		if seenName[p.Name] {
			return fmt.Errorf("%s: 属性名重复", label)
		}
		seenName[p.Name] = true

		// 访问模式先归一化，便于后续判断（"RW" / "rw" 视为同一语义）。
		p.AccessMode = NormalizeAccess(p.AccessMode)
		switch p.AccessMode {
		case "", "r", "w", "rw", "wr":
		default:
			return fmt.Errorf("%s: 访问模式 %q 非法（只允许 r / w / rw）", label, p.AccessMode)
		}

		// 虚拟属性（在线状态等）不映射寄存器：只要求有属性 ID。
		if isVirtualProp(p) {
			if p.PropID == "" {
				return fmt.Errorf("%s: 虚拟属性必须填写属性 ID", label)
			}
			continue
		}

		if p.PropID == "" {
			return fmt.Errorf("%s: 属性 ID 不能为空（遥测上报以此为键）", label)
		}
		if seenID[p.PropID] {
			return fmt.Errorf("%s: 属性 ID %q 与其它属性重复", label, p.PropID)
		}
		seenID[p.PropID] = true

		// 功能码白名单。
		if p.ReadFC != 0 && !modbus.IsValidReadFC(p.ReadFC) {
			return fmt.Errorf("%s: 读功能码 0x%02X 非法（只允许 01/02/03/04）", label, p.ReadFC)
		}
		if p.WriteFC != 0 && !modbus.IsValidWriteFC(p.WriteFC) {
			return fmt.Errorf("%s: 写功能码 0x%02X 非法（只允许 05/06/0F/10）", label, p.WriteFC)
		}
		if p.ReadFC == 0 && canRead(p.AccessMode) {
			return fmt.Errorf("%s: 可读属性必须配置读功能码", label)
		}
		if p.WriteFC == 0 && canWrite(p.AccessMode) {
			return fmt.Errorf("%s: 可写属性必须配置写功能码", label)
		}

		// 位地址空间只在协议支持时可用。
		if !supportsBits && (modbus.IsBitFunctionCode(p.ReadFC) || modbus.IsBitFunctionCode(p.WriteFC)) {
			return fmt.Errorf("%s: 当前协议不支持位地址空间（FC01/02）", label)
		}

		// 数量：寄存器空间 ≤125，位空间 ≤2000。
		if p.ReadFC != 0 || p.WriteFC != 0 {
			if p.RegisterCount < 1 {
				return fmt.Errorf("%s: 寄存器/位数量必填（≥1）", label)
			}
			limit := modbus.MaxReadRegisters
			if p.AddrSpace() == AddrSpaceBit {
				limit = modbus.MaxReadBits
			}
			if p.RegisterCount > limit {
				return fmt.Errorf("%s: 寄存器/位数量 %d 超出上限 %d", label, p.RegisterCount, limit)
			}
		}

		// 地址范围：基址 + 偏移 + 数量 必须落在 16 位地址空间内。
		// 否则组帧时会被 uint16 静默截断，使网关去读一个完全无关的地址，
		// 并把那个寄存器的值当作本属性的正确数据上报。
		if p.RegisterBase < 0 || p.RegisterBase > modbus.MaxAddress {
			return fmt.Errorf("%s: 寄存器基址 %d 越界（0~65535）", label, p.RegisterBase)
		}
		if p.Offset < 0 {
			return fmt.Errorf("%s: 寄存器偏移 %d 不能为负", label, p.Offset)
		}
		span := p.RegisterCount
		if span < 1 {
			span = 1
		}
		if p.RegisterBase+p.Offset+span-1 > modbus.MaxAddress {
			return fmt.Errorf("%s: 基址(%d)+偏移(%d)+数量(%d) 超出 65535", label, p.RegisterBase, p.Offset, span)
		}

		// 位区间必须落在数据宽度内（位空间按位数算，寄存器空间按 regCount×16 算）。
		width := p.RegisterCount * 16
		if p.AddrSpace() == AddrSpaceBit {
			width = p.RegisterCount
		}
		if p.StartBit < 0 || p.EndBit < p.StartBit || p.EndBit >= width {
			return fmt.Errorf("%s: 位区间 %d~%d 超出数据宽度 %d 位", label, p.StartBit, p.EndBit, width)
		}

		// 字节序白名单。
		if !ValidByteOrder(p.ByteOrder) {
			return fmt.Errorf("%s: 字节序 %q 非法（只允许 ABCD/CDAB/BADC/DCBA 或 AB/BA）", label, p.ByteOrder)
		}

		// 写功能码与数据类型/数量的组合一致性。
		if p.WriteFC != 0 {
			if err := ValidateWriteFunctionCode(p.DataType, p.RegisterCount, p.WriteFC, p.AddrSpace() == AddrSpaceBit); err != nil {
				return fmt.Errorf("%s: %v", label, err)
			}
		}
	}
	return nil
}
