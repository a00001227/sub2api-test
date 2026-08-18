//go:build unit

package repository

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// —— §四 周期状态：Worker 使用 CycleStore ——

// 已完成周期 → 不重新全量扫描。
func TestWorker_SkipCompletedCycle(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 6, 5, 0, time.UTC)
	p := defaultWorkerParams()
	p.MaxUsersPerSecond = 0 // 固定时钟下关闭节流，避免 pacer 死循环
	cyc := baseCycleEnd(now, p)
	reader := newFakeReader()
	reader.setCycle(cyc, []int64{1, 2, 3})
	repo := newFakeRepo()
	store := newFakeCycleStore()
	store.states[cyc] = service.RiskV2CycleState{Status: service.RiskV2CycleCompleted}
	w := service.NewRiskV2ScoringWorker(service.RiskV2ScoringWorkerDeps{
		Reader: reader, Lease: &fakeLease{}, Health: healthyHealth(), Repo: repo, CycleStore: store,
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, Now: func() time.Time { return now }, Sleep: nil,
	}, p)
	w.RunDueCycles(context.Background())
	require.Equal(t, 0, repo.count(), "completed cycle must not be re-scanned")
	require.EqualValues(t, 0, reader.batchCalls)
}

// 从持久化 cursor 恢复：只处理剩余用户。
func TestWorker_ResumeFromPersistedCursor(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 6, 5, 0, time.UTC)
	p := defaultWorkerParams()
	p.MaxUsersPerSecond = 0 // 固定时钟下关闭节流，避免 pacer 死循环
	p.MaxUsersPerSecond = 0
	cyc := baseCycleEnd(now, p)
	reader := newFakeReader()
	reader.setCycle(cyc, []int64{1, 2, 3, 4})
	repo := newFakeRepo()
	store := newFakeCycleStore()
	// 预置：已处理到 cursor="2"（表示 1,2 已完成），INCOMPLETE。
	store.states[cyc] = service.RiskV2CycleState{Status: service.RiskV2CycleIncomplete, Cursor: "2"}
	w := service.NewRiskV2ScoringWorker(service.RiskV2ScoringWorkerDeps{
		Reader: reader, Lease: &fakeLease{}, Health: healthyHealth(), Repo: repo, CycleStore: store,
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, Now: func() time.Time { return now },
	}, p)
	w.RunDueCycles(context.Background())
	// 只处理 3,4（从 cursor "2" 之后）。
	require.Equal(t, 2, repo.count())
	repo.mu.Lock()
	got := []int64{repo.upserts[0].userID, repo.upserts[1].userID}
	repo.mu.Unlock()
	require.ElementsMatch(t, []int64{3, 4}, got)
	// 完成后状态 COMPLETED。
	st, _, _ := store.LoadCycleState(context.Background(), cyc)
	require.Equal(t, service.RiskV2CycleCompleted, st.Status)
}

// §六：运行中 owner 被别的 leader 接管（lease 易主）→ 状态写被拒 → 中止，绝不标记 COMPLETED。
func TestWorker_StaleWriteRejectedNoComplete(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 6, 5, 0, time.UTC)
	p := defaultWorkerParams()
	p.MaxUsersPerSecond = 0
	p.BatchSize = 1 // 每批后 saveState → 触发 failSave
	cyc := baseCycleEnd(now, p)
	reader := newFakeReader()
	reader.setCycle(cyc, []int64{1, 2})
	repo := newFakeRepo()
	store := newFakeCycleStore()
	store.failSave = true // 模拟本 token 已非当前 owner（被接管）
	w := service.NewRiskV2ScoringWorker(service.RiskV2ScoringWorkerDeps{
		Reader: reader, Lease: &fakeLease{}, Health: healthyHealth(), Repo: repo, CycleStore: store,
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, Now: func() time.Time { return now },
	}, p)
	w.RunDueCycles(context.Background())
	require.EqualValues(t, 1, w.Metrics().CyclesIncomplete)
	st, _, _ := store.LoadCycleState(context.Background(), cyc)
	require.NotEqual(t, service.RiskV2CycleCompleted, st.Status, "stale owner must never mark COMPLETED")
}

// §五 失败用户进入下一周期 retry。
func TestWorker_FailedUserRetryNextCycle(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 6, 5, 0, time.UTC)
	p := defaultWorkerParams()
	p.MaxUsersPerSecond = 0 // 固定时钟下关闭节流，避免 pacer 死循环
	cyc := baseCycleEnd(now, p)
	iv := int64(p.Interval.Seconds())
	reader := newFakeReader()
	reader.setCycle(cyc, []int64{1, 2, 3})
	repo := newFakeRepo()
	repo.errByUser[2] = errors.New("db down") // 瞬时 DB 错误 → 可重试
	store := newFakeCycleStore()
	w := service.NewRiskV2ScoringWorker(service.RiskV2ScoringWorkerDeps{
		Reader: reader, Lease: &fakeLease{}, Health: healthyHealth(), Repo: repo, CycleStore: store,
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, Now: func() time.Time { return now },
	}, p)
	w.RunDueCycles(context.Background())
	added := store.addedTo(cyc + iv)
	require.Len(t, added, 1)
	require.EqualValues(t, 2, added[0].UserID)
	require.Equal(t, 1, added[0].Attempts, "first failure → attempts=1")
}

// §五 retry 用户在下一周期被合并处理；conflict 不重试。
func TestWorker_RetryUsersMerged(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 6, 5, 0, time.UTC)
	p := defaultWorkerParams()
	p.MaxUsersPerSecond = 0 // 固定时钟下关闭节流，避免 pacer 死循环
	cyc := baseCycleEnd(now, p)
	reader := newFakeReader()
	reader.setCycle(cyc, []int64{1}) // active 只有 1
	reader.snaps[9] = service.RiskV2ScoringSnapshot{UserID: 9, SchemaVersion: "s1", FingerprintKeyVersion: "v1"}
	repo := newFakeRepo()
	store := newFakeCycleStore()
	store.retry[cyc] = []service.RiskV2RetryUser{{UserID: 9, Attempts: 1}} // 上一周期失败的 9
	w := service.NewRiskV2ScoringWorker(service.RiskV2ScoringWorkerDeps{
		Reader: reader, Lease: &fakeLease{}, Health: healthyHealth(), Repo: repo, CycleStore: store,
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, Now: func() time.Time { return now },
	}, p)
	w.RunDueCycles(context.Background())
	repo.mu.Lock()
	seen := map[int64]bool{}
	for _, r := range repo.upserts {
		seen[r.userID] = true
	}
	repo.mu.Unlock()
	require.True(t, seen[1], "active user processed")
	require.True(t, seen[9], "retry user merged and processed even without new activity")
}

// §五 conflict 不进入 retry。
func TestWorker_ConflictNotRetried(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 6, 5, 0, time.UTC)
	p := defaultWorkerParams()
	p.MaxUsersPerSecond = 0 // 固定时钟下关闭节流，避免 pacer 死循环
	cyc := baseCycleEnd(now, p)
	iv := int64(p.Interval.Seconds())
	reader := newFakeReader()
	reader.setCycle(cyc, []int64{1})
	repo := newFakeRepo()
	repo.errByUser[1] = service.ErrRiskV2AssessmentConflict
	store := newFakeCycleStore()
	w := service.NewRiskV2ScoringWorker(service.RiskV2ScoringWorkerDeps{
		Reader: reader, Lease: &fakeLease{}, Health: healthyHealth(), Repo: repo, CycleStore: store,
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, Now: func() time.Time { return now },
	}, p)
	w.RunDueCycles(context.Background())
	require.Empty(t, store.addedTo(cyc+iv), "conflict is terminal, must not be retried")
	require.EqualValues(t, 1, w.Metrics().AssessmentConflicts)
}

// —— Redis 周期状态/重试 store（miniredis）——

func newStoreRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func TestCycleStore_SaveLoadOwnerOnly(t *testing.T) {
	rdb := newStoreRedis(t)
	ctx := context.Background()
	store := NewRiskV2CycleStore(rdb, "s1", "v1", time.Hour, time.Hour, nil)
	cyc := int64(1000)
	// §三：写周期状态需持有 lease。设 lease=tokenA。
	require.NoError(t, rdb.Set(ctx, riskV2LeaseKey("s1", "v1"), "tokenA", time.Hour).Err())

	ok, err := store.SaveCycleState(ctx, cyc, service.RiskV2CycleState{Status: service.RiskV2CycleRunning, Cursor: "42"}, "tokenA")
	require.NoError(t, err)
	require.True(t, ok)

	st, found, err := store.LoadCycleState(ctx, cyc)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, service.RiskV2CycleRunning, st.Status)
	require.Equal(t, "42", st.Cursor)

	// 不持有 lease 的 tokenB 不能改（lease 校验拒绝）。
	ok, err = store.SaveCycleState(ctx, cyc, service.RiskV2CycleState{Status: service.RiskV2CycleCompleted}, "tokenB")
	require.NoError(t, err)
	require.False(t, ok, "non-lease-holder must not modify cycle state")

	// 持有 lease 的 tokenA 可改。
	ok, err = store.SaveCycleState(ctx, cyc, service.RiskV2CycleState{Status: service.RiskV2CycleCompleted}, "tokenA")
	require.NoError(t, err)
	require.True(t, ok)
	st, _, _ = store.LoadCycleState(ctx, cyc)
	require.Equal(t, service.RiskV2CycleCompleted, st.Status)
}

// §三：新 leader 接管 INCOMPLETE；旧 leader（lease 已易主）Save/Complete 被拒。
func TestCycleStore_LeaseHandoffAndStaleReject(t *testing.T) {
	rdb := newStoreRedis(t)
	ctx := context.Background()
	store := NewRiskV2CycleStore(rdb, "s1", "v1", time.Hour, time.Hour, nil)
	cyc := int64(2000)
	leaseKey := riskV2LeaseKey("s1", "v1")

	// 旧 leader A 持有 lease，Claim + 写进度。
	require.NoError(t, rdb.Set(ctx, leaseKey, "A", time.Hour).Err())
	require.NoError(t, store.ClaimCycleState(ctx, cyc, "A"))
	ok, _ := store.SaveCycleState(ctx, cyc, service.RiskV2CycleState{Status: service.RiskV2CycleIncomplete, Cursor: "c1"}, "A")
	require.True(t, ok)

	// lease 易主给 B（A 崩溃/过期，B Acquire）。
	require.NoError(t, rdb.Set(ctx, leaseKey, "B", time.Hour).Err())

	// 旧 leader A 的 Save/Complete 被拒（lease 不再属于 A）。
	ok, _ = store.SaveCycleState(ctx, cyc, service.RiskV2CycleState{Status: service.RiskV2CycleCompleted}, "A")
	require.False(t, ok, "stale leader (lost lease) must not write/complete")

	// 新 leader B 可接管 INCOMPLETE（保留 cursor）并完成。
	require.NoError(t, store.ClaimCycleState(ctx, cyc, "B"))
	st, _, _ := store.LoadCycleState(ctx, cyc)
	require.Equal(t, "c1", st.Cursor, "new leader resumes from persisted cursor")
	ok, _ = store.SaveCycleState(ctx, cyc, service.RiskV2CycleState{Status: service.RiskV2CycleCompleted, Cursor: ""}, "B")
	require.True(t, ok, "new leader can complete")
}

// §三：Acquire 后、Claim 前 lease 过期 → Claim 被拒。
func TestCycleStore_ClaimRejectedWhenLeaseExpired(t *testing.T) {
	rdb := newStoreRedis(t)
	ctx := context.Background()
	store := NewRiskV2CycleStore(rdb, "s1", "v1", time.Hour, time.Hour, nil)
	// lease 不存在（已过期）→ Claim 用 token "A" 被拒。
	err := store.ClaimCycleState(ctx, 3000, "A")
	require.ErrorIs(t, err, ErrRiskV2LeaseNotHeld)
}

func TestCycleStore_StateTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	require.NoError(t, rdb.Set(ctx, riskV2LeaseKey("s1", "v1"), "tok", time.Hour).Err())
	store := NewRiskV2CycleStore(rdb, "s1", "v1", 30*time.Minute, time.Hour, nil)
	_, _ = store.SaveCycleState(ctx, 1000, service.RiskV2CycleState{Status: service.RiskV2CycleRunning}, "tok")
	ttl := mr.TTL("riskv2:s1:fp:v1:scoring:{coord}:cycle:1000")
	require.Greater(t, ttl, time.Duration(0))
}

func TestCycleStore_RetryAddLoadAndCap(t *testing.T) {
	rdb := newStoreRedis(t)
	ctx := context.Background()
	store := NewRiskV2CycleStore(rdb, "s1", "v1", time.Hour, time.Hour, nil)
	cyc := int64(2000)
	// 正常加入。
	overflow, err := store.AddRetryUsers(ctx, cyc, []service.RiskV2RetryUser{{UserID: 1, Attempts: 1}, {UserID: 2, Attempts: 2}})
	require.NoError(t, err)
	require.False(t, overflow)
	// attempts 超过 maxAttempts → 丢弃。
	_, err = store.AddRetryUsers(ctx, cyc, []service.RiskV2RetryUser{{UserID: 3, Attempts: riskV2RetryMaxAttempts + 1}})
	require.NoError(t, err)
	users, err := store.LoadRetryUsers(ctx, cyc, 100)
	require.NoError(t, err)
	ids := map[int64]int{}
	for _, u := range users {
		ids[u.UserID] = u.Attempts
	}
	require.Contains(t, ids, int64(1))
	require.Contains(t, ids, int64(2))
	require.NotContains(t, ids, int64(3), "attempts over cap must be dropped (no infinite retry)")
}

// —— §八 健康覆盖率：已知实例缺报 → HealthAvailable=false ——

func TestHealth_CoverageMissingInstanceBlocks(t *testing.T) {
	cycleEnd := time.Date(2026, 5, 1, 12, 5, 0, 0, time.UTC).Unix()
	rdb := newStoreRedis(t)
	ctx := context.Background()
	// inst-A 新鲜上报（reporting）；inst-B 只在 expected 窗口内心跳过、但当前已 stale（missing）。
	rA := NewRiskV2HealthReporter(rdb, "s1", "v1", "inst-A", func() time.Time { return time.Unix(cycleEnd, 0) })
	mustReport(t, rA, 1, service.RiskV2HealthDelta{Enqueued: 100, Processed: 100})
	// 手动把 inst-B 心跳设为 cycleEnd-200（>staleWindow 120，但 <expectedWindow 300）→ 缺报。
	require.NoError(t, rdb.ZAdd(ctx, "riskv2:s1:fp:v1:health:instances", redis.Z{Score: float64(cycleEnd - 200), Member: "inst-B"}).Err())

	reader := NewRiskV2IngestionHealthReader(rdb, "s1", "v1", func() time.Time { return time.Unix(cycleEnd, 0) })
	h, err := reader.ReadIngestionHealth(ctx, cycleEnd, 5)
	require.NoError(t, err)
	require.Equal(t, 2, h.ExpectedInstanceCount)
	require.Equal(t, 1, h.ReportingInstanceCount)
	require.Equal(t, 1, h.MissingInstanceCount)
	require.False(t, h.HealthAvailable, "missing instance coverage must block (HealthAvailable=false)")
}

// §八 全覆盖 → 健康可用。
func TestHealth_CoverageComplete(t *testing.T) {
	cycleEnd := time.Date(2026, 5, 1, 12, 5, 0, 0, time.UTC).Unix()
	rdb := newStoreRedis(t)
	ctx := context.Background()
	r := NewRiskV2HealthReporter(rdb, "s1", "v1", "inst-A", func() time.Time { return time.Unix(cycleEnd, 0) })
	mustReport(t, r, 1, service.RiskV2HealthDelta{Enqueued: 100, Processed: 100})
	reader := NewRiskV2IngestionHealthReader(rdb, "s1", "v1", func() time.Time { return time.Unix(cycleEnd, 0) })
	h, err := reader.ReadIngestionHealth(ctx, cycleEnd, 5)
	require.NoError(t, err)
	require.True(t, h.CoverageAvailable)
	require.Equal(t, 0, h.MissingInstanceCount)
	require.True(t, h.HealthAvailable)
}

// §八 Deregister（draining）后不再计入 expected。
func TestHealth_DeregisterDraining(t *testing.T) {
	cycleEnd := time.Date(2026, 5, 1, 12, 5, 0, 0, time.UTC).Unix()
	rdb := newStoreRedis(t)
	ctx := context.Background()
	rA := NewRiskV2HealthReporter(rdb, "s1", "v1", "inst-A", func() time.Time { return time.Unix(cycleEnd, 0) })
	rB := NewRiskV2HealthReporter(rdb, "s1", "v1", "inst-B", func() time.Time { return time.Unix(cycleEnd, 0) })
	mustReport(t, rA, 1, service.RiskV2HealthDelta{Enqueued: 100, Processed: 100})
	mustReport(t, rB, 1, service.RiskV2HealthDelta{Enqueued: 100, Processed: 100})
	require.NoError(t, rB.Deregister(ctx)) // 优雅缩容
	reader := NewRiskV2IngestionHealthReader(rdb, "s1", "v1", func() time.Time { return time.Unix(cycleEnd, 0) })
	h, err := reader.ReadIngestionHealth(ctx, cycleEnd, 5)
	require.NoError(t, err)
	require.Equal(t, 1, h.ExpectedInstanceCount, "drained instance excluded from expected")
	require.True(t, h.HealthAvailable)
}

// —— §七/§二 Reporter delta baseline + sequenced 幂等（ambiguous ACK 安全）——

// seqReporter 模拟 sequenced 幂等语义：每 seq 至多应用一次；可注入纯失败或「应用后返回 error」（ambiguous ACK）。
type seqReporter struct {
	mu            sync.Mutex
	applied       map[uint64]bool // 已应用的 seq（幂等）
	totalEnqueued int64           // 累计应用（用于验证不重不丢）
	ambiguousSeq  map[uint64]bool // 这些 seq 首次「应用后返回 error」
	failAll       bool            // 纯失败：不应用、返回 error
	appliedDeltas []service.RiskV2HealthDelta
}

func newSeqReporter() *seqReporter {
	return &seqReporter{applied: map[uint64]bool{}, ambiguousSeq: map[uint64]bool{}}
}

func (r *seqReporter) Report(ctx context.Context, seq uint64, d service.RiskV2HealthDelta) (service.RiskV2HealthReportResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failAll {
		return service.RiskV2HealthApplied, errors.New("redis down") // 未应用
	}
	if r.applied[seq] {
		return service.RiskV2HealthAlreadyApplied, nil // 幂等：不重复累加
	}
	r.applied[seq] = true
	r.totalEnqueued += d.Enqueued
	r.appliedDeltas = append(r.appliedDeltas, d)
	if r.ambiguousSeq[seq] {
		// 已在 Redis 应用，但客户端收到 timeout / 丢 ACK。
		return service.RiskV2HealthApplied, errors.New("timeout after apply")
	}
	return service.RiskV2HealthApplied, nil
}
func (r *seqReporter) Deregister(ctx context.Context) error { return nil }

type fakeStatsSrc struct{ s service.RiskV2Stats }

func (f *fakeStatsSrc) Stats() service.RiskV2Stats { return f.s }

// §七：纯失败不推进 baseline；成功后不丢累计差（无静默丢失）。
func TestHealthLoop_NoLossOnFailureRetry(t *testing.T) {
	src := &fakeStatsSrc{}
	rep := newSeqReporter()
	rep.failAll = true
	loop := service.NewRiskV2HealthReportLoop(src, rep, time.Hour, time.Now)

	src.s = service.RiskV2Stats{Enqueued: 100, Processed: 100}
	loop.FlushForTest() // 失败 → 未应用
	require.EqualValues(t, 0, rep.totalEnqueued)

	// 累计涨到 150，恢复成功 → 用同 pending seq 先补发 100，下一轮再发 50。合计 150，不丢。
	src.s = service.RiskV2Stats{Enqueued: 150, Processed: 150}
	rep.failAll = false
	loop.FlushForTest() // 补发 pending(100)
	loop.FlushForTest() // 发增量(50)
	require.EqualValues(t, 150, rep.totalEnqueued, "no silent loss across failure/retry")
}

// §二：ambiguous ACK —— 写已应用但客户端 timeout；同序号重试只累加一次，baseline 最终推进。
func TestHealthLoop_AmbiguousAckNoDoubleCount(t *testing.T) {
	src := &fakeStatsSrc{}
	rep := newSeqReporter()
	rep.ambiguousSeq[1] = true // seq1 首次应用后返回 error
	loop := service.NewRiskV2HealthReportLoop(src, rep, time.Hour, time.Now)

	src.s = service.RiskV2Stats{Enqueued: 100, Processed: 100}
	loop.FlushForTest() // seq1 应用 100，但返回 error → pending 保留
	require.EqualValues(t, 100, rep.totalEnqueued)

	loop.FlushForTest() // 用同 seq1 重试 → ALREADY_APPLIED，不重复累加 → baseline 推进
	require.EqualValues(t, 100, rep.totalEnqueued, "ambiguous ACK retry must NOT double count")

	// baseline 已推进：新增量 50 用 seq2 正常发出。
	src.s = service.RiskV2Stats{Enqueued: 150, Processed: 150}
	loop.FlushForTest()
	require.EqualValues(t, 150, rep.totalEnqueued)
	require.True(t, rep.applied[2], "baseline advanced → next report uses seq2")
}

// §十一.17：无 goroutine 泄漏 —— 用 fakes（无 redis 连接池干扰）启动 worker+health loop，Stop 后 goroutine 回落。
func TestRuntime_NoGoroutineLeak(t *testing.T) {
	baseline := runtime.NumGoroutine()

	reader := newFakeReader() // 空活跃集 → 周期立即完成
	worker := service.NewRiskV2ScoringWorker(service.RiskV2ScoringWorkerDeps{
		Reader: reader, Lease: &fakeLease{}, Health: healthyHealth(), Repo: newFakeRepo(),
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, // Now 默认 time.Now，interval=5m ticker
	}, defaultWorkerParams())
	require.True(t, worker.Ready())
	loop := service.NewRiskV2HealthReportLoop(&fakeStatsSrc{}, newSeqReporter(), time.Hour, time.Now)

	worker.Start(context.Background())
	loop.Start()
	time.Sleep(100 * time.Millisecond) // 让 goroutine 起来

	worker.Stop()
	loop.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.LessOrEqualf(t, runtime.NumGoroutine(), baseline+1,
		"goroutines must return to baseline after Stop (base=%d now=%d)", baseline, runtime.NumGoroutine())
}

var _ = config.RiskV2WorkerConfig{}

// 切片 5.1 §七：RuntimeStatus 并发安全 —— worker 持续更新状态 + 多并发读取快照，-race 无竞争。
func TestRuntimeStatus_ConcurrentRace(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 6, 5, 0, time.UTC)
	p := defaultWorkerParams()
	p.MaxUsersPerSecond = 0
	cyc := baseCycleEnd(now, p)
	reader := newFakeReader()
	reader.setCycle(cyc, []int64{1, 2, 3})
	dispatcher := service.NewRiskV2Dispatcher(16, &countingSinkR{})
	dispatcher.Start()
	defer func() { _ = dispatcher.Stop(context.Background()) }()
	w := service.NewRiskV2ScoringWorker(service.RiskV2ScoringWorkerDeps{
		Reader: reader, Lease: &fakeLease{}, Health: healthyHealth(), Repo: newFakeRepo(),
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, Now: func() time.Time { return now },
	}, p)
	require.True(t, w.Ready())

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// 写方：反复驱动周期（改 currentCycle/cursor/lastCycleStatus/leader）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				w.RunDueCycles(context.Background())
			}
		}
	}()
	// 读方：多并发组装 RuntimeStatus 快照。
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				st := dispatcher.Stats()
				status := service.BuildRiskV2RuntimeStatus(service.RiskV2RuntimeStatusInputs{
					Enabled: true, AggregationEnabled: true, ScoringEnabled: true,
					Worker: w, DispatcherStats: &st, NowUnix: now.Unix(),
				})
				// 字段自洽：LastCycleStatus 只能是稳定枚举之一。
				switch status.LastCycleStatus {
				case "", service.RiskV2CycleCompleted, service.RiskV2CycleIncomplete, service.RiskV2CycleRunning, service.RiskV2CyclePending:
				default:
					t.Errorf("unexpected LastCycleStatus %q", status.LastCycleStatus)
				}
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

type countingSinkR struct{ n int64 }

func (s *countingSinkR) Consume(env service.RiskFeatureEnvelope) {}
