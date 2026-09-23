package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// key 绑在 Claude 组、请求 gpt-* 模型却没带 GPT 组 slug 前缀:中央直接 400 说明怎么改,
// 不转到 cell,并标记业务限制(排除 SLA)。模型/分组平台一致或无法判定时照常转发。
func TestEdgeForward_ModelPlatformMismatchRejectedAtCentral(t *testing.T) {
	var cellHit bool
	cell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cellHit = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("cell-ok"))
	}))
	defer cell.Close()

	resolver := &staticResolver{target: mustURL(t, cell.URL)}
	gin.SetMode(gin.TestMode)
	e := gin.New()
	h := newEdgeForwardHandler(resolver, map[string]struct{}{"claude": {}, "gpt": {}}, nil, "k", func() float64 { return 0 }, nil, nil, nil, nil, nil, edgeEarlyPing{})

	var capturedCtx *gin.Context
	var group *service.Group
	e.POST("/v1/chat/completions",
		func(c *gin.Context) {
			c.Set(string(ContextKeyAPIKey), &service.APIKey{Group: group})
			capturedCtx = c
			c.Next()
		},
		h,
		func(c *gin.Context) { c.String(http.StatusOK, "local-ok") },
	)
	serve := func(body string) *httptest.ResponseRecorder {
		cellHit = false
		capturedCtx = nil
		w := httptest.NewRecorder()
		e.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
		return w
	}

	// 1) Claude 组 + gpt 模型 → 400(Anthropic 形状),不打 cell,业务限制标记。
	group = &service.Group{Slug: "claude", Name: "EiRouter-Claude-Standard", Platform: service.PlatformAnthropic}
	w := serve(`{"model":"gpt-5.6-sol"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("平台不一致应 400; got code=%d body=%q", w.Code, w.Body.String())
	}
	if cellHit {
		t.Fatalf("平台不一致不应转发到 cell")
	}
	body := w.Body.String()
	// gin 的 c.JSON 会把 < > 转义成 < >,故只匹配占位符主体。
	for _, want := range []string{`"type":"error"`, `"invalid_request_error"`, "gpt-5.6-sol", "EiRouter-Claude-Standard", "channel-slug"} {
		if !strings.Contains(body, want) {
			t.Fatalf("响应应包含 %q; got %q", want, body)
		}
	}
	if !service.HasOpsClientBusinessLimited(capturedCtx) {
		t.Fatalf("平台不一致拒绝应标记 business-limited(排除 SLA)")
	}

	// 2) GPT 组 + claude 模型 → 400(OpenAI 形状,顶层无 "type":"error")。
	group = &service.Group{Slug: "gpt", Name: "EiRouter-GPT-Standard", Platform: service.PlatformOpenAI}
	w = serve(`{"model":"claude-sonnet-5"}`)
	if w.Code != http.StatusBadRequest || cellHit {
		t.Fatalf("GPT 组 + claude 模型应 400 且不转发; code=%d hit=%v", w.Code, cellHit)
	}
	if strings.Contains(w.Body.String(), `"type":"error"`) {
		t.Fatalf("OpenAI 形状不应带顶层 type=error; got %q", w.Body.String())
	}

	// 3) 平台一致 → 正常转发。
	group = &service.Group{Slug: "claude", Name: "EiRouter-Claude-Standard", Platform: service.PlatformAnthropic}
	w = serve(`{"model":"claude-sonnet-5"}`)
	if w.Code != http.StatusOK || !cellHit {
		t.Fatalf("平台一致应转发到 cell; code=%d hit=%v body=%q", w.Code, cellHit, w.Body.String())
	}
	if service.HasOpsClientBusinessLimited(capturedCtx) {
		t.Fatalf("正常转发不应被标记 business-limited")
	}

	// 4) 模型前缀不认识 / 分组无平台 → 放行(不误拦)。
	w = serve(`{"model":"some-custom-model"}`)
	if w.Code != http.StatusOK || !cellHit {
		t.Fatalf("未知模型前缀应放行转发; code=%d hit=%v", w.Code, cellHit)
	}
	group = &service.Group{Slug: "claude", Name: "NoPlatform"}
	w = serve(`{"model":"gpt-5.5"}`)
	if w.Code != http.StatusOK || !cellHit {
		t.Fatalf("分组无平台应放行转发; code=%d hit=%v", w.Code, cellHit)
	}
}
