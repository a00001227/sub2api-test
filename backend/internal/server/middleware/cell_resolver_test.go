package middleware

import (
	"context"
	"math"
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

// rng 恒返回 0 → weightedOrder 退化为插入顺序(确定性,用于失败转移/存活性测试)。
func zeroRng() float64 { return 0 }

// scriptedRng 依次返回给定值,便于确定性验证加权抽样。
func scriptedRng(vals ...float64) func() float64 {
	i := 0
	return func() float64 {
		v := vals[i%len(vals)]
		i++
		return v
	}
}

// 用注入的 resolver + 确定性 rng 直接构造转发处理函数(绕过配置解析)。
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
		newEdgeForwardHandler(resolver, groupSet, nil, "cellkey", zeroRng, nil, nil, nil),
		func(c *gin.Context) {
			c.Header("X-Handled-By", "local")
			c.String(http.StatusOK, "local-ok")
		},
	)
	return e
}

func TestReputationWeight(t *testing.T) {
	if w := reputationWeight(50); math.Abs(w-1.0) > 1e-9 {
		t.Fatalf("base 50 应 ×1.00; got %v", w)
	}
	if w := reputationWeight(100); math.Abs(w-1.2) > 1e-9 {
		t.Fatalf("100 应 ×1.20; got %v", w)
	}
	if reputationWeight(90) <= reputationWeight(50) {
		t.Fatalf("高信誉权重应更大")
	}
	if w := reputationWeight(0); w < 0.1-1e-9 {
		t.Fatalf("低信誉应有下限 0.1,不彻底排除; got %v", w)
	}
}

func TestWeightedOrder_RngSelectsByWeight(t *testing.T) {
	a := cellCandidate{url: mustURL(t, "http://a"), reputation: 50}
	b := cellCandidate{url: mustURL(t, "http://b"), reputation: 50}
	// 等权,total=2:rng=0 → 首个 A;rng=0.6 → r=1.2,扣 A(1.0) 后 0.2>0,扣 B → 选 B。
	if got := urlStrings(weightedOrder([]cellCandidate{a, b}, zeroRng)); strings.Join(got, ",") != "http://a,http://b" {
		t.Fatalf("rng=0 应首选 A; got %v", got)
	}
	if got := urlStrings(weightedOrder([]cellCandidate{a, b}, scriptedRng(0.6, 0))); strings.Join(got, ",") != "http://b,http://a" {
		t.Fatalf("rng=0.6 应首选 B; got %v", got)
	}
}

func TestDynamicResolver_PoolAndFallback(t *testing.T) {
	static := mustURL(t, "http://s")
	d := &dynamicResolver{static: static}

	d.set([]cellCandidate{
		{url: mustURL(t, "http://a"), reputation: 90},
		{url: mustURL(t, "http://b"), reputation: 60},
	})
	pool := d.poolCandidates()
	if len(pool) != 2 || pool[0].reputation != 90 || pool[1].reputation != 60 {
		t.Fatalf("poolCandidates 应带信誉分; got %+v", pool)
	}
	if d.fallback() == nil || d.fallback().String() != "http://s" {
		t.Fatalf("fallback 应为静态兜底; got %v", d.fallback())
	}
	// static 不混入加权池。
	for _, c := range pool {
		if c.url.String() == "http://s" {
			t.Fatalf("static 不应出现在 poolCandidates")
		}
	}

	// 空池 → poolCandidates 空,fallback 仍在(中央会兜底转发)。
	d.set(nil)
	if len(d.poolCandidates()) != 0 {
		t.Fatalf("空池 poolCandidates 应空")
	}
	if d.fallback().String() != "http://s" {
		t.Fatalf("空池仍应有 fallback")
	}
}

func TestStaticResolver_IsPoolNotFallback(t *testing.T) {
	s := &staticResolver{target: mustURL(t, "http://only")}
	if len(s.poolCandidates()) != 1 || s.poolCandidates()[0].reputation != 50 {
		t.Fatalf("静态单 cell 应是池、base 50; got %+v", s.poolCandidates())
	}
	if s.poolCandidates()[0].lane != laneNormal {
		t.Fatalf("静态单 cell 应视为 normal 道; got %q", s.poolCandidates()[0].lane)
	}
	if s.fallback() != nil {
		t.Fatalf("静态模式无独立 fallback")
	}
}

func TestNormalizeLane(t *testing.T) {
	cases := map[string]string{
		"":             laneNormal,
		"   ":          laneNormal,
		"NORMAL":       laneNormal,
		"normal":       laneNormal,
		"batch":        laneBatch,
		"  Batch ":     laneBatch,
		"distillation": laneDistillation,
		"DISTILLATION": laneDistillation,
		"garbage":      laneNormal, // 未知 → normal(fail-safe)
	}
	for in, want := range cases {
		if got := normalizeLane(in); got != want {
			t.Fatalf("normalizeLane(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestFilterByLane(t *testing.T) {
	pool := []cellCandidate{
		{url: mustURL(t, "http://n"), lane: laneNormal},
		{url: mustURL(t, "http://b"), lane: laneBatch},
		{url: mustURL(t, "http://d"), lane: laneDistillation},
		{url: mustURL(t, "http://x"), lane: ""}, // 空 → 视为 normal
	}
	if got := urlStrings(candURLs(filterByLane(pool, laneDistillation))); strings.Join(got, ",") != "http://d" {
		t.Fatalf("distillation 过滤应只留 d; got %v", got)
	}
	if got := candURLs(filterByLane(pool, laneNormal)); len(got) != 2 {
		t.Fatalf("normal 过滤应含 n + 空道 x(共 2); got %d", len(got))
	}
	if got := candURLs(filterByLane(pool, laneBatch)); len(got) != 1 || got[0].String() != "http://b" {
		t.Fatalf("batch 过滤应只留 b; got %v", urlStrings(got))
	}
}

// candURLs 抽出候选的 URL 便于断言。
func candURLs(cs []cellCandidate) []*url.URL {
	out := make([]*url.URL, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.url)
	}
	return out
}

// 陈旧的跨道会话绑定绝不能把消费者钉到别道 cell:order 已按道过滤,moveToFront 找不到
// 该 cell(idx=-1)→ 忽略。这是「亲和不跨道」的结构性保证。
func TestAffinity_DoesNotPinAcrossLane(t *testing.T) {
	normal := cellCandidate{url: mustURL(t, "http://n"), reputation: 50, lane: laneNormal}
	distill := cellCandidate{url: mustURL(t, "http://d"), reputation: 50, lane: laneDistillation}
	order := weightedOrder(filterByLane([]cellCandidate{normal, distill}, laneDistillation), zeroRng)
	order = moveToFront(order, normal.url) // 上一轮 normal 会话遗留的绑定
	if len(order) != 1 || order[0].String() != "http://d" {
		t.Fatalf("跨道陈旧绑定不应把消费者钉到 normal cell; got %v", urlStrings(order))
	}
}

// 首候选传输失败(写响应前)→ 顺位转移到下一个存活 cell。
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
	dead.Close()

	d := &dynamicResolver{}
	d.set([]cellCandidate{
		{url: mustURL(t, deadURL), reputation: 50},
		{url: mustURL(t, live.URL), reputation: 50},
	})
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
	d.set([]cellCandidate{
		{url: mustURL(t, u1), reputation: 50},
		{url: mustURL(t, u2), reputation: 50},
	})
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

// 动态池全挂 → 回落到静态兜底候选。
func TestEdgeForwardHandler_FallbackWhenPoolDead(t *testing.T) {
	fb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Handled-By", "fallback")
		_, _ = w.Write([]byte("fb-ok"))
	}))
	defer fb.Close()
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	d := &dynamicResolver{static: mustURL(t, fb.URL)}
	d.set([]cellCandidate{{url: mustURL(t, deadURL), reputation: 90}})
	w := httptest.NewRecorder()
	engineWithHandler(d, "claude").ServeHTTP(
		w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))

	if w.Body.String() != "fb-ok" || w.Header().Get("X-Handled-By") != "fallback" {
		t.Fatalf("池全挂应回落静态兜底; got header=%q body=%q", w.Header().Get("X-Handled-By"), w.Body.String())
	}
}

// 从 Portal routable 端点拉取 baseUrl + reputation 并填充缓存,带内部 token。
func TestStartRegistryRefresh_PopulatesFromPortal(t *testing.T) {
	var gotAuth string
	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cells":[{"baseUrl":"http://a","reputation":90},{"baseUrl":"http://b","reputation":60}]}`))
	}))
	defer portal.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &dynamicResolver{}
	startRegistryRefresh(ctx, d, portal.URL, "tok", time.Hour)

	var pool []cellCandidate
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pool = d.poolCandidates(); len(pool) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(pool) != 2 || pool[0].url.String() != "http://a" || pool[0].reputation != 90 || pool[1].reputation != 60 {
		t.Fatalf("应按序拉到带信誉的 cell; got %+v", pool)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("应带内部 token; got %q", gotAuth)
	}
}

// Portal routable 的 type 字段 → 候选的 lane;缺 type 的 cell 归一为 normal(向后兼容旧 Portal)。
func TestStartRegistryRefresh_ParsesLane(t *testing.T) {
	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cells":[` +
			`{"baseUrl":"http://d","reputation":50,"type":"distillation"},` +
			`{"baseUrl":"http://n","reputation":50}]}`)) // 无 type → normal
	}))
	defer portal.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &dynamicResolver{}
	startRegistryRefresh(ctx, d, portal.URL, "", time.Hour)

	var pool []cellCandidate
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pool = d.poolCandidates(); len(pool) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	byURL := map[string]string{}
	for _, c := range pool {
		byURL[c.url.String()] = c.lane
	}
	if byURL["http://d"] != laneDistillation {
		t.Fatalf("type=distillation 应解析为 distillation 道; got %q", byURL["http://d"])
	}
	if byURL["http://n"] != laneNormal {
		t.Fatalf("缺 type 应归一为 normal; got %q", byURL["http://n"])
	}
}
