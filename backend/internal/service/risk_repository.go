package service

import (
	"context"
	"time"
)

// Risk Phase 0（仅观测/影子模式）：user_risk 表 + usage_logs 24h 聚合的仓储接口。

// UserRiskRecord 对应 user_risk 表一行。
type UserRiskRecord struct {
	UserID      int64
	Score       int
	Tier        string
	FeaturesRaw []byte // JSONB 原始字节（RiskFeatures 序列化）
	Allowlisted bool
	ManualTier  *string
	UpdatedAt   time.Time
}

// UserRiskListItem 是 admin 列表项（含 email 联表）。
type UserRiskListItem struct {
	UserRiskRecord
	Email string
}

// UserRiskUsageAggregate 是某用户 usage_logs 24h 聚合（评分 worker fallback / 观测用）。
type UserRiskUsageAggregate struct {
	Requests24h      int
	OutputTokens24h  int64
	InputTokens24h   int64
	DistinctSimhash  int
	TotalSimhash     int
	SingleTurn       int
	TotalTurns       int
	TopModelCount    int
	ModelVariety     int
	TopModel         string
	ZeroTempShare    float64
	MaxTokenPinShare float64
	// SpendMicros 近 window 累计消费（USDC micros，由 actual_cost 求和换算）。观测用。
	SpendMicros int64
	// RPMPeak / ActiveMinutes 由评分 worker 结合免费计数器估算，聚合查询中不填。
}

// UserRiskRepository 读写 user_risk + usage_logs 聚合。
type UserRiskRepository interface {
	// Upsert 写入/更新评分（score/tier/features/updated_at）；不覆盖 allowlisted/manual_tier。
	Upsert(ctx context.Context, rec *UserRiskRecord) error
	// GetByUserID 读单条（不存在返回 nil,nil）。
	GetByUserID(ctx context.Context, userID int64) (*UserRiskRecord, error)
	// List 列出（可按 tier 过滤，按 score 降序，limit 限制）。联表 email。
	List(ctx context.Context, tier string, limit int) ([]UserRiskListItem, error)
	// SetAllowlist 设置 allowlisted（行不存在则创建默认行）。
	SetAllowlist(ctx context.Context, userID int64, on bool) error
	// SetManualTier 设置/清除 manual_tier（nil=清除；行不存在则创建默认行）。
	SetManualTier(ctx context.Context, userID int64, tier *string) error
	// AggregateUsage 计算某用户 usage_logs 近 window 的聚合特征。
	AggregateUsage(ctx context.Context, userID int64, window time.Duration) (UserRiskUsageAggregate, error)
}
