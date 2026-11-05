// Package store 负责 SQLite 配置库的打开、迁移与持久化。
package store

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gateway/internal/config"
	"gateway/internal/logx"

	"gorm.io/datatypes"
	gormlogger "gorm.io/gorm/logger"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// Open 打开（或创建）SQLite 配置库并自动迁移表结构。
func Open(path string) (*gorm.DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}

	// PRAGMA 设置是并发正确性的前提：配置保存（写）与采集热重载（读）
	// 天然并发，默认 rollback journal + 无 busy_timeout 时一次瞬时
	// 「database is locked」就足以让热重载读到空配置——而空配置会
	// 把全部链路停掉。WAL 让读写互不阻塞，busy_timeout 提供重试窗口。
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logx.NewGormLogger(gormlogger.Warn),
	})
	if err != nil {
		return nil, err
	}

	// 迁移前先备份，迁移失败时可人工回滚。
	// 用户确认不保留存量数据，但「迁移中途掉电导致配置库变成半截」这件事
	// 不该靠「反正可以删库重来」兜底——一次备份只要一次 io.Copy。
	if err := migrateLegacyChannels(db, path); err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(&DeviceModel{}, &Channel{}, &config.Record{}, &config.Provisioning{}); err != nil {
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
//
// 整个迁移包在单个事务里：以前 DROP TABLE → AutoMigrate → 回写三步
// 不在事务中，中途失败（磁盘满、进程被杀）会让 channels 表已删除、
// 数据在新表中不存在，配置永久丢失且下次启动不会重跑迁移。
func migrateLegacyChannels(db *gorm.DB, path string) error {
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

	// 迁移前备份（失败不阻断，但会记录告警）。
	if err := backupDatabase(db, path); err != nil {
		logx.Module("store").Warn("迁移前备份配置库失败，继续迁移", "err", err, "path", path)
	}

	return db.Transaction(func(tx *gorm.DB) error {
		var rows []legacyChannelRow
		if err := tx.Raw("SELECT id, name, type, config, devices, created_at, updated_at FROM channels ORDER BY id ASC").Scan(&rows).Error; err != nil {
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
		if err := tx.Exec("DROP TABLE channels").Error; err != nil {
			return fmt.Errorf("重建 channels 表失败: %w", err)
		}
		if err := tx.AutoMigrate(&Channel{}); err != nil {
			return fmt.Errorf("重建 channels 表失败: %w", err)
		}
		if len(converted) > 0 {
			if err := tx.Create(&converted).Error; err != nil {
				return fmt.Errorf("回写迁移后的链路记录失败: %w", err)
			}
		}
		return nil
	})
}

// backupDatabase 把当前数据库文件复制一份带时间戳的副本。
// 先用 SQLite 的 VACUUM INTO 做不到（可能正持有写锁），这里退回文件级复制：
// 关闭 WAL 检查点后主库文件已包含全部已提交数据。
func backupDatabase(db *gorm.DB, path string) error {
	if path == "" || strings.HasPrefix(path, ":memory:") {
		return nil
	}
	// 触发一次 WAL 检查点，确保主库文件是最新的。
	_ = db.Exec("PRAGMA wal_checkpoint(FULL)").Error

	src, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer src.Close()

	dstPath := fmt.Sprintf("%s.bak-%s", path, time.Now().Format("20060102-150405"))
	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	return dst.Sync()
}

// BackupForUpgrade 在版本升级前调用，行为与迁移前备份一致。
func BackupForUpgrade(db *gorm.DB, path string) error {
	return backupDatabase(db, path)
}

// ChannelIDFromIndex 由通道索引生成通道ID：Channel-{索引}。
// 通道ID 与通道索引一一对应且由服务端分配，客户端不可指定。
func ChannelIDFromIndex(index int) string {
	return fmt.Sprintf("Channel-%d", index)
}

// Close 关闭底层数据库连接（进程退出或测试清理时调用）。
func Close(db *gorm.DB) error {
	if db == nil {
		return nil
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
