package modbus

import "encoding/binary"

// PDU（Protocol Data Unit）是 Modbus RTU 与 TCP 共享的协议层核心：
//
//	[功能码(1)][数据(N)]
//
// 本文件提供 PDU 级的组装与解析函数，不含任何传输层封装（地址/CRC/MBAP）。

// 单次请求的协议上限。超过这些值的请求在组帧阶段直接拒绝，
// 而不是交给设备去回一个异常码——后者会浪费一个完整的总线往返。
const (
	// MaxReadRegisters 单次读寄存器（FC 03/04）的数量上限。
	MaxReadRegisters = 125
	// MaxWriteRegisters 单次写寄存器（FC 16）的数量上限。
	MaxWriteRegisters = 123
	// MaxReadBits 单次读位（FC 01/02）的数量上限。
	MaxReadBits = 2000
	// MaxWriteBits 单次写位（FC 15）的数量上限。
	MaxWriteBits = 1968
	// MaxAddress 16 位地址空间的寄存器/位地址上限。
	MaxAddress = 0xFFFF
)

// validateReadRequest 校验读请求的地址与数量是否落在协议允许范围内。
// 之前此处不做任何检查：寄存器基址+偏移越界时会被 uint16 静默截断，
// 导致网关读取一个完全无关的寄存器并把它的值当作正确数据上报。
func validateReadRequest(fc byte, startAddr, quantity int) error {
	if startAddr < 0 || startAddr > MaxAddress {
		return ErrInvalidAddress
	}
	if quantity < 1 {
		return ErrInvalidAddress
	}
	switch fc {
	case FCReadCoils, FCReadDiscreteInputs:
		if quantity > MaxReadBits {
			return ErrInvalidAddress
		}
	default:
		if quantity > MaxReadRegisters {
			return ErrInvalidAddress
		}
	}
	if startAddr+quantity-1 > MaxAddress {
		return ErrInvalidAddress
	}
	return nil
}

// ─── 读取请求 PDU ───────────────────────────────────────────
//
// PDU 格式: [FC(1)][起始地址 2B 大端][数量 2B 大端]，共 5 字节。

func buildReadPDU(fc byte, startAddr, quantity int) ([]byte, error) {
	if err := validateReadRequest(fc, startAddr, quantity); err != nil {
		return nil, err
	}
	pdu := make([]byte, 5)
	pdu[0] = fc
	binary.BigEndian.PutUint16(pdu[1:3], uint16(startAddr))
	binary.BigEndian.PutUint16(pdu[3:5], uint16(quantity))
	return pdu, nil
}

// ─── 读取响应 PDU ───────────────────────────────────────────
//
// PDU 格式: [FC(1)][字节计数(1)][数据(N)]
// 其中 N = 2 × quantity（寄存器）或 ceil(quantity/8)（线圈/离散输入）。

// parseReadPDU 从 PDU（不含地址/CRC/MBAP）中提取裸数据。
// 返回的 data 是去掉 FC 与 byteCount 后的纯数据字节。
func parseReadPDU(pdu []byte, fc byte, expectedQuantity int) ([]byte, error) {
	if len(pdu) < 2 {
		return nil, ErrShortFrame
	}
	respFC := pdu[0]
	if isExceptionFC(respFC) {
		// 异常响应: [异常FC(1)][异常码(1)]
		return nil, &ExceptionError{FunctionCode: respFC, ExceptionCode: pdu[1]}
	}
	if respFC != fc {
		return nil, ErrFunctionCodeMismatch
	}
	byteCount := int(pdu[1])
	if expected := readRespByteCount(fc, expectedQuantity); byteCount != expected {
		// 字节计数与请求不符：说明帧错位或设备实现异常，不应继续解析。
		if byteCount > expected || len(pdu) < 2+byteCount {
			return nil, ErrShortFrame
		}
	}
	if len(pdu) < 2+byteCount {
		return nil, ErrShortFrame
	}
	return pdu[2 : 2+byteCount], nil
}

// readRespByteCount 根据 FC 和 quantity 计算响应数据区的字节数。
// 寄存器类(03/04): 每寄存器 2 字节；位类(01/02): 每 8 位 1 字节。
func readRespByteCount(fc byte, quantity int) int {
	switch fc {
	case FCReadCoils, FCReadDiscreteInputs:
		return (quantity + 7) / 8
	default:
		return quantity * 2
	}
}

// ─── 写请求 PDU ─────────────────────────────────────────────

// BuildWriteSingleRegPDU 组装「写单个寄存器」(FC=06) PDU: [FC][addr 2B][value 2B]。
func BuildWriteSingleRegPDU(startAddr int, value uint16) ([]byte, error) {
	if startAddr < 0 || startAddr > MaxAddress {
		return nil, ErrInvalidAddress
	}
	pdu := make([]byte, 5)
	pdu[0] = FCWriteSingleReg
	binary.BigEndian.PutUint16(pdu[1:3], uint16(startAddr))
	binary.BigEndian.PutUint16(pdu[3:5], value)
	return pdu, nil
}

// BuildWriteMultiRegsPDU 组装「写多个寄存器」(FC=10) PDU:
// [FC][addr 2B][quantity 2B][byteCount 1][data N×2B]。
func BuildWriteMultiRegsPDU(startAddr, quantity int, data []byte) ([]byte, error) {
	if startAddr < 0 || startAddr > MaxAddress {
		return nil, ErrInvalidAddress
	}
	if quantity < 1 || quantity > MaxWriteRegisters {
		return nil, ErrInvalidAddress
	}
	if startAddr+quantity-1 > MaxAddress {
		return nil, ErrInvalidAddress
	}
	if len(data) != quantity*2 {
		return nil, ErrInvalidData
	}
	pdu := make([]byte, 6+len(data))
	pdu[0] = FCWriteMultiRegs
	binary.BigEndian.PutUint16(pdu[1:3], uint16(startAddr))
	binary.BigEndian.PutUint16(pdu[3:5], uint16(quantity))
	pdu[5] = byte(len(data))
	copy(pdu[6:], data)
	return pdu, nil
}

// BuildWriteSingleCoilPDU 组装「写单个线圈」(FC=05) PDU: [FC][addr 2B][value 2B]。
// value 为 true 时写入 0xFF00，false 时写入 0x0000。
func BuildWriteSingleCoilPDU(startAddr int, on bool) ([]byte, error) {
	if startAddr < 0 || startAddr > MaxAddress {
		return nil, ErrInvalidAddress
	}
	pdu := make([]byte, 5)
	pdu[0] = FCWriteSingleCoil
	binary.BigEndian.PutUint16(pdu[1:3], uint16(startAddr))
	if on {
		binary.BigEndian.PutUint16(pdu[3:5], 0xFF00)
	} else {
		binary.BigEndian.PutUint16(pdu[3:5], 0x0000)
	}
	return pdu, nil
}

// BuildWriteMultiCoilsPDU 组装「写多个线圈」(FC=15) PDU:
// [FC][addr 2B][quantity 2B][byteCount 1][data ceil(qty/8)]。
//
// bits 按 Modbus 规范排列：首个线圈对应首字节的最低位。
// 此前该 PDU 完全没有实现，导致模型里配置的「0x0F 写多线圈」
// 会被编码层静默退化成 FC05 单线圈写，响应校验再按 FC15 比对而必然失败。
func BuildWriteMultiCoilsPDU(startAddr, quantity int, bits []bool) ([]byte, error) {
	if startAddr < 0 || startAddr > MaxAddress {
		return nil, ErrInvalidAddress
	}
	if quantity < 1 || quantity > MaxWriteBits {
		return nil, ErrInvalidAddress
	}
	if startAddr+quantity-1 > MaxAddress {
		return nil, ErrInvalidAddress
	}
	if len(bits) != quantity {
		return nil, ErrInvalidData
	}
	byteCount := (quantity + 7) / 8
	data := make([]byte, byteCount)
	for i, on := range bits {
		if on {
			data[i/8] |= 1 << (i % 8)
		}
	}
	pdu := make([]byte, 6+byteCount)
	pdu[0] = FCWriteMultiCoils
	binary.BigEndian.PutUint16(pdu[1:3], uint16(startAddr))
	binary.BigEndian.PutUint16(pdu[3:5], uint16(quantity))
	pdu[5] = byte(byteCount)
	copy(pdu[6:], data)
	return pdu, nil
}

// ─── 写响应 PDU 校验 ────────────────────────────────────────

// parseWritePDU 校验写应答 PDU。
// 正常写应答是请求的回显（单写 5 字节）或 [FC][addr][quantity]（多写 5 字节）。
// 异常则返回 ExceptionError。
func parseWritePDU(pdu []byte, reqFC byte, startAddr, quantity int) error {
	if len(pdu) < 1 {
		return ErrShortFrame
	}
	respFC := pdu[0]
	if isExceptionFC(respFC) {
		if len(pdu) < 2 {
			return ErrShortFrame
		}
		return &ExceptionError{FunctionCode: respFC, ExceptionCode: pdu[1]}
	}
	if respFC != reqFC {
		return ErrFunctionCodeMismatch
	}
	// 单写与多写的正常应答都是 5 字节：[FC][addr 2B][value|quantity 2B]
	if len(pdu) < 5 {
		return ErrShortFrame
	}
	switch reqFC {
	case FCWriteSingleReg, FCWriteSingleCoil, FCWriteMultiRegs, FCWriteMultiCoils:
		if got := int(binary.BigEndian.Uint16(pdu[1:3])); got != startAddr {
			return ErrAddressMismatch
		}
		return nil
	}
	return nil
}
