package handler

import (
	"fmt"
	"log/slog"
	"net/http"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// 边缘 cell「交还中央换 cell」:
//
// 号分散在各 cell,单台 cell 对某组/模型往往只有一两个号。上游回 429/529/5xx/401/403 后本地
// 换号耗尽(同 cell 选不到第二个号,或切换次数用完),以前会把最后一次的上游错误映射后直接回
// 客户端(如 "Upstream rate limit exceeded")—— 而系统整体号很多,别的 cell 明明能接。
// 中央 edge_forward 只在 cell 回 503「No available accounts」时才顺位换 cell,于是这里把
// 「本地候选耗尽 + 上游账号级错误 + 尚未给客户端写任何字节」统一改写成该 503,让中央继续换
// cell 重放同一请求(上游未产出任何内容,重放安全)。真实上游原因仍经 ops + 脱敏头留档。
// 仅对中央转发来的可信流量(IsEdgeTrusted)生效;直连/中央本地执行路径行为不变。
func edgeShouldHandBack(c *gin.Context, upstreamStatus int, streamStarted bool) bool {
	if c == nil || streamStarted {
		return false
	}
	if !middleware2.IsEdgeTrusted(c) {
		return false
	}
	if service.IsResponseCommitted(c) || c.Writer.Written() {
		return false
	}
	switch upstreamStatus {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, 529:
		return true
	default:
		return upstreamStatus >= 500
	}
}

// edgeHandBackNoAvailable 写出中央可识别的 503「No available accounts」,并把真实上游原因
// 记入 ops(标记为路由容量受限,排除出 SLA)与脱敏头。writeErr 由调用方按自身协议格式写响应。
func edgeHandBackNoAvailable(c *gin.Context, upstreamStatus int, upstreamMsg string, writeErr func(status int, errType, message string)) {
	service.SetOpsUpstreamError(c, upstreamStatus, upstreamMsg, "")
	service.SetEdgeUpstreamCauseHeader(c, false, upstreamStatus, upstreamMsg)
	markOpsRoutingCapacityLimited(c)
	msg := fmt.Sprintf("No available accounts: local candidates exhausted after upstream %d, hand back to central for another cell", upstreamStatus)
	slog.Info("edge_hand_back: 本地换号耗尽,交还中央换 cell", "upstream_status", upstreamStatus, "path", c.Request.URL.Path)
	writeErr(http.StatusServiceUnavailable, "api_error", msg)
}
