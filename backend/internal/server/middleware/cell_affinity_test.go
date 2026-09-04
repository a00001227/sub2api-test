package middleware

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func TestStickySessionKey(t *testing.T) {
	// metadata.user_id 最优先。
	if k := stickySessionKey([]byte(`{"metadata":{"user_id":"session_abc"},"messages":[{"role":"user","content":"hi"}]}`)); k != "uid:session_abc" {
		t.Fatalf("应取 metadata.user_id; got %q", k)
	}
	// 无 user_id → system+首条消息摘要,跨轮稳定(后续消息变化不影响键)。
	turn1 := stickySessionKey([]byte(`{"system":"you are x","messages":[{"role":"user","content":"q1"}]}`))
	turn2 := stickySessionKey([]byte(`{"system":"you are x","messages":[{"role":"user","content":"q1"},{"role":"assistant","content":"a1"},{"role":"user","content":"q2"}]}`))
	if turn1 == "" || turn1 != turn2 {
		t.Fatalf("同会话跨轮键应稳定; t1=%q t2=%q", turn1, turn2)
	}
	// 不同会话 → 不同键。
	other := stickySessionKey([]byte(`{"system":"you are y","messages":[{"role":"user","content":"q1"}]}`))
	if other == turn1 {
		t.Fatalf("不同会话不应同键")
	}
	// 空/无信号 → 空键(不做粘性)。
	if stickySessionKey(nil) != "" || stickySessionKey([]byte(`{}`)) != "" {
		t.Fatalf("无稳定信号应返回空键")
	}
}

func TestSessionAffinity_GetPutExpiry(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	sa := newSessionAffinity(5 * time.Minute)
	sa.now = func() time.Time { return now }
	u, _ := url.Parse("http://cell-a")

	sa.put("k", u)
	if got, ok := sa.get("k"); !ok || got.String() != "http://cell-a" {
		t.Fatalf("put 后应能 get 回; got %v ok=%v", got, ok)
	}
	// 未过期。
	now = now.Add(4 * time.Minute)
	if _, ok := sa.get("k"); !ok {
		t.Fatalf("TTL 内应仍有效")
	}
	// 过期。
	now = now.Add(2 * time.Minute)
	if _, ok := sa.get("k"); ok {
		t.Fatalf("过期后应失效")
	}
	// 空键 no-op。
	sa.put("", u)
	if _, ok := sa.get(""); ok {
		t.Fatalf("空键不应绑定")
	}
}

func TestMoveToFront(t *testing.T) {
	mk := func(s string) *url.URL { u, _ := url.Parse(s); return u }
	order := []*url.URL{mk("http://a"), mk("http://b"), mk("http://c")}

	got := urlStrings(moveToFront(order, mk("http://c")))
	if strings.Join(got, ",") != "http://c,http://a,http://b" {
		t.Fatalf("应把 c 移到队首,其余保序; got %v", got)
	}
	// 不在列表 → 原样。
	got = urlStrings(moveToFront(order, mk("http://x")))
	if strings.Join(got, ",") != "http://a,http://b,http://c" {
		t.Fatalf("陈旧绑定应忽略; got %v", got)
	}
	// 已在队首 → 原样。
	got = urlStrings(moveToFront(order, mk("http://a")))
	if strings.Join(got, ",") != "http://a,http://b,http://c" {
		t.Fatalf("已在队首应不变; got %v", got)
	}
}

// 用注入 rng 构造引擎(复用同一处理函数 → 同一 affinity,跨请求粘性生效)。
func engineWithHandlerRng(resolver cellResolver, rng func() float64) *gin.Engine {
	gin.SetMode(gin.TestMode)
	groupSet := map[string]struct{}{"claude": {}}
	e := gin.New()
	e.POST("/v1/messages",
		func(c *gin.Context) {
			c.Set(string(ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Slug: "claude"}})
			c.Next()
		},
		newEdgeForwardHandler(resolver, groupSet, nil, "cellkey", rng, nil, nil, nil, nil),
		func(c *gin.Context) { c.String(http.StatusOK, "local-should-not-run") },
	)
	return e
}

// 同会话第二次请求应固定回第一次落定的 cell,即使加权随机本会选另一台。
func TestEdgeForwardHandler_StickyRoutesToSameCell(t *testing.T) {
	mkCell := func(id string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(id))
		}))
	}
	cellA := mkCell("A")
	defer cellA.Close()
	cellB := mkCell("B")
	defer cellB.Close()

	d := &dynamicResolver{}
	d.set([]cellCandidate{
		{url: mustURL(t, cellA.URL), reputation: 50},
		{url: mustURL(t, cellB.URL), reputation: 50},
	})
	// req1 rng(0,0) → order [A,B] → 落 A,绑定;req2 rng(0.6,0) → 加权本应 [B,A],
	// 但粘性把 A 提前 → 仍落 A。
	engine := engineWithHandlerRng(d, scriptedRng(0, 0, 0.6, 0))
	body := `{"metadata":{"user_id":"session_xyz"},"messages":[{"role":"user","content":"hi"}]}`

	w1 := httptest.NewRecorder()
	engine.ServeHTTP(w1, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	if w1.Body.String() != "A" {
		t.Fatalf("首次应落 A; got %q", w1.Body.String())
	}
	w2 := httptest.NewRecorder()
	engine.ServeHTTP(w2, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	if w2.Body.String() != "A" {
		t.Fatalf("同会话应粘回 A(即使加权本会选 B); got %q", w2.Body.String())
	}
}

// 绑定的 cell 已从存活池移除 → 忽略陈旧绑定,按加权随机落到其它存活 cell。
func TestEdgeForwardHandler_StickyIgnoresStaleBinding(t *testing.T) {
	cellA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("A"))
	}))
	cellB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("B"))
	}))
	defer cellB.Close()

	d := &dynamicResolver{}
	d.set([]cellCandidate{{url: mustURL(t, cellA.URL), reputation: 50}})
	engine := engineWithHandlerRng(d, zeroRng)
	body := `{"metadata":{"user_id":"session_stale"},"messages":[{"role":"user","content":"hi"}]}`

	w1 := httptest.NewRecorder()
	engine.ServeHTTP(w1, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	if w1.Body.String() != "A" {
		t.Fatalf("首次应落 A; got %q", w1.Body.String())
	}
	// A 从池中移除(掉线/下架),只剩 B。
	cellA.Close()
	d.set([]cellCandidate{{url: mustURL(t, cellB.URL), reputation: 50}})

	w2 := httptest.NewRecorder()
	engine.ServeHTTP(w2, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	if w2.Body.String() != "B" {
		t.Fatalf("绑定 cell 已下架 → 应落存活的 B; got %q", w2.Body.String())
	}
}
