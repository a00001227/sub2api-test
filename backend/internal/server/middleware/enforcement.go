package middleware

import (
	"bytes"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// enforcementMaxModelPeek 读取 body 取模型名的上限（仅 HIGH 用户且配置了受限模型时才读）。
const enforcementMaxModelPeek = 1 << 20 // 1 MiB，超出不再取模型（bodyLimit 已在前面兜底）

// Enforcement 蒸馏执行层限速/禁用中间件：
//   - 用户级：对 risk_v2 判定为 HIGH 且未豁免的用户施加独立低 RPM 上限；
//   - 模型级：对 HIGH 用户请求「受限模型」时按规则 block（拒绝）或 throttle（该模型独立低 RPM）。
//
// 挂在 apiKeyAuth/requireGroup 之后、edgeForward 之前 → 本地与转发两条路径统一覆盖、转发前即拦。
// master 关或无 HIGH 用户时 Active()=false → 一次原子读即放行（零开销）。
// 仅当用户命中 HIGH 且配置了受限模型时才读 body 取模型名（读后还原，不影响下游）。
// cell 回源可信流量（IsEdgeTrusted）跳过。
func Enforcement(svc *service.EnforcementService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if svc == nil || !svc.Active() {
			c.Next()
			return
		}
		if IsEdgeTrusted(c) {
			c.Next()
			return
		}
		apiKey, ok := GetAPIKeyFromContext(c)
		if !ok || apiKey == nil || apiKey.UserID <= 0 {
			c.Next()
			return
		}
		userID := apiKey.UserID
		if !svc.ShouldThrottle(userID) { // 非 HIGH / 已豁免 / 未启用 → 零 I/O 放行
			c.Next()
			return
		}

		// 模型级：仅当配置了受限模型时才读 body 取模型名。
		if svc.HasModelRules() {
			if model := peekModel(c); model != "" {
				if action, ok := svc.ModelAction(model); ok {
					switch action {
					case service.EnforcementActionBlock:
						abortModelBlocked(c, model)
						return
					case service.EnforcementActionThrottle:
						if throttled, retryAfter := svc.ThrottledModel(c.Request.Context(), userID, model); throttled {
							abortThrottled(c, retryAfter)
							return
						}
						c.Next() // 受限模型的 throttle 规则接管本请求（未超限 → 放行）
						return
					}
				}
			}
		}

		// 用户级兜底限速。
		if throttled, retryAfter := svc.Throttled(c.Request.Context(), userID); throttled {
			abortThrottled(c, retryAfter)
			return
		}
		c.Next()
	}
}

// peekModel 读取请求 body 的 model 字段（读后还原 body，供 edgeForward/handler 复用）。
func peekModel(c *gin.Context) string {
	if c.Request == nil || c.Request.Body == nil {
		return ""
	}
	if c.Request.ContentLength > enforcementMaxModelPeek {
		return ""
	}
	buf, err := io.ReadAll(io.LimitReader(c.Request.Body, enforcementMaxModelPeek))
	// 无论成功与否都还原 body（已消费的字节放回）。
	c.Request.Body = io.NopCloser(bytes.NewReader(buf))
	if err != nil || len(buf) == 0 {
		return ""
	}
	return gjson.GetBytes(buf, "model").String()
}

func abortThrottled(c *gin.Context, retryAfter int) {
	if retryAfter > 0 {
		c.Header("Retry-After", strconv.Itoa(retryAfter))
	}
	c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
		"type": "error",
		"error": gin.H{
			"type":    "rate_limit_exceeded",
			"code":    "RISK_THROTTLED",
			"message": "request rate limited due to elevated model-extraction risk",
		},
	})
}

func abortModelBlocked(c *gin.Context, model string) {
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
		"type": "error",
		"error": gin.H{
			"type":    "permission_error",
			"code":    "RISK_MODEL_BLOCKED",
			"message": "model '" + model + "' is not available due to elevated model-extraction risk",
		},
	})
}
