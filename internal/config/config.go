// Package config defines static defaults and database-backed gateway settings.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gateway/internal/logx"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// App 应用配置根节点。
type App struct {
	Log     Log     `json:"log" yaml:"log"`
	Gateway Gateway `json:"gateway" yaml:"gateway"`
	NATS    NATS    `json:"nats" yaml:"nats"`
}

// Gateway 是网关实例的静态信息配置。
type Gateway struct {
	GWID     string `json:"gw_id" yaml:"gw_id"`
	SN       string `json:"sn" yaml:"sn"` // 网关硬件序列号
	Location string `json:"location" yaml:"location"`
}

// NATS 是 Core NATS 客户端配置。
type NATS struct {
	Enabled              bool   `json:"enabled" yaml:"enabled"`
	URL                  string `json:"url" yaml:"url"`
	Name                 string `json:"name" yaml:"name"`
	QueueSize            int    `json:"queueSize" yaml:"queueSize"`
	ConnectTimeout       int    `json:"connectTimeout" yaml:"connectTimeout"`
	ReconnectWait        int    `json:"reconnectWait" yaml:"reconnectWait"`
	MaxReconnects        int    `json:"maxReconnects" yaml:"maxReconnects"`
	RetryOnFailedConnect bool   `json:"retryOnFailedConnect" yaml:"retryOnFailedConnect"`
	ReconnectBufSize     int    `json:"reconnectBufSize" yaml:"reconnectBufSize"`
	PingInterval         int    `json:"pingInterval" yaml:"pingInterval"`
	MaxPingsOut          int    `json:"maxPingsOut" yaml:"maxPingsOut"`
	SubjectPrefix        string `json:"subjectPrefix" yaml:"subjectPrefix"`
}

// Log 日志相关配置参数。
type Log struct {
	Level       string `json:"level" yaml:"level"`             // debug | info | warn | error
	Console     bool   `json:"console" yaml:"console"`         // 终端窗口输出
	File        string `json:"file" yaml:"file"`               // 文件输出路径（空则不落文件）
	MaxSizeMB   int    `json:"maxSizeMB" yaml:"maxSizeMB"`     // 单文件大小上限（MB），大小轮转阈值
	MaxBackups  int    `json:"maxBackups" yaml:"maxBackups"`   // 保留历史文件份数
	MaxAgeDays  int    `json:"maxAgeDays" yaml:"maxAgeDays"`   // 历史文件保留天数
	Compress    bool   `json:"compress" yaml:"compress"`       // 历史文件 gzip 压缩
	DailyRotate bool   `json:"dailyRotate" yaml:"dailyRotate"` // 每日 00:00 轮转
	BufferSize  int    `json:"bufferSize" yaml:"bufferSize"`   // 前端出口环形缓冲条数
}

// LogOptions converts persisted logging settings to the logger's runtime options.
func (app App) LogOptions() logx.Options {
	return logx.Options{
		Level: app.Log.Level, Console: app.Log.Console, File: app.Log.File,
		MaxSizeMB: app.Log.MaxSizeMB, MaxBackups: app.Log.MaxBackups, MaxAgeDays: app.Log.MaxAgeDays,
		Compress: app.Log.Compress, DailyRotate: app.Log.DailyRotate, BufferSize: app.Log.BufferSize,
	}
}

// Default returns the static application defaults used to seed a new configuration database.
func Default() App {
	return App{
		Log: Log{
			Level:       "info",
			Console:     true,
			File:        "logs/gateway.log",
			MaxSizeMB:   20,
			MaxBackups:  30,
			MaxAgeDays:  30,
			Compress:    true,
			DailyRotate: true,
			BufferSize:  500,
		},
		Gateway: Gateway{GWID: "gw_000", SN: "", Location: ""},
		NATS: NATS{
			Enabled: false, URL: "nats://127.0.0.1:4222", Name: "gateway",
			QueueSize: 4096, ConnectTimeout: 2000, ReconnectWait: 2000,
			MaxReconnects: -1, RetryOnFailedConnect: true, ReconnectBufSize: 8388608,
			PingInterval: 20000, MaxPingsOut: 3, SubjectPrefix: "powerpulse.gateway",
		},
	}
}

// Settings is the complete gateway configuration exposed by the settings API.
// It is persisted as a single record so all categories update atomically.
type Settings struct {
	App      App                          `json:"app"`
	Hardware map[string]map[string]string `json:"hardware"`
}

// Record is the database representation of Settings.
type Record struct {
	ID       uint           `gorm:"primaryKey"`
	App      datatypes.JSON `gorm:"not null"`
	Hardware datatypes.JSON `gorm:"not null"`
}

func (Record) TableName() string { return "gateway_settings" }

// DefaultHardware returns the hardware mapping used for a newly created configuration database.
// 采集链路已收窄为串口 / 网络（Modbus RTU / TCP），不再提供 CAN 接口映射。
func DefaultHardware() map[string]map[string]string {
	return map[string]map[string]string{
		"Serial": {"COM1": "/dev/ttyS1", "COM2": "/dev/ttyS2"},
	}
}

// DefaultSettings returns the configuration written when a database has no settings record.
func DefaultSettings() Settings {
	return Settings{App: Default(), Hardware: DefaultHardware()}
}

// LoadSettings returns the persisted settings. A missing record is reported to the caller.
func LoadSettings(db *gorm.DB) (Settings, error) {
	var record Record
	if err := db.First(&record, 1).Error; err != nil {
		return Settings{}, err
	}
	var settings Settings
	if err := json.Unmarshal(record.App, &settings.App); err != nil {
		return Settings{}, fmt.Errorf("解析应用配置: %w", err)
	}
	if err := json.Unmarshal(record.Hardware, &settings.Hardware); err != nil {
		return Settings{}, fmt.Errorf("解析硬件配置: %w", err)
	}
	return settings, nil
}

// EnsureSettings loads persisted settings or writes the built-in defaults for a new database.
func EnsureSettings(db *gorm.DB) (Settings, error) {
	settings, err := LoadSettings(db)
	if err == nil {
		return settings, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return Settings{}, err
	}
	settings = DefaultSettings()
	if err := SaveSettings(db, settings); err != nil {
		return Settings{}, err
	}
	return settings, nil
}

// SaveSettings validates and atomically persists all gateway setting categories.
func SaveSettings(db *gorm.DB, settings Settings) error {
	if err := ValidateSettings(settings); err != nil {
		return err
	}
	appData, err := json.Marshal(settings.App)
	if err != nil {
		return err
	}
	hardwareData, err := json.Marshal(settings.Hardware)
	if err != nil {
		return err
	}
	record := Record{ID: 1, App: appData, Hardware: hardwareData}
	return db.Save(&record).Error
}

// ValidateSettings keeps invalid values from being persisted through the settings UI.
func ValidateSettings(settings Settings) error {
	if settings.App.Gateway.GWID == "" || strings.Contains(settings.App.Gateway.GWID, ".") {
		return fmt.Errorf("gateway.gw_id 不能为空且不能包含句点")
	}
	switch settings.App.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level 必须为 debug、info、warn 或 error")
	}
	if settings.App.Log.MaxSizeMB < 0 || settings.App.Log.MaxBackups < 0 || settings.App.Log.MaxAgeDays < 0 || settings.App.Log.BufferSize < 0 {
		return fmt.Errorf("日志数值配置不能为负数")
	}
	if settings.App.NATS.Enabled && (settings.App.NATS.URL == "" || settings.App.NATS.SubjectPrefix == "") {
		return fmt.Errorf("启用 NATS 时必须填写服务地址和主题前缀")
	}
	if settings.App.NATS.QueueSize < 1 || settings.App.NATS.ConnectTimeout < 0 || settings.App.NATS.ReconnectWait < 0 || settings.App.NATS.ReconnectBufSize < 0 || settings.App.NATS.PingInterval < 0 || settings.App.NATS.MaxPingsOut < 0 || settings.App.NATS.MaxReconnects < -1 {
		return fmt.Errorf("NATS 数值配置无效")
	}
	for category, entries := range settings.Hardware {
		if strings.TrimSpace(category) == "" {
			return fmt.Errorf("硬件分类不能为空")
		}
		for label, node := range entries {
			if strings.TrimSpace(label) == "" || strings.TrimSpace(node) == "" {
				return fmt.Errorf("硬件丝印和设备节点不能为空")
			}
		}
	}
	return nil
}
