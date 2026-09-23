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
	"github.com/gin-gonic/gin"
)

// 早期心跳:cell 迟迟不回响应头时,中央先回 200+SSE 头并发注释帧,避免 Cloudflare 524。
func newEarlyPingCentral(t *testing.T, cellURL string, afterSec int) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := config.EdgeForwardConfig{Enabled: true, CellURL: cellURL, Key: "k", Groups: []string{"claude"},
		EarlyPingAfterSeconds: afterSec, EarlyPingIntervalSeconds: 1}
	e := gin.New()
	e.POST("/v1/messages",
		func(c *gin.Context) {
			c.Set(string(ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Slug: "claude", Platform: service.PlatformAnthropic}})
			c.Next()
		},
		EdgeForward(cfg, nil, nil, nil, nil, nil),
		func(c *gin.Context) { c.String(http.StatusOK, "local-should-not-run") },
	)
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	return srv
}

func TestEdgeForward_EarlyPing_SlowCellThenStream(t *testing.T) {
	cell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1800 * time.Millisecond) // 超过 1s 阈值
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer cell.Close()
	central := newEarlyPingCentral(t, cell.URL, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, central.URL+"/v1/messages", strings.NewReader(`{"stream":true}`))
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求中央失败: %v", err)
	}
	defer resp.Body.Close()
	headersAt := time.Since(start)
	if headersAt > 1500*time.Millisecond {
		t.Fatalf("响应头应在阈值(1s)附近就回给客户端,实际 %s", headersAt)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("早期心跳应回 200 + SSE 头; got %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	all, _ := io.ReadAll(bufio.NewReader(resp.Body))
	out := string(all)
	if !strings.Contains(out, ": ping\n\n") {
		t.Fatalf("应收到注释帧心跳; got %q", out)
	}
	if !strings.Contains(out, "event: message_start") || !strings.Contains(out, "event: message_stop") {
		t.Fatalf("cell 正文应在心跳之后原样透传; got %q", out)
	}
	if strings.Contains(out, "event: error") {
		t.Fatalf("正常流不应合成错误帧; got %q", out)
	}
}

func TestEdgeForward_EarlyPing_SlowCellThenError_BecomesSSEErrorFrame(t *testing.T) {
	cell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1800 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(service.EdgeUpstreamCauseHeader, "529|overloaded")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"upstream_error","message":"Upstream service overloaded, please retry later"}}`))
	}))
	defer cell.Close()
	central := newEarlyPingCentral(t, cell.URL, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, central.URL+"/v1/messages", strings.NewReader(`{"stream":true}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求中央失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("心跳已开始,状态码只能是 200; got %d", resp.StatusCode)
	}
	all, _ := io.ReadAll(resp.Body)
	out := string(all)
	if !strings.Contains(out, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"upstream_error\",\"message\":\"Upstream service overloaded, please retry later\"}}\n\n") {
		t.Fatalf("cell 错误应改写为 event: error 帧且保留原文; got %q", out)
	}
	if resp.Header.Get(service.EdgeUpstreamCauseHeader) != "" {
		t.Fatalf("边信道头不得透传给客户端")
	}
}

// 非流式请求不发早期心跳:cell 慢也照常等,错误按 HTTP 状态码原样回。
func TestEdgeForward_EarlyPing_NonStreamUnchanged(t *testing.T) {
	cell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"type":"error"}`))
	}))
	defer cell.Close()
	central := newEarlyPingCentral(t, cell.URL, 1)

	req, _ := http.NewRequest(http.MethodPost, central.URL+"/v1/messages", strings.NewReader(`{"stream":false}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求中央失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("非流式应原样回 502; got %d", resp.StatusCode)
	}
}

// 心跳开始前 cell 就回了:完全走旧路径(头/状态码原样透传,无心跳帧)。
func TestEdgeForward_EarlyPing_FastCellNoPing(t *testing.T) {
	cell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Cell-Marker", "1")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: fast\n\n"))
	}))
	defer cell.Close()
	central := newEarlyPingCentral(t, cell.URL, 1)

	req, _ := http.NewRequest(http.MethodPost, central.URL+"/v1/messages", strings.NewReader(`{"stream":true}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求中央失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Cell-Marker") != "1" {
		t.Fatalf("快速响应应原样透传 cell 头")
	}
	all, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(all), ": ping") {
		t.Fatalf("cell 在阈值内回了,不应有心跳帧; got %q", string(all))
	}
}
