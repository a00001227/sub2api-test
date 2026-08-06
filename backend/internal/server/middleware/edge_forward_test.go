package middleware

import (
	"context"
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
		EdgeForward(cfg),
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
		EdgeForward(cfg),
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
