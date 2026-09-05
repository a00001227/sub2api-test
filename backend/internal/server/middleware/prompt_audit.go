package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// PromptAuditCapture 提示词审计捕获中间件：与内容审核完全解耦，仅负责“留存原文”。
//
//   - Active()=false（未启用）→ 零开销放行，不读 body。
//   - IsEdgeTrusted（cell 回源可信流量）→ 跳过（中央已捕获，避免 cell 重复留存）。
//   - 命中可留存端点 → 拷贝 body + 元数据后非阻塞入队，队列满即丢，绝不阻塞/影响请求。
//
// 复用内容审核中间件的路径→协议映射、body 读取还原、输入拼装等私有函数（同包），
// 因此不改动内容审核任何判定/拦截逻辑。
func PromptAuditCapture(svc *service.PromptAuditService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if svc == nil || c.Request == nil || !svc.Active(c.Request.Context()) {
			c.Next()
			return
		}
		if IsEdgeTrusted(c) {
			c.Next()
			return
		}
		protocol := moderationProtocolForPath(c.FullPath(), c.Request.URL.Path)
		if protocol == "" { // 无可留存文本的端点（如 /embeddings）→ 放行
			c.Next()
			return
		}

		apiKey, _ := GetAPIKeyFromContext(c)
		body := readAndRestoreModerationBody(c)
		if len(body) == 0 {
			c.Next()
			return
		}
		// 防御性拷贝：worker 异步读取，避免与下游 handler / edgeForward 共享同一底层切片。
		bodyCopy := make([]byte, len(body))
		copy(bodyCopy, body)

		input := buildModerationInput(c, apiKey, protocol, bodyCopy)

		requestID, _ := c.Request.Context().Value(ctxkey.ClientRequestID).(string)
		task := svc.BuildTask(
			strings.TrimSpace(requestID),
			int64PtrOrNil(input.UserID),
			input.UserEmail,
			int64PtrOrNil(input.APIKeyID),
			input.APIKeyName,
			input.GroupID,
			input.GroupName,
			input.Provider,
			input.Endpoint,
			input.Protocol,
			input.Model,
			bodyCopy,
		)
		svc.Capture(c.Request.Context(), task)
		c.Next()
	}
}

func int64PtrOrNil(v int64) *int64 {
	if v <= 0 {
		return nil
	}
	return &v
}
