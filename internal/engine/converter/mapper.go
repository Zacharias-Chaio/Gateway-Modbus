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

// MapRegisters 将裸寄存器字节解释为工程值。
//
// 参数:
//   - raw:       响应中对应属性的裸字节（已按 ByteOffset 切片好）
//   - prop:      属性元数据（含 dataType / startBit / endBit / byteOrder / coefficient / deltaValue）
//
// 位提取规则：startBit/endBit 定义在 registerCount × 16 位宽度内的位区间（bit 0 = 最低位）。
// string 类型不走位提取，按字节级处理。
func MapRegisters(raw []byte, prop PropMeta) (any, error) {
	need := prop.RegisterCount * 2
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
		full := decodeInt(reordered)
		v := extractBits(int64(full), prop.StartBit, prop.EndBit)
		return float64(v)*prop.Coefficient + prop.DeltaValue, nil

	case "float":
		reordered, err := applyByteOrder(raw, prop.ByteOrder)
		if err != nil {
			return nil, err
		}
		var fv float64
		switch len(reordered) {
		case 4:
			fv = float64(math.Float32frombits(binary.BigEndian.Uint32(reordered)))
		case 8:
			fv = math.Float64frombits(binary.BigEndian.Uint64(reordered))
		default:
			// 非 2/4 寄存器的 float，退化为 int 解释
			v := decodeInt(reordered)
			return float64(v)*prop.Coefficient + prop.DeltaValue, nil
		}
		return fv*prop.Coefficient + prop.DeltaValue, nil

	case "string":
		return strings.TrimRight(string(raw), "\x00 "), nil

	default:
		// 未知类型，退化为 int
		reordered, err := applyByteOrder(raw, prop.ByteOrder)
		if err != nil {
			return nil, err
		}
		full := decodeInt(reordered)
		v := extractBits(int64(full), prop.StartBit, prop.EndBit)
		return float64(v)*prop.Coefficient + prop.DeltaValue, nil
	}
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
// startBit / endBit 越界时收敛为单点，保证位宽掩码始终有效。
func extractBits(v int64, startBit, endBit int) int64 {
	if startBit < 0 {
		startBit = 0
	}
	if endBit < startBit {
		endBit = startBit
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
		case "DCBA": // 小端（全反转）
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

// decodeInt 将重排后的字节解释为有符号整数（支持 2/4/8 字节）。
func decodeInt(b []byte) int64 {
	switch len(b) {
	case 2:
		return int64(int16(binary.BigEndian.Uint16(b)))
	case 4:
		return int64(int32(binary.BigEndian.Uint32(b)))
	case 8:
		return int64(binary.BigEndian.Uint64(b))
	default:
		var v int64
		for _, x := range b {
			v = v<<8 | int64(x)
		}
		return v
	}
}

func reverse8(b []byte) {
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
}

func swapWords8(b []byte) {
	// [A B C D E F G H] → [E F G H A B C D]
	b[0], b[4] = b[4], b[0]
	b[1], b[5] = b[5], b[1]
	b[2], b[6] = b[6], b[2]
	b[3], b[7] = b[7], b[3]
}

func swapBytes8(b []byte) {
	// [A B C D E F G H] → [B A D C F E H G]
	for i := 0; i < 8; i += 2 {
		b[i], b[i+1] = b[i+1], b[i]
	}
}
