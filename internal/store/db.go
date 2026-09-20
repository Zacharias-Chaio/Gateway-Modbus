package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gateway/internal/config"
	"gateway/internal/logx"

	"github.com/glebarez/sqlite"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// ChannelIDFromIndex 按固定生成规则合成通道ID：Channel-{通道索引}。
// 通道ID 与通道索引均为自动生成、不可修改，二者始终保持一一对应。
func ChannelIDFromIndex(index int) string {
	return "Channel-" + strconv.Itoa(index)
}

// Open 打开（或创建）SQLite 配置库并自动迁移表结构。
func Open(path string) (*gorm.DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{
		Logger: logx.NewGormLogger(gormlogger.Warn),
	})
	if err != nil {
		return nil, err
	}
	// 先把历史「整数自增主键」的链路表迁移为 Channel-{通道索引} 结构，
	// 再做自动迁移，避免 AutoMigrate 试图原地修改主键列类型。
	if err := migrateLegacyChannels(db); err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(&DeviceModel{}, &Channel{}, &config.Record{}); err != nil {
		return nil, err
	}
	return db, nil
}

// legacyChannelRow 是旧结构（整数自增主键）链路表的行镜像。
type legacyChannelRow struct {
	ID        int
	Name      string
	Type      string
	Config    []byte
	Devices   []byte
	CreatedAt time.Time
	UpdatedAt time.Time
}

// migrateLegacyChannels 把历史链路表迁移到新结构：
// 旧行按自增 id 升序重编号为通道索引 0,1,2…，主键改写为 Channel-{索引}，
// 其余字段原样保留。新结构（id 为 TEXT）或尚无表时为空操作。
func migrateLegacyChannels(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Channel{}) {
		return nil
	}
	type column struct {
		Name string
		Type string
	}
	var cols []column
	if err := db.Raw("PRAGMA table_info(channels)").Scan(&cols).Error; err != nil {
		return fmt.Errorf("检查 channels 表结构失败: %w", err)
	}
	legacy := false
	for _, c := range cols {
		if c.Name == "id" && strings.Contains(strings.ToUpper(c.Type), "INT") {
			legacy = true
		}
	}
	if !legacy {
		return nil // 已是新结构
	}
	var rows []legacyChannelRow
	if err := db.Raw("SELECT id, name, type, config, devices, created_at, updated_at FROM channels ORDER BY id ASC").Scan(&rows).Error; err != nil {
		return fmt.Errorf("读取历史链路记录失败: %w", err)
	}
	converted := make([]Channel, 0, len(rows))
	for i, r := range rows {
		converted = append(converted, Channel{
			ID:           ChannelIDFromIndex(i),
			ChannelIndex: i,
			Name:         r.Name,
			Type:         r.Type,
			Config:       datatypes.JSON(r.Config),
			Devices:      datatypes.JSON(r.Devices),
			CreatedAt:    r.CreatedAt,
			UpdatedAt:    r.UpdatedAt,
		})
	}
	if err := db.Exec("DROP TABLE channels").Error; err != nil {
		return fmt.Errorf("重建 channels 表失败: %w", err)
	}
	if err := db.AutoMigrate(&Channel{}); err != nil {
		return fmt.Errorf("重建 channels 表失败: %w", err)
	}
	if len(converted) > 0 {
		if err := db.Create(&converted).Error; err != nil {
			return fmt.Errorf("回写迁移后的链路记录失败: %w", err)
		}
	}
	return nil
}
