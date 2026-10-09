package service

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// requestLevel429Repo 只实现本组用例会触达的方法;其余方法走嵌入的 nil 接口(不应被调用)。
type requestLevel429Repo struct {
	AccountRepository
	rateLimitCalls int
	lastResetAt    time.Time
	clearCalls     int
}

func (r *requestLevel429Repo) UpdateSessionWindow(_ context.Context, _ int64, _, _ *time.Time, _ string) error {
	return nil
}

func (r *requestLevel429Repo) SetRateLimited(_ context.Context, _ int64, resetAt time.Time) error {
	r.rateLimitCalls++
	r.lastResetAt = resetAt
	return nil
}

func (r *requestLevel429Repo) ClearRateLimit(_ context.Context, _ int64) error {
	r.clearCalls++
	return nil
}

func TestClassifyAnthropicRequestLevel429(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"long context credits (upstream shape)", `{"type":"error","error":{"type":"rate_limit_error","message":"Usage credits are required for long context requests."}}`, anthropicRequestLevel429LongContext},
		{"request exceeds remaining", `{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."}}`, anthropicRequestLevel429RequestTooLarge},
		{"real window exhaustion", `{"type":"error","error":{"type":"rate_limit_error","message":"You have exceeded your rate limit for this 5 hour window."}}`, ""},
		{"empty", ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, classifyAnthropicRequestLevel429([]byte(tc.body)))
		})
	}
}

// 长上下文需付费额度:只带月度汇总头的 429 → 不冷却账号(以前会按汇总头冷却 20 多天)。
func TestHandle429_AnthropicLongContextCredits_DoesNotCoolAccount(t *testing.T) {
	repo := &requestLevel429Repo{}
	svc := NewRateLimitService(repo, nil, nil, nil, nil)
	account := &Account{ID: 7, Platform: PlatformAnthropic, Type: AccountTypeOAuth}
	monthly := time.Now().AddDate(0, 0, 23).Unix()
	headers := http.Header{}
	headers.Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(monthly, 10))

	svc.handle429(context.Background(), account, headers, []byte(`{"error":{"message":"Usage credits are required for long context requests."}}`))

	require.Zero(t, repo.rateLimitCalls, "请求级 429 不得冷却账号")
}

// 单条请求超过窗口剩余额度:只短冷却 5 分钟,不采信月度汇总头。
func TestHandle429_AnthropicRequestExceedsRemaining_ShortCooldown(t *testing.T) {
	repo := &requestLevel429Repo{}
	svc := NewRateLimitService(repo, nil, nil, nil, nil)
	account := &Account{ID: 8, Platform: PlatformAnthropic, Type: AccountTypeOAuth}
	headers := http.Header{}
	headers.Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(time.Now().AddDate(0, 0, 23).Unix(), 10))

	svc.handle429(context.Background(), account, headers, []byte(`{"error":{"message":"This request would exceed your account's rate limit. Please try again later."}}`))

	require.Equal(t, 1, repo.rateLimitCalls)
	require.WithinDuration(t, time.Now().Add(anthropicRequestLevel429ShortCooldown), repo.lastResetAt, 10*time.Second)
}

// 汇总头兜底分支:指向几十天后的重置时间 → 封顶 24h。
func TestHandle429_AggregatedResetCappedAt24h(t *testing.T) {
	repo := &requestLevel429Repo{}
	svc := NewRateLimitService(repo, nil, nil, nil, nil)
	account := &Account{ID: 9, Platform: PlatformAnthropic, Type: AccountTypeOAuth}
	headers := http.Header{}
	headers.Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(time.Now().AddDate(0, 0, 23).Unix(), 10))

	svc.handle429(context.Background(), account, headers, []byte(`{"error":{"message":"some other rate limit"}}`))

	require.Equal(t, 1, repo.rateLimitCalls)
	require.WithinDuration(t, time.Now().Add(maxAggregated429Cooldown), repo.lastResetAt, 10*time.Second)
}

// 汇总头在 24h 内 → 原样采用,不受封顶影响。
func TestHandle429_AggregatedResetWithin24hKept(t *testing.T) {
	repo := &requestLevel429Repo{}
	svc := NewRateLimitService(repo, nil, nil, nil, nil)
	account := &Account{ID: 10, Platform: PlatformAnthropic, Type: AccountTypeOAuth}
	want := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	headers := http.Header{}
	headers.Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(want.Unix(), 10))

	svc.handle429(context.Background(), account, headers, []byte(`{"error":{"message":"some other rate limit"}}`))

	require.Equal(t, 1, repo.rateLimitCalls)
	require.Equal(t, want.Unix(), repo.lastResetAt.Unix())
}

func TestAnthropicRateLimitHeadersAllowed(t *testing.T) {
	mk := func(kv ...string) http.Header {
		h := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			h.Set(kv[i], kv[i+1])
		}
		return h
	}
	require.False(t, anthropicRateLimitHeadersAllowed(nil))
	require.False(t, anthropicRateLimitHeadersAllowed(mk()), "无 unified 头不敢断言")
	require.True(t, anthropicRateLimitHeadersAllowed(mk("anthropic-ratelimit-unified-status", "allowed")))
	require.True(t, anthropicRateLimitHeadersAllowed(mk("anthropic-ratelimit-unified-5h-status", "allowed_warning", "anthropic-ratelimit-unified-7d-status", "allowed")))
	require.False(t, anthropicRateLimitHeadersAllowed(mk("anthropic-ratelimit-unified-5h-status", "allowed", "anthropic-ratelimit-unified-7d-status", "rejected")))
	require.False(t, anthropicRateLimitHeadersAllowed(mk("anthropic-ratelimit-unified-5h-status", "rejected")))
}

// 测试连接成功 + 头部 allowed + 账号仍标记限流 → 清除;未标记或头部不 allowed → 不动。
func TestAccountTest_ClearStaleRateLimitOnSuccess(t *testing.T) {
	repo := &requestLevel429Repo{}
	svc := &AccountTestService{accountRepo: repo}
	future := time.Now().Add(20 * 24 * time.Hour)

	limited := &Account{ID: 11, Platform: PlatformAnthropic, RateLimitResetAt: &future}
	ok := http.Header{}
	ok.Set("anthropic-ratelimit-unified-status", "allowed")
	svc.clearStaleRateLimitOnTestSuccess(context.Background(), limited, ok)
	require.Equal(t, 1, repo.clearCalls)
	require.Nil(t, limited.RateLimitResetAt)

	// 头部说 rejected → 不清
	limited2 := &Account{ID: 12, Platform: PlatformAnthropic, RateLimitResetAt: &future}
	bad := http.Header{}
	bad.Set("anthropic-ratelimit-unified-5h-status", "rejected")
	svc.clearStaleRateLimitOnTestSuccess(context.Background(), limited2, bad)
	require.Equal(t, 1, repo.clearCalls)

	// 本来没限流 → 不清
	svc.clearStaleRateLimitOnTestSuccess(context.Background(), &Account{ID: 13, Platform: PlatformAnthropic}, ok)
	require.Equal(t, 1, repo.clearCalls)
}
