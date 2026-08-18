//go:build unit

package repository

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// —— 命令计数 / key 访问记录 hook（证明读取成本与 key 数量无关、且绝不触碰 per-key 桶）——
// mu 保护并发读取（批量读取会并发调用，共享同一 hook）。

type cmdSpy struct {
	mu    sync.Mutex
	total int
	keys  []string
}

func (h *cmdSpy) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *cmdSpy) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.record(cmd)
		return next(ctx, cmd)
	}
}
func (h *cmdSpy) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, c := range cmds {
			h.record(c)
		}
		return next(ctx, cmds)
	}
}
func (h *cmdSpy) record(cmd redis.Cmder) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.total++
	for _, a := range cmd.Args() {
		if s, ok := a.(string); ok && strings.HasPrefix(s, "riskv2:") {
			h.keys = append(h.keys, s)
		}
	}
}
func (h *cmdSpy) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.total = 0
	h.keys = nil
}
func (h *cmdSpy) totalCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.total
}

func newSpyCache(t *testing.T, clk func() time.Time) (*riskV2AggCache, *miniredis.Miniredis, *cmdSpy) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	spy := &cmdSpy{}
	rdb.AddHook(spy)
	return newRiskV2AggCacheWithClock(rdb, "s1", "v1", clk), mr, spy
}

// §四 验收：RiskV2ScoringSnapshot 结构上没有 PerAPIKey 字段（类型层面杜绝逐 key 展开）。
func TestScoringSnapshot_HasNoPerAPIKeyField(t *testing.T) {
	_, ok := reflect.TypeOf(service.RiskV2ScoringSnapshot{}).FieldByName("PerAPIKey")
	require.False(t, ok, "RiskV2ScoringSnapshot must NOT have a PerAPIKey field")
}

// ReadForScoring 正确性：用户级三窗口 + 紧凑多 Key 汇总（含 cross-key overlap / sync / handoff）。
func TestScoringRead_UserWindowsAndMultiKey(t *testing.T) {
	fixed := time.Date(2026, 1, 2, 10, 30, 15, 0, time.UTC)
	c, _, _ := newSpyCache(t, func() time.Time { return fixed })
	ctx := context.Background()

	// 用户 7，3 个 key；其中 scaffold "sc-shared" 被 key 1 与 key 2 共用（cross-key overlap=1）。
	mustAgg := func(server string, kid int64, scaffold string) {
		e := fullScaffoldEnv(server, 7, kid, scaffold)
		e.ExactHMAC = "ex-" + server
		require.NoError(t, c.Aggregate(ctx, e))
	}
	mustAgg("a1", 1, "sc-shared")
	mustAgg("a2", 2, "sc-shared")
	mustAgg("a3", 3, "sc-k3")

	snap, err := c.ReadForScoring(ctx, 7)
	require.NoError(t, err)
	require.EqualValues(t, 3, snap.User.W1h.RequestCount)
	require.EqualValues(t, 3, snap.User.W24h.RequestCount)
	require.True(t, snap.MultiKey.MultiKeyAvailable)
	require.Equal(t, 3, snap.MultiKey.ActiveAPIKeyCount24h)
	// 同一分钟内 3 个 key 都活跃 → sync 分钟 ≥1。
	require.GreaterOrEqual(t, snap.MultiKey.SynchronizedMultiKeyMinutes1h, 1)
	// sc-shared 出现在 key1、key2 下 → cross-key full scaffold overlap = 1。
	require.True(t, snap.MultiKey.CrossKeyFullScaffoldOverlapAvailable1h)
	require.Equal(t, 1, snap.MultiKey.CrossKeyFullScaffoldOverlapEstimate1h)
}

// §四 验收：ReadForScoring 绝不读取任何 per-key（":k:"）桶。
func TestScoringRead_NeverTouchesPerKeyBuckets(t *testing.T) {
	fixed := time.Date(2026, 1, 2, 10, 30, 15, 0, time.UTC)
	c, _, spy := newSpyCache(t, func() time.Time { return fixed })
	ctx := context.Background()
	for k := int64(1); k <= 8; k++ {
		e := fullScaffoldEnv("r"+strconv.FormatInt(k, 10), 7, k, "sc-"+strconv.FormatInt(k, 10))
		require.NoError(t, c.Aggregate(ctx, e))
	}
	spy.reset() // 只关注读取阶段
	_, err := c.ReadForScoring(ctx, 7)
	require.NoError(t, err)
	require.NotEmpty(t, spy.keys)
	for _, k := range spy.keys {
		require.NotContains(t, k, ":k:", "ReadForScoring must not read per-key bucket %q", k)
	}
}

// §四 验收：1 key vs 64 key，ReadForScoring 基础命令数不随 key 数量增长（不 ~64x）。
func TestScoringRead_CommandCountIndependentOfKeyCount(t *testing.T) {
	fixed := time.Date(2026, 1, 2, 10, 30, 15, 0, time.UTC)
	measure := func(nKeys int) int {
		c, _, spy := newSpyCache(t, func() time.Time { return fixed })
		ctx := context.Background()
		for k := 1; k <= nKeys; k++ {
			e := fullScaffoldEnv("r"+strconv.Itoa(k), 7, int64(k), "sc-"+strconv.Itoa(k))
			e.ExactHMAC = "ex-" + strconv.Itoa(k)
			require.NoError(t, c.Aggregate(ctx, e))
		}
		spy.reset()
		_, err := c.ReadForScoring(ctx, 7)
		require.NoError(t, err)
		return spy.totalCount()
	}
	one := measure(1)
	sixtyFour := measure(64)
	t.Logf("ReadForScoring base commands/read: 1key=%d 64key=%d", one, sixtyFour)
	require.Greater(t, one, 0)
	// 命令数应基本相同（固定：24 bitmap + 用户窗口 + 24/60/60 ak/xkscf + 用户 ak 唯一数）；
	// 绝不随 key 数量线性增长。给极小裕度以防实现细节，但必须远小于 64x。
	require.LessOrEqual(t, sixtyFour, one+2, "scoring read command count must not grow with key count (1=%d, 64=%d)", one, sixtyFour)
}

// 对照：ReadForAdminDetail 的命令数**随 key 数量增长**（证明二者读取形态不同）。
func TestAdminDetailRead_CommandCountGrowsWithKeyCount(t *testing.T) {
	fixed := time.Date(2026, 1, 2, 10, 30, 15, 0, time.UTC)
	measure := func(nKeys int) int {
		c, _, spy := newSpyCache(t, func() time.Time { return fixed })
		ctx := context.Background()
		for k := 1; k <= nKeys; k++ {
			e := fullScaffoldEnv("r"+strconv.Itoa(k), 7, int64(k), "sc-"+strconv.Itoa(k))
			require.NoError(t, c.Aggregate(ctx, e))
		}
		spy.reset()
		_, err := c.ReadForAdminDetail(ctx, 7)
		require.NoError(t, err)
		return spy.totalCount()
	}
	require.Greater(t, measure(64), measure(1)*4, "admin detail read must grow with key count")
}

// §五 批量：单用户失败不拖累整批；每用户各自结果或错误。
func TestScoringReadBatch_PerUserResults(t *testing.T) {
	fixed := time.Date(2026, 1, 2, 10, 30, 15, 0, time.UTC)
	c, _, _ := newSpyCache(t, func() time.Time { return fixed })
	ctx := context.Background()
	for uid := int64(1); uid <= 5; uid++ {
		require.NoError(t, c.Aggregate(ctx, okEnv("r"+strconv.FormatInt(uid, 10), uid, 1)))
	}
	ids := []int64{1, 2, 3, 4, 5, 999} // 999 无数据
	items, err := c.ReadForScoringBatch(ctx, ids)
	require.NoError(t, err)
	require.Len(t, items, len(ids))
	for i, it := range items {
		require.Equal(t, ids[i], it.UserID)
		require.NoError(t, it.Err)
	}
	require.EqualValues(t, 1, items[0].Snapshot.User.W24h.RequestCount)
	require.EqualValues(t, 0, items[5].Snapshot.User.W24h.RequestCount) // 无数据 → 空快照
}

// §五 批量：context 取消时立即返回并携带 ctx.Err（不吊死、不无界 goroutine）。
func TestScoringReadBatch_ContextCancel(t *testing.T) {
	fixed := time.Date(2026, 1, 2, 10, 30, 15, 0, time.UTC)
	c, _, _ := newSpyCache(t, func() time.Time { return fixed })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ids := make([]int64, 500) // 跨多个分块
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	_, err := c.ReadForScoringBatch(ctx, ids)
	require.ErrorIs(t, err, context.Canceled)
}

// §六 活跃用户游标：周期 ZSET lex 分页遍历，覆盖全部活跃用户且不重，NextCursor=="" 结束。
func TestListActiveUserIDs_CursorPaginationCoversAll(t *testing.T) {
	fixed := time.Date(2026, 1, 2, 10, 30, 15, 0, time.UTC)
	c, _, _ := newSpyCache(t, func() time.Time { return fixed })
	ctx := context.Background()
	const n = 250
	for uid := int64(1); uid <= n; uid++ {
		require.NoError(t, c.Aggregate(ctx, okEnv("r"+strconv.FormatInt(uid, 10), uid, 1)))
	}
	cycleEnd := riskV2CycleEnd(fixed.Unix(), int64((5 * time.Minute).Seconds()))
	seen := map[int64]bool{}
	cursor := ""
	for iter := 0; iter < 10000; iter++ {
		page, err := c.ListActiveUserIDsForCycle(ctx, cycleEnd, cursor, 50)
		require.NoError(t, err)
		require.LessOrEqual(t, len(page.UserIDs), 50)
		for _, id := range page.UserIDs {
			require.False(t, seen[id], "duplicate user %d in pagination", id)
			seen[id] = true
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	require.Len(t, seen, n, "cursor pagination must cover all active users exactly once")
}

// §六 活跃用户游标：limit 受硬上限约束。
func TestListActiveUserIDs_LimitCapped(t *testing.T) {
	fixed := time.Date(2026, 1, 2, 10, 30, 15, 0, time.UTC)
	c, _, _ := newSpyCache(t, func() time.Time { return fixed })
	ctx := context.Background()
	for uid := int64(1); uid <= 100; uid++ {
		require.NoError(t, c.Aggregate(ctx, okEnv("r"+strconv.FormatInt(uid, 10), uid, 1)))
	}
	cycleEnd := riskV2CycleEnd(fixed.Unix(), int64((5 * time.Minute).Seconds()))
	page, err := c.ListActiveUserIDsForCycle(ctx, cycleEnd, "", 999999)
	require.NoError(t, err)
	require.LessOrEqual(t, len(page.UserIDs), riskV2ActiveUserScanMax)
}

// —— §四 DI 边界：admin-detail-only 实现绝不满足 ScoringReader 接口 ——

type adminOnlyReader struct{}

func (adminOnlyReader) ReadForAdminDetail(ctx context.Context, userID int64) (service.RiskV2WindowMetrics, error) {
	return service.RiskV2WindowMetrics{}, nil
}

// 编译期：adminOnlyReader 满足 AdminDetailReader；若它也被当作 ScoringReader 注入会编译失败。
var _ service.RiskV2AdminDetailReader = adminOnlyReader{}

// fakeScoringWorkerCtor 模拟未来 Scoring Worker 的 ProviderSet 入参：只接受 ScoringReader。
func fakeScoringWorkerCtor(r service.RiskV2ScoringReader) service.RiskV2ScoringReader { return r }

func TestDIGuard_AdminOnlyIsNotScoringReader(t *testing.T) {
	var r interface{} = adminOnlyReader{}
	_, isScoring := r.(service.RiskV2ScoringReader)
	require.False(t, isScoring, "admin-detail-only reader must NOT satisfy RiskV2ScoringReader")
	_, isAdmin := r.(service.RiskV2AdminDetailReader)
	require.True(t, isAdmin)

	// 真正的评分读取面可注入 Worker ctor；构造器返回 nil 客户端时安全。
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	sr := NewRiskV2ScoringReader(rdb, "s1", "v1")
	require.NotNil(t, fakeScoringWorkerCtor(sr))

	// 且 NewRiskV2ScoringReader 的返回值天然也是 AdminDetailReader 的具体实现——
	// 但 Worker 入参类型限定为 ScoringReader，故 Worker 内拿不到 ReadForAdminDetail。
	_ = NewRiskV2AdminDetailReader(rdb, "s1", "v1")
}
