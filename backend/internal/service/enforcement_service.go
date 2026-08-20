package service

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

var (
	// ErrEnforcementUnavailable 执行层未启用（store/repo 缺失 / master 关）。
	ErrEnforcementUnavailable = errors.New("enforcement: unavailable")
	// ErrEnforcementBadRequest 入参非法。
	ErrEnforcementBadRequest = errors.New("enforcement: bad request")
)

// 蒸馏执行层：HIGH 自动限速 + 人工封禁的自动限速部分。
//
// 设计原则：这是 risk_v2 tier 的【独立消费者】——只读、绝不改 scoring（effective_action 恒 NONE）。
// 后台 ticker 从 user_risk_v2 表拉「HIGH 且 confidence≥地板 且 data_sufficient」的用户进内存集合；
// 网关中间件零 I/O 判断当前用户是否命中且未豁免 → 命中则走一个独立的低 RPM 分钟桶（不动用户正常配额）。
// 豁免名单一票否决。master 开关默认关 → Active()=false，中间件与端点整体 no-op、零开销。封禁只走人工 admin API。

const (
	enforcementThrottleRPMDefault     = 5
	enforcementConfidenceMinDefault   = 0.6
	enforcementRefreshIntervalDefault = 60 * time.Second
	enforcementCounterTTLDefault      = 2 * time.Hour
	enforcementRefreshOpTimeout       = 10 * time.Second
	enforcementIncrOpTimeout          = 2 * time.Second
	enforcementHighUsersPageLimit     = 500 // = riskV2ListMaxLimit（仓储硬上限）
	enforcementMaxHighUsersPages      = 40  // 上限 40*500=20000，防病态无界循环
)

// 受限模型处置动作。
const (
	EnforcementActionBlock    = "block"    // 直接拒绝该模型
	EnforcementActionThrottle = "throttle" // 该模型独立低 RPM
)

// EnforcementStore 由 repository 实现（Redis）：豁免名单 + 独立限速计数 + 受限模型规则。
type EnforcementStore interface {
	LoadAllowlist(ctx context.Context) ([]int64, error)
	AddAllowlist(ctx context.Context, userID int64) error
	RemoveAllowlist(ctx context.Context, userID int64) error
	IncrThrottleCounter(ctx context.Context, userID int64, ttl time.Duration) (int64, error)
	IncrModelThrottleCounter(ctx context.Context, userID int64, model string, ttl time.Duration) (int64, error)
	LoadModelRules(ctx context.Context) (map[string]string, error)
	SetModelRule(ctx context.Context, model, action string) error
	DeleteModelRule(ctx context.Context, model string) error
}

// EnforcementConfigView 运行参数（从 config 提取）。
type EnforcementConfigView struct {
	Enabled         bool
	ThrottleRPM     int
	ConfidenceMin   float64
	RefreshInterval time.Duration
	CounterTTL      time.Duration
}

// EnforcementStatus 执行层运行态快照（供 admin status 端点）。
type EnforcementStatus struct {
	Enabled        bool    `json:"enabled"`
	ThrottleRPM    int     `json:"throttle_rpm"`
	ConfidenceMin  float64 `json:"confidence_min"`
	HighUserCount  int     `json:"high_user_count"`
	AllowlistSize  int     `json:"allowlist_size"`
	ModelRuleCount int     `json:"model_rule_count"`
	RefreshedAt    int64   `json:"refreshed_at"`
}

// EnforcementModelRule 一条受限模型规则（供 admin 列表）。
type EnforcementModelRule struct {
	Model  string `json:"model"`
	Action string `json:"action"` // block | throttle
}

// enforcementSnapshot 是不可变的内存快照，用 atomic.Pointer 原子替换（读侧无锁）。
type enforcementSnapshot struct {
	highUsers   map[int64]struct{}
	allowlist   map[int64]struct{}
	modelRules  map[string]string // model → action（block|throttle）
	refreshedAt int64
}

// EnforcementService 管理 HIGH 名单刷新 + 热路径限速判定 + 豁免名单。
type EnforcementService struct {
	store EnforcementStore
	repo  UserRiskV2Repository
	cfg   EnforcementConfigView

	snap atomic.Pointer[enforcementSnapshot]
	gate atomic.Int32 // >0 才需要检查（Enabled && 有 HIGH 用户）；零开销闸门

	stopOnce sync.Once
	stop     chan struct{}
	wg       sync.WaitGroup
}

// NewEnforcementService 构造并载入豁免名单 + 首次刷新 HIGH 名单（best-effort）。
// store/repo 为 nil 或 master 关 → 禁用态服务（Active()=false，中间件/端点 no-op）。
func NewEnforcementService(store EnforcementStore, repo UserRiskV2Repository, cfg EnforcementConfigView) *EnforcementService {
	if cfg.ThrottleRPM <= 0 {
		cfg.ThrottleRPM = enforcementThrottleRPMDefault
	}
	if cfg.ConfidenceMin <= 0 {
		cfg.ConfidenceMin = enforcementConfidenceMinDefault
	}
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = enforcementRefreshIntervalDefault
	}
	if cfg.CounterTTL <= 0 {
		cfg.CounterTTL = enforcementCounterTTLDefault
	}
	s := &EnforcementService{store: store, repo: repo, cfg: cfg, stop: make(chan struct{})}
	s.snap.Store(&enforcementSnapshot{highUsers: map[int64]struct{}{}, allowlist: map[int64]struct{}{}, modelRules: map[string]string{}})
	if s.configured() {
		ctx, cancel := context.WithTimeout(context.Background(), enforcementRefreshOpTimeout)
		defer cancel()
		// 豁免名单 + 受限模型规则无论 master 开关都载入（admin 可在启用前预置）；HIGH 名单仅启用时才拉。
		allow := s.loadAllowlist(ctx)
		rules := s.loadModelRules(ctx)
		var high map[int64]struct{}
		if s.cfg.Enabled {
			high = s.loadHighUsers(ctx)
		}
		s.swap(high, allow, rules)
	}
	return s
}

// configured 表示 store/repo 齐备（不看 master 开关）——供 admin API（可在启用前预置豁免/预览名单）。
func (s *EnforcementService) configured() bool {
	return s != nil && s.store != nil && s.repo != nil
}

// available 表示已配置且 master 开——供热路径限速闸门。
func (s *EnforcementService) available() bool {
	return s.configured() && s.cfg.Enabled
}

// Active 是热路径零开销闸门：master 开且当前有 HIGH 用户才为 true（一次原子读）。
func (s *EnforcementService) Active() bool {
	return s != nil && s.gate.Load() > 0
}

// Start 启动后台刷新 goroutine（周期从 user_risk_v2 拉 HIGH 名单）。禁用态 no-op。
func (s *EnforcementService) Start() {
	if !s.available() {
		return
	}
	s.wg.Add(1)
	go s.refreshLoop()
	logger.L().With(zap.String("component", "service.enforcement")).
		Info("enforcement started", zap.Int("throttle_rpm", s.cfg.ThrottleRPM), zap.Float64("confidence_min", s.cfg.ConfidenceMin))
}

// Stop 停止刷新 goroutine（nil-safe，幂等）。
func (s *EnforcementService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stop) })
	s.wg.Wait()
}

func (s *EnforcementService) refreshLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.cfg.RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), enforcementRefreshOpTimeout)
			high := s.loadHighUsers(ctx)
			allow := s.loadAllowlist(ctx)
			rules := s.loadModelRules(ctx)
			cancel()
			s.swap(high, allow, rules)
		}
	}
}

// loadHighUsers 分页拉「HIGH 且 confidence≥地板 且 data_sufficient」的用户集合。失败返回当前快照的 highUsers（不清空）。
func (s *EnforcementService) loadHighUsers(ctx context.Context) map[int64]struct{} {
	dataSufficient := true
	filter := RiskV2ListFilter{Tier: RiskV2TierHigh, DataSufficient: &dataSufficient}
	out := make(map[int64]struct{})
	for page := 0; page < enforcementMaxHighUsersPages; page++ {
		items, err := s.repo.ListCurrentAssessments(ctx, filter, RiskV2Pagination{Limit: enforcementHighUsersPageLimit, Offset: page * enforcementHighUsersPageLimit})
		if err != nil {
			logger.L().With(zap.String("component", "service.enforcement")).
				Warn("load high users failed, keeping previous snapshot", zap.Error(err))
			if cur := s.snap.Load(); cur != nil {
				return cur.highUsers
			}
			return out
		}
		for _, it := range items {
			if it.Confidence >= s.cfg.ConfidenceMin && it.DataSufficient && it.UserID > 0 {
				out[it.UserID] = struct{}{}
			}
		}
		if len(items) < enforcementHighUsersPageLimit {
			break
		}
	}
	return out
}

// loadAllowlist 载入豁免名单。失败返回当前快照的 allowlist（不清空）。
func (s *EnforcementService) loadAllowlist(ctx context.Context) map[int64]struct{} {
	ids, err := s.store.LoadAllowlist(ctx)
	if err != nil {
		logger.L().With(zap.String("component", "service.enforcement")).
			Warn("load allowlist failed, keeping previous", zap.Error(err))
		if cur := s.snap.Load(); cur != nil {
			return cur.allowlist
		}
		return map[int64]struct{}{}
	}
	out := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out
}

// loadModelRules 载入受限模型规则。失败返回当前快照的 modelRules（不清空）。
func (s *EnforcementService) loadModelRules(ctx context.Context) map[string]string {
	rules, err := s.store.LoadModelRules(ctx)
	if err != nil {
		logger.L().With(zap.String("component", "service.enforcement")).
			Warn("load model rules failed, keeping previous", zap.Error(err))
		if cur := s.snap.Load(); cur != nil {
			return cur.modelRules
		}
		return map[string]string{}
	}
	if rules == nil {
		rules = map[string]string{}
	}
	return rules
}

// swap 原子替换快照并更新闸门。
func (s *EnforcementService) swap(high, allow map[int64]struct{}, rules map[string]string) {
	if high == nil {
		high = map[int64]struct{}{}
	}
	if allow == nil {
		allow = map[int64]struct{}{}
	}
	if rules == nil {
		rules = map[string]string{}
	}
	s.snap.Store(&enforcementSnapshot{highUsers: high, allowlist: allow, modelRules: rules, refreshedAt: time.Now().Unix()})
	if s.cfg.Enabled && len(high) > 0 {
		s.gate.Store(1)
	} else {
		s.gate.Store(0)
	}
}

func (s *EnforcementService) currentRules() map[string]string {
	if snap := s.snap.Load(); snap != nil {
		return snap.modelRules
	}
	return map[string]string{}
}

// ShouldThrottle 判定某用户当前是否应被限速（HIGH 且未豁免；master 关时恒 false）。零 I/O。
func (s *EnforcementService) ShouldThrottle(userID int64) bool {
	if !s.Active() || userID <= 0 {
		return false
	}
	snap := s.snap.Load()
	if snap == nil {
		return false
	}
	if _, ok := snap.allowlist[userID]; ok {
		return false // 豁免一票否决
	}
	_, high := snap.highUsers[userID]
	return high
}

// IsAllowlisted 该用户是否在豁免名单（供 admin 名单标注）。
func (s *EnforcementService) IsAllowlisted(userID int64) bool {
	if s == nil {
		return false
	}
	snap := s.snap.Load()
	if snap == nil {
		return false
	}
	_, ok := snap.allowlist[userID]
	return ok
}

// Throttled 热路径：命中则对独立分钟桶自增并判定是否超限。未命中/禁用返回 (false,0)。
func (s *EnforcementService) Throttled(ctx context.Context, userID int64) (bool, int) {
	if !s.ShouldThrottle(userID) {
		return false, 0
	}
	cctx, cancel := context.WithTimeout(ctx, enforcementIncrOpTimeout)
	defer cancel()
	count, err := s.store.IncrThrottleCounter(cctx, userID, s.cfg.CounterTTL)
	if err != nil {
		// fail-open：Redis 故障不拦截正常请求（观察阶段安全优先）。
		logger.L().With(zap.String("component", "service.enforcement")).
			Warn("throttle counter failed, fail-open", zap.Int64("user_id", userID), zap.Error(err))
		return false, 0
	}
	if count > int64(s.cfg.ThrottleRPM) {
		retryAfter := 60 - int(time.Now().Unix()%60)
		if retryAfter <= 0 {
			retryAfter = 1
		}
		return true, retryAfter
	}
	return false, 0
}

// AddAllowlist 将某用户加入豁免名单（管理员操作，审计）。允许在 master 关时预置。
func (s *EnforcementService) AddAllowlist(ctx context.Context, userID, adminID int64) error {
	if !s.configured() {
		return ErrEnforcementUnavailable
	}
	if userID <= 0 {
		return ErrEnforcementBadRequest
	}
	if err := s.store.AddAllowlist(ctx, userID); err != nil {
		return err
	}
	s.swap(s.currentHigh(), s.loadAllowlist(ctx), s.currentRules())
	s.Audit(adminID, "allowlist_add", "user", userID, nil)
	return nil
}

// RemoveAllowlist 将某用户移出豁免名单（管理员操作，审计）。允许在 master 关时预置。
func (s *EnforcementService) RemoveAllowlist(ctx context.Context, userID, adminID int64) error {
	if !s.configured() {
		return ErrEnforcementUnavailable
	}
	if userID <= 0 {
		return ErrEnforcementBadRequest
	}
	if err := s.store.RemoveAllowlist(ctx, userID); err != nil {
		return err
	}
	s.swap(s.currentHigh(), s.loadAllowlist(ctx), s.currentRules())
	s.Audit(adminID, "allowlist_remove", "user", userID, nil)
	return nil
}

// HasModelRules 当前是否配置了受限模型规则（供中间件决定是否读 body 取模型名）。
func (s *EnforcementService) HasModelRules() bool {
	if s == nil {
		return false
	}
	snap := s.snap.Load()
	return snap != nil && len(snap.modelRules) > 0
}

// ModelAction 返回某模型的处置动作（block|throttle）；无规则返回 ("",false)。
func (s *EnforcementService) ModelAction(model string) (string, bool) {
	if s == nil || model == "" {
		return "", false
	}
	snap := s.snap.Load()
	if snap == nil {
		return "", false
	}
	a, ok := snap.modelRules[model]
	return a, ok
}

// ThrottledModel 对某 (user,model) 的独立分钟桶自增并判定是否超限（受限模型 throttle 动作用）。
func (s *EnforcementService) ThrottledModel(ctx context.Context, userID int64, model string) (bool, int) {
	if s == nil || !s.configured() {
		return false, 0
	}
	cctx, cancel := context.WithTimeout(ctx, enforcementIncrOpTimeout)
	defer cancel()
	count, err := s.store.IncrModelThrottleCounter(cctx, userID, model, s.cfg.CounterTTL)
	if err != nil {
		logger.L().With(zap.String("component", "service.enforcement")).
			Warn("model throttle counter failed, fail-open", zap.Int64("user_id", userID), zap.String("model", model), zap.Error(err))
		return false, 0
	}
	if count > int64(s.cfg.ThrottleRPM) {
		retryAfter := 60 - int(time.Now().Unix()%60)
		if retryAfter <= 0 {
			retryAfter = 1
		}
		return true, retryAfter
	}
	return false, 0
}

// SetModelRule 设置一条受限模型规则（管理员操作，审计）。允许在 master 关时预置。
func (s *EnforcementService) SetModelRule(ctx context.Context, model, action string, adminID int64) error {
	if !s.configured() {
		return ErrEnforcementUnavailable
	}
	model = strings.TrimSpace(model)
	if model == "" || (action != EnforcementActionBlock && action != EnforcementActionThrottle) {
		return ErrEnforcementBadRequest
	}
	if err := s.store.SetModelRule(ctx, model, action); err != nil {
		return err
	}
	s.swap(s.currentHigh(), s.currentAllow(), s.loadModelRules(ctx))
	s.Audit(adminID, "model_rule_set", "model", 0, map[string]any{"model": model, "action": action})
	return nil
}

// DeleteModelRule 移除一条受限模型规则（管理员操作，审计）。
func (s *EnforcementService) DeleteModelRule(ctx context.Context, model string, adminID int64) error {
	if !s.configured() {
		return ErrEnforcementUnavailable
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return ErrEnforcementBadRequest
	}
	if err := s.store.DeleteModelRule(ctx, model); err != nil {
		return err
	}
	s.swap(s.currentHigh(), s.currentAllow(), s.loadModelRules(ctx))
	s.Audit(adminID, "model_rule_delete", "model", 0, map[string]any{"model": model})
	return nil
}

// ListModelRules 列出当前受限模型规则。
func (s *EnforcementService) ListModelRules() []EnforcementModelRule {
	out := []EnforcementModelRule{}
	if s == nil {
		return out
	}
	snap := s.snap.Load()
	if snap == nil {
		return out
	}
	for m, a := range snap.modelRules {
		out = append(out, EnforcementModelRule{Model: m, Action: a})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

func (s *EnforcementService) currentAllow() map[int64]struct{} {
	if snap := s.snap.Load(); snap != nil {
		return snap.allowlist
	}
	return map[int64]struct{}{}
}

func (s *EnforcementService) currentHigh() map[int64]struct{} {
	if snap := s.snap.Load(); snap != nil {
		return snap.highUsers
	}
	return map[int64]struct{}{}
}

// EnforcementHighUser 一条 HIGH 用户名单项（供 admin 名单查看）。
type EnforcementHighUser struct {
	UserID         int64   `json:"user_id"`
	RiskIndex      float64 `json:"risk_index"`
	Confidence     float64 `json:"confidence"`
	DataSufficient bool    `json:"data_sufficient"`
	AssessedAt     int64   `json:"assessed_at"`
	Throttled      bool    `json:"throttled"`   // master 开 + 命中限速阈值 + 未豁免
	Allowlisted    bool    `json:"allowlisted"` // 在豁免名单
}

// ListHighUsers 列出当前 HIGH 用户（读 user_risk_v2，master 关也可预览），并标注是否被限速/豁免。
func (s *EnforcementService) ListHighUsers(ctx context.Context) ([]EnforcementHighUser, error) {
	if !s.configured() {
		return nil, ErrEnforcementUnavailable
	}
	items, err := s.repo.ListCurrentAssessments(ctx, RiskV2ListFilter{Tier: RiskV2TierHigh}, RiskV2Pagination{Limit: enforcementHighUsersPageLimit})
	if err != nil {
		return nil, err
	}
	out := make([]EnforcementHighUser, 0, len(items))
	for _, it := range items {
		allow := s.IsAllowlisted(it.UserID)
		// 限速前提：master 开 + confidence≥地板 + data_sufficient + 未豁免。
		throttled := s.cfg.Enabled && it.Confidence >= s.cfg.ConfidenceMin && it.DataSufficient && !allow
		out = append(out, EnforcementHighUser{
			UserID:         it.UserID,
			RiskIndex:      it.RiskIndex,
			Confidence:     it.Confidence,
			DataSufficient: it.DataSufficient,
			AssessedAt:     it.AssessedAtUnix,
			Throttled:      throttled,
			Allowlisted:    allow,
		})
	}
	return out, nil
}

// Status 返回执行层运行态快照。
func (s *EnforcementService) Status() EnforcementStatus {
	st := EnforcementStatus{}
	if s == nil {
		return st
	}
	st.Enabled = s.cfg.Enabled
	st.ThrottleRPM = s.cfg.ThrottleRPM
	st.ConfidenceMin = s.cfg.ConfidenceMin
	if snap := s.snap.Load(); snap != nil {
		st.HighUserCount = len(snap.highUsers)
		st.AllowlistSize = len(snap.allowlist)
		st.ModelRuleCount = len(snap.modelRules)
		st.RefreshedAt = snap.refreshedAt
	}
	return st
}

// Audit 结构化审计日志（who/action/subject/when）。封禁/豁免等管理操作统一走这里。
func (s *EnforcementService) Audit(operatorID int64, action, subjectType string, subjectID int64, detail map[string]any) {
	fields := []zap.Field{
		zap.String("component", "audit.enforcement"),
		zap.Int64("operator_id", operatorID),
		zap.String("action", action),
		zap.String("subject_type", subjectType),
		zap.Int64("subject_id", subjectID),
	}
	for k, v := range detail {
		fields = append(fields, zap.Any(k, v))
	}
	logger.L().Info("enforcement action", fields...)
}
