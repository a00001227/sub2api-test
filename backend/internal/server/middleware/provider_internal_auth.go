package middleware

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Phase 21E-6C-2B-1: Provider Portal 内部调用鉴权。
//
// 独立于 AdminAuth —— Portal 是机器调用方而非管理员会话：独立 secret
// （PROVIDER_INTERNAL_TOKEN 环境配置）、Bearer 形态、常数时间比较、
// 统一 401 错误体（不区分「缺 token / 错 token / 未配置」，避免探测）。
// secret 未配置时整个内部面关闭（fail-closed）。
type ProviderInternalAuthMiddleware gin.HandlerFunc

// NewProviderInternalAuth builds the middleware from the configured token.
func NewProviderInternalAuth(token string) ProviderInternalAuthMiddleware {
	expected := strings.TrimSpace(token)
	unauthorized := func(c *gin.Context) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"code":    "PROVIDER_INTERNAL_UNAUTHORIZED",
			"message": "unauthorized",
		})
	}
	return func(c *gin.Context) {
		// fail-closed：未配置 secret 时内部面不可用
		if expected == "" {
			unauthorized(c)
			return
		}
		header := c.GetHeader("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(header, prefix) {
			unauthorized(c)
			return
		}
		got := strings.TrimSpace(strings.TrimPrefix(header, prefix))
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
			unauthorized(c)
			return
		}
		c.Next()
	}
}
