package middleware

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// ClientRequestURL 还原分组前缀改写前的完整 URL;未改写时用当前路径。
func TestClientRequestURL_UsesOriginalPathAndForwardedProto(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Request.Host = "api.eirouter.ai"
	c.Request.Header.Set("X-Forwarded-Proto", "https")
	c.Set(string(ContextKeyOriginalPath), "/openai/v1/responses")
	if got := ClientRequestURL(c); got != "https://api.eirouter.ai/openai/v1/responses" {
		t.Fatalf("got %q", got)
	}

	c2, _ := gin.CreateTestContext(httptest.NewRecorder())
	c2.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	c2.Request.Host = "example.com"
	if got := ClientRequestURL(c2); got != "http://example.com/v1/messages" {
		t.Fatalf("no rewrite → current path, plain http; got %q", got)
	}
	if got := modelNotAllowedBody("", "POST", "http://x/v1/messages"); got == "" || !strings.Contains(got, `no \"model\" field`) {
		t.Fatalf("empty model message; got %q", got)
	}
}
