package converter

import (
	"encoding/binary"
	"errors"
	"math"
	"strings"
)

// 本文件定义寄存器字节与工程值之间的双向映射，协议无关（Modbus/IEC 均可复用）。
//
// 工程值 = 原始寄存器值 × coefficient + base
//
// 字节序按 Modbus 标准枚举:
//
//	ABCD → 大端（默认）          [A B C D]
//	CDAB → 字交换                 [C D A B]
//	BADC → 字节交换               [B A D C]
//	DCBA → 小端                   [D C B A]
//
// 对 2 字节（单寄存器）使用 AB（大端）/ BA（小端）。

// ErrInvalidByteOrder 不支持的字节序标识。
var ErrInvalidByteOrder = errors.New("不支持的字节序")

// ErrInsufficientBytes 寄存器数据不足以解释目标类型。
var ErrInsufficientBytes = errors.New("寄存器数据字节不足")

// ValidByteOrder 校验字节序标识是否合法（构建期校验使用）。
func ValidByteOrder(order string) bool {
	switch strings.ToUpper(strings.TrimSpace(order)) {
	case "", "AB", "BA", "ABCD", "CDAB", "BADC", "DCBA",
		"ABCDEFGH", "HGFEDCBA", "EFGHABCD", "BADCFEHG":
		return true
	}
	return false
}

// MapRegisters 将裸寄存器字节解释为工程值。
//
//	raw : 响应中对应属性的裸字节（已按 ReadByteRange 切片好）
//	prop: 属性元数据
//
// 位空间的位偏移由 MapRegistersAt 提供，本函数等价于位偏移 0。
func MapRegisters(raw []byte, prop PropMeta) (any, error) {
	return MapRegistersAt(raw, prop, 0)
}

// MapRegistersAt 与 MapRegisters 相同，额外接受「属性在切片首字节内的位偏移」。
//
// bitInChunk 仅对位地址空间（FC01/02）有意义：切片是按字节对齐的，
// 因此属性起始位在切片内部可能不是第 0 位。
func MapRegistersAt(raw []byte, prop PropMeta, bitInChunk int) (any, error) {
	if prop.AddrSpace() == AddrSpaceBit {
		return mapBits(raw, prop, bitInChunk)
	}

	need := prop.RegisterCount * 2
	if need <= 0 {
		return nil, ErrInsufficientBytes
	}
	if len(raw) < need {
		return nil, ErrInsufficientBytes
	}
	raw = raw[:need]

	switch strings.ToLower(prop.DataType) {
	case "bool":
		return extractBit(raw, prop.StartBit), nil

	case "int":
		reordered, err := applyByteOrder(raw, prop.ByteOrder)
		if err != nil {
			return nil, err
		}
		v := interpretInt(reordered, prop)
		return float64(v)*prop.Coefficient + prop.DeltaValue, nil

	case "float":
		reordered, err := applyByteOrder(raw, prop.ByteOrder)
		if err != nil {
			return nil, err
		}
		switch len(reordered) {
		case 4:
			fv := float64(math.Float32frombits(binary.BigEndian.Uint32(reordered)))
			return fv*prop.Coefficient + prop.DeltaValue, nil
		case 8:
			fv := math.Float64frombits(binary.BigEndian.Uint64(reordered))
			return fv*prop.Coefficient + prop.DeltaValue, nil
		default:
			// 非 2/4 寄存器的 float，退化为整数解释
			v := interpretInt(reordered, prop)
			return float64(v)*prop.Coefficient + prop.DeltaValue, nil
		}

	case "string":
		return strings.TrimRight(string(raw), "\x00 "), nil

	default:
		// 未知类型，退化为整数解释
		reordered, err := applyByteOrder(raw, prop.ByteOrder)
		if err != nil {
			return nil, err
		}
		v := interpretInt(reordered, prop)
		return float64(v)*prop.Coefficient + prop.DeltaValue, nil
	}
}

// mapBits 解析位地址空间（FC 01/02）的属性值。
//
// Modbus 位空间的打包规则：首个位对应首字节的最低位（LSB）。
// bool 类型直接取目标位；其他数值类型把连续 span 个位按 LSB-first 组装成整数。
func mapBits(raw []byte, prop PropMeta, bitInChunk int) (any, error) {
	span := prop.bitSpan()
	if span <= 0 {
		span = 1
	}
	if bitInChunk < 0 {
		bitInChunk = 0
	}
	if len(raw)*8 < bitInChunk+span {
		return nil, ErrInsufficientBytes
	}

	if strings.ToLower(prop.DataType) == "bool" {
		return bitAt(raw, bitInChunk), nil
	}

	var v int64
	for i := 0; i < span; i++ {
		if bitAt(raw, bitInChunk+i) {
			v |= 1 << uint(i)
		}
	}
	if prop.Signed != nil && *prop.Signed && span > 0 && span < 64 {
		if v&(1<<uint(span-1)) != 0 {
			v -= 1 << uint(span)
		}
	}
	return float64(v)*prop.Coefficient + prop.DeltaValue, nil
}

// bitAt 读取字节流中第 bit 个位（bit 0 = 首字节最低位）。
func bitAt(raw []byte, bit int) bool {
	if bit < 0 || bit >= len(raw)*8 {
		return false
	}
	return raw[bit/8]&(1<<uint(bit%8)) != 0
}

// interpretInt 把（已按字节序重排的）字节解释为整数，并应用位区间与符号。
//
// 关键修正：此前先 decodeInt 得到有符号值，再用 extractBits 做无符号掩码，
// 结果恒为非负——温度 -10（0xFFF6）会被读成 65526。现在按「无符号取位 +
// 可选符号扩展」处理，负值可以正确还原。
func interpretInt(reordered []byte, prop PropMeta) int64 {
	width := len(reordered) * 8
	var u uint64
	switch len(reordered) {
	case 1:
		u = uint64(reordered[0])
	case 2:
		u = uint64(binary.BigEndian.Uint16(reordered))
	case 3:
		u = uint64(reordered[0])<<16 | uint64(reordered[1])<<8 | uint64(reordered[2])
	case 4:
		u = uint64(binary.BigEndian.Uint32(reordered))
	case 8:
		u = binary.BigEndian.Uint64(reordered)
	default:
		for _, b := range reordered {
			u = u<<8 | uint64(b)
		}
		width = len(reordered) * 8
	}

	startBit, endBit := prop.StartBit, prop.EndBit
	if endBit < startBit {
		endBit = startBit
	}
	if endBit >= width {
		endBit = width - 1
	}
	if startBit < 0 {
		startBit = 0
	}

	span := endBit - startBit + 1
	if span >= 64 {
		v := int64(u)
		if !prop.signedOrAuto(width, startBit, endBit) {
			return int64(u)
		}
		return v
	}

	mask := uint64(1)<<uint(span) - 1
	v := int64((u >> uint(startBit)) & mask)

	if prop.signedOrAuto(width, startBit, endBit) && v&(1<<uint(span-1)) != 0 {
		v -= 1 << uint(span)
	}
	return v
}

// signedOrAuto 决定是否按有符号解释。
//
//   - 显式配置了 signed → 按配置；
//   - 未配置（nil）→ 位区间覆盖整个数据宽度时按有符号（兼容 int16 温度等常见点表），
//     只取部分位（如 16 位里的低 8 位）时按无符号，避免把标志位误读成负数。
func (p PropMeta) signedOrAuto(width, startBit, endBit int) bool {
	if p.Signed != nil {
		return *p.Signed
	}
	return startBit == 0 && endBit == width-1
}

// extractBit 从字节切片中提取指定位（bit 0 = 最低位）。
func extractBit(raw []byte, bit int) bool {
	if bit < 0 || bit >= len(raw)*8 {
		return false
	}
	// Modbus 寄存器按大端传输，bit 0 是整个寄存器值的最低位。
	byteIdx := len(raw) - 1 - bit/8
	return raw[byteIdx]&(1<<(bit%8)) != 0
}

// extractBits 从整数值中提取 [startBit, endBit] 位段（含两端，bit 0 = 最低位）。
// startBit / endBit 越界时收敛，保证位宽掩码始终有效。
func extractBits(v int64, startBit, endBit int) int64 {
	if startBit < 0 {
		startBit = 0
	}
	if endBit < startBit {
		endBit = startBit
	}
	if endBit >= 63 {
		endBit = 62
	}
	mask := (int64(1) << (endBit - startBit + 1)) - 1
	return (v >> startBit) & mask
}

// applyByteOrder 按字节序标识重排字节。
func applyByteOrder(raw []byte, order string) ([]byte, error) {
	order = strings.ToUpper(strings.TrimSpace(order))
	if len(raw) <= 1 {
		return raw, nil
	}
	out := make([]byte, len(raw))
	copy(out, raw)

	switch len(raw) {
	case 2:
		switch order {
		case "", "AB", "ABCD": // 大端
			// 原序不变
		case "BA", "DCBA": // 小端
			out[0], out[1] = raw[1], raw[0]
		default:
			return nil, ErrInvalidByteOrder
		}
	case 4:
		switch order {
		case "", "ABCD": // 大端
			// 原序不变
		case "CDAB": // 字交换
			out[0], out[1] = raw[2], raw[3]
			out[2], out[3] = raw[0], raw[1]
		case "BADC": // 字节交换
			out[0], out[1] = raw[1], raw[0]
			out[2], out[3] = raw[3], raw[2]
		case "DCBA", "BA": // 小端（全反转）
			out[0], out[1], out[2], out[3] = raw[3], raw[2], raw[1], raw[0]
		default:
			return nil, ErrInvalidByteOrder
		}
	case 8:
		// 对 64 位值，按 32 位字组分别处理
		switch order {
		case "", "ABCD", "ABCDEFGH":
			// 原序不变
		case "DCBA", "HGFEDCBA":
			reverse8(out)
		case "CDAB", "EFGHABCD":
			swapWords8(out)
		case "BADC", "BADCFEHG":
			swapBytes8(out)
		default:
			return nil, ErrInvalidByteOrder
		}
	default:
		// 其他长度不做重排
	}
	return out, nil
}

func reverse8(b []byte) {
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
}

func swapWords8(b []byte) {
	b[0], b[1], b[2], b[3], b[4], b[5], b[6], b[7] = b[4], b[5], b[6], b[7], b[0], b[1], b[2], b[3]
}

func swapBytes8(b []byte) {
	b[0], b[1] = b[1], b[0]
	b[2], b[3] = b[3], b[2]
	b[4], b[5] = b[5], b[4]
	b[6], b[7] = b[7], b[6]
}
