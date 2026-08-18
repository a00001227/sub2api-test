package main

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 切片 4.2 §一：两段式 HTTP drain 单测（无需 Redis）。

func serveTracked(t *testing.T, tracker *inflightTracker, h http.Handler) (*http.Server, string) {
	t.Helper()
	srv := &http.Server{Handler: tracker.Middleware(h)}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(ln) }()
	return srv, ln.Addr().String()
}

// 正常短请求：在 graceful 期内完成，无强制 Close。
func TestHTTPDrain_NormalCompletesInGraceful(t *testing.T) {
	tracker := &inflightTracker{}
	var once sync.Once
	entered := make(chan struct{})
	srv, addr := serveTracked(t, tracker, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(200)
	}))
	done := make(chan struct{})
	go func() {
		resp, err := http.Get("http://" + addr + "/")
		if err == nil {
			_ = resp.Body.Close()
		}
		close(done)
	}()
	<-entered
	res := httpDrain(context.Background(), srv, tracker, 2*time.Second, time.Second, nil, nil)
	require.False(t, res.HTTPGracefulTimedOut)
	require.False(t, res.ForceClosed)
	require.False(t, res.Incomplete)
	<-done
	require.EqualValues(t, 0, tracker.inflight())
}

// 请求超过 graceful → 强制 Close；handler 感知取消并在 force 内退出 → Incomplete=false。
func TestHTTPDrain_ForcedCloseHandlerExits(t *testing.T) {
	tracker := &inflightTracker{}
	var once sync.Once
	entered := make(chan struct{})
	exited := make(chan struct{})
	srv, addr := serveTracked(t, tracker, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-r.Context().Done() // 被强制 Close 取消
		close(exited)
	}))
	go func() { _, _ = http.Get("http://" + addr + "/") }()
	<-entered
	res := httpDrain(context.Background(), srv, tracker, 150*time.Millisecond, 2*time.Second, nil, nil)
	require.True(t, res.HTTPGracefulTimedOut)
	require.True(t, res.ForceClosed)
	require.False(t, res.Incomplete, "handler exits within force timeout")
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("handler did not exit")
	}
}

// handler 在 force 超时内仍不退出 → Incomplete=true（不无界等待）。
func TestHTTPDrain_IncompleteWhenHandlerStuck(t *testing.T) {
	tracker := &inflightTracker{}
	var once sync.Once
	entered := make(chan struct{})
	release := make(chan struct{})
	srv, addr := serveTracked(t, tracker, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-release // 无视取消，卡住
		w.WriteHeader(200)
	}))
	defer close(release)
	go func() { _, _ = http.Get("http://" + addr + "/") }()
	<-entered
	res := httpDrain(context.Background(), srv, tracker, 100*time.Millisecond, 200*time.Millisecond, nil, nil)
	require.True(t, res.ForceClosed)
	require.True(t, res.Incomplete, "stuck handler → incomplete (bounded, no infinite wait)")
}

// waitZero：注入时钟，确定性验证有界等待。
func TestInflightTracker_WaitZero(t *testing.T) {
	tr := &inflightTracker{}
	// 空 → 立即 true。
	require.True(t, tr.waitZero(time.Second, nil, nil))
	// 有 1 个在途，超时后归零判定为 false。
	tr.n = 1
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	clk := now
	require.False(t, tr.waitZero(50*time.Millisecond,
		func() time.Time { return clk },
		func(d time.Duration) { clk = clk.Add(d) }))
}

// gracefulRiskV2Shutdown nil 组件安全 + 幂等（无 panic / send-on-closed）。
func TestGracefulShutdown_NilSafeIdempotent(t *testing.T) {
	res := gracefulRiskV2Shutdown(context.Background(), nil, nil, nil, time.Second, time.Second, nil, nil, nil, nil)
	require.False(t, res.ForceClosed)
	// 再次调用不 panic。
	_ = gracefulRiskV2Shutdown(context.Background(), nil, nil, nil, time.Second, time.Second, nil, nil, nil, nil)
}
