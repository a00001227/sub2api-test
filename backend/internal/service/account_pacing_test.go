package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Phase 21I DeRouter 五档 pacing 单元测试。
// 关注：
//   1. 未设 pacing_mode 的存量账号行为完全不变（回归安全）。
//   2. 五档容量表照搬 DeRouter 数值（并发/RPM/RPH）。
//   3. 旧档位名（steady/smart/burst）别名映射，存量账号免迁移。
//   4. RPM/RPH/并发由档位权威决定；预算刹车与 AIMD 已删除。

func TestPacing_NoMode_LegacyBehaviorUnchanged(t *testing.T) {
	// 无 pacing 账号：window_cost_limit 硬阈值语义保持（黄区/红区）。
	a := &Account{
		ID: 1, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
		Extra: map[string]any{"window_cost_limit": 10.0},
	}
	require.Equal(t, WindowCostSchedulable, a.CheckWindowCostSchedulability(9.0))
	require.Equal(t, WindowCostStickyOnly, a.CheckWindowCostSchedulability(10.0))
	require.Equal(t, WindowCostNotSchedulable, a.CheckWindowCostSchedulability(20.0))
	require.Equal(t, "", a.GetPacingMode())
}

func TestPacing_FiveModeProfiles_DeRouterNumbers(t *testing.T) {
	// 照搬 DeRouter：并发 / RPM / RPH。
	cases := []struct {
		mode           string
		conc, rpm, rph int
		humanized      bool
	}{
		{PacingModeHumanized, 2, 20, 190, true},
		{PacingModeStandard, 2, 20, 190, false},
		{PacingModeSpeed2x, 4, 40, 380, false},
		{PacingModeSpeed3x, 6, 60, 570, false},
		{PacingModeSpeed5x, 10, 100, 950, false},
	}
	for _, c := range cases {
		a := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
			Extra: map[string]any{"pacing_mode": c.mode}}
		require.Equal(t, c.mode, a.GetPacingMode())
		require.Equal(t, c.conc, a.EffectiveLoadFactor(), "%s concurrency", c.mode)
		require.Equal(t, c.rpm, a.GetBaseRPM(), "%s rpm", c.mode)
		require.Equal(t, c.rph, a.GetBaseRPH(), "%s rph", c.mode)
		require.Equal(t, !c.humanized, pacingIsSpeedMode(c.mode), "%s humanized", c.mode)
	}
}

func TestPacing_LegacyModeAliases(t *testing.T) {
	// 存量账号 extra 里存的是旧名，应映射到新五档，无需迁移。
	mk := func(stored string) *Account {
		return &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
			Extra: map[string]any{"pacing_mode": stored}}
	}
	require.Equal(t, PacingModeHumanized, mk("smart").GetPacingMode())
	require.Equal(t, PacingModeStandard, mk("steady").GetPacingMode())
	require.Equal(t, PacingModeSpeed2x, mk("burst").GetPacingMode())
	// 大小写不敏感。
	require.Equal(t, PacingModeHumanized, mk("SMART").GetPacingMode())
}

func TestPacing_ModeAuthoritative_IgnoresExtraRPM(t *testing.T) {
	// 启用 pacing 时忽略 Extra 里遗留的 base_rpm，用档位权威值。
	a := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
		Extra: map[string]any{"pacing_mode": PacingModeSpeed5x, "base_rpm": 3}}
	require.Equal(t, 100, a.GetBaseRPM(), "档位权威覆盖 Extra base_rpm")
}

func TestPacing_RPMCheck_UsesModeThreshold(t *testing.T) {
	// humanized RPM=20：19 可调度，20 起进入黄区。
	a := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
		Extra: map[string]any{"pacing_mode": PacingModeHumanized}}
	require.Equal(t, WindowCostSchedulable, a.CheckRPMSchedulability(19))
	require.Equal(t, WindowCostStickyOnly, a.CheckRPMSchedulability(20))
}

func TestPacing_BudgetBrakeRemoved(t *testing.T) {
	// 预算软刹车已删除：启用 pacing 的账号在 window_cost_limit 之下永远可调度，
	// 不再因"烧太快"降为 StickyOnly。
	a := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
		Extra: map[string]any{"pacing_mode": PacingModeHumanized, "window_cost_limit": 10.0}}
	require.Equal(t, WindowCostSchedulable, a.CheckWindowCostSchedulability(9.99))
}

func TestPacing_InvalidModeIgnored(t *testing.T) {
	a := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
		Extra: map[string]any{"pacing_mode": "turbo-9000"}}
	require.Equal(t, "", a.GetPacingMode())
	require.False(t, IsValidPacingMode("turbo-9000"))
	require.True(t, IsValidPacingMode("humanized"))
	require.True(t, IsValidPacingMode("smart")) // 别名仍有效
	require.True(t, IsValidPacingMode("SPEED_5X"))
}

func TestPacing_TierProfiles_GradedByPlan(t *testing.T) {
	max20 := PacingProfileForTier("default_claude_max_20x")
	max5 := PacingProfileForTier("default_claude_max_5x")
	pro := PacingProfileForTier("claude_pro")

	// 分级递减：max_20x > max_5x > pro（预算、速率、容量全部）。
	require.Greater(t, max20.WindowCostLimit, max5.WindowCostLimit)
	require.Greater(t, max5.WindowCostLimit, pro.WindowCostLimit)
	require.Greater(t, max20.BaseRPM, max5.BaseRPM)
	require.Greater(t, max20.Concurrency, pro.Concurrency)
	require.Greater(t, max20.MaxSessions, pro.MaxSessions)

	require.Equal(t, 35.0, max20.WindowCostLimit)
	require.Equal(t, 3, max20.Concurrency)
	require.Equal(t, 5, max20.MaxSessions)
}

func TestPacing_TierProfiles_UnknownFallsBackConservative(t *testing.T) {
	unknown := PacingProfileForTier("some_future_tier")
	empty := PacingProfileForTier("")
	require.Equal(t, unknown, empty)
	// 保守默认低于 max_20x、不为零。
	require.Less(t, unknown.WindowCostLimit, PacingProfileForTier("max_20x").WindowCostLimit)
	require.Greater(t, unknown.WindowCostLimit, 0.0)
	require.Greater(t, unknown.Concurrency, 0)
}

func TestPacing_TierProfiles_TolerantMatching(t *testing.T) {
	// 前缀变体、大小写、子串都能命中。
	require.Equal(t, PacingProfileForTier("max_20x"), PacingProfileForTier("DEFAULT_CLAUDE_MAX_20X"))
	require.Equal(t, PacingProfileForTier("pro"), PacingProfileForTier("claude_pro"))
	require.Equal(t, PacingProfileForTier("max_5x"), PacingProfileForTier("team_max_5x_extra"))
}

// ── Phase 21I: 利用率驱动主动休眠 ──────────────────────────────────────

func utilAccount(mode string, extra map[string]any) *Account {
	e := map[string]any{}
	if mode != "" {
		e["pacing_mode"] = mode
	}
	for k, v := range extra {
		e[k] = v
	}
	return &Account{
		ID:          1,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth, // Anthropic OAuth → 有 unified 利用率头
		Status:      "active",
		Schedulable: true,
		Extra:       e,
	}
}

func TestPacing_Util5hBelowThreshold_NotDormant(t *testing.T) {
	a := utilAccount("smart", map[string]any{"session_window_utilization": 0.89})
	require.False(t, a.IsUtilizationDormant())
}

func TestPacing_Util5hAtThreshold_Dormant(t *testing.T) {
	a := utilAccount("smart", map[string]any{"session_window_utilization": 0.90})
	require.True(t, a.IsUtilizationDormant())
	require.False(t, a.IsSchedulable(), "利用率达阈值应退出调度")
}

func TestPacing_Util7dAtThreshold_Dormant(t *testing.T) {
	future := time.Now().Add(3 * 24 * time.Hour).Unix()
	a := utilAccount("smart", map[string]any{
		"passive_usage_7d_utilization": 0.95,
		"passive_usage_7d_reset":       float64(future),
	})
	require.True(t, a.IsUtilizationDormant())
}

func TestPacing_Util7dExpiredWindow_NotDormant(t *testing.T) {
	past := time.Now().Add(-1 * time.Hour).Unix()
	a := utilAccount("smart", map[string]any{
		"passive_usage_7d_utilization": 0.99, // 陈旧数据
		"passive_usage_7d_reset":       float64(past),
	})
	require.False(t, a.IsUtilizationDormant(), "7d 窗口已过期，陈旧利用率不应触发休眠")
}

func TestPacing_NoPacingMode_UtilizationIgnored(t *testing.T) {
	// admin 账号（无 pacing_mode）：即便利用率满，也不因 util 主动休眠（旧行为）。
	a := utilAccount("", map[string]any{"session_window_utilization": 0.99})
	require.True(t, a.IsSchedulable(), "无 pacing 的账号不受利用率休眠影响")
}

func TestPacing_NonOAuth_UtilizationIgnored(t *testing.T) {
	// 非 Anthropic OAuth（如 API key）没有 unified 利用率头，不参与休眠判定。
	a := utilAccount("smart", map[string]any{"session_window_utilization": 0.99})
	a.Type = AccountTypeAPIKey
	require.False(t, a.IsUtilizationDormant())
}

// ── Phase 21I: RPH 每小时闸 ────────────────────────────────────────────

func TestPacing_RPH_Disabled_AlwaysSchedulable(t *testing.T) {
	// 无 pacing 且 Extra 无 base_rph → RPH 闸未启用，恒可调度。
	a := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
		Extra: map[string]any{}}
	require.Equal(t, 0, a.GetBaseRPH())
	require.Equal(t, WindowCostSchedulable, a.CheckRPHSchedulability(9999))
}

func TestPacing_RPH_ThreeZones_ModeAuthoritative(t *testing.T) {
	// speed_2x → RPH=380，buffer = 38。
	a := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
		Extra: map[string]any{"pacing_mode": PacingModeSpeed2x}}
	require.Equal(t, 380, a.GetBaseRPH())
	require.Equal(t, WindowCostSchedulable, a.CheckRPHSchedulability(379))
	require.Equal(t, WindowCostStickyOnly, a.CheckRPHSchedulability(380))     // 黄区
	require.Equal(t, WindowCostStickyOnly, a.CheckRPHSchedulability(417))     // 黄区内 (380+37)
	require.Equal(t, WindowCostNotSchedulable, a.CheckRPHSchedulability(418)) // 红区 (>=380+38)
}

func TestPacing_ModeProfiles_RPHMonotonic(t *testing.T) {
	// RPH 随速度档单调递增（照搬 DeRouter）。
	mk := func(mode string) int {
		a := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
			Extra: map[string]any{"pacing_mode": mode}}
		return a.GetBaseRPH()
	}
	require.Equal(t, 190, mk(PacingModeHumanized))
	require.Less(t, mk(PacingModeStandard), mk(PacingModeSpeed2x))
	require.Less(t, mk(PacingModeSpeed2x), mk(PacingModeSpeed3x))
	require.Less(t, mk(PacingModeSpeed3x), mk(PacingModeSpeed5x))
	require.Equal(t, 950, mk(PacingModeSpeed5x))
}

// ── Phase 21I: 配额降权选择权重 ────────────────────────────────────────

func TestPacing_SelectionWeight_Normal(t *testing.T) {
	a := utilAccount("smart", map[string]any{"session_window_utilization": 0.30})
	require.Equal(t, 1.0, a.PacingSelectionWeight())
}

func TestPacing_SelectionWeight_DownweightBand(t *testing.T) {
	a := utilAccount("smart", map[string]any{"session_window_utilization": 0.60})
	require.Equal(t, 0.5, a.PacingSelectionWeight(), "util∈[0.5,0.9) 应降权 0.5")
	b := utilAccount("smart", map[string]any{"session_window_utilization": 0.89})
	require.Equal(t, 0.5, b.PacingSelectionWeight())
}

func TestPacing_SelectionWeight_DormantBandStillReportsWeight(t *testing.T) {
	// util>=0.9 的排除由 IsUtilizationDormant 处理，权重函数此时回落到 1.0
	// （不在降权带内），避免与休眠逻辑重复作用。
	a := utilAccount("smart", map[string]any{"session_window_utilization": 0.95})
	require.Equal(t, 1.0, a.PacingSelectionWeight())
	require.True(t, a.IsUtilizationDormant())
}

func TestPacing_SelectionWeight_NoPacingOrNonOAuth(t *testing.T) {
	a := utilAccount("", map[string]any{"session_window_utilization": 0.60})
	require.Equal(t, 1.0, a.PacingSelectionWeight(), "无 pacing 不降权")
	b := utilAccount("smart", map[string]any{"session_window_utilization": 0.60})
	b.Type = AccountTypeAPIKey
	require.Equal(t, 1.0, b.PacingSelectionWeight(), "非 OAuth 无利用率头，不降权")
}

// TestPacing_WeightedShuffle_DownweightLandsLater 统计降权账号在加权洗牌后
// 排在首位的频率应显著低于正常账号（约一半量级），且仍有机会靠前（非排除）。
func TestPacing_WeightedShuffle_DownweightLandsLater(t *testing.T) {
	normal := utilAccount("smart", map[string]any{"session_window_utilization": 0.10}) // weight 1.0
	normal.ID = 1
	down := utilAccount("smart", map[string]any{"session_window_utilization": 0.70}) // weight 0.5
	down.ID = 2

	const runs = 20000
	normalFirst, downFirst := 0, 0
	for i := 0; i < runs; i++ {
		group := []accountWithLoad{
			{account: normal, loadInfo: &AccountLoadInfo{}},
			{account: down, loadInfo: &AccountLoadInfo{}},
		}
		weightedShuffleByPacing(group)
		switch group[0].account.ID {
		case 1:
			normalFirst++
		case 2:
			downFirst++
		}
	}
	// 权重 1.0 vs 0.5：正常账号排首位的概率应明显更高。
	require.Greater(t, normalFirst, downFirst,
		"正常账号应比降权账号更常排首位 (normal=%d down=%d)", normalFirst, downFirst)
	// 降权账号仍应有可观机会靠前（不是被完全排除），保守下界 15%。
	require.Greater(t, downFirst, runs*15/100,
		"降权≠排除，降权账号仍应有机会 (down=%d)", downFirst)
}

// TestPacing_WeightedShuffle_EqualWeightsUnbiased 权重全相等时应退化为等概率洗牌。
func TestPacing_WeightedShuffle_EqualWeightsUnbiased(t *testing.T) {
	a := utilAccount("smart", map[string]any{"session_window_utilization": 0.10})
	a.ID = 1
	b := utilAccount("smart", map[string]any{"session_window_utilization": 0.10})
	b.ID = 2

	const runs = 20000
	aFirst := 0
	for i := 0; i < runs; i++ {
		group := []accountWithLoad{
			{account: a, loadInfo: &AccountLoadInfo{}},
			{account: b, loadInfo: &AccountLoadInfo{}},
		}
		weightedShuffleByPacing(group)
		if group[0].account.ID == 1 {
			aFirst++
		}
	}
	// 约 50%，允许 ±5% 抖动。
	require.InDelta(t, runs/2, aFirst, float64(runs)*0.05)
}

// ── Phase 21I: 拟人化（每日休息 + 活跃-冷却）──────────────────────────

func humanAccount(id int64, mode string) *Account {
	return &Account{
		ID:       id,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"pacing_mode": mode},
	}
}

func TestPacing_DailyRest_WindowIsFourHours(t *testing.T) {
	a := humanAccount(7, PacingModeHumanized)
	start, end := a.DailyRestWindowUTC()
	require.Equal(t, pacingDailyRestDurationMin, end-start)
	require.GreaterOrEqual(t, start, 0)
	require.Less(t, start, pacingDayMinutes)
}

func TestPacing_DailyRest_HitAndMiss(t *testing.T) {
	a := humanAccount(7, PacingModeHumanized)
	start, _ := a.DailyRestWindowUTC()
	// 构造一个落在休息窗口内的 UTC 时刻（start+30 分钟）。
	inMin := (start + 30) % pacingDayMinutes
	inside := time.Date(2026, 1, 1, inMin/60, inMin%60, 0, 0, time.UTC)
	require.True(t, a.IsInDailyRestWindow(inside))
	// 窗口外：start 之前 30 分钟。
	outMin := (start - 30 + pacingDayMinutes) % pacingDayMinutes
	outside := time.Date(2026, 1, 1, outMin/60, outMin%60, 0, 0, time.UTC)
	require.False(t, a.IsInDailyRestWindow(outside))
}

func TestPacing_DailyRest_SpreadAcrossPool(t *testing.T) {
	// 全池打散：1000 个账号的休息起点应铺满一天的多数小时段，
	// 不应集中在少数几个小时（否则会造成容量断崖）。
	buckets := map[int]int{}
	for id := int64(1); id <= 1000; id++ {
		a := humanAccount(id, PacingModeHumanized)
		start, _ := a.DailyRestWindowUTC()
		buckets[start/60]++ // 按小时统计
	}
	require.GreaterOrEqual(t, len(buckets), 20, "休息起点应覆盖至少 20 个不同小时段")
}

func TestPacing_DailyRest_SpeedModeSkips(t *testing.T) {
	a := humanAccount(7, PacingModeSpeed2x) // 速度档
	start, _ := a.DailyRestWindowUTC()
	inMin := (start + 30) % pacingDayMinutes
	inside := time.Date(2026, 1, 1, inMin/60, inMin%60, 0, 0, time.UTC)
	require.False(t, a.IsInDailyRestWindow(inside), "速度档应跳过每日休息")
}

func TestPacing_Cooldown_CyclesAndSpeedModeSkips(t *testing.T) {
	a := humanAccount(0, PacingModeHumanized) // offset 0，便于推算
	// minuteOfEpoch % 60：前 50 分钟活跃、后 10 分钟冷却。
	// 找一个冷却分钟与一个活跃分钟。
	activeFound, coolFound := false, false
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for m := 0; m < pacingCycleMin; m++ {
		ts := base.Add(time.Duration(m) * time.Minute)
		if a.IsInCooldownPhase(ts) {
			coolFound = true
		} else {
			activeFound = true
		}
	}
	require.True(t, activeFound && coolFound, "一个周期内应同时出现活跃段与冷却段")

	// 速度档跳过冷却。
	sp := humanAccount(0, PacingModeSpeed2x)
	anyCool := false
	for m := 0; m < pacingCycleMin; m++ {
		if sp.IsInCooldownPhase(base.Add(time.Duration(m) * time.Minute)) {
			anyCool = true
		}
	}
	require.False(t, anyCool, "速度档应跳过活跃-冷却节奏")
}

func TestPacing_Humanization_NoPacingModeNeverDormant(t *testing.T) {
	a := humanAccount(7, "") // 无 pacing（admin 账号）
	// 无论何时都不受拟人化影响。
	for h := 0; h < 24; h++ {
		ts := time.Date(2026, 1, 1, h, 0, 0, 0, time.UTC)
		require.False(t, a.IsHumanizedDormant(ts))
	}
}

// ── Phase 21I: DeRouter 面板对齐（下次休息窗口标签）──────────────────────

func TestPacing_DailyRestLabels_HumanizedOnly(t *testing.T) {
	h := &Account{ID: 7, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
		Extra: map[string]any{"pacing_mode": PacingModeHumanized}}
	start, end := h.DailyRestWindowLabels()
	// "HH:MM" 形式，非空。
	require.Regexp(t, `^\d{2}:\d{2}$`, start)
	require.Regexp(t, `^\d{2}:\d{2}$`, end)

	// 速度档无休息窗口 → 空串。
	sp := &Account{ID: 7, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
		Extra: map[string]any{"pacing_mode": PacingModeSpeed5x}}
	s2, e2 := sp.DailyRestWindowLabels()
	require.Equal(t, "", s2)
	require.Equal(t, "", e2)
}

func TestPacing_DailyRestLabels_MatchWindowMinutes(t *testing.T) {
	a := &Account{ID: 3, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
		Extra: map[string]any{"pacing_mode": PacingModeHumanized}}
	startMin, endMin := a.DailyRestWindowUTC()
	start, end := a.DailyRestWindowLabels()
	require.Equal(t, sprintfHHMM(startMin/60, startMin%60), start)
	require.Equal(t, sprintfHHMM((endMin%pacingDayMinutes)/60, (endMin%pacingDayMinutes)%60), end)
}
