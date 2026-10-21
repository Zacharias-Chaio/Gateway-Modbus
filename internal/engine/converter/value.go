package converter

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"

	"gateway/internal/engine/converter/modbus"
)

// ErrInvalidValue 写入值无法序列化为目标类型。
var ErrInvalidValue = errors.New("写入值无效")

// 写功能码常量（与前端下拉框、docs/data-models.md 保持一致）。
const (
	WriteFCSingleCoil = 5
	WriteFCMultiCoils = 15
	WriteFCSingleReg  = 6
	WriteFCMultiRegs  = 16
)

// WritePlan 是一次写命令的完整执行计划：可能拆成多帧。
//
// 之所以需要「多帧」，是因为 Modbus 单帧写单寄存器（FC06）只能携带
// 一个 16 位值。当一个属性横跨多个寄存器（如 32 位 float、32/64 位整数）时，
// 有两条互斥的实现路径：
//
//	A. 用 FC16（写多寄存器）一次写完——规范做法，绝大多数设备支持；
//	B. 用 FC06 逐寄存器写——仅在设备明确不支持 FC16、或写多个连续寄存器
//	   会被设备当作一个整体块而拒绝时才需要。
//
// 本实现按模型配置的 WriteFC 决定：配了 16 就走 A（一帧），
// 配了 6 且寄存器数 >1 就走 B（按寄存器拆成多帧，按地址升序依次发送）。
// 见 BuildWritePlan 与 docs/data-models.md 的说明。
type WritePlan struct {
	// Frames 是按顺序依次发送的写 PDU。
	Frames []WriteFrame
	// Mode 说明采用的写策略，用于日志与前端提示。
	Mode string
	// StartAddr 是首个帧的起始地址（用于结果日志）。
	StartAddr int
}

// WriteFrame 是写计划中的一帧。
type WriteFrame struct {
	PDU       []byte
	FC        byte
	StartAddr int
	Quantity  int // 本帧覆盖的寄存器数 / 位数
}

// ValidateWriteFunctionCode 校验「数据类型 + 寄存器数量 + 写功能码」组合是否自洽。
// 在构建采集计划时调用，把配置错误挡在保存阶段，而不是等到运行期才发现
// 「设备已经写成功，网关却判定失败」。
func ValidateWriteFunctionCode(dataType string, regCount, writeFC int, isBitSpace bool) error {
	dt := strings.ToLower(strings.TrimSpace(dataType))
	switch writeFC {
	case 0:
		return nil // 不可写
	case WriteFCSingleCoil, WriteFCMultiCoils:
		// 线圈写（05/15）只作用于位地址空间。若属性是「寄存器空间里的一位」
		// （如 FC03 读到 16 位寄存器、StartBit=5 取一位），用 FC05 写会向整个
		// 寄存器写 0xFF00/0x0000，把同一寄存器内的其它位属性一并清掉。
		//
		// 正确做法是 FC06 读-改-写（读出整寄存器 → 只改目标位 → 写回）。
		// 该路径尚未实现，因此这里明确拒绝，避免「静默破坏其它点位」。
		if !isBitSpace {
			return fmt.Errorf(
				"该属性位于寄存器空间（读功能码 03/04），不能用线圈写功能码 0x%02X：会覆盖整个寄存器、破坏同寄存器内的其它位。"+
					"请改用 0x06 写单寄存器（需配合读-改-写），或把属性改为独立的线圈地址（读功能码 01）",
				writeFC)
		}
		if writeFC == WriteFCSingleCoil && regCount > 1 {
			return fmt.Errorf("写单线圈（0x05）只能写 1 位，但配置了 %d 位；请改用 0x0F 写多线圈", regCount)
		}
		return nil
	case WriteFCSingleReg:
		if dt == "bool" && isBitSpace {
			return fmt.Errorf("位地址空间的 bool 属性应使用 0x05/0x0F 写线圈，而不是 0x06")
		}
		if dt == "float" {
			// 单寄存器（16 位）装不下 IEEE754 浮点。以前这里会退化成
			// 「uint16(int16(v))」把 25.5 写成 25，静默丢精度。
			if regCount == 1 {
				return fmt.Errorf("float 需要 2 个（32位）或 4 个（64位）寄存器，当前配置 %d 个；如设备仅预留 1 个寄存器，请把数据类型改为 int", regCount)
			}
			if regCount != 2 && regCount != 4 {
				return fmt.Errorf("float 的寄存器数量必须为 2 或 4，当前 %d", regCount)
			}
		}
		if regCount > 1 {
			// 允许：用 FC06 逐寄存器拆分写入（见 BuildWritePlan 的 Mode B）。
			return nil
		}
		return nil
	case WriteFCMultiRegs:
		if dt == "bool" && isBitSpace {
			return fmt.Errorf("位地址空间的 bool 属性应使用 0x05/0x0F 写线圈，而不是 0x10")
		}
		if dt == "float" && regCount != 2 && regCount != 4 {
			return fmt.Errorf("float 的寄存器数量必须为 2（32位）或 4（64位），当前 %d", regCount)
		}
		if regCount < 1 {
			return fmt.Errorf("写多寄存器（0x10）至少需要 1 个寄存器")
		}
		if regCount > modbus.MaxWriteRegisters {
			return fmt.Errorf("写多寄存器（0x10）单帧上限 %d 个寄存器，当前配置 %d", modbus.MaxWriteRegisters, regCount)
		}
		return nil
	default:
		return fmt.Errorf("不支持的写功能码 0x%02X（允许 0x05/0x06/0x0F/0x10）", writeFC)
	}
}

// EncodeValuePDU 将工程值序列化为 Modbus 写入 PDU。
//
// 已废弃单帧语义：多寄存器 + FC06 需要拆成多帧，请改用 BuildWritePlan。
// 保留本函数是为了兼容「单寄存器/bool」这类必然单帧的场景。
func EncodeValuePDU(prop PropMeta, rawVal float64, startAddr int) ([]byte, error) {
	plan, err := BuildWritePlan(prop, rawVal, startAddr)
	if err != nil {
		return nil, err
	}
	if len(plan.Frames) != 1 {
		return nil, fmt.Errorf("该写操作需要 %d 帧，请使用 BuildWritePlan", len(plan.Frames))
	}
	return plan.Frames[0].PDU, nil
}

// BuildWritePlan 依据属性配置生成写执行计划（可能多帧）。
//
// startAddr 是属性的绝对起始地址（= registerBase + registerOffset）。
func BuildWritePlan(prop PropMeta, rawVal float64, startAddr int) (WritePlan, error) {
	regCount := prop.RegisterCount
	if regCount < 1 {
		// 以前这里没有校验：regCount=0 时 encodeFloatBytes 会在长度为 0 的
		// 切片上调用 PutUint32 直接 panic，而 panic 发生在 worker goroutine 内，
		// 会带走整个网关进程。
		return WritePlan{}, fmt.Errorf("%w: 寄存器数量为 %d，必须 ≥1", ErrInvalidValue, regCount)
	}

	dataType := strings.ToLower(strings.TrimSpace(prop.DataType))
	writeFC := prop.WriteFC
	if writeFC == 0 {
		// 未配置写功能码时按数据类型推导默认值（兼容历史模型）。
		writeFC = defaultWriteFC(dataType, regCount, prop.IsBitSpace())
	}
	if err := ValidateWriteFunctionCode(prop.DataType, regCount, writeFC, prop.IsBitSpace()); err != nil {
		return WritePlan{}, fmt.Errorf("%w: %v", ErrInvalidValue, err)
	}

	// ── 位空间（FC 01/02 读到的位）写线圈 ──
	if prop.IsBitSpace() || writeFC == WriteFCSingleCoil || writeFC == WriteFCMultiCoils {
		return buildCoilWritePlan(prop, rawVal, startAddr, writeFC)
	}

	// ── 寄存器空间 ──
	data, err := encodeValueBytes(dataType, rawVal, regCount, prop.ByteOrder)
	if err != nil {
		return WritePlan{}, err
	}

	switch writeFC {
	case WriteFCSingleReg:
		if regCount == 1 {
			pdu, err := modbus.BuildWriteSingleRegPDU(startAddr, binary.BigEndian.Uint16(data))
			if err != nil {
				return WritePlan{}, err
			}
			return WritePlan{
				Frames:    []WriteFrame{{PDU: pdu, FC: modbus.FCWriteSingleReg, StartAddr: startAddr, Quantity: 1}},
				Mode:      "single-register",
				StartAddr: startAddr,
			}, nil
		}
		// FC06 逐寄存器写：把已按字节序编码好的数据按寄存器切片，地址升序逐帧发送。
		frames := make([]WriteFrame, 0, regCount)
		for i := 0; i < regCount; i++ {
			word := binary.BigEndian.Uint16(data[i*2 : i*2+2])
			pdu, err := modbus.BuildWriteSingleRegPDU(startAddr+i, word)
			if err != nil {
				return WritePlan{}, err
			}
			frames = append(frames, WriteFrame{
				PDU: pdu, FC: modbus.FCWriteSingleReg, StartAddr: startAddr + i, Quantity: 1,
			})
		}
		return WritePlan{Frames: frames, Mode: "split-single-register", StartAddr: startAddr}, nil

	case WriteFCMultiRegs:
		pdu, err := modbus.BuildWriteMultiRegsPDU(startAddr, regCount, data)
		if err != nil {
			return WritePlan{}, err
		}
		return WritePlan{
			Frames:    []WriteFrame{{PDU: pdu, FC: modbus.FCWriteMultiRegs, StartAddr: startAddr, Quantity: regCount}},
			Mode:      "multi-registers",
			StartAddr: startAddr,
		}, nil
	}

	return WritePlan{}, fmt.Errorf("%w: 无法为功能码 0x%02X 生成写帧", ErrInvalidValue, writeFC)
}

// buildCoilWritePlan 生成线圈写计划。
func buildCoilWritePlan(prop PropMeta, rawVal float64, startAddr int, writeFC int) (WritePlan, error) {
	span := prop.RegisterCount
	if prop.IsBitSpace() {
		span = prop.bitSpan()
	}
	if span < 1 {
		span = 1
	}
	// 位地址空间不允许用位提取区间以外的写法；bool 直接取 rawVal 真假。
	if writeFC == WriteFCSingleCoil || span == 1 {
		pdu, err := modbus.BuildWriteSingleCoilPDU(startAddr, rawVal != 0)
		if err != nil {
			return WritePlan{}, err
		}
		return WritePlan{
			Frames:    []WriteFrame{{PDU: pdu, FC: modbus.FCWriteSingleCoil, StartAddr: startAddr, Quantity: 1}},
			Mode:      "single-coil",
			StartAddr: startAddr,
		}, nil
	}
	bits := make([]bool, span)
	bits[0] = rawVal != 0
	pdu, err := modbus.BuildWriteMultiCoilsPDU(startAddr, span, bits)
	if err != nil {
		return WritePlan{}, err
	}
	return WritePlan{
		Frames:    []WriteFrame{{PDU: pdu, FC: modbus.FCWriteMultiCoils, StartAddr: startAddr, Quantity: span}},
		Mode:      "multi-coils",
		StartAddr: startAddr,
	}, nil
}

// defaultWriteFC 在模型未配置写功能码时推导默认值。
func defaultWriteFC(dataType string, regCount int, isBitSpace bool) int {
	if isBitSpace || dataType == "bool" && regCount == 1 && isBitSpace {
		return WriteFCSingleCoil
	}
	switch dataType {
	case "bool":
		return WriteFCSingleCoil
	case "string":
		return WriteFCMultiRegs
	}
	if regCount <= 1 {
		return WriteFCSingleReg
	}
	return WriteFCMultiRegs
}

// encodeValueBytes 将工程值按 dataType/字节序编码为寄存器字节序列。
func encodeValueBytes(dataType string, rawVal float64, regCount int, byteOrder string) ([]byte, error) {
	if regCount < 1 {
		return nil, fmt.Errorf("%w: 寄存器数量必须 ≥1", ErrInvalidValue)
	}
	switch dataType {
	case "float":
		return encodeFloatBytes(rawVal, regCount, byteOrder)
	case "string":
		data := make([]byte, regCount*2)
		text := strings.TrimSpace(formatFloat(rawVal))
		copy(data, text)
		return data, nil
	default: // int 及未知类型
		return encodeIntBytes(int64(rawVal), regCount, byteOrder)
	}
}

func formatFloat(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}

// encodeIntBytes 将整数序列化并应用字节序（用于多寄存器写入）。
func encodeIntBytes(v int64, regCount int, byteOrder string) ([]byte, error) {
	if regCount < 1 {
		return nil, fmt.Errorf("%w: 寄存器数量必须 ≥1", ErrInvalidValue)
	}
	ordered := make([]byte, regCount*2)
	switch regCount {
	case 2:
		binary.BigEndian.PutUint32(ordered, uint32(int64(v)))
	case 4:
		binary.BigEndian.PutUint64(ordered, uint64(v))
	default:
		// 单寄存器：填充低 16 位；其他长度：大端填充低 N 字节。
		// 注意 regCount 可能大于 4（如 8 寄存器 16 字节），此时高字节补 0。
		width := regCount * 2
		for i := width - 1; i >= 0; i-- {
			ordered[i] = byte(v)
			v >>= 8
		}
	}
	return applyByteOrderForWrite(ordered, byteOrder)
}

// encodeFloatBytes 将浮点数序列化并应用字节序。
func encodeFloatBytes(v float64, regCount int, byteOrder string) ([]byte, error) {
	if regCount < 1 {
		return nil, fmt.Errorf("%w: 寄存器数量必须 ≥1", ErrInvalidValue)
	}
	ordered := make([]byte, regCount*2)
	switch regCount {
	case 1:
		binary.BigEndian.PutUint16(ordered, uint16(int16(v)))
	case 2:
		binary.BigEndian.PutUint32(ordered, math.Float32bits(float32(v)))
	case 4:
		binary.BigEndian.PutUint64(ordered, math.Float64bits(v))
	default:
		// 非 2/4 寄存器宽度的 float：按 float32 解释，其余字节补 0。
		if len(ordered) >= 4 {
			binary.BigEndian.PutUint32(ordered[:4], math.Float32bits(float32(v)))
		} else {
			binary.BigEndian.PutUint16(ordered, uint16(int16(v)))
		}
	}
	return applyByteOrderForWrite(ordered, byteOrder)
}

// applyByteOrderForWrite 是 applyByteOrder 的逆操作写入版本。
//
// 读取时 applyByteOrder 把「设备字节序」重排成「大端语义」；
// 写入时需要对大端编码好的字节做同样的重排，才能得到设备期望的字节序。
func applyByteOrderForWrite(raw []byte, order string) ([]byte, error) {
	return applyByteOrder(raw, order)
}
