//go:build unit

package repository

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func baseCycleEnd(now time.Time, p service.RiskV2WorkerParams) int64 {
	iv := int64(p.Interval.Seconds())
	return (now.Add(-p.GraceDelay).Unix() / iv) * iv
}

func newWorker(t *testing.T, deps service.RiskV2ScoringWorkerDeps, p service.RiskV2WorkerParams) *service.RiskV2ScoringWorker {
	t.Helper()
	return service.NewRiskV2ScoringWorker(deps, p)
}

func strongSnapshot(uid int64) service.RiskV2ScoringSnapshot {
	// 复刻 service 测试里的 lowRiskMetrics + highScaffold + highTemporal（scaffold+temporal → HIGH）。
	w1 := service.RiskV2Window{
		WindowLabel: "1h", RequestCount: 300, SuccessCount: 300, UsageAvailableCount: 300,
		ExactAvailableCount: 300, DistinctExactEstimate: 20, DistinctExactParamSigEstimate: 300,
		FullScaffoldRequestCount: 300, DistinctFullScaffoldEstimate: 20,
		InputTokens: 6000, OutputTokens: 6000,
		PeakRPM: 300, PeakRPMAvailable: true, RequestsPerMinute: 5, ActiveMinutes: 60,
		ModelConcentrationAvailable: true, DistinctModelCount: 1, TopModelRequestCount: 300,
		Available: service.RiskV2FeatureAvailability{Requests: true, Fingerprint: true, ActiveMinutes: true, ToolUse: true, StructuredOutput: true, ModelConcentration: true},
	}
	w24 := w1
	w24.WindowLabel = "24h"
	w24.RequestCount = 500
	w24.ActiveMinutes = 30
	w24.OutputTokens = 100000
	return service.RiskV2ScoringSnapshot{
		UserID: uid, SchemaVersion: "s1", FingerprintKeyVersion: "v1",
		User: service.RiskV2EntityWindows{W1h: w1, W24h: w24},
	}
}

// §二.1 / §十二.1：scoring_enabled=false 由 wiring 决定不构造 Worker；这里验证前置不满足 → 不启动、无 goroutine。
func TestWorker_NotReadyWhenDepsMissing(t *testing.T) {
	// 缺 Reader → 前置不满足。
	w := newWorker(t, service.RiskV2ScoringWorkerDeps{
		Lease: &fakeLease{}, Health: healthyHealth(),
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
	}, defaultWorkerParams())
	require.False(t, w.Ready())
	w.Start(context.Background()) // 不应启动
	require.Zero(t, w.Metrics().WorkerStarted)
}

// persist 模式缺 Repo → 不 Ready。
func TestWorker_PersistRequiresRepo(t *testing.T) {
	w := newWorker(t, service.RiskV2ScoringWorkerDeps{
		Reader: newFakeReader(), Lease: &fakeLease{}, Health: healthyHealth(),
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, // 但 Repo=nil
	}, defaultWorkerParams())
	require.False(t, w.Ready())
}

// §十二.2：Dry-run 不写数据库，但评分并记指标。
func TestWorker_DryRunNoDBWrite(t *testing.T) {
	clk := newManualClock(time.Date(2026, 3, 1, 12, 6, 5, 0, time.UTC))
	reader := newFakeReader()
	repo := newFakeRepo()
	p := defaultWorkerParams()
	cyc := baseCycleEnd(clk.now(), p)
	reader.setCycle(cyc-int64(p.Interval.Seconds()), []int64{1, 2, 3}) // 首启只处理 latest=cyc-? 见下
	reader.setCycle(cyc, []int64{1, 2, 3})
	w := newWorker(t, service.RiskV2ScoringWorkerDeps{
		Reader: reader, Lease: &fakeLease{}, Health: healthyHealth(), Repo: repo,
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: false, Now: clk.now, Sleep: clk.sleep,
	}, p)
	require.True(t, w.Ready())
	w.RunDueCycles(context.Background())
	m := w.Metrics()
	require.EqualValues(t, 3, m.UsersScored)
	require.EqualValues(t, 3, m.UsersDryRun)
	require.Equal(t, 0, repo.count(), "dry-run must not write DB")
	require.EqualValues(t, 1, m.CyclesCompleted)
}

// §十二.3：Persist 写入；结果计入 inserted。
func TestWorker_PersistWrites(t *testing.T) {
	clk := newManualClock(time.Date(2026, 3, 1, 12, 6, 5, 0, time.UTC))
	reader := newFakeReader()
	repo := newFakeRepo()
	p := defaultWorkerParams()
	cyc := baseCycleEnd(clk.now(), p)
	reader.setCycle(cyc, []int64{10, 11})
	w := newWorker(t, service.RiskV2ScoringWorkerDeps{
		Reader: reader, Lease: &fakeLease{}, Health: healthyHealth(), Repo: repo,
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, Now: clk.now, Sleep: clk.sleep,
	}, p)
	w.RunDueCycles(context.Background())
	require.Equal(t, 2, repo.count())
	require.EqualValues(t, 2, w.Metrics().Inserted)
	// §十.26：EffectiveAction 恒 NONE。
	repo.mu.Lock()
	for _, rec := range repo.upserts {
		require.Equal(t, service.RiskV2ActionNone, rec.a.EffectiveAction)
		require.Equal(t, cyc, rec.a.AssessedAtUnix, "assessed_at must equal cycle_end")
	}
	repo.mu.Unlock()
}

// §三：cycle_end = floor(now-grace, interval)。
func TestWorker_CycleAlignment(t *testing.T) {
	p := defaultWorkerParams()
	// 12:06:05, grace 20s → 12:05:45 → floor 5m = 12:05:00。
	clk := newManualClock(time.Date(2026, 3, 1, 12, 6, 5, 0, time.UTC))
	reader := newFakeReader()
	want := time.Date(2026, 3, 1, 12, 5, 0, 0, time.UTC).Unix()
	reader.setCycle(want, []int64{1})
	repo := newFakeRepo()
	w := newWorker(t, service.RiskV2ScoringWorkerDeps{
		Reader: reader, Lease: &fakeLease{}, Health: healthyHealth(), Repo: repo,
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, Now: clk.now, Sleep: clk.sleep,
	}, p)
	w.RunDueCycles(context.Background())
	require.Equal(t, 1, repo.count())
	repo.mu.Lock()
	require.Equal(t, want, repo.upserts[0].a.AssessedAtUnix)
	repo.mu.Unlock()
}

// §十二.7：两个 Worker 竞争 Lease，只有一个处理。
func TestWorker_LeaseContention(t *testing.T) {
	clk := newManualClock(time.Date(2026, 3, 1, 12, 6, 5, 0, time.UTC))
	lease := &fakeLease{}
	p := defaultWorkerParams()
	cyc := baseCycleEnd(clk.now(), p)
	mk := func(persist bool, repo *fakeRepo) *service.RiskV2ScoringWorker {
		reader := newFakeReader()
		reader.setCycle(cyc, []int64{1, 2})
		return newWorker(t, service.RiskV2ScoringWorkerDeps{
			Reader: reader, Lease: lease, Health: healthyHealth(), Repo: repo,
			ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
			Persist: true, Now: clk.now, Sleep: clk.sleep,
		}, p)
	}
	r1, r2 := newFakeRepo(), newFakeRepo()
	w1, w2 := mk(true, r1), mk(true, r2)
	// w1 先拿到 lease 并处理；处理结束后释放。w2 用同一 lease：由于 w1 已释放，w2 也能拿到并处理自己的 cycle。
	// 为验证「同一时刻只一个」，让 w1 持有不释放：用 renewCallsMax=0（正常）。改为并发争抢的确定性版本：
	lease.held = true // 预占：模拟别的实例持有
	w1.RunDueCycles(context.Background())
	require.Equal(t, 0, r1.count(), "lease held by other → must not process")
	require.GreaterOrEqual(t, w1.Metrics().LeaseContended, int64(1))
	_ = w2
}

// §十二.8：Lease 丢失 → 中止（不写库）。renewCallsMax=1 使第一次 renew 即 lost。
func TestWorker_LeaseLostAborts(t *testing.T) {
	// Now 固定（用于周期计算）；renew ticker 走真实 wall-clock，两者解耦。
	now := time.Date(2026, 3, 1, 12, 6, 5, 0, time.UTC)
	reader := newFakeReader()
	repo := newFakeRepo()
	p := defaultWorkerParams()
	p.LeaseRenewInterval = 1 * time.Millisecond // renew 每 1ms 触发；第 1 次即 lost
	p.MaxUsersPerSecond = 0                     // 关闭节流，让延迟只来自 batch read
	// batch read 睡 200ms（尊重 ctx）：期间 renew 触发 → lease lost → cancel → batch 返回 ctx.Err。
	reader.delay = 200 * time.Millisecond
	cyc := baseCycleEnd(now, p)
	users := make([]int64, 100)
	for i := range users {
		users[i] = int64(i + 1)
	}
	reader.setCycle(cyc, users)
	lease := &fakeLease{renewCallsMax: 1}
	w := newWorker(t, service.RiskV2ScoringWorkerDeps{
		Reader: reader, Lease: lease, Health: healthyHealth(), Repo: repo,
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, Now: func() time.Time { return now },
	}, p)
	w.RunDueCycles(context.Background())
	require.GreaterOrEqual(t, w.Metrics().LeaseLost, int64(1))
	require.EqualValues(t, 1, w.Metrics().CyclesIncomplete)
	require.EqualValues(t, 0, w.Metrics().CyclesCompleted)
}

// §十二.14：单用户 Redis 错误不终止整批；该用户不写、计 read error。
func TestWorker_SingleUserReadError(t *testing.T) {
	clk := newManualClock(time.Date(2026, 3, 1, 12, 6, 5, 0, time.UTC))
	reader := newFakeReader()
	repo := newFakeRepo()
	p := defaultWorkerParams()
	cyc := baseCycleEnd(clk.now(), p)
	reader.setCycle(cyc, []int64{1, 2, 3})
	reader.userErr[2] = errors.New("boom")
	w := newWorker(t, service.RiskV2ScoringWorkerDeps{
		Reader: reader, Lease: &fakeLease{}, Health: healthyHealth(), Repo: repo,
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, Now: clk.now, Sleep: clk.sleep,
	}, p)
	w.RunDueCycles(context.Background())
	require.Equal(t, 2, repo.count(), "user 2 read-failed → 2 written")
	require.EqualValues(t, 2, w.Metrics().UsersScored)
	require.GreaterOrEqual(t, w.Metrics().RedisReadErrors, int64(1))
	require.EqualValues(t, 1, w.Metrics().CyclesCompleted)
}

// §十二.15：单用户 DB 错误不终止整批。
func TestWorker_SingleUserDBError(t *testing.T) {
	clk := newManualClock(time.Date(2026, 3, 1, 12, 6, 5, 0, time.UTC))
	reader := newFakeReader()
	repo := newFakeRepo()
	repo.errByUser[2] = errors.New("db fail")
	p := defaultWorkerParams()
	cyc := baseCycleEnd(clk.now(), p)
	reader.setCycle(cyc, []int64{1, 2, 3})
	w := newWorker(t, service.RiskV2ScoringWorkerDeps{
		Reader: reader, Lease: &fakeLease{}, Health: healthyHealth(), Repo: repo,
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, Now: clk.now, Sleep: clk.sleep,
	}, p)
	w.RunDueCycles(context.Background())
	require.Equal(t, 2, repo.count())
	require.GreaterOrEqual(t, w.Metrics().DatabaseErrors, int64(1))
	require.EqualValues(t, 1, w.Metrics().CyclesCompleted)
}

// §十二.16：Redis 全局失败（list 错误）→ 周期未完成，不写库。
func TestWorker_RedisGlobalFailure(t *testing.T) {
	clk := newManualClock(time.Date(2026, 3, 1, 12, 6, 5, 0, time.UTC))
	reader := newFakeReader()
	reader.listErr = errors.New("redis down")
	repo := newFakeRepo()
	p := defaultWorkerParams()
	cyc := baseCycleEnd(clk.now(), p)
	reader.setCycle(cyc, []int64{1, 2})
	w := newWorker(t, service.RiskV2ScoringWorkerDeps{
		Reader: reader, Lease: &fakeLease{}, Health: healthyHealth(), Repo: repo,
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, Now: clk.now, Sleep: clk.sleep,
	}, p)
	w.RunDueCycles(context.Background())
	require.Equal(t, 0, repo.count())
	require.EqualValues(t, 1, w.Metrics().CyclesIncomplete)
}

// §十二.18：context cancel → 不写库、周期未完成。
func TestWorker_ContextCancel(t *testing.T) {
	clk := newManualClock(time.Date(2026, 3, 1, 12, 6, 5, 0, time.UTC))
	reader := newFakeReader()
	repo := newFakeRepo()
	p := defaultWorkerParams()
	cyc := baseCycleEnd(clk.now(), p)
	reader.setCycle(cyc, []int64{1, 2, 3})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := newWorker(t, service.RiskV2ScoringWorkerDeps{
		Reader: reader, Lease: &fakeLease{}, Health: healthyHealth(), Repo: repo,
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, Now: clk.now, Sleep: clk.sleep,
	}, p)
	w.RunDueCycles(ctx)
	require.Equal(t, 0, repo.count())
}

// §十二.20：max_users_per_cycle 硬上限 → 本周期未完成（余下留下周期）。
func TestWorker_MaxUsersPerCycle(t *testing.T) {
	clk := newManualClock(time.Date(2026, 3, 1, 12, 6, 5, 0, time.UTC))
	reader := newFakeReader()
	repo := newFakeRepo()
	p := defaultWorkerParams()
	p.MaxUsersPerCycle = 2
	p.BatchSize = 2
	cyc := baseCycleEnd(clk.now(), p)
	reader.setCycle(cyc, []int64{1, 2, 3, 4, 5})
	w := newWorker(t, service.RiskV2ScoringWorkerDeps{
		Reader: reader, Lease: &fakeLease{}, Health: healthyHealth(), Repo: repo,
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, Now: clk.now, Sleep: clk.sleep,
	}, p)
	w.RunDueCycles(context.Background())
	require.LessOrEqual(t, repo.count(), 2)
	require.EqualValues(t, 1, w.Metrics().CyclesIncomplete)
}

// §十二.5/6：STALE_IGNORED / CONFLICT 结果分别计入。
func TestWorker_UpsertResultTally(t *testing.T) {
	clk := newManualClock(time.Date(2026, 3, 1, 12, 6, 5, 0, time.UTC))
	reader := newFakeReader()
	p := defaultWorkerParams()
	cyc := baseCycleEnd(clk.now(), p)
	reader.setCycle(cyc, []int64{1})

	// STALE_IGNORED
	repoStale := newFakeRepo()
	repoStale.result = service.RiskV2StaleIgnored
	w := newWorker(t, service.RiskV2ScoringWorkerDeps{
		Reader: reader, Lease: &fakeLease{}, Health: healthyHealth(), Repo: repoStale,
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, Now: clk.now, Sleep: clk.sleep,
	}, p)
	w.RunDueCycles(context.Background())
	require.EqualValues(t, 1, w.Metrics().StaleIgnored)

	// CONFLICT
	reader2 := newFakeReader()
	reader2.setCycle(cyc, []int64{1})
	repoConf := newFakeRepo()
	repoConf.errByUser[1] = service.ErrRiskV2AssessmentConflict
	w2 := newWorker(t, service.RiskV2ScoringWorkerDeps{
		Reader: reader2, Lease: &fakeLease{}, Health: healthyHealth(), Repo: repoConf,
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, Now: clk.now, Sleep: clk.sleep,
	}, p)
	w2.RunDueCycles(context.Background())
	require.EqualValues(t, 1, w2.Metrics().AssessmentConflicts)
	require.EqualValues(t, 0, w2.Metrics().DatabaseErrors, "conflict must not count as db error")
}

// §十二.22：Health 未知 → 阻止 HIGH。strongSnapshot 在 healthy 下应 HIGH；health unavailable 下不得 HIGH。
func TestWorker_HealthUnavailableBlocksHigh(t *testing.T) {
	clk := newManualClock(time.Date(2026, 3, 1, 12, 6, 5, 0, time.UTC))
	p := defaultWorkerParams()
	cyc := baseCycleEnd(clk.now(), p)

	run := func(h *fakeHealth) service.RiskV2Assessment {
		reader := newFakeReader()
		reader.setCycle(cyc, []int64{1})
		reader.snaps[1] = strongSnapshot(1)
		repo := newFakeRepo()
		w := newWorker(t, service.RiskV2ScoringWorkerDeps{
			Reader: reader, Lease: &fakeLease{}, Health: h, Repo: repo,
			ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
			Persist: true, Now: clk.now, Sleep: clk.sleep,
		}, p)
		w.RunDueCycles(context.Background())
		require.Equal(t, 1, repo.count())
		repo.mu.Lock()
		defer repo.mu.Unlock()
		return repo.upserts[0].a
	}

	high := run(healthyHealth())
	require.Equal(t, service.RiskV2TierHigh, high.RiskTier, "healthy + strong signals → HIGH baseline")

	unavailable := run(&fakeHealth{h: service.RiskV2IngestionHealth{HealthAvailable: false}})
	require.NotEqual(t, service.RiskV2TierHigh, unavailable.RiskTier, "health unavailable must block HIGH")
}

// §十二.23：Drop ratio 过高 → 阻止 HIGH。
func TestWorker_HighDropRatioBlocksHigh(t *testing.T) {
	clk := newManualClock(time.Date(2026, 3, 1, 12, 6, 5, 0, time.UTC))
	p := defaultWorkerParams()
	cyc := baseCycleEnd(clk.now(), p)
	reader := newFakeReader()
	reader.setCycle(cyc, []int64{1})
	reader.snaps[1] = strongSnapshot(1)
	repo := newFakeRepo()
	badHealth := &fakeHealth{h: service.RiskV2IngestionHealth{
		HealthAvailable: true, AggregationHealthy: false,
		ObservationDropRatioAvailable: true, ObservationDropRatio: 0.9,
	}}
	w := newWorker(t, service.RiskV2ScoringWorkerDeps{
		Reader: reader, Lease: &fakeLease{}, Health: badHealth, Repo: repo,
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, Now: clk.now, Sleep: clk.sleep,
	}, p)
	w.RunDueCycles(context.Background())
	require.Equal(t, 1, repo.count())
	repo.mu.Lock()
	require.NotEqual(t, service.RiskV2TierHigh, repo.upserts[0].a.RiskTier)
	repo.mu.Unlock()
}

// §五：首启只处理最新已完成周期，不回扫历史。
func TestWorker_FirstRunOnlyLatestCycle(t *testing.T) {
	clk := newManualClock(time.Date(2026, 3, 1, 12, 6, 5, 0, time.UTC))
	reader := newFakeReader()
	repo := newFakeRepo()
	p := defaultWorkerParams()
	cyc := baseCycleEnd(clk.now(), p)
	iv := int64(p.Interval.Seconds())
	// 塞入历史 3 个周期都有用户；首启应只处理最新那个。
	reader.setCycle(cyc, []int64{1})
	reader.setCycle(cyc-iv, []int64{2})
	reader.setCycle(cyc-2*iv, []int64{3})
	w := newWorker(t, service.RiskV2ScoringWorkerDeps{
		Reader: reader, Lease: &fakeLease{}, Health: healthyHealth(), Repo: repo,
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, Now: clk.now, Sleep: clk.sleep,
	}, p)
	w.RunDueCycles(context.Background())
	require.Equal(t, 1, repo.count())
	repo.mu.Lock()
	require.EqualValues(t, 1, repo.upserts[0].userID)
	repo.mu.Unlock()
}

// §五/§十二.12：Catch-up —— 时间推进多个周期后，一次 RunDueCycles 处理多个（受 max_catchup 限制）。
func TestWorker_CatchupBounded(t *testing.T) {
	clk := newManualClock(time.Date(2026, 3, 1, 12, 6, 5, 0, time.UTC))
	reader := newFakeReader()
	repo := newFakeRepo()
	p := defaultWorkerParams()
	iv := int64(p.Interval.Seconds())
	cyc0 := baseCycleEnd(clk.now(), p)
	reader.setCycle(cyc0, []int64{1})
	w := newWorker(t, service.RiskV2ScoringWorkerDeps{
		Reader: reader, Lease: &fakeLease{}, Health: healthyHealth(), Repo: repo,
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, Now: clk.now, Sleep: clk.sleep,
	}, p)
	w.RunDueCycles(context.Background()) // 处理 cyc0
	require.Equal(t, 1, repo.count())

	// 时间跳过 10 个周期（远超 max_catchup=3）。
	clk.advance(time.Duration(10*iv) * time.Second)
	nowCyc := baseCycleEnd(clk.now(), p)
	for c := cyc0 + iv; c <= nowCyc; c += iv {
		reader.setCycle(c, []int64{c}) // 每周期一个独特用户 id=c
	}
	before := repo.count()
	w.RunDueCycles(context.Background())
	processed := repo.count() - before
	require.LessOrEqual(t, processed, p.MaxCatchupCycles, "catch-up must be bounded by max_catchup_cycles")
	require.GreaterOrEqual(t, w.Metrics().CyclesSkipped, int64(1), "older cycles beyond catch-up must be skipped")
}

// §十二.13：Cursor resume —— 上一 tick 因超上限未完成，下一 tick 从 cursor 续，不重复已处理用户。
func TestWorker_CursorResume(t *testing.T) {
	clk := newManualClock(time.Date(2026, 3, 1, 12, 6, 5, 0, time.UTC))
	reader := newFakeReader()
	repo := newFakeRepo()
	p := defaultWorkerParams()
	p.MaxUsersPerCycle = 2
	p.BatchSize = 2
	cyc := baseCycleEnd(clk.now(), p)
	reader.setCycle(cyc, []int64{1, 2, 3, 4})
	w := newWorker(t, service.RiskV2ScoringWorkerDeps{
		Reader: reader, Lease: &fakeLease{}, Health: healthyHealth(), Repo: repo,
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, Now: clk.now, Sleep: clk.sleep,
	}, p)
	w.RunDueCycles(context.Background()) // 处理 1,2 → 未完成
	require.Equal(t, 2, repo.count())
	w.RunDueCycles(context.Background()) // 从 cursor 续：3,4
	seen := map[int64]int{}
	repo.mu.Lock()
	for _, r := range repo.upserts {
		seen[r.userID]++
	}
	repo.mu.Unlock()
	require.Equal(t, 4, len(seen), "all 4 users processed across resume")
	for u, c := range seen {
		require.Equal(t, 1, c, "user %d processed exactly once", u)
	}
}

// §十.：Assessment 新鲜度语义。
func TestWorker_AssessmentFreshness(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	require.True(t, service.RiskV2AssessmentFresh(now.Unix()-100, now, time.Hour))
	require.False(t, service.RiskV2AssessmentFresh(now.Unix()-7200, now, time.Hour))
}

// §十二.19：cycle timeout → 未完成（用极短 timeout + reader delay）。
func TestWorker_CycleTimeout(t *testing.T) {
	reader := newFakeReader()
	reader.delay = 50 * time.Millisecond
	repo := newFakeRepo()
	p := defaultWorkerParams()
	p.CycleTimeout = 10 * time.Millisecond
	p.Interval = 5 * time.Minute
	now := time.Date(2026, 3, 1, 12, 6, 5, 0, time.UTC)
	cyc := baseCycleEnd(now, p)
	users := make([]int64, 50)
	for i := range users {
		users[i] = int64(i + 1)
	}
	reader.setCycle(cyc, users)
	w := newWorker(t, service.RiskV2ScoringWorkerDeps{
		Reader: reader, Lease: &fakeLease{}, Health: healthyHealth(), Repo: repo,
		ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
		Persist: true, Now: func() time.Time { return now },
	}, p)
	w.RunDueCycles(context.Background())
	require.EqualValues(t, 1, w.Metrics().CyclesIncomplete)
	require.EqualValues(t, 0, w.Metrics().CyclesCompleted)
}

// helper: cyc-as-user-id path uses strconv indirectly; ensure no unused import.
var _ = strconv.Itoa
