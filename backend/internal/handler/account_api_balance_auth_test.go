//go:build unit

package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 回归:客户密钥(sub key, ParentKeyID != nil)调 GET /balance 必须 403,
// 不能读到账号级余额。守卫在任何 service 调用之前返回,故用空 handler 即可。
func TestAccountAPIBalanceRejectsSubKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	parent := int64(1)
	h := &AccountAPIHandler{} // 守卫先行返回,不会触达 nil service

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/balance", nil)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{UserID: 7, ParentKeyID: &parent})

	h.Balance(c)

	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), "ACCOUNT_KEY_REQUIRED")
	require.True(t, c.IsAborted())
	// 绝不能泄露任何余额字段
	require.False(t, strings.Contains(w.Body.String(), "available_balance"))
}
