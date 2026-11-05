package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"
)

// 本文件实现网关的「首次配置」状态。
//
// 网关不做用户登录体系、也不做接口鉴权：没有用户表、没有会话、没有角色，
// 访问控制边界是网络本身（见 README「部署前提」）。
// 网关与平台之间只靠一对接入凭据互相识别：
//
//	网关名（gw_id）  标识本网关是谁，写入 NATS 主题与 device_code
//	Token            由物模型模块生成并分配给本网关
//
// Token 只落库、只在上行方向使用，**永远不出现在任何 HTTP 响应里**。

// TokenLength 是物模型模块分配的 Token 长度（32 位随机字符串）。
const TokenLength = 32

// tokenPattern 限定 Token 为 32 位字母数字。
//
// Token 会被拼进北向请求头，限制字符集可以挡住复制粘贴时混入的空白与
// 不可见字符——这类字符肉眼不可见，却会让北向接入一直失败且极难排查。
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9]{32}$`)

// Provisioning 是网关的接入凭据记录，全库仅一行（ID 恒为 1）。
//
// 拆成独立表而不是塞进 gateway_settings：网关设置会经 GET /api/settings
// 整体回传给前端，Token 混在里面就会被响应带出去。
type Provisioning struct {
	ID            uint      `gorm:"primaryKey"`
	Token         string    `gorm:"not null"`
	ProvisionedAt time.Time `gorm:"not null"`
}

func (Provisioning) TableName() string { return "gateway_provisioning" }

// ProvisioningState 是首次配置门所需的状态快照，也是 GET /api/provisioning 的响应体。
type ProvisioningState struct {
	// Configured 为 true 表示网关名与 Token 都已就绪，网关可以正常使用。
	Configured bool `json:"configured"`
	// GWID 是当前网关名，用于在设置页回显。
	GWID string `json:"gwId"`
}

// ValidateToken 校验并返回规范化后的 Token。
func ValidateToken(token string) (string, error) {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return "", fmt.Errorf("Token 不能为空")
	}
	if !tokenPattern.MatchString(trimmed) {
		// 不复述 Token 内容：错误信息会进日志与前端提示，凭据不应出现在其中。
		return "", fmt.Errorf("Token 必须为 %d 位字母或数字（当前长度 %d）", TokenLength, len(trimmed))
	}
	return trimmed, nil
}

// LoadProvisioning 读取接入凭据记录；未配置时返回 gorm.ErrRecordNotFound。
func LoadProvisioning(db *gorm.DB) (Provisioning, error) {
	var record Provisioning
	if err := db.First(&record, 1).Error; err != nil {
		return Provisioning{}, err
	}
	return record, nil
}

// ProvisioningStatus 汇总首次配置是否完成。
//
// 两个条件都满足才算完成：接入凭据已写入，且网关名合法。
// 只看凭据记录是不够的——库里的 gw_id 可能是空值或非法值，
// 那种状态下放行会让网关带着坏标识接入北向。
func ProvisioningStatus(db *gorm.DB) (ProvisioningState, error) {
	state := ProvisioningState{}

	settings, err := LoadSettings(db)
	switch {
	case err == nil:
		if gwID, gwErr := ValidateGWID(settings.App.Gateway.GWID); gwErr == nil {
			state.GWID = gwID
		}
	case errors.Is(err, gorm.ErrRecordNotFound):
		// 尚未初始化设置，网关名同样视为未配置。
	default:
		return ProvisioningState{}, err
	}

	record, err := LoadProvisioning(db)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return state, nil
		}
		return ProvisioningState{}, err
	}
	state.Configured = state.GWID != "" && record.Token != ""
	return state, nil
}

// SaveProvisioning 原子写入网关名与 Token。
//
// 两者写在同一个事务里：只落一半（有网关名没 Token）会让首次配置门
// 停在「填过了但始终不通过」的状态，而用户无法自行恢复。
func SaveProvisioning(db *gorm.DB, gwID, token string) error {
	normalizedGWID, err := ValidateGWID(gwID)
	if err != nil {
		return err
	}
	normalizedToken, err := ValidateToken(token)
	if err != nil {
		return err
	}

	return db.Transaction(func(tx *gorm.DB) error {
		settings, err := LoadSettings(tx)
		if err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			settings = DefaultSettings()
		}
		settings.App.Gateway.GWID = normalizedGWID
		if err := SaveSettings(tx, settings); err != nil {
			return err
		}
		return tx.Save(&Provisioning{
			ID:            1,
			Token:         normalizedToken,
			ProvisionedAt: time.Now(),
		}).Error
	})
}
