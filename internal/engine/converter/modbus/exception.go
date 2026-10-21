package modbus

import "errors"

// Modbus 功能码常量。
const (
	FCReadCoils          = 0x01 // 读线圈（位，可读写）
	FCReadDiscreteInputs = 0x02 // 读离散输入（位，只读）
	FCReadHoldingRegs    = 0x03 // 读保持寄存器（16位，可读写）
	FCReadInputRegs      = 0x04 // 读输入寄存器（16位，只读）
	FCWriteSingleCoil    = 0x05 // 写单个线圈
	FCWriteSingleReg     = 0x06 // 写单个寄存器
	FCWriteMultiCoils    = 0x0F // 写多个线圈
	FCWriteMultiRegs     = 0x10 // 写多个寄存器
)

// 异常码（Modbus Exception Code）。
const (
	ExcIllegalFunction        = 0x01 // 非法功能码
	ExcIllegalDataAddress     = 0x02 // 非法数据地址
	ExcIllegalDataValue       = 0x03 // 非法数据值
	ExcSlaveDeviceFailure     = 0x04 // 从站设备故障
	ExcAcknowledge            = 0x05 // 确认
	ExcSlaveDeviceBusy        = 0x06 // 从站设备忙
	ExcMemoryParityError      = 0x08 // 存储器奇偶校验错误
	ExcGatewayPathUnavailable = 0x0A // 网关路径不可用
	ExcGatewayTargetNoResp    = 0x0B // 网关目标设备无响应
)

// exceptionText 返回异常码的中文描述。
func exceptionText(code byte) string {
	switch code {
	case ExcIllegalFunction:
		return "非法功能码"
	case ExcIllegalDataAddress:
		return "非法数据地址（请核对寄存器基址/偏移/数量与功能码是否匹配设备点表）"
	case ExcIllegalDataValue:
		return "非法数据值"
	case ExcSlaveDeviceFailure:
		return "从站设备故障"
	case ExcAcknowledge:
		return "从站确认（需稍后重试）"
	case ExcSlaveDeviceBusy:
		return "从站设备忙"
	case ExcMemoryParityError:
		return "存储器奇偶校验错误"
	case ExcGatewayPathUnavailable:
		return "网关路径不可用"
	case ExcGatewayTargetNoResp:
		return "网关目标设备无响应"
	default:
		return "未知异常码"
	}
}

// ExceptionError 表示 Modbus 异常响应。
type ExceptionError struct {
	FunctionCode  byte // 含最高位异常标志（0x80+原始FC）
	ExceptionCode byte
}

func (e *ExceptionError) Error() string {
	return "Modbus 异常: FC=0x" + btoHex(e.FunctionCode) + " code=0x" + btoHex(e.ExceptionCode) + "(" + exceptionText(e.ExceptionCode) + ")"
}

// Transient 判断该异常是否值得重发。
// 从站忙/稍后重试/网关上游无响应属于瞬时状态，重发有意义；
// 非法功能码、非法数据地址等是确定性错误，重发只会白白占用总线时隙，
// 并把同一条错误日志重复 N 次掩盖真实问题。
func (e *ExceptionError) Transient() bool {
	switch e.ExceptionCode {
	case ExcAcknowledge, ExcSlaveDeviceBusy, ExcGatewayTargetNoResp:
		return true
	}
	return false
}

// isExceptionFC 判断功能码是否设置了异常标志位（最高位为1）。
func isExceptionFC(fc byte) bool { return fc&0x80 != 0 }

// AddrSpace 表示 Modbus 的两种地址空间。
//
//	寄存器空间（FC03/04）：1 单位 = 1 个寄存器 = 2 字节。
//	位空间（FC01/02）    ：1 单位 = 1 个位，响应数据按 ceil(qty/8) 字节打包。
type AddrSpace int

const (
	// AddrSpaceRegister 寄存器地址空间（FC 03/04）。
	AddrSpaceRegister AddrSpace = iota
	// AddrSpaceBit 位地址空间（FC 01/02）。
	AddrSpaceBit
)

func (a AddrSpace) String() string {
	if a == AddrSpaceBit {
		return "bit"
	}
	return "register"
}

// AddrSpaceOf 由功能码推导地址空间。
func AddrSpaceOf(fc int) AddrSpace {
	switch fc {
	case FCReadCoils, FCReadDiscreteInputs:
		return AddrSpaceBit
	}
	return AddrSpaceRegister
}

// IsBitFunctionCode 判断功能码是否作用于位地址空间。
func IsBitFunctionCode(fc int) bool { return AddrSpaceOf(fc) == AddrSpaceBit }

// IsValidReadFC 判断是否为受支持的读功能码。
func IsValidReadFC(fc int) bool {
	switch fc {
	case FCReadCoils, FCReadDiscreteInputs, FCReadHoldingRegs, FCReadInputRegs:
		return true
	}
	return false
}

// IsValidWriteFC 判断是否为受支持的写功能码。
func IsValidWriteFC(fc int) bool {
	switch fc {
	case FCWriteSingleCoil, FCWriteSingleReg, FCWriteMultiCoils, FCWriteMultiRegs:
		return true
	}
	return false
}

// ErrShortFrame 帧数据不完整，需要更多字节。
// 转换器为无状态设计：每次 Decode 调用都从 buf 开头解析，
// 不足一帧时返回此错误，由 worker 负责累积更多字节后重试。
var ErrShortFrame = errors.New("Modbus 帧数据不完整")

// ErrCRCMismatch CRC 校验失败。
var ErrCRCMismatch = errors.New("Modbus CRC 校验失败")

// ErrUnitIDMismatch 响应的从站地址与请求不符。
var ErrUnitIDMismatch = errors.New("Modbus 响应从站地址不匹配")

// ErrFunctionCodeMismatch 响应的功能码与请求不符。
var ErrFunctionCodeMismatch = errors.New("Modbus 响应功能码不匹配")

// ErrAddressMismatch 写响应回显的地址与请求不符。
var ErrAddressMismatch = errors.New("Modbus 写响应地址不匹配")

// ErrTransactionIDMismatch TCP 响应的事务 ID 与请求不符。
var ErrTransactionIDMismatch = errors.New("Modbus TCP 事务 ID 不匹配")

// ErrProtocolIDMismatch TCP 响应的协议 ID 非 0。
var ErrProtocolIDMismatch = errors.New("Modbus TCP 协议 ID 不匹配")

// ErrInvalidAddress 请求地址或数量超出 Modbus 协议允许范围。
var ErrInvalidAddress = errors.New("Modbus 地址或数量越界（地址 0~65535，读寄存器 ≤125，读位 ≤2000）")

// ErrIllegalFunctionCode 请求使用了 Modbus 标准之外/未实现的读写功能码。
// 这类错误在构建采集计划时就应该被拦下，而不是把非法功能码真的发到总线上。
var ErrIllegalFunctionCode = errors.New("非法的 Modbus 功能码")

// ErrInvalidData 写请求的数据长度与声明数量不符。
var ErrInvalidData = errors.New("Modbus 写请求数据长度与寄存器数量不符")

// IsFrameResetError 判断错误是否意味着「接收缓冲里可能残留了半截帧」，
// 需要在重发前刷新链路缓冲（Driver.Refresh），否则残留字节会污染下一轮请求。
func IsFrameResetError(err error) bool {
	return errors.Is(err, ErrCRCMismatch) ||
		errors.Is(err, ErrUnitIDMismatch) ||
		errors.Is(err, ErrFunctionCodeMismatch) ||
		errors.Is(err, ErrAddressMismatch) ||
		errors.Is(err, ErrTransactionIDMismatch) ||
		errors.Is(err, ErrProtocolIDMismatch)
}

// IsPermanentError 判断错误是否为「确定性错误」——重发不可能成功。
func IsPermanentError(err error) bool {
	var exc *ExceptionError
	if errors.As(err, &exc) {
		return !exc.Transient()
	}
	return errors.Is(err, ErrInvalidAddress) ||
		errors.Is(err, ErrInvalidData) ||
		errors.Is(err, ErrIllegalFunctionCode)
}

func btoHex(b byte) string {
	const hexDigits = "0123456789ABCDEF"
	return string([]byte{hexDigits[b>>4], hexDigits[b&0x0F]})
}
