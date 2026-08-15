package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// Risk Phase 0（仅观测）单元测试：simhash 归一化/坍缩、特征计算、AND-gate、预算。

func TestRiskSimhashTemplateCollision(t *testing.T) {
	// 同模板、仅数字槽位不同 → 归一化后应完全一致 → simhash 相同。
	a := []byte(`[{"role":"user","content":"Translate order 12345 shipped on 2024-01-02"}]`)
	b := []byte(`[{"role":"user","content":"Translate order 98765 shipped on 2025-11-30"}]`)
	ha := ComputeMessagesSimhash(a)
	hb := ComputeMessagesSimhash(b)
	if ha == 0 || hb == 0 {
		t.Fatalf("simhash should be non-zero: ha=%d hb=%d", ha, hb)
	}
	if ha != hb {
		t.Errorf("same template different digits should collide: ha=%d hb=%d (hamming=%d)", ha, hb, hammingDistance64(ha, hb))
	}
}

func TestRiskSimhashCaseAndWhitespaceCollision(t *testing.T) {
	a := []byte(`[{"role":"user","content":"HELLO   WORLD"}]`)
	b := []byte(`[{"role":"user","content":"hello world"}]`)
	if ComputeMessagesSimhash(a) != ComputeMessagesSimhash(b) {
		t.Errorf("case/whitespace should normalize to same hash")
	}
}

func TestRiskSimhashDifferentContentDiverges(t *testing.T) {
	a := []byte(`[{"role":"user","content":"write a poem about the ocean and its waves"}]`)
	b := []byte(`[{"role":"user","content":"explain quantum entanglement in simple terms"}]`)
	ha := ComputeMessagesSimhash(a)
	hb := ComputeMessagesSimhash(b)
	if ha == hb {
		t.Errorf("clearly different content should not collide")
	}
	if d := hammingDistance64(ha, hb); d < 5 {
		t.Errorf("expected meaningful hamming distance, got %d", d)
	}
}

func TestRiskSimhashEmptyIsZero(t *testing.T) {
	if ComputeMessagesSimhash(nil) != 0 {
		t.Errorf("nil input should be 0")
	}
	if ComputeMessagesSimhash([]byte("   ")) != 0 {
		t.Errorf("whitespace-only should be 0")
	}
}

func TestNormalizeForSimhashCapsAt8KB(t *testing.T) {
	big := make([]byte, 20*1024)
	for i := range big {
		big[i] = 'a'
	}
	n := normalizeForSimhash(big)
	if len(n) > riskSimhashMaxInput {
		t.Errorf("normalized should be capped near 8KB, got %d", len(n))
	}
}

func defaultRiskConfig() config.RiskConfig {
	return config.RiskConfig{
		Mode:               "observe",
		VolumeFloor:        200,
		AndGateK:           3,
		MediumScore:        40,
		HighScore:          70,
		DailyBudgetMicros:  50_000_000,
		WeeklyBudgetMicros: 300_000_000,
		Weights: map[string]float64{
			"f1": 0.20, "f2": 0.15, "f3": 0.20, "f4": 0.15, "f5": 0.15, "f6": 0.10, "f7": 0.05,
		},
		Thresholds: map[string]float64{
			"f1": 0.85, "f2": 0.80, "f3": 0.70, "f4": 0.70, "f5": 0.80, "f6": 0.70, "f7": 0.60,
		},
	}
}

func TestRiskFeaturesDiversityCollapseRequiresVolumeFloor(t *testing.T) {
	cfg := defaultRiskConfig()
	// 低于 volume floor：f1 应为 0（即便 distinct/total 很低）。
	low := RiskFeatureInputs{TotalSim: 50, DistinctSim: 1}
	if f := ComputeRiskFeatures(low, cfg); f.F1DiversityCollapse != 0 {
		t.Errorf("below volume floor f1 must be 0, got %f", f.F1DiversityCollapse)
	}
	// 达到 volume floor：f1 = 1 - distinct/total = 1 - 2/1000 = 0.998。
	high := RiskFeatureInputs{TotalSim: 1000, DistinctSim: 2}
	f := ComputeRiskFeatures(high, cfg)
	if f.F1DiversityCollapse < 0.99 {
		t.Errorf("expected high f1, got %f", f.F1DiversityCollapse)
	}
}

func TestRiskBudgetPctCalc(t *testing.T) {
	cfg := defaultRiskConfig()
	in := RiskFeatureInputs{
		SpendDailyMicros:  25_000_000, // 半个日预算
		SpendWeeklyMicros: 150_000_000,
	}
	f := ComputeRiskFeatures(in, cfg)
	if f.BudgetDailyPct < 0.49 || f.BudgetDailyPct > 0.51 {
		t.Errorf("expected daily budget pct ~0.5, got %f", f.BudgetDailyPct)
	}
	if f.BudgetWeeklyPct < 0.49 || f.BudgetWeeklyPct > 0.51 {
		t.Errorf("expected weekly budget pct ~0.5, got %f", f.BudgetWeeklyPct)
	}
}

func TestRiskAndGateBlocksHighBelowVolumeFloor(t *testing.T) {
	cfg := defaultRiskConfig()
	// 极端可疑但请求量不足：AND-gate 前置条件（volume floor）未过 → 不得为 high。
	in := RiskFeatureInputs{
		Requests24h: 50, // < floor 200
		TotalSim:    50, DistinctSim: 1,
		SingleTurn: 50, TotalTurns: 50,
		OutputTokens: 5_000_000, InputTokens: 100_000,
		RPMPeak: 60, TopModelCount: 50, ModelVariety: 1,
		ZeroTempShare: 1, MaxTokenPinShare: 1,
	}
	res := ScoreRisk(in, cfg, false, nil)
	if res.Tier == RiskTierHigh {
		t.Errorf("must not be high below volume floor; tier=%s score=%d", res.Tier, res.Score)
	}
}

func TestRiskAndGateAllowsHighWhenConditionsMet(t *testing.T) {
	cfg := defaultRiskConfig()
	// 足量 + 多特征超阈值 → high。
	in := RiskFeatureInputs{
		Requests24h: 5000,
		TotalSim:    5000, DistinctSim: 5, // f1 ~ 0.999
		SingleTurn: 5000, TotalTurns: 5000, // f2 = 1.0
		OutputTokens: 10_000_000, InputTokens: 100_000, // f3 高
		RPMPeak: 60, ActiveMinutes: 1440, // f4 高
		TopModelCount: 5000, ModelVariety: 1, // f5 高
		ZeroTempShare: 1, MaxTokenPinShare: 1, // f6 高
	}
	res := ScoreRisk(in, cfg, false, nil)
	if res.TriggeredCount < cfg.AndGateK {
		t.Fatalf("expected >= K triggered, got %d", res.TriggeredCount)
	}
	if res.Tier != RiskTierHigh {
		t.Errorf("expected high, got tier=%s score=%d triggered=%d", res.Tier, res.Score, res.TriggeredCount)
	}
}

func TestRiskAndGateKThreshold(t *testing.T) {
	cfg := defaultRiskConfig()
	// 足量但仅 2 个特征超阈值（< K=3）→ 不得为 high，即便 score 偶然过线也被门控压到 medium。
	in := RiskFeatureInputs{
		Requests24h: 5000,
		TotalSim:    5000, DistinctSim: 5, // f1 高
		SingleTurn: 5000, TotalTurns: 5000, // f2 高
		// f3/f4/f5 保持低：无 output、无 rpm、多模型
		OutputTokens: 0, InputTokens: 100000,
		RPMPeak: 0, ModelVariety: 20, TopModelCount: 250,
	}
	res := ScoreRisk(in, cfg, false, nil)
	if res.Tier == RiskTierHigh {
		t.Errorf("only 2 features triggered (<K) must not be high; triggered=%d tier=%s", res.TriggeredCount, res.Tier)
	}
}

func TestRiskAllowlistForcesWatch(t *testing.T) {
	cfg := defaultRiskConfig()
	in := RiskFeatureInputs{
		Requests24h: 5000, TotalSim: 5000, DistinctSim: 5,
		SingleTurn: 5000, TotalTurns: 5000,
		OutputTokens: 10_000_000, InputTokens: 100_000,
		RPMPeak: 60, ActiveMinutes: 1440,
		TopModelCount: 5000, ModelVariety: 1,
		ZeroTempShare: 1, MaxTokenPinShare: 1,
	}
	res := ScoreRisk(in, cfg, true, nil)
	if res.Tier != RiskTierWatch {
		t.Errorf("allowlisted must be watch, got %s", res.Tier)
	}
}

func TestRiskManualTierOverride(t *testing.T) {
	cfg := defaultRiskConfig()
	in := RiskFeatureInputs{Requests24h: 10} // 本应 watch
	manual := RiskTierHigh
	res := ScoreRisk(in, cfg, false, &manual)
	if res.Tier != RiskTierHigh {
		t.Errorf("manual tier should override to high, got %s", res.Tier)
	}
	// allowlisted 仍压制 manual。
	res2 := ScoreRisk(in, cfg, true, &manual)
	if res2.Tier != RiskTierWatch {
		t.Errorf("allowlist must suppress manual tier, got %s", res2.Tier)
	}
}

func TestRiskManualTierInvalidIgnored(t *testing.T) {
	cfg := defaultRiskConfig()
	in := RiskFeatureInputs{Requests24h: 10}
	bad := "bogus"
	res := ScoreRisk(in, cfg, false, &bad)
	if res.Tier != RiskTierWatch {
		t.Errorf("invalid manual tier should be ignored (stays watch), got %s", res.Tier)
	}
}

func TestWouldDoActionMapping(t *testing.T) {
	if WouldDoAction(RiskTierHigh) == "none" {
		t.Errorf("high should map to a non-none shadow action")
	}
	if WouldDoAction(RiskTierWatch) != "none" {
		t.Errorf("watch should map to none")
	}
}
