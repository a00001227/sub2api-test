package service

import (
	"sync/atomic"
	"time"
)

// 切片 4：Worker 指标（§十一）。全部为聚合计数/时长，**绝不含高基数 user_id label**。

type RiskV2WorkerMetrics struct {
	workerStarted int64
	workerReady   int64

	leaseAcquired  int64
	leaseContended int64
	leaseLost      int64

	cyclesStarted    int64
	cyclesCompleted  int64
	cyclesIncomplete int64
	cyclesSkipped    int64
	catchupCycles    int64

	activeUsersSeen int64
	usersRead       int64
	usersScored     int64
	usersDryRun     int64

	inserted            int64
	updated             int64
	noop                int64
	staleIgnored        int64
	assessmentConflicts int64

	redisReadErrors   int64
	databaseErrors    int64
	configInvalid     int64
	healthUnavailable int64

	highCount         int64
	mediumCount       int64
	watchCount        int64
	insufficientCount int64

	// 时长/速率（存最近一次；纳秒或 milli）。
	lastCycleDurationNanos int64
	lastScoringLagNanos    int64
	lastUsersPerSecMilli   int64
	lastBatchReadNanos     int64 // 累计
	batchReadNanos         int64
	lastPacingSleepNanos   int64
}

func (m *RiskV2WorkerMetrics) inc(p *int64)                     { atomic.AddInt64(p, 1) }
func (m *RiskV2WorkerMetrics) add(p *int64, n int64)            { atomic.AddInt64(p, n) }
func (m *RiskV2WorkerMetrics) addDur(p *int64, d time.Duration) { atomic.AddInt64(p, int64(d)) }
func (m *RiskV2WorkerMetrics) setDur(p *int64, d time.Duration) { atomic.StoreInt64(p, int64(d)) }

// RiskV2WorkerMetricsSnapshot 是对外只读快照（无 user_id 维度）。
type RiskV2WorkerMetricsSnapshot struct {
	WorkerStarted int64
	WorkerReady   int64

	LeaseAcquired  int64
	LeaseContended int64
	LeaseLost      int64

	CyclesStarted    int64
	CyclesCompleted  int64
	CyclesIncomplete int64
	CyclesSkipped    int64
	CatchupCycles    int64

	ActiveUsersSeen int64
	UsersRead       int64
	UsersScored     int64
	UsersDryRun     int64

	Inserted            int64
	Updated             int64
	Noop                int64
	StaleIgnored        int64
	AssessmentConflicts int64

	RedisReadErrors   int64
	DatabaseErrors    int64
	ConfigInvalid     int64
	HealthUnavailable int64

	HighCount         int64
	MediumCount       int64
	WatchCount        int64
	InsufficientCount int64

	CycleDuration  time.Duration
	ScoringLag     time.Duration
	UsersPerSecond float64
	BatchReadTotal time.Duration
	PacingSleep    time.Duration
}

func (m *RiskV2WorkerMetrics) snapshot() RiskV2WorkerMetricsSnapshot {
	ld := func(p *int64) int64 { return atomic.LoadInt64(p) }
	return RiskV2WorkerMetricsSnapshot{
		WorkerStarted:       ld(&m.workerStarted),
		WorkerReady:         ld(&m.workerReady),
		LeaseAcquired:       ld(&m.leaseAcquired),
		LeaseContended:      ld(&m.leaseContended),
		LeaseLost:           ld(&m.leaseLost),
		CyclesStarted:       ld(&m.cyclesStarted),
		CyclesCompleted:     ld(&m.cyclesCompleted),
		CyclesIncomplete:    ld(&m.cyclesIncomplete),
		CyclesSkipped:       ld(&m.cyclesSkipped),
		CatchupCycles:       ld(&m.catchupCycles),
		ActiveUsersSeen:     ld(&m.activeUsersSeen),
		UsersRead:           ld(&m.usersRead),
		UsersScored:         ld(&m.usersScored),
		UsersDryRun:         ld(&m.usersDryRun),
		Inserted:            ld(&m.inserted),
		Updated:             ld(&m.updated),
		Noop:                ld(&m.noop),
		StaleIgnored:        ld(&m.staleIgnored),
		AssessmentConflicts: ld(&m.assessmentConflicts),
		RedisReadErrors:     ld(&m.redisReadErrors),
		DatabaseErrors:      ld(&m.databaseErrors),
		ConfigInvalid:       ld(&m.configInvalid),
		HealthUnavailable:   ld(&m.healthUnavailable),
		HighCount:           ld(&m.highCount),
		MediumCount:         ld(&m.mediumCount),
		WatchCount:          ld(&m.watchCount),
		InsufficientCount:   ld(&m.insufficientCount),
		CycleDuration:       time.Duration(ld(&m.lastCycleDurationNanos)),
		ScoringLag:          time.Duration(ld(&m.lastScoringLagNanos)),
		UsersPerSecond:      float64(ld(&m.lastUsersPerSecMilli)) / 1000.0,
		BatchReadTotal:      time.Duration(ld(&m.batchReadNanos)),
		PacingSleep:         time.Duration(ld(&m.lastPacingSleepNanos)),
	}
}
