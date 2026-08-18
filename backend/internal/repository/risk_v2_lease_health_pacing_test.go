//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newLeaseRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// §六：两个 Worker 竞争，只有一个拿到 lease；owner-only renew/release。
func TestLease_ContentionAndOwnerOnly(t *testing.T) {
	rdb := newLeaseRedis(t)
	ctx := context.Background()
	l1 := NewRiskV2Lease(rdb, "s1", "v1")
	l2 := NewRiskV2Lease(rdb, "s1", "v1")

	tok1, ok1, err := l1.Acquire(ctx, 30*time.Second)
	require.NoError(t, err)
	require.True(t, ok1)

	_, ok2, err := l2.Acquire(ctx, 30*time.Second)
	require.NoError(t, err)
	require.False(t, ok2, "second acquire must fail while held")

	// owner renew ok。
	ok, err := l1.Renew(ctx, tok1, 30*time.Second)
	require.NoError(t, err)
	require.True(t, ok)

	// 非 owner（错误 token）renew/release 无效，不得动他人 lease。
	ok, err = l2.Renew(ctx, "wrong-token", 30*time.Second)
	require.NoError(t, err)
	require.False(t, ok)
	require.NoError(t, l2.Release(ctx, "wrong-token"))
	// l1 仍持有 → l2 仍拿不到。
	_, ok2b, _ := l2.Acquire(ctx, 30*time.Second)
	require.False(t, ok2b)

	// owner release → 他人可获取。
	require.NoError(t, l1.Release(ctx, tok1))
	_, ok2c, err := l2.Acquire(ctx, 30*time.Second)
	require.NoError(t, err)
	require.True(t, ok2c)
}

// §六：lease 过期后（TTL 到）他人可获取（miniredis FastForward 模拟 crash 后自动过期）。
func TestLease_ExpiryAfterCrash(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	l1 := NewRiskV2Lease(rdb, "s1", "v1")
	l2 := NewRiskV2Lease(rdb, "s1", "v1")

	_, ok, err := l1.Acquire(ctx, 2*time.Second)
	require.NoError(t, err)
	require.True(t, ok)
	// 模拟 l1 崩溃不 renew：TTL 到期。
	mr.FastForward(3 * time.Second)
	_, ok2, err := l2.Acquire(ctx, 2*time.Second)
	require.NoError(t, err)
	require.True(t, ok2, "lease must auto-expire after TTL so another worker can take over")
}

// §八：health 上报 + 聚合。
func TestHealth_ReportAndAggregate(t *testing.T) {
	fixed := time.Date(2026, 3, 1, 12, 5, 0, 0, time.UTC)
	rdb := newLeaseRedis(t)
	ctx := context.Background()
	reporter := NewRiskV2HealthReporter(rdb, "s1", "v1", "inst-A", func() time.Time { return fixed })
	mustReport(t, reporter, 1, service.RiskV2HealthDelta{Enqueued: 100, Processed: 98, Dropped: 2})

	reader := NewRiskV2IngestionHealthReader(rdb, "s1", "v1", func() time.Time { return fixed })
	// cycleEnd 与上报同一分钟窗口。
	h, err := reader.ReadIngestionHealth(ctx, fixed.Unix(), 5)
	require.NoError(t, err)
	require.True(t, h.HealthAvailable)
	require.True(t, h.ObservationDropRatioAvailable)
	require.InDelta(t, 0.02, h.ObservationDropRatio, 1e-9)
	require.True(t, h.AggregationHealthy) // drop 2% <= 5%
}

// §二：Redis 层幂等 —— 同一 seq 重复 Report 只累加一次（模拟 ambiguous ACK 下的重试）。
func TestHealth_SequencedIdempotentRedis(t *testing.T) {
	fixed := time.Date(2026, 3, 1, 12, 5, 0, 0, time.UTC)
	rdb := newLeaseRedis(t)
	ctx := context.Background()
	reporter := NewRiskV2HealthReporter(rdb, "s1", "v1", "inst-A", func() time.Time { return fixed })

	res, err := reporter.Report(ctx, 1, service.RiskV2HealthDelta{Enqueued: 100, Processed: 100})
	require.NoError(t, err)
	require.Equal(t, service.RiskV2HealthApplied, res)
	// 同 seq 重试 → ALREADY_APPLIED，Redis 计数不再增加。
	res, err = reporter.Report(ctx, 1, service.RiskV2HealthDelta{Enqueued: 100, Processed: 100})
	require.NoError(t, err)
	require.Equal(t, service.RiskV2HealthAlreadyApplied, res)

	reader := NewRiskV2IngestionHealthReader(rdb, "s1", "v1", func() time.Time { return fixed })
	h, err := reader.ReadIngestionHealth(ctx, fixed.Unix(), 5)
	require.NoError(t, err)
	require.True(t, h.HealthAvailable)
	// 只累加一次 → drop ratio 基于 100 enqueued（若累加两次会变 0/200 等异常）。
	require.InDelta(t, 0.0, h.ObservationDropRatio, 1e-9)
	// 新 seq 才继续累加。
	res, err = reporter.Report(ctx, 2, service.RiskV2HealthDelta{Enqueued: 50})
	require.NoError(t, err)
	require.Equal(t, service.RiskV2HealthApplied, res)
}

// §八：无活跃实例 → HealthAvailable=false（不假设健康）。
func TestHealth_NoInstancesUnavailable(t *testing.T) {
	fixed := time.Date(2026, 3, 1, 12, 5, 0, 0, time.UTC)
	rdb := newLeaseRedis(t)
	reader := NewRiskV2IngestionHealthReader(rdb, "s1", "v1", func() time.Time { return fixed })
	h, err := reader.ReadIngestionHealth(context.Background(), fixed.Unix(), 5)
	require.NoError(t, err)
	require.False(t, h.HealthAvailable)
}

// §八：高丢弃率 → AggregationHealthy=false（供评分阻止 HIGH）。
func TestHealth_HighDropUnhealthy(t *testing.T) {
	fixed := time.Date(2026, 3, 1, 12, 5, 0, 0, time.UTC)
	rdb := newLeaseRedis(t)
	ctx := context.Background()
	reporter := NewRiskV2HealthReporter(rdb, "s1", "v1", "inst-A", func() time.Time { return fixed })
	mustReport(t, reporter, 1, service.RiskV2HealthDelta{Enqueued: 100, Processed: 50, Dropped: 50})
	reader := NewRiskV2IngestionHealthReader(rdb, "s1", "v1", func() time.Time { return fixed })
	h, err := reader.ReadIngestionHealth(ctx, fixed.Unix(), 5)
	require.NoError(t, err)
	require.True(t, h.HealthAvailable)
	require.False(t, h.AggregationHealthy)
	require.InDelta(t, 0.5, h.ObservationDropRatio, 1e-9)
}

// §八：多实例聚合。
func TestHealth_MultiInstanceAggregate(t *testing.T) {
	fixed := time.Date(2026, 3, 1, 12, 5, 0, 0, time.UTC)
	rdb := newLeaseRedis(t)
	ctx := context.Background()
	rA := NewRiskV2HealthReporter(rdb, "s1", "v1", "inst-A", func() time.Time { return fixed })
	rB := NewRiskV2HealthReporter(rdb, "s1", "v1", "inst-B", func() time.Time { return fixed })
	mustReport(t, rA, 1, service.RiskV2HealthDelta{Enqueued: 100, Processed: 100})
	mustReport(t, rB, 1, service.RiskV2HealthDelta{Enqueued: 100, Processed: 100, Dropped: 4})
	reader := NewRiskV2IngestionHealthReader(rdb, "s1", "v1", func() time.Time { return fixed })
	h, err := reader.ReadIngestionHealth(ctx, fixed.Unix(), 5)
	require.NoError(t, err)
	require.True(t, h.HealthAvailable)
	require.InDelta(t, 4.0/200.0, h.ObservationDropRatio, 1e-9)
}

// §八：fingerprint epoch 隔离（v1 上报不进 v2 聚合）。
func TestHealth_FingerprintEpochIsolation(t *testing.T) {
	fixed := time.Date(2026, 3, 1, 12, 5, 0, 0, time.UTC)
	rdb := newLeaseRedis(t)
	ctx := context.Background()
	r1 := NewRiskV2HealthReporter(rdb, "s1", "v1", "inst-A", func() time.Time { return fixed })
	mustReport(t, r1, 1, service.RiskV2HealthDelta{Enqueued: 100, Processed: 100})
	reader2 := NewRiskV2IngestionHealthReader(rdb, "s1", "v2", func() time.Time { return fixed })
	h, err := reader2.ReadIngestionHealth(ctx, fixed.Unix(), 5)
	require.NoError(t, err)
	require.False(t, h.HealthAvailable, "v2 epoch must not see v1 health")
}

// §七：Token Bucket 节流 —— 50/s 下处理 N 用户耗时约 (N/50)s（用注入时钟确定性验证）。
func TestPacer_SmoothRate(t *testing.T) {
	clk := newManualClock(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	pacer := service.NewRiskV2Pacer(50, clk.now, clk.sleep)
	start := clk.now()
	ctx := context.Background()
	// 请求 250 个令牌（=250 用户），空桶起，速率 50/s → 约 5s。
	total := 0
	for total < 250 {
		require.NoError(t, pacer.WaitN(ctx, 50))
		total += 50
	}
	elapsed := clk.now().Sub(start)
	// 250/50 = 5s；容量=50（1s 突发）→ 首个 50 需等 1s，故约 4~5s。放宽断言：>=4s。
	require.GreaterOrEqual(t, elapsed, 4*time.Second, "pacing must spread reads (got %s)", elapsed)
	require.LessOrEqual(t, elapsed, 6*time.Second)
}

// §七：rate<=0 → 不节流。
func TestPacer_Disabled(t *testing.T) {
	clk := newManualClock(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	pacer := service.NewRiskV2Pacer(0, clk.now, clk.sleep)
	start := clk.now()
	require.NoError(t, pacer.WaitN(context.Background(), 1000))
	require.Equal(t, time.Duration(0), clk.now().Sub(start))
}

// §七：ctx 取消 → WaitN 返回 ctx.Err。
func TestPacer_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pacer := service.NewRiskV2Pacer(1, time.Now, nil)
	require.Error(t, pacer.WaitN(ctx, 1000))
}
