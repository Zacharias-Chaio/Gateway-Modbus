package converter

import (
	"fmt"
	"strings"

	"gateway/internal/engine/converter/modbus"
)

// OnlinePropName 是设备在线状态的虚拟属性名（不映射实际寄存器）。
//
// 注意：这只是「默认属性」的中文显示名，用于识别内置在线点；
// 属性之间真正唯一的标识是 PropID，属性名允许重复。
const OnlinePropName = "在线状态"

// PropIssue 描述一条属性级校验问题。
//
// 携带 Index/PropID/PropName 是为了让前端能把错误精确落到表格的某一行，
// 而不是只弹一句「保存失败」让用户自己去日志里找。
type PropIssue struct {
	Index    int    `json:"index"`              // 属性在模型属性列表中的序号
	PropID   string `json:"propId,omitempty"`   // 属性 ID（唯一标识）
	PropName string `json:"propName,omitempty"` // 属性名（仅用于展示）
	Field    string `json:"field,omitempty"`    // 出问题的字段，如 registerBase
	Message  string `json:"message"`            // 面向用户的中文说明
}

func (i PropIssue) Error() string {
	label := fmt.Sprintf("属性 #%d", i.Index)
	if i.PropID != "" {
		label = fmt.Sprintf("属性 %q(id=%s)", i.PropName, i.PropID)
	} else if i.PropName != "" {
		label = fmt.Sprintf("属性 %q", i.PropName)
	}
	return label + ": " + i.Message
}

// PropIssues 是属性校验问题的集合，实现 error 以便直接返回。
type PropIssues []PropIssue

func (p PropIssues) Error() string {
	if len(p) == 0 {
		return ""
	}
	if len(p) == 1 {
		return p[0].Error()
	}
	parts := make([]string, 0, len(p))
	for _, issue := range p {
		parts = append(parts, issue.Error())
	}
	return fmt.Sprintf("共 %d 处属性配置错误：%s", len(p), strings.Join(parts, "；"))
}

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

// ValidatePropsFor 集中校验属性配置，返回**全部**问题而不是遇到第一个就返回。
//
// 唯一性只以 PropID 为准：属性名（name）是给人看的显示字段，允许重复——
// 现场经常有「A相无功功率」出现在不同数据块的情况，按名字判定重名会误伤，
// 而且会让整个设备甚至整条链路被判为无效。
//
// supportsBits 表示协议是否支持位地址空间（FC01/02）。不支持时，
// 配置了位功能码的属性会被明确拒绝——否则它会「保存成功但永远采不到值」。
func ValidatePropsFor(props []PropMeta, supportsBits bool) error {
	var issues PropIssues
	add := func(i int, p *PropMeta, field, format string, args ...any) {
		issue := PropIssue{Index: i, Field: field, Message: fmt.Sprintf(format, args...)}
		if p != nil {
			issue.PropID = p.PropID
			issue.PropName = p.Name
		}
		issues = append(issues, issue)
	}

	seenID := make(map[string]int, len(props)) // PropID → 首次出现的下标

	for i := range props {
		p := &props[i]

		if strings.TrimSpace(p.Name) == "" {
			add(i, p, "name", "属性名不能为空（属性名仅用于展示，但必须填写）")
			// 名字为空不需要继续校验其它字段，但仍把能查的都查完
			continue
		}

		// 访问模式先归一化，便于后续判断（"RW" / "rw" 视为同一语义）。
		p.AccessMode = NormalizeAccess(p.AccessMode)
		switch p.AccessMode {
		case "", "r", "w", "rw", "wr":
		default:
			add(i, p, "accessMode", "访问模式 %q 非法（只允许 r / w / rw）", p.AccessMode)
		}

		// 属性 ID 是唯一标识：所有属性（含虚拟属性）都必须有，且不可重复。
		if strings.TrimSpace(p.PropID) == "" {
			add(i, p, "id", "属性 ID 不能为空（遥测上报与写命令都以属性 ID 为键）")
		} else if first, dup := seenID[p.PropID]; dup {
			add(i, p, "id", "属性 ID %q 与第 %d 个属性重复", p.PropID, first)
		} else {
			seenID[p.PropID] = i
		}

		// 虚拟属性（在线状态等）不映射寄存器，到这里即可。
		if isVirtualProp(p) {
			continue
		}

		// 功能码白名单。
		if p.ReadFC != 0 && !modbus.IsValidReadFC(p.ReadFC) {
			add(i, p, "readFunctionCode", "读功能码 0x%02X 非法（只允许 01/02/03/04）", p.ReadFC)
		}
		if p.WriteFC != 0 && !modbus.IsValidWriteFC(p.WriteFC) {
			add(i, p, "writeFunctionCode", "写功能码 0x%02X 非法（只允许 05/06/0F/10）", p.WriteFC)
		}
		if p.ReadFC == 0 && canRead(p.AccessMode) {
			add(i, p, "readFunctionCode", "可读属性必须配置读功能码")
		}
		if p.WriteFC == 0 && canWrite(p.AccessMode) {
			add(i, p, "writeFunctionCode", "可写属性必须配置写功能码")
		}

		// 位地址空间只在协议支持时可用。
		if !supportsBits && (modbus.IsBitFunctionCode(p.ReadFC) || modbus.IsBitFunctionCode(p.WriteFC)) {
			add(i, p, "readFunctionCode", "当前协议不支持位地址空间（FC01/02）")
		}

		// 数量：寄存器空间 ≤125，位空间 ≤2000。
		if p.ReadFC != 0 || p.WriteFC != 0 {
			limit := modbus.MaxReadRegisters
			if p.AddrSpace() == AddrSpaceBit {
				limit = modbus.MaxReadBits
			}
			if p.RegisterCount < 1 {
				add(i, p, "registerCount", "寄存器/位数量必填（1~%d），当前 %d", limit, p.RegisterCount)
			} else if p.RegisterCount > limit {
				add(i, p, "registerCount", "寄存器/位数量 %d 超出上限 %d", p.RegisterCount, limit)
			}
		}

		// 地址范围：基址 + 偏移 + 数量 必须落在 16 位地址空间内。
		// 否则组帧时会被 uint16 静默截断，使网关去读一个完全无关的地址，
		// 并把那个寄存器的值当作本属性的正确数据上报。
		if p.RegisterBase < 0 || p.RegisterBase > modbus.MaxAddress {
			add(i, p, "registerBase", "寄存器基址 %d 越界（0~65535）", p.RegisterBase)
		} else if p.Offset < 0 {
			add(i, p, "registerOffset", "寄存器偏移 %d 不能为负", p.Offset)
		} else {
			span := p.RegisterCount
			if span < 1 {
				span = 1
			}
			if p.RegisterBase+p.Offset+span-1 > modbus.MaxAddress {
				add(i, p, "registerOffset", "基址(%d)+偏移(%d)+数量(%d) 超出 65535", p.RegisterBase, p.Offset, span)
			}
		}

		// 位区间必须落在数据宽度内（位空间按位数算，寄存器空间按 regCount×16 算）。
		width := p.RegisterCount * 16
		if p.AddrSpace() == AddrSpaceBit {
			width = p.RegisterCount
		}
		if width > 0 && (p.StartBit < 0 || p.EndBit < p.StartBit || p.EndBit >= width) {
			add(i, p, "endBit", "位区间 %d~%d 超出数据宽度 %d 位", p.StartBit, p.EndBit, width)
		}

		// 字节序白名单。
		if !ValidByteOrder(p.ByteOrder) {
			add(i, p, "byteOrder", "字节序 %q 非法（只允许 ABCD/CDAB/BADC/DCBA 或 AB/BA）", p.ByteOrder)
		}

		// 写功能码与数据类型/数量的组合一致性。
		if p.WriteFC != 0 {
			if err := ValidateWriteFunctionCode(p.DataType, p.RegisterCount, p.WriteFC, p.AddrSpace() == AddrSpaceBit); err != nil {
				add(i, p, "writeFunctionCode", "%v", err)
			}
		}
	}

	if len(issues) > 0 {
		return issues
	}
	return nil
}
