package service

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// Risk Phase 0（仅观测/影子模式）：评分 worker。
//
// 每 scoring_interval 遍历活跃用户（risk:active 集合），读取 Redis 草图 + 免费
// 计数器（用户 RPM）+ usage_logs 24h 聚合，计算 7 特征并按 AND-gate 定级，
// 结果 upsert 到 user_risk 表 + 缓存到 risk:user:{uid}。绝不据此拦截任何请求。

// RiskScoringService 是评分 worker。
type RiskScoringService struct {
	sketch   RiskSketchCache
	riskRepo UserRiskRepository
	userRPM  UserRPMCache
	cfg      *config.Config

	stopCh chan struct{}
	wg     sync.WaitGroup
	now    func() time.Time
}

// NewRiskScoringService 构造评分 worker。任一必需依赖为 nil 时 Start 变为 no-op。
func NewRiskScoringService(
	sketch RiskSketchCache,
	riskRepo UserRiskRepository,
	userRPM UserRPMCache,
	cfg *config.Config,
) *RiskScoringService {
	return &RiskScoringService{
		sketch:   sketch,
		riskRepo: riskRepo,
		userRPM:  userRPM,
		cfg:      cfg,
		stopCh:   make(chan struct{}),
		now:      time.Now,
	}
}

func (s *RiskScoringService) interval() time.Duration {
	if s.cfg != nil && s.cfg.Risk.ScoringIntervalSeconds > 0 {
		return time.Duration(s.cfg.Risk.ScoringIntervalSeconds) * time.Second
	}
	return 5 * time.Minute
}

// Start 启动评分循环。依赖不全（nil sketch/repo）时静默不启动（不报错）。
func (s *RiskScoringService) Start() {
	if s == nil || s.sketch == nil || s.riskRepo == nil {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.run()
	}()
}

// Stop 停止评分循环（幂等）。
func (s *RiskScoringService) Stop() {
	if s == nil {
		return
	}
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
	s.wg.Wait()
}

func (s *RiskScoringService) run() {
	ticker := time.NewTicker(s.interval())
	defer ticker.Stop()
	// 启动即跑一轮（便于观察），随后按间隔。
	s.scoreOnce()
	for {
		select {
		case <-ticker.C:
			s.scoreOnce()
		case <-s.stopCh:
			return
		}
	}
}

// scoreOnce 对所有活跃用户跑一轮评分。
func (s *RiskScoringService) scoreOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	users, err := s.sketch.ActiveUsers(ctx)
	if err != nil {
		logger.LegacyPrintf("service.risk_scoring", "[risk] active users failed: %v", err)
		return
	}
	for _, uid := range users {
		select {
		case <-s.stopCh:
			return
		default:
		}
		s.scoreUser(ctx, uid)
	}
}

// scoreUser 采集单用户信号 → 计算特征/打分 → upsert + 缓存。
func (s *RiskScoringService) scoreUser(ctx context.Context, userID int64) {
	riskCfg := config.RiskConfig{}
	if s.cfg != nil {
		riskCfg = s.cfg.Risk
	}

	// 1. Redis 草图。
	snap, err := s.sketch.ReadSketch(ctx, userID)
	if err != nil {
		logger.LegacyPrintf("service.risk_scoring", "[risk] read sketch uid=%d failed: %v", userID, err)
	}

	// 2. usage_logs 24h 聚合（更可靠的持久口径 + 观测量：模型/温度/max_tokens/消费）。
	agg, aggErr := s.riskRepo.AggregateUsage(ctx, userID, 24*time.Hour)
	if aggErr != nil {
		logger.LegacyPrintf("service.risk_scoring", "[risk] aggregate uid=%d failed: %v", userID, aggErr)
	}

	// 3. 免费计数器：用户当前分钟 RPM（作为 RPM 峰值近似）。
	rpmPeak := 0
	if s.userRPM != nil {
		if v, rerr := s.userRPM.GetUserRPM(ctx, userID); rerr == nil {
			rpmPeak = v
		}
	}

	// 合并信号：请求量/去重比优先取 usage_logs（持久），其余取二者较大值以观测为先。
	in := RiskFeatureInputs{
		Requests24h:      maxInt(agg.Requests24h, snap.RequestCount),
		DistinctSim:      maxInt(agg.DistinctSimhash, snap.DistinctSim),
		TotalSim:         maxInt(agg.TotalSimhash, snap.TotalSim),
		SingleTurn:       maxInt(agg.SingleTurn, snap.SingleTurn),
		TotalTurns:       maxInt(agg.TotalTurns, snap.TotalTurns),
		InputTokens:      maxInt64(agg.InputTokens24h, snap.InputTokens),
		OutputTokens:     maxInt64(agg.OutputTokens24h, snap.OutputTokens),
		RPMPeak:          rpmPeak,
		ActiveMinutes:    0, // Phase 0：未采集精确活动分钟，留 0（cadence 主要靠 RPM）。
		TopModelCount:    maxInt(agg.TopModelCount, snap.TopModelCount),
		ModelVariety:     maxInt(agg.ModelVariety, snap.ModelVariety),
		ZeroTempShare:    agg.ZeroTempShare,
		MaxTokenPinShare: agg.MaxTokenPinShare,
		SubkeyCount:      0, // Phase 0：并行子键数暂不采集，留 0。
		AccountAgeDays:   -1,
		SpendDailyMicros: agg.SpendMicros,
		// 周消费用日消费的近似（Phase 0 观测，避免额外 7d 查询）。
		SpendWeeklyMicros: agg.SpendMicros,
	}

	// 读现有行以尊重 allowlisted / manual_tier。
	var allowlisted bool
	var manualTier *string
	if existing, gerr := s.riskRepo.GetByUserID(ctx, userID); gerr == nil && existing != nil {
		allowlisted = existing.Allowlisted
		manualTier = existing.ManualTier
	}

	result := ScoreRisk(in, riskCfg, allowlisted, manualTier)

	featuresJSON, _ := json.Marshal(result.Features)
	rec := &UserRiskRecord{
		UserID:      userID,
		Score:       result.Score,
		Tier:        result.Tier,
		FeaturesRaw: featuresJSON,
	}
	if err := s.riskRepo.Upsert(ctx, rec); err != nil {
		logger.LegacyPrintf("service.risk_scoring", "[risk] upsert uid=%d failed: %v", userID, err)
		return
	}

	// 缓存评分结果到 risk:user:{uid}。
	cachePayload, _ := json.Marshal(map[string]any{
		"score":    result.Score,
		"tier":     result.Tier,
		"features": result.Features,
		"updated":  s.now().Unix(),
	})
	if err := s.sketch.StoreScore(ctx, userID, cachePayload, RiskSketchTTL); err != nil {
		logger.LegacyPrintf("service.risk_scoring", "[risk] cache score uid=%d failed: %v", userID, err)
	}
}

func maxInt(a, b int) int {
	if a >= b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a >= b {
		return a
	}
	return b
}
