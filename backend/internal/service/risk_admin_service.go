package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// Risk Phase 0（仅观测/影子模式）：admin 服务层。为 admin API 提供
// 列表/详情/allowlist/manual-tier/config 读写。绝不据此执行任何拦截动作。

// ErrInvalidRiskTier manual-tier 非法。
var ErrInvalidRiskTier = errors.New("invalid risk tier")

// RiskUserWindow 是仪表盘展示的窗口聚合。
type RiskUserWindow struct {
	Requests24h     int     `json:"requests_24h"`
	OutputTokens24h int64   `json:"output_tokens_24h"`
	DistinctRatio   float64 `json:"distinct_ratio"`
	SingleTurnRatio float64 `json:"single_turn_ratio"`
	TopModel        string  `json:"top_model"`
	RPMPeak         int     `json:"rpm_peak"`
	BudgetPct       float64 `json:"budget_pct"`
}

// RiskUserView 是 admin 列表/详情的一条用户视图。
type RiskUserView struct {
	UserID        int64          `json:"user_id"`
	Email         string         `json:"email"`
	Score         int            `json:"score"`
	Tier          string         `json:"tier"`
	Allowlisted   bool           `json:"allowlisted"`
	ManualTier    *string        `json:"manual_tier"`
	Features      RiskFeatures   `json:"features"`
	Window        RiskUserWindow `json:"window"`
	WouldDoAction string         `json:"would_do_action"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

// RiskAdminService 封装 admin 只读/维护操作。
type RiskAdminService struct {
	repo    UserRiskRepository
	userRPM UserRPMCache
	cfg     *config.Config
}

// NewRiskAdminService 构造 admin 服务。
func NewRiskAdminService(repo UserRiskRepository, userRPM UserRPMCache, cfg *config.Config) *RiskAdminService {
	return &RiskAdminService{repo: repo, userRPM: userRPM, cfg: cfg}
}

func (s *RiskAdminService) riskConfig() config.RiskConfig {
	if s.cfg != nil {
		return s.cfg.Risk
	}
	return config.RiskConfig{}
}

// ListUsers 返回风险用户列表（按 score 降序）。tier 可选过滤。
func (s *RiskAdminService) ListUsers(ctx context.Context, tier string, limit int) ([]RiskUserView, error) {
	items, err := s.repo.List(ctx, tier, limit)
	if err != nil {
		return nil, err
	}
	views := make([]RiskUserView, 0, len(items))
	for i := range items {
		views = append(views, s.toView(ctx, items[i].UserRiskRecord, items[i].Email, false))
	}
	return views, nil
}

// GetUser 返回单用户详情（含 usage_logs 24h 聚合窗口）。
func (s *RiskAdminService) GetUser(ctx context.Context, userID int64) (*RiskUserView, error) {
	rec, err := s.repo.GetByUserID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		// 无评分记录：返回一个空视图（tier=watch），仍带聚合窗口便于观测。
		rec = &UserRiskRecord{UserID: userID, Tier: RiskTierWatch}
	}
	view := s.toView(ctx, *rec, "", true)
	return &view, nil
}

// toView 组装视图；withAggregate 为 true 时补充 usage_logs 聚合窗口（详情用，避免列表 N 次查询）。
func (s *RiskAdminService) toView(ctx context.Context, rec UserRiskRecord, email string, withAggregate bool) RiskUserView {
	var features RiskFeatures
	if len(rec.FeaturesRaw) > 0 {
		_ = json.Unmarshal(rec.FeaturesRaw, &features)
	}
	view := RiskUserView{
		UserID:        rec.UserID,
		Email:         email,
		Score:         rec.Score,
		Tier:          rec.Tier,
		Allowlisted:   rec.Allowlisted,
		ManualTier:    rec.ManualTier,
		Features:      features,
		WouldDoAction: WouldDoAction(rec.Tier),
		UpdatedAt:     rec.UpdatedAt,
		Window: RiskUserWindow{
			DistinctRatio:   features.DistinctRatio,
			SingleTurnRatio: features.F2SingleTurnRatio,
			BudgetPct:       features.BudgetDailyPct,
		},
	}
	if s.userRPM != nil {
		if v, err := s.userRPM.GetUserRPM(ctx, rec.UserID); err == nil {
			view.Window.RPMPeak = v
		}
	}
	if withAggregate {
		if agg, err := s.repo.AggregateUsage(ctx, rec.UserID, 24*time.Hour); err == nil {
			view.Window.Requests24h = agg.Requests24h
			view.Window.OutputTokens24h = agg.OutputTokens24h
			view.Window.TopModel = agg.TopModel
		}
	}
	return view
}

// SetAllowlist 设置 allowlisted。
func (s *RiskAdminService) SetAllowlist(ctx context.Context, userID int64, on bool) error {
	return s.repo.SetAllowlist(ctx, userID, on)
}

// SetManualTier 设置/清除 manual_tier（nil=清除）。
func (s *RiskAdminService) SetManualTier(ctx context.Context, userID int64, tier *string) error {
	if tier != nil {
		switch *tier {
		case RiskTierWatch, RiskTierMedium, RiskTierHigh:
		default:
			return ErrInvalidRiskTier
		}
	}
	return s.repo.SetManualTier(ctx, userID, tier)
}

// GetConfig 返回当前 risk 配置（观测）。
func (s *RiskAdminService) GetConfig() config.RiskConfig {
	return s.riskConfig()
}

// PatchConfigInput 是 config PATCH 的可选字段（nil=不改）。
// Phase 0：仅内存态更新（进程重启回到 config.yaml）。持久化留待后续 Phase。
type PatchConfigInput struct {
	Mode         *string            `json:"mode"`
	MediumScore  *int               `json:"medium_score"`
	HighScore    *int               `json:"high_score"`
	VolumeFloor  *int               `json:"volume_floor"`
	AndGateK     *int               `json:"and_gate_k"`
	DailyBudget  *int64             `json:"daily_budget_micros"`
	WeeklyBudget *int64             `json:"weekly_budget_micros"`
	Weights      map[string]float64 `json:"weights"`
	Thresholds   map[string]float64 `json:"thresholds"`
}

// PatchConfig 就地更新内存 risk 配置（Phase 0 观测：mode 变更不触发任何执行）。
func (s *RiskAdminService) PatchConfig(in PatchConfigInput) config.RiskConfig {
	if s.cfg == nil {
		return config.RiskConfig{}
	}
	r := &s.cfg.Risk
	if in.Mode != nil {
		r.Mode = *in.Mode
	}
	if in.MediumScore != nil {
		r.MediumScore = *in.MediumScore
	}
	if in.HighScore != nil {
		r.HighScore = *in.HighScore
	}
	if in.VolumeFloor != nil {
		r.VolumeFloor = *in.VolumeFloor
	}
	if in.AndGateK != nil {
		r.AndGateK = *in.AndGateK
	}
	if in.DailyBudget != nil {
		r.DailyBudgetMicros = *in.DailyBudget
	}
	if in.WeeklyBudget != nil {
		r.WeeklyBudgetMicros = *in.WeeklyBudget
	}
	if in.Weights != nil {
		r.Weights = in.Weights
	}
	if in.Thresholds != nil {
		r.Thresholds = in.Thresholds
	}
	return *r
}
