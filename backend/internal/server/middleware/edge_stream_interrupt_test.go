package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// sseResp 构造一个 text/event-stream 的 *http.Response,body 为给定 SSE 文本。
func sseResp(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// 中央↔cell 流在中途断掉(没有 message_stop / 没有 sub2api_usage 用量哨兵就 EOF):
// streamCellResponse 必须补发一帧带 `event: error` 行的终止帧,并回报 interrupted=true。
// 这是修 Claude Code「api_error 死循环 / 只能新开客户端」的中央中继侧补丁。
func TestStreamCellResponse_TruncatedEmitsTerminalError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 流开了头(发过正文),但没到 message_stop、也没有用量哨兵就断了。
	body := "event: message_start\n" +
		"data: {\"type\":\"message_start\"}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hel\"}}\n\n"

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	_, interrupted := streamCellResponse(c, sseResp(body))

	if !interrupted {
		t.Fatal("截断流必须回报 interrupted=true(供上层驱逐会话亲和)")
	}
	out := w.Body.String()
	// 关键:必须带 `event: error` 行,客户端 SDK 才认作终止而非断流。
	if !strings.Contains(out, "event: error\n") {
		t.Fatalf("截断流必须补发带 `event: error` 行的终止帧,got: %q", out)
	}
	if !strings.Contains(out, `"type":"error"`) || !strings.Contains(out, "edge cell stream interrupted") {
		t.Fatalf("终止帧 data 载荷不符,got: %q", out)
	}
	// 正文仍应先透传出去。
	if !strings.Contains(out, "message_start") || !strings.Contains(out, "content_block_delta") {
		t.Fatalf("截断前的正文必须已透传,got: %q", out)
	}
}

// 正常收尾(有 message_stop + 用量哨兵):不得注入任何 event: error 帧,interrupted=false。
func TestStreamCellResponse_NormalNoInjectedError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := "event: message_start\n" +
		"data: {\"type\":\"message_start\"}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n" +
		"event: sub2api_usage\n" +
		"data: {\"model\":\"claude-sonnet-4-6\",\"usage\":{\"input_tokens\":10,\"output_tokens\":5},\"stream\":true}\n\n"

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	env, interrupted := streamCellResponse(c, sseResp(body))

	if interrupted {
		t.Fatal("正常收尾不得回报 interrupted")
	}
	out := w.Body.String()
	if strings.Contains(out, "event: error") {
		t.Fatalf("正常收尾不得注入 event: error 帧,got: %q", out)
	}
	if env == nil || env.Model != "claude-sonnet-4-6" {
		t.Fatalf("正常收尾应捕获用量,got: %+v", env)
	}
}

// cell 自己中途发了 event: error(它那侧已按 Anthropic 规范补好终止帧)后断流:
// 中央应原样透传该帧,且不再二次合成、不回报 interrupted(避免双终止帧 / 误驱逐亲和)。
func TestStreamCellResponse_CellErrorNotDoubleEmitted(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := "event: message_start\n" +
		"data: {\"type\":\"message_start\"}\n\n" +
		"event: error\n" +
		"data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	_, interrupted := streamCellResponse(c, sseResp(body))

	if interrupted {
		t.Fatal("cell 已自发 error 终止,中央不应再判为截断/驱逐亲和")
	}
	out := w.Body.String()
	if strings.Count(out, "event: error") != 1 {
		t.Fatalf("必须原样透传 cell 的单个 event: error,不得二次合成,got: %q", out)
	}
	// 应保留 cell 的原始错误类型,而非被换成中央的 upstream_error。
	if !strings.Contains(out, "overloaded_error") || strings.Contains(out, "edge cell stream interrupted") {
		t.Fatalf("应透传 cell 原始错误,不得覆盖,got: %q", out)
	}
}
