package modbus

// TCP 帧封装层（MBAP + PDU）。
//
// Modbus TCP 帧格式（MBAP Header + PDU）:
//
//	[事务ID 2B][协议ID 2B=0x0000][长度 2B][单元ID 1B][PDU(N)]
//
// MBAP = Modbus Application Protocol Header，固定 7 字节（含 unitID）。
// 长度字段 = 后续字节数（unitID + PDU）。
// 无 CRC。
// 本文件的所有函数都是无状态的纯函数。

// tcpBuildFrame 组装一帧完整的 Modbus TCP 报文。
// tid 为事务 ID，用于请求-响应配对（由调用方管理递增）。
func tcpBuildFrame(tid uint16, unitID byte, pdu []byte) []byte {
	frame := make([]byte, 0, 6+1+len(pdu))
	// 事务 ID（大端）
	frame = append(frame, byte(tid>>8), byte(tid))
	// 协议 ID = 0x0000
	frame = append(frame, 0x00, 0x00)
	// 长度 = unitID(1) + PDU
	length := uint16(1 + len(pdu))
	frame = append(frame, byte(length>>8), byte(length))
	// Unit ID
	frame = append(frame, unitID)
	// PDU
	frame = append(frame, pdu...)
	return frame
}

// tcpMBAPHeaderLen MBAP 头固定长度（含 unitID）。
const tcpMBAPHeaderLen = 7

// tcpParseFrameLen 从已读取的 MBAP 头（至少 6 字节）中解析出完整帧总长度。
// 返回 MBAP(6) + length 字段值；头部不足时返回 0。
func tcpParseFrameLen(header []byte) int {
	if len(header) < 6 {
		return 0
	}
	length := int(header[4])<<8 | int(header[5])
	return 6 + length
}

// tcpExtractPDU 从完整 TCP 帧中提取 PDU（校验 tid/protocol/unitID）。
// 返回去掉 MBAP 头后的纯 PDU。
func tcpExtractPDU(frame []byte, expectTID uint16, expectUnitID byte) ([]byte, error) {
	if len(frame) < tcpMBAPHeaderLen {
		return nil, ErrShortFrame
	}
	// 校验事务 ID
	tid := uint16(frame[0])<<8 | uint16(frame[1])
	if tid != expectTID {
		return nil, ErrTransactionIDMismatch
	}
	// 校验协议 ID = 0
	protoID := uint16(frame[2])<<8 | uint16(frame[3])
	if protoID != 0 {
		return nil, ErrProtocolIDMismatch
	}
	length := int(frame[4])<<8 | int(frame[5])
	if length < 1 || len(frame) < 6+length {
		return nil, ErrShortFrame
	}
	unitID := frame[6]
	if unitID != expectUnitID {
		return nil, ErrUnitIDMismatch
	}
	// PDU = frame[7 : 6+length]
	return frame[7 : 6+length], nil
}
