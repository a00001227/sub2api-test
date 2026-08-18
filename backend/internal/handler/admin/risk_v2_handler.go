package admin

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// 切片 5：Risk V2 只读 Admin API。纯只读——绝不修改 tier/index/reason/阈值/allowlist，不封禁/限速/暂停/触发重评分。
// RBAC 复用现有 admin 路由组鉴权（未认证 401 / 非 admin 403 由上层中间件处理）。

// —— 稳定 error code ——
const (
	riskV2ErrSchemaNotReady   = "RISK_V2_SCHEMA_NOT_READY"
	riskV2ErrLiveUnavailable  = "RISK_V2_LIVE_METRICS_UNAVAILABLE"
	riskV2ErrLiveBusy         = "RISK_V2_LIVE_METRICS_BUSY"
	riskV2ErrLiveRateLimited  = "RISK_V2_LIVE_METRICS_RATE_LIMITED"
	riskV2ErrAssessmentNotFnd = "RISK_V2_ASSESSMENT_NOT_FOUND"
	riskV2ErrBadRequest       = "RISK_V2_BAD_REQUEST"
	riskV2ErrInternal         = "RISK_V2_INTERNAL"
)

const (
	riskV2ListDefaultLimit = 100
	riskV2ListMaxLimit     = 200
	riskV2LiveConcurrency  = 4 // Live Window 接口全局并发上限
	riskV2ValidTiers       = "INSUFFICIENT_DATA,WATCH,MEDIUM,HIGH"
)

// RiskV2Handler 只读 Admin Handler。liveSem 限并发；rl 限流（每管理员+全局）。
type RiskV2Handler struct {
	svc     *service.RiskV2AdminService
	liveSem chan struct{}
	rl      *riskV2RateLimiter
}

// NewRiskV2Handler 构造只读 Admin Handler。ratePerSec/burst 为 Live Window 每管理员限流参数（<=0 回落默认）。
func NewRiskV2Handler(svc *service.RiskV2AdminService, ratePerSec, burst int) *RiskV2Handler {
	return &RiskV2Handler{
		svc:     svc,
		liveSem: make(chan struct{}, riskV2LiveConcurrency),
		rl:      newRiskV2RateLimiter(ratePerSec, burst, nil),
	}
}

// —— 响应 DTO（显式白名单；绝不序列化 repo/ent/config/worker/redis 内部对象）——

type RiskV2APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type RiskV2Meta struct {
	Shadow                 bool `json:"shadow"`                    // 仅影子观测
	Enforcement            bool `json:"enforcement"`               // 恒 false（EffectiveAction=NONE）
	RiskIndexIsProbability bool `json:"risk_index_is_probability"` // 恒 false：risk_index 是风险指数，非概率
}

func riskV2Meta() RiskV2Meta {
	return RiskV2Meta{Shadow: true, Enforcement: false, RiskIndexIsProbability: false}
}

type RiskV2RuntimeStatusResponse struct {
	Available            bool       `json:"available"` // 状态接口恒 true（即使 V2 关闭）
	Shadow               bool       `json:"shadow"`
	Enabled              bool       `json:"enabled"`
	AggregationEnabled   bool       `json:"aggregation_enabled"`
	ScoringEnabled       bool       `json:"scoring_enabled"`
	PersistEnabled       bool       `json:"persist_enabled"`
	DispatcherReady      bool       `json:"dispatcher_ready"`
	ReporterReady        bool       `json:"reporter_ready"`
	WorkerReady          bool       `json:"worker_ready"`
	WorkerDegraded       bool       `json:"worker_degraded"`
	SchemaReady          bool       `json:"schema_ready"`
	Leader               bool       `json:"leader"`
	CurrentCycle         int64      `json:"current_cycle"`
	LastCompletedCycle   int64      `json:"last_completed_cycle"`
	LastCycleStatus      string     `json:"last_cycle_status"`
	QueueDepth           int        `json:"queue_depth"`
	ObservationDropRatio float64    `json:"observation_drop_ratio"`
	HealthAvailable      bool       `json:"health_available"`
	HealthCoverageRatio  float64    `json:"health_coverage_ratio"`
	LastErrorCode        string     `json:"last_error_code"`
	LastUpdatedAt        int64      `json:"last_updated_at"`
	Meta                 RiskV2Meta `json:"meta"`
}

type RiskV2UserSummaryDTO struct {
	UserID   int64  `json:"user_id"`
	Email    string `json:"email"`
	Username string `json:"username"`
	Status   string `json:"status"`
	Known    bool   `json:"known"`
}

type RiskV2ReasonCodeDTO struct {
	Code                   string  `json:"code"`
	Window                 string  `json:"window"`
	ObservedValue          float64 `json:"observed_value"`
	Threshold              float64 `json:"threshold"`
	EvidenceFamily         string  `json:"evidence_family"`
	EvidenceGroup          string  `json:"evidence_group"`
	ConfidenceContribution float64 `json:"confidence_contribution"`
}

type RiskV2PlaneDTO struct {
	Score     *float64 `json:"score"` // 不可用为 null（绝不用 0 冒充）
	Available bool     `json:"available"`
}

type RiskV2AssessmentListItemDTO struct {
	User                  RiskV2UserSummaryDTO  `json:"user"`
	UserID                int64                 `json:"user_id"`
	RiskIndex             float64               `json:"risk_index"`
	RiskTier              string                `json:"risk_tier"`
	Confidence            float64               `json:"confidence"`
	DataSufficient        bool                  `json:"data_sufficient"`
	Automation            RiskV2PlaneDTO        `json:"automation"`
	Harvest               RiskV2PlaneDTO        `json:"harvest"`
	Campaign              RiskV2PlaneDTO        `json:"campaign"`
	Exposure              RiskV2PlaneDTO        `json:"exposure"`
	Degraded              bool                  `json:"degraded"`
	Incomplete            bool                  `json:"incomplete"`
	HealthAvailable       bool                  `json:"health_available"`
	TopReasonCodes        []RiskV2ReasonCodeDTO `json:"top_reason_codes"`
	AssessedAt            int64                 `json:"assessed_at"`
	UpdatedAt             int64                 `json:"updated_at"`
	Fresh                 bool                  `json:"fresh"`
	AgeSeconds            int64                 `json:"age_seconds"`
	TimeAnomaly           bool                  `json:"time_anomaly"`
	FeatureVersion        string                `json:"feature_version"`
	PolicyVersion         string                `json:"policy_version"`
	FingerprintKeyVersion string                `json:"fingerprint_key_version"`
	EffectiveAction       string                `json:"effective_action"` // 恒 NONE
}

type RiskV2AssessmentListResponse struct {
	Items            []RiskV2AssessmentListItemDTO `json:"items"`
	Limit            int                           `json:"limit"`
	Offset           int                           `json:"offset"`
	HasMore          bool                          `json:"has_more"`
	NextOffset       int                           `json:"next_offset"`
	StaleAfterSecond int64                         `json:"stale_after_seconds"`
	Meta             RiskV2Meta                    `json:"meta"`
}

type RiskV2EvidenceFamilyDTO struct {
	Family    string  `json:"family"`
	Group     string  `json:"group"`
	Available bool    `json:"available"`
	Strength  float64 `json:"strength"`
	MeetsHigh bool    `json:"meets_high"`
	Window    string  `json:"window"`
}

type RiskV2EvidenceGroupDTO struct {
	Group    string  `json:"group"`
	MetHigh  bool    `json:"met_high"`
	Strength float64 `json:"strength"`
}

type RiskV2FeatureAvailabilityDTO struct {
	Requests           bool `json:"requests"`
	Fingerprint        bool `json:"fingerprint"`
	Cache              bool `json:"cache"`
	ActiveMinutes      bool `json:"active_minutes"`
	NearDuplicate      bool `json:"near_duplicate"`
	ToolUse            bool `json:"tool_use"`
	StructuredOutput   bool `json:"structured_output"`
	ModelConcentration bool `json:"model_concentration"`
}

type RiskV2AssessmentDetailResponse struct {
	User                  RiskV2UserSummaryDTO         `json:"user"`
	UserID                int64                        `json:"user_id"`
	RiskIndex             float64                      `json:"risk_index"`
	RiskTier              string                       `json:"risk_tier"`
	Confidence            float64                      `json:"confidence"`
	DataSufficient        bool                         `json:"data_sufficient"`
	Automation            RiskV2PlaneDTO               `json:"automation"`
	Harvest               RiskV2PlaneDTO               `json:"harvest"`
	Campaign              RiskV2PlaneDTO               `json:"campaign"`
	Exposure              RiskV2PlaneDTO               `json:"exposure"`
	FeatureAvailability   RiskV2FeatureAvailabilityDTO `json:"feature_availability"`
	EvidenceFamilies      []RiskV2EvidenceFamilyDTO    `json:"evidence_families"`
	EvidenceGroups        []RiskV2EvidenceGroupDTO     `json:"evidence_groups"`
	ReasonCodes           []RiskV2ReasonCodeDTO        `json:"reason_codes"`
	IncompleteReasons     []string                     `json:"incomplete_reasons"`
	Degraded              bool                         `json:"degraded"`
	Incomplete            bool                         `json:"incomplete"`
	HealthAvailable       bool                         `json:"health_available"`
	AssessedAt            int64                        `json:"assessed_at"`
	Fresh                 bool                         `json:"fresh"`
	AgeSeconds            int64                        `json:"age_seconds"`
	TimeAnomaly           bool                         `json:"time_anomaly"`
	StaleAfterSecond      int64                        `json:"stale_after_seconds"`
	FeatureVersion        string                       `json:"feature_version"`
	PolicyVersion         string                       `json:"policy_version"`
	FingerprintKeyVersion string                       `json:"fingerprint_key_version"`
	EffectiveAction       string                       `json:"effective_action"` // 恒 NONE
	Meta                  RiskV2Meta                   `json:"meta"`
}

type RiskV2WindowDTO struct {
	Window            string `json:"window"`
	Available         bool   `json:"available"` // 该窗口是否有请求数据
	RequestCount      int64  `json:"request_count"`
	SuccessCount      int64  `json:"success_count"`
	ActiveMinutes     int    `json:"active_minutes"`
	PeakRPM           int    `json:"peak_rpm"`
	InputTokens       int64  `json:"input_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	DistinctExact     int    `json:"distinct_exact_estimate"`
	DistinctScaffold  int    `json:"distinct_full_scaffold_estimate"`
	UniqueAPIKeyCount int    `json:"unique_api_key_count"`
}

type RiskV2MultiKeyDTO struct {
	MultiKeyAvailable                      bool `json:"multi_key_available"`
	ActiveAPIKeyCount24h                   int  `json:"active_api_key_count_24h"`
	SynchronizedMultiKeyMinutes1h          int  `json:"synchronized_multi_key_minutes_1h"`
	CrossKeyFullScaffoldOverlapEstimate1h  int  `json:"cross_key_full_scaffold_overlap_estimate_1h"`
	CrossKeyFullScaffoldOverlapAvailable1h bool `json:"cross_key_full_scaffold_overlap_available_1h"`
	KeyHandoffCount1h                      int  `json:"key_handoff_count_1h"`
	MultiKeyIncomplete                     bool `json:"multi_key_incomplete"`
}

type RiskV2WindowSummaryResponse struct {
	UserID                int64             `json:"user_id"`
	DataAvailable         bool              `json:"data_available"` // false=Redis 正常但用户无近期数据
	W5m                   RiskV2WindowDTO   `json:"w_5m"`
	W1h                   RiskV2WindowDTO   `json:"w_1h"`
	W24h                  RiskV2WindowDTO   `json:"w_24h"`
	MultiKey              RiskV2MultiKeyDTO `json:"multi_key"`
	Degraded              bool              `json:"degraded"`
	Incomplete            bool              `json:"incomplete"`
	IncompleteReasons     []string          `json:"incomplete_reasons"`
	AssessedAt            int64             `json:"assessed_at"`
	FingerprintKeyVersion string            `json:"fingerprint_key_version"`
	Meta                  RiskV2Meta        `json:"meta"`
}

// —— helpers ——

func riskV2NoStore(c *gin.Context) { c.Header("Cache-Control", "no-store") }

func riskV2WriteErr(c *gin.Context, status int, code, msg string) {
	riskV2NoStore(c)
	c.AbortWithStatusJSON(status, RiskV2APIError{Code: code, Message: msg})
}

func riskV2OK(c *gin.Context, data any) {
	riskV2NoStore(c)
	c.JSON(http.StatusOK, data)
}

func fptr(v float64, avail bool) *float64 {
	if !avail {
		return nil
	}
	return &v
}

// GetStatus GET <admin>/risk/v2/status —— V2 关闭时仍返回 200。
func (h *RiskV2Handler) GetStatus(c *gin.Context) {
	st := h.svc.RuntimeStatus(c.Request.Context())
	riskV2OK(c, RiskV2RuntimeStatusResponse{
		Available: true, Shadow: true,
		Enabled: st.Enabled, AggregationEnabled: st.AggregationEnabled, ScoringEnabled: st.ScoringEnabled, PersistEnabled: st.PersistEnabled,
		DispatcherReady: st.DispatcherReady, ReporterReady: st.ReporterReady, WorkerReady: st.WorkerReady, WorkerDegraded: st.WorkerDegraded,
		SchemaReady: st.SchemaReady, Leader: st.Leader, CurrentCycle: st.CurrentCycle, LastCompletedCycle: st.LastCompletedCycle,
		LastCycleStatus: st.LastCycleStatus, QueueDepth: st.QueueDepth, ObservationDropRatio: st.ObservationDropRatio,
		HealthAvailable: st.HealthAvailable, HealthCoverageRatio: st.HealthCoverageRatio, LastErrorCode: st.LastErrorCode,
		LastUpdatedAt: st.LastUpdatedAt, Meta: riskV2Meta(),
	})
}

// ListUsers GET <admin>/risk/v2/users
func (h *RiskV2Handler) ListUsers(c *gin.Context) {
	filter, page, freshOnly, ok := parseRiskV2ListQuery(c)
	if !ok {
		riskV2WriteErr(c, http.StatusBadRequest, riskV2ErrBadRequest, "invalid query parameters")
		return
	}
	pg, err := h.svc.ListAssessments(c.Request.Context(), filter, page, freshOnly)
	if err != nil {
		h.writeServiceErr(c, err)
		return
	}
	items := make([]RiskV2AssessmentListItemDTO, 0, len(pg.Rows))
	for i := range pg.Rows {
		items = append(items, mapListItem(pg.Rows[i]))
	}
	riskV2OK(c, RiskV2AssessmentListResponse{
		Items: items, Limit: pg.Limit, Offset: pg.Offset,
		HasMore: pg.HasMore, NextOffset: pg.NextOffset,
		StaleAfterSecond: h.svc.StaleAfterSeconds(), Meta: riskV2Meta(),
	})
}

// GetUser GET <admin>/risk/v2/users/:user_id
func (h *RiskV2Handler) GetUser(c *gin.Context) {
	uid, err := strconv.ParseInt(strings.TrimSpace(c.Param("user_id")), 10, 64)
	if err != nil || uid <= 0 {
		riskV2WriteErr(c, http.StatusBadRequest, riskV2ErrBadRequest, "invalid user id")
		return
	}
	d, err := h.svc.GetAssessmentDetail(c.Request.Context(), uid)
	if err != nil {
		h.writeServiceErr(c, err)
		return
	}
	riskV2OK(c, mapDetail(d, h.svc.StaleAfterSeconds()))
}

// GetWindows GET <admin>/risk/v2/users/:user_id/windows —— 轻量实时 Redis 读（仅用户级+MultiKey），
// 带每管理员限流 + 全局并发上限 + 独立超时。绝不调用重量级 AdminDetail Reader。
func (h *RiskV2Handler) GetWindows(c *gin.Context) {
	uid, err := strconv.ParseInt(strings.TrimSpace(c.Param("user_id")), 10, 64)
	if err != nil || uid <= 0 {
		riskV2WriteErr(c, http.StatusBadRequest, riskV2ErrBadRequest, "invalid user id")
		return
	}
	// §二 限流：每管理员令牌桶（防前端高频轮询）。命中 → 429 RATE_LIMITED（与 BUSY 区分）。
	if !h.rl.Allow(riskV2AdminKey(c)) {
		riskV2WriteErr(c, http.StatusTooManyRequests, riskV2ErrLiveRateLimited, "live metrics rate limit exceeded")
		return
	}
	// 全局并发上限：满则拒绝（不排队、不阻塞）。
	select {
	case h.liveSem <- struct{}{}:
		defer func() { <-h.liveSem }()
	default:
		riskV2WriteErr(c, http.StatusTooManyRequests, riskV2ErrLiveBusy, "live metrics concurrency limit reached")
		return
	}
	lw, err := h.svc.LiveWindowSummary(c.Request.Context(), uid)
	if err != nil {
		h.writeServiceErr(c, err)
		return
	}
	riskV2OK(c, mapWindows(uid, lw))
}

// riskV2AdminKey 取「已认证管理员 user ID」作为限流 key（§5.1 §一.2）。
// 路由在 admin 鉴权之后，AuthSubject 恒存在；若异常缺失，回落到「进程级全局」单键（而非可伪造的
// X-Forwarded-For/client IP）——绝不把 XFF 当作已认证管理员身份。
//
// 注意：本 limiter 的全局桶是 **per-process**，不是 cluster-global；多实例部署下总量上限按实例计。
func riskV2AdminKey(c *gin.Context) string {
	if subj, ok := middleware.GetAuthSubjectFromContext(c); ok && subj.UserID > 0 {
		return "admin:" + strconv.FormatInt(subj.UserID, 10)
	}
	return "__no_subject_global__"
}

func (h *RiskV2Handler) writeServiceErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrRiskV2SchemaNotReady):
		riskV2WriteErr(c, http.StatusServiceUnavailable, riskV2ErrSchemaNotReady, "risk v2 schema not ready")
	case errors.Is(err, service.ErrRiskV2LiveMetricsUnavailable):
		riskV2WriteErr(c, http.StatusServiceUnavailable, riskV2ErrLiveUnavailable, "risk v2 live metrics unavailable")
	case errors.Is(err, service.ErrRiskV2AssessmentNotFound):
		riskV2WriteErr(c, http.StatusNotFound, riskV2ErrAssessmentNotFnd, "assessment not found")
	default:
		// 绝不把 SQL/Redis/stack 返回客户端。
		riskV2WriteErr(c, http.StatusInternalServerError, riskV2ErrInternal, "internal error")
	}
}

// —— query 解析 ——

// parseRiskV2ListQuery 严格校验所有查询参数（§三）。任何非法 → ok=false（Handler 返回 400），
// 绝不静默忽略、绝不把非法值改 0、绝不把 NaN/Inf 传给 PostgreSQL。
func parseRiskV2ListQuery(c *gin.Context) (service.RiskV2ListFilter, service.RiskV2Pagination, *bool, bool) {
	var f service.RiskV2ListFilter
	bad := func() (service.RiskV2ListFilter, service.RiskV2Pagination, *bool, bool) {
		return service.RiskV2ListFilter{}, service.RiskV2Pagination{}, nil, false
	}
	q := c.Request.URL.Query()

	// tier：仅合法 RiskTier。
	if t := strings.TrimSpace(q.Get("tier")); t != "" {
		if !strings.Contains(","+riskV2ValidTiers+",", ","+t+",") {
			return bad()
		}
		f.Tier = t
	}
	// user_id：正整数。
	if v := strings.TrimSpace(q.Get("user_id")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return bad()
		}
		f.UserID = n
	}
	// min_risk_index：finite 且 0~100。
	if v := strings.TrimSpace(q.Get("min_risk_index")); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 100 {
			return bad()
		}
		f.MinRiskIndex = &n
	}
	// data_sufficient / degraded：仅合法 bool。
	if b, ok, valid := parseBoolQ(q.Get("data_sufficient")); !valid {
		return bad()
	} else if ok {
		f.DataSufficient = &b
	}
	if b, ok, valid := parseBoolQ(q.Get("degraded")); !valid {
		return bad()
	} else if ok {
		f.Degraded = &b
	}
	// assessed_from / assessed_to：非负；若都给则 from <= to。
	fromSet, toSet := false, false
	if v := strings.TrimSpace(q.Get("assessed_from")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return bad()
		}
		f.AssessedFromUnix = n
		fromSet = true
	}
	if v := strings.TrimSpace(q.Get("assessed_to")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return bad()
		}
		f.AssessedToUnix = n
		toSet = true
	}
	if fromSet && toSet && f.AssessedFromUnix > f.AssessedToUnix {
		return bad()
	}
	// freshness：仅 fresh / stale / all / 空。
	var freshOnly *bool
	switch strings.ToLower(strings.TrimSpace(q.Get("freshness"))) {
	case "fresh":
		t := true
		freshOnly = &t
	case "stale":
		fl := false
		freshOnly = &fl
	case "", "all":
	default:
		return bad()
	}
	// 分页：limit 必须为正整数且 <= 上限 200（缺省 100）；offset >= 0。
	page := service.RiskV2Pagination{Limit: riskV2ListDefaultLimit}
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return bad()
		}
		if n > riskV2ListMaxLimit {
			n = riskV2ListMaxLimit
		}
		page.Limit = n
	}
	if v := strings.TrimSpace(q.Get("offset")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return bad()
		}
		page.Offset = n
	}
	return f, page, freshOnly, true
}

// parseBoolQ 返回 (值, 存在, 合法)。空 → (false,false,true)。
func parseBoolQ(s string) (bool, bool, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return false, false, true
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		return false, false, false
	}
	return b, true, true
}

// —— 映射 service → DTO ——

func mapUser(u service.RiskV2UserSummary) RiskV2UserSummaryDTO {
	return RiskV2UserSummaryDTO{UserID: u.UserID, Email: u.Email, Username: u.Username, Status: u.Status, Known: u.Known}
}

func mapReasonCodes(rc []service.RiskV2ReasonCode) []RiskV2ReasonCodeDTO {
	out := make([]RiskV2ReasonCodeDTO, 0, len(rc))
	for _, r := range rc {
		out = append(out, RiskV2ReasonCodeDTO{
			Code: r.Code, Window: r.Window, ObservedValue: r.ObservedValue, Threshold: r.Threshold,
			EvidenceFamily: r.EvidenceFamily, EvidenceGroup: r.EvidenceGroup, ConfidenceContribution: r.ConfidenceContribution,
		})
	}
	return out
}

func mapListItem(row service.RiskV2AdminAssessmentRow) RiskV2AssessmentListItemDTO {
	it := row.Item
	action := it.EffectiveAction
	if action == "" {
		action = "NONE"
	}
	return RiskV2AssessmentListItemDTO{
		User: mapUser(row.User), UserID: it.UserID, RiskIndex: it.RiskIndex, RiskTier: it.RiskTier,
		Confidence: it.Confidence, DataSufficient: it.DataSufficient,
		Automation: RiskV2PlaneDTO{Score: it.AutomationScore, Available: it.AutomationScore != nil},
		Harvest:    RiskV2PlaneDTO{Score: it.HarvestScore, Available: it.HarvestScore != nil},
		Campaign:   RiskV2PlaneDTO{Score: it.CampaignScore, Available: it.CampaignScore != nil},
		Exposure:   RiskV2PlaneDTO{Score: it.ExposureScore, Available: it.ExposureScore != nil},
		Degraded:   it.Degraded, Incomplete: it.Incomplete, HealthAvailable: it.HealthAvailable,
		TopReasonCodes: mapReasonCodes(it.TopReasonCodes),
		AssessedAt:     it.AssessedAtUnix, UpdatedAt: it.UpdatedAtUnix, Fresh: row.Fresh, AgeSeconds: row.AgeSeconds, TimeAnomaly: row.TimeAnomaly,
		FeatureVersion: it.FeatureVersion, PolicyVersion: it.PolicyVersion, FingerprintKeyVersion: it.FingerprintKeyVersion,
		EffectiveAction: action,
	}
}

func mapDetail(d service.RiskV2AdminAssessmentDetail, staleAfter int64) RiskV2AssessmentDetailResponse {
	a := d.Assessment
	action := a.EffectiveAction
	if action == "" {
		action = "NONE"
	}
	fams := make([]RiskV2EvidenceFamilyDTO, 0, len(a.EvidenceFamilies))
	for _, f := range a.EvidenceFamilies {
		fams = append(fams, RiskV2EvidenceFamilyDTO{Family: f.Family, Group: f.Group, Available: f.Available, Strength: f.Strength, MeetsHigh: f.MeetsHigh, Window: f.Window})
	}
	grps := make([]RiskV2EvidenceGroupDTO, 0, len(a.EvidenceGroups))
	for _, g := range a.EvidenceGroups {
		grps = append(grps, RiskV2EvidenceGroupDTO{Group: g.Group, MetHigh: g.MetHigh, Strength: g.Strength})
	}
	fa := a.FeatureAvailability
	return RiskV2AssessmentDetailResponse{
		User: mapUser(d.User), UserID: d.User.UserID, RiskIndex: a.RiskIndex, RiskTier: a.RiskTier,
		Confidence: a.Confidence, DataSufficient: a.DataSufficient,
		Automation: RiskV2PlaneDTO{Score: fptr(a.Automation.Score, a.Automation.Available), Available: a.Automation.Available},
		Harvest:    RiskV2PlaneDTO{Score: fptr(a.Harvest.Score, a.Harvest.Available), Available: a.Harvest.Available},
		Campaign:   RiskV2PlaneDTO{Score: fptr(a.Campaign.Score, a.Campaign.Available), Available: a.Campaign.Available},
		Exposure:   RiskV2PlaneDTO{Score: fptr(a.Exposure.Score, a.Exposure.Available), Available: a.Exposure.Available},
		FeatureAvailability: RiskV2FeatureAvailabilityDTO{
			Requests: fa.Requests, Fingerprint: fa.Fingerprint, Cache: fa.Cache, ActiveMinutes: fa.ActiveMinutes,
			NearDuplicate: fa.NearDuplicate, ToolUse: fa.ToolUse, StructuredOutput: fa.StructuredOutput, ModelConcentration: fa.ModelConcentration,
		},
		EvidenceFamilies: fams, EvidenceGroups: grps, ReasonCodes: mapReasonCodes(a.ReasonCodes),
		IncompleteReasons: a.IncompleteReasons, Degraded: a.Degraded, Incomplete: a.Incomplete, HealthAvailable: a.HealthAvailable,
		AssessedAt: a.AssessedAtUnix, Fresh: d.Fresh, AgeSeconds: d.AgeSeconds, TimeAnomaly: d.TimeAnomaly, StaleAfterSecond: staleAfter,
		FeatureVersion: a.FeatureVersion, PolicyVersion: a.PolicyVersion, FingerprintKeyVersion: a.FingerprintKeyVersion,
		EffectiveAction: action, Meta: riskV2Meta(),
	}
}

func mapWindow(w service.RiskV2Window) RiskV2WindowDTO {
	return RiskV2WindowDTO{
		Window: w.WindowLabel, Available: w.RequestCount > 0, RequestCount: w.RequestCount, SuccessCount: w.SuccessCount, ActiveMinutes: w.ActiveMinutes,
		PeakRPM: w.PeakRPM, InputTokens: w.InputTokens, OutputTokens: w.OutputTokens,
		DistinctExact: w.DistinctExactEstimate, DistinctScaffold: w.DistinctFullScaffoldEstimate, UniqueAPIKeyCount: w.UniqueAPIKeyCount,
	}
}

func mapWindows(uid int64, lw service.RiskV2AdminLiveWindow) RiskV2WindowSummaryResponse {
	m := lw.Snapshot
	mk := m.MultiKey
	return RiskV2WindowSummaryResponse{
		UserID: uid, DataAvailable: lw.DataAvailable,
		W5m: mapWindow(m.User.W5m), W1h: mapWindow(m.User.W1h), W24h: mapWindow(m.User.W24h),
		MultiKey: RiskV2MultiKeyDTO{
			MultiKeyAvailable: mk.MultiKeyAvailable, ActiveAPIKeyCount24h: mk.ActiveAPIKeyCount24h,
			SynchronizedMultiKeyMinutes1h: mk.SynchronizedMultiKeyMinutes1h, CrossKeyFullScaffoldOverlapEstimate1h: mk.CrossKeyFullScaffoldOverlapEstimate1h,
			CrossKeyFullScaffoldOverlapAvailable1h: mk.CrossKeyFullScaffoldOverlapAvailable1h, KeyHandoffCount1h: mk.KeyHandoffCount1h,
			MultiKeyIncomplete: mk.MultiKeyIncomplete,
		},
		Degraded: m.Degraded, Incomplete: m.Incomplete, IncompleteReasons: m.IncompleteReasons,
		AssessedAt: m.AssessedAtUnix, FingerprintKeyVersion: m.FingerprintKeyVersion, Meta: riskV2Meta(),
	}
}
