//go:build unit

package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// streaming_error_frame_dump_test.go —— 只为“抓字节”:把 handleStreamingAwareError
// 在 streamStarted=true(流已开始,发过 ping)时写给客户端的原始 SSE 帧原样打出来,
// 并和真·Anthropic 中途出错的标准帧做对比。用于诊断“客户端一直 api_error / 断线重连”。
//
// 真·Anthropic 中途出错的帧(文档 + 抓包一致):
//
//	event: error
//	data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}
//
// 关键是有 `event: error` 那一行。跑:
//
//	go test -tags unit -run TestDumpStreamingErrorFrame -v ./internal/handler/
func TestDumpStreamingErrorFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 覆盖用户遇到的三种触发,全部走 streamStarted=true 分支。
	cases := []struct {
		name    string
		status  int
		errType string
		message string
	}{
		{"no_available_accounts_503", http.StatusServiceUnavailable, "api_error", "No available accounts"},
		{"upstream_403_to_502", http.StatusBadGateway, "upstream_error", "Upstream access forbidden, please contact administrator"},
		{"upstream_rate_limit_429", http.StatusTooManyRequests, "rate_limit_error", "Upstream rate limit exceeded, please retry later"},
	}

	h := &GatewayHandler{}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

			h.handleStreamingAwareError(c, tc.status, tc.errType, tc.message, true /* streamStarted */)

			body := rec.Body.String()
			// %q 把 \n 显式打出来,一眼看清有没有 `event: error` 行、行尾是不是 \n\n。
			t.Logf("\n──[ %s ]── HTTP状态=%d\n我们发出的原始字节: %q\n可读形式:\n%s",
				tc.name, rec.Code, body, body)

			// 对照真·Anthropic:错误帧必须带 `event: error` 行,客户端 SDK 才认得是终止事件。
			if !hasEventErrorLine(body) {
				t.Errorf("❌ 缺少 `event: error` 行 —— 客户端会把这帧当成断流而不是错误终止,导致断线重连/死循环。\n"+
					"    我们发的:   %q\n"+
					"    Anthropic:  %q",
					body,
					"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n")
			}
		})
	}
}

// hasEventErrorLine 报告 SSE 文本里是否存在一行恰好是 `event: error`。
func hasEventErrorLine(sse string) bool {
	for _, line := range splitLines(sse) {
		if line == "event: error" {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			if n := len(line); n > 0 && line[n-1] == '\r' {
				line = line[:n-1]
			}
			out = append(out, line)
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
