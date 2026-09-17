package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 404「模型不存在」("model: gpt-xxx":key 绑在 Claude 组、请求 GPT 模型却没带 GPT 组 slug 前缀):
// 归 request 相位 / owner=client、排除出 SLA,严重度 P3;即使带着上游错误上下文也不抬成 upstream。
func TestClassifyOpsErrorLog_Upstream404ModelNotFoundIsClientSide(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set(service.OpsUpstreamStatusCodeKey, http.StatusNotFound)
	c.Set(service.OpsUpstreamErrorMessageKey, "model: gpt-5.6-sol")

	// 转发路径此前统一标 server_error → 归一成 api_error;现在按状态标 not_found_error。两种都要被 404 规则盖住。
	for _, errType := range []string{"not_found_error", "api_error"} {
		phase, limited, owner, source := classifyOpsErrorLog(c, errType, "model: gpt-5.6-sol", "", http.StatusNotFound)
		require.Equal(t, "request", phase, errType)
		require.True(t, limited, errType)
		require.Equal(t, "client", owner, errType)
		require.Equal(t, "client_request", source, errType)
	}
	require.Equal(t, "P3", classifyOpsSeverity("not_found_error", http.StatusNotFound))
	require.Equal(t, "P3", classifyOpsSeverity("api_error", http.StatusNotFound))

	// OpenAI 形状的模型不存在文案同样命中
	require.True(t, isOpsModelNotFoundMessage("the model 'gpt-9' does not exist"))
	// 非模型类 404(provider 端点不支持某功能)不命中,维持既有 SLA 口径
	require.False(t, isOpsModelNotFoundMessage("token counting is not supported for this platform"))
	require.False(t, isOpsModelNotFoundMessage("images api is not supported for this platform"))
}

// 对照:上游 5xx 不受 404 规则影响,仍是 upstream 相位、计入 SLA、P1。
func TestClassifyOpsErrorLog_Upstream502Unchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set(service.OpsUpstreamStatusCodeKey, http.StatusBadGateway)
	c.Set(service.OpsUpstreamErrorMessageKey, "Upstream request failed")

	phase, limited, owner, _ := classifyOpsErrorLog(c, "upstream_error", "Upstream request failed", "", http.StatusBadGateway)
	require.Equal(t, "upstream", phase)
	require.False(t, limited)
	require.Equal(t, "provider", owner)
	require.Equal(t, "P1", classifyOpsSeverity("upstream_error", http.StatusBadGateway))
}
