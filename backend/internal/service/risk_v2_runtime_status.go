package service

// 切片 4.2 §五：Risk V2 运行态只读快照，为后续 Admin API 预留。本轮只实现内部快照，不注册 HTTP API。
//
// 严格低基数、纯只读；绝不包含 Prompt / HMAC / SimHash / user_id / API Key / Lease token / Secret / Redis member。

type RiskV2RuntimeStatus struct {
	Enabled            bool
	AggregationEnabled bool
	ScoringEnabled     bool
	PersistEnabled     bool

	DispatcherReady bool
	ReporterReady   bool
	WorkerReady     bool
	WorkerDegraded  bool
	SchemaReady     bool

	Leader             bool
	CurrentCycle       int64
	LastCompletedCycle int64
	LastCycleStatus    string

	QueueDepth           int
	ObservationDropRatio float64

	HealthAvailable     bool
	HealthCoverageRatio float64

	LastErrorCode string // 低基数错误码（如 "schema_not_ready" / "redis_unavailable"），绝非原始错误串/敏感值
	LastUpdatedAt int64  // unix 秒
}

// RiskV2RuntimeStatusInputs 是构建快照的原始输入（由接线层从各组件收集）。
type RiskV2RuntimeStatusInputs struct {
	Enabled            bool
	AggregationEnabled bool
	ScoringEnabled     bool
	PersistEnabled     bool

	DispatcherReady bool
	ReporterReady   bool
	SchemaReady     bool

	Worker *RiskV2ScoringWorker // 可为 nil（未启动）

	DispatcherStats *RiskV2Stats           // 可为 nil
	Health          *RiskV2IngestionHealth // 可为 nil
	LastErrorCode   string
	NowUnix         int64
}

// BuildRiskV2RuntimeStatus 组装只读运行态快照（纯函数；nil 输入安全）。
func BuildRiskV2RuntimeStatus(in RiskV2RuntimeStatusInputs) RiskV2RuntimeStatus {
	s := RiskV2RuntimeStatus{
		Enabled:            in.Enabled,
		AggregationEnabled: in.AggregationEnabled,
		ScoringEnabled:     in.ScoringEnabled,
		PersistEnabled:     in.PersistEnabled,
		DispatcherReady:    in.DispatcherReady,
		ReporterReady:      in.ReporterReady,
		SchemaReady:        in.SchemaReady,
		LastErrorCode:      in.LastErrorCode,
		LastUpdatedAt:      in.NowUnix,
	}
	if in.Worker != nil {
		v := in.Worker.StatusSnapshot()
		s.WorkerReady = v.Ready
		s.WorkerDegraded = v.Degraded
		s.Leader = v.Leader
		s.CurrentCycle = v.CurrentCycle
		s.LastCompletedCycle = v.LastCompletedCycle
		s.LastCycleStatus = v.LastCycleStatus
	}
	if in.DispatcherStats != nil {
		s.QueueDepth = in.DispatcherStats.QueueDepth
		if in.DispatcherStats.Enqueued > 0 {
			s.ObservationDropRatio = float64(in.DispatcherStats.Dropped) / float64(in.DispatcherStats.Enqueued)
		}
	}
	if in.Health != nil {
		s.HealthAvailable = in.Health.HealthAvailable
		s.HealthCoverageRatio = in.Health.HealthCoverageRatio
	}
	return s
}
