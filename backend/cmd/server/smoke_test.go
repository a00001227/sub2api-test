package main

import (
	"context"
	"database/sql"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

// 切片 4.2 §四：完整 Server 生命周期 Smoke Test。
// 走真实 Application 生命周期（ShutdownRiskV2 → Cleanup，与 main 完全一致）+ 真实 Risk V2 运行态组件
// （dispatcher/worker/reporter/lease/cyclestore on 临时 Redis；repo on 临时 PG）+ 真实 http.Server + mock handler。
// 不调用真实上游；不访问生产 Redis/DB。全 flags 门控。
//
// 说明：完整单体 initializeApplication 需要完整生产级配置 + ent 迁移，不适合在单测内引导；
// 本测试驱动真实 Application 生命周期与有序关闭/清理入口（非仅 provideRiskV2Runtime），并用 mock handler 产生观测。

// countingSink 计数被 dispatcher 消费的观测（验证在途请求产生的 Observation 被 drain）。
type countingSink struct{ n int64 }

func (s *countingSink) Consume(env service.RiskFeatureEnvelope) { atomic.AddInt64(&s.n, 1) }
func (s *countingSink) count() int64                            { return atomic.LoadInt64(&s.n) }

func smokeRedisAddr(t *testing.T) string {
	addr := os.Getenv("RISK_V2_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set RISK_V2_TEST_REDIS_ADDR for server smoke test")
	}
	return addr
}

func smokeCfg(enabled, agg, scoring, persist bool) *config.Config {
	cfg := &config.Config{}
	cfg.Risk.V2 = config.RiskV2Config{
		Enabled:               enabled,
		AggregationEnabled:    agg,
		ScoringEnabled:        scoring,
		ScoringPersistEnabled: persist,
		FingerprintHMACKey:    config.SecretString(strings.Repeat("k", 32)),
		FingerprintKeyVersion: "v1",
	}
	return cfg
}

// assembleSmokeApp 组装与 main 一致的 Application（真实 dispatcher + provideRiskV2Runtime + 两段式有序关闭 + cleanup）。
func assembleSmokeApp(t *testing.T, rdb *redis.Client, db *sql.DB, cfg *config.Config, handler http.Handler, graceful, force time.Duration) (*Application, *service.RiskV2Dispatcher, *countingSink) {
	t.Helper()
	sink := &countingSink{}
	var disp *service.RiskV2Dispatcher
	if cfg.Risk.V2.AggregationActive() {
		disp = service.NewRiskV2Dispatcher(64, sink)
		disp.Start()
	}
	params := service.RiskV2WorkerParamsFromConfig(cfg.Risk.V2.Worker)
	loop, worker := provideRiskV2Runtime(cfg, rdb, db, disp, params)

	tracker := &inflightTracker{}
	srv := &http.Server{Handler: tracker.Middleware(handler)}
	cleanup := func() {
		worker.Stop()
		if disp != nil {
			_ = disp.Stop(context.Background())
		}
		loop.Stop()
		if rdb != nil {
			_ = rdb.Close()
		}
		if db != nil {
			_ = db.Close()
		}
	}
	app := &Application{
		Server:  srv,
		Cleanup: cleanup,
		ShutdownRiskV2: func(ctx context.Context) ShutdownResult {
			return gracefulRiskV2Shutdown(ctx, worker, srv, tracker, graceful, force, disp, loop, nil, nil)
		},
	}
	return app, disp, sink
}

func startServer(t *testing.T, app *Application) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = app.Server.Serve(ln) }()
	return ln.Addr().String()
}

// §四.7：SIGTERM 时有在途请求 —— Listener 先关、请求在超时内完成、Observation 被 drain、Reporter flush、退出。
func TestSmoke_InFlightDrainOnShutdown(t *testing.T) {
	addr := smokeRedisAddr(t)
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	require.NoError(t, rdb.Ping(context.Background()).Err())

	var once sync.Once
	entered := make(chan struct{})
	var dispHolder *service.RiskV2Dispatcher
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		// 在途请求产生 Risk Observation：请求仍在处理中即入队（保证在 drain 之前进入队列）。
		dispHolder.Enqueue(service.RiskFeatureEnvelope{ServerRequestID: "inflight", UserID: 1, APIKeyID: 1, TerminalStatus: "ok"})
		time.Sleep(300 * time.Millisecond) // 模拟在途工作
		w.WriteHeader(http.StatusOK)
	})

	cfg := smokeCfg(true, true, true, false) // dry-run（无需 DB）
	app, disp, sink := assembleSmokeApp(t, rdb, nil, cfg, handler, 5*time.Second, time.Second)
	require.NotNil(t, disp)
	dispHolder = disp
	serverAddr := startServer(t, app)

	reqDone := make(chan int)
	go func() {
		resp, err := (&http.Client{}).Get("http://" + serverAddr + "/x")
		if err != nil {
			reqDone <- 0
			return
		}
		_ = resp.Body.Close()
		reqDone <- resp.StatusCode
	}()

	<-entered // 请求已进入 handler（观测已入队）

	// 触发与 main 一致的有序关闭（内部完成 HTTP drain）。
	res := app.ShutdownRiskV2(context.Background())
	require.False(t, res.Incomplete, "short request must drain within graceful timeout")

	// 在途请求应完成（200）。
	require.Equal(t, http.StatusOK, <-reqDone, "in-flight request must complete during drain")

	// Shutdown 开始后新请求不再被接受。
	_, err := (&http.Client{Timeout: time.Second}).Get("http://" + serverAddr + "/y")
	require.Error(t, err, "new requests must be refused after listener shutdown")

	// 在途请求产生的 Observation 已被 Dispatcher drain 消费。
	require.EqualValues(t, 1, sink.count(), "in-flight observation must be drained/consumed")

	// 最后关闭基础设施（Redis）。
	app.Cleanup()
	require.Error(t, rdb.Ping(context.Background()).Err(), "redis closed last in cleanup")
}

// §一.5：请求超过 graceful timeout → 记录 timeout + Server.Close 强制断连；被取消的请求计 cancelled；
// Handler 在第二段 force timeout 内退出（Incomplete=false）；Dispatcher 在 Handler 之后才停。
func TestSmoke_ForcedCloseOnGracefulTimeout(t *testing.T) {
	addr := smokeRedisAddr(t)
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	require.NoError(t, rdb.Ping(context.Background()).Err())

	var once sync.Once
	entered := make(chan struct{})
	exited := make(chan struct{})
	var dispHolder *service.RiskV2Dispatcher
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		select {
		case <-r.Context().Done():
			// 被强制取消：明确产生 cancelled terminal observation。
			dispHolder.Enqueue(service.RiskFeatureEnvelope{ServerRequestID: "forced", UserID: 1, APIKeyID: 1, TerminalStatus: "cancelled"})
		case <-time.After(3 * time.Second):
		}
		close(exited)
	})

	cfg := smokeCfg(true, true, true, false)
	// graceful 很短（200ms）→ 触发强制 Close；force 2s 足够 Handler 感知取消并退出。
	app, disp, sink := assembleSmokeApp(t, rdb, nil, cfg, handler, 200*time.Millisecond, 2*time.Second)
	require.NotNil(t, disp)
	dispHolder = disp
	serverAddr := startServer(t, app)

	go func() { _, _ = (&http.Client{}).Get("http://" + serverAddr + "/slow") }()
	<-entered

	res := app.ShutdownRiskV2(context.Background())
	require.True(t, res.HTTPGracefulTimedOut, "must record graceful timeout")
	require.True(t, res.ForceClosed, "must force Server.Close on timeout")
	require.False(t, res.Incomplete, "handler must exit within force timeout")

	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not exit after forced close")
	}
	require.EqualValues(t, 1, sink.count(), "cancelled observation consumed before dispatcher stop")
	app.Cleanup()
}

// §四.1：全部 Flag 关闭 —— Server 正常起停，无 V2 组件。
func TestSmoke_AllDisabledLifecycle(t *testing.T) {
	addr := smokeRedisAddr(t)
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	cfg := smokeCfg(false, false, false, false)
	app, disp, _ := assembleSmokeApp(t, rdb, nil, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }), 3*time.Second, time.Second)
	require.Nil(t, disp)
	serverAddr := startServer(t, app)
	resp, err := (&http.Client{Timeout: time.Second}).Get("http://" + serverAddr + "/health")
	require.NoError(t, err)
	_ = resp.Body.Close()
	_ = app.ShutdownRiskV2(context.Background())
	app.Cleanup()
}

// §四.6：Redis 不可用 —— 主 Server 正常提供服务；Risk V2 不拖垮主业务；无快速重试循环。
func TestSmoke_RedisUnavailable(t *testing.T) {
	// 指向一个不可达地址（不 Ping；provideRiskV2Runtime 不阻塞启动）。
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond})
	cfg := smokeCfg(true, true, true, false)
	app, disp, _ := assembleSmokeApp(t, rdb, nil, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }), 3*time.Second, time.Second)
	require.NotNil(t, disp)
	serverAddr := startServer(t, app)
	// 主业务 HTTP 正常（Risk V2 的 Redis 故障不影响）。
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + serverAddr + "/health")
	require.NoError(t, err, "main HTTP must serve even when Risk V2 Redis is down")
	require.Equal(t, 200, resp.StatusCode)
	_ = resp.Body.Close()
	_ = app.ShutdownRiskV2(context.Background())
	app.Cleanup()
}

// §四.5：persist 但 Schema 缺失 —— Worker DEGRADED，HTTP 仍正常提供非 V2 服务。
func TestSmoke_PersistSchemaMissing(t *testing.T) {
	addr := smokeRedisAddr(t)
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	db, err := sql.Open("sqlite", ":memory:") // 无 user_risk_v2 表
	require.NoError(t, err)
	cfg := smokeCfg(true, true, true, true)
	app, disp, _ := assembleSmokeApp(t, rdb, db, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }), 3*time.Second, time.Second)
	require.NotNil(t, disp)
	serverAddr := startServer(t, app)
	resp, err := (&http.Client{Timeout: time.Second}).Get("http://" + serverAddr + "/health")
	require.NoError(t, err, "HTTP serves non-V2 traffic even when persist schema missing")
	_ = resp.Body.Close()
	_ = app.ShutdownRiskV2(context.Background())
	app.Cleanup()
}
