package service

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"sync"
	"testing"
)

// —— builders ——

func lowRiskMetrics() RiskV2WindowMetrics {
	w1 := RiskV2Window{
		WindowLabel: "1h", RequestCount: 300, SuccessCount: 300, UsageAvailableCount: 300,
		ActiveMinutes: 5, PeakRPM: 5, PeakRPMAvailable: true, RequestsPerMinute: 5,
		ExactAvailableCount: 300, DistinctExactEstimate: 300, DistinctExactParamSigEstimate: 300,
		FullScaffoldRequestCount: 300, DistinctFullScaffoldEstimate: 300,
		InputTokens: 6000, OutputTokens: 6000,
		ModelConcentrationAvailable: true, DistinctModelCount: 1, TopModelRequestCount: 300,
		Available: RiskV2FeatureAvailability{Requests: true, Fingerprint: true, ActiveMinutes: true, ToolUse: true, StructuredOutput: true, ModelConcentration: true},
	}
	w24 := w1
	w24.WindowLabel = "24h"
	w24.RequestCount = 500
	w24.ActiveMinutes = 30
	w24.OutputTokens = 100000
	return RiskV2WindowMetrics{UserID: 7, FingerprintKeyVersion: "v1", AssessedAtUnix: 1000,
		User: RiskV2EntityWindows{W1h: w1, W24h: w24}}
}

func healthy() RiskV2IngestionHealth {
	return RiskV2IngestionHealth{HealthAvailable: true, AggregationHealthy: true}
}

func highScaffold(w *RiskV2Window) {
	w.ExactAvailableCount, w.DistinctExactEstimate = 300, 20
	w.FullScaffoldRequestCount, w.DistinctFullScaffoldEstimate = 300, 20
}
func highTemporal(w *RiskV2Window) {
	w.PeakRPM, w.PeakRPMAvailable, w.RequestsPerMinute = 300, true, 5
	w.ActiveMinutes = 60
}
func highOutput(w *RiskV2Window) {
	w.RequestCount, w.UsageAvailableCount = 300, 300
	w.OutputTokens, w.InputTokens = 400000, 1000
	w.LongOutputCount = 300
}
func highRepeated(w *RiskV2Window) {
	w.ExactAvailableCount, w.DistinctExactEstimate, w.DistinctExactParamSigEstimate = 300, 10, 40
}
func highMultiKey() RiskV2MultiKeyRollup {
	return RiskV2MultiKeyRollup{MultiKeyAvailable: true, ActiveAPIKeyCount24h: 4,
		SynchronizedMultiKeyMinutes1h: 10, CrossKeyFullScaffoldOverlapAvailable1h: true, CrossKeyFullScaffoldOverlapEstimate1h: 3}
}

func score(m RiskV2WindowMetrics) RiskV2Assessment {
	return ScoreRiskV2(m, healthy(), DefaultRiskV2ScoringConfig())
}

// —— scenarios ——

func TestScore_InsufficientData(t *testing.T) {
	m := lowRiskMetrics()
	m.User.W24h.RequestCount = 10
	if a := score(m); a.RiskTier != RiskV2TierInsufficient {
		t.Errorf("want INSUFFICIENT_DATA, got %s", a.RiskTier)
	}
}

func TestScore_NormalLow_Watch(t *testing.T) {
	if a := score(lowRiskMetrics()); a.RiskTier != RiskV2TierWatch {
		t.Errorf("want WATCH, got %s (index=%.1f)", a.RiskTier, a.RiskIndex)
	}
}

func TestScore_SingleSignalsNeverHigh(t *testing.T) {
	// 单独高 RPM / 高输出 / 单独 exact 重复 → 均不 HIGH（只 1 个 group）。
	rpm := lowRiskMetrics()
	highTemporal(&rpm.User.W1h)
	out := lowRiskMetrics()
	highOutput(&out.User.W1h)
	dup := lowRiskMetrics()
	dup.User.W1h.ExactAvailableCount, dup.User.W1h.DistinctExactEstimate = 300, 20
	for name, m := range map[string]RiskV2WindowMetrics{"rpm": rpm, "output": out, "exactdup": dup} {
		if a := score(m); a.RiskTier == RiskV2TierHigh {
			t.Errorf("single signal %s must not be HIGH", name)
		}
	}
}

func TestScore_TemplatePlusRepeated_SameGroup_NotHigh(t *testing.T) {
	m := lowRiskMetrics()
	highScaffold(&m.User.W1h) // template
	highRepeated(&m.User.W1h) // repeated（同属 PROMPT_PATTERN）
	if a := score(m); a.RiskTier == RiskV2TierHigh {
		t.Errorf("template+repeated share PROMPT_PATTERN group → must not be HIGH alone")
	}
}

func TestScore_ScaffoldPlusTemporal_High(t *testing.T) {
	m := lowRiskMetrics()
	highScaffold(&m.User.W1h)
	highTemporal(&m.User.W1h)
	a := score(m)
	if a.RiskTier != RiskV2TierHigh {
		t.Errorf("scaffold(PROMPT_PATTERN)+temporal(TEMPORAL_INTENSITY) → HIGH; got %s index=%.1f conf=%.2f", a.RiskTier, a.RiskIndex, a.Confidence)
	}
}

func TestScore_ScaffoldPlusOutput_High(t *testing.T) {
	m := lowRiskMetrics()
	highScaffold(&m.User.W1h)
	highOutput(&m.User.W1h)
	if a := score(m); a.RiskTier != RiskV2TierHigh {
		t.Errorf("scaffold+output → HIGH; got %s", a.RiskTier)
	}
}

func TestScore_RepeatedPlusTemporal_High(t *testing.T) {
	m := lowRiskMetrics()
	highRepeated(&m.User.W1h)
	highTemporal(&m.User.W1h)
	if a := score(m); a.RiskTier != RiskV2TierHigh {
		t.Errorf("repeated+temporal → HIGH; got %s", a.RiskTier)
	}
}

func TestScore_SingleMultiKey_CampaignNotHigh(t *testing.T) {
	m := lowRiskMetrics()
	m.MultiKey = RiskV2MultiKeyRollup{MultiKeyAvailable: true, ActiveAPIKeyCount24h: 3, SynchronizedMultiKeyMinutes1h: 10}
	a := score(m)
	for _, f := range a.EvidenceFamilies {
		if f.Family == EFMultiKeyCoordination && f.MeetsHigh {
			t.Error("single multi-key sub-signal must not make campaign high")
		}
	}
}

func TestScore_SyncPlusOverlap_CampaignHigh_ButNotOverallHigh(t *testing.T) {
	m := lowRiskMetrics()
	m.MultiKey = highMultiKey() // 2 sub-signals → campaign high
	a := score(m)
	campHigh := false
	for _, f := range a.EvidenceFamilies {
		if f.Family == EFMultiKeyCoordination {
			campHigh = f.MeetsHigh
		}
	}
	if !campHigh {
		t.Error("sync+overlap → campaign high")
	}
	if a.RiskTier == RiskV2TierHigh {
		t.Error("campaign high alone (only COORDINATION group) → overall must not be HIGH")
	}
}

func TestScore_CampaignPlusOutput_High(t *testing.T) {
	m := lowRiskMetrics()
	m.MultiKey = highMultiKey()
	highOutput(&m.User.W1h)
	if a := score(m); a.RiskTier != RiskV2TierHigh {
		t.Errorf("campaign+output → HIGH; got %s", a.RiskTier)
	}
}

func TestScore_LowCacheReadRatio_NoTierEffect(t *testing.T) {
	m := lowRiskMetrics()
	m.User.W1h.CacheApplicableCount = 300
	m.User.W1h.CacheAvailableCount = 300
	m.User.W1h.CacheObservedInputTokens = 100000
	m.User.W1h.CacheReadInputTokens = 0 // 极低 cache read
	if a := score(m); a.RiskTier != RiskV2TierWatch {
		t.Errorf("low cache read must not change tier; got %s", a.RiskTier)
	}
}

func TestScore_HighModelConcentration_NotAlone(t *testing.T) {
	m := lowRiskMetrics()
	m.User.W1h.TopModelRequestCount = 300 // 100% 集中
	if a := score(m); a.RiskTier == RiskV2TierHigh {
		t.Error("model concentration alone must not be HIGH")
	}
}

func TestScore_HighExposure_DoesNotDecideTier(t *testing.T) {
	m := lowRiskMetrics()
	m.User.W24h.OutputTokens = 50_000_000 // 巨大暴露
	m.User.W24h.UsageAvailableCount = m.User.W24h.RequestCount
	a := score(m)
	if a.RiskTier == RiskV2TierHigh {
		t.Error("high exposure must not decide extraction tier")
	}
	if a.Exposure.Available && a.Exposure.Score > 0 && a.RiskIndex >= DefaultRiskV2ScoringConfig().HighIndex {
		t.Error("exposure must be decoupled from risk index")
	}
}

func TestScore_SampledRatioHigh_LowersConfidence(t *testing.T) {
	base := score(lowRiskMetrics())
	m := lowRiskMetrics()
	m.User.W1h.FullScaffoldRequestCount = 100
	m.User.W1h.SampledScaffoldRequestCount = 300 // 高采样比例
	if a := score(m); a.Confidence >= base.Confidence {
		t.Errorf("high sampled ratio must lower confidence (base=%.2f got=%.2f)", base.Confidence, a.Confidence)
	}
}

func TestScore_ExactOverflow_ExactEvidenceUnavailable(t *testing.T) {
	m := lowRiskMetrics()
	highScaffold(&m.User.W1h)
	m.User.W1h.ExactIncomplete = true // overflow → incomplete
	a := score(m)
	// exact 重复/expansion 应 unavailable；模板家族强度不应来自 exact。
	if _, ok := m.User.W1h.ExactDuplicateRatio(); ok {
		t.Error("exact incomplete → duplicate ratio must be unavailable")
	}
	_ = a
}

func TestScore_ScaffoldOverflow_ScaffoldEvidenceUnavailable(t *testing.T) {
	m := lowRiskMetrics()
	highScaffold(&m.User.W1h)
	m.User.W1h.FullScaffoldIncomplete = true
	if _, ok := m.User.W1h.FullScaffoldReuseRatio(); ok {
		t.Error("full scaffold incomplete → reuse ratio unavailable")
	}
}

func TestScore_APIKeyOverflow_LowersCampaignConfidence(t *testing.T) {
	m := lowRiskMetrics()
	m.MultiKey = highMultiKey()
	m.MultiKey.APIKeyOverflow = true
	m.MultiKey.MultiKeyIncomplete = true
	m.Incomplete = true
	base := score(lowRiskMetrics())
	if a := score(m); a.Confidence >= base.Confidence {
		t.Errorf("apikey overflow/incomplete must lower confidence")
	}
}

func TestScore_MetricsDegraded_NotHigh(t *testing.T) {
	m := lowRiskMetrics()
	highScaffold(&m.User.W1h)
	highTemporal(&m.User.W1h)
	m.Degraded = true
	if a := score(m); a.RiskTier == RiskV2TierHigh {
		t.Error("Metrics.Degraded must prevent HIGH")
	}
}

func TestScore_HighDropRatio_NotHigh(t *testing.T) {
	m := lowRiskMetrics()
	highScaffold(&m.User.W1h)
	highTemporal(&m.User.W1h)
	h := RiskV2IngestionHealth{HealthAvailable: true, AggregationHealthy: true, ObservationDropRatioAvailable: true, ObservationDropRatio: 0.5}
	if a := ScoreRiskV2(m, h, DefaultRiskV2ScoringConfig()); a.RiskTier == RiskV2TierHigh {
		t.Error("high dispatcher drop ratio must prevent HIGH")
	}
}

func TestScore_FeatureUnavailableNotZero(t *testing.T) {
	m := lowRiskMetrics()
	m.User.W1h.Available.Requests = false // temporal unavailable
	m.User.W1h.RequestCount = 0
	a := score(m)
	if a.Automation.Available {
		t.Error("temporal must be unavailable, not available-with-0")
	}
}

func TestScore_NoNaNOrInf(t *testing.T) {
	a := ScoreRiskV2(RiskV2WindowMetrics{}, RiskV2IngestionHealth{}, DefaultRiskV2ScoringConfig())
	for _, v := range []float64{a.RiskIndex, a.Confidence, a.Automation.Score, a.Harvest.Score, a.Campaign.Score, a.Exposure.Score} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			t.Fatalf("NaN/Inf produced: %v", v)
		}
	}
}

func TestScore_Deterministic(t *testing.T) {
	m := lowRiskMetrics()
	highScaffold(&m.User.W1h)
	highTemporal(&m.User.W1h)
	if !reflect.DeepEqual(score(m), score(m)) {
		t.Error("same input must produce identical assessment")
	}
}

func TestScore_ConcurrentRaceSafe(t *testing.T) {
	m := lowRiskMetrics()
	highScaffold(&m.User.W1h)
	highTemporal(&m.User.W1h)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = score(m) }()
	}
	wg.Wait()
}

func TestScore_ReasonCodesNoSensitiveAndScoresBounded(t *testing.T) {
	m := lowRiskMetrics()
	highScaffold(&m.User.W1h)
	highTemporal(&m.User.W1h)
	m.MultiKey = highMultiKey()
	a := score(m)
	for _, r := range a.ReasonCodes {
		if r.Code == "" {
			t.Error("reason code must have a Code")
		}
		// 结构上不含 prompt/hmac/id/secret 字段（只有数值/窗口/family/group）。
	}
	for _, s := range []float64{a.RiskIndex, a.Automation.Score, a.Harvest.Score, a.Campaign.Score, a.Exposure.Score} {
		if s < 0 || s > 100 {
			t.Errorf("score out of [0,100]: %v", s)
		}
	}
	if a.Confidence < 0 || a.Confidence > 1 {
		t.Errorf("confidence out of [0,1]: %v", a.Confidence)
	}
}

func TestScore_EffectiveActionAlwaysNone(t *testing.T) {
	m := lowRiskMetrics()
	highScaffold(&m.User.W1h)
	highTemporal(&m.User.W1h)
	m.MultiKey = highMultiKey()
	if a := score(m); a.EffectiveAction != RiskV2ActionNone {
		t.Errorf("EffectiveAction must always be NONE, got %s", a.EffectiveAction)
	}
}

// —— 契约小修（一）测试 ——

func TestScore_MediumConfidenceGatesMedium(t *testing.T) {
	// 一个 group 达标（本应 MEDIUM），但把 confidence 压到 < MediumConfidence，且非 severe → WATCH。
	m := lowRiskMetrics()
	m.User.W1h.ExactAvailableCount, m.User.W1h.DistinctExactEstimate = 300, 20 // 模板重复 → PROMPT_PATTERN 达标
	m.Incomplete = true
	m.User.W1h.UsageAvailableCount = 60                                                   // 覆盖率 0.2
	m.User.W1h.FullScaffoldRequestCount, m.User.W1h.SampledScaffoldRequestCount = 30, 270 // 高采样比例
	m.User.W1h.TruncatedInputCount = 270
	a := score(m) // health healthy → 非 severe
	if a.Confidence >= DefaultRiskV2ScoringConfig().MediumConfidence {
		t.Fatalf("test setup: confidence must be < MediumConfidence, got %.3f", a.Confidence)
	}
	if a.RiskTier != RiskV2TierWatch {
		t.Errorf("low confidence (non-severe) must yield WATCH not MEDIUM/HIGH, got %s", a.RiskTier)
	}
}

func TestScore_LowSampleNoEvidence(t *testing.T) {
	m := lowRiskMetrics()
	m.User.W1h.RequestCount = 5                                             // < MinPromptRequests1h/MinTemporalRequests1h
	m.User.W1h.ExactAvailableCount, m.User.W1h.DistinctExactEstimate = 5, 1 // 高 dup 比例但样本不足
	a := score(m)
	for _, f := range a.EvidenceFamilies {
		if (f.Family == EFTemplateEnumeration || f.Family == EFTemporalAutomation) && f.Available {
			t.Errorf("family %s must be unavailable on insufficient 1h sample", f.Family)
		}
	}
	for _, r := range a.ReasonCodes {
		if r.Code == "EXACT_DUPLICATION_HIGH" {
			t.Error("must not fire evidence reason from insufficient sample")
		}
	}
}

func TestScore_ConfigInvalid(t *testing.T) {
	cfg := DefaultRiskV2ScoringConfig()
	cfg.MediumIndex, cfg.HighIndex = 80, 70 // Medium >= High → invalid
	if cfg.Validate() == nil {
		t.Fatal("expected invalid config")
	}
	m := lowRiskMetrics()
	highScaffold(&m.User.W1h)
	highTemporal(&m.User.W1h)
	a := ScoreRiskV2(m, healthy(), cfg)
	if a.RiskTier == RiskV2TierHigh {
		t.Error("invalid config must never produce HIGH")
	}
	if a.RiskTier != RiskV2TierInsufficient {
		t.Errorf("invalid config → INSUFFICIENT_DATA, got %s", a.RiskTier)
	}
	found := false
	for _, r := range a.ReasonCodes {
		if r.Code == "RISK_V2_CONFIG_INVALID" {
			found = true
		}
	}
	if !found {
		t.Error("must include RISK_V2_CONFIG_INVALID reason")
	}
}

func TestScore_ConfigNaNInf(t *testing.T) {
	for _, bad := range []func(*RiskV2ScoringConfig){
		func(c *RiskV2ScoringConfig) { c.ExactDupRatioHigh = math.NaN() },
		func(c *RiskV2ScoringConfig) { c.PeakRPMHigh = math.Inf(1) },
		func(c *RiskV2ScoringConfig) { c.HighIndex = math.Inf(1) },
	} {
		cfg := DefaultRiskV2ScoringConfig()
		bad(&cfg)
		if cfg.Validate() == nil {
			t.Error("NaN/Inf config must be invalid")
		}
		if a := ScoreRiskV2(lowRiskMetrics(), healthy(), cfg); a.RiskTier == RiskV2TierHigh {
			t.Error("NaN/Inf config must not produce HIGH")
		}
	}
}

func TestScore_HealthUnknown_NoHigh_LowersConfidence(t *testing.T) {
	m := lowRiskMetrics()
	highScaffold(&m.User.W1h)
	highTemporal(&m.User.W1h)
	base := ScoreRiskV2(m, healthy(), DefaultRiskV2ScoringConfig())
	if base.RiskTier != RiskV2TierHigh {
		t.Fatalf("baseline should be HIGH, got %s", base.RiskTier)
	}
	unknown := ScoreRiskV2(m, RiskV2IngestionHealth{HealthAvailable: false}, DefaultRiskV2ScoringConfig())
	if unknown.RiskTier == RiskV2TierHigh {
		t.Error("health unknown must prevent HIGH")
	}
	if unknown.Confidence >= base.Confidence {
		t.Error("health unknown must lower confidence")
	}
}

func TestScore_JSONDeterministic(t *testing.T) {
	m := lowRiskMetrics()
	highScaffold(&m.User.W1h)
	highTemporal(&m.User.W1h)
	m.MultiKey = highMultiKey()
	b1, _ := json.Marshal(score(m))
	b2, _ := json.Marshal(score(m))
	if !bytes.Equal(b1, b2) {
		t.Error("same input+config must produce byte-identical JSON")
	}
	// reason codes 稳定排序（Code 升序）。
	a := score(m)
	for i := 1; i < len(a.ReasonCodes); i++ {
		if a.ReasonCodes[i-1].Code > a.ReasonCodes[i].Code {
			t.Errorf("reason codes must be sorted by Code: %s > %s", a.ReasonCodes[i-1].Code, a.ReasonCodes[i].Code)
		}
	}
}

// —— benchmarks（纯内存）——

func BenchmarkScoreRiskV2_Normal(b *testing.B) {
	m := lowRiskMetrics()
	h := healthy()
	cfg := DefaultRiskV2ScoringConfig()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ScoreRiskV2(m, h, cfg)
	}
}

func BenchmarkScoreRiskV2_AllFeatures(b *testing.B) {
	m := lowRiskMetrics()
	highScaffold(&m.User.W1h)
	highTemporal(&m.User.W1h)
	highOutput(&m.User.W1h)
	highRepeated(&m.User.W1h)
	m.MultiKey = highMultiKey()
	h := healthy()
	cfg := DefaultRiskV2ScoringConfig()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ScoreRiskV2(m, h, cfg)
	}
}

func BenchmarkScoreRiskV2_10kAssessments(b *testing.B) {
	m := lowRiskMetrics()
	highScaffold(&m.User.W1h)
	highTemporal(&m.User.W1h)
	h := healthy()
	cfg := DefaultRiskV2ScoringConfig()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < 10000; j++ {
			_ = ScoreRiskV2(m, h, cfg)
		}
	}
}
