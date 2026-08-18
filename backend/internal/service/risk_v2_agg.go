package service

import (
	"context"
	"sync/atomic"
	"time"
)

// Model Extraction Risk V2 Shadow — 切片 1 / 1.1：Redis 多时间窗口聚合层（仅检测/观测，无评分、无执行）。
//
// 数据流：RiskFeatureEnvelope → RiskV2AggSink → RiskV2AggCache（Redis）→ RiskV2WindowMetrics。
// 主 API 请求绝不等待聚合；Redis 失败仅计指标 + 结构化错误，不影响主请求与 legacy。
//
// 实体维度（仅两种）：u=同一用户全部 API Key 汇总；k=单个 API Key。多 Key 协同由 u 下多个 k 推导。
// upstream_account_id / proxy_id / route_id 绝不作为客户身份关系。
//
// FingerprintKeyVersion = Observation Epoch：新旧版本 Key 命名空间隔离，不合并 exact/scaffold，
// 当前版本重新积累 data sufficiency，旧版本按 TTL 自然过期。

// RiskV2AggSchemaVersion 是聚合 Key 的 schema 版本（进 Redis Key 命名空间）。
// 其余桶 TTL / 窗口大小 / 集合上限等属于 Redis 实现细节，定义在 repository 聚合实现中。
const RiskV2AggSchemaVersion = "s1"

// RiskV2FeatureAvailability 标记各族特征是否真实可用（供切片 2 决定 unavailable / 降 confidence /
// 禁止产生对应 reason code）。凡 envelope 未真实提供的特征一律 false，绝不当作 0。
type RiskV2FeatureAvailability struct {
	Requests      bool
	Fingerprint   bool // exact 指纹可用且未整体 overflow/incomplete
	Cache         bool // cache 适用覆盖率足够
	ActiveMinutes bool
	NearDuplicate bool // 本阶段恒 false（未实现 LSH）

	ToolUse            bool
	StructuredOutput   bool
	ModelConcentration bool

	// 以下模式当前 envelope 未真实提供 → 恒 false（切片 2 不得据此产 reason code）。
	CandidateGeneration bool
	GradingOrRanking    bool
	RewriteOrVariation  bool
	CodeGeneration      bool
}

// RiskV2Window 是单个时间窗口内某实体的聚合读数（契约结构；比率由方法派生，避免重复存储）。
type RiskV2Window struct {
	WindowLabel string

	// 基础请求
	RequestCount        int64
	SuccessCount        int64
	TerminalErrorCount  int64
	CancelledCount      int64
	UsageAvailableCount int64
	UniqueAPIKeyCount   int // 仅 user 实体有意义
	ActiveMinutes       int // 由小时桶 60-bit bitmap 精确 popcount

	// 速率 / 并发
	RequestsPerMinute float64
	PeakRPM           int  // 5m/1h：每分钟计数取 max（读时计算，无存储 max→无竞态）
	PeakRPMAvailable  bool // 24h 无分钟粒度 → false
	// 加速度只用「两个已完整结束的分钟」（绝不用仍在进行中的当前分钟）。
	LatestCompletedMinuteRequestCount   int64
	PreviousCompletedMinuteRequestCount int64
	RequestAccelerationAvailable        bool
	MaxObservedConcurrency              int  // 无真实并发信号 → 0
	MaxObservedConcurrencyAvailable     bool // 当前 envelope 无并发信号 → false

	// token 与输出
	InputTokens      int64
	OutputTokens     int64
	ShortOutputCount int64
	LongOutputCount  int64

	// exact 指纹基数
	ExactAvailableCount           int64
	DistinctExactEstimate         int
	ExactOverflow                 bool
	ExactIncomplete               bool
	DistinctExactParamSigEstimate int // repeated sampling：同 exact 不同采样参数
	ExactParamSigOverflow         bool

	// scaffold 分离：完整 vs 采样
	FullScaffoldRequestCount        int64
	DistinctFullScaffoldEstimate    int
	FullScaffoldOverflow            bool
	FullScaffoldIncomplete          bool
	SampledScaffoldRequestCount     int64
	DistinctSampledScaffoldEstimate int
	SampledScaffoldOverflow         bool
	SampledScaffoldIncomplete       bool

	NearDuplicateAvailable bool // 本阶段 = false
	TruncatedInputCount    int64

	// cache（严格 presence + 正确分母）
	CacheApplicableCount     int64
	CacheAvailableCount      int64
	CacheObservedInputTokens int64 // 仅 applicable&available 请求的普通 input tokens（cache 分母用）
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64

	// 请求形态
	ToolUseRequestCount   int64
	StructuredOutputCount int64
	MultiTurnCount        int64

	// 模型集中度（canonical + bounded other）
	DistinctModelCount          int
	TopModelRequestCount        int64
	ModelConcentrationAvailable bool

	Available RiskV2FeatureAvailability
}

// —— 派生比率（只读基础字段；缺分母/incomplete/不适用 → (0,false)=unavailable）——

func ratio(num, den int64) (float64, bool) {
	if den <= 0 {
		return 0, false
	}
	return float64(num) / float64(den), true
}

func (w RiskV2Window) OutputInputRatio() (float64, bool) { return ratio(w.OutputTokens, w.InputTokens) }

func (w RiskV2Window) ExactDuplicateCount() (int64, bool) {
	if w.ExactIncomplete || w.ExactAvailableCount == 0 {
		return 0, false
	}
	dup := w.ExactAvailableCount - int64(w.DistinctExactEstimate)
	if dup < 0 {
		dup = 0
	}
	return dup, true
}
func (w RiskV2Window) ExactDuplicateRatio() (float64, bool) {
	dup, ok := w.ExactDuplicateCount()
	if !ok {
		return 0, false
	}
	return ratio(dup, w.ExactAvailableCount)
}

// FullScaffoldReuseRatio 仅用完整 scaffold（sampled 不参与）。
func (w RiskV2Window) FullScaffoldReuseRatio() (float64, bool) {
	if w.FullScaffoldIncomplete || w.FullScaffoldRequestCount == 0 {
		return 0, false
	}
	reuse := w.FullScaffoldRequestCount - int64(w.DistinctFullScaffoldEstimate)
	if reuse < 0 {
		reuse = 0
	}
	return ratio(reuse, w.FullScaffoldRequestCount)
}

// ScaffoldExpansionEstimate = distinct_full_exact / max(distinct_full_scaffold,1)。
// 只用完整 exact + 完整 scaffold；任一 incomplete 或无完整 scaffold → unavailable。
func (w RiskV2Window) ScaffoldExpansionEstimate() (float64, bool) {
	if w.ExactIncomplete || w.FullScaffoldIncomplete || w.DistinctFullScaffoldEstimate == 0 {
		return 0, false
	}
	return float64(w.DistinctExactEstimate) / float64(w.DistinctFullScaffoldEstimate), true
}

// SampledScaffoldRatio 采样 scaffold 占全部 scaffold 请求的比例（越高 confidence 越低）。
func (w RiskV2Window) SampledScaffoldRatio() (float64, bool) {
	total := w.FullScaffoldRequestCount + w.SampledScaffoldRequestCount
	return ratio(w.SampledScaffoldRequestCount, total)
}

func (w RiskV2Window) cacheTotalInput() int64 {
	return w.CacheObservedInputTokens + w.CacheCreationInputTokens + w.CacheReadInputTokens
}

// CacheReadRatio = read / (observed + creation + read)。无 cache_total_input → unavailable。
func (w RiskV2Window) CacheReadRatio() (float64, bool) {
	return ratio(w.CacheReadInputTokens, w.cacheTotalInput())
}

// NovelInputRatio = (observed + creation) / (observed + creation + read)。
func (w RiskV2Window) NovelInputRatio() (float64, bool) {
	return ratio(w.CacheObservedInputTokens+w.CacheCreationInputTokens, w.cacheTotalInput())
}

func (w RiskV2Window) CacheAvailabilityRatio() (float64, bool) {
	return ratio(w.CacheAvailableCount, w.CacheApplicableCount)
}

// BurstRatio = PeakRPM / 平均 rpm。无速率数据 → unavailable。
func (w RiskV2Window) BurstRatio() (float64, bool) {
	if !w.PeakRPMAvailable || w.RequestsPerMinute <= 0 {
		return 0, false
	}
	return float64(w.PeakRPM) / w.RequestsPerMinute, true
}

// RequestAccelerationAbsolute = 最近完整分钟 - 上一完整分钟（仅两个已结束分钟）。
func (w RiskV2Window) RequestAccelerationAbsolute() (int64, bool) {
	if !w.RequestAccelerationAvailable {
		return 0, false
	}
	return w.LatestCompletedMinuteRequestCount - w.PreviousCompletedMinuteRequestCount, true
}

// RequestAccelerationRatio = 最近完整分钟 / 上一完整分钟。上一完整分钟为 0 → unavailable。
func (w RiskV2Window) RequestAccelerationRatio() (float64, bool) {
	if !w.RequestAccelerationAvailable || w.PreviousCompletedMinuteRequestCount == 0 {
		return 0, false
	}
	return float64(w.LatestCompletedMinuteRequestCount) / float64(w.PreviousCompletedMinuteRequestCount), true
}

// RiskV2EntityWindows 是某实体（user 或某 api_key）的三窗口读数。
type RiskV2EntityWindows struct {
	W5m  RiskV2Window
	W1h  RiskV2Window
	W24h RiskV2Window
}

// RiskV2MultiKeyRollup 是「同一用户多 API Key」汇总（reader 内部完成，只输出数值与 availability，
// 绝不向评分暴露底层 HMAC/Set 成员）。所有子信号单独均为弱证据，切片 2 须组合≥2 类才成立。
type RiskV2MultiKeyRollup struct {
	MultiKeyAvailable                      bool
	ActiveAPIKeyCount24h                   int
	APIKeyOverflow                         bool
	PerAPIKeyIncomplete                    bool
	SynchronizedMultiKeyMinutes1h          int
	CrossKeyFullScaffoldOverlapEstimate1h  int
	CrossKeyFullScaffoldOverlapAvailable1h bool
	KeyHandoffCount1h                      int
	MultiKeyIncomplete                     bool
}

// RiskV2WindowMetrics 是切片 1.1 的纯数据契约。后续评分只读它,不得直接读底层 Redis Key。
type RiskV2WindowMetrics struct {
	UserID                int64
	SchemaVersion         string
	FingerprintKeyVersion string // = Observation Epoch
	AssessedAtUnix        int64

	Degraded          bool // 读取期间发生 Redis 错误 → 数据不完整
	Incomplete        bool // 任一窗口/特征达上限或截断 → 不完整
	IncompleteReasons []string

	User      RiskV2EntityWindows
	PerAPIKey map[int64]RiskV2EntityWindows
	MultiKey  RiskV2MultiKeyRollup
}

// RiskV2ScoringSnapshot 是「评分专用」读取结果（切片 3.5）。
//
// 与 RiskV2WindowMetrics 的关键差异：**没有 PerAPIKey map**。评分 Worker 只需要用户级三窗口 +
// 紧凑多 Key 汇总（MultiKey），绝不需要逐 Key 展开。因此本类型在类型层面就无法承载 per-key 明细，
// 从结构上杜绝评分路径退化成 O(keys) 的 Admin Detail 读取。ScoreRiskV2 只依赖 User + MultiKey +
// Degraded/Incomplete，本 snapshot 恰好只给这些。
type RiskV2ScoringSnapshot struct {
	UserID                int64
	SchemaVersion         string
	FingerprintKeyVersion string // = Observation Epoch
	AssessedAtUnix        int64

	Degraded          bool
	Incomplete        bool
	IncompleteReasons []string

	User     RiskV2EntityWindows
	MultiKey RiskV2MultiKeyRollup
	// 注意：故意不含 PerAPIKey。评分路径永远拿不到逐 Key 窗口。
}

// ToWindowMetrics 把评分快照适配为 ScoreRiskV2 的输入契约（PerAPIKey 恒 nil；评分不看 per-key）。
// assessedAtOverride>0 时覆盖 AssessedAtUnix（Worker 用 cycle_end 作为评估版本，而非随机 now）。
func (s RiskV2ScoringSnapshot) ToWindowMetrics(assessedAtOverride int64) RiskV2WindowMetrics {
	at := s.AssessedAtUnix
	if assessedAtOverride > 0 {
		at = assessedAtOverride
	}
	return RiskV2WindowMetrics{
		UserID:                s.UserID,
		SchemaVersion:         s.SchemaVersion,
		FingerprintKeyVersion: s.FingerprintKeyVersion,
		AssessedAtUnix:        at,
		Degraded:              s.Degraded,
		Incomplete:            s.Incomplete,
		IncompleteReasons:     s.IncompleteReasons,
		User:                  s.User,
		PerAPIKey:             nil, // 评分路径永远无 per-key
		MultiKey:              s.MultiKey,
	}
}

// RiskV2ScoringBatchItem 是批量评分读取的单用户结果：成功给 Snapshot，失败给 Err（不拖累整批）。
type RiskV2ScoringBatchItem struct {
	UserID   int64
	Snapshot RiskV2ScoringSnapshot
	Err      error
}

// RiskV2ActiveUserPage 是周期活跃用户游标分页的一页结果。
// NextCursor 为最后一个成员（lex 游标）；为空表示遍历结束。
type RiskV2ActiveUserPage struct {
	UserIDs    []int64
	NextCursor string
}

// RiskV2AggCache 是 V2 多窗口聚合的**写入面**（Redis 实现，独立命名空间 riskv2:*）。
// 读取面已按用途拆分为 RiskV2ScoringReader（评分 Worker 用）与 RiskV2AdminDetailReader（管理端单用户详情用）。
type RiskV2AggCache interface {
	Aggregate(ctx context.Context, env RiskFeatureEnvelope) error
}

// RiskV2ScoringReader 是**评分专用只读面**（切片 3.5）。
//
// 契约保证：
//   - 返回的 RiskV2ScoringSnapshot 结构上无 PerAPIKey，绝不逐 Key 展开窗口；
//   - 读取成本不随用户 API Key 数量线性增长（用户级窗口 + 紧凑 cross-key 汇总）；
//   - 单用户读取失败只影响该用户（批量里体现为该项 Err），不拖累整批；
//   - 绝不暴露 HMAC / SimHash / Set 成员 / 原文。
//
// 未来的 Scoring Worker 只允许依赖本接口，**不得**依赖 RiskV2AdminDetailReader。
type RiskV2ScoringReader interface {
	// ReadForScoring 读取单用户评分快照（用户级三窗口 + 紧凑多 Key 汇总，无 per-key）。
	ReadForScoring(ctx context.Context, userID int64) (RiskV2ScoringSnapshot, error)
	// ReadForScoringBatch 批量读取；单用户失败以该项 Err 体现，不使整批失败。
	// userIDs 超过硬上限会被截断（见实现常量），调用方应分页喂入。
	ReadForScoringBatch(ctx context.Context, userIDs []int64) ([]RiskV2ScoringBatchItem, error)
	// ListActiveUserIDsForCycle 用「周期活跃用户 ZSET（active:c:<cycleEnd>）」+ 稳定 lex 游标分页，
	// 列出归属该 cycleEnd 的活跃用户。cursor 从 "" 起，返回 NextCursor=="" 表示结束；limit 受硬上限约束。
	// 周期结束并过 grace 后该集合视为只读快照，分页稳定不漏不重。
	ListActiveUserIDsForCycle(ctx context.Context, cycleEnd int64, cursor string, limit int) (RiskV2ActiveUserPage, error)
}

// RiskV2AdminDetailReader 是**管理端单用户详情只读面**：保留完整读取（含有界 PerAPIKey 展开）。
// 仅供管理端单用户查看；评分 Worker 的 ProviderSet 绝不注入本接口（防止评分退化成 O(keys) 读取）。
type RiskV2AdminDetailReader interface {
	ReadForAdminDetail(ctx context.Context, userID int64) (RiskV2WindowMetrics, error)
}

// RiskV2AggSink 是 RiskV2Sink 实现：把观测写入 Redis 聚合。cache 为 nil → no-op（fail-open）。
type RiskV2AggSink struct {
	cache         RiskV2AggCache
	timeout       time.Duration
	consumed      int64
	aggErrors     int64
	skippedNoUser int64
}

func NewRiskV2AggSink(cache RiskV2AggCache, timeout time.Duration) *RiskV2AggSink {
	if timeout <= 0 {
		timeout = 500 * time.Millisecond
	}
	return &RiskV2AggSink{cache: cache, timeout: timeout}
}

// Consume 在 dispatcher 有界 worker 内调用（不阻塞主请求）。Redis 错误仅计数（DEGRADED 信号），不重试（避免重复计数）。
func (s *RiskV2AggSink) Consume(env RiskFeatureEnvelope) {
	if s == nil || s.cache == nil {
		return
	}
	if env.UserID <= 0 || env.ServerRequestID == "" {
		atomic.AddInt64(&s.skippedNoUser, 1)
		return
	}
	atomic.AddInt64(&s.consumed, 1)
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	if err := s.cache.Aggregate(ctx, env); err != nil {
		atomic.AddInt64(&s.aggErrors, 1)
	}
}

type RiskV2AggSinkStats struct {
	Consumed         int64
	AggregationError int64
	SkippedNoUser    int64
}

func (s *RiskV2AggSink) Stats() RiskV2AggSinkStats {
	if s == nil {
		return RiskV2AggSinkStats{}
	}
	return RiskV2AggSinkStats{
		Consumed:         atomic.LoadInt64(&s.consumed),
		AggregationError: atomic.LoadInt64(&s.aggErrors),
		SkippedNoUser:    atomic.LoadInt64(&s.skippedNoUser),
	}
}
