package service

import (
	"time"

	"golang.org/x/crypto/bcrypt"
)

type User struct {
	ID             int64
	Email          string
	Username       string
	Notes          string
	AvatarURL      string
	AvatarSource   string
	AvatarMIME     string
	AvatarByteSize int
	AvatarSHA256   string
	PasswordHash   string
	Role           string
	Lane           string // 工作道/号池:normal|batch|distillation(护号焊池)
	Balance        float64
	Concurrency    int
	Status         string
	AllowedGroups  []int64
	TokenVersion   int64 // Incremented on password change to invalidate existing tokens
	// TokenVersionResolved indicates TokenVersion already contains the fingerprint-derived
	// value expected in JWT claims and refresh-token state.
	TokenVersionResolved bool
	SignupSource         string
	LastLoginAt          *time.Time
	LastActiveAt         *time.Time
	LastUsedAt           *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
	DeletedAt            *time.Time // 非 nil 表示用户已软删除

	// GroupRates 用户专属分组倍率配置
	// map[groupID]rateMultiplier
	GroupRates map[int64]float64

	// TOTP 双因素认证字段
	TotpSecretEncrypted *string    // AES-256-GCM 加密的 TOTP 密钥
	TotpEnabled         bool       // 是否启用 TOTP
	TotpEnabledAt       *time.Time // TOTP 启用时间

	// 余额不足通知
	BalanceNotifyEnabled       bool
	BalanceNotifyThresholdType string // "fixed" (default) | "percentage"
	BalanceNotifyThreshold     *float64
	BalanceNotifyExtraEmails   []NotifyEmailEntry
	TotalRecharged             float64

	// RPMLimit 用户级每分钟请求数上限（0 = 不限制）。仅在所用分组未设置 rpm_limit
	// 且该 (用户, 分组) 无 rpm_override 时作为全局兜底生效，计数键 rpm:u:{userID}:{min}。
	RPMLimit int

	// UserGroupRPMOverride 来自 auth cache snapshot 的 (user, group) RPM 覆盖值。
	// nil = 该 API Key 对应的 (user, group) 无 override；非 nil 时 checkRPM 直接使用，
	// 避免每请求查 DB。字段不持久化到数据库。
	UserGroupRPMOverride *int

	// PlatformLimits 用户 × 平台 专属并发 / RPM 上限（来自 user_platform_quotas，经鉴权
	// 缓存快照带入，不持久化在 users 表）。key = 平台名（anthropic/openai/…）。
	// 只收录至少设了一项的平台；某平台缺失或字段为 nil = 沿用全局值（Concurrency / RPMLimit）。
	// 语义：专属值**替代**全局值，不叠加；0 = 该平台不限。
	PlatformLimits map[string]UserPlatformLimit

	APIKeys       []APIKey
	Subscriptions []UserSubscription
}

// UserPlatformLimit 某平台的专属并发 / RPM 上限。nil 字段 = 沿用全局值；0 = 不限；>0 = 专属上限。
type UserPlatformLimit struct {
	Concurrency *int
	RPMLimit    *int
}

// EffectiveConcurrency 返回 platform 上生效的并发上限，以及是否为平台专属值。
// 专属值替代全局值（不叠加）；platform 为空或未设专属值时回退到 u.Concurrency（scoped=false）。
func (u *User) EffectiveConcurrency(platform string) (limit int, scoped bool) {
	if u == nil {
		return 0, false
	}
	if platform != "" {
		if l, ok := u.PlatformLimits[platform]; ok && l.Concurrency != nil {
			return *l.Concurrency, true
		}
	}
	return u.Concurrency, false
}

// EffectiveRPMLimit 返回 platform 上生效的用户级 RPM 上限，以及是否为平台专属值。
// 语义同 EffectiveConcurrency。
func (u *User) EffectiveRPMLimit(platform string) (limit int, scoped bool) {
	if u == nil {
		return 0, false
	}
	if platform != "" {
		if l, ok := u.PlatformLimits[platform]; ok && l.RPMLimit != nil {
			return *l.RPMLimit, true
		}
	}
	return u.RPMLimit, false
}

func (u *User) IsAdmin() bool {
	return u.Role == RoleAdmin
}

func (u *User) IsActive() bool {
	return u.Status == StatusActive
}

// CanBindGroup checks whether a user can bind to a given group.
// For standard groups:
// - Public groups (non-exclusive): all users can bind
// - Exclusive groups: only users with the group in AllowedGroups can bind
func (u *User) CanBindGroup(groupID int64, isExclusive bool) bool {
	// 公开分组（非专属）：所有用户都可以绑定
	if !isExclusive {
		return true
	}
	// 专属分组：需要在 AllowedGroups 中
	for _, id := range u.AllowedGroups {
		if id == groupID {
			return true
		}
	}
	return false
}

func (u *User) SetPassword(password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	u.PasswordHash = string(hash)
	return nil
}

func (u *User) CheckPassword(password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) == nil
}
