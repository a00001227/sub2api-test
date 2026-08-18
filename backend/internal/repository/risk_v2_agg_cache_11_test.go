//go:build unit

package repository

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func fullScaffoldEnv(server string, uid, kid int64, scaffold string) service.RiskFeatureEnvelope {
	e := okEnv(server, uid, kid)
	e.ScaffoldHMAC = scaffold
	e.ScaffoldFingerprintSampled = false
	return e
}

// §一 原子有界 Set：多 goroutine + 多 client（模拟多实例）并发准入，基数绝不超过 cap，且置 overflow。
func TestAgg_AtomicBoundedSetConcurrent(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb1 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	rdb2 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb1.Close(); _ = rdb2.Close() })

	const cap = 20
	set := "riskv2:s1:{u:1}:fp:v1:m:100:xe"
	counter := "riskv2:s1:{u:1}:fp:v1:m:100:c"
	ctx := context.Background()

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rdb := rdb1
			if g%2 == 1 {
				rdb = rdb2
			}
			for i := 0; i < 50; i++ {
				rdb.Eval(ctx, boundedSAddSrc, []string{set, counter},
					fmt.Sprintf("m-%d-%d", g, i), cap, 3600, "ov_xe", "ovc_xe")
			}
		}(g)
	}
	wg.Wait()

	card, err := rdb1.SCard(ctx, set).Result()
	require.NoError(t, err)
	require.LessOrEqual(t, card, int64(cap), "bounded set must never exceed cap under concurrency")
	require.Equal(t, "1", rdb1.HGet(ctx, counter, "ov_xe").Val(), "overflow flag must be set")
	ovc, _ := rdb1.HGet(ctx, counter, "ovc_xe").Int64()
	require.Positive(t, ovc, "overflow_count must be incremented")
}

// §二 Full/Sampled Scaffold 分离 + Expansion 只用完整。
func TestAgg_FullVsSampledScaffold(t *testing.T) {
	c, _, _ := newAggTest(t)
	// 2 个完整 scaffold（不同）+ 1 个采样 scaffold。
	require.NoError(t, c.Aggregate(context.Background(), fullScaffoldEnv("r1", 7, 3, "v1:full-A")))
	require.NoError(t, c.Aggregate(context.Background(), fullScaffoldEnv("r2", 7, 3, "v1:full-B")))
	e := okEnv("r3", 7, 3)
	e.ScaffoldHMAC = "v1:sampled-X"
	e.ScaffoldFingerprintSampled = true
	require.NoError(t, c.Aggregate(context.Background(), e))

	m, _ := c.ReadForAdminDetail(context.Background(), 7)
	w := m.User.W5m
	require.EqualValues(t, 2, w.FullScaffoldRequestCount)
	require.Equal(t, 2, w.DistinctFullScaffoldEstimate)
	require.EqualValues(t, 1, w.SampledScaffoldRequestCount)
	require.Equal(t, 1, w.DistinctSampledScaffoldEstimate)
	// Expansion 用完整 exact(1, okEnv 全用同一 "v1:exact") / 完整 scaffold(2) = 0.5。
	exp, ok := w.ScaffoldExpansionEstimate()
	require.True(t, ok)
	require.InDelta(t, 0.5, exp, 0.001)
	sr, ok := w.SampledScaffoldRatio()
	require.True(t, ok)
	require.InDelta(t, 1.0/3.0, sr, 0.001)
}

// §三 Cache 正确分母 + 混合 Provider（anthropic 计入分母，openai 不污染）。
func TestAgg_CacheDenominatorMixedProvider(t *testing.T) {
	c, _, _ := newAggTest(t)
	cc, cr := int64(100), int64(400)
	// anthropic：applicable+available，obs_in=10（okEnv InputTokens），creation=100，read=400。
	a := okEnv("r1", 7, 3)
	a.CacheUsageApplicable, a.CacheUsageAvailable = true, true
	a.CacheCreationTokens, a.CacheReadTokens = &cc, &cr
	require.NoError(t, c.Aggregate(context.Background(), a))
	// openai：非 applicable，但有大量 input tokens → 不得进入 cache 分母。
	o := okEnv("r2", 7, 3)
	o.CacheUsageApplicable = false
	o.InputTokens = 99999
	require.NoError(t, c.Aggregate(context.Background(), o))

	w := mustRead(t, c, 7).User.W5m
	// cache_total_input = obs_in(10) + creation(100) + read(400) = 510；openai 的 99999 不在内。
	rr, ok := w.CacheReadRatio()
	require.True(t, ok)
	require.InDelta(t, 400.0/510.0, rr, 0.001)
	nv, ok := w.NovelInputRatio()
	require.True(t, ok)
	require.InDelta(t, 110.0/510.0, nv, 0.001)
	require.EqualValues(t, 10, w.CacheObservedInputTokens, "only applicable&available input enters cache observed")
}

func TestAgg_CacheUnavailableWhenNoApplicable(t *testing.T) {
	c, _, _ := newAggTest(t)
	o := okEnv("r1", 7, 3)
	o.CacheUsageApplicable = false
	require.NoError(t, c.Aggregate(context.Background(), o))
	_, ok := mustRead(t, c, 7).User.W5m.CacheReadRatio()
	require.False(t, ok, "no cache_total_input → unavailable, not 0")
}

// §六 多 API Key 汇总：跨 key 完整 scaffold overlap + 同步分钟 + ActiveAPIKeyCount。
func TestAgg_MultiKeyRollup(t *testing.T) {
	c, _, _ := newAggTest(t)
	// 同一 full scaffold "v1:shared" 出现在 key 3 与 key 4（同一分钟）→ overlap≥1, sync≥1。
	require.NoError(t, c.Aggregate(context.Background(), fullScaffoldEnv("r1", 7, 3, "v1:shared")))
	require.NoError(t, c.Aggregate(context.Background(), fullScaffoldEnv("r2", 7, 4, "v1:shared")))
	m := mustRead(t, c, 7)
	require.True(t, m.MultiKey.MultiKeyAvailable)
	require.Equal(t, 2, m.MultiKey.ActiveAPIKeyCount24h)
	require.True(t, m.MultiKey.CrossKeyFullScaffoldOverlapAvailable1h)
	require.GreaterOrEqual(t, m.MultiKey.CrossKeyFullScaffoldOverlapEstimate1h, 1)
	require.GreaterOrEqual(t, m.MultiKey.SynchronizedMultiKeyMinutes1h, 1)
}

func TestAgg_MultiKeyUnavailableSingleKey(t *testing.T) {
	c, _, _ := newAggTest(t)
	require.NoError(t, c.Aggregate(context.Background(), okEnv("r1", 7, 3)))
	require.False(t, mustRead(t, c, 7).MultiKey.MultiKeyAvailable, "single key → multi-key unavailable")
}

// §七 Fingerprint Epoch：不同 fpVer 命名空间隔离。
func TestAgg_FingerprintEpochIsolation(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	cV1 := newRiskV2AggCacheWithClock(rdb, "s1", "v1", time.Now)
	cV2 := newRiskV2AggCacheWithClock(rdb, "s1", "v2", time.Now)
	require.NoError(t, cV1.Aggregate(context.Background(), okEnv("r1", 7, 3)))
	require.EqualValues(t, 1, mustRead(t, cV1, 7).User.W5m.RequestCount)
	require.EqualValues(t, 0, mustRead(t, cV2, 7).User.W5m.RequestCount, "new epoch must not see old-version data")
}

// §六/§八 API Key 超读取上限 → overflow + incomplete。
func TestAgg_APIKeyOverflowIncomplete(t *testing.T) {
	c, _, _ := newAggTest(t)
	for i := 1; i <= riskV2MaxAPIKeysPerRead+5; i++ {
		require.NoError(t, c.Aggregate(context.Background(), okEnv(fmt.Sprintf("r%d", i), 7, int64(i))))
	}
	m := mustRead(t, c, 7)
	require.True(t, m.MultiKey.APIKeyOverflow)
	require.True(t, m.MultiKey.MultiKeyIncomplete)
	require.True(t, m.Incomplete)
	require.Contains(t, m.IncompleteReasons, "apikey_overflow")
}

// §九 DEGRADED：读路径 Redis 错误 → Degraded=true。
func TestAgg_ReadDegradedOnRedisError(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, ReadTimeout: 50 * time.Millisecond, WriteTimeout: 50 * time.Millisecond})
	t.Cleanup(func() { _ = rdb.Close() })
	c := newRiskV2AggCacheWithClock(rdb, "s1", "v1", time.Now)
	m, err := c.ReadForAdminDetail(context.Background(), 7)
	require.NoError(t, err) // 读不返回错误,但标记 degraded
	require.True(t, m.Degraded)
	require.True(t, m.Incomplete)
	require.Contains(t, m.IncompleteReasons, "redis_degraded")
}

func mustRead(t *testing.T, c *riskV2AggCache, uid int64) service.RiskV2WindowMetrics {
	t.Helper()
	m, err := c.ReadForAdminDetail(context.Background(), uid)
	require.NoError(t, err)
	return m
}

// —— §八 Read 最坏情况 benchmark（miniredis 进程内，不含真实网络 RTT）——

func benchReadWithKeys(b *testing.B, nKeys int, perKeyDistinct int) {
	mr, _ := miniredis.Run()
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	c := NewRiskV2AggCache(rdb, "s1", "v1").(*riskV2AggCache)
	for k := 1; k <= nKeys; k++ {
		for d := 0; d < perKeyDistinct; d++ {
			e := fullScaffoldEnv("r", 7, int64(k), "v1:sc-"+strconv.Itoa(k)+"-"+strconv.Itoa(d))
			e.ExactHMAC = "v1:ex-" + strconv.Itoa(k) + "-" + strconv.Itoa(d)
			_ = c.Aggregate(context.Background(), e)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.ReadForAdminDetail(context.Background(), 7)
	}
}

func BenchmarkRiskV2Read_1Key_LowCard(b *testing.B)  { benchReadWithKeys(b, 1, 5) }
func BenchmarkRiskV2Read_8Key_LowCard(b *testing.B)  { benchReadWithKeys(b, 8, 5) }
func BenchmarkRiskV2Read_64Key_LowCard(b *testing.B) { benchReadWithKeys(b, 64, 5) }
func BenchmarkRiskV2Read_8Key_HighCard(b *testing.B) { benchReadWithKeys(b, 8, 300) }
func BenchmarkRiskV2Read_OverKeyLimit(b *testing.B)  { benchReadWithKeys(b, 80, 5) }
