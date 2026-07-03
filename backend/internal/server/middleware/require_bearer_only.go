package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// RequireBearerOnly 前置中间件：只接受 Authorization: Bearer <token>，
// 拒绝 x-api-key、x-goog-api-key 及 query 参数中的 key。
// 本中间件只检查输入方式，不验证 token 有效性（由后续 APIKeyAuth 负责）。
func RequireBearerOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 拒绝 query 参数 key（与 APIKeyAuth 保持一致，返回 400）
		if c.Query("key") != "" || c.Query("api_key") != "" {
			AbortWithError(c, 400, "API_KEY_IN_QUERY_DEPRECATED", "API key in query parameter is not supported on this endpoint")
			return
		}

		// 拒绝 x-api-key / x-goog-api-key
		if c.GetHeader("x-api-key") != "" || c.GetHeader("x-goog-api-key") != "" {
			AbortWithError(c, 401, "INVALID_AUTHORIZATION_HEADER", "This endpoint only accepts Authorization: Bearer <key>")
			return
		}

		// 必须有 Authorization header
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			AbortWithError(c, 401, "API_KEY_REQUIRED", "API key is required: Authorization: Bearer <key>")
			return
		}

		// 必须是 Bearer 格式
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			AbortWithError(c, 401, "INVALID_AUTHORIZATION_HEADER", "Authorization header must use Bearer scheme")
			return
		}

		// Bearer token 不能为空
		if strings.TrimSpace(parts[1]) == "" {
			AbortWithError(c, 401, "API_KEY_REQUIRED", "Bearer token must not be empty")
			return
		}

		c.Next()
	}
}
