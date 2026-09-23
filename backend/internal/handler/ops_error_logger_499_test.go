package handler

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 499(客户端在响应前自己断开)必须归客户端侧并排除出 SLA/健康分。
func TestClassifyOpsErrorLog_499IsClientSideBusinessLimited(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)

	_, limited, owner, source := classifyOpsErrorLog(c, "api_error", "context canceled", "", 499)
	require.True(t, limited, "499 must be business-limited (excluded from SLA)")
	require.Equal(t, "client", owner)
	require.Equal(t, "client_request", source)

	// 对照:同样文案的 500 不受影响
	_, limited500, owner500, _ := classifyOpsErrorLog(c, "api_error", "context canceled", "", 500)
	require.False(t, limited500)
	require.NotEqual(t, "client", owner500)
}
