package engine

import (
	"fmt"
	"sync"
	"time"
)

const defaultCommEventCapacity = 500

// CommunicationEvent is one observed packet or the final failure of a logical request.
type CommunicationEvent struct {
	Seq         uint64    `json:"seq"`
	Time        time.Time `json:"time"`
	DeviceIndex int       `json:"deviceIndex"`
	UnitID      byte      `json:"unitId"`
	Direction   string    `json:"direction"` // TX, RX, or ERR
	Operation   string    `json:"operation"` // read or write
	Attempt     int       `json:"attempt"`
	Hex         string    `json:"hex,omitempty"`
	Error       string    `json:"error,omitempty"`
	ErrorKind   string    `json:"errorKind,omitempty"` // transient / permanent / link / map
	LatencyMs   int64     `json:"latencyMs,omitempty"`
}

// CommunicationStats contains per-session and cumulative transaction counters.
// A retried request is counted once, using its final outcome.
type CommunicationStats struct {
	StartedAt  time.Time `json:"startedAt"`
	Requests   uint64    `json:"requests"`
	Successful uint64    `json:"successful"`
	Failed     uint64    `json:"failed"`
	ErrorRate  float64   `json:"errorRate"`

	// Cumulative 是跨会话累计值：重连不会清零，用于回答「错误率趋势如何」。
	// 以前 reset() 会把统计全部清空，于是每次重连后错误率归零，
	// 现场看到的永远是一片祥和。
	Cumulative CommunicationCounters `json:"cumulative"`

	LastError     string    `json:"lastError,omitempty"`
	LastErrorAt   time.Time `json:"lastErrorAt,omitempty"`
	LastSuccessAt time.Time `json:"lastSuccessAt,omitempty"`
}

// CommunicationCounters 是一组请求计数。
type CommunicationCounters struct {
	Requests   uint64  `json:"requests"`
	Successful uint64  `json:"successful"`
	Failed     uint64  `json:"failed"`
	ErrorRate  float64 `json:"errorRate"`
}

// CommunicationSnapshot is an API-safe monitor view for one device or all
// devices on a channel. DeviceIndex is -1 when the selection is the whole link.
type CommunicationSnapshot struct {
	ChannelIndex int                  `json:"channelIndex"`
	DeviceIndex  int                  `json:"deviceIndex"`
	Stats        CommunicationStats   `json:"stats"`
	Events       []CommunicationEvent `json:"events"`
	NextSeq      uint64               `json:"nextSeq"`
}

type communicationCounters struct {
	requests   uint64
	successful uint64
	failed     uint64
}

// commMonitor keeps the current connected session in a bounded in-memory ring,
// plus cumulative counters that survive reconnects.
type commMonitor struct {
	mu           sync.RWMutex
	channelIndex int
	capacity     int
	startedAt    time.Time
	nextSeq      uint64
	events       []CommunicationEvent
	total        communicationCounters
	devices      map[int]communicationCounters

	// 跨会话累计（reset 不清零）
	cumulative    communicationCounters
	cumDevices    map[int]communicationCounters
	lastError     string
	lastErrorAt   time.Time
	lastSuccessAt time.Time
}

func newCommMonitor(channelIndex, capacity int) *commMonitor {
	if capacity <= 0 {
		capacity = defaultCommEventCapacity
	}
	return &commMonitor{
		channelIndex: channelIndex,
		capacity:     capacity,
		devices:      make(map[int]communicationCounters),
		cumDevices:   make(map[int]communicationCounters),
	}
}

// reset starts a new connection session.
// Sequence values and cumulative counters are preserved so a client using
// afterSeq cannot mistake a reconnect for old data, and so the error-rate
// trend survives reconnects.
func (m *commMonitor) reset(now time.Time) {
	m.mu.Lock()
	m.startedAt = now
	m.events = nil
	m.total = communicationCounters{}
	m.devices = make(map[int]communicationCounters)
	m.mu.Unlock()
}

func (m *commMonitor) tx(deviceIndex int, unitID byte, operation string, attempt int, frame []byte) {
	m.append(CommunicationEvent{
		Time: now(), DeviceIndex: deviceIndex, UnitID: unitID, Direction: "TX",
		Operation: operation, Attempt: attempt, Hex: fmt.Sprintf("% x", frame),
	})
}

func (m *commMonitor) rx(deviceIndex int, unitID byte, operation string, attempt int, frame []byte, latency time.Duration) {
	m.append(CommunicationEvent{
		Time: now(), DeviceIndex: deviceIndex, UnitID: unitID, Direction: "RX",
		Operation: operation, Attempt: attempt, Hex: fmt.Sprintf("% x", frame),
		LatencyMs: latency.Milliseconds(),
	})
}

func (m *commMonitor) complete(deviceIndex int, unitID byte, operation string, attempt int, err error, latency time.Duration) {
	m.mu.Lock()
	m.total.requests++
	m.cumulative.requests++
	stats := m.devices[deviceIndex]
	stats.requests++
	cum := m.cumDevices[deviceIndex]
	cum.requests++

	ts := now()
	if err == nil {
		m.total.successful++
		m.cumulative.successful++
		stats.successful++
		cum.successful++
		m.lastSuccessAt = ts
	} else {
		m.total.failed++
		m.cumulative.failed++
		stats.failed++
		cum.failed++
		m.lastError = err.Error()
		m.lastErrorAt = ts
		m.appendLocked(CommunicationEvent{
			Time: ts, DeviceIndex: deviceIndex, UnitID: unitID, Direction: "ERR",
			Operation: operation, Attempt: attempt, Error: err.Error(),
			ErrorKind: classifyError(err), LatencyMs: latency.Milliseconds(),
		})
	}
	m.devices[deviceIndex] = stats
	m.cumDevices[deviceIndex] = cum
	m.mu.Unlock()
}

// classifyError 把错误归类，便于前端/运维一眼区分「线路问题」与「配置问题」。
func classifyError(err error) string {
	if err == nil {
		return ""
	}
	if isLinkError(err) {
		return "link"
	}
	if isTimeout(err) {
		return "transient"
	}
	return "permanent"
}

func (m *commMonitor) append(event CommunicationEvent) {
	m.mu.Lock()
	m.appendLocked(event)
	m.mu.Unlock()
}

func (m *commMonitor) appendLocked(event CommunicationEvent) {
	m.nextSeq++
	event.Seq = m.nextSeq
	m.events = append(m.events, event)
	if len(m.events) > m.capacity {
		m.events = m.events[len(m.events)-m.capacity:]
	}
}

func (m *commMonitor) snapshot(deviceIndex int, afterSeq uint64, limit int) CommunicationSnapshot {
	if limit <= 0 {
		limit = 200
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	counters := m.total
	cumCounters := m.cumulative
	if deviceIndex >= 0 {
		counters = m.devices[deviceIndex]
		cumCounters = m.cumDevices[deviceIndex]
	}
	stats := CommunicationStats{
		StartedAt: m.startedAt, Requests: counters.requests,
		Successful: counters.successful, Failed: counters.failed,
		Cumulative: CommunicationCounters{
			Requests: cumCounters.requests, Successful: cumCounters.successful,
			Failed: cumCounters.failed, ErrorRate: errRate(cumCounters),
		},
		LastError: m.lastError, LastErrorAt: m.lastErrorAt, LastSuccessAt: m.lastSuccessAt,
	}
	if stats.Requests > 0 {
		stats.ErrorRate = float64(stats.Failed) * 100 / float64(stats.Requests)
	}

	events := make([]CommunicationEvent, 0, limit)
	for _, event := range m.events {
		if event.Seq <= afterSeq || (deviceIndex >= 0 && event.DeviceIndex != deviceIndex) {
			continue
		}
		events = append(events, event)
	}
	if len(events) > limit {
		events = events[len(events)-limit:]
	}
	return CommunicationSnapshot{
		ChannelIndex: m.channelIndex, DeviceIndex: deviceIndex, Stats: stats,
		Events: events, NextSeq: m.nextSeq,
	}
}

func errRate(c communicationCounters) float64 {
	if c.requests == 0 {
		return 0
	}
	return float64(c.failed) * 100 / float64(c.requests)
}

var now = time.Now
