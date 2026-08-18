package service

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// 切片 4.1：运行态接线辅助（config→params、实例 ID、指标摘要日志 sink）。

// RiskV2WorkerParamsFromConfig 把归一化后的 config.RiskV2WorkerConfig 转成 Worker 运行参数。
func RiskV2WorkerParamsFromConfig(c config.RiskV2WorkerConfig) RiskV2WorkerParams {
	n := c.Normalized()
	return RiskV2WorkerParams{
		Interval:             time.Duration(n.IntervalSeconds) * time.Second,
		GraceDelay:           time.Duration(n.GraceDelaySeconds) * time.Second,
		CycleTimeout:         time.Duration(n.CycleTimeoutSecs) * time.Second,
		BatchSize:            n.BatchSize,
		MaxUsersPerSecond:    n.MaxUsersPerSec,
		MaxUsersPerCycle:     n.MaxUsersPerCycle,
		MaxReadErrorRatio:    n.MaxReadErrorRatio,
		MaxDBErrorRatio:      n.MaxDBErrorRatio,
		LeaseTTL:             time.Duration(n.LeaseTTLSeconds) * time.Second,
		LeaseRenewInterval:   time.Duration(n.LeaseRenewIntervalMS) * time.Millisecond,
		MaxCatchupCycles:     n.MaxCatchupCycles,
		AssessmentStaleAfter: time.Duration(n.AssessmentStaleAfterSeconds) * time.Second,
	}
}

// NewRiskV2InstanceID 生成本进程健康上报用的实例 ID（随机；绝非 user/api_key/账号）。
func NewRiskV2InstanceID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "inst-unknown"
	}
	return "inst-" + hex.EncodeToString(b)
}

// NewRiskV2WorkerLogSink 返回一个有频率限制（最多每 60s 一条）的结构化周期摘要日志 sink。
// 只输出低基数聚合计数，绝不含 user_id / api_key / 任何敏感信息。
func NewRiskV2WorkerLogSink() func(RiskV2WorkerMetricsSnapshot) {
	var mu sync.Mutex
	var last time.Time
	const minInterval = 60 * time.Second
	return func(m RiskV2WorkerMetricsSnapshot) {
		mu.Lock()
		now := time.Now()
		if !last.IsZero() && now.Sub(last) < minInterval {
			mu.Unlock()
			return
		}
		last = now
		mu.Unlock()
		logger.LegacyPrintf("service.risk_v2",
			"[risk_v2.worker] ready=%d lease(acq=%d contend=%d lost=%d) cycles(done=%d incomplete=%d skipped=%d catchup=%d) "+
				"users(seen=%d scored=%d dry=%d) upsert(ins=%d upd=%d noop=%d stale=%d conflict=%d) "+
				"err(read=%d db=%d) health_unavail=%d tier(H=%d M=%d W=%d I=%d) "+
				"cycle_dur=%s lag=%s ups=%.1f pacing=%s",
			m.WorkerReady, m.LeaseAcquired, m.LeaseContended, m.LeaseLost,
			m.CyclesCompleted, m.CyclesIncomplete, m.CyclesSkipped, m.CatchupCycles,
			m.ActiveUsersSeen, m.UsersScored, m.UsersDryRun,
			m.Inserted, m.Updated, m.Noop, m.StaleIgnored, m.AssessmentConflicts,
			m.RedisReadErrors, m.DatabaseErrors, m.HealthUnavailable,
			m.HighCount, m.MediumCount, m.WatchCount, m.InsufficientCount,
			m.CycleDuration, m.ScoringLag, m.UsersPerSecond, m.PacingSleep)
	}
}
