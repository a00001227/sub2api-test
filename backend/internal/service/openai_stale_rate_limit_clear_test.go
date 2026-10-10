package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type staleClearRepo struct {
	cleared []int64
	err     error
}

func (r *staleClearRepo) ClearRateLimit(_ context.Context, id int64) error {
	if r.err != nil {
		return r.err
	}
	r.cleared = append(r.cleared, id)
	return nil
}

func codexHeaders(p5h, p7d string) http.Header {
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", p5h)
	h.Set("x-codex-primary-window-minutes", "300")
	h.Set("x-codex-secondary-used-percent", p7d)
	h.Set("x-codex-secondary-window-minutes", "10080")
	return h
}

func rateLimitedOpenAIAccount() *Account {
	limitedAt := time.Now().Add(-2 * time.Hour)
	resetAt := time.Now().Add(72 * time.Hour)
	return &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, RateLimitedAt: &limitedAt, RateLimitResetAt: &resetAt}
}

// 仍限流 + 2xx + 两个窗口都没满 → 清除并同步内存对象。
func TestClearStaleOpenAIRateLimitOnSuccess_Clears(t *testing.T) {
	repo := &staleClearRepo{}
	acc := rateLimitedOpenAIAccount()
	ok := clearStaleOpenAIRateLimitOnSuccess(context.Background(), repo, acc, http.StatusOK, ParseCodexRateLimitHeaders(codexHeaders("0", "0")))
	require.True(t, ok)
	require.Equal(t, []int64{2}, repo.cleared)
	require.Nil(t, acc.RateLimitResetAt)
	require.Nil(t, acc.RateLimitedAt)
	require.False(t, acc.IsRateLimited())
}

func TestClearStaleOpenAIRateLimitOnSuccess_Skips(t *testing.T) {
	cases := []struct {
		name    string
		account *Account
		status  int
		headers http.Header
	}{
		{"not rate limited", &Account{ID: 2, Platform: PlatformOpenAI}, http.StatusOK, codexHeaders("0", "0")},
		{"429 even with <100 headers", rateLimitedOpenAIAccount(), http.StatusTooManyRequests, codexHeaders("10", "20")},
		{"7d exhausted", rateLimitedOpenAIAccount(), http.StatusOK, codexHeaders("0", "100")},
		{"5h exhausted", rateLimitedOpenAIAccount(), http.StatusOK, codexHeaders("100", "0")},
		{"no codex headers", rateLimitedOpenAIAccount(), http.StatusOK, http.Header{}},
		{"anthropic account", func() *Account { a := rateLimitedOpenAIAccount(); a.Platform = PlatformAnthropic; return a }(), http.StatusOK, codexHeaders("0", "0")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &staleClearRepo{}
			ok := clearStaleOpenAIRateLimitOnSuccess(context.Background(), repo, tc.account, tc.status, ParseCodexRateLimitHeaders(tc.headers))
			require.False(t, ok)
			require.Empty(t, repo.cleared)
		})
	}
}

// 仓储失败不改内存对象。
func TestClearStaleOpenAIRateLimitOnSuccess_RepoError(t *testing.T) {
	repo := &staleClearRepo{err: errors.New("db down")}
	acc := rateLimitedOpenAIAccount()
	ok := clearStaleOpenAIRateLimitOnSuccess(context.Background(), repo, acc, http.StatusOK, ParseCodexRateLimitHeaders(codexHeaders("0", "0")))
	require.False(t, ok)
	require.True(t, acc.IsRateLimited())
}

// nil 仓储(接口里装了 nil)直接跳过,不 panic。
func TestClearStaleOpenAIRateLimitOnSuccess_NilRepo(t *testing.T) {
	var repo AccountRepository
	acc := rateLimitedOpenAIAccount()
	require.False(t, clearStaleOpenAIRateLimitOnSuccess(context.Background(), repo, acc, http.StatusOK, ParseCodexRateLimitHeaders(codexHeaders("0", "0"))))
}
