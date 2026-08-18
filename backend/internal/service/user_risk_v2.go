package service

import (
	"context"
	"errors"
)

// RiskV2UpsertResult 是 upsert 四态结果。
type RiskV2UpsertResult int

const (
	RiskV2Inserted     RiskV2UpsertResult = iota // 无记录 → 插入
	RiskV2Updated                                // incoming.assessed_at > stored → 更新
	RiskV2Noop                                   // 同 assessed_at + 同内容 → 不写、不动 updated_at
	RiskV2StaleIgnored                           // incoming.assessed_at < stored → 忽略
)

func (r RiskV2UpsertResult) String() string {
	switch r {
	case RiskV2Inserted:
		return "INSERTED"
	case RiskV2Updated:
		return "UPDATED"
	case RiskV2Noop:
		return "NOOP"
	case RiskV2StaleIgnored:
		return "STALE_IGNORED"
	default:
		return "UNKNOWN"
	}
}

// ErrRiskV2AssessmentConflict：同一 assessed_at（同评分周期）但持久化内容不同 → 冲突，绝不覆盖。
var ErrRiskV2AssessmentConflict = errors.New("risk_v2: assessment timestamp conflict (same assessed_at, different content)")

// Model Extraction Risk V2 Shadow — 切片 3：user_risk_v2 仓储接口（只存每用户最新一条 Assessment，仅观测）。
// legacy user_risk / f1–f7 / /admin/risk 旧接口保持不变；本表为 additive，空表时系统正常。

// RiskV2ListFilter 是列表查询过滤（为后续 Admin API 预留；本切片只做 Repository，不注册路由）。
type RiskV2ListFilter struct {
	Tier             string   // 空=不过滤
	MinRiskIndex     *float64 // nil=不过滤
	DataSufficient   *bool
	Degraded         *bool
	UserID           int64 // 0=不过滤
	AssessedFromUnix int64 // 0=不过滤
	AssessedToUnix   int64 // 0=不过滤
}

// RiskV2Pagination 分页（有界；Limit 有硬上限，禁止无界列表）。
type RiskV2Pagination struct {
	Limit  int
	Offset int
}

// RiskV2AssessmentListItem 列表摘要项（不含 JSON 明细/敏感字段）。
type RiskV2AssessmentListItem struct {
	UserID                int64
	RiskIndex             float64
	RiskTier              string
	Confidence            float64
	DataSufficient        bool
	Degraded              bool
	Incomplete            bool
	AutomationScore       *float64
	HarvestScore          *float64
	CampaignScore         *float64
	ExposureScore         *float64
	AssessedAtUnix        int64
	UpdatedAtUnix         int64
	FingerprintKeyVersion string

	// 切片 5（Admin 列表用）：额外白名单字段。
	HealthAvailable bool
	FeatureVersion  string
	PolicyVersion   string
	EffectiveAction string             // 恒 NONE
	TopReasonCodes  []RiskV2ReasonCode // 按 ConfidenceContribution 降序取前 3
}

// UserRiskV2Repository 是 user_risk_v2 的数据访问面（raw database/sql）。
type UserRiskV2Repository interface {
	// UpsertCurrentAssessment 四态 upsert：INSERTED/UPDATED/NOOP/STALE_IGNORED；
	// 同 assessed_at 不同内容 → ErrRiskV2AssessmentConflict（绝不覆盖）。
	UpsertCurrentAssessment(ctx context.Context, userID int64, a RiskV2Assessment) (RiskV2UpsertResult, error)
	// GetCurrentAssessment 读取某用户最新 Assessment；不存在返回 (nil,false,nil)。
	GetCurrentAssessment(ctx context.Context, userID int64) (*RiskV2Assessment, bool, error)
	// ListCurrentAssessments 有界列表（稳定排序 risk_index DESC, assessed_at DESC, user_id ASC）。
	ListCurrentAssessments(ctx context.Context, filter RiskV2ListFilter, page RiskV2Pagination) ([]RiskV2AssessmentListItem, error)
	// DeleteByUserID 删除某用户的 V2 评分行（幂等）。
	DeleteByUserID(ctx context.Context, userID int64) error
	// CheckSchemaReady 轻量只读探针：确认 user_risk_v2 表与 assessment_digest 列存在且可查询。
	// 绝不写测试行、绝不连生产测试库；Schema 不满足返回无秘密的结构化错误。persist 模式启动前调用。
	CheckSchemaReady(ctx context.Context) error
}
