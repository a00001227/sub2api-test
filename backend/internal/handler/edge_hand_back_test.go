package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newHandBackCtx(t *testing.T, edgeTrusted bool) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if edgeTrusted {
		c.Set(string(middleware2.ContextKeyEdgeTrusted), true)
	}
	return c, w
}

var handBack429Body = []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your rate limit"}}`)

// 边缘 cell 上换号耗尽(上游 429)且未写字节 → 503 "No available accounts",中央据此换 cell。
func TestHandleFailoverExhausted_EdgeHandsBack429AsNoAvailable(t *testing.T) {
	c, w := newHandBackCtx(t, true)
	h := &GatewayHandler{}
	h.handleFailoverExhausted(c, &service.UpstreamFailoverError{StatusCode: 429, ResponseBody: handBack429Body}, "anthropic", false)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.True(t, strings.Contains(strings.ToLower(w.Body.String()), "no available accounts"), w.Body.String())
	require.True(t, isOpsRoutingCapacityLimited(c), "should be excluded from SLA as routing capacity")
}

// 非中央转发流量(直连/中央本地执行)保持原映射:429 → 429。
func TestHandleFailoverExhausted_NonEdgeKeepsMapping(t *testing.T) {
	c, w := newHandBackCtx(t, false)
	h := &GatewayHandler{}
	h.handleFailoverExhausted(c, &service.UpstreamFailoverError{StatusCode: 429, ResponseBody: handBack429Body}, "anthropic", false)
	require.Equal(t, http.StatusTooManyRequests, w.Code)
}

// 已给客户端写过字节(流已开始)不能交还 —— 保持原有 SSE 错误帧收尾。
func TestEdgeShouldHandBack_SkipsWhenWrittenOrNonAccountError(t *testing.T) {
	c, _ := newHandBackCtx(t, true)
	require.True(t, edgeShouldHandBack(c, 429, false))
	require.True(t, edgeShouldHandBack(c, 529, false))
	require.True(t, edgeShouldHandBack(c, 502, false))
	require.False(t, edgeShouldHandBack(c, 400, false), "400 is request-side, must not hand back")
	require.False(t, edgeShouldHandBack(c, 413, false))
	require.False(t, edgeShouldHandBack(c, 429, true), "stream started")
	c.Writer.WriteHeader(http.StatusOK)
	_, _ = c.Writer.Write([]byte(": ping\n\n"))
	require.False(t, edgeShouldHandBack(c, 429, false), "bytes already written")
}

func TestOpenAIHandleFailoverExhausted_EdgeHandsBack(t *testing.T) {
	c, w := newHandBackCtx(t, true)
	h := &OpenAIGatewayHandler{}
	h.handleFailoverExhausted(c, &service.UpstreamFailoverError{StatusCode: 502, ResponseBody: []byte(`{"error":{"message":"Our servers are currently overloaded"}}`)}, false)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Contains(t, strings.ToLower(w.Body.String()), "no available accounts")
}
