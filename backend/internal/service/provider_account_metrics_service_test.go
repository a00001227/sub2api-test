package service

import (
	"context"
	"errors"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

// Phase 21E-6E account-metrics: ProviderAccountMetricsService 单元测试（全假件）。

type fakeMetricsLocator struct {
	id    int64
	found bool
	err   error
}

func (f *fakeMetricsLocator) FindAccountIDByExternalRef(_ context.Context, _ string) (int64, bool, error) {
	return f.id, f.found, f.err
}

type fakeMetricsAccountReader struct {
	acc *Account
	err error
}

func (f *fakeMetricsAccountReader) GetByID(_ context.Context, _ int64) (*Account, error) {
	return f.acc, f.err
}

type fakeMetricsUsageReader struct {
	usage    *UsageInfo
	usageErr error
	today    *WindowStats
	todayErr error
	hour     *usagestats.AccountStats
	hourErr  error
}

func (f *fakeMetricsUsageReader) GetPassiveUsage(_ context.Context, _ int64) (*UsageInfo, error) {
	return f.usage, f.usageErr
}
func (f *fakeMetricsUsageReader) GetTodayStats(_ context.Context, _ int64) (*WindowStats, error) {
	return f.today, f.todayErr
}
func (f *fakeMetricsUsageReader) GetAccountWindowStats(_ context.Context, _ int64, _ time.Time) (*usagestats.AccountStats, error) {
	return f.hour, f.hourErr
}

func newMetricsSvc(loc *fakeMetricsLocator, acc *fakeMetricsAccountReader, usage *fakeMetricsUsageReader) *ProviderAccountMetricsService {
	return &ProviderAccountMetricsService{locator: loc, accounts: acc, usage: usage, now: time.Now}
}

type fakeMetricsConcurrency struct{ used int }

func (f *fakeMetricsConcurrency) GetAccountConcurrencyBatch(_ context.Context, ids []int64) (map[int64]int, error) {
	m := map[int64]int{}
	for _, id := range ids {
		m[id] = f.used
	}
	return m, nil
}

type fakeMetricsRPM struct{ used int }

func (f *fakeMetricsRPM) GetRPM(_ context.Context, _ int64) (int, error) { return f.used, nil }

// 成功：组装状态/并发/用量窗口/配额/订阅等级/今日请求（全脱敏）。
func TestMetrics_Success(t *testing.T) {
	reset := time.Date(2026, 7, 18, 19, 0, 0, 0, time.UTC)
	svc := newMetricsSvc(
		&fakeMetricsLocator{id: 5, found: true},
		&fakeMetricsAccountReader{acc: &Account{Status: StatusActive, Concurrency: 2}},
		&fakeMetricsUsageReader{
			usage: &UsageInfo{
				SubscriptionTier: "PRO",
				FiveHour:         &UsageProgress{Utilization: 97, ResetsAt: &reset, RemainingSeconds: 3600},
				SevenDay:         &UsageProgress{Utilization: 94, RemainingSeconds: 449400},
			},
			today: &WindowStats{Requests: 13, Tokens: 12345},
		},
	)

	m, err := svc.Metrics(context.Background(), "pa_abc")
	require.NoError(t, err)
	require.Equal(t, StatusActive, m.Status)
	require.Equal(t, 2, m.Concurrency)
	require.Equal(t, "PRO", m.SubscriptionTier)
	require.NotNil(t, m.FiveHour)
	require.Equal(t, float64(97), m.FiveHour.Utilization)
	require.NotNil(t, m.FiveHour.ResetsAt)
	require.Equal(t, "2026-07-18T19:00:00Z", *m.FiveHour.ResetsAt)
	require.Equal(t, float64(94), m.SevenDay.Utilization)
	require.Nil(t, m.SevenDay.ResetsAt, "no reset time → nil")
	require.Equal(t, int64(13), m.TodayRequests)
	require.Equal(t, int64(12345), m.TodayTokens)
}

// 并发/RPM/RPH：当前占用来自 concurrency/rpm reader + past-hour usage，上限来自 account。
func TestMetrics_ConcurrencyAndRPM(t *testing.T) {
	svc := &ProviderAccountMetricsService{
		locator:     &fakeMetricsLocator{id: 5, found: true},
		accounts:    &fakeMetricsAccountReader{acc: &Account{Status: StatusActive, Concurrency: 2, Extra: map[string]any{"base_rpm": 20}}},
		usage:       &fakeMetricsUsageReader{hour: &usagestats.AccountStats{Requests: 42}},
		concurrency: &fakeMetricsConcurrency{used: 1},
		rpm:         &fakeMetricsRPM{used: 7},
		now:         time.Now,
	}
	m, err := svc.Metrics(context.Background(), "pa_abc")
	require.NoError(t, err)
	require.Equal(t, 2, m.ConcurrencyMax)
	require.Equal(t, 1, m.ConcurrencyUsed)
	require.Equal(t, 20, m.RPMLimit)
	require.Equal(t, 7, m.RPMUsed)
	require.Equal(t, 42, m.RPHUsed)
}

// 模型数：具体映射 → key 数；含通配 "*" → 0（表示“全部”）。
func TestMetrics_ModelCount(t *testing.T) {
	specific := &fakeMetricsAccountReader{acc: &Account{
		Status:      StatusActive,
		Credentials: map[string]any{"model_mapping": map[string]any{"claude-3-opus": "x", "claude-3-sonnet": "y"}},
	}}
	svc := newMetricsSvc(&fakeMetricsLocator{id: 5, found: true}, specific, &fakeMetricsUsageReader{})
	m, err := svc.Metrics(context.Background(), "pa_abc")
	require.NoError(t, err)
	require.Equal(t, 2, m.ModelCount)

	wildcard := &fakeMetricsAccountReader{acc: &Account{
		Status:      StatusActive,
		Credentials: map[string]any{"model_mapping": map[string]any{"*": "all"}},
	}}
	svc2 := newMetricsSvc(&fakeMetricsLocator{id: 5, found: true}, wildcard, &fakeMetricsUsageReader{})
	m2, err := svc2.Metrics(context.Background(), "pa_abc")
	require.NoError(t, err)
	require.Equal(t, 0, m2.ModelCount)
}

// 归属引用无账号 → PROVIDER_ACCOUNT_NOT_FOUND。
func TestMetrics_NotFound(t *testing.T) {
	svc := newMetricsSvc(&fakeMetricsLocator{found: false}, &fakeMetricsAccountReader{}, &fakeMetricsUsageReader{})
	m, err := svc.Metrics(context.Background(), "pa_missing")
	require.Nil(t, m)
	var appErr *infraerrors.ApplicationError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, "PROVIDER_ACCOUNT_NOT_FOUND", appErr.Reason)
}

// usage 取失败 → 仍返回基础 status/concurrency（best-effort 降级，不整体失败）。
func TestMetrics_UsageFailureDegrades(t *testing.T) {
	svc := newMetricsSvc(
		&fakeMetricsLocator{id: 5, found: true},
		&fakeMetricsAccountReader{acc: &Account{Status: StatusActive, Concurrency: 3}},
		&fakeMetricsUsageReader{usageErr: errors.New("upstream down"), todayErr: errors.New("db blip")},
	)
	m, err := svc.Metrics(context.Background(), "pa_abc")
	require.NoError(t, err)
	require.Equal(t, StatusActive, m.Status)
	require.Equal(t, 3, m.Concurrency)
	require.Empty(t, m.SubscriptionTier)
	require.Nil(t, m.FiveHour)
	require.Equal(t, int64(0), m.TodayRequests)
}
