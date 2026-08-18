//go:build unit

package repository

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 真实 Redis 基准（切片 3.5 §八）。通过 RISK_V2_BENCH_REDIS_ADDR 指向临时非生产 Redis；未设则跳过。
// 起停临时实例见 scripts/risk_v2_bench_redis.sh。绝不连接生产 Redis。

func benchRealRedis(b *testing.B) *redis.Client {
	addr := os.Getenv("RISK_V2_BENCH_REDIS_ADDR")
	if addr == "" {
		b.Skip("set RISK_V2_BENCH_REDIS_ADDR to a temp non-prod redis to run this benchmark")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		b.Skipf("cannot reach bench redis at %s: %v", addr, err)
	}
	b.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// seedUser 向真实 Redis 塞入某用户 nKeys 个 key、每 key perKeyDistinct 条不同 scaffold/exact 的观测。
func seedUser(b *testing.B, c *riskV2AggCache, uid int64, nKeys, perKeyDistinct int) {
	b.Helper()
	ctx := context.Background()
	for k := 1; k <= nKeys; k++ {
		for d := 0; d < perKeyDistinct; d++ {
			e := fullScaffoldEnv("bseed", uid, int64(k),
				"v1:sc-"+strconv.Itoa(k)+"-"+strconv.Itoa(d))
			e.ExactHMAC = "v1:ex-" + strconv.Itoa(k) + "-" + strconv.Itoa(d)
			if err := c.Aggregate(ctx, e); err != nil {
				b.Fatalf("seed aggregate: %v", err)
			}
		}
	}
}

func newRealCache(rdb *redis.Client) *riskV2AggCache {
	// 固定时钟：让 seed 与读取落在同一分钟/小时窗口，基准可复现。
	fixed := time.Date(2026, 2, 3, 12, 20, 30, 0, time.UTC)
	return newRiskV2AggCacheWithClock(rdb, "s1", "v1", func() time.Time { return fixed })
}

func benchScoringRead(b *testing.B, nKeys, perKeyDistinct int) {
	rdb := benchRealRedis(b)
	c := newRealCache(rdb)
	uid := int64(700000 + nKeys*1000 + perKeyDistinct)
	seedUser(b, c, uid, nKeys, perKeyDistinct)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.ReadForScoring(ctx, uid); err != nil {
			b.Fatalf("ReadForScoring: %v", err)
		}
	}
}

func BenchmarkScoringRead_1Key_LowCard(b *testing.B)   { benchScoringRead(b, 1, 5) }
func BenchmarkScoringRead_8Key_MidCard(b *testing.B)   { benchScoringRead(b, 8, 20) }
func BenchmarkScoringRead_64Key_MidCard(b *testing.B)  { benchScoringRead(b, 64, 20) }
func BenchmarkScoringRead_64Key_HighCard(b *testing.B) { benchScoringRead(b, 64, 200) }

// 对照：Admin Detail 64 key（成本随 key 数量增长的基线）。
func BenchmarkAdminDetailRead_64Key_MidCard(b *testing.B) {
	rdb := benchRealRedis(b)
	c := newRealCache(rdb)
	uid := int64(800064)
	seedUser(b, c, uid, 64, 20)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.ReadForAdminDetail(ctx, uid); err != nil {
			b.Fatalf("ReadForAdminDetail: %v", err)
		}
	}
}

// 批量 100 用户（每用户 8 key）。
func BenchmarkScoringReadBatch_100Users(b *testing.B) {
	rdb := benchRealRedis(b)
	c := newRealCache(rdb)
	ids := make([]int64, 100)
	for i := range ids {
		uid := int64(900000 + i)
		ids[i] = uid
		seedUser(b, c, uid, 8, 10)
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.ReadForScoringBatch(ctx, ids); err != nil {
			b.Fatalf("batch: %v", err)
		}
	}
}

// 活跃用户 SSCAN 全量遍历（10k 用户）。
func BenchmarkListActiveUserIDs_10kFullScan(b *testing.B) {
	rdb := benchRealRedis(b)
	c := newRealCache(rdb)
	ctx := context.Background()
	for uid := int64(1); uid <= 10000; uid++ {
		e := okEnv("aseed", uid, 1)
		if err := c.Aggregate(ctx, e); err != nil {
			b.Fatalf("seed active: %v", err)
		}
	}
	cycleEnd := riskV2CycleEnd(newRealCache(rdb).now().Unix(), int64((5 * time.Minute).Seconds()))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cursor := ""
		total := 0
		for {
			page, err := c.ListActiveUserIDsForCycle(ctx, cycleEnd, cursor, 1000)
			if err != nil {
				b.Fatalf("scan: %v", err)
			}
			total += len(page.UserIDs)
			cursor = page.NextCursor
			if cursor == "" {
				break
			}
		}
	}
}
