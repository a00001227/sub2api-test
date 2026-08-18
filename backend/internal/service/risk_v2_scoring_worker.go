package service

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// 切片 4：Risk V2 Shadow Scoring Worker —— 定时自动评分 + 可选 Shadow 持久化。
//
// 只观测：EffectiveAction 恒 NONE；绝不限速/封禁/暂停/切换账号或代理。多实例下由 Leader Lease 保证唯一主动扫描。
// 全部参数保守未校准；localhost benchmark 数字不是生产容量承诺，故引入 Token Bucket 平滑节流。
//
// 时钟（now/sleep）注入，单元测试不依赖真实时间与真实 ticker。

// RiskV2WorkerParams 是归一化后的 Worker 运行参数（由 config.RiskV2WorkerConfig 转换）。
type RiskV2WorkerParams struct {
	Interval             time.Duration
	GraceDelay           time.Duration
	CycleTimeout         time.Duration
	BatchSize            int
	MaxUsersPerSecond    int
	MaxUsersPerCycle     int
	MaxReadErrorRatio    float64
	MaxDBErrorRatio      float64
	LeaseTTL             time.Duration
	LeaseRenewInterval   time.Duration
	MaxCatchupCycles     int
	AssessmentStaleAfter time.Duration
}

func (p RiskV2WorkerParams) batchSizeOr() int {
	if p.BatchSize <= 0 {
		return 100
	}
	return p.BatchSize
}

// RiskV2ScoringWorkerDeps 是 Worker 的依赖（全部接口，便于以 fake 做确定性单测）。
type RiskV2ScoringWorkerDeps struct {
	Reader                RiskV2ScoringReader
	Repo                  UserRiskV2Repository // persist 模式必需；dry-run 可为 nil
	Lease                 RiskV2Lease
	Health                RiskV2IngestionHealthReader
	CycleStore            RiskV2CycleStore // 可选：周期状态/重试持久化；nil → 纯内存 cursor
	ScoringConfig         RiskV2ScoringConfig
	FingerprintKeyVersion string

	// Persist=true → 写 user_risk_v2；false → Dry-Run（只记指标）。
	Persist bool
	// EnabledFn 动态开关：返回 false 时 Worker 立即中止当前周期（V2 被动态关闭）。可为 nil（视为恒开）。
	EnabledFn func() bool
	// MetricsSink 在每轮 RunDueCycles 结束后收到低基数指标快照（无 user_id/api_key label）。
	// 接线侧可接入现有 Metrics 体系或有频率限制的结构化摘要日志。可为 nil。
	MetricsSink func(RiskV2WorkerMetricsSnapshot)

	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error
}

// RiskV2ScoringWorker 是 Shadow Scoring Worker。Start/Stop 幂等；disabled/前置不满足时不启动 goroutine。
type RiskV2ScoringWorker struct {
	deps    RiskV2ScoringWorkerDeps
	params  RiskV2WorkerParams
	pacer   *RiskV2Pacer
	metrics *RiskV2WorkerMetrics
	now     func() time.Time

	mu                 sync.Mutex
	lastCompletedCycle int64
	currentCycle       int64
	cursor             string
	initialized        bool
	lastCycleStatus    string // §五 只读状态：最近一个周期的结果

	leader   int32 // 1=当前持有 lease
	degraded int32
	started  int32
	cancel   context.CancelFunc
	doneCh   chan struct{}
}

// RiskV2WorkerStatusView 是 Worker 的低基数只读状态视图（无 user_id / lease token / 敏感值）。
type RiskV2WorkerStatusView struct {
	Ready              bool
	Degraded           bool
	Leader             bool
	CurrentCycle       int64
	LastCompletedCycle int64
	LastCycleStatus    string
}

// StatusSnapshot 返回 Worker 只读状态（供 RuntimeStatus 汇总；不含敏感信息）。nil-safe。
func (w *RiskV2ScoringWorker) StatusSnapshot() RiskV2WorkerStatusView {
	if w == nil {
		return RiskV2WorkerStatusView{}
	}
	w.mu.Lock()
	cur, last, st := w.currentCycle, w.lastCompletedCycle, w.lastCycleStatus
	w.mu.Unlock()
	return RiskV2WorkerStatusView{
		Ready:              w.Ready(),
		Degraded:           atomic.LoadInt32(&w.degraded) == 1,
		Leader:             atomic.LoadInt32(&w.leader) == 1,
		CurrentCycle:       cur,
		LastCompletedCycle: last,
		LastCycleStatus:    st,
	}
}

// NewRiskV2ScoringWorker 构造 Worker（未启动）。前置不满足 → Ready()=false，Start() 不启动 goroutine。
func NewRiskV2ScoringWorker(deps RiskV2ScoringWorkerDeps, params RiskV2WorkerParams) *RiskV2ScoringWorker {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Sleep == nil {
		deps.Sleep = ctxSleep
	}
	w := &RiskV2ScoringWorker{
		deps:    deps,
		params:  params,
		metrics: &RiskV2WorkerMetrics{},
		now:     deps.Now,
		doneCh:  make(chan struct{}),
	}
	w.pacer = NewRiskV2Pacer(params.MaxUsersPerSecond, deps.Now, deps.Sleep)
	if !w.preconditionsOK() {
		atomic.StoreInt32(&w.degraded, 1)
	}
	return w
}

// preconditionsOK：§二 前置。ScoringReader/Lease/Health 可用；persist 模式 Repo 可用；scoring config 有效；fpVer 有效。
func (w *RiskV2ScoringWorker) preconditionsOK() bool {
	if w.deps.Reader == nil || w.deps.Lease == nil || w.deps.Health == nil {
		return false
	}
	if w.deps.Persist && w.deps.Repo == nil {
		return false
	}
	if w.deps.FingerprintKeyVersion == "" {
		return false
	}
	if err := w.deps.ScoringConfig.Validate(); err != nil {
		return false
	}
	if w.params.Interval <= 0 || w.params.LeaseTTL <= 0 {
		return false
	}
	return true
}

// Ready 表示前置满足、可作为 Worker 启动（false → DEGRADED，不启动）。
func (w *RiskV2ScoringWorker) Ready() bool { return atomic.LoadInt32(&w.degraded) == 0 }

// Metrics 返回运行指标快照。
func (w *RiskV2ScoringWorker) Metrics() RiskV2WorkerMetricsSnapshot { return w.metrics.snapshot() }

// Start 启动周期 ticker（幂等）。前置不满足 → 不启动 goroutine、标记 DEGRADED、返回。
func (w *RiskV2ScoringWorker) Start(ctx context.Context) {
	if w == nil || !w.Ready() {
		return
	}
	if !atomic.CompareAndSwapInt32(&w.started, 0, 1) {
		return
	}
	w.metrics.inc(&w.metrics.workerStarted)
	runCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	go w.loop(runCtx)
}

func (w *RiskV2ScoringWorker) loop(ctx context.Context) {
	defer close(w.doneCh)
	w.metrics.inc(&w.metrics.workerReady)
	t := time.NewTicker(w.params.Interval)
	defer t.Stop()
	// 启动即跑一次（处理最新已完成周期），随后每 interval 跑一次。
	w.RunDueCycles(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.RunDueCycles(ctx)
		}
	}
}

// Stop 停止（幂等），有限等待循环退出。
func (w *RiskV2ScoringWorker) Stop() {
	if w == nil || atomic.LoadInt32(&w.started) == 0 {
		return
	}
	if w.cancel != nil {
		w.cancel()
	}
	select {
	case <-w.doneCh:
	case <-time.After(5 * time.Second):
	}
}

// cycleEndFor 计算给定 wall 时间的目标周期结束点：floor(now - grace, interval)。
func (w *RiskV2ScoringWorker) cycleEndFor(now time.Time) int64 {
	iv := int64(w.params.Interval.Seconds())
	if iv <= 0 {
		return 0
	}
	t := now.Add(-w.params.GraceDelay).Unix()
	return (t / iv) * iv
}

// RunDueCycles 处理所有到期周期（含有界 catch-up）。测试可直接调用（配合注入时钟）以确定性驱动。
func (w *RiskV2ScoringWorker) RunDueCycles(ctx context.Context) {
	if w.deps.MetricsSink != nil {
		defer func() { w.deps.MetricsSink(w.metrics.snapshot()) }()
	}
	if !w.Ready() {
		return
	}
	if w.deps.EnabledFn != nil && !w.deps.EnabledFn() {
		return // V2 被动态关闭：不评分、不读、不写
	}
	iv := int64(w.params.Interval.Seconds())
	target := w.cycleEndFor(w.now())
	if target <= 0 {
		return
	}

	w.mu.Lock()
	if !w.initialized {
		// 初次启动：只处理最新已完成周期，不回扫无限历史。
		w.lastCompletedCycle = target - iv
		w.initialized = true
	}
	last := w.lastCompletedCycle
	w.mu.Unlock()

	if target <= last {
		return // 无新周期
	}

	start := last + iv
	// 有界 catch-up：最多处理最近 MaxCatchupCycles 个周期。
	minStart := target - int64(w.params.MaxCatchupCycles-1)*iv
	if start < minStart {
		skipped := (minStart - start) / iv
		w.metrics.add(&w.metrics.cyclesSkipped, skipped)
		w.mu.Lock()
		w.lastCompletedCycle = minStart - iv // 放弃过老周期，避免无限热循环
		w.mu.Unlock()
		start = minStart
	}

	processed := 0
	for cyc := start; cyc <= target; cyc += iv {
		if ctx.Err() != nil {
			return
		}
		if w.deps.EnabledFn != nil && !w.deps.EnabledFn() {
			return
		}
		if processed > 0 {
			w.metrics.inc(&w.metrics.catchupCycles)
		}
		completed := w.processCycleWithLease(ctx, cyc)
		processed++
		if !completed {
			// 未完成（超时/lease 丢失/熔断）：保留 cursor，下个 tick 继续本周期，不推进 lastCompletedCycle。
			return
		}
		w.mu.Lock()
		w.lastCompletedCycle = cyc
		w.currentCycle = 0
		w.cursor = ""
		w.mu.Unlock()
	}
}

// processCycleWithLease 获取 Leader Lease 并处理一个周期。返回是否「完整完成」。
func (w *RiskV2ScoringWorker) processCycleWithLease(ctx context.Context, cycleEnd int64) bool {
	w.metrics.inc(&w.metrics.cyclesStarted)

	acqCtx, acqCancel := context.WithTimeout(ctx, 3*time.Second)
	token, ok, err := w.deps.Lease.Acquire(acqCtx, w.params.LeaseTTL)
	acqCancel()
	if err != nil {
		w.metrics.inc(&w.metrics.leaseContended)
		w.metrics.inc(&w.metrics.cyclesSkipped)
		return false
	}
	if !ok {
		w.metrics.inc(&w.metrics.leaseContended)
		w.metrics.inc(&w.metrics.cyclesSkipped)
		return false // 非 leader：不处理，等待下次
	}
	w.metrics.inc(&w.metrics.leaseAcquired)
	atomic.StoreInt32(&w.leader, 1)
	defer atomic.StoreInt32(&w.leader, 0)

	cycleCtx, cancel := context.WithTimeout(ctx, w.params.CycleTimeout)
	defer cancel()

	var leaseLost int32
	renewDone := make(chan struct{})
	go w.renewLoop(cycleCtx, token, cancel, &leaseLost, renewDone)

	completed := w.runCycle(cycleCtx, cycleEnd, token, &leaseLost)

	cancel()    // 停 renew loop
	<-renewDone // 等 renew goroutine 退出（无泄漏）
	// 用独立短 ctx 释放 lease（cycleCtx 可能已取消）。
	relCtx, relCancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = w.deps.Lease.Release(relCtx, token)
	relCancel()

	w.mu.Lock()
	if completed {
		w.lastCycleStatus = RiskV2CycleCompleted
	} else {
		w.lastCycleStatus = RiskV2CycleIncomplete
	}
	w.mu.Unlock()
	if completed {
		w.metrics.inc(&w.metrics.cyclesCompleted)
	} else {
		w.metrics.inc(&w.metrics.cyclesIncomplete)
	}
	return completed
}

func (w *RiskV2ScoringWorker) renewLoop(ctx context.Context, token string, cancel context.CancelFunc, leaseLost *int32, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(w.params.LeaseRenewInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rc, rcCancel := context.WithTimeout(context.Background(), 2*time.Second)
			ok, err := w.deps.Lease.Renew(rc, token, w.params.LeaseTTL)
			rcCancel()
			if err != nil || !ok {
				atomic.StoreInt32(leaseLost, 1)
				w.metrics.inc(&w.metrics.leaseLost)
				cancel() // 立即中止当前周期，不再写库
				return
			}
		}
	}
}

// cycleRun 持有单周期运行时状态（含 retry 收集）。
type cycleRun struct {
	usersRead, usersScored int64
	readErrors, dbErrors   int64
	usersThisCycle         int64
	pacingSleep            time.Duration
	processed              map[int64]bool // 本 run 已处理用户（retry∩active 去重）
	attempts               map[int64]int  // 用户当前 retry 次数（来自 retry 集，active 默认 0）
	failedTransient        map[int64]int  // 失败可重试用户 → 下一次 attempts
}

// runCycle 处理单个周期：加载状态/恢复 cursor → retry 用户 → 活跃用户分页 → 节流 → 批量读 → 评分 → dry-run/persist。
// token 用于 owner-only 状态写与 lease 复核。返回是否完整完成。
func (w *RiskV2ScoringWorker) runCycle(ctx context.Context, cycleEnd int64, token string, leaseLost *int32) bool {
	startT := w.now()

	// 周期 Ingestion Health（阻止 HIGH 的关键输入）。
	windowMin := int(w.params.Interval.Minutes())
	if windowMin < 1 {
		windowMin = 1
	}
	health, herr := w.deps.Health.ReadIngestionHealth(ctx, cycleEnd, windowMin)
	if herr != nil || !health.HealthAvailable {
		w.metrics.inc(&w.metrics.healthUnavailable)
		// 不中止：ScoreRiskV2 在 HealthAvailable=false 时会自行阻止 HIGH。
	}

	// 加载持久化周期状态（若配置了 store）：COMPLETED → 跳过；否则从 cursor 恢复。
	cursor := ""
	resumed := false
	if w.deps.CycleStore != nil {
		if st, ok, lerr := w.deps.CycleStore.LoadCycleState(ctx, cycleEnd); lerr == nil && ok {
			if st.Status == RiskV2CycleCompleted {
				return true // 已完成周期不重新全量扫描
			}
			cursor = st.Cursor
			resumed = cursor != ""
		}
		// 接管周期（仅 lease 持有者可达）：强制 owner=本 token + RUNNING，保留 cursor 供恢复。
		// 接管后旧 leader 的 owner-only 写入会被拒。
		if err := w.deps.CycleStore.ClaimCycleState(ctx, cycleEnd, token); err != nil {
			return false
		}
	} else {
		w.mu.Lock()
		if w.currentCycle != cycleEnd {
			w.currentCycle = cycleEnd
			w.cursor = ""
		}
		cursor = w.cursor
		resumed = cursor != ""
		w.mu.Unlock()
	}

	run := &cycleRun{
		processed:       map[int64]bool{},
		attempts:        map[int64]int{},
		failedTransient: map[int64]int{},
	}

	saveState := func(status, cursor, reason string) bool {
		w.saveCursor(cursor)
		if w.deps.CycleStore == nil {
			return true
		}
		ok, _ := w.deps.CycleStore.SaveCycleState(ctx, cycleEnd, RiskV2CycleState{
			Status: status, Cursor: cursor,
			UsersSeen: run.usersThisCycle, UsersScored: run.usersScored, UsersFailed: run.readErrors + run.dbErrors,
			IncompleteReason: reason,
		}, token)
		return ok
	}
	// 中止：保存 INCOMPLETE + 把失败用户推入下一周期 retry。绝不标记 COMPLETED。
	abort := func(cursor, reason string) bool {
		saveState(RiskV2CycleIncomplete, cursor, reason)
		w.flushRetry(ctx, cycleEnd, run)
		return false
	}

	// —— retry 阶段：仅在全新开始（未 resume）时处理，避免与 active 重复；resume 时 retry 已在上次完成 ——
	if !resumed && w.deps.CycleStore != nil {
		retryUsers, rerr := w.deps.CycleStore.LoadRetryUsers(ctx, cycleEnd, w.params.MaxUsersPerCycle)
		if rerr == nil {
			ids := make([]int64, 0, len(retryUsers))
			for _, ru := range retryUsers {
				run.attempts[ru.UserID] = ru.Attempts
				ids = append(ids, ru.UserID)
			}
			for start := 0; start < len(ids); start += w.params.batchSizeOr() {
				end := start + w.params.batchSizeOr()
				if end > len(ids) {
					end = len(ids)
				}
				if !w.processBatch(ctx, cycleEnd, ids[start:end], health, leaseLost, run) {
					return abort(cursor, "retry_phase_interrupted")
				}
			}
		}
	}

	// —— active 用户分页 ——
	for {
		if ctx.Err() != nil {
			return abort(cursor, "context_done")
		}
		if atomic.LoadInt32(leaseLost) == 1 {
			return abort(cursor, "lease_lost")
		}
		if w.deps.EnabledFn != nil && !w.deps.EnabledFn() {
			return abort(cursor, "dynamically_disabled")
		}

		page, err := w.deps.Reader.ListActiveUserIDsForCycle(ctx, cycleEnd, cursor, w.params.batchSizeOr())
		if err != nil {
			w.metrics.inc(&w.metrics.redisReadErrors)
			return abort(cursor, "active_list_error")
		}
		if len(page.UserIDs) == 0 {
			break // 遍历结束
		}
		w.metrics.add(&w.metrics.activeUsersSeen, int64(len(page.UserIDs)))

		if w.params.MaxUsersPerCycle > 0 && run.usersThisCycle+int64(len(page.UserIDs)) > int64(w.params.MaxUsersPerCycle) {
			return abort(cursor, "max_users_per_cycle")
		}

		if !w.processBatch(ctx, cycleEnd, page.UserIDs, health, leaseLost, run) {
			return abort(cursor, "batch_interrupted")
		}

		// 熔断：读/写错误比率超阈值（需最小样本）。
		if run.usersRead >= 20 && float64(run.readErrors)/float64(run.usersRead) > w.params.MaxReadErrorRatio {
			return abort(page.NextCursor, "read_error_ratio")
		}
		if w.deps.Persist && run.usersScored >= 20 && float64(run.dbErrors)/float64(run.usersScored) > w.params.MaxDBErrorRatio {
			return abort(page.NextCursor, "db_error_ratio")
		}

		cursor = page.NextCursor
		if !saveState(RiskV2CycleRunning, cursor, "") {
			return false // owner 易主
		}
		if cursor == "" {
			break
		}
	}

	// lease 复核：标记 COMPLETED 前确认仍持有；丢失 → INCOMPLETE。
	if atomic.LoadInt32(leaseLost) == 1 {
		return abort(cursor, "lease_lost_before_complete")
	}
	// 失败用户进入下一周期 retry。
	w.flushRetry(ctx, cycleEnd, run)
	if !saveState(RiskV2CycleCompleted, "", "") {
		return false // owner 易主：不得标记完成
	}

	// 记录时长 / lag / 吞吐。
	dur := w.now().Sub(startT)
	w.metrics.setDur(&w.metrics.lastCycleDurationNanos, dur)
	lag := w.now().Unix() - cycleEnd
	if lag < 0 {
		lag = 0
	}
	w.metrics.setDur(&w.metrics.lastScoringLagNanos, time.Duration(lag)*time.Second)
	w.metrics.setDur(&w.metrics.lastPacingSleepNanos, run.pacingSleep)
	w.metrics.add(&w.metrics.usersRead, run.usersRead)
	w.metrics.add(&w.metrics.usersScored, run.usersScored)
	if dur > 0 {
		ups := float64(run.usersScored) / dur.Seconds()
		w.metrics.setDur(&w.metrics.lastUsersPerSecMilli, time.Duration(ups*1000)*time.Millisecond)
	}
	return true
}

// processBatch 处理一批用户（节流→读→评分→persist）。返回 false 表示应中止（ctx/lease/pacing/整批读失败）。
func (w *RiskV2ScoringWorker) processBatch(ctx context.Context, cycleEnd int64, userIDs []int64, health RiskV2IngestionHealth, leaseLost *int32, run *cycleRun) bool {
	// 去重：跳过本 run 已处理用户（retry∩active）。
	todo := make([]int64, 0, len(userIDs))
	for _, u := range userIDs {
		if run.processed[u] {
			continue
		}
		run.processed[u] = true
		todo = append(todo, u)
	}
	if len(todo) == 0 {
		return true
	}
	// §六 lease 复核：读取每个 Batch 前。
	if atomic.LoadInt32(leaseLost) == 1 || ctx.Err() != nil {
		return false
	}
	sleepBefore := w.now()
	if err := w.pacer.WaitN(ctx, len(todo)); err != nil {
		return false
	}
	run.pacingSleep += w.now().Sub(sleepBefore)

	readStart := w.now()
	items, berr := w.deps.Reader.ReadForScoringBatch(ctx, todo)
	w.metrics.addDur(&w.metrics.batchReadNanos, w.now().Sub(readStart))
	if berr != nil {
		w.metrics.inc(&w.metrics.redisReadErrors)
		return false
	}

	// §六 lease 复核：数据库写入每个 Batch 前。
	if w.deps.Persist && atomic.LoadInt32(leaseLost) == 1 {
		return false
	}

	for _, it := range items {
		run.usersThisCycle++
		run.usersRead++
		if it.Err != nil {
			run.readErrors++
			w.metrics.inc(&w.metrics.redisReadErrors)
			run.failedTransient[it.UserID] = run.attempts[it.UserID] + 1 // 读错误 → 可重试
			continue
		}
		metrics := it.Snapshot.ToWindowMetrics(cycleEnd)
		assessment := ScoreRiskV2(metrics, health, w.deps.ScoringConfig)
		run.usersScored++
		w.tallyTier(assessment.RiskTier)

		if !w.deps.Persist {
			w.metrics.inc(&w.metrics.usersDryRun)
			continue
		}
		res, uerr := w.deps.Repo.UpsertCurrentAssessment(ctx, it.UserID, assessment)
		if uerr != nil {
			if uerr == ErrRiskV2AssessmentConflict {
				w.metrics.inc(&w.metrics.assessmentConflicts) // 同周期冲突：终态，不重试
			} else {
				run.dbErrors++
				w.metrics.inc(&w.metrics.databaseErrors)
				run.failedTransient[it.UserID] = run.attempts[it.UserID] + 1 // DB 错误 → 可重试
			}
			continue
		}
		w.tallyUpsert(res)
	}
	return true
}

// flushRetry 把本 run 的可重试失败用户推入「下一周期」retry 集。
func (w *RiskV2ScoringWorker) flushRetry(ctx context.Context, cycleEnd int64, run *cycleRun) {
	if w.deps.CycleStore == nil || len(run.failedTransient) == 0 {
		return
	}
	next := cycleEnd + int64(w.params.Interval.Seconds())
	users := make([]RiskV2RetryUser, 0, len(run.failedTransient))
	for uid, attempts := range run.failedTransient {
		users = append(users, RiskV2RetryUser{UserID: uid, Attempts: attempts})
	}
	if overflow, _ := w.deps.CycleStore.AddRetryUsers(ctx, next, users); overflow {
		w.metrics.inc(&w.metrics.cyclesIncomplete) // retry 溢出：标记（下一周期不完整信号）
	}
}

func (w *RiskV2ScoringWorker) saveCursor(cursor string) {
	w.mu.Lock()
	w.cursor = cursor
	w.mu.Unlock()
}

func (w *RiskV2ScoringWorker) tallyTier(tier string) {
	switch tier {
	case RiskV2TierHigh:
		w.metrics.inc(&w.metrics.highCount)
	case RiskV2TierMedium:
		w.metrics.inc(&w.metrics.mediumCount)
	case RiskV2TierWatch:
		w.metrics.inc(&w.metrics.watchCount)
	default:
		w.metrics.inc(&w.metrics.insufficientCount)
	}
}

func (w *RiskV2ScoringWorker) tallyUpsert(res RiskV2UpsertResult) {
	switch res {
	case RiskV2Inserted:
		w.metrics.inc(&w.metrics.inserted)
	case RiskV2Updated:
		w.metrics.inc(&w.metrics.updated)
	case RiskV2Noop:
		w.metrics.inc(&w.metrics.noop)
	case RiskV2StaleIgnored:
		w.metrics.inc(&w.metrics.staleIgnored)
	}
}

// —— Assessment 新鲜度（§十）：本阶段仅计算/预留，不建 Admin API ——

// RiskV2AssessmentFresh 判断某 assessed_at 相对 now 是否仍 fresh（未过 staleAfter）。
func RiskV2AssessmentFresh(assessedAtUnix int64, now time.Time, staleAfter time.Duration) bool {
	if staleAfter <= 0 {
		return true
	}
	return now.Unix()-assessedAtUnix <= int64(staleAfter.Seconds())
}
