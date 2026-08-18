//go:build unit

package repository

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// fixedClock 让桶时间可控（跨桶/TTL 验证）。
type fixedClock struct{ t time.Time }

func (c *fixedClock) now() time.Time      { return c.t }
func (c *fixedClock) add(d time.Duration) { c.t = c.t.Add(d) }

func newAggTest(t *testing.T) (*riskV2AggCache, *miniredis.Miniredis, *fixedClock) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	clk := &fixedClock{t: time.Date(2026, 1, 2, 10, 30, 15, 0, time.UTC)}
	c := newRiskV2AggCacheWithClock(rdb, "s1", "v1", clk.now)
	return c, mr, clk
}

func okEnv(server string, uid, kid int64) service.RiskFeatureEnvelope {
	return service.RiskFeatureEnvelope{
		ServerRequestID: server, UserID: uid, APIKeyID: kid,
		TerminalStatus: "ok", UsageAvailable: true, InputTokens: 10, OutputTokens: 20,
		ExactFingerprintAvailable: true, ExactHMAC: "v1:exact", ScaffoldHMAC: "v1:scaf",
		KeyVersion: "v1", FingerprintVersion: "fp-v2",
	}
}

func TestAgg_NamespaceIsolatedFromLegacy(t *testing.T) {
	c, mr, _ := newAggTest(t)
	require.NoError(t, c.Aggregate(context.Background(), okEnv("s1req", 7, 3)))
	for _, k := range mr.Keys() {
		require.Truef(t, strings.HasPrefix(k, "riskv2:"), "key outside riskv2 namespace: %q", k)
	}
}

func TestAgg_UserAndAPIKeyWindows(t *testing.T) {
	c, _, _ := newAggTest(t)
	require.NoError(t, c.Aggregate(context.Background(), okEnv("r1", 7, 3)))
	m, err := c.ReadForAdminDetail(context.Background(), 7)
	require.NoError(t, err)
	require.EqualValues(t, 1, m.User.W5m.RequestCount)
	require.EqualValues(t, 1, m.User.W1h.RequestCount)
	require.EqualValues(t, 1, m.User.W24h.RequestCount)
	require.Contains(t, m.PerAPIKey, int64(3))
	require.EqualValues(t, 1, m.PerAPIKey[3].W5m.RequestCount)
	require.Equal(t, 1, m.User.W5m.UniqueAPIKeyCount)
}

func TestAgg_BucketBoundaryUnionAndDuplicate(t *testing.T) {
	c, _, clk := newAggTest(t)
	// 同一 exact 在两个相邻分钟桶 → distinct=1（跨桶并集），request=2 → duplicate=1。
	require.NoError(t, c.Aggregate(context.Background(), okEnv("r1", 7, 3)))
	clk.add(60 * time.Second)
	require.NoError(t, c.Aggregate(context.Background(), okEnv("r2", 7, 3)))
	m, _ := c.ReadForAdminDetail(context.Background(), 7)
	require.EqualValues(t, 2, m.User.W5m.RequestCount)
	require.Equal(t, 1, m.User.W5m.DistinctExactEstimate, "cross-bucket union must not sum SCARDs")
	dup, ok := m.User.W5m.ExactDuplicateCount()
	require.True(t, ok)
	require.EqualValues(t, 1, dup)
}

func TestAgg_TTLExpiryMinuteVsHour(t *testing.T) {
	c, mr, clk := newAggTest(t)
	require.NoError(t, c.Aggregate(context.Background(), okEnv("r1", 7, 3)))
	// 所有 key 有 TTL。
	for _, k := range mr.Keys() {
		require.Positivef(t, mr.TTL(k), "key %q must have TTL", k)
	}
	// 前进 ~2h5m：分钟桶(TTL2h)过期,小时桶(TTL30h)存活 → 5m 空,24h 仍有。
	mr.FastForward(2*time.Hour + 5*time.Minute)
	clk.add(2*time.Hour + 5*time.Minute)
	m, _ := c.ReadForAdminDetail(context.Background(), 7)
	require.EqualValues(t, 0, m.User.W5m.RequestCount, "minute buckets expired")
	require.EqualValues(t, 1, m.User.W24h.RequestCount, "hour bucket survives")
}

func TestAgg_OverflowNoFalseHighDuplicate(t *testing.T) {
	c, mr, clk := newAggTest(t)
	// 预填当前分钟 user exact set 到上限,制造 overflow。
	mts := strconv.FormatInt(clk.now().Unix()/60, 10)
	xeKey := c.userBase(7) + ":m:" + mts + ":xe"
	for i := 0; i < riskV2AggSetMax; i++ {
		mr.SetAdd(xeKey, "dummy-"+strconv.Itoa(i))
	}
	require.NoError(t, c.Aggregate(context.Background(), okEnv("r1", 7, 3))) // 新 exact 被拒 → overflow
	m, _ := c.ReadForAdminDetail(context.Background(), 7)
	require.True(t, m.User.W5m.ExactOverflow)
	require.True(t, m.User.W5m.ExactIncomplete)
	_, ok := m.User.W5m.ExactDuplicateRatio()
	require.False(t, ok, "overflow/incomplete must make duplicate ratio unavailable (no false high rate)")
}

func TestAgg_EmptyAndTruncatedExactNotAggregated(t *testing.T) {
	c, _, _ := newAggTest(t)
	e := okEnv("r1", 7, 3)
	e.ExactFingerprintAvailable = false
	e.ExactHMAC = ""
	e.InputTruncated = true
	e.ScaffoldHMAC = "v1:scaf"
	e.ScaffoldFingerprintSampled = true
	require.NoError(t, c.Aggregate(context.Background(), e))
	m, _ := c.ReadForAdminDetail(context.Background(), 7)
	require.EqualValues(t, 0, m.User.W5m.ExactAvailableCount)
	require.Equal(t, 0, m.User.W5m.DistinctExactEstimate)
	require.EqualValues(t, 1, m.User.W5m.TruncatedInputCount)
	require.EqualValues(t, 1, m.User.W5m.SampledScaffoldRequestCount, "sampled scaffold must be marked")
	require.EqualValues(t, 0, m.User.W5m.FullScaffoldRequestCount, "sampled must not count as full")
}

func TestAgg_CacheFourStates(t *testing.T) {
	c, _, _ := newAggTest(t)
	cc0, cr512 := int64(0), int64(512)
	// applicable+available+ptr(0)/ptr(512)
	e1 := okEnv("r1", 7, 3)
	e1.CacheUsageApplicable, e1.CacheUsageAvailable = true, true
	e1.CacheCreationTokens, e1.CacheReadTokens = &cc0, &cr512
	require.NoError(t, c.Aggregate(context.Background(), e1))
	// non-applicable
	e2 := okEnv("r2", 7, 3)
	e2.CacheUsageApplicable = false
	require.NoError(t, c.Aggregate(context.Background(), e2))
	m, _ := c.ReadForAdminDetail(context.Background(), 7)
	require.EqualValues(t, 1, m.User.W5m.CacheApplicableCount, "non-applicable must not enter applicable count")
	require.EqualValues(t, 1, m.User.W5m.CacheAvailableCount)
	require.EqualValues(t, 512, m.User.W5m.CacheReadInputTokens)
	require.EqualValues(t, 0, m.User.W5m.CacheCreationInputTokens) // 显式 0 计入（值为 0）
	ratio, ok := m.User.W5m.CacheAvailabilityRatio()
	require.True(t, ok)
	require.InDelta(t, 1.0, ratio, 0.001) // available/applicable = 1/1
}

func TestAgg_ParameterSignature(t *testing.T) {
	c, _, _ := newAggTest(t)
	temp1, temp2 := 0.2, 0.9
	e1 := okEnv("r1", 7, 3)
	e1.Temperature = &temp1
	e2 := okEnv("r2", 7, 3)
	e2.Temperature = &temp2
	require.NoError(t, c.Aggregate(context.Background(), e1))
	require.NoError(t, c.Aggregate(context.Background(), e2))
	m, _ := c.ReadForAdminDetail(context.Background(), 7)
	require.Equal(t, 1, m.User.W5m.DistinctExactEstimate, "same exact → distinct exact = 1")
	require.Equal(t, 2, m.User.W5m.DistinctExactParamSigEstimate, "different temperature → 2 param signatures (repeated sampling)")
}

func TestAgg_ActiveMinutesAndPeakRPM(t *testing.T) {
	c, _, clk := newAggTest(t)
	// 同一分钟 2 次 → peak_rpm(5m)=2, active_minutes=1。
	require.NoError(t, c.Aggregate(context.Background(), okEnv("r1", 7, 3)))
	require.NoError(t, c.Aggregate(context.Background(), okEnv("r2", 7, 3)))
	clk.add(2 * time.Minute) // 新分钟 1 次
	require.NoError(t, c.Aggregate(context.Background(), okEnv("r3", 7, 3)))
	m, _ := c.ReadForAdminDetail(context.Background(), 7)
	require.Equal(t, 2, m.User.W5m.PeakRPM, "peak rpm = max per-minute count")
	require.True(t, m.User.W5m.PeakRPMAvailable)
	require.Equal(t, 2, m.User.W5m.ActiveMinutes, "two distinct active minutes")
	require.False(t, m.User.W24h.PeakRPMAvailable, "24h has no minute granularity")
}

func TestAgg_RedisUnavailableReturnsError(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, ReadTimeout: 50 * time.Millisecond, WriteTimeout: 50 * time.Millisecond})
	t.Cleanup(func() { _ = rdb.Close() })
	c := newRiskV2AggCacheWithClock(rdb, "s1", "v1", time.Now)
	require.Error(t, c.Aggregate(context.Background(), okEnv("r1", 7, 3)), "unavailable Redis must surface error (sink counts DEGRADED)")
}

func TestAgg_NoPromptOrSecretInKeysOrValues(t *testing.T) {
	c, mr, _ := newAggTest(t)
	const sentinel = "ZZ_SENTINEL_CLIENTID"
	e := okEnv(sentinel, 7, 3) // server id 只作 dispatcher 去重键，不应进入聚合 Redis
	require.NoError(t, c.Aggregate(context.Background(), e))
	for _, k := range mr.Keys() {
		require.NotContains(t, k, sentinel, "server/client id must not appear in Redis keys")
		v, _ := mr.Get(k)
		require.NotContains(t, v, sentinel)
		if members, err := mr.SMembers(k); err == nil {
			for _, mm := range members {
				require.NotContains(t, mm, sentinel)
			}
		}
	}
}

func TestAgg_SinkFailOpenAndStats(t *testing.T) {
	c, _, _ := newAggTest(t)
	sink := service.NewRiskV2AggSink(c, 0)
	sink.Consume(okEnv("r1", 7, 3))
	sink.Consume(service.RiskFeatureEnvelope{ServerRequestID: "r2", UserID: 0}) // skip (no user)
	st := sink.Stats()
	require.EqualValues(t, 1, st.Consumed)
	require.EqualValues(t, 1, st.SkippedNoUser)
	require.EqualValues(t, 0, st.AggregationError)
}

// —— benchmark（10k 活跃用户容量估算参考）——

func BenchmarkRiskV2Aggregate(b *testing.B) {
	mr, _ := miniredis.Run()
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	c := NewRiskV2AggCache(rdb, "s1", "v1")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Aggregate(context.Background(), okEnv(fmt.Sprintf("r%d", i), int64(i%10000)+1, int64(i%3)+1))
	}
}

func BenchmarkRiskV2ReadForAdminDetail(b *testing.B) {
	mr, _ := miniredis.Run()
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	c := NewRiskV2AggCache(rdb, "s1", "v1").(*riskV2AggCache)
	for i := 0; i < 200; i++ {
		_ = c.Aggregate(context.Background(), okEnv(fmt.Sprintf("r%d", i), 7, int64(i%3)+1))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.ReadForAdminDetail(context.Background(), 7)
	}
}
