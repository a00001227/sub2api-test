package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// streamCellResponse 必须:把正常 SSE 事件透传给客户端,但剥掉末尾的 sub2api_usage
// 事件(不泄漏给消费者)并捕获其 usage(#86b)。
func TestStreamCellResponse_StripsUsageSentinel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := "event: message_start\n" +
		"data: {\"type\":\"message_start\"}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n" +
		"event: sub2api_usage\n" +
		"data: {\"model\":\"claude-sonnet-4-6\",\"usage\":{\"input_tokens\":10,\"output_tokens\":5},\"stream\":true}\n\n"

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	env := streamCellResponse(c, resp)

	out := w.Body.String()
	if !strings.Contains(out, "message_start") || !strings.Contains(out, "message_stop") {
		t.Fatalf("normal events must pass through, got: %q", out)
	}
	if strings.Contains(out, "sub2api_usage") || strings.Contains(out, "input_tokens") {
		t.Fatalf("usage sentinel must be stripped from client stream, got: %q", out)
	}
	if env == nil {
		t.Fatal("expected captured usage envelope, got nil")
	}
	if env.Model != "claude-sonnet-4-6" || env.Usage.InputTokens != 10 || env.Usage.OutputTokens != 5 {
		t.Errorf("captured envelope mismatch: %+v", env)
	}
}

// 非 SSE(非流式)响应:body 原样透传;用量从 X-Sub2api-Usage 头捕获、头不透传。
func TestStreamCellResponse_NonStreamHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":    []string{"application/json"},
			"X-Sub2api-Usage": []string{`{"model":"claude-opus-4-8","usage":{"input_tokens":7,"output_tokens":3},"stream":false}`},
		},
		Body: io.NopCloser(strings.NewReader(`{"type":"message","usage":{"input_tokens":7}}`)),
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	env := streamCellResponse(c, resp)

	if w.Header().Get("X-Sub2api-Usage") != "" {
		t.Error("usage header must not be forwarded to client")
	}
	if !strings.Contains(w.Body.String(), "message") {
		t.Errorf("json body must pass through, got: %q", w.Body.String())
	}
	if env == nil || env.Model != "claude-opus-4-8" || env.Usage.InputTokens != 7 {
		t.Errorf("expected captured header envelope, got: %+v", env)
	}
}
