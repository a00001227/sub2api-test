package middleware

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// ContentModerationDoneContextKey：前置审核中间件审完后置于 gin context;下游 handler
// 据此跳过重复审核（本地路径 = 中间件已审，handler 不再审）。
const ContentModerationDoneContextKey = "_content_moderation_done"

// ctxKeyInboundEndpointLiteral 与 handler.ctxKeyInboundEndpoint 同值——中间件在 handler
// 之前运行且不能 import handler（会成环），故直读该 context key 拿归一化端点。
const ctxKeyInboundEndpointLiteral = "_gateway_inbound_endpoint"

// ContentModeration 前置内容审计中间件：挂在 apiKeyAuth/requireGroup/enforcement 之后、
// edgeForward 之前 —— 此前审核只在网关 handler 内执行，而 EdgeForward 命中转发时会短路
// 不进 handler，导致 cell 流量绕过审核。此中间件让转发路径也统一覆盖、转发前即拦。
//
//   - Active()=false（未启用）→ 零开销放行，不读 body。
//   - IsEdgeTrusted（cell 回源可信流量）→ 跳过（中央已审，避免 cell 重复审）。
//   - 命中（decision.Blocked）→ 按协议格式直接拒绝，绝不转发到 cell。
//   - 放行 → 置 done 标记，下游 handler 不再重复审核。
func ContentModeration(svc *service.ContentModerationService) gin.HandlerFunc {
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
		if protocol == "" { // 无可审文本的端点（如 /embeddings）→ 放行
			c.Next()
			return
		}

		apiKey, _ := GetAPIKeyFromContext(c)
		body := readAndRestoreModerationBody(c)
		input := buildModerationInput(c, apiKey, protocol, body)

		decision, err := svc.Check(c.Request.Context(), input)
		if err != nil || decision == nil {
			// 审核出错不阻断请求（服务内部按策略放行/记录）;标记已审，避免下游重复调用。
			c.Set(ContentModerationDoneContextKey, true)
			c.Next()
			return
		}
		if decision.Blocked {
			// 内容审核主动拦截(违规词等)是中转的策略行为,非系统故障 → 标记业务限制,
			// 使其排除出 SLA/健康分(仍留在错误列表可见)。协议无关地在此统一打标,
			// 避免依赖各协议拦截响应体格式(anthropic 走 permission_error、无 code)。
			service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonContentPolicy)
			writeModerationBlock(c, protocol, decision)
			c.Abort()
			return
		}
		c.Set(ContentModerationDoneContextKey, true)
		c.Next()
	}
}

// readAndRestoreModerationBody 读取整个请求体并原样还原（供 edgeForward/handler 复用）。
// bodyLimit 已在更上游封顶，此处全量读取安全；读后用内存 reader 还原，不截断。
func readAndRestoreModerationBody(c *gin.Context) []byte {
	if c.Request == nil || c.Request.Body == nil {
		return nil
	}
	buf, err := io.ReadAll(c.Request.Body)
	_ = c.Request.Body.Close()
	c.Request.Body = io.NopCloser(bytes.NewReader(buf))
	if err != nil {
		return nil
	}
	return buf
}

// moderationProtocolForPath 按请求路径映射审核协议;无可审文本的端点返回空（跳过）。
func moderationProtocolForPath(fullPath, rawPath string) string {
	p := fullPath
	if p == "" {
		p = rawPath
	}
	switch {
	case strings.Contains(p, "/chat/completions"):
		return service.ContentModerationProtocolOpenAIChat
	case strings.Contains(p, "/responses"):
		return service.ContentModerationProtocolOpenAIResponses
	case strings.Contains(p, "/images/"):
		return service.ContentModerationProtocolOpenAIImages
	case strings.Contains(p, "/messages"): // 含 /v1/messages 与 /v1/messages/count_tokens
		return service.ContentModerationProtocolAnthropicMessages
	case strings.Contains(p, "/v1beta/"):
		return service.ContentModerationProtocolGemini
	default: // /embeddings 等 → 无消息文本可审
		return ""
	}
}

// buildModerationInput 用鉴权上下文 + 请求体拼审核输入（不依赖 handler 包，避免 import 成环）。
func buildModerationInput(c *gin.Context, apiKey *service.APIKey, protocol string, body []byte) service.ContentModerationCheckInput {
	input := service.ContentModerationCheckInput{
		Endpoint: c.GetString(ctxKeyInboundEndpointLiteral),
		Model:    strings.TrimSpace(gjson.GetBytes(body, "model").String()),
		Protocol: protocol,
		Body:     body,
	}
	if input.Endpoint == "" && c.Request != nil && c.Request.URL != nil {
		input.Endpoint = c.Request.URL.Path
	}
	// 写入 request_id（与提示词审计同源 ctxkey.ClientRequestID）→ 供提示词审计「结果」列读时
	// 按 request_id 关联本条风控日志。不设则恒为空串，两表无法关联（历史 bug）。
	if c.Request != nil {
		if rid, _ := c.Request.Context().Value(ctxkey.ClientRequestID).(string); rid != "" {
			input.RequestID = strings.TrimSpace(rid)
		}
	}
	if forced, ok := GetForcePlatformFromContext(c); ok {
		input.Provider = strings.TrimSpace(forced)
	}
	if apiKey != nil {
		input.UserID = apiKey.UserID
		input.APIKeyID = apiKey.ID
		input.APIKeyName = apiKey.Name
		if apiKey.User != nil {
			input.UserEmail = apiKey.User.Email
		}
		if apiKey.GroupID != nil {
			g := *apiKey.GroupID
			input.GroupID = &g
		}
		if apiKey.Group != nil {
			input.GroupName = apiKey.Group.Name
			if input.Provider == "" {
				input.Provider = strings.TrimSpace(apiKey.Group.Platform)
			}
		}
	}
	return input
}

// writeModerationBlock 按协议格式输出拦截错误（用 decision 的状态码/文案）。
func writeModerationBlock(c *gin.Context, protocol string, decision *service.ContentModerationDecision) {
	status := decision.StatusCode
	if status < 400 || status > 599 {
		status = http.StatusForbidden
	}
	msg := strings.TrimSpace(decision.Message)
	if msg == "" {
		msg = "request blocked by content policy"
	}
	switch protocol {
	case service.ContentModerationProtocolOpenAIChat,
		service.ContentModerationProtocolOpenAIResponses,
		service.ContentModerationProtocolOpenAIImages:
		c.JSON(status, gin.H{"error": gin.H{
			"message": msg,
			"type":    "content_policy_violation",
			"code":    "content_policy_violation",
		}})
	case service.ContentModerationProtocolGemini:
		GoogleErrorWriter(c, status, msg)
	default: // anthropic_messages
		AnthropicErrorWriter(c, status, msg)
	}
}
