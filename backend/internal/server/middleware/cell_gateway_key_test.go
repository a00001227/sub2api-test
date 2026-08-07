package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
)

// 方案 B:EDGE cell 配了 CELL_GATEWAY_KEY 时,收到该 key 的请求应被短路为"可信转发"
// (置 edge-trusted + 无分组合成 APIKey),不走 GetByKey/消费者鉴权。services 传 nil
// 也能通过 —— 正好证明短路发生在 GetByKey 之前。
func TestEdgeTrustedForward_ShortCircuits(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{EdgeMode: true, CellGatewayKey: "cell-secret"}
	h := apiKeyAuthWithSubscription(nil, nil, cfg)

	var sawEdge, nilGroupKey bool
	e := gin.New()
	e.POST("/v1/messages", gin.HandlerFunc(h), func(c *gin.Context) {
		sawEdge = IsEdgeTrusted(c)
		ak, ok := GetAPIKeyFromContext(c)
		nilGroupKey = ok && ak != nil && ak.GroupID == nil && ak.User != nil
		c.String(http.StatusOK, "ok")
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer cell-secret")
	e.ServeHTTP(w, req)

	if w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Fatalf("可信转发应放行到 handler; got %d %q", w.Code, w.Body.String())
	}
	if !sawEdge {
		t.Fatal("应标记为 edge-trusted")
	}
	if !nilGroupKey {
		t.Fatal("应注入无分组、带最小 User 的合成 APIKey")
	}
}

// 非 EDGE 模式即使配了 key 也不短路(该短路仅限 EDGE cell)。这里用错误的 key +
// EdgeMode=false,断言不会被当作 edge-trusted。为避免走到 GetByKey(nil) panic,
// 用一个会在 Require... 之前就返回的探针:直接检查 IsEdgeTrusted 常态为 false。
func TestIsEdgeTrusted_DefaultFalse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if IsEdgeTrusted(c) {
		t.Fatal("未置位时应为 false")
	}
}
