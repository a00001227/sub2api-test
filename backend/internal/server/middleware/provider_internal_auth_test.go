package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Phase 21E-6C-2B-1: ProviderInternalAuth 中间件测试。

func newInternalAuthEngine(token string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(gin.HandlerFunc(NewProviderInternalAuth(token)))
	r.POST("/probe", func(c *gin.Context) { c.Status(http.StatusOK) })
	return r
}

func doAuth(r *gin.Engine, header string) int {
	req := httptest.NewRequest(http.MethodPost, "/probe", nil)
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

func TestInternalAuth_ValidToken(t *testing.T) {
	r := newInternalAuthEngine("secret-xyz")
	require.Equal(t, http.StatusOK, doAuth(r, "Bearer secret-xyz"))
}

func TestInternalAuth_WrongToken(t *testing.T) {
	r := newInternalAuthEngine("secret-xyz")
	require.Equal(t, http.StatusUnauthorized, doAuth(r, "Bearer wrong"))
}

func TestInternalAuth_MissingHeader(t *testing.T) {
	r := newInternalAuthEngine("secret-xyz")
	require.Equal(t, http.StatusUnauthorized, doAuth(r, ""))
}

func TestInternalAuth_NonBearer(t *testing.T) {
	r := newInternalAuthEngine("secret-xyz")
	require.Equal(t, http.StatusUnauthorized, doAuth(r, "secret-xyz"))
}

func TestInternalAuth_FailClosedWhenUnconfigured(t *testing.T) {
	// secret 未配置：即使请求带任意 token 也必须 401（内部面默认关闭）。
	r := newInternalAuthEngine("")
	require.Equal(t, http.StatusUnauthorized, doAuth(r, "Bearer anything"))
	require.Equal(t, http.StatusUnauthorized, doAuth(r, ""))
}
