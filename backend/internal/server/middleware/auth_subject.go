package middleware

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// AuthSubject 鉴权后挂到 gin context 的最小主体信息（供网关热路径使用，不带完整 User）。
// Decision: {UserID int64, Concurrency int}
type AuthSubject struct {
	UserID      int64
	Concurrency int
	// PlatformConcurrency 用户 × 平台 专属并发上限（只含设了专属值的平台）。
	// 命中的平台用专属值替代 Concurrency（不叠加）；未命中沿用 Concurrency 的共享槽位。
	PlatformConcurrency map[string]int
}

// authSubjectFromUser 从鉴权得到的 User 构造 AuthSubject（含平台专属并发）。
func authSubjectFromUser(u *service.User) AuthSubject {
	subject := AuthSubject{UserID: u.ID, Concurrency: u.Concurrency}
	for platform, l := range u.PlatformLimits {
		if l.Concurrency == nil {
			continue
		}
		if subject.PlatformConcurrency == nil {
			subject.PlatformConcurrency = make(map[string]int, len(u.PlatformLimits))
		}
		subject.PlatformConcurrency[platform] = *l.Concurrency
	}
	return subject
}

func GetAuthSubjectFromContext(c *gin.Context) (AuthSubject, bool) {
	value, exists := c.Get(string(ContextKeyUser))
	if !exists {
		return AuthSubject{}, false
	}
	subject, ok := value.(AuthSubject)
	return subject, ok
}

// RequestPlatform 本请求的平台口径：ForcePlatform（/antigravity/*、cell 回源头）优先，
// 否则取 API Key 所属分组的平台；无分组 = ""（调用方按 anthropic 兜底或视为不分平台）。
// 与 service.QuotaPlatform 同口径，供中间件层无 ctx 时使用。
func RequestPlatform(c *gin.Context) string {
	if p, ok := GetForcePlatformFromContext(c); ok && p != "" {
		return p
	}
	if apiKey, ok := GetAPIKeyFromContext(c); ok && apiKey != nil && apiKey.Group != nil {
		return apiKey.Group.Platform
	}
	return ""
}

// ResolveUserSlot 解析本请求应占用的用户并发槽位：
//   - 该平台设了专属并发 → 用平台槽位（platform 非空，上限 = 专属值）；
//   - 否则 → 用户共享槽位（platform = ""，上限 = 全局 Concurrency）。
//
// 所有网关入口（本地 handler 与中央转发）统一走这里，保证同一用户同一平台只有一种口径。
func ResolveUserSlot(c *gin.Context) (userID int64, platform string, maxConcurrency int) {
	subject, ok := GetAuthSubjectFromContext(c)
	if !ok {
		return 0, "", 0
	}
	if p := RequestPlatform(c); p != "" {
		if limit, hit := subject.PlatformConcurrency[p]; hit {
			return subject.UserID, p, limit
		}
	}
	return subject.UserID, "", subject.Concurrency
}

func GetUserRoleFromContext(c *gin.Context) (string, bool) {
	value, exists := c.Get(string(ContextKeyUserRole))
	if !exists {
		return "", false
	}
	role, ok := value.(string)
	return role, ok
}
