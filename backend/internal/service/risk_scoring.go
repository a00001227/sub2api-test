package service

import (
	"math"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// Risk Phase 0（仅观测/影子模式）：特征归一化 + 加权打分 + AND-gate 定级。
// 纯函数，无 IO，便于单测。评分 worker 采集原始信号后调用本文件计算 score/tier。

// Risk tier 常量。
const (
	RiskTierWatch  = "watch"
	RiskTierMedium = "medium"
	RiskTierHigh   = "high"
)

// RiskFeatureInputs 是评分 worker 从 Redis 草图 + 免费计数器 + usage_logs 24h
// 聚合采集到的原始信号（未归一化）。全部为观测量，不含任何 prompt 文本。
type RiskFeatureInputs struct {
	// 请求量
	Requests24h      int     // 24h 请求总数（volume floor 基准）
	DistinctSim      int     // 窗口内去重 simhash 数
	TotalSim         int     // 窗口内 simhash 总数（有 hash 的请求数）
	SingleTurn       int     // 单轮（message_count<=1 或恰好一问）请求数
	TotalTurns       int     // 计入轮次统计的请求数
	InputTokens      int64   // 24h 累计输入 token
	OutputTokens     int64   // 24h 累计输出 token
	RPMPeak          int     // 窗口内峰值 RPM（免费计数器）
	ActiveMinutes    int     // 24h 内有活动的分钟数（cadence 覆盖度，最大 1440）
	TopModelCount    int     // 头部模型请求数
	ModelVariety     int     // 去重模型数
	ZeroTempShare    float64 // temp==0 或固定 temp 请求占比 [0,1]
	MaxTokenPinShare float64 // max_tokens 固定（单一取值）占比 [0,1]
	SubkeyCount      int     // 该用户当前活跃子键/并行 key 数
	AccountAgeDays   int     // 账号年龄（天）；<0 表示未知
	// 经济消耗（USDC micros，来自 spend 计数器）
	SpendDailyMicros  int64
	SpendWeeklyMicros int64
}

// RiskFeatures 是归一化后的 7 个特征（[0,1]）+ 观测衍生量。
type RiskFeatures struct {
	F1DiversityCollapse    float64 `json:"f1_diversity_collapse"`
	F2SingleTurnRatio      float64 `json:"f2_single_turn_ratio"`
	F3OutputHarvest        float64 `json:"f3_output_harvest"`
	F4MachineCadence       float64 `json:"f4_machine_cadence"`
	F5TeacherConcentration float64 `json:"f5_teacher_concentration"`
	F6Determinism          float64 `json:"f6_determinism"`
	F7FanoutNovelty        float64 `json:"f7_fanout_novelty"`

	// 观测衍生量（不参与打分，供仪表盘展示）。
	DistinctRatio   float64 `json:"distinct_ratio"`
	OutInRatio      float64 `json:"out_in_ratio"`
	TopModelShare   float64 `json:"top_model_share"`
	BudgetDailyPct  float64 `json:"budget_daily_pct"`
	BudgetWeeklyPct float64 `json:"budget_weekly_pct"`
}

// RiskScoreResult 是打分结果。
type RiskScoreResult struct {
	Score    int
	Tier     string
	Features RiskFeatures
	// TriggeredCount 是 f1..f5 中超阈值的个数（AND-gate 计数）。
	TriggeredCount int
}

func riskClamp01(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func riskSafeRatio(num, den float64) float64 {
	if den <= 0 {
		return 0
	}
	return num / den
}

// ComputeRiskFeatures 归一化原始信号为 7 个 [0,1] 特征 + 观测衍生量。
func ComputeRiskFeatures(in RiskFeatureInputs, cfg config.RiskConfig) RiskFeatures {
	var f RiskFeatures

	// f1 diversity_collapse = 1 - distinct/total（仅当 total >= volume floor 才有意义；
	// 否则置 0 以免小样本误判——AND-gate 亦要求 volume floor）。
	distinctRatio := riskSafeRatio(float64(in.DistinctSim), float64(in.TotalSim))
	f.DistinctRatio = distinctRatio
	if in.TotalSim >= cfg.VolumeFloor {
		f.F1DiversityCollapse = riskClamp01(1 - distinctRatio)
	}

	// f2 single_turn_ratio
	f.F2SingleTurnRatio = riskClamp01(riskSafeRatio(float64(in.SingleTurn), float64(in.TotalTurns)))

	// f3 output_harvest：out/in 比值（归一到 [0,1]，比值>=4 视为满格）+ 总产出量加权。
	outIn := riskSafeRatio(float64(in.OutputTokens), float64(in.InputTokens))
	f.OutInRatio = outIn
	ratioComponent := riskClamp01(outIn / 4.0)
	// 产出规模：>=2M output token 视为满格，避免小账号误判。
	volumeComponent := riskClamp01(float64(in.OutputTokens) / 2_000_000.0)
	f.F3OutputHarvest = riskClamp01(0.7*ratioComponent + 0.3*volumeComponent)

	// f4 machine_cadence：持续高 RPM + 24h 覆盖度（活动分钟占比）。
	// RPM>=30 视为满格；覆盖度 = active_minutes/1440。
	rpmComponent := riskClamp01(float64(in.RPMPeak) / 30.0)
	coverage := riskClamp01(float64(in.ActiveMinutes) / 1440.0)
	f.F4MachineCadence = riskClamp01(0.6*rpmComponent + 0.4*coverage)

	// f5 teacher_concentration：头部模型占比高 + 模型种类少。
	topShare := riskSafeRatio(float64(in.TopModelCount), float64(in.Requests24h))
	f.TopModelShare = topShare
	varietyPenalty := 0.0
	if in.ModelVariety <= 1 {
		varietyPenalty = 1.0
	} else if in.ModelVariety == 2 {
		varietyPenalty = 0.5
	}
	f.F5TeacherConcentration = riskClamp01(0.7*topShare + 0.3*varietyPenalty)

	// f6 determinism：temp==0/固定 temp 占比 + max_tokens 固定占比。
	f.F6Determinism = riskClamp01(0.5*riskClamp01(in.ZeroTempShare) + 0.5*riskClamp01(in.MaxTokenPinShare))

	// f7 fanout_novelty：并行子键数（>=5 满格）+ 新账号高量。
	subkeyComponent := riskClamp01(float64(in.SubkeyCount) / 5.0)
	newAccountComponent := 0.0
	if in.AccountAgeDays >= 0 && in.AccountAgeDays <= 3 && in.Requests24h >= cfg.VolumeFloor {
		newAccountComponent = 1.0
	}
	f.F7FanoutNovelty = riskClamp01(0.6*subkeyComponent + 0.4*newAccountComponent)

	// 观测衍生量：经济消耗预算占比。
	if cfg.DailyBudgetMicros > 0 {
		f.BudgetDailyPct = riskSafeRatio(float64(in.SpendDailyMicros), float64(cfg.DailyBudgetMicros))
	}
	if cfg.WeeklyBudgetMicros > 0 {
		f.BudgetWeeklyPct = riskSafeRatio(float64(in.SpendWeeklyMicros), float64(cfg.WeeklyBudgetMicros))
	}

	return f
}

// featureValue 按键取归一化特征值。
func featureValue(f RiskFeatures, key string) float64 {
	switch key {
	case "f1":
		return f.F1DiversityCollapse
	case "f2":
		return f.F2SingleTurnRatio
	case "f3":
		return f.F3OutputHarvest
	case "f4":
		return f.F4MachineCadence
	case "f5":
		return f.F5TeacherConcentration
	case "f6":
		return f.F6Determinism
	case "f7":
		return f.F7FanoutNovelty
	default:
		return 0
	}
}

// ScoreRisk 计算 0..100 分并按 AND-gate 定级。
// AND-gate：tier=high 仅当 24h 请求量 >= volume_floor 且 f1..f5 中 >= K 个超阈值
// 且 score >= high_score；否则最高只能到 medium/watch。
// manualTier 非空则直接覆盖（仍返回计算得到的 score/features 以便观测）。
// allowlisted → 永远 watch。
func ScoreRisk(in RiskFeatureInputs, cfg config.RiskConfig, allowlisted bool, manualTier *string) RiskScoreResult {
	f := ComputeRiskFeatures(in, cfg)

	// 加权和 → 0..100。
	var weightSum, weighted float64
	for _, key := range []string{"f1", "f2", "f3", "f4", "f5", "f6", "f7"} {
		w := cfg.Weights[key]
		weightSum += w
		weighted += w * featureValue(f, key)
	}
	score := 0
	if weightSum > 0 {
		score = int(math.Round(100 * weighted / weightSum))
	}
	if score < 0 {
		score = 0
	} else if score > 100 {
		score = 100
	}

	// AND-gate 计数：f1..f5 超各自阈值的个数。
	triggered := 0
	for _, key := range []string{"f1", "f2", "f3", "f4", "f5"} {
		if featureValue(f, key) >= cfg.Thresholds[key] {
			triggered++
		}
	}

	result := RiskScoreResult{Score: score, Features: f, TriggeredCount: triggered}

	// 计算 tier（AND-gate 门控）。
	tier := RiskTierWatch
	if score >= cfg.MediumScore {
		tier = RiskTierMedium
	}
	if score >= cfg.HighScore && in.Requests24h >= cfg.VolumeFloor && triggered >= cfg.AndGateK {
		tier = RiskTierHigh
	}

	// allowlisted 永远 watch。
	if allowlisted {
		tier = RiskTierWatch
	}

	// 手动覆盖（非空且合法）优先于自动定级；allowlisted 仍然压制到 watch。
	if manualTier != nil && !allowlisted {
		switch *manualTier {
		case RiskTierWatch, RiskTierMedium, RiskTierHigh:
			tier = *manualTier
		}
	}

	result.Tier = tier
	return result
}

// WouldDoAction 返回 enforce 模式下针对某 tier“将会”采取的动作字符串（影子仪表盘用）。
// Phase 0 绝不真正执行；仅供前端展示。
func WouldDoAction(tier string) string {
	switch tier {
	case RiskTierHigh:
		return "throttle_and_flag"
	case RiskTierMedium:
		return "monitor_closely"
	default:
		return "none"
	}
}
