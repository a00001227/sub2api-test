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

func iv5() int64 { return int64((5 * time.Minute).Seconds()) }

// §四：周期归属 = ceil(t/interval)*interval（下一个已完成周期）。
func TestCycleEnd_Assignment(t *testing.T) {
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC).Unix()
	// 12:01 → 12:05
	require.Equal(t, base+300, riskV2CycleEnd(base+60, iv5()))
	// 12:06 → 12:10
	require.Equal(t, base+600, riskV2CycleEnd(base+360, iv5()))
	// 边界 12:05:00 → 12:05
	require.Equal(t, base+300, riskV2CycleEnd(base+300, iv5()))
}

// §四：普通周期 —— 观测落入正确的 active:c 集合，可被游标读出。
func TestCycleActive_NormalCycle(t *testing.T) {
	fixed := time.Date(2026, 3, 1, 12, 1, 30, 0, time.UTC) // → cycle 12:05
	c, _, _ := newSpyCache(t, func() time.Time { return fixed })
	ctx := context.Background()
	require.NoError(t, c.Aggregate(ctx, okEnv("r1", 7, 1)))
	cycleEnd := riskV2CycleEnd(fixed.Unix(), iv5())
	page, err := c.ListActiveUserIDsForCycle(ctx, cycleEnd, "", 100)
	require.NoError(t, err)
	require.Equal(t, []int64{7}, page.UserIDs)
}

// §四：12:59 → 13:00 小时边界，周期跨小时仍正确归属。
func TestCycleActive_HourBoundary(t *testing.T) {
	fixed := time.Date(2026, 3, 1, 12, 59, 30, 0, time.UTC) // → cycle 13:00
	c, _, _ := newSpyCache(t, func() time.Time { return fixed })
	ctx := context.Background()
	require.NoError(t, c.Aggregate(ctx, okEnv("r1", 42, 1)))
	cycleEnd := riskV2CycleEnd(fixed.Unix(), iv5())
	require.Equal(t, time.Date(2026, 3, 1, 13, 0, 0, 0, time.UTC).Unix(), cycleEnd)
	page, err := c.ListActiveUserIDsForCycle(ctx, cycleEnd, "", 100)
	require.NoError(t, err)
	require.Equal(t, []int64{42}, page.UserIDs)
}

// §四：长流式请求晚到 —— 12:01 完成的进 12:05，12:06 才完成的进 12:10。
func TestCycleActive_LateStreamNextCycle(t *testing.T) {
	// 同一用户两次观测（模拟先短后长流），分别在 12:01 与 12:06 聚合完成。
	clk := newManualClock(time.Date(2026, 3, 1, 12, 1, 0, 0, time.UTC))
	c, _, _ := newSpyCache(t, clk.now)
	ctx := context.Background()
	require.NoError(t, c.Aggregate(ctx, okEnv("early", 7, 1)))
	clk.advance(5 * time.Minute) // → 12:06
	require.NoError(t, c.Aggregate(ctx, okEnv("late", 7, 1)))

	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC).Unix()
	// 12:05 周期含该用户。
	p1, _ := c.ListActiveUserIDsForCycle(ctx, base+300, "", 100)
	require.Equal(t, []int64{7}, p1.UserIDs)
	// 12:10 周期也含该用户（晚到观测不漏评分）。
	p2, _ := c.ListActiveUserIDsForCycle(ctx, base+600, "", 100)
	require.Equal(t, []int64{7}, p2.UserIDs)
}

// §四：同一用户一周期只出现一次（ZADD 幂等）。
func TestCycleActive_UserOncePerCycle(t *testing.T) {
	fixed := time.Date(2026, 3, 1, 12, 1, 30, 0, time.UTC)
	c, _, _ := newSpyCache(t, func() time.Time { return fixed })
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		require.NoError(t, c.Aggregate(ctx, okEnv("r", 7, 1)))
	}
	cycleEnd := riskV2CycleEnd(fixed.Unix(), iv5())
	page, err := c.ListActiveUserIDsForCycle(ctx, cycleEnd, "", 100)
	require.NoError(t, err)
	require.Equal(t, []int64{7}, page.UserIDs, "user must appear exactly once per cycle")
}

// §四：不同 fingerprint epoch 隔离。
func TestCycleActive_FingerprintEpochIsolation(t *testing.T) {
	fixed := time.Date(2026, 3, 1, 12, 1, 30, 0, time.UTC)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	cV1 := newRiskV2AggCacheWithClock(rdb, "s1", "v1", func() time.Time { return fixed })
	cV2 := newRiskV2AggCacheWithClock(rdb, "s1", "v2", func() time.Time { return fixed })
	ctx := context.Background()
	require.NoError(t, cV1.Aggregate(ctx, okEnv("r", 7, 1)))
	cycleEnd := riskV2CycleEnd(fixed.Unix(), iv5())
	p1, _ := cV1.ListActiveUserIDsForCycle(ctx, cycleEnd, "", 100)
	require.Equal(t, []int64{7}, p1.UserIDs)
	p2, _ := cV2.ListActiveUserIDsForCycle(ctx, cycleEnd, "", 100)
	require.Empty(t, p2.UserIDs, "v2 epoch must not see v1 cycle-active users")
}

// §四：周期集合 TTL 存在（不会永久残留）。
func TestCycleActive_TTL(t *testing.T) {
	fixed := time.Date(2026, 3, 1, 12, 1, 30, 0, time.UTC)
	c, mr, _ := newSpyCache(t, func() time.Time { return fixed })
	ctx := context.Background()
	require.NoError(t, c.Aggregate(ctx, okEnv("r", 7, 1)))
	cycleEnd := riskV2CycleEnd(fixed.Unix(), iv5())
	key := "riskv2:s1:fp:v1:active:c:" + i64toa(cycleEnd)
	ttl := mr.TTL(key)
	require.Greater(t, ttl, time.Duration(0), "cycle-active set must have a TTL")
}

// §十二.1：V2 动态关闭 → RunDueCycles 不读不写。
func TestWorker_DynamicallyDisabled(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 6, 5, 0, time.UTC)
	reader := newFakeReader()
	repo := newFakeRepo()
	p := defaultWorkerParams()
	cyc := baseCycleEnd(now, p)
	reader.setCycle(cyc, []int64{1, 2, 3})
	w := service.NewRiskV2ScoringWorker(service.RiskV2ScoringWorkerDeps{
		Reader: reader, Lease: &fakeLease{}, Health: healthyHealth(), Repo: repo,
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, Now: func() time.Time { return now },
		EnabledFn: func() bool { return false }, // 动态关闭
	}, p)
	w.RunDueCycles(context.Background())
	require.Equal(t, 0, repo.count())
	require.EqualValues(t, 0, reader.listCalls)
	require.EqualValues(t, 0, w.Metrics().CyclesStarted)
}

// §十三：延迟/容量 —— 注入 5/20/50ms reader 延迟，测 100 用户周期耗时并外推 1k/10k。
func TestWorker_LatencyAndCapacity(t *testing.T) {
	for _, delay := range []time.Duration{5 * time.Millisecond, 20 * time.Millisecond, 50 * time.Millisecond} {
		delay := delay
		t.Run(delay.String(), func(t *testing.T) {
			now := time.Date(2026, 3, 1, 12, 6, 5, 0, time.UTC)
			reader := newFakeReader()
			reader.delay = delay
			repo := newFakeRepo()
			p := defaultWorkerParams()
			p.MaxUsersPerSecond = 0 // 测原始读取吞吐（pacing 单独测）
			p.BatchSize = 100
			p.CycleTimeout = 240 * time.Second
			cyc := baseCycleEnd(now, p)
			users := make([]int64, 100)
			for i := range users {
				users[i] = int64(i + 1)
			}
			reader.setCycle(cyc, users)
			w := service.NewRiskV2ScoringWorker(service.RiskV2ScoringWorkerDeps{
				Reader: reader, Lease: &fakeLease{}, Health: healthyHealth(), Repo: repo,
				ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
				Persist: true, Now: func() time.Time { return now },
			}, p)
			start := time.Now()
			w.RunDueCycles(context.Background())
			elapsed := time.Since(start)
			require.EqualValues(t, 1, w.Metrics().CyclesCompleted)
			require.Equal(t, 100, repo.count())
			// 100 用户 1 批 → 约 1×delay（+评分开销）。外推：读取分批线性。
			batches100 := 1.0
			per := elapsed
			est1k := time.Duration(float64(per) * 10)
			est10k := time.Duration(float64(per) * 100)
			t.Logf("delay=%s: 100-user cycle=%s (batches=%.0f); est 1k=%s, 10k=%s",
				delay, elapsed.Round(time.Millisecond), batches100,
				est1k.Round(time.Millisecond), est10k.Round(time.Millisecond))
		})
	}
}

// §十三：部分用户错误下仍完成周期（不整批失败）。
func TestWorker_LatencyPartialErrors(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 6, 5, 0, time.UTC)
	reader := newFakeReader()
	reader.delay = 10 * time.Millisecond
	repo := newFakeRepo()
	p := defaultWorkerParams()
	p.MaxUsersPerSecond = 0
	cyc := baseCycleEnd(now, p)
	users := make([]int64, 30)
	for i := range users {
		users[i] = int64(i + 1)
	}
	reader.setCycle(cyc, users)
	reader.userErr[5] = context.DeadlineExceeded // 单用户错误（<50% 阈值）
	reader.userErr[10] = context.DeadlineExceeded
	w := service.NewRiskV2ScoringWorker(service.RiskV2ScoringWorkerDeps{
		Reader: reader, Lease: &fakeLease{}, Health: healthyHealth(), Repo: repo,
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, Now: func() time.Time { return now },
	}, p)
	w.RunDueCycles(context.Background())
	require.EqualValues(t, 1, w.Metrics().CyclesCompleted)
	require.Equal(t, 28, repo.count()) // 30 - 2 errored
}
