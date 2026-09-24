package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// 全局统计在测试间隔离:换一个新实例并在结束时还原。
func withFreshRecentFailures(t *testing.T, now func() time.Time) *AccountRecentFailureStats {
	t.Helper()
	prev := accountRecentFailures
	fresh := &AccountRecentFailureStats{now: now}
	accountRecentFailures = fresh
	t.Cleanup(func() { accountRecentFailures = prev })
	return fresh
}

func TestRecentFailure_ReportDecayAndWeight(t *testing.T) {
	clock := time.Date(2026, 9, 24, 18, 0, 0, 0, time.UTC)
	s := withFreshRecentFailures(t, func() time.Time { return clock })

	_, ok := s.ErrorRate(7)
	require.False(t, ok, "无样本")
	require.Equal(t, 1.0, s.SelectionWeight(7))

	// 连续 5 次失败:EWMA 逼近 1,权重逼近 0.25
	for i := 0; i < 5; i++ {
		s.Report(7, true)
	}
	rate, ok := s.ErrorRate(7)
	require.True(t, ok)
	require.InDelta(t, 0.67, rate, 0.02)
	require.InDelta(t, 1-0.75*rate, s.SelectionWeight(7), 1e-9)
	require.GreaterOrEqual(t, s.SelectionWeight(7), 0.25)

	// 成功样本拉回
	for i := 0; i < 5; i++ {
		s.Report(7, false)
	}
	after, _ := s.ErrorRate(7)
	require.Less(t, after, rate)

	// 时间衰减:5 分钟半衰,30 分钟后低于噪声阈值 → 视为无样本、权重回 1
	for i := 0; i < 10; i++ {
		s.Report(8, true)
	}
	clock = clock.Add(5 * time.Minute)
	r5, _ := s.ErrorRate(8)
	require.InDelta(t, 0.45, r5, 0.05)
	clock = clock.Add(25 * time.Minute)
	_, ok = s.ErrorRate(8)
	require.False(t, ok)
	require.Equal(t, 1.0, s.SelectionWeight(8))
}

func TestRecentFailure_AccountLevelStatusOnly(t *testing.T) {
	for _, st := range []int{401, 403, 429, 529, 500, 502, 503, 504, 522} {
		require.True(t, isAccountLevelUpstreamStatus(st), "%d", st)
	}
	for _, st := range []int{400, 404, 413, 422, 200} {
		require.False(t, isAccountLevelUpstreamStatus(st), "%d", st)
	}
}

// HandleUpstreamError 是失败样本的入口:5xx 记入,400 不记。
func TestRecentFailure_HandleUpstreamErrorReports(t *testing.T) {
	s := withFreshRecentFailures(t, nil)
	rls := NewRateLimitService(&openAIOverloadRepoStub{}, nil, &config.Config{}, nil, nil)
	acc := &Account{ID: 31, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Status: StatusActive}
	rls.HandleUpstreamError(context.Background(), acc, http.StatusBadGateway, http.Header{}, []byte(`{"error":{"message":"bad gateway"}}`))
	_, ok := s.ErrorRate(31)
	require.True(t, ok, "5xx 应计入近期故障")

	acc2 := &Account{ID: 32, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Status: StatusActive}
	rls.HandleUpstreamError(context.Background(), acc2, http.StatusBadRequest, http.Header{}, []byte(`{"error":{"message":"invalid"}}`))
	_, ok = s.ErrorRate(32)
	require.False(t, ok, "400 是请求侧问题,不计入")
}

// 刚报过错的号在同一排序组内应明显更少排首位,但不会被排除。
func TestRecentFailure_WeightedShufflePenalizesFailingAccount(t *testing.T) {
	s := withFreshRecentFailures(t, nil)
	healthy := utilAccount("smart", map[string]any{"session_window_utilization": 0.10})
	healthy.ID = 101
	failing := utilAccount("smart", map[string]any{"session_window_utilization": 0.10})
	failing.ID = 102
	for i := 0; i < 8; i++ {
		s.Report(failing.ID, true)
	}
	require.Less(t, s.SelectionWeight(failing.ID), 0.4)

	const runs = 20000
	healthyFirst, failingFirst := 0, 0
	for i := 0; i < runs; i++ {
		group := []accountWithLoad{
			{account: healthy, loadInfo: &AccountLoadInfo{}},
			{account: failing, loadInfo: &AccountLoadInfo{}},
		}
		weightedShuffleByPacing(group)
		if group[0].account.ID == healthy.ID {
			healthyFirst++
		} else {
			failingFirst++
		}
	}
	require.Greater(t, healthyFirst, failingFirst*2, "健康号应远多于故障号排首位 (healthy=%d failing=%d)", healthyFirst, failingFirst)
	require.Greater(t, failingFirst, runs*10/100, "降权≠排除 (failing=%d)", failingFirst)
}

// 面板评分:有近期故障样本时成功率项改用 1-近期故障率,分数立刻下降。
func TestPacingScore_UsesRecentErrorRateWhenPresent(t *testing.T) {
	lifetime := 0.99
	base := &ProviderAccountMetrics{SuccessRate: &lifetime}
	before := computePacingScore(base)
	require.NotNil(t, before)

	recent := 0.5
	penalized := &ProviderAccountMetrics{SuccessRate: &lifetime, RecentErrorRate: &recent}
	after := computePacingScore(penalized)
	require.InDelta(t, 10.0, after.SuccessPart, 1e-9, "成功率项 = (1-0.5)×20")
	require.Less(t, after.Total, before.Total)
	require.InDelta(t, before.Total-after.Total, 20*(0.99-0.5), 1e-9)
}
