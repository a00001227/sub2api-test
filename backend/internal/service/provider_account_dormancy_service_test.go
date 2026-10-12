package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Portal「唤醒」休眠账号:ProviderAccountDormancyService 单测(全假件)。

type fakeDormancyRepo struct {
	acc     *Account
	getErr  error
	updates map[string]any
	updErr  error
}

func (f *fakeDormancyRepo) GetByID(_ context.Context, _ int64) (*Account, error) {
	return f.acc, f.getErr
}

func (f *fakeDormancyRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	f.updates = updates
	return f.updErr
}

func dormancyTestAccount(extra map[string]any, windowEnd *time.Time) *Account {
	e := map[string]any{"pacing_mode": "standard"}
	for k, v := range extra {
		e[k] = v
	}
	return &Account{
		ID:               7,
		Platform:         PlatformAnthropic,
		Type:             AccountTypeOAuth,
		Status:           "active",
		Schedulable:      true,
		Extra:            e,
		SessionWindowEnd: windowEnd,
	}
}

func newDormancySvc(repo *fakeDormancyRepo, now time.Time) *ProviderAccountDormancyService {
	svc := NewProviderAccountDormancyService(&fakeSchedulingLocator{id: 7, found: true}, repo)
	svc.now = func() time.Time { return now }
	return svc
}

func TestDormancy_Wake7dDefaultsToWindowReset(t *testing.T) {
	now := time.Now()
	reset := now.Add(3 * 24 * time.Hour).Truncate(time.Second)
	repo := &fakeDormancyRepo{acc: dormancyTestAccount(map[string]any{
		"passive_usage_7d_utilization": 0.76,
		"passive_usage_7d_reset":       float64(reset.Unix()),
	}, nil)}
	res, err := newDormancySvc(repo, now).SetDormancyOverride(context.Background(), "pa_x", "wake", 0)
	require.NoError(t, err)
	require.Equal(t, "updated", res.Status)
	require.True(t, res.Dormant7d)
	require.False(t, res.Dormant5h)
	require.Equal(t, reset.Unix(), res.OverrideUnix, "默认唤醒到 7d 重置时间")
	require.Equal(t, reset.Unix(), repo.updates[ExtraKeyUtilizationDormancyOverrideUntil])
}

func TestDormancy_Wake5hDefaultsToSessionWindowEnd(t *testing.T) {
	now := time.Now()
	end := now.Add(2 * time.Hour).Truncate(time.Second)
	repo := &fakeDormancyRepo{acc: dormancyTestAccount(map[string]any{
		"session_window_utilization": 0.9,
	}, &end)}
	res, err := newDormancySvc(repo, now).SetDormancyOverride(context.Background(), "pa_x", "wake", 0)
	require.NoError(t, err)
	require.Equal(t, "updated", res.Status)
	require.True(t, res.Dormant5h)
	require.Equal(t, end.Unix(), res.OverrideUnix, "默认唤醒到 5h 窗口结束")
}

func TestDormancy_WakeBothWindowsTakesLater(t *testing.T) {
	now := time.Now()
	end := now.Add(2 * time.Hour).Truncate(time.Second)
	reset := now.Add(3 * 24 * time.Hour).Truncate(time.Second)
	repo := &fakeDormancyRepo{acc: dormancyTestAccount(map[string]any{
		"session_window_utilization":   0.9,
		"passive_usage_7d_utilization": 0.8,
		"passive_usage_7d_reset":       float64(reset.Unix()),
	}, &end)}
	res, err := newDormancySvc(repo, now).SetDormancyOverride(context.Background(), "pa_x", "wake", 0)
	require.NoError(t, err)
	require.True(t, res.Dormant5h)
	require.True(t, res.Dormant7d)
	require.Equal(t, reset.Unix(), res.OverrideUnix, "两窗口都休眠取较晚的重置时间")
}

func TestDormancy_WakeNotDormantIsNoop(t *testing.T) {
	now := time.Now()
	repo := &fakeDormancyRepo{acc: dormancyTestAccount(map[string]any{
		"passive_usage_7d_utilization": 0.3,
	}, nil)}
	res, err := newDormancySvc(repo, now).SetDormancyOverride(context.Background(), "pa_x", "wake", 0)
	require.NoError(t, err)
	require.Equal(t, "not_dormant", res.Status)
	require.Nil(t, repo.updates, "未休眠且无显式 until 不写库")
}

func TestDormancy_WakeExpired7dWindowIsNotDormant(t *testing.T) {
	now := time.Now()
	repo := &fakeDormancyRepo{acc: dormancyTestAccount(map[string]any{
		"passive_usage_7d_utilization": 0.99,
		"passive_usage_7d_reset":       float64(now.Add(-time.Hour).Unix()),
	}, nil)}
	res, err := newDormancySvc(repo, now).SetDormancyOverride(context.Background(), "pa_x", "wake", 0)
	require.NoError(t, err)
	require.Equal(t, "not_dormant", res.Status, "7d 窗口已过期的陈旧值不算休眠")
}

func TestDormancy_WakeNoResetKnownFallsBack5h(t *testing.T) {
	now := time.Now()
	repo := &fakeDormancyRepo{acc: dormancyTestAccount(map[string]any{
		"passive_usage_7d_utilization": 0.8, // 无 reset 键
	}, nil)}
	res, err := newDormancySvc(repo, now).SetDormancyOverride(context.Background(), "pa_x", "wake", 0)
	require.NoError(t, err)
	require.Equal(t, "updated", res.Status)
	require.Equal(t, now.Add(dormancyOverrideFallbackDuration).Unix(), res.OverrideUnix)
}

func TestDormancy_WakeExplicitUntil(t *testing.T) {
	now := time.Now()
	until := now.Add(26 * time.Hour).Truncate(time.Second)
	repo := &fakeDormancyRepo{acc: dormancyTestAccount(nil, nil)} // 未休眠也允许显式预设
	res, err := newDormancySvc(repo, now).SetDormancyOverride(context.Background(), "pa_x", "wake", until.Unix())
	require.NoError(t, err)
	require.Equal(t, "updated", res.Status)
	require.Equal(t, until.Unix(), res.OverrideUnix)
}

func TestDormancy_WakeExplicitUntilOutOfRangeRejected(t *testing.T) {
	now := time.Now()
	repo := &fakeDormancyRepo{acc: dormancyTestAccount(nil, nil)}
	svc := newDormancySvc(repo, now)
	_, err := svc.SetDormancyOverride(context.Background(), "pa_x", "wake", now.Add(-time.Minute).Unix())
	require.ErrorIs(t, err, ErrInvalidDormancyUntil, "过去时间")
	_, err = svc.SetDormancyOverride(context.Background(), "pa_x", "wake", now.Add(8*24*time.Hour).Unix())
	require.ErrorIs(t, err, ErrInvalidDormancyUntil, "超过 7 天")
	require.Nil(t, repo.updates)
}

func TestDormancy_ClearWritesNull(t *testing.T) {
	now := time.Now()
	repo := &fakeDormancyRepo{acc: dormancyTestAccount(map[string]any{
		ExtraKeyUtilizationDormancyOverrideUntil: float64(now.Add(time.Hour).Unix()),
	}, nil)}
	res, err := newDormancySvc(repo, now).SetDormancyOverride(context.Background(), "pa_x", "clear", 0)
	require.NoError(t, err)
	require.Equal(t, "cleared", res.Status)
	v, ok := repo.updates[ExtraKeyUtilizationDormancyOverrideUntil]
	require.True(t, ok)
	require.Nil(t, v, "clear 写 JSON null 覆盖")
}

func TestDormancy_InvalidActionAndRef(t *testing.T) {
	now := time.Now()
	svc := newDormancySvc(&fakeDormancyRepo{acc: dormancyTestAccount(nil, nil)}, now)
	_, err := svc.SetDormancyOverride(context.Background(), "pa_x", "sleep", 0)
	require.ErrorIs(t, err, ErrInvalidDormancyAction)
	_, err = svc.SetDormancyOverride(context.Background(), "bad_ref", "wake", 0)
	require.Error(t, err)
}

func TestDormancy_UnknownRefNotFound(t *testing.T) {
	svc := NewProviderAccountDormancyService(&fakeSchedulingLocator{found: false}, &fakeDormancyRepo{})
	res, err := svc.SetDormancyOverride(context.Background(), "pa_x", "wake", 0)
	require.NoError(t, err)
	require.Equal(t, "not_found", res.Status)
}
