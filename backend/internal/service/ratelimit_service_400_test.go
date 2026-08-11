//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// Anthropic 更新消费者条款后对每个请求返回 400,消息含 "Consumer Terms" /
// "accept them in claude.ai"。这是账号级不可用(每发必败),必须 SetError 标失效
// 并通过状态回流推给 Portal,否则账号一直显示"正常"却发一个失败一个,provider 无从察觉。
func TestRateLimitService_HandleUpstreamError_400ConsumerTerms(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"We've updated our Consumer Terms and Privacy Policy. You'll need to accept them in claude.ai with the email in /status to continue."}}`)

	t.Run("marks account invalid and disables", func(t *testing.T) {
		repo := &rateLimitAccountRepoStub{}
		service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
		account := &Account{
			ID:          900,
			Platform:    PlatformAnthropic,
			Type:        AccountTypeOAuth,
			Credentials: map[string]any{"refresh_token": "rt-900"},
		}

		shouldDisable := service.HandleUpstreamError(context.Background(), account, 400, http.Header{}, body)

		require.True(t, shouldDisable)
		require.Equal(t, 1, repo.setErrorCalls, "consumer-terms 400 must SetError so the status reflows to the provider")
		require.Equal(t, 0, repo.tempCalls)
		require.Contains(t, repo.lastErrorMsg, "Consumer terms not accepted (400)")
	})

	// 回归:普通参数类 400(如 max_tokens 非法)仍不应禁用账号。
	t.Run("generic 400 still ignored", func(t *testing.T) {
		repo := &rateLimitAccountRepoStub{}
		service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
		account := &Account{
			ID:          901,
			Platform:    PlatformAnthropic,
			Type:        AccountTypeOAuth,
			Credentials: map[string]any{"refresh_token": "rt-901"},
		}

		shouldDisable := service.HandleUpstreamError(context.Background(), account, 400, http.Header{}, []byte(`{"type":"error","error":{"message":"max_tokens: must be greater than or equal to 1"}}`))

		require.False(t, shouldDisable)
		require.Equal(t, 0, repo.setErrorCalls)
	})
}
