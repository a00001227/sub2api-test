package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

// 切片 4.1 §十一：运行态接线的 Flag 模式矩阵（miniredis + 无表 sqlite）。
// 通过 provideRiskV2Runtime 驱动真实构造路径，验证「按 Flag 决定启动什么」。

func rtRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// 无 user_risk_v2 表的 sqlite（用于 schema-missing 路径）。
func rtDBNoSchema(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func rtCfg(enabled, agg, scoring, persist bool) *config.Config {
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

// §十一.1：所有 Flag 关闭 → 无 V2 运行态组件。
func TestRuntime_AllDisabled(t *testing.T) {
	loop, worker := provideRiskV2Runtime(rtCfg(false, false, false, false), rtRedis(t), nil,
		nil, service.RiskV2WorkerParamsFromConfig(config.RiskV2WorkerConfig{}))
	require.Nil(t, loop)
	require.Nil(t, worker)
}

// enabled 但 aggregation=false → 不启动 Reporter/Worker。
func TestRuntime_AggregationOff(t *testing.T) {
	loop, worker := provideRiskV2Runtime(rtCfg(true, false, false, false), rtRedis(t), nil,
		nil, service.RiskV2WorkerParamsFromConfig(config.RiskV2WorkerConfig{}))
	require.Nil(t, loop)
	require.Nil(t, worker)
}

// §十一.2：只 Aggregation → Reporter 启动、Worker 不启动。
func TestRuntime_AggregationOnlyStartsReporter(t *testing.T) {
	cfg := rtCfg(true, true, false, false)
	loop, worker := provideRiskV2Runtime(cfg, rtRedis(t), nil, nil,
		service.RiskV2WorkerParamsFromConfig(cfg.Risk.V2.Worker))
	require.NotNil(t, loop, "health reporter loop must start")
	require.Nil(t, worker, "scoring worker must NOT start when scoring disabled")
	loop.Stop()
}

// §十一.3：Scoring Dry-Run → Worker 启动、persist=false。
func TestRuntime_ScoringDryRunStartsWorker(t *testing.T) {
	cfg := rtCfg(true, true, true, false)
	loop, worker := provideRiskV2Runtime(cfg, rtRedis(t), nil, nil,
		service.RiskV2WorkerParamsFromConfig(cfg.Risk.V2.Worker))
	require.NotNil(t, loop)
	require.NotNil(t, worker, "dry-run worker must start without DB")
	require.True(t, worker.Ready())
	worker.Stop()
	loop.Stop()
}

// §十一.5 / §二.6 / §三：persist=true 但 Schema 不可用 → Worker 不启动（DEGRADED），Reporter 仍启动。
func TestRuntime_PersistSchemaMissingDegraded(t *testing.T) {
	cfg := rtCfg(true, true, true, true)
	loop, worker := provideRiskV2Runtime(cfg, rtRedis(t), rtDBNoSchema(t), nil,
		service.RiskV2WorkerParamsFromConfig(cfg.Risk.V2.Worker))
	require.NotNil(t, loop, "reporter still starts")
	require.Nil(t, worker, "persist worker must NOT start when schema not ready")
	loop.Stop()
}

// Stop 幂等（多次调用不 panic / 不 send-on-closed）。
func TestRuntime_StopIdempotent(t *testing.T) {
	cfg := rtCfg(true, true, true, false)
	loop, worker := provideRiskV2Runtime(cfg, rtRedis(t), nil, nil,
		service.RiskV2WorkerParamsFromConfig(cfg.Risk.V2.Worker))
	require.NotNil(t, worker)
	worker.Stop()
	worker.Stop() // 幂等
	loop.Stop()
	loop.Stop() // 幂等
	_ = context.Background()
}
