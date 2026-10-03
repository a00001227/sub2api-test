package middleware

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// responsesAllowedSubpath 是 /responses 下唯一支持的子端点(Codex 上下文压缩 /responses/compact)。
const responsesAllowedSubpath = "/compact"

// ResponsesSubpathGuard 收口 /responses/*subpath 通配路由:子路径只放行 /compact,其它一律 404。
//
// 背景:上游开源项目为了接 /responses/compact 把路由开成通配,后缀原样拼到上游 URL 发出去。
// 2026-10-03 有人拿 `/v1/responses/..%2f..%2faccounts/check` 借我们的号打 OpenAI 内部接口,
// 上游拒绝被当成"号坏了"连锁临时下线,整个号池被扫空。能打哪些路径必须由我们自己定义:
// 主路径(/responses)和 /compact 之外没有可变部分。
//
// 挂在鉴权之后(日志里能定位是谁在扫)、enforcement/审核/edgeForward 之前(不审核、不转发、
// 不选号、不记上游错误)。只对带 subpath 参数的通配路由生效,其它路由为 no-op。
// 拒绝标为客户端侧 business_limited,不进 SLA/健康分。
func ResponsesSubpathGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		sub := c.Param("subpath")
		if sub == "" {
			c.Next()
			return
		}
		normalized := strings.TrimRight(strings.TrimSpace(sub), "/")
		if normalized == "" || normalized == responsesAllowedSubpath {
			c.Next()
			return
		}

		var userID, apiKeyID int64
		openAIStyle := false
		if apiKey, ok := GetAPIKeyFromContext(c); ok && apiKey != nil {
			userID, apiKeyID = apiKey.UserID, apiKey.ID
			openAIStyle = apiKey.Group != nil && apiKey.Group.Platform == service.PlatformOpenAI
		}
		slog.Warn("responses_subpath_rejected",
			"path", c.Request.URL.EscapedPath(),
			"subpath", sub,
			"user_id", userID,
			"api_key_id", apiKeyID,
			"client_ip", c.ClientIP(),
		)
		service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalPolicyDenied)
		writeClientRequestError(c, http.StatusNotFound, "not_found_error",
			"unsupported path under /responses; only /responses and /responses/compact are available", openAIStyle)
	}
}
