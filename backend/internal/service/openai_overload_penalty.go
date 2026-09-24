package service

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
)

// OpenAI 过载的账号侧处置对齐 Claude。
//
// Anthropic 过载有专属状态码 529,账号侧靠它命中「529 | overloaded」临时不可调度规则或过载
// 冷却(handle529)。OpenAI/ChatGPT 的过载("Our servers are currently overloaded")没有 529:
// 要么是 HTTP 200 流的终态 response.failed,要么是 502/503 的响应体文案 —— 按真实状态码走
// 账号处置时,529 规则永远匹配不上、5xx 只记一条 warn,于是过载号不摘、下一请求立刻又选中它。
// 这里把「过载文案」在**账号处置层**归一成 529(不改回给客户端的状态码),使 Portal 上与 Claude
// 同一套规则(529 过载 2min / 429 限流 10min / 503 维护 2min)对 OpenAI 号同样生效;未配规则时
// 退回全局过载冷却。
const openAIOverloadEffectiveStatus = 529

// openAIEffectiveUpstreamStatus 返回用于账号侧处置的状态码:200 流终态 / 5xx 且文案为过载 → 529。
func openAIEffectiveUpstreamStatus(status int, body []byte) int {
	if status == http.StatusOK || status >= 500 {
		if ClassifyUpstreamCause(status, string(body)) == UpstreamCauseOverloaded {
			return openAIOverloadEffectiveStatus
		}
	}
	return status
}

// penalizeOpenAIStreamFailure 对 200 流终态 response.failed 做账号侧处置(目前只认过载)。
// 用 gin 请求 ctx(内部会 WithoutCancel),客户端已断开也照样落库。
func (s *OpenAIGatewayService) penalizeOpenAIStreamFailure(c *gin.Context, account *Account, payload []byte, message string) {
	if s == nil || account == nil {
		return
	}
	body := payload
	if len(body) == 0 {
		body = []byte(message)
	}
	if openAIEffectiveUpstreamStatus(http.StatusOK, body) != openAIOverloadEffectiveStatus {
		return
	}
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	_ = s.handleOpenAIAccountUpstreamError(ctx, account, openAIOverloadEffectiveStatus, nil, body)
}
