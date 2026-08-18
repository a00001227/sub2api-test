package service

import (
	"context"
	"errors"
	"time"
)

// 切片 5：Risk V2 只读 Admin Service。纯只读——绝不修改 tier/index/reason/阈值/allowlist，不封禁/限速/暂停/触发重评分。
// Handler 不得直接拼 SQL / 操作 Redis Key；本 Service 负责查询、批量用户摘要、fresh/stale、错误分类。

// —— 稳定错误（Handler 映射为稳定 error code）——
var (
	ErrRiskV2SchemaNotReady         = errors.New("risk_v2: schema not ready")
	ErrRiskV2LiveMetricsUnavailable = errors.New("risk_v2: live metrics unavailable")
	ErrRiskV2AssessmentNotFound     = errors.New("risk_v2: assessment not found")
)

// RiskV2UserSummary 是列表/详情可复用的安全用户摘要（仅现有 Admin 页面已允许展示的字段）。
type RiskV2UserSummary struct {
	UserID   int64
	Email    string
	Username string
	Status   string
	Known    bool // false = 用户记录缺失（理论上因 FK CASCADE 不应发生）
}

// RiskV2UserSummaryReader 批量读取用户摘要（避免列表 N+1）。绝不返回密码/密钥/Token/凭据。
type RiskV2UserSummaryReader interface {
	GetSummariesByIDs(ctx context.Context, ids []int64) (map[int64]RiskV2UserSummary, error)
}

// RiskV2RuntimeStatusProvider 提供运行态只读快照（由接线层用运行组件实现）。nil-safe：返回 disabled。
type RiskV2RuntimeStatusProvider interface {
	RuntimeStatus(ctx context.Context) RiskV2RuntimeStatus
}

// RiskV2AdminSummaryReader 是 Live Window 专用的**轻量**用户级读取（§5.1 §一）。
// 只读单用户的 Scoring Snapshot（用户级三窗口 + 紧凑 Multi-Key），**绝不展开 PerAPIKey、绝不逐 key 读窗口**，
// 也不暴露任何 Worker 控制能力。命令数不随 API Key 数量近似线性增长。
type RiskV2AdminSummaryReader interface {
	ReadForScoring(ctx context.Context, userID int64) (RiskV2ScoringSnapshot, error)
}

// —— Service 输出视图（Handler 再映射为响应 DTO；不直接序列化 repo/ent/config 对象）——

type RiskV2AdminAssessmentRow struct {
	Item        RiskV2AssessmentListItem
	User        RiskV2UserSummary
	Fresh       bool
	AgeSeconds  int64
	TimeAnomaly bool
}

type RiskV2AdminAssessmentDetail struct {
	Assessment  RiskV2Assessment
	User        RiskV2UserSummary
	Fresh       bool
	AgeSeconds  int64
	TimeAnomaly bool
}

// RiskV2AdminListPage 是列表分页结果（不做 COUNT(*)；has_more 由 limit+1 探测）。
type RiskV2AdminListPage struct {
	Rows       []RiskV2AdminAssessmentRow
	Limit      int
	Offset     int
	HasMore    bool
	NextOffset int
}

// RiskV2AdminLiveWindow 是实时窗口摘要（轻量 Snapshot + 数据可用性）。
type RiskV2AdminLiveWindow struct {
	Snapshot      RiskV2ScoringSnapshot
	DataAvailable bool
}

// RiskV2AdminService 只读 Admin 服务。
type RiskV2AdminService struct {
	repo          UserRiskV2Repository
	users         RiskV2UserSummaryReader
	summaryReader RiskV2AdminSummaryReader // §5.1：轻量 Live Window 读；列表/详情绝不调用
	status        RiskV2RuntimeStatusProvider
	now           func() time.Time
	staleAfter    time.Duration
	liveTimeout   time.Duration
}

// NewRiskV2AdminService 构造只读 Admin 服务。summaryReader/status 可为 nil（对应功能降级）。
func NewRiskV2AdminService(repo UserRiskV2Repository, users RiskV2UserSummaryReader, summaryReader RiskV2AdminSummaryReader,
	status RiskV2RuntimeStatusProvider, staleAfter, liveTimeout time.Duration, now func() time.Time) *RiskV2AdminService {
	if now == nil {
		now = time.Now
	}
	if staleAfter <= 0 {
		staleAfter = time.Hour
	}
	if liveTimeout <= 0 {
		liveTimeout = 3 * time.Second
	}
	return &RiskV2AdminService{repo: repo, users: users, summaryReader: summaryReader, status: status, now: now, staleAfter: staleAfter, liveTimeout: liveTimeout}
}

// StaleAfterSeconds 暴露给响应（页面显示是否过期）。
func (s *RiskV2AdminService) StaleAfterSeconds() int64 { return int64(s.staleAfter.Seconds()) }

// RuntimeStatus 返回运行态只读快照。status provider 为 nil → disabled。
func (s *RiskV2AdminService) RuntimeStatus(ctx context.Context) RiskV2RuntimeStatus {
	if s.status == nil {
		return RiskV2RuntimeStatus{Enabled: false, LastUpdatedAt: s.now().Unix()}
	}
	return s.status.RuntimeStatus(ctx)
}

// ListAssessments 从 user_risk_v2 读取（绝不扫描 Redis）。freshOnly：nil=不过滤，true=仅 fresh，false=仅 stale。
// fresh/stale 翻译为 assessed_at 边界下推到 SQL，保持稳定排序与分页正确。
// 分页：取 limit+1 探测 has_more（不做 COUNT(*)），对外只返回 limit 条。
func (s *RiskV2AdminService) ListAssessments(ctx context.Context, filter RiskV2ListFilter, page RiskV2Pagination, freshOnly *bool) (RiskV2AdminListPage, error) {
	var out RiskV2AdminListPage
	if err := s.repo.CheckSchemaReady(ctx); err != nil {
		return out, ErrRiskV2SchemaNotReady
	}
	now := s.now()
	if freshOnly != nil {
		boundary := now.Add(-s.staleAfter).Unix()
		if *freshOnly {
			// fresh 下界：assessed_at >= now-staleAfter。
			if filter.AssessedFromUnix == 0 || boundary > filter.AssessedFromUnix {
				filter.AssessedFromUnix = boundary
			}
			// §5.1 §一.1：fresh 上界 = now+futureTolerance，排除未来时间异常记录（DTO 中它们 fresh=false/time_anomaly=true），
			// 保证 SQL 命中集与 DTO fresh 语义一致——未来异常记录绝不被 freshness=fresh 命中。
			futureBound := now.Unix() + riskV2ClockToleranceSeconds
			if filter.AssessedToUnix == 0 || futureBound < filter.AssessedToUnix {
				filter.AssessedToUnix = futureBound
			}
		} else {
			// stale：assessed_at < boundary。用 AssessedToUnix = boundary-1（闭区间）。
			if filter.AssessedToUnix == 0 || boundary-1 < filter.AssessedToUnix {
				filter.AssessedToUnix = boundary - 1
			}
		}
	}
	limit := page.Limit
	if limit <= 0 {
		limit = 100
	}
	offset := page.Offset
	if offset < 0 {
		offset = 0
	}
	// limit+1 探测 has_more（稳定排序由 repo 保证）。
	items, err := s.repo.ListCurrentAssessments(ctx, filter, RiskV2Pagination{Limit: limit + 1, Offset: offset})
	if err != nil {
		return out, err
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	// 批量用户摘要（无 N+1）。
	ids := make([]int64, 0, len(items))
	for i := range items {
		ids = append(ids, items[i].UserID)
	}
	summaries := map[int64]RiskV2UserSummary{}
	if s.users != nil && len(ids) > 0 {
		if m, uerr := s.users.GetSummariesByIDs(ctx, ids); uerr == nil {
			summaries = m
		}
	}
	rows := make([]RiskV2AdminAssessmentRow, 0, len(items))
	for i := range items {
		it := items[i]
		u, ok := summaries[it.UserID]
		if !ok {
			u = RiskV2UserSummary{UserID: it.UserID, Known: false}
		}
		fresh, age, anomaly := s.freshness(it.AssessedAtUnix, now)
		rows = append(rows, RiskV2AdminAssessmentRow{Item: it, User: u, Fresh: fresh, AgeSeconds: age, TimeAnomaly: anomaly})
	}
	out = RiskV2AdminListPage{Rows: rows, Limit: limit, Offset: offset, HasMore: hasMore}
	if hasMore {
		out.NextOffset = offset + limit
	}
	return out, nil
}

// GetAssessmentDetail 单用户详情（DB）。不存在 → ErrRiskV2AssessmentNotFound。
func (s *RiskV2AdminService) GetAssessmentDetail(ctx context.Context, userID int64) (RiskV2AdminAssessmentDetail, error) {
	var out RiskV2AdminAssessmentDetail
	if err := s.repo.CheckSchemaReady(ctx); err != nil {
		return out, ErrRiskV2SchemaNotReady
	}
	a, found, err := s.repo.GetCurrentAssessment(ctx, userID)
	if err != nil {
		return out, err
	}
	if !found || a == nil {
		return out, ErrRiskV2AssessmentNotFound
	}
	u := RiskV2UserSummary{UserID: userID, Known: false}
	if s.users != nil {
		if m, uerr := s.users.GetSummariesByIDs(ctx, []int64{userID}); uerr == nil {
			if v, ok := m[userID]; ok {
				u = v
			}
		}
	}
	fresh, age, anomaly := s.freshness(a.AssessedAtUnix, s.now())
	return RiskV2AdminAssessmentDetail{Assessment: *a, User: u, Fresh: fresh, AgeSeconds: age, TimeAnomaly: anomaly}, nil
}

// LiveWindowSummary 实时窗口摘要（§5.1：轻量 Summary Reader，用户级 + Multi-Key，绝不展开 per-key）。
// 独立超时；列表/详情绝不调用本接口。Redis 不可用/超时 → ErrRiskV2LiveMetricsUnavailable（不影响 DB List/Detail）。
// Redis 正常但用户无近期数据 → 返回 DataAvailable=false（不伪装成一组完整测量的零风险窗口）。
func (s *RiskV2AdminService) LiveWindowSummary(ctx context.Context, userID int64) (RiskV2AdminLiveWindow, error) {
	if s.summaryReader == nil {
		return RiskV2AdminLiveWindow{}, ErrRiskV2LiveMetricsUnavailable
	}
	cctx, cancel := context.WithTimeout(ctx, s.liveTimeout)
	defer cancel()
	snap, err := s.summaryReader.ReadForScoring(cctx, userID)
	if err != nil {
		return RiskV2AdminLiveWindow{}, ErrRiskV2LiveMetricsUnavailable
	}
	// data_available：任一窗口有请求即视为有近期数据。
	dataAvail := snap.User.W5m.RequestCount > 0 || snap.User.W1h.RequestCount > 0 || snap.User.W24h.RequestCount > 0
	return RiskV2AdminLiveWindow{Snapshot: snap, DataAvailable: dataAvail}, nil
}

// freshness 返回 (fresh, ageSeconds>=0, timeAnomaly)。assessed_at 明显位于 now 之后 → age clamp 0 + anomaly=true
// （且不因未来时间让 Assessment 永久 fresh）；允许小幅周期边界容差。
func (s *RiskV2AdminService) freshness(assessedAtUnix int64, now time.Time) (bool, int64, bool) {
	age := now.Unix() - assessedAtUnix
	anomaly := false
	if age < -riskV2ClockToleranceSeconds {
		anomaly = true // assessed_at 明显在未来
	}
	if age < 0 {
		age = 0
	}
	fresh := RiskV2AssessmentFresh(assessedAtUnix, now, s.staleAfter)
	if anomaly {
		fresh = false // 未来时间不得判为 fresh
	}
	return fresh, age, anomaly
}

// riskV2ClockToleranceSeconds 周期边界/时钟漂移容差（秒）。
const riskV2ClockToleranceSeconds = 120
