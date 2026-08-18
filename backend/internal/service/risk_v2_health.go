package service

import (
	"context"
	"sync/atomic"
	"time"
)

// 切片 4：全局 Ingestion Health —— 不能只用 Leader 本地 Dispatcher Stats。
//
// 每个 Gateway 实例定期把「累计计数的 delta」写入 Redis 分钟桶 + 心跳；Worker 对评分周期聚合所有活跃实例，
// 产出 RiskV2IngestionHealth（HealthAvailable=false 时阻止 HIGH，绝不假设健康）。
//
// 隐私：只写 instanceID（服务启动实例 ID，绝非 user/api_key/上游账号）+ 计数；Health Key 有 TTL；
// 绝不记录 Prompt/HMAC/请求 ID/秘密/Set 成员。

// RiskV2HealthDelta 是一次上报的增量计数（相对上次上报）。
type RiskV2HealthDelta struct {
	Enqueued  int64
	Processed int64
	Dropped   int64
	Panics    int64
	AggErrors int64
}

func (d RiskV2HealthDelta) isZero() bool {
	return d.Enqueued == 0 && d.Processed == 0 && d.Dropped == 0 && d.Panics == 0 && d.AggErrors == 0
}

// RiskV2HealthReportResult 是幂等上报结果（§二：解决 ambiguous ACK 的重复累计）。
type RiskV2HealthReportResult int

const (
	RiskV2HealthApplied        RiskV2HealthReportResult = iota // 本序号首次应用，已累加
	RiskV2HealthAlreadyApplied                                 // 该序号已应用过（重试/ambiguous ACK），未重复累加
)

// RiskV2HealthReporter 把某实例的健康增量按「单调序号」幂等写入 Redis（有界、失败不影响主请求）。
//
// sequence 每 instance 独立递增。Redis 端原子比对 last_applied_sequence：
//   - incoming <= last → ALREADY_APPLIED（绝不重复 HINCRBY）；
//   - incoming 为新序号 → 原子累加 delta + 保存序号 + TTL → APPLIED。
//
// 由此即使「写已被 Redis 应用但客户端收到 timeout / 丢 ACK」，用同序号重试也只累加一次。
type RiskV2HealthReporter interface {
	Report(ctx context.Context, sequence uint64, d RiskV2HealthDelta) (RiskV2HealthReportResult, error)
	// Deregister 优雅缩容时把本实例从 Registry 移除（draining），避免正常下线长期被判为缺失。
	Deregister(ctx context.Context) error
}

// RiskV2IngestionHealthReader 聚合某评分周期窗口内所有活跃实例的健康，产出评分用的 RiskV2IngestionHealth。
// windowMinutes 是聚合窗口（分钟），一般取评分 interval 覆盖的分钟数。
type RiskV2IngestionHealthReader interface {
	ReadIngestionHealth(ctx context.Context, cycleEnd int64, windowMinutes int) (RiskV2IngestionHealth, error)
}

// RiskV2StatsSource 提供 dispatcher 累计计数（用于计算上报 delta）。RiskV2Dispatcher.Stats 即实现之。
type RiskV2StatsSource interface {
	Stats() RiskV2Stats
}

// RiskV2HealthReportLoop 周期性地把 dispatcher 累计计数的 delta 上报到 Redis。
// 独立于主请求；Redis 失败仅忽略本次（下次 delta 会带上累计差）。Clock 注入以便测试。
type RiskV2HealthReportLoop struct {
	src      RiskV2StatsSource
	reporter RiskV2HealthReporter
	interval time.Duration
	now      func() time.Time

	last     RiskV2Stats // 已成功应用的累计基线
	sequence uint64      // 最近已应用的序号
	pending  *pendingHealthReport
	started  int32
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// pendingHealthReport 是一次尚未确认应用的上报（用于 ambiguous ACK 下用同序号安全重试）。
type pendingHealthReport struct {
	seq   uint64
	delta RiskV2HealthDelta
	cur   RiskV2Stats // 应用成功后推进到的累计基线
}

func NewRiskV2HealthReportLoop(src RiskV2StatsSource, reporter RiskV2HealthReporter, interval time.Duration, now func() time.Time) *RiskV2HealthReportLoop {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if now == nil {
		now = time.Now
	}
	return &RiskV2HealthReportLoop{
		src: src, reporter: reporter, interval: interval, now: now,
		stopCh: make(chan struct{}), doneCh: make(chan struct{}),
	}
}

// Start 启动上报循环（幂等）。src 或 reporter 为 nil → no-op（不启动 goroutine）。
func (l *RiskV2HealthReportLoop) Start() {
	if l == nil || l.src == nil || l.reporter == nil {
		return
	}
	if !atomic.CompareAndSwapInt32(&l.started, 0, 1) {
		return
	}
	go l.run()
}

func (l *RiskV2HealthReportLoop) run() {
	defer close(l.doneCh)
	t := time.NewTicker(l.interval)
	defer t.Stop()
	for {
		select {
		case <-l.stopCh:
			l.flush() // 退出前最后一次 best-effort flush
			// 优雅缩容：从 Registry 注销，避免正常下线被判为缺失（draining）。
			dctx, dcancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = l.reporter.Deregister(dctx)
			dcancel()
			return
		case <-t.C:
			l.flush()
		}
	}
}

// FlushForTest 暴露单次 flush 供测试确定性驱动 delta baseline 语义。
func (l *RiskV2HealthReportLoop) FlushForTest() { l.flush() }

func (l *RiskV2HealthReportLoop) flush() {
	// 无未决上报 → 计算新 delta；有未决 → 用同序号重试（ambiguous ACK 安全）。
	if l.pending == nil {
		cur := l.src.Stats()
		d := RiskV2HealthDelta{
			Enqueued:  cur.Enqueued - l.last.Enqueued,
			Processed: cur.Processed - l.last.Processed,
			Dropped:   cur.Dropped - l.last.Dropped,
			Panics:    cur.Panics - l.last.Panics,
			AggErrors: 0, // 聚合错误由 sink 侧统计，dispatcher 无此计数；预留
		}
		if d.isZero() {
			return
		}
		l.pending = &pendingHealthReport{seq: l.sequence + 1, delta: d, cur: cur}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := l.reporter.Report(ctx, l.pending.seq, l.pending.delta)
	if err != nil {
		return // 保留 pending，下次用同序号重试；不推进 baseline（不丢、不重）
	}
	// APPLIED 或 ALREADY_APPLIED（含 ambiguous ACK 后的重试）均视为已生效：推进基线与序号。
	_ = res
	l.last = l.pending.cur
	l.sequence = l.pending.seq
	l.pending = nil
}

// Stop 停止上报循环（幂等）；有限等待最后一次 flush。
func (l *RiskV2HealthReportLoop) Stop() {
	if l == nil || atomic.LoadInt32(&l.started) == 0 {
		return
	}
	select {
	case <-l.stopCh:
	default:
		close(l.stopCh)
	}
	select {
	case <-l.doneCh:
	case <-time.After(3 * time.Second):
	}
}
