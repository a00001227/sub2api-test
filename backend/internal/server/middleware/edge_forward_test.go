package middleware

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

// 构造一个带 EdgeForward 的引擎:前置中间件把 APIKey(组 slug)塞进上下文,
// 终端 handler 标记 "local"(表示走了本地,未转发)。
func newEdgeForwardEngine(cfg config.EdgeForwardConfig, groupSlug string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.POST("/v1/messages",
		func(c *gin.Context) {
			if groupSlug != "" {
				c.Set(string(ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Slug: groupSlug}})
			}
			c.Next()
		},
		EdgeForward(cfg, nil, nil, nil, nil),
		func(c *gin.Context) {
			c.Header("X-Handled-By", "local")
			c.String(http.StatusOK, "local-ok")
		},
	)
	return e
}

func TestEdgeForward_DisabledIsNoop(t *testing.T) {
	e := newEdgeForwardEngine(config.EdgeForwardConfig{Enabled: false}, "claude")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))
	if w.Header().Get("X-Handled-By") != "local" || w.Body.String() != "local-ok" {
		t.Fatalf("disabled should pass through to local; got header=%q body=%q", w.Header().Get("X-Handled-By"), w.Body.String())
	}
}

func TestEdgeForward_GroupMatchForwards(t *testing.T) {
	// 假 cell:回一个可识别的响应。
	cell := httptest.NewServer(http.HandlerFunc(func(cw http.ResponseWriter, cr *http.Request) {
		if got := cr.Header.Get("Authorization"); got != "Bearer cellkey" {
			t.Errorf("cell 期望收到 Bearer cellkey, got %q", got)
		}
		if cr.URL.Path != "/v1/messages" {
			t.Errorf("cell 期望路径 /v1/messages, got %q", cr.URL.Path)
		}
		cw.Header().Set("X-Handled-By", "cell")
		cw.WriteHeader(http.StatusOK)
		_, _ = cw.Write([]byte("cell-ok"))
	}))
	defer cell.Close()

	cfg := config.EdgeForwardConfig{Enabled: true, CellURL: cell.URL, Key: "cellkey", Groups: []string{"claude"}}
	e := newEdgeForwardEngine(cfg, "claude")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))
	if w.Body.String() != "cell-ok" || w.Header().Get("X-Handled-By") != "cell" {
		t.Fatalf("命中组应转发到 cell; got header=%q body=%q", w.Header().Get("X-Handled-By"), w.Body.String())
	}
}

func TestEdgeForward_GroupNoMatchStaysLocal(t *testing.T) {
	cfg := config.EdgeForwardConfig{Enabled: true, CellURL: "http://127.0.0.1:1", Key: "k", Groups: []string{"other"}}
	e := newEdgeForwardEngine(cfg, "claude") // 组 claude 不在列表 → 本地
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))
	if w.Body.String() != "local-ok" {
		t.Fatalf("未命中组应走本地; got body=%q", w.Body.String())
	}
}

func TestEdgeForward_EmptyGroupsIsNoop(t *testing.T) {
	cfg := config.EdgeForwardConfig{Enabled: true, CellURL: "http://127.0.0.1:1", Key: "k", Groups: nil}
	e := newEdgeForwardEngine(cfg, "claude")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))
	if w.Body.String() != "local-ok" {
		t.Fatalf("空组列表应走本地; got body=%q", w.Body.String())
	}
}

// WS: 命中组的升级请求应被双向代理到 cell(echo),客户端能收到经 cell 回来的消息。
func TestEdgeForward_WebSocketForwards(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// 假 cell:WS echo,回 "echo:"+原文。
	cell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(-1)
		for {
			typ, data, rerr := conn.Read(r.Context())
			if rerr != nil {
				return
			}
			if werr := conn.Write(r.Context(), typ, append([]byte("echo:"), data...)); werr != nil {
				return
			}
		}
	}))
	defer cell.Close()

	cfg := config.EdgeForwardConfig{Enabled: true, CellURL: cell.URL, Key: "k", Groups: []string{"claude"}}
	e := gin.New()
	e.GET("/v1/responses",
		func(c *gin.Context) {
			c.Set(string(ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Slug: "claude"}})
			c.Next()
		},
		EdgeForward(cfg, nil, nil, nil, nil),
		func(c *gin.Context) { c.String(http.StatusOK, "local-should-not-run") },
	)
	central := httptest.NewServer(e)
	defer central.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(central.URL, "http") + "/v1/responses"
	cconn, _, err := coderws.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("拨号中央 WS 失败: %v", err)
	}
	defer cconn.CloseNow()
	if err := cconn.Write(ctx, coderws.MessageText, []byte("hi")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	_, data, err := cconn.Read(ctx)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(data) != "echo:hi" {
		t.Fatalf("WS 应经 cell echo 回传; got %q", string(data))
	}
}

// SSE 必须逐块流式回传(每块 flush),而不是缓冲到 cell 结束才吐。
// 用一个 handshake 证明:cell 写完 event1 就阻塞,直到客户端确认已收到 event1
// 才写 event2 —— 客户端能在 event2 之前拿到 event1,即证明中途 flush 生效。
func TestEdgeForward_SSEStreamsChunked(t *testing.T) {
	gin.SetMode(gin.TestMode)
	released := make(chan struct{})
	cell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: event1\n\n"))
		if fl != nil {
			fl.Flush()
		}
		<-released // 客户端确认拿到 event1 后才继续
		_, _ = w.Write([]byte("data: event2\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
	defer cell.Close()

	cfg := config.EdgeForwardConfig{Enabled: true, CellURL: cell.URL, Key: "k", Groups: []string{"claude"}}
	e := gin.New()
	e.POST("/v1/messages",
		func(c *gin.Context) {
			c.Set(string(ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Slug: "claude"}})
			c.Next()
		},
		EdgeForward(cfg, nil, nil, nil, nil),
		func(c *gin.Context) { c.String(http.StatusOK, "local-should-not-run") },
	)
	central := httptest.NewServer(e)
	defer central.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, central.URL+"/v1/messages", strings.NewReader("{}"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求中央失败: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("应透传 SSE Content-Type; got %q", ct)
	}
	br := bufio.NewReader(resp.Body)
	line1, err := br.ReadString('\n') // 应在 cell 尚未写 event2 前就到达
	if err != nil || !strings.Contains(line1, "event1") {
		t.Fatalf("应先流式收到 event1; got %q err=%v", line1, err)
	}
	close(released) // 放行 cell 写 event2
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "event2") {
		t.Fatalf("应继续收到 event2; got %q", string(rest))
	}
}

// 转发到 cell 时不得泄漏消费者凭据:中央把 Authorization 换成 cell key,
// 并删掉消费者的 x-api-key —— cell 只能看到 cell 自己的 key。
func TestEdgeForward_DoesNotLeakConsumerKey(t *testing.T) {
	var gotAuth, gotXAPIKey string
	cell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotXAPIKey = r.Header.Get("X-Api-Key")
		w.WriteHeader(http.StatusOK)
	}))
	defer cell.Close()

	cfg := config.EdgeForwardConfig{Enabled: true, CellURL: cell.URL, Key: "cellkey", Groups: []string{"claude"}}
	e := newEdgeForwardEngine(cfg, "claude")
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer consumer-secret")
	req.Header.Set("X-Api-Key", "consumer-secret")
	e.ServeHTTP(httptest.NewRecorder(), req)

	if gotAuth != "Bearer cellkey" {
		t.Fatalf("cell 应只收到 cell key; got Authorization=%q", gotAuth)
	}
	if gotXAPIKey != "" {
		t.Fatalf("消费者 x-api-key 不应泄漏到 cell; got %q", gotXAPIKey)
	}
}

// 用注入的多-cell resolver + 确定性 rng(=0 → 候选顺序=池顺序)构造引擎,
// 便于测跨 cell 失败转移。
func newEdgeForwardEngineWithResolver(resolver cellResolver, groupSlug, key string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	h := newEdgeForwardHandler(resolver, map[string]struct{}{"claude": {}}, nil, key, func() float64 { return 0 }, nil, nil, nil, nil)
	e.POST("/v1/messages",
		func(c *gin.Context) {
			if groupSlug != "" {
				c.Set(string(ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Slug: groupSlug}})
			}
			c.Next()
		},
		h,
		func(c *gin.Context) { c.String(http.StatusOK, "local-ok") },
	)
	return e
}

// newEdgeForwardEngineWithLanes:带「组→工作道」映射的引擎,groupSet 含消费者组,
// 便于测按道路由(护号)。zeroRng → 候选顺序=过滤后池顺序。
func newEdgeForwardEngineWithLanes(resolver cellResolver, groupSlug, key string, groupLanes map[string]string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	h := newEdgeForwardHandler(resolver, map[string]struct{}{groupSlug: {}}, groupLanes, key, func() float64 { return 0 }, nil, nil, nil, nil)
	e.POST("/v1/messages",
		func(c *gin.Context) {
			c.Set(string(ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Slug: groupSlug}})
			c.Next()
		},
		h,
		func(c *gin.Context) { c.Header("X-Handled-By", "local"); c.String(http.StatusOK, "local-ok") },
	)
	return e
}

// newEdgeForwardEngineWithGroupLane:消费者工作道来自组自身的 Group.Lane(D:后台打标签),
// env 映射为 nil。验证中央改读 Group.Lane 后免 env 也能按池路由。
func newEdgeForwardEngineWithGroupLane(resolver cellResolver, groupSlug, groupLane, key string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	h := newEdgeForwardHandler(resolver, map[string]struct{}{groupSlug: {}}, nil, key, func() float64 { return 0 }, nil, nil, nil, nil)
	e.POST("/v1/messages",
		func(c *gin.Context) {
			c.Set(string(ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Slug: groupSlug, Lane: groupLane}})
			c.Next()
		},
		h,
		func(c *gin.Context) { c.Header("X-Handled-By", "local"); c.String(http.StatusOK, "local-ok") },
	)
	return e
}

// D:消费者工作道直接来自 Group.Lane(后台打的标签),不依赖 env 映射。
func TestEdgeForward_ConsumerLaneFromGroupField(t *testing.T) {
	var normalHit bool
	normalCell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		normalHit = true
		_, _ = w.Write([]byte("normal-ok"))
	}))
	defer normalCell.Close()
	distillCell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Handled-By", "distill")
		_, _ = w.Write([]byte("distill-ok"))
	}))
	defer distillCell.Close()

	resolver := &dynamicResolver{cached: []cellCandidate{
		{url: mustURL(t, normalCell.URL), reputation: 90, lane: laneNormal},
		{url: mustURL(t, distillCell.URL), reputation: 50, lane: laneDistillation},
	}}
	// 组自身 Lane=distillation,env 映射为 nil。
	e := newEdgeForwardEngineWithGroupLane(resolver, "anygroup", "distillation", "k")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))

	if w.Body.String() != "distill-ok" || w.Header().Get("X-Handled-By") != "distill" {
		t.Fatalf("应按 Group.Lane 路由到蒸馏 cell; got header=%q body=%q", w.Header().Get("X-Handled-By"), w.Body.String())
	}
	if normalHit {
		t.Fatalf("好号 cell 绝不应被命中(Group.Lane=distillation)")
	}
}

// 蒸馏消费者只应命中蒸馏 cell —— 即便好号 cell 信誉更高也绝不跨道(护号)。
func TestEdgeForward_DistillationConsumerOnlyDrawsDistillationCells(t *testing.T) {
	var normalHit bool
	normalCell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		normalHit = true
		_, _ = w.Write([]byte("normal-ok"))
	}))
	defer normalCell.Close()
	distillCell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Handled-By", "distill")
		_, _ = w.Write([]byte("distill-ok"))
	}))
	defer distillCell.Close()

	resolver := &dynamicResolver{cached: []cellCandidate{
		{url: mustURL(t, normalCell.URL), reputation: 90, lane: laneNormal}, // 高信誉好号 cell
		{url: mustURL(t, distillCell.URL), reputation: 50, lane: laneDistillation},
	}}
	e := newEdgeForwardEngineWithLanes(resolver, "distill", "k", map[string]string{"distill": laneDistillation})
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))

	if w.Body.String() != "distill-ok" || w.Header().Get("X-Handled-By") != "distill" {
		t.Fatalf("蒸馏消费者只应命中蒸馏 cell; got header=%q body=%q", w.Header().Get("X-Handled-By"), w.Body.String())
	}
	if normalHit {
		t.Fatalf("好号 cell 绝不应被蒸馏流量命中(护号)")
	}
}

// 未映射的组 → normal:只命中 normal cell,不碰蒸馏 cell。
func TestEdgeForward_UnmappedGroupIsNormal(t *testing.T) {
	var distillHit bool
	distillCell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		distillHit = true
		_, _ = w.Write([]byte("distill-ok"))
	}))
	defer distillCell.Close()
	normalCell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Handled-By", "normal")
		_, _ = w.Write([]byte("normal-ok"))
	}))
	defer normalCell.Close()

	resolver := &dynamicResolver{cached: []cellCandidate{
		{url: mustURL(t, normalCell.URL), reputation: 50, lane: laneNormal},
		{url: mustURL(t, distillCell.URL), reputation: 50, lane: laneDistillation},
	}}
	// 消费者组 "claude" 不在映射里 → normal。
	e := newEdgeForwardEngineWithLanes(resolver, "claude", "k", map[string]string{"distill": laneDistillation})
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))

	if w.Body.String() != "normal-ok" {
		t.Fatalf("未映射组应走 normal cell; got body=%q", w.Body.String())
	}
	if distillHit {
		t.Fatalf("normal 消费者不应命中蒸馏 cell")
	}
}

// 对应道无可用 cell → 502,绝不跨道、绝不回落本地(fail-closed 护号)。
func TestEdgeForward_EmptyAfterFilterReturns502NoLocal(t *testing.T) {
	var normalHit bool
	normalCell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		normalHit = true
		_, _ = w.Write([]byte("normal-ok"))
	}))
	defer normalCell.Close()

	// 池里只有 normal cell,消费者是蒸馏 → 过滤后为空。
	resolver := &dynamicResolver{cached: []cellCandidate{
		{url: mustURL(t, normalCell.URL), reputation: 50, lane: laneNormal},
	}}
	e := newEdgeForwardEngineWithLanes(resolver, "distill", "k", map[string]string{"distill": laneDistillation})
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))

	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "upstream_error") {
		t.Fatalf("蒸馏无对应 cell 应 502 upstream_error; got code=%d body=%q", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Handled-By") == "local" || strings.Contains(w.Body.String(), "local-ok") {
		t.Fatalf("绝不应回落本地; body=%q", w.Body.String())
	}
	if normalHit {
		t.Fatalf("蒸馏流量绝不应落到 normal cell(护号)")
	}
}

// 静态兜底仅对 normal 消费者生效:蒸馏消费者绝不回落到静态(normal)cell。
func TestEdgeForward_StaticFallbackNormalOnly(t *testing.T) {
	var fbHits int
	fb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fbHits++
		w.Header().Set("X-Handled-By", "fallback")
		_, _ = w.Write([]byte("fb-ok"))
	}))
	defer fb.Close()

	// 空动态池 + 静态兜底。normal 消费者 → 命中兜底。
	rN := &dynamicResolver{static: mustURL(t, fb.URL)}
	eN := newEdgeForwardEngineWithLanes(rN, "gnorm", "k", map[string]string{"gnorm": laneNormal})
	wN := httptest.NewRecorder()
	eN.ServeHTTP(wN, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))
	if wN.Body.String() != "fb-ok" || wN.Header().Get("X-Handled-By") != "fallback" {
		t.Fatalf("normal 消费者应回落静态兜底; got header=%q body=%q", wN.Header().Get("X-Handled-By"), wN.Body.String())
	}
	hitsAfterNormal := fbHits

	// 蒸馏消费者 → 502,兜底不再被命中。
	rD := &dynamicResolver{static: mustURL(t, fb.URL)}
	eD := newEdgeForwardEngineWithLanes(rD, "gdist", "k", map[string]string{"gdist": laneDistillation})
	wD := httptest.NewRecorder()
	eD.ServeHTTP(wD, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))
	if wD.Code != http.StatusBadGateway {
		t.Fatalf("蒸馏消费者无对应 cell 应 502; got %d", wD.Code)
	}
	if fbHits != hitsAfterNormal {
		t.Fatalf("蒸馏消费者绝不应回落到静态(normal)兜底; fbHits 从 %d 变到 %d", hitsAfterNormal, fbHits)
	}
}

// cell 返回 503「no available accounts」(选号前失败,没调上游)→ 中央应视同该
// 候选不可用、顺位转移到下一个 cell,而不是把 503 透传给客户端。
func TestEdgeForward_FailoverOnNoAvailableAccounts503(t *testing.T) {
	var bHit bool
	cellA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"No available accounts: no available accounts","type":"api_error"},"type":"error"}`))
	}))
	defer cellA.Close()
	cellB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHit = true
		w.Header().Set("X-Handled-By", "cellB")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("cell-b-ok"))
	}))
	defer cellB.Close()

	resolver := &dynamicResolver{cached: []cellCandidate{
		{url: mustURL(t, cellA.URL), reputation: 50},
		{url: mustURL(t, cellB.URL), reputation: 50},
	}}
	e := newEdgeForwardEngineWithResolver(resolver, "claude", "k")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))

	if w.Code != http.StatusOK || w.Body.String() != "cell-b-ok" {
		t.Fatalf("cellA 无可用账号应转移到 cellB; got code=%d body=%q", w.Code, w.Body.String())
	}
	if !bHit {
		t.Fatalf("cellB 应作为转移候选被调用")
	}
}

// 非「no available」的 503(cell/上游真实错误)必须原样透传、绝不转移
// (可能已触上游,重放有双执行风险)。
func TestEdgeForward_OtherError503NotFailedOver(t *testing.T) {
	var bHit bool
	cellA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream overloaded","type":"overloaded_error"}}`))
	}))
	defer cellA.Close()
	cellB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer cellB.Close()

	resolver := &dynamicResolver{cached: []cellCandidate{
		{url: mustURL(t, cellA.URL), reputation: 50},
		{url: mustURL(t, cellB.URL), reputation: 50},
	}}
	e := newEdgeForwardEngineWithResolver(resolver, "claude", "k")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("非 no-available 的 503 应原样透传; got code=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "upstream overloaded") {
		t.Fatalf("应透传原始 503 body; got %q", w.Body.String())
	}
	if bHit {
		t.Fatalf("非 no-available 的 503 不应转移到 cellB")
	}
}

// 命中组但 cell 不可达:客户端应拿到干净的 502 upstream_error,
// 且绝不回落到中央本地执行(避免"转发失败悄悄用了中央的号")。
func TestEdgeForward_CellUnreachableReturns502(t *testing.T) {
	cell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	cellURL := cell.URL
	cell.Close() // 关掉 → 连接被拒

	cfg := config.EdgeForwardConfig{Enabled: true, CellURL: cellURL, Key: "k", Groups: []string{"claude"}}
	e := newEdgeForwardEngine(cfg, "claude")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))

	if w.Code != http.StatusBadGateway {
		t.Fatalf("cell 不可达应 502; got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "upstream_error") {
		t.Fatalf("应返回干净的 upstream_error; got %q", w.Body.String())
	}
	if w.Header().Get("X-Handled-By") == "local" || strings.Contains(w.Body.String(), "local-ok") {
		t.Fatalf("cell 不可达绝不应回落到本地执行; body=%q", w.Body.String())
	}
}

// 转发模型白名单:不在白名单的 model 直接 403、不转发 cell;在白名单的正常转发。
func TestEdgeForward_ModelWhitelist(t *testing.T) {
	var cellHit bool
	cell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cellHit = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("cell-ok"))
	}))
	defer cell.Close()

	resolver := &staticResolver{target: mustURL(t, cell.URL)}
	allow := func(_ context.Context, model string) bool { return strings.EqualFold(model, "allowed-model") }

	gin.SetMode(gin.TestMode)
	e := gin.New()
	h := newEdgeForwardHandler(resolver, map[string]struct{}{"claude": {}}, nil, "k", func() float64 { return 0 }, nil, allow, nil, nil)
	e.POST("/v1/messages",
		func(c *gin.Context) {
			c.Set(string(ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Slug: "claude"}})
			c.Next()
		},
		h,
		func(c *gin.Context) { c.String(http.StatusOK, "local-ok") },
	)

	// 1) 不在白名单 → 403,且不应打到 cell。
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"blocked-model"}`)))
	if w.Code != http.StatusForbidden {
		t.Fatalf("白名单外模型应 403; got code=%d body=%q", w.Code, w.Body.String())
	}
	if cellHit {
		t.Fatalf("白名单外模型不应转发到 cell")
	}
	if !strings.Contains(w.Body.String(), "model not allowed") {
		t.Fatalf("应返回 model not allowed; got %q", w.Body.String())
	}

	// 2) 在白名单 → 正常转发到 cell。
	cellHit = false
	w = httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"allowed-model"}`)))
	if w.Code != http.StatusOK || !cellHit {
		t.Fatalf("白名单内模型应转发到 cell; code=%d hit=%v", w.Code, cellHit)
	}
}
