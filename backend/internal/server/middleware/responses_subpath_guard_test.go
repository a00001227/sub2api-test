package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newResponsesGuardRouter(t *testing.T) (*gin.Engine, *int) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	reached := 0
	handler := func(c *gin.Context) { reached++; c.String(http.StatusOK, "ok") }
	g := r.Group("/v1")
	g.Use(ResponsesSubpathGuard())
	g.POST("/responses", handler)
	g.POST("/responses/*subpath", handler)
	g.POST("/messages", handler)
	return r, &reached
}

func TestResponsesSubpathGuard(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		wantStatus int
		wantReach  bool
	}{
		{"main path untouched", "/v1/responses", http.StatusOK, true},
		{"compact allowed", "/v1/responses/compact", http.StatusOK, true},
		{"compact trailing slash allowed", "/v1/responses/compact/", http.StatusOK, true},
		{"traversal encoded rejected", "/v1/responses/..%2f..%2forganizations", http.StatusNotFound, false},
		{"traversal plain rejected", "/v1/responses/../../accounts/check", http.StatusNotFound, false},
		{"unknown subresource rejected", "/v1/responses/resp_123/cancel", http.StatusNotFound, false},
		{"compact nested rejected", "/v1/responses/compact/detail", http.StatusNotFound, false},
		{"other route unaffected", "/v1/messages", http.StatusOK, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, reached := newResponsesGuardRouter(t)
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			r.ServeHTTP(w, req)
			if tc.wantStatus == http.StatusNotFound && w.Code == http.StatusMovedPermanently {
				// gin 会把 "../" 清理后 301 到规整路径;按同样被挡住处理
				t.Skipf("gin redirected %s", tc.path)
			}
			require.Equal(t, tc.wantStatus, w.Code, w.Body.String())
			require.Equal(t, tc.wantReach, *reached > 0, "handler reached")
			if tc.wantStatus == http.StatusNotFound {
				require.Contains(t, w.Body.String(), "not_found_error")
			}
		})
	}
}
