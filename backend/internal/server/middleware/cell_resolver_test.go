package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("bad url %q: %v", s, err)
	}
	return u
}

func urlStrings(us []*url.URL) []string {
	out := make([]string, 0, len(us))
	for _, u := range us {
		out = append(out, u.String())
	}
	return out
}

// 用注入的 resolver 直接构造转发处理函数的测试引擎(绕过配置解析),
// 前置中间件塞入组 slug,终端 handler 标记 local(未转移到本地才会出现)。
func engineWithHandler(resolver cellResolver, groupSlug string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	groupSet := map[string]struct{}{"claude": {}}
	e := gin.New()
	e.POST("/v1/messages",
		func(c *gin.Context) {
			if groupSlug != "" {
				c.Set(string(ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Slug: groupSlug}})
			}
			c.Next()
		},
		newEdgeForwardHandler(resolver, groupSet, "cellkey"),
		func(c *gin.Context) {
			c.Header("X-Handled-By", "local")
			c.String(http.StatusOK, "local-ok")
		},
	)
	return e
}

func TestDynamicResolver_OrderAndStaticFallback(t *testing.T) {
	u1, u2, static := mustURL(t, "http://a"), mustURL(t, "http://b"), mustURL(t, "http://s")
	d := &dynamicResolver{static: static}

	d.set([]*url.URL{u1, u2})
	if got := strings.Join(urlStrings(d.candidates()), ","); got != "http://a,http://b,http://s" {
		t.Fatalf("动态列表 + static 兜底,顺序应保持; got %q", got)
	}

	// static 已在动态列表中 → 不重复追加。
	d.set([]*url.URL{static, u1})
	if got := strings.Join(urlStrings(d.candidates()), ","); got != "http://s,http://a" {
		t.Fatalf("static 应去重; got %q", got)
	}

	// 动态池空 + 有 static → 只剩兜底。
	d.set(nil)
	if got := strings.Join(urlStrings(d.candidates()), ","); got != "http://s" {
		t.Fatalf("空池应退回 static; got %q", got)
	}

	// 无 static + 空池 → 无候选(EdgeForward 会 502)。
	dNoStatic := &dynamicResolver{}
	if got := dNoStatic.candidates(); len(got) != 0 {
		t.Fatalf("无 static 空池应无候选; got %v", urlStrings(got))
	}
}

// 首候选传输失败(写响应前)→ 顺位转移到下一个存活 cell;客户端拿到存活 cell 的响应。
func TestEdgeForwardHandler_FailoverToNextCell(t *testing.T) {
	var served string
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = r.Header.Get("Authorization")
		w.Header().Set("X-Handled-By", "cell")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("live-ok"))
	}))
	defer live.Close()
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // 关掉 → 拨号被拒

	d := &dynamicResolver{}
	d.set([]*url.URL{mustURL(t, deadURL), mustURL(t, live.URL)})
	w := httptest.NewRecorder()
	engineWithHandler(d, "claude").ServeHTTP(
		w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))

	if w.Body.String() != "live-ok" || w.Header().Get("X-Handled-By") != "cell" {
		t.Fatalf("应转移到存活 cell; got header=%q body=%q", w.Header().Get("X-Handled-By"), w.Body.String())
	}
	if served != "Bearer cellkey" {
		t.Fatalf("存活 cell 应收到 cell key; got %q", served)
	}
}

// 全部候选不可达 → 502 upstream_error,绝不回落本地。
func TestEdgeForwardHandler_AllCandidatesDead502(t *testing.T) {
	d1 := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	u1 := d1.URL
	d1.Close()
	d2 := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	u2 := d2.URL
	d2.Close()

	d := &dynamicResolver{}
	d.set([]*url.URL{mustURL(t, u1), mustURL(t, u2)})
	w := httptest.NewRecorder()
	engineWithHandler(d, "claude").ServeHTTP(
		w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))

	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "upstream_error") {
		t.Fatalf("全挂应 502 upstream_error; got %d %q", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Handled-By") == "local" || strings.Contains(w.Body.String(), "local-ok") {
		t.Fatalf("不应回落本地; body=%q", w.Body.String())
	}
}

// 从 Portal routable 端点拉取并按顺序填充缓存,带内部 token。
func TestStartRegistryRefresh_PopulatesFromPortal(t *testing.T) {
	var gotAuth string
	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cells":[{"baseUrl":"http://a"},{"baseUrl":"http://b"}]}`))
	}))
	defer portal.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &dynamicResolver{}
	startRegistryRefresh(ctx, d, portal.URL, "tok", time.Hour) // 大间隔:只验首次异步拉取

	var cands []*url.URL
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cands = d.candidates(); len(cands) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := urlStrings(cands); strings.Join(got, ",") != "http://a,http://b" {
		t.Fatalf("应从 Portal 按序拉到 2 个 cell; got %v", got)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("应带内部 token; got %q", gotAuth)
	}
}
