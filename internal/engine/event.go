package engine

import "time"

// EventSink 接收引擎产生的事件。实现方不得阻塞采集 worker。
type EventSink interface {
	PublishTelemetry(TelemetryEvent)
	PublishWriteResult(WriteResultEvent)
}

// TelemetryEvent 表示一个设备完成一轮采集后的完整遥测快照。
type TelemetryEvent struct {
	ChannelIndex int
	DeviceIndex  int
	DeviceName   string
	CommNo       int
	ModelID      string
	ModelName    string
	Online       bool
	Properties   map[string]TelemetryProperty
	Timestamp    time.Time
}

// TelemetryProperty is a single property in a telemetry snapshot, keyed by ID.
type TelemetryProperty struct {
	Name        string
	Unit        string
	Description string
	AccessMode  string
	Value       any
	Timestamp   time.Time
}

// WriteResultEvent 表示一条写命令的最终执行结果。
type WriteResultEvent struct {
	RequestID    string
	ChannelIndex int
	DeviceIndex  int
	OK           bool
	Error        string
	Timestamp    time.Time
}
