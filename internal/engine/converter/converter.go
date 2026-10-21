// Package converter 定义协议转换层的统一抽象。
//
// 转换器（Converter）负责协议知识：PDU 组帧、CRC/MBAP 校验、异常码解析。
// 转换器为无状态设计（TCP 仅持有自增事务 ID 计数器），可被安全复用。
//
// 与 connector（传输层）和 engine（编排层）的关系：
//
//	Worker ──持有──► Driver(connector)     传输层：字节收发
//	         ──持有──► Converter(converter) 协议层：帧组装/解析
//	         ──持有──► DevicePlan(engine)   编排层：分组/调度/值缓存
package converter

import (
	"errors"
	"sync"

	"gateway/internal/engine/converter/modbus"
)

// ErrShortFrame 帧数据不完整，需要更多字节。
// 统一暴露给 worker 做渐进式帧读取。
var ErrShortFrame = errors.New("协议帧数据不完整")

// ErrUnsupportedProtocol 不支持的协议类型。
var ErrUnsupportedProtocol = errors.New("不支持的协议类型")

// Protocol 标识协议类型，与设备模型 profile.protocolType 对应。
type Protocol string

const (
	ModbusRTU Protocol = "Modbus RTU"
	ModbusTCP Protocol = "Modbus TCP"
)

// FrameIO 是协议转换器的统一接口。
// 无论 RTU 还是 TCP，对 worker 而言暴露的都是统一的组帧/解帧方法。
//
// 设计说明：
//   - EncodeRead/EncodeWrite 返回完整请求帧 + 可选的事务标识符（TCP 用）。
//   - DecodeRead/DecodeWrite 接收累积的字节缓冲，不足返回 ErrShortFrame。
//   - 工作器负责字节累积与重试，转换器始终保持无状态。
type FrameIO interface {
	// Protocol 返回协议标识。
	Protocol() string

	// EncodeRead 组装读取请求帧。
	//   unitID: 从站地址 / 单元 ID
	//   fc:     Modbus 功能码
	//   startAddr, quantity: 起始地址与数量（寄存器数或位数，取决于功能码）
	//   返回: 完整帧字节 + 事务标识符（TCP 用，RTU 为 0）
	EncodeRead(unitID byte, fc, startAddr, quantity int) ([]byte, uint16, error)

	// EncodeWrite 组装写入请求帧（pdu 已由 value.go 序列化好）。
	EncodeWrite(unitID byte, pdu []byte) ([]byte, uint16, error)

	// DecodeRead 解析读取响应帧，返回裸数据字节。
	//   tid: 请求时分配的事务标识符（TCP 用于配对校验，RTU 忽略）
	//   buf 不足一帧时返回 ErrShortFrame。
	DecodeRead(buf []byte, tid uint16, expectUnitID, fc byte, quantity int) ([]byte, error)

	// DecodeWrite 校验写入响应帧。
	DecodeWrite(buf []byte, tid uint16, expectUnitID, fc byte, startAddr, quantity int) error
}

// ─── 协议注册表 ──────────────────────────────────────────────
//
// 新增一种协议只需在本包或独立包内调用 RegisterProtocol，
// API 层的协议白名单从 Names() 生成，不必再改 4 处 switch。

// ProtocolSpec 描述一种可用的协议。
type ProtocolSpec struct {
	Name          Protocol // 协议名，与 profile.protocolType 对应
	InterfaceType string   // 允许的链路类型：Serial / Network / 空=不限
	SupportsBits  bool     // 是否支持位地址空间（FC01/02）
	New           func() FrameIO
}

var (
	protocolMu       sync.RWMutex
	protocolRegistry = map[Protocol]ProtocolSpec{}
	protocolOrder    []Protocol
)

// RegisterProtocol 注册协议实现。重复注册同名协议会覆盖实现但保留顺序。
func RegisterProtocol(spec ProtocolSpec) {
	if spec.Name == "" || spec.New == nil {
		return
	}
	protocolMu.Lock()
	defer protocolMu.Unlock()
	if _, exists := protocolRegistry[spec.Name]; !exists {
		protocolOrder = append(protocolOrder, spec.Name)
	}
	protocolRegistry[spec.Name] = spec
}

// Names 返回全部已注册协议名（按注册顺序）。API 白名单由此生成。
func Names() []Protocol {
	protocolMu.RLock()
	defer protocolMu.RUnlock()
	out := make([]Protocol, len(protocolOrder))
	copy(out, protocolOrder)
	return out
}

// Lookup 查找协议规格。
func Lookup(name Protocol) (ProtocolSpec, bool) {
	protocolMu.RLock()
	defer protocolMu.RUnlock()
	spec, ok := protocolRegistry[name]
	return spec, ok
}

// Exists 判断协议是否已注册（替代 API 层的硬编码白名单）。
func Exists(name string) bool {
	_, ok := Lookup(Protocol(name))
	return ok
}

// InterfaceTypeOf 返回协议允许的链路类型；未知协议返回空串。
func InterfaceTypeOf(name string) string {
	if spec, ok := Lookup(Protocol(name)); ok {
		return spec.InterfaceType
	}
	return ""
}

// SupportsBitsOf 返回协议是否支持位地址空间。
func SupportsBitsOf(name string) bool {
	if spec, ok := Lookup(Protocol(name)); ok {
		return spec.SupportsBits
	}
	return false
}

func init() {
	RegisterProtocol(ProtocolSpec{
		Name: ModbusRTU, InterfaceType: "Serial", SupportsBits: true,
		New: func() FrameIO { return &rtuAdapter{inner: modbus.NewRTU()} },
	})
	RegisterProtocol(ProtocolSpec{
		Name: ModbusTCP, InterfaceType: "Network", SupportsBits: true,
		New: func() FrameIO { return &tcpAdapter{inner: modbus.NewTCP()} },
	})
}

// ─── 通用辅助 ────────────────────────────────────────────────

// 编译期断言：modbus.AddrSpaceOf 与包内 AddrSpaceOf 语义必须一致。
var _ = func() bool {
	if modbus.AddrSpaceOf(1) == modbus.AddrSpaceBit {
		return true
	}
	panic("converter: modbus.AddrSpaceOf 与 converter.AddrSpaceOf 语义不一致")
}()

// ─── RTU 适配器 ──────────────────────────────────────────────

// rtuAdapter 将 modbus.RTU 包装为 FrameIO 接口。
type rtuAdapter struct{ inner *modbus.RTU }

func (a *rtuAdapter) Protocol() string { return a.inner.Protocol() }

func (a *rtuAdapter) EncodeRead(unitID byte, fc, startAddr, quantity int) ([]byte, uint16, error) {
	frame, err := a.inner.EncodeRead(unitID, byte(fc), startAddr, quantity)
	return frame, 0, err // RTU 无事务 ID
}

func (a *rtuAdapter) EncodeWrite(unitID byte, pdu []byte) ([]byte, uint16, error) {
	frame, err := a.inner.EncodeWrite(unitID, pdu)
	return frame, 0, err
}

func (a *rtuAdapter) DecodeRead(buf []byte, _ uint16, expectUnitID, fc byte, quantity int) ([]byte, error) {
	data, err := a.inner.DecodeRead(buf, expectUnitID, fc, quantity)
	return data, wrapShort(err)
}

func (a *rtuAdapter) DecodeWrite(buf []byte, _ uint16, expectUnitID, fc byte, startAddr, quantity int) error {
	return wrapShort(a.inner.DecodeWrite(buf, expectUnitID, fc, startAddr, quantity))
}

// ─── TCP 适配器 ──────────────────────────────────────────────

// tcpAdapter 将 modbus.TCP 包装为 FrameIO 接口。
type tcpAdapter struct{ inner *modbus.TCP }

func (a *tcpAdapter) Protocol() string { return a.inner.Protocol() }

func (a *tcpAdapter) EncodeRead(unitID byte, fc, startAddr, quantity int) ([]byte, uint16, error) {
	return a.inner.EncodeRead(unitID, byte(fc), startAddr, quantity)
}

func (a *tcpAdapter) EncodeWrite(unitID byte, pdu []byte) ([]byte, uint16, error) {
	return a.inner.EncodeWrite(unitID, pdu)
}

func (a *tcpAdapter) DecodeRead(buf []byte, tid uint16, expectUnitID, fc byte, quantity int) ([]byte, error) {
	data, err := a.inner.DecodeRead(buf, tid, expectUnitID, fc, quantity)
	return data, wrapShort(err)
}

func (a *tcpAdapter) DecodeWrite(buf []byte, tid uint16, expectUnitID, fc byte, startAddr, quantity int) error {
	return wrapShort(a.inner.DecodeWrite(buf, tid, expectUnitID, fc, startAddr, quantity))
}

// wrapShort 将 modbus.ErrShortFrame 转换为本包的 ErrShortFrame，
// 保持 worker 侧只依赖 converter 包的错误定义。
func wrapShort(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, modbus.ErrShortFrame) {
		return ErrShortFrame
	}
	return err
}

// New 根据协议标识创建对应的 FrameIO 实现。
func New(proto string) (FrameIO, error) {
	spec, ok := Lookup(Protocol(proto))
	if !ok {
		return nil, ErrUnsupportedProtocol
	}
	return spec.New(), nil
}

// IsShortFrame 判断错误是否为「帧数据不完整」。
func IsShortFrame(err error) bool { return errors.Is(err, ErrShortFrame) }

// IsPermanent 判断错误是否为确定性错误（重发无意义）。
func IsPermanent(err error) bool { return modbus.IsPermanentError(err) }

// NeedsResync 判断错误后是否需要清空链路残留字节再重发。
func NeedsResync(err error) bool { return modbus.IsFrameResetError(err) }

// AsException 返回 Modbus 异常响应详情（如不是异常响应返回 false）。
func AsException(err error) (*modbus.ExceptionError, bool) { return modbus.AsException(err) }
