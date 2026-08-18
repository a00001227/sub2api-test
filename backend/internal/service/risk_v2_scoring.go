package service

import (
	"errors"
	"math"
	"sort"
)

// Model Extraction Risk V2 Shadow — 切片 2：纯函数 Shadow 评分（RiskV2WindowMetrics → RiskV2Assessment）。
//
// 纯函数：无 Redis / 无数据库 / 无网络 / 无全局可变状态 / 不调用 time.Now()（时间取自 metrics）。
// 相同输入+配置 → 相同结果；并发安全；绝不产生 NaN/Inf。EffectiveAction 恒为 NONE（仅检测，不执行）。
// risk_index 是风险指数（0–100），不是概率。不可用分数用 Available=false 表达，绝不用 0 冒充有效低分。

const (
	RiskV2FeatureVersion = "score-v2"
	RiskV2PolicyVersion  = "shadow-uncalibrated-1"

	// Tier
	RiskV2TierInsufficient = "INSUFFICIENT_DATA"
	RiskV2TierWatch        = "WATCH"
	RiskV2TierMedium       = "MEDIUM"
	RiskV2TierHigh         = "HIGH"

	RiskV2ActionNone = "NONE"

	// Evidence families
	EFTemplateEnumeration  = "TEMPLATE_ENUMERATION"
	EFTemporalAutomation   = "TEMPORAL_AUTOMATION"
	EFOutputCollection     = "OUTPUT_COLLECTION"
	EFRepeatedSampling     = "REPEATED_SAMPLING"
	EFMultiKeyCoordination = "MULTI_KEY_COORDINATION"
	EFCacheContinuity      = "CACHE_SESSION_CONTINUITY"
	EFModelConcentration   = "MODEL_CONCENTRATION"

	// Evidence groups（相关性分组，避免相关特征重复计分；HIGH 需 ≥2 个不同 group）
	EGPromptPattern     = "PROMPT_PATTERN"     // TEMPLATE_ENUMERATION + REPEATED_SAMPLING
	EGTemporalIntensity = "TEMPORAL_INTENSITY" // TEMPORAL_AUTOMATION
	EGResponseHarvest   = "RESPONSE_HARVEST"   // OUTPUT_COLLECTION
	EGCoordination      = "COORDINATION"       // MULTI_KEY_COORDINATION
	EGModel             = "MODEL"              // MODEL_CONCENTRATION（解释性，不计入 HIGH group）
)

// RiskV2IngestionHealth 是纯输入：采集/写入健康度（Reader 的 Degraded 只反映读，不代表写期是否丢观测）。
type RiskV2IngestionHealth struct {
	HealthAvailable               bool // false=健康未知 → 降 confidence + 阻止 HIGH
	AggregationHealthy            bool
	ObservationDropRatioAvailable bool
	ObservationDropRatio          float64 // [0,1]
	AggregationErrorObserved      bool
	DispatcherDegraded            bool

	// 切片 4.1 §八：覆盖率。已知实例缺报 / 覆盖不完整 / Registry 读失败 → HealthAvailable=false（不假设健康）。
	ExpectedInstanceCount  int
	ReportingInstanceCount int
	MissingInstanceCount   int
	HealthCoverageRatio    float64 // reporting/expected，[0,1]
	CoverageAvailable      bool    // Registry 可读且 expected>0
}

// RiskV2ScoringConfig 全部阈值（未校准 Shadow 默认，见 DefaultRiskV2ScoringConfig）。
type RiskV2ScoringConfig struct {
	Calibration string // 恒为 UNCALIBRATED_SHADOW_DEFAULTS（除非显式覆盖）

	// data sufficiency（24h 全局）
	MinRequests24h      int64
	MinActiveMinutes24h int

	// 各 Evidence Family 自身 1h 数据门槛（不足 → Family Available=false，不当 0）
	MinPromptRequests1h    int64
	MinTemporalRequests1h  int64
	MinUsageObservations1h int64
	MinMultiKeyRequests1h  int64
	MinExactAvailable1h    int64

	// template
	ExactDupRatioHigh     float64
	FullScaffoldReuseHigh float64
	ScaffoldExpansionHigh float64

	// temporal
	PeakRPMHigh             float64
	BurstRatioHigh          float64
	AccelRatioHigh          float64
	ActiveMinuteDensityHigh float64

	// output
	OutputPerMinuteHigh  float64
	OutputInputRatioHigh float64
	LongOutputRatioHigh  float64
	ShortOutputFreqHigh  float64

	// repeated sampling
	ParamVariationHigh float64
	MinDistinctExact   int

	// multi-key sub-signals
	SyncMinutesHigh     int
	CrossKeyOverlapHigh int
	KeyHandoffHigh      int

	// plane weights (RiskIndex = 可用平面加权平均，Exposure 不参与)
	WAutomation float64
	WHarvest    float64
	WCampaign   float64

	// tier gates
	MediumIndex      float64
	HighIndex        float64
	MediumConfidence float64
	HighConfidence   float64

	// availability / confidence
	UsageCoverageMin        float64 // output/exposure 可用所需的 usage 覆盖率
	SampledRatioPenaltyAt   float64 // sampled scaffold 比例超此值开始降 confidence
	TruncatedRatioPenaltyAt float64
	SevereDropRatio         float64 // observation drop 超此视为严重降级
	FamilyHighStrength      float64 // 家族 meetsHigh 所需的归一化强度（0–100）
}

// DefaultRiskV2ScoringConfig 返回保守的未校准 Shadow 默认阈值。
// 明确未校准；非 Anthropic 封号阈值；非蒸馏概率；后续 P1.5 可回放校准。
func DefaultRiskV2ScoringConfig() RiskV2ScoringConfig {
	return RiskV2ScoringConfig{
		Calibration:         "UNCALIBRATED_SHADOW_DEFAULTS",
		MinRequests24h:      200,
		MinActiveMinutes24h: 10,

		MinPromptRequests1h:    20,
		MinTemporalRequests1h:  20,
		MinUsageObservations1h: 20,
		MinMultiKeyRequests1h:  20,
		MinExactAvailable1h:    10,

		ExactDupRatioHigh:     0.60,
		FullScaffoldReuseHigh: 0.60,
		ScaffoldExpansionHigh: 5.0,

		PeakRPMHigh:             60,
		BurstRatioHigh:          5.0,
		AccelRatioHigh:          3.0,
		ActiveMinuteDensityHigh: 0.80,

		OutputPerMinuteHigh:  5000,
		OutputInputRatioHigh: 8.0,
		LongOutputRatioHigh:  0.60,
		ShortOutputFreqHigh:  0.70,

		ParamVariationHigh: 2.0,
		MinDistinctExact:   5,

		SyncMinutesHigh:     5,
		CrossKeyOverlapHigh: 1,
		KeyHandoffHigh:      3,

		WAutomation: 1.0,
		WHarvest:    1.0,
		WCampaign:   1.0,

		MediumIndex:      40,
		HighIndex:        70,
		MediumConfidence: 0.30,
		HighConfidence:   0.60,

		UsageCoverageMin:        0.50,
		SampledRatioPenaltyAt:   0.30,
		TruncatedRatioPenaltyAt: 0.30,
		SevereDropRatio:         0.20,
		FamilyHighStrength:      70,
	}
}

// Validate 校验配置合法性（全 finite；比例∈[0,1]；分数∈[0,100]；Medium<High 等）。非法 → error。
func (c RiskV2ScoringConfig) Validate() error {
	finite := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
	for _, r := range []float64{c.ExactDupRatioHigh, c.FullScaffoldReuseHigh, c.ActiveMinuteDensityHigh,
		c.LongOutputRatioHigh, c.ShortOutputFreqHigh, c.UsageCoverageMin, c.SampledRatioPenaltyAt,
		c.TruncatedRatioPenaltyAt, c.SevereDropRatio} {
		if !finite(r) || r < 0 || r > 1 {
			return errors.New("risk.v2.scoring: ratio threshold out of [0,1] or non-finite")
		}
	}
	for _, s := range []float64{c.MediumIndex, c.HighIndex, c.FamilyHighStrength} {
		if !finite(s) || s < 0 || s > 100 {
			return errors.New("risk.v2.scoring: score threshold out of [0,100] or non-finite")
		}
	}
	for _, v := range []float64{c.ScaffoldExpansionHigh, c.PeakRPMHigh, c.BurstRatioHigh, c.AccelRatioHigh,
		c.OutputPerMinuteHigh, c.OutputInputRatioHigh, c.ParamVariationHigh, c.WAutomation, c.WHarvest,
		c.WCampaign, c.MediumConfidence, c.HighConfidence} {
		if !finite(v) {
			return errors.New("risk.v2.scoring: non-finite config value")
		}
	}
	if c.MediumConfidence < 0 || c.MediumConfidence > 1 || c.HighConfidence < 0 || c.HighConfidence > 1 {
		return errors.New("risk.v2.scoring: confidence gate out of [0,1]")
	}
	if !(c.MediumIndex < c.HighIndex) {
		return errors.New("risk.v2.scoring: MediumIndex must be < HighIndex")
	}
	if !(c.MediumConfidence <= c.HighConfidence) {
		return errors.New("risk.v2.scoring: MediumConfidence must be <= HighConfidence")
	}
	for _, n := range []int64{c.MinRequests24h, c.MinPromptRequests1h, c.MinTemporalRequests1h,
		c.MinUsageObservations1h, c.MinMultiKeyRequests1h, c.MinExactAvailable1h} {
		if n < 0 {
			return errors.New("risk.v2.scoring: negative request threshold")
		}
	}
	if c.MinActiveMinutes24h < 0 || c.MinDistinctExact < 0 || c.SyncMinutesHigh < 0 ||
		c.CrossKeyOverlapHigh < 0 || c.KeyHandoffHigh < 0 {
		return errors.New("risk.v2.scoring: negative threshold")
	}
	if RiskV2FeatureVersion == "" || RiskV2PolicyVersion == "" {
		return errors.New("risk.v2.scoring: empty feature/policy version")
	}
	return nil
}

// configInvalidAssessment 返回安全的 CONFIG_INVALID 结果（绝不 HIGH；EffectiveAction=NONE）。
func configInvalidAssessment(metrics RiskV2WindowMetrics) RiskV2Assessment {
	return RiskV2Assessment{
		RiskTier:              RiskV2TierInsufficient,
		DataSufficient:        false,
		FeatureVersion:        RiskV2FeatureVersion,
		PolicyVersion:         RiskV2PolicyVersion,
		FingerprintKeyVersion: metrics.FingerprintKeyVersion,
		AssessedAtUnix:        metrics.AssessedAtUnix,
		EffectiveAction:       RiskV2ActionNone,
		ReasonCodes:           []RiskV2ReasonCode{{Code: "RISK_V2_CONFIG_INVALID", Window: "n/a"}},
	}
}

// RiskV2EvidenceFamily 单个证据家族评估结果。
type RiskV2EvidenceFamily struct {
	Family    string
	Group     string
	Available bool
	Strength  float64 // 0–100
	MeetsHigh bool
	Window    string
}

// RiskV2EvidenceGroup 相关性分组结果。
type RiskV2EvidenceGroup struct {
	Group    string
	MetHigh  bool
	Strength float64 // 0–100（组内取最大，避免相关特征重复计分）
}

// RiskV2ReasonCode 一条可解释原因（绝不含 prompt/response/HMAC/SimHash/api key/请求 id/secret）。
type RiskV2ReasonCode struct {
	Code                   string
	Window                 string
	ObservedValue          float64
	Threshold              float64
	EvidenceFamily         string
	EvidenceGroup          string
	ConfidenceContribution float64
}

// RiskV2PlaneScore 四平面之一。
type RiskV2PlaneScore struct {
	Score     float64 // 0–100
	Available bool
}

// RiskV2Assessment 是评分输出。
type RiskV2Assessment struct {
	RiskIndex      float64 // 0–100（风险指数，非概率）
	RiskTier       string
	Confidence     float64 // 0–1（证据完整度，非风险高低）
	DataSufficient bool

	Automation RiskV2PlaneScore
	Harvest    RiskV2PlaneScore
	Campaign   RiskV2PlaneScore
	Exposure   RiskV2PlaneScore

	EvidenceFamilies    []RiskV2EvidenceFamily
	EvidenceGroups      []RiskV2EvidenceGroup
	ReasonCodes         []RiskV2ReasonCode
	FeatureAvailability RiskV2FeatureAvailability

	// 数据质量标志（源自 metrics/health，随评分一并记录，供持久化与解释）。
	Degraded          bool
	Incomplete        bool
	IncompleteReasons []string
	HealthAvailable   bool

	FeatureVersion        string
	PolicyVersion         string
	FingerprintKeyVersion string
	AssessedAtUnix        int64
	EffectiveAction       string // 恒 NONE
}

// —— helpers（纯，无 NaN/Inf）——

func clamp0100(v float64) float64 {
	if v != v { // NaN
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
func riskV2Clamp01(v float64) float64 {
	if v != v {
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// ramp 把 value 相对 high 阈值线性映射到 0–100（high<=0 → 0，避免除零/Inf）。
func ramp(value, high float64) float64 {
	if high <= 0 {
		return 0
	}
	return clamp0100(value / high * 100)
}

// ScoreRiskV2 纯函数评分入口。非法配置 → 安全 CONFIG_INVALID 结果（绝不 HIGH）。
func ScoreRiskV2(metrics RiskV2WindowMetrics, health RiskV2IngestionHealth, cfg RiskV2ScoringConfig) RiskV2Assessment {
	if err := cfg.Validate(); err != nil {
		return configInvalidAssessment(metrics)
	}
	w1h := metrics.User.W1h
	w24 := metrics.User.W24h

	a := RiskV2Assessment{
		FeatureVersion:        RiskV2FeatureVersion,
		PolicyVersion:         RiskV2PolicyVersion,
		FingerprintKeyVersion: metrics.FingerprintKeyVersion,
		AssessedAtUnix:        metrics.AssessedAtUnix,
		EffectiveAction:       RiskV2ActionNone,
		FeatureAvailability:   w1h.Available,
	}
	var reasons []RiskV2ReasonCode
	addReason := func(r RiskV2ReasonCode) { reasons = append(reasons, r) }

	// ── 证据家族 ──
	template := scoreTemplate(w1h, cfg, &reasons)
	temporal := scoreTemporal(w1h, cfg, &reasons)
	output := scoreOutput(w1h, cfg, &reasons)
	repeated := scoreRepeatedSampling(w1h, cfg, &reasons)
	campaign := scoreMultiKey(metrics.MultiKey, w1h.RequestCount, cfg, &reasons)
	cache := RiskV2EvidenceFamily{Family: EFCacheContinuity, Group: "", Available: false} // 本切片不参与
	model := scoreModelConcentration(w1h)

	a.EvidenceFamilies = []RiskV2EvidenceFamily{template, temporal, output, repeated, campaign, cache, model}

	// ── 相关性分组（组内取最大强度；HIGH 需 ≥2 个不同组达标）──
	groups := computeGroups(template, repeated, temporal, output, campaign)
	a.EvidenceGroups = groups
	groupsMet := 0
	for _, g := range groups {
		if g.MetHigh {
			groupsMet++
		}
	}

	// ── 四平面 ──
	a.Automation = RiskV2PlaneScore{Score: temporal.Strength, Available: temporal.Available}
	// Harvest：prompt-pattern（template/repeated 相关 → 取 max，相关性上限）+ output，组合。
	promptPattern := maxFloat(strengthOrZero(template), strengthOrZero(repeated))
	ppAvail := template.Available || repeated.Available
	harvestAvail := ppAvail || output.Available
	harvestScore := combineAvailable([]weighted{
		{promptPattern, 0.6, ppAvail},
		{strengthOrZero(output), 0.4, output.Available},
	})
	a.Harvest = RiskV2PlaneScore{Score: harvestScore, Available: harvestAvail}
	a.Campaign = RiskV2PlaneScore{Score: campaign.Strength, Available: campaign.Available}
	a.Exposure = scoreExposure(w24, cfg)

	// ── RiskIndex 由「证据组」强度前二名决定（Exposure 解耦不参与）。
	// 前二名 0.6/0.4 加权：单个强组最多 ~60（<HighIndex）→ 天然阻止单信号 HIGH；
	// 两个不同强组才达 HIGH 区间。避免「可用但低」平面稀释真实强证据。
	gs := make([]float64, 0, len(groups))
	for _, g := range groups {
		gs = append(gs, g.Strength)
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(gs)))
	var top1, top2 float64
	if len(gs) >= 1 {
		top1 = gs[0]
	}
	if len(gs) >= 2 {
		top2 = gs[1]
	}
	a.RiskIndex = clamp0100(0.6*top1 + 0.4*top2)
	_ = cfg.WAutomation // 平面权重保留于 config 供后续校准

	// ── data sufficiency ──
	a.DataSufficient = w24.RequestCount >= cfg.MinRequests24h && w24.ActiveMinutes >= cfg.MinActiveMinutes24h

	// ── confidence（证据完整度，非风险高低）──
	a.Confidence, _ = computeConfidence(metrics, health, cfg, &reasons)

	// ── 严重降级判定（健康未知 = 未知 → 视为严重，阻止 HIGH）──
	healthUnknown := !health.HealthAvailable
	severe := metrics.Degraded ||
		healthUnknown ||
		(health.HealthAvailable && !health.AggregationHealthy) ||
		health.DispatcherDegraded ||
		(health.ObservationDropRatioAvailable && health.ObservationDropRatio > cfg.SevereDropRatio)
	if severe {
		addReason(RiskV2ReasonCode{Code: "RISK_V2_DEGRADED", Window: "n/a", EvidenceGroup: "", ConfidenceContribution: -0.3})
	}
	if !a.DataSufficient {
		addReason(RiskV2ReasonCode{Code: "DATA_INSUFFICIENT", Window: "24h", ObservedValue: float64(w24.RequestCount), Threshold: float64(cfg.MinRequests24h)})
	}

	// 数据质量标志（供持久化/解释）。
	a.Degraded = metrics.Degraded
	a.Incomplete = metrics.Incomplete
	a.IncompleteReasons = append([]string(nil), metrics.IncompleteReasons...)
	a.HealthAvailable = health.HealthAvailable

	// ── Tier ──
	a.RiskTier = decideTier(a, groupsMet, severe, metrics, cfg)

	// ── 固定序列化顺序：ReasonCodes 按 Code + Window + EvidenceGroup 稳定排序 ──
	sortReasonCodes(reasons)
	a.ReasonCodes = reasons
	return a
}

// sortReasonCodes 稳定排序（Code → Window → EvidenceGroup），保证相同输入逐字节相同 JSON。
func sortReasonCodes(rs []RiskV2ReasonCode) {
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].Code != rs[j].Code {
			return rs[i].Code < rs[j].Code
		}
		if rs[i].Window != rs[j].Window {
			return rs[i].Window < rs[j].Window
		}
		return rs[i].EvidenceGroup < rs[j].EvidenceGroup
	})
}

func decideTier(a RiskV2Assessment, groupsMet int, severe bool, metrics RiskV2WindowMetrics, cfg RiskV2ScoringConfig) string {
	if !a.DataSufficient {
		return RiskV2TierInsufficient
	}
	// 证据质量太低（confidence < MediumConfidence）：数据充分但证据弱 → WATCH；严重降级/健康未知 → INSUFFICIENT_DATA。
	if a.Confidence < cfg.MediumConfidence {
		if severe {
			return RiskV2TierInsufficient
		}
		return RiskV2TierWatch
	}
	// confidence >= MediumConfidence。
	high := a.Confidence >= cfg.HighConfidence &&
		a.RiskIndex >= cfg.HighIndex &&
		groupsMet >= 2 &&
		!metrics.Degraded &&
		!severe
	if high {
		return RiskV2TierHigh
	}
	// MEDIUM：需 confidence>=MediumConfidence（上面已保证）且（RiskIndex 达 Medium 或 至少一个独立 group 达标）。
	if a.RiskIndex >= cfg.MediumIndex || groupsMet >= 1 {
		return RiskV2TierMedium
	}
	return RiskV2TierWatch
}

// —— 家族评分 ——

func scoreTemplate(w RiskV2Window, cfg RiskV2ScoringConfig, reasons *[]RiskV2ReasonCode) RiskV2EvidenceFamily {
	f := RiskV2EvidenceFamily{Family: EFTemplateEnumeration, Group: EGPromptPattern, Window: w.WindowLabel}
	// 1h 样本门槛：请求过少 → 不可用（不当 0，不从少量样本触发）。
	if w.RequestCount < cfg.MinPromptRequests1h {
		return f
	}
	var strengths []float64
	if dr, ok := w.ExactDuplicateRatio(); ok && w.ExactAvailableCount >= cfg.MinExactAvailable1h {
		f.Available = true
		s := ramp(dr, cfg.ExactDupRatioHigh)
		strengths = append(strengths, s)
		if dr >= cfg.ExactDupRatioHigh {
			*reasons = append(*reasons, RiskV2ReasonCode{Code: "EXACT_DUPLICATION_HIGH", Window: w.WindowLabel, ObservedValue: dr, Threshold: cfg.ExactDupRatioHigh, EvidenceFamily: f.Family, EvidenceGroup: f.Group})
		}
	}
	if rr, ok := w.FullScaffoldReuseRatio(); ok && w.FullScaffoldRequestCount >= cfg.MinPromptRequests1h {
		f.Available = true
		s := ramp(rr, cfg.FullScaffoldReuseHigh)
		strengths = append(strengths, s)
		if rr >= cfg.FullScaffoldReuseHigh {
			*reasons = append(*reasons, RiskV2ReasonCode{Code: "FULL_SCAFFOLD_REUSE_HIGH", Window: w.WindowLabel, ObservedValue: rr, Threshold: cfg.FullScaffoldReuseHigh, EvidenceFamily: f.Family, EvidenceGroup: f.Group})
		}
	}
	if ex, ok := w.ScaffoldExpansionEstimate(); ok {
		f.Available = true
		s := ramp(ex, cfg.ScaffoldExpansionHigh)
		strengths = append(strengths, s)
		if ex >= cfg.ScaffoldExpansionHigh {
			*reasons = append(*reasons, RiskV2ReasonCode{Code: "SCAFFOLD_EXPANSION_HIGH", Window: w.WindowLabel, ObservedValue: ex, Threshold: cfg.ScaffoldExpansionHigh, EvidenceFamily: f.Family, EvidenceGroup: f.Group})
		}
	}
	f.Strength = meanFloat(strengths)
	f.MeetsHigh = f.Available && f.Strength >= cfg.FamilyHighStrength
	return f
}

func scoreTemporal(w RiskV2Window, cfg RiskV2ScoringConfig, reasons *[]RiskV2ReasonCode) RiskV2EvidenceFamily {
	f := RiskV2EvidenceFamily{Family: EFTemporalAutomation, Group: EGTemporalIntensity, Window: w.WindowLabel}
	var strengths []float64
	if w.Available.Requests && w.RequestCount >= cfg.MinTemporalRequests1h {
		f.Available = true
		if w.PeakRPMAvailable {
			s := ramp(float64(w.PeakRPM), cfg.PeakRPMHigh)
			strengths = append(strengths, s)
			if float64(w.PeakRPM) >= cfg.PeakRPMHigh {
				*reasons = append(*reasons, RiskV2ReasonCode{Code: "REQUEST_BURST_HIGH", Window: w.WindowLabel, ObservedValue: float64(w.PeakRPM), Threshold: cfg.PeakRPMHigh, EvidenceFamily: f.Family, EvidenceGroup: f.Group})
			}
		}
		if br, ok := w.BurstRatio(); ok {
			strengths = append(strengths, ramp(br, cfg.BurstRatioHigh))
		}
		if ar, ok := w.RequestAccelerationRatio(); ok {
			strengths = append(strengths, ramp(ar, cfg.AccelRatioHigh))
			if ar >= cfg.AccelRatioHigh {
				*reasons = append(*reasons, RiskV2ReasonCode{Code: "REQUEST_ACCELERATION_HIGH", Window: w.WindowLabel, ObservedValue: ar, Threshold: cfg.AccelRatioHigh, EvidenceFamily: f.Family, EvidenceGroup: f.Group})
			}
		}
		density := float64(w.ActiveMinutes) / 60.0
		strengths = append(strengths, ramp(density, cfg.ActiveMinuteDensityHigh))
	}
	f.Strength = meanFloat(strengths)
	f.MeetsHigh = f.Available && f.Strength >= cfg.FamilyHighStrength
	return f
}

func scoreOutput(w RiskV2Window, cfg RiskV2ScoringConfig, reasons *[]RiskV2ReasonCode) RiskV2EvidenceFamily {
	f := RiskV2EvidenceFamily{Family: EFOutputCollection, Group: EGResponseHarvest, Window: w.WindowLabel}
	// usage 覆盖率不足或 usage 观测样本过少 → 降 availability。
	coverage, ok := ratio(w.UsageAvailableCount, w.RequestCount)
	if !ok || coverage < cfg.UsageCoverageMin || w.UsageAvailableCount < cfg.MinUsageObservations1h {
		return f // Available=false
	}
	f.Available = true
	var strengths []float64
	opm := float64(w.OutputTokens) / 60.0
	strengths = append(strengths, ramp(opm, cfg.OutputPerMinuteHigh))
	if opm >= cfg.OutputPerMinuteHigh {
		*reasons = append(*reasons, RiskV2ReasonCode{Code: "OUTPUT_COLLECTION_HIGH", Window: w.WindowLabel, ObservedValue: opm, Threshold: cfg.OutputPerMinuteHigh, EvidenceFamily: f.Family, EvidenceGroup: f.Group})
	}
	if oir, ok := w.OutputInputRatio(); ok {
		strengths = append(strengths, ramp(oir, cfg.OutputInputRatioHigh))
	}
	if lr, ok := ratio(w.LongOutputCount, w.RequestCount); ok {
		strengths = append(strengths, ramp(lr, cfg.LongOutputRatioHigh))
	}
	if sr, ok := ratio(w.ShortOutputCount, w.RequestCount); ok {
		// 短输出高频：无真实 grading/ranking 分类 → 至多弱-中等信号（0.5 权重）。
		strengths = append(strengths, 0.5*ramp(sr, cfg.ShortOutputFreqHigh))
		if sr >= cfg.ShortOutputFreqHigh {
			*reasons = append(*reasons, RiskV2ReasonCode{Code: "SHORT_OUTPUT_HIGH_FREQUENCY", Window: w.WindowLabel, ObservedValue: sr, Threshold: cfg.ShortOutputFreqHigh, EvidenceFamily: f.Family, EvidenceGroup: f.Group})
		}
	}
	f.Strength = meanFloat(strengths)
	f.MeetsHigh = f.Available && f.Strength >= cfg.FamilyHighStrength
	return f
}

func scoreRepeatedSampling(w RiskV2Window, cfg RiskV2ScoringConfig, reasons *[]RiskV2ReasonCode) RiskV2EvidenceFamily {
	f := RiskV2EvidenceFamily{Family: EFRepeatedSampling, Group: EGPromptPattern, Window: w.WindowLabel}
	// 仅 exact 完整 + paramSig 完整 + 足够 exact 样本/distinct 时可用。
	if w.ExactIncomplete || w.ExactParamSigOverflow || w.ExactAvailableCount < cfg.MinExactAvailable1h || w.DistinctExactEstimate < cfg.MinDistinctExact {
		return f
	}
	f.Available = true
	// parameter_variation_estimate = (distinct_param_sig - distinct_exact)/max(distinct_exact,1)
	de := float64(w.DistinctExactEstimate)
	pv := (float64(w.DistinctExactParamSigEstimate) - de) / maxFloat(de, 1)
	if pv < 0 {
		pv = 0
	}
	f.Strength = ramp(pv, cfg.ParamVariationHigh)
	f.MeetsHigh = f.Strength >= cfg.FamilyHighStrength
	if pv >= cfg.ParamVariationHigh {
		*reasons = append(*reasons, RiskV2ReasonCode{Code: "REPEATED_SAMPLING_HIGH", Window: w.WindowLabel, ObservedValue: pv, Threshold: cfg.ParamVariationHigh, EvidenceFamily: f.Family, EvidenceGroup: f.Group})
	}
	return f
}

func scoreMultiKey(mk RiskV2MultiKeyRollup, userReq1h int64, cfg RiskV2ScoringConfig, reasons *[]RiskV2ReasonCode) RiskV2EvidenceFamily {
	f := RiskV2EvidenceFamily{Family: EFMultiKeyCoordination, Group: EGCoordination, Window: "mixed"}
	// 多 Key 需足够整体请求样本，否则不可用（不当 0）。
	if !mk.MultiKeyAvailable || userReq1h < cfg.MinMultiKeyRequests1h {
		return f
	}
	f.Available = true
	// 三个子信号，各自是否达标。
	sub := 0
	var strengths []float64
	if mk.SynchronizedMultiKeyMinutes1h >= cfg.SyncMinutesHigh {
		sub++
		*reasons = append(*reasons, RiskV2ReasonCode{Code: "MULTI_KEY_SYNCHRONIZED", Window: "1h", ObservedValue: float64(mk.SynchronizedMultiKeyMinutes1h), Threshold: float64(cfg.SyncMinutesHigh), EvidenceFamily: f.Family, EvidenceGroup: f.Group})
	}
	strengths = append(strengths, ramp(float64(mk.SynchronizedMultiKeyMinutes1h), float64(cfg.SyncMinutesHigh)))
	if mk.CrossKeyFullScaffoldOverlapAvailable1h && mk.CrossKeyFullScaffoldOverlapEstimate1h >= cfg.CrossKeyOverlapHigh {
		sub++
		*reasons = append(*reasons, RiskV2ReasonCode{Code: "CROSS_KEY_SCAFFOLD_OVERLAP", Window: "1h", ObservedValue: float64(mk.CrossKeyFullScaffoldOverlapEstimate1h), Threshold: float64(cfg.CrossKeyOverlapHigh), EvidenceFamily: f.Family, EvidenceGroup: f.Group})
	}
	if mk.CrossKeyFullScaffoldOverlapAvailable1h {
		strengths = append(strengths, ramp(float64(mk.CrossKeyFullScaffoldOverlapEstimate1h), float64(cfg.CrossKeyOverlapHigh)))
	}
	if mk.KeyHandoffCount1h >= cfg.KeyHandoffHigh {
		sub++
		*reasons = append(*reasons, RiskV2ReasonCode{Code: "KEY_HANDOFF_DETECTED", Window: "1h", ObservedValue: float64(mk.KeyHandoffCount1h), Threshold: float64(cfg.KeyHandoffHigh), EvidenceFamily: f.Family, EvidenceGroup: f.Group})
	}
	strengths = append(strengths, ramp(float64(mk.KeyHandoffCount1h), float64(cfg.KeyHandoffHigh)))
	f.Strength = meanFloat(strengths)
	// COORDINATION 达 High 至少要两个不同子信号。
	f.MeetsHigh = sub >= 2
	if f.MeetsHigh {
		*reasons = append(*reasons, RiskV2ReasonCode{Code: "MULTI_KEY_COORDINATION_HIGH", Window: "1h", ObservedValue: float64(sub), Threshold: 2, EvidenceFamily: f.Family, EvidenceGroup: f.Group})
	}
	return f
}

func scoreModelConcentration(w RiskV2Window) RiskV2EvidenceFamily {
	// 解释性辅助：不单独构成 HIGH group，不单独触发 HIGH。
	f := RiskV2EvidenceFamily{Family: EFModelConcentration, Group: EGModel, Window: w.WindowLabel}
	if !w.ModelConcentrationAvailable || w.RequestCount == 0 {
		return f
	}
	f.Available = true
	conc, _ := ratio(w.TopModelRequestCount, w.RequestCount)
	f.Strength = clamp0100(conc * 100)
	f.MeetsHigh = false // 恒不作为独立 HIGH 证据
	return f
}

func scoreExposure(w RiskV2Window, cfg RiskV2ScoringConfig) RiskV2PlaneScore {
	// 仅表达 token/请求容量暴露，与 Extraction Risk 解耦；usage 覆盖不足 → unavailable。
	coverage, ok := ratio(w.UsageAvailableCount, w.RequestCount)
	if !ok || coverage < cfg.UsageCoverageMin {
		return RiskV2PlaneScore{Available: false}
	}
	// 归一：24h 输出 token 相对一个较大参考容量。
	s := ramp(float64(w.OutputTokens), 5_000_000)
	return RiskV2PlaneScore{Score: s, Available: true}
}

func computeGroups(template, repeated, temporal, output, campaign RiskV2EvidenceFamily) []RiskV2EvidenceGroup {
	pp := RiskV2EvidenceGroup{Group: EGPromptPattern,
		MetHigh:  template.MeetsHigh || repeated.MeetsHigh,
		Strength: maxFloat(strengthOrZero(template), strengthOrZero(repeated))}
	ti := RiskV2EvidenceGroup{Group: EGTemporalIntensity, MetHigh: temporal.MeetsHigh, Strength: strengthOrZero(temporal)}
	rh := RiskV2EvidenceGroup{Group: EGResponseHarvest, MetHigh: output.MeetsHigh, Strength: strengthOrZero(output)}
	co := RiskV2EvidenceGroup{Group: EGCoordination, MetHigh: campaign.MeetsHigh, Strength: strengthOrZero(campaign)}
	return []RiskV2EvidenceGroup{pp, ti, rh, co}
}

func computeConfidence(metrics RiskV2WindowMetrics, health RiskV2IngestionHealth, cfg RiskV2ScoringConfig, reasons *[]RiskV2ReasonCode) (float64, bool) {
	w1h := metrics.User.W1h
	w24 := metrics.User.W24h
	conf := 1.0

	// 请求量 / 活跃分钟：数据越少 confidence 越低。
	if w24.RequestCount < cfg.MinRequests24h {
		conf *= riskV2Clamp01(float64(w24.RequestCount) / maxFloat(float64(cfg.MinRequests24h), 1))
	}
	if w24.ActiveMinutes < cfg.MinActiveMinutes24h {
		conf *= riskV2Clamp01(float64(w24.ActiveMinutes) / maxFloat(float64(cfg.MinActiveMinutes24h), 1))
	}
	// usage 覆盖率。
	if cov, ok := ratio(w1h.UsageAvailableCount, w1h.RequestCount); ok && cov < 1 {
		conf *= 0.7 + 0.3*cov
	}
	// exact 可用率。
	if er, ok := ratio(w1h.ExactAvailableCount, w1h.RequestCount); ok && er < 1 {
		conf *= 0.7 + 0.3*er
	}
	// sampled scaffold 比例高 → 降。
	if sr, ok := w1h.SampledScaffoldRatio(); ok && sr > cfg.SampledRatioPenaltyAt {
		conf *= riskV2Clamp01(1 - (sr - cfg.SampledRatioPenaltyAt))
		*reasons = append(*reasons, RiskV2ReasonCode{Code: "SAMPLED_INPUT_RATIO_HIGH", Window: "1h", ObservedValue: sr, Threshold: cfg.SampledRatioPenaltyAt, ConfidenceContribution: -0.2})
	}
	// truncated 比例高 → 降。
	if tr, ok := ratio(w1h.TruncatedInputCount, w1h.RequestCount); ok && tr > cfg.TruncatedRatioPenaltyAt {
		conf *= riskV2Clamp01(1 - (tr - cfg.TruncatedRatioPenaltyAt))
		*reasons = append(*reasons, RiskV2ReasonCode{Code: "TRUNCATED_INPUT_RATIO_HIGH", Window: "1h", ObservedValue: tr, Threshold: cfg.TruncatedRatioPenaltyAt, ConfidenceContribution: -0.2})
	}
	// incomplete / degraded。
	if metrics.Incomplete {
		conf *= 0.8
	}
	if metrics.Degraded {
		conf *= 0.5
	}
	if !health.HealthAvailable {
		conf *= 0.5 // 健康未知 → 降 confidence
		*reasons = append(*reasons, RiskV2ReasonCode{Code: "FEATURE_COVERAGE_LOW", Window: "n/a", ConfidenceContribution: -0.3})
	} else if !health.AggregationHealthy || health.DispatcherDegraded {
		conf *= 0.5
		*reasons = append(*reasons, RiskV2ReasonCode{Code: "FEATURE_COVERAGE_LOW", Window: "n/a", ConfidenceContribution: -0.3})
	}
	if health.ObservationDropRatioAvailable && health.ObservationDropRatio > 0 {
		conf *= riskV2Clamp01(1 - health.ObservationDropRatio)
	}
	return riskV2Clamp01(conf), true
}

// —— 小工具 ——

type weighted struct {
	value  float64
	weight float64
	avail  bool
}

func combineAvailable(items []weighted) float64 {
	var sum, wsum float64
	for _, it := range items {
		if it.avail && it.weight > 0 {
			sum += it.value * it.weight
			wsum += it.weight
		}
	}
	if wsum <= 0 {
		return 0
	}
	return clamp0100(sum / wsum)
}

func strengthOrZero(f RiskV2EvidenceFamily) float64 {
	if !f.Available {
		return 0
	}
	return f.Strength
}
func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
func meanFloat(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return clamp0100(s / float64(len(xs)))
}
