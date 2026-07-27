package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Phase 21H quota-budget pacing 单元测试。
// 关注四件事：
//   1. 未设 pacing_mode 的存量账号行为完全不变（回归安全）。
//   2. 预算软刹车：窗口前期烧太快 → StickyOnly，硬上限语义不变。
//   3. 反馈系数：限流后减半、随时间线性恢复。
//   4. 档位差异：steady < smart < burst。

func pacingAccount(mode string, windowStart time.Time, extra map[string]any) *Account {
	e := map[string]any{
		"window_cost_limit": 10.0, // $10 / 5h 窗口
	}
	if mode != "" {
		e["pacing_mode"] = mode
	}
	for k, v := range extra {
		e[k] = v
	}
	end := windowStart.Add(5 * time.Hour)
	return &Account{
		ID:                 1,
		Platform:           PlatformAnthropic,
		Type:               AccountTypeOAuth,
		Extra:              e,
		SessionWindowStart: &windowStart,
		SessionWindowEnd:   &end,
	}
}

func TestPacing_NoMode_LegacyBehaviorUnchanged(t *testing.T) {
	a := pacingAccount("", time.Now().Add(-30*time.Minute), nil)
	// 传统行为：只要 < limit 就 Schedulable，哪怕窗口才过 10% 已花 90%。
	require.Equal(t, WindowCostSchedulable, a.CheckWindowCostSchedulability(9.0))
	// limit 之上进入黄区（默认 sticky reserve $10），limit+reserve 之上红区。
	require.Equal(t, WindowCostStickyOnly, a.CheckWindowCostSchedulability(10.0))
	require.Equal(t, WindowCostNotSchedulable, a.CheckWindowCostSchedulability(20.0))
	require.Equal(t, "", a.GetPacingMode())
	require.Equal(t, 1.0, a.WindowBudgetTargetFraction(time.Now()))
}

func TestPacing_BudgetBrake_EarlyBurnGoesStickyOnly(t *testing.T) {
	// 窗口刚过 30 分钟（10%），smart 档预算目标 ≈ 10% + 10% floor = 20% → $2。
	a := pacingAccount(PacingModeSmart, time.Now().Add(-30*time.Minute), nil)

	// 已花 $1（10%）：低于预算目标 → 正常调度。
	require.Equal(t, WindowCostSchedulable, a.CheckWindowCostSchedulability(1.0))
	// 已花 $9（90%）：远超预算目标 → 软刹车 StickyOnly（不是排除）。
	require.Equal(t, WindowCostStickyOnly, a.CheckWindowCostSchedulability(9.0))
	// 硬上限之上仍是原语义（limit+reserve 之上 → 红区）。
	require.Equal(t, WindowCostNotSchedulable, a.CheckWindowCostSchedulability(20.5))
}

func TestPacing_BudgetBrake_ReleasesAsWindowProgresses(t *testing.T) {
	// 同样花了 $6，窗口前期被刹车、窗口后期（80% 已过）应放行。
	early := pacingAccount(PacingModeSmart, time.Now().Add(-30*time.Minute), nil)
	late := pacingAccount(PacingModeSmart, time.Now().Add(-4*time.Hour), nil)
	require.Equal(t, WindowCostStickyOnly, early.CheckWindowCostSchedulability(6.0))
	require.Equal(t, WindowCostSchedulable, late.CheckWindowCostSchedulability(6.0))
}

func TestPacing_FeedbackFactor_HalvesAndRecovers(t *testing.T) {
	now := time.Now()
	a := pacingAccount(PacingModeSmart, now.Add(-time.Hour), nil)

	// 无限流历史 → 满速。
	require.Equal(t, 1.0, a.PacingFeedbackFactor(now))

	// 10 分钟前被限流 → 地板 0.5。
	rl := now.Add(-10 * time.Minute)
	a.RateLimitedAt = &rl
	require.Equal(t, 0.5, a.PacingFeedbackFactor(now))

	// 30min hold + 45min（恢复期一半）→ 0.75 左右。
	rl2 := now.Add(-(30 + 45) * time.Minute)
	a.RateLimitedAt = &rl2
	f := a.PacingFeedbackFactor(now)
	require.InDelta(t, 0.75, f, 0.01)

	// 完全恢复（>120min）。
	rl3 := now.Add(-3 * time.Hour)
	a.RateLimitedAt = &rl3
	require.Equal(t, 1.0, a.PacingFeedbackFactor(now))

	// 过载期内直接地板。
	a.RateLimitedAt = nil
	ov := now.Add(10 * time.Minute)
	a.OverloadUntil = &ov
	require.Equal(t, 0.5, a.PacingFeedbackFactor(now))
}

func TestPacing_ModeCoefficients_SteadySmartBurst(t *testing.T) {
	windowStart := time.Now().Add(-2*time.Hour - 30*time.Minute) // 50% elapsed
	steady := pacingAccount(PacingModeSteady, windowStart, nil)
	smart := pacingAccount(PacingModeSmart, windowStart, nil)
	burst := pacingAccount(PacingModeBurst, windowStart, nil)

	now := time.Now()
	fs, fm, fb := steady.WindowBudgetTargetFraction(now), smart.WindowBudgetTargetFraction(now),
		burst.WindowBudgetTargetFraction(now)
	require.Less(t, fs, fm, "steady spends slower than smart")
	require.Less(t, fm, fb, "smart spends slower than burst")
	// smart @50% elapsed ≈ 0.10 + 0.50×1.0 = 0.60
	require.InDelta(t, 0.60, fm, 0.02)
}

func TestPacing_EffectiveRPM_ModeAndFeedback(t *testing.T) {
	now := time.Now()
	windowStart := now.Add(-time.Hour)
	mk := func(mode string) *Account {
		return pacingAccount(mode, windowStart, map[string]any{"base_rpm": 20})
	}

	require.Equal(t, 20, mk("").EffectiveBaseRPM(now), "no mode → base unchanged")
	require.Equal(t, 14, mk(PacingModeSteady).EffectiveBaseRPM(now)) // 20×0.7
	require.Equal(t, 20, mk(PacingModeSmart).EffectiveBaseRPM(now))
	require.Equal(t, 30, mk(PacingModeBurst).EffectiveBaseRPM(now)) // 20×1.5

	// 刚被限流：smart 减半 → 10。
	limited := mk(PacingModeSmart)
	rl := now.Add(-5 * time.Minute)
	limited.RateLimitedAt = &rl
	require.Equal(t, 10, limited.EffectiveBaseRPM(now))
}

func TestPacing_RPMCheck_UsesEffectiveThreshold(t *testing.T) {
	now := time.Now()
	a := pacingAccount(PacingModeSteady, now.Add(-time.Hour), map[string]any{"base_rpm": 20})
	// steady 有效上限 14：13 可调度，14 起进入黄区。
	require.Equal(t, WindowCostSchedulable, a.CheckRPMSchedulability(13))
	require.Equal(t, WindowCostStickyOnly, a.CheckRPMSchedulability(14))
}

func TestPacing_InvalidModeIgnored(t *testing.T) {
	a := pacingAccount("turbo-9000", time.Now().Add(-time.Hour), nil)
	require.Equal(t, "", a.GetPacingMode())
	require.Equal(t, 1.0, a.WindowBudgetTargetFraction(time.Now()))
	require.False(t, IsValidPacingMode("turbo-9000"))
	require.True(t, IsValidPacingMode("smart"))
	require.True(t, IsValidPacingMode("STEADY"))
}
