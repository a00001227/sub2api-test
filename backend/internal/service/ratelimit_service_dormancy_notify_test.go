package service

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// dormancyNotifyRepo 只实现 UpdateSessionWindow 会触达的方法 + 调度快照通知。
type dormancyNotifyRepo struct {
	AccountRepository
	notified []int64
}

func (r *dormancyNotifyRepo) UpdateExtra(context.Context, int64, map[string]any) error { return nil }
func (r *dormancyNotifyRepo) UpdateSessionWindow(context.Context, int64, *time.Time, *time.Time, string) error {
	return nil
}
func (r *dormancyNotifyRepo) ClearRateLimit(context.Context, int64) error { return nil }
func (r *dormancyNotifyRepo) NotifySchedulerAccountChanged(_ context.Context, id int64) error {
	r.notified = append(r.notified, id)
	return nil
}

func anthropicUsageHeaders(util5h float64, resetAt time.Time) http.Header {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-status", "allowed")
	h.Set("anthropic-ratelimit-unified-5h-utilization", strconv.FormatFloat(util5h, 'f', 2, 64))
	h.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(resetAt.Unix(), 10))
	return h
}

// 利用率跨过休眠阈值(进入 / 退出)时立刻通知调度快照;没跨线不通知。
func TestUpdateSessionWindow_NotifiesSchedulerOnDormancyFlip(t *testing.T) {
	repo := &dormancyNotifyRepo{}
	svc := NewRateLimitService(repo, nil, nil, nil, nil)
	reset := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	account := &Account{
		ID:       5,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"pacing_mode": PacingModeStandard, "session_window_utilization": 0.30},
	}

	// 0.30 → 0.90:进入休眠 → 通知一次
	svc.UpdateSessionWindow(context.Background(), account, anthropicUsageHeaders(0.90, reset))
	require.Equal(t, []int64{5}, repo.notified)
	require.True(t, account.IsUtilizationDormant())

	// 0.90 → 0.95:仍在休眠 → 不再通知
	svc.UpdateSessionWindow(context.Background(), account, anthropicUsageHeaders(0.95, reset))
	require.Equal(t, []int64{5}, repo.notified)

	// 新窗口(旧窗口已过期)且利用率回落:退出休眠 → 再通知一次
	past := time.Now().Add(-time.Minute)
	account.SessionWindowEnd = &past
	svc.UpdateSessionWindow(context.Background(), account, anthropicUsageHeaders(0.10, time.Now().Add(5*time.Hour)))
	require.Equal(t, []int64{5, 5}, repo.notified)
	require.False(t, account.IsUtilizationDormant())
}

// 未启用 pacing 的号(admin 建的)不参与休眠,也不发通知。
func TestUpdateSessionWindow_NoPacingNoNotify(t *testing.T) {
	repo := &dormancyNotifyRepo{}
	svc := NewRateLimitService(repo, nil, nil, nil, nil)
	account := &Account{ID: 6, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Extra: map[string]any{}}
	svc.UpdateSessionWindow(context.Background(), account, anthropicUsageHeaders(0.99, time.Now().Add(time.Hour)))
	require.Empty(t, repo.notified)
}
