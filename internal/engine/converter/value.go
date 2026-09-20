package converter

import (
	"encoding/binary"
	"errors"
	"math"
	"strings"

	"gateway/internal/engine/converter/modbus"
)

// ErrInvalidValue 写入值无法序列化为目标类型。
var ErrInvalidValue = errors.New("写入值无效")

// EncodeValuePDU 将工程值序列化为 Modbus 写入 PDU。
//
// 参数:
//   - prop:     属性元数据（含 dataType / startBit / endBit / byteOrder / writeFC）
//   - rawVal:   已逆变换的原始寄存器值（engineering = raw×coef+delta 的逆，调用方负责）
//   - startAddr: 写入起始地址
//
// 返回完整的 PDU（功能码+数据），可直接传给 FrameIO.EncodeWrite。
func EncodeValuePDU(prop PropMeta, rawVal float64, startAddr int) ([]byte, error) {
	regCount := prop.RegisterCount
	switch strings.ToLower(prop.DataType) {
	case "bool":
		// 单线圈写入 FC=05
		return modbus.BuildWriteSingleCoilPDU(startAddr, rawVal != 0), nil

	case "int":
		if regCount <= 1 {
			// 单寄存器 FC=06
			return modbus.BuildWriteSingleRegPDU(startAddr, uint16(int16(rawVal))), nil
		}
		// 多寄存器 FC=10
		data := encodeIntBytes(int64(rawVal), regCount, prop.ByteOrder)
		return modbus.BuildWriteMultiRegsPDU(startAddr, regCount, data), nil

	case "float":
		data := encodeFloatBytes(rawVal, regCount, prop.ByteOrder)
		if regCount <= 1 {
			return modbus.BuildWriteSingleRegPDU(startAddr, uint16(binary.BigEndian.Uint16(data))), nil
		}
		return modbus.BuildWriteMultiRegsPDU(startAddr, regCount, data), nil

	case "string":
		if regCount > 0 {
			data := make([]byte, regCount*2)
			return modbus.BuildWriteMultiRegsPDU(startAddr, regCount, data), nil
		}
		return nil, ErrInvalidValue

	default:
		// 退化为 int
		if regCount <= 1 {
			return modbus.BuildWriteSingleRegPDU(startAddr, uint16(int16(rawVal))), nil
		}
		data := encodeIntBytes(int64(rawVal), regCount, prop.ByteOrder)
		return modbus.BuildWriteMultiRegsPDU(startAddr, regCount, data), nil
	}
}

// encodeIntBytes 将整数序列化并应用字节序（用于多寄存器写入）。
func encodeIntBytes(v int64, regCount int, byteOrder string) []byte {
	ordered := make([]byte, regCount*2)
	switch regCount {
	case 2:
		binary.BigEndian.PutUint32(ordered, uint32(int32(v)))
	case 4:
		binary.BigEndian.PutUint64(ordered, uint64(v))
	default:
		// 单寄存器或其他长度，大端填充
		for i := regCount*2 - 1; i >= 0; i-- {
			ordered[i] = byte(v)
			v >>= 8
		}
	}
	out, _ := applyByteOrderForWrite(ordered, byteOrder)
	return out
}

// encodeFloatBytes 将浮点数序列化并应用字节序。
func encodeFloatBytes(v float64, regCount int, byteOrder string) []byte {
	ordered := make([]byte, regCount*2)
	switch regCount {
	case 1:
		binary.BigEndian.PutUint16(ordered, uint16(int16(v)))
	case 2:
		binary.BigEndian.PutUint32(ordered, math.Float32bits(float32(v)))
	case 4:
		binary.BigEndian.PutUint64(ordered, math.Float64bits(v))
	default:
		binary.BigEndian.PutUint32(ordered, math.Float32bits(float32(v)))
	}
	out, _ := applyByteOrderForWrite(ordered, byteOrder)
	return out
}

// applyByteOrderForWrite 是 applyByteOrder 的逆操作写入版本。
// 写入时需要把"大端排列的工程值"按目标设备要求的字节序编码。
// 由于读取和写入是互逆的，这里复用同一套重排逻辑。
func applyByteOrderForWrite(raw []byte, order string) ([]byte, error) {
	return applyByteOrder(raw, order)
}
