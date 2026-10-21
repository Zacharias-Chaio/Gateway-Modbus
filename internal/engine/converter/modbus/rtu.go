package modbus

// RTU 帧封装层。
//
// RTU 帧格式:
//
//	[从站地址(1)][PDU(N)][CRC16(2)]
//
// 其中 CRC16 低字节在前。
// 本文件的所有函数都是无状态的纯函数。

// rtuExceptionRespLen 是 RTU 异常响应的固定帧长度：
// [addr(1)][异常FC(1)][异常码(1)][CRC(2)] = 5 字节。
//
// 关键性质：异常帧长度与请求的 quantity 无关，且恒小于任何正常读响应
// （最小的正常读响应是 FC01 + qty=1 → 6 字节）。因此「先按期望长度判
// 短帧、再解析异常码」的顺序永远无法识别异常响应，必须提前探测。
const rtuExceptionRespLen = 5

// rtuBuildFrame 组装一帧完整的 RTU 报文：[addr][PDU][CRC]。
func rtuBuildFrame(unitID byte, pdu []byte) []byte {
	body := make([]byte, 0, 1+len(pdu)+2)
	body = append(body, unitID)
	body = append(body, pdu...)
	return AppendCRC(body)
}

// rtuReadRespLen 根据 PDU 内容推导 RTU 读响应的完整帧长度：
//
//	1(addr) + 1(fc) + 1(byteCount) + byteCount + 2(crc)
//
// 调用方先读 3 字节（addr+fc+byteCount）后即可调用此函数算出总长。
func rtuReadRespLen(fc byte, quantity int) int {
	byteCount := readRespByteCount(fc, quantity)
	return 1 + 1 + 1 + byteCount + 2 // addr + fc + byteCountField + data + crc
}

// tryRTUException 在已累积的字节中探测异常响应帧。
// 只有当缓冲中的前 5 个字节恰好构成一个「地址匹配 + 异常功能码 + CRC 正确」
// 的完整帧时才返回异常错误，避免与正常响应的前缀误判。
func tryRTUException(buf []byte, expectUnitID, fc byte) *ExceptionError {
	if len(buf) < rtuExceptionRespLen {
		return nil
	}
	frame := buf[:rtuExceptionRespLen]
	if frame[0] != expectUnitID {
		return nil
	}
	if frame[1] != fc|0x80 {
		return nil
	}
	if !CheckCRC(frame) {
		return nil
	}
	return &ExceptionError{FunctionCode: frame[1], ExceptionCode: frame[2]}
}

// rtuExtractPDU 从完整 RTU 帧中提取 PDU 数据（校验 addr/CRC）。
// 返回去掉 addr 和 CRC 后的纯 PDU 部分。异常码由 tryRTUException 提前处理。
func rtuExtractPDU(frame []byte, expectUnitID byte) ([]byte, error) {
	if len(frame) < rtuExceptionRespLen {
		return nil, ErrShortFrame
	}
	if frame[0] != expectUnitID {
		return nil, ErrUnitIDMismatch
	}
	if !CheckCRC(frame) {
		return nil, ErrCRCMismatch
	}
	// 去掉 addr(1) 和 crc(2)
	return frame[1 : len(frame)-2], nil
}

// rtuWriteRespLen 写响应帧长度。
// 单写(05/06): addr+fc+value2+crc = 6；多写(0F/10): addr+fc+addr2+qty2+crc = 8。
func rtuWriteRespLen(reqFC byte) int {
	switch reqFC {
	case FCWriteMultiRegs, FCWriteMultiCoils:
		return 8
	default:
		return 6
	}
}
