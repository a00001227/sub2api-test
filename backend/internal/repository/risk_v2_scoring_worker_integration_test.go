package repository

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// 切片 4 真实集成：临时非生产 Redis 8 + PostgreSQL 18。
// 需同时设置 RISK_V2_TEST_PG_DSN 与 RISK_V2_TEST_REDIS_ADDR，否则 skip（POSTGRES/REDIS_INTEGRATION_UNVERIFIED）。

func workerITEnv(t *testing.T) (*sql.DB, *redis.Client) {
	t.Helper()
	dsn := os.Getenv("RISK_V2_TEST_PG_DSN")
	addr := os.Getenv("RISK_V2_TEST_REDIS_ADDR")
	if dsn == "" || addr == "" {
		t.Skip("set RISK_V2_TEST_PG_DSN and RISK_V2_TEST_REDIS_ADDR for worker integration")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	t.Cleanup(func() { _ = db.Close() })
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	require.NoError(t, rdb.Ping(context.Background()).Err())
	t.Cleanup(func() { _ = rdb.FlushAll(context.Background()); _ = rdb.Close() })
	return db, rdb
}

// 构造一个「写在 Tagg、读在 Tread」共享 rdb 的 cache（同时充当 ScoringReader）。
func itCache(rdb *redis.Client, clk func() time.Time) *riskV2AggCache {
	c := newRiskV2AggCacheWithClock(rdb, "s1", "v1", clk)
	c.SetScoringCycle(5*time.Minute, 2*time.Hour)
	return c
}

// mustReport 适配 sequenced Report 签名（无 build tag，unit 与非 unit 构建共用）。
func mustReport(t *testing.T, r service.RiskV2HealthReporter, seq uint64, d service.RiskV2HealthDelta) {
	t.Helper()
	_, err := r.Report(context.Background(), seq, d)
	require.NoError(t, err)
}

// itEnv 构造最小可用观测（与 unit 侧 okEnv 等价；此文件无 unit tag，故本地定义）。
func itEnv(server string, uid, kid int64) service.RiskFeatureEnvelope {
	return service.RiskFeatureEnvelope{
		ServerRequestID: server, UserID: uid, APIKeyID: kid,
		TerminalStatus: "ok", UsageAvailable: true, InputTokens: 10, OutputTokens: 20,
		ExactFingerprintAvailable: true, ExactHMAC: "v1:exact", ScaffoldHMAC: "v1:scaf",
		KeyVersion: "v1", FingerprintVersion: "fp-v2",
	}
}

func itParams() service.RiskV2WorkerParams {
	return service.RiskV2WorkerParams{
		Interval: 5 * time.Minute, GraceDelay: 20 * time.Second, CycleTimeout: 240 * time.Second,
		BatchSize: 50, MaxUsersPerSecond: 0, MaxUsersPerCycle: 10000,
		MaxReadErrorRatio: 0.5, MaxDBErrorRatio: 0.5,
		LeaseTTL: 30 * time.Second, LeaseRenewInterval: 10 * time.Second,
		MaxCatchupCycles: 3, AssessmentStaleAfter: time.Hour,
	}
}

// 端到端：聚合 → 周期活跃 → Worker persist 写入 user_risk_v2；重跑 NOOP。
func TestWorkerIT_PersistAndNoop(t *testing.T) {
	db, rdb := workerITEnv(t)
	setupSchema(t, db)
	ctx := context.Background()

	tAgg := time.Date(2026, 4, 1, 12, 1, 0, 0, time.UTC) // → cycle 12:05
	cache := itCache(rdb, func() time.Time { return tAgg })
	for _, uid := range []int64{9001, 9002, 9003} {
		require.NoError(t, cache.Aggregate(ctx, itEnv("it", uid, 1)))
	}

	tRun := time.Date(2026, 4, 1, 12, 5, 20, 0, time.UTC) // grace 20s → floor 12:05
	readCache := itCache(rdb, func() time.Time { return tRun })
	reporter := NewRiskV2HealthReporter(rdb, "s1", "v1", "inst-A", func() time.Time { return tRun })
	mustReport(t, reporter, 1, service.RiskV2HealthDelta{Enqueued: 100, Processed: 100})

	repo := NewUserRiskV2Repository(db)
	deps := service.RiskV2ScoringWorkerDeps{
		Reader: readCache, Repo: repo, Lease: NewRiskV2Lease(rdb, "s1", "v1"),
		Health:        NewRiskV2IngestionHealthReader(rdb, "s1", "v1", func() time.Time { return tRun }),
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, Now: func() time.Time { return tRun },
	}
	w := service.NewRiskV2ScoringWorker(deps, itParams())
	require.True(t, w.Ready())
	w.RunDueCycles(ctx)

	m := w.Metrics()
	require.EqualValues(t, 3, m.Inserted, "3 users persisted")
	require.EqualValues(t, 1, m.CyclesCompleted)

	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM user_risk_v2 WHERE user_id IN (9001,9002,9003)`).Scan(&n))
	require.Equal(t, 3, n)
	// assessed_at == cycle_end 12:05。
	var assessedAt int64
	require.NoError(t, db.QueryRow(`SELECT assessed_at FROM user_risk_v2 WHERE user_id=9001`).Scan(&assessedAt))
	require.Equal(t, time.Date(2026, 4, 1, 12, 5, 0, 0, time.UTC).Unix(), assessedAt)
	// EffectiveAction 恒 NONE。
	var action string
	require.NoError(t, db.QueryRow(`SELECT effective_action FROM user_risk_v2 WHERE user_id=9001`).Scan(&action))
	require.Equal(t, "NONE", action)

	// 重跑同一周期 → NOOP（无有意义写）。
	w2 := service.NewRiskV2ScoringWorker(deps, itParams())
	w2.RunDueCycles(ctx)
	require.EqualValues(t, 3, w2.Metrics().Noop, "re-run same cycle → NOOP")
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM user_risk_v2 WHERE user_id IN (9001,9002,9003)`).Scan(&n))
	require.Equal(t, 3, n)
}

// 旧周期 → STALE_IGNORED（先持久化新周期，再处理更旧周期）。
func TestWorkerIT_StaleIgnored(t *testing.T) {
	db, rdb := workerITEnv(t)
	setupSchema(t, db)
	ctx := context.Background()

	// 新周期 12:10 先写。
	tNew := time.Date(2026, 4, 1, 12, 6, 0, 0, time.UTC) // → cycle 12:10
	cNew := itCache(rdb, func() time.Time { return tNew })
	require.NoError(t, cNew.Aggregate(ctx, itEnv("new", 9001, 1)))
	repo := NewUserRiskV2Repository(db)
	tRunNew := time.Date(2026, 4, 1, 12, 10, 20, 0, time.UTC)
	depsNew := service.RiskV2ScoringWorkerDeps{
		Reader: itCache(rdb, func() time.Time { return tRunNew }), Repo: repo,
		Lease: NewRiskV2Lease(rdb, "s1", "v1"), Health: NewRiskV2IngestionHealthReader(rdb, "s1", "v1", func() time.Time { return tRunNew }),
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1", Persist: true,
		Now: func() time.Time { return tRunNew },
	}
	service.NewRiskV2ScoringWorker(depsNew, itParams()).RunDueCycles(ctx)

	var storedAt int64
	require.NoError(t, db.QueryRow(`SELECT assessed_at FROM user_risk_v2 WHERE user_id=9001`).Scan(&storedAt))
	require.Equal(t, time.Date(2026, 4, 1, 12, 10, 0, 0, time.UTC).Unix(), storedAt)

	// 处理更旧周期 12:05（活跃集也放该用户）。
	tOld := time.Date(2026, 4, 1, 12, 1, 0, 0, time.UTC) // → cycle 12:05
	cOld := itCache(rdb, func() time.Time { return tOld })
	require.NoError(t, cOld.Aggregate(ctx, itEnv("old", 9001, 1)))
	tRunOld := time.Date(2026, 4, 1, 12, 5, 20, 0, time.UTC)
	depsOld := depsNew
	depsOld.Reader = itCache(rdb, func() time.Time { return tRunOld })
	depsOld.Health = NewRiskV2IngestionHealthReader(rdb, "s1", "v1", func() time.Time { return tRunOld })
	depsOld.Now = func() time.Time { return tRunOld }
	wOld := service.NewRiskV2ScoringWorker(depsOld, itParams())
	wOld.RunDueCycles(ctx)
	require.EqualValues(t, 1, wOld.Metrics().StaleIgnored, "older cycle must be STALE_IGNORED")

	// 存储仍是新周期的 assessed_at（未被旧周期覆盖）。
	require.NoError(t, db.QueryRow(`SELECT assessed_at FROM user_risk_v2 WHERE user_id=9001`).Scan(&storedAt))
	require.Equal(t, time.Date(2026, 4, 1, 12, 10, 0, 0, time.UTC).Unix(), storedAt)
}

// 两个 Worker 竞争同一真实 Lease：预占后另一个必然 contended、不处理。
func TestWorkerIT_TwoWorkerLeaseContention(t *testing.T) {
	db, rdb := workerITEnv(t)
	setupSchema(t, db)
	ctx := context.Background()

	tAgg := time.Date(2026, 4, 1, 12, 1, 0, 0, time.UTC)
	cache := itCache(rdb, func() time.Time { return tAgg })
	require.NoError(t, cache.Aggregate(ctx, itEnv("it", 9001, 1)))

	// 预占 lease（模拟另一实例正持有）。
	holder := NewRiskV2Lease(rdb, "s1", "v1")
	tok, ok, err := holder.Acquire(ctx, 30*time.Second)
	require.NoError(t, err)
	require.True(t, ok)
	defer holder.Release(ctx, tok)

	tRun := time.Date(2026, 4, 1, 12, 5, 20, 0, time.UTC)
	deps := service.RiskV2ScoringWorkerDeps{
		Reader: itCache(rdb, func() time.Time { return tRun }), Repo: NewUserRiskV2Repository(db),
		Lease: NewRiskV2Lease(rdb, "s1", "v1"), Health: NewRiskV2IngestionHealthReader(rdb, "s1", "v1", func() time.Time { return tRun }),
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1", Persist: true,
		Now: func() time.Time { return tRun },
	}
	w := service.NewRiskV2ScoringWorker(deps, itParams())
	w.RunDueCycles(ctx)
	require.GreaterOrEqual(t, w.Metrics().LeaseContended, int64(1), "must be contended while other holds lease")

	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM user_risk_v2 WHERE user_id=9001`).Scan(&n))
	require.Equal(t, 0, n, "non-leader must not write")
}

// Legacy user_risk 表不受 Worker 影响。
func TestWorkerIT_LegacyUntouched(t *testing.T) {
	db, rdb := workerITEnv(t)
	setupSchema(t, db)
	ctx := context.Background()
	_, err := db.Exec(`INSERT INTO user_risk (user_id, score, tier) VALUES (9001, 42, 'watch')`)
	require.NoError(t, err)

	tAgg := time.Date(2026, 4, 1, 12, 1, 0, 0, time.UTC)
	cache := itCache(rdb, func() time.Time { return tAgg })
	require.NoError(t, cache.Aggregate(ctx, itEnv("it", 9001, 1)))
	tRun := time.Date(2026, 4, 1, 12, 5, 20, 0, time.UTC)
	deps := service.RiskV2ScoringWorkerDeps{
		Reader: itCache(rdb, func() time.Time { return tRun }), Repo: NewUserRiskV2Repository(db),
		Lease: NewRiskV2Lease(rdb, "s1", "v1"), Health: NewRiskV2IngestionHealthReader(rdb, "s1", "v1", func() time.Time { return tRun }),
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1", Persist: true,
		Now: func() time.Time { return tRun },
	}
	service.NewRiskV2ScoringWorker(deps, itParams()).RunDueCycles(ctx)

	var score int
	var tier string
	require.NoError(t, db.QueryRow(`SELECT score, tier FROM user_risk WHERE user_id=9001`).Scan(&score, &tier))
	require.Equal(t, 42, score)
	require.Equal(t, "watch", tier)
}
