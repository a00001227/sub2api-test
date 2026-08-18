package main

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 切片 4.2 / checkpoint 前修正：可配置的两段式 HTTP 优雅关闭 + in-flight tracker + Risk V2 有序停机。

// inflightTracker 统计「已进入 Handler 且尚未返回」的请求数，用于强制 Close 后确认 Handler 真正退出。
type inflightTracker struct{ n int64 }

// Middleware 包裹业务 handler：进入 +1、返回 -1（含 panic 也 -1）。
func (t *inflightTracker) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&t.n, 1)
		defer atomic.AddInt64(&t.n, -1)
		next.ServeHTTP(w, r)
	})
}

func (t *inflightTracker) inflight() int64 { return atomic.LoadInt64(&t.n) }

// waitZero 在 timeout 内轮询等待在途归零。返回是否归零。now/sleep 注入以便测试。
func (t *inflightTracker) waitZero(timeout time.Duration, now func() time.Time, sleep func(time.Duration)) bool {
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	deadline := now().Add(timeout)
	for {
		if atomic.LoadInt64(&t.n) <= 0 {
			return true
		}
		if !now().Before(deadline) {
			return atomic.LoadInt64(&t.n) <= 0
		}
		sleep(10 * time.Millisecond)
	}
}

// ShutdownResult 是关闭结果（可观测指标：是否超时 / 强制断连 / 未完成）。
type ShutdownResult struct {
	HTTPGracefulTimedOut bool // 优雅超时到期（触发强制 Close）
	ForceClosed          bool // 执行了 Server.Close
	Incomplete           bool // 第二段超时后仍有 Handler 未退出（shutdown_incomplete）
}

// httpDrain 两段式 HTTP 关闭：
//  1. Server.Shutdown 停 Listener + 等待在途请求在 graceful 超时内完成；
//  2. 超时未完 → 记录 timeout、Server.Close 强制断连、在 force 超时内等待 Handler tracker 归零；
//     仍未归零 → Incomplete=true（不无界等待）。
func httpDrain(ctx context.Context, srv *http.Server, tracker *inflightTracker, graceful, force time.Duration,
	now func() time.Time, sleep func(time.Duration)) ShutdownResult {
	var res ShutdownResult
	if srv == nil {
		return res
	}
	sctx, cancel := context.WithTimeout(ctx, graceful)
	err := srv.Shutdown(sctx)
	cancel()
	if err == nil {
		return res // 所有在途请求在优雅期内完成
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		// 其它错误（罕见）：也走强制路径确保有界收尾。
		res.HTTPGracefulTimedOut = true
	} else {
		res.HTTPGracefulTimedOut = true
	}
	// 强制断连 + 第二段有界等待 Handler 归零。
	_ = srv.Close()
	res.ForceClosed = true
	if tracker != nil {
		if zeroed := tracker.waitZero(force, now, sleep); !zeroed {
			res.Incomplete = true
		}
	}
	return res
}

// gracefulRiskV2Shutdown 有序优雅关闭（§一，全部 nil-safe）：
//
//  1. Scoring Worker 停止开启新周期；
//  2. HTTP 两段式 drain（Listener 停 → 在途完成 / 超时强制 Close → 等 Handler 归零）；
//  3. **仅在 Handler 已退出或最终 timeout 后**才停 Dispatcher（排空队列）；
//  4. Health Reporter final flush + deregister + stop；
//  5. 之后由 Cleanup 关闭 Redis/DB（最后）。
func gracefulRiskV2Shutdown(
	ctx context.Context,
	worker *service.RiskV2ScoringWorker,
	srv *http.Server,
	tracker *inflightTracker,
	graceful, force time.Duration,
	dispatcher *service.RiskV2Dispatcher,
	healthLoop *service.RiskV2HealthReportLoop,
	now func() time.Time, sleep func(time.Duration),
) ShutdownResult {
	// 1) 先停 Worker：不再开启新评分周期。
	worker.Stop() // nil-safe

	// 2) 两段式 HTTP drain。
	res := httpDrain(ctx, srv, tracker, graceful, force, now, sleep)

	// 3) Handler 已退出（或最终 timeout）后才停 Dispatcher，排空在途请求投递的观测。
	if dispatcher != nil {
		_ = dispatcher.Stop(ctx)
	}

	// 4) Health Reporter：final flush + deregister + stop。
	healthLoop.Stop() // nil-safe

	return res
}
