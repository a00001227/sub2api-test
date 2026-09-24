package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 客户端已断开(ctx 取消)导致的 "context canceled" 传输错:记 499 客户端侧,不再写成 502。
func TestHandleAnthropicUpstreamTransportError_ClientCanceledIs499(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
	upstreamReq, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	acc := &Account{ID: 77, Platform: PlatformAnthropic}
	err := handleAnthropicUpstreamTransportError(c, acc, upstreamReq, context.Canceled, false, nil)
	require.Error(t, err)
	require.Equal(t, 499, w.Code)
	require.Contains(t, w.Body.String(), "Client closed request")
	require.True(t, HasOpsClientBusinessLimited(c))
}
