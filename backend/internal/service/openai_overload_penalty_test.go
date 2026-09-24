package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"time"
)

// 本文件不带 unit 标签(该标签下 service 包另有历史编译错误),自带最小仓储桩。
type openAIOverloadRepoStub struct {
	AccountRepository
	tempCalls     int
	setErrorCalls int
}

func (r *openAIOverloadRepoStub) SetTempUnschedulable(context.Context, int64, time.Time, string) error {
	r.tempCalls++
	return nil
}

func (r *openAIOverloadRepoStub) SetError(context.Context, int64, string) error {
	r.setErrorCalls++
	return nil
}

// Portal 接入时给两平台都套的默认规则之一:529 | overloaded, too many | 2 | 过载。
func openAIOverloadTestAccount() *Account {
	return &Account{ID: 501, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Credentials: map[string]any{
		"temp_unschedulable_enabled": true,
		"temp_unschedulable_rules": []any{
			map[string]any{"error_code": 529, "keywords": []any{"overloaded", "too many"}, "duration_minutes": 2, "description": "过载"},
		},
	}}
}

const openAIOverloadMsg = "Our servers are currently overloaded. Please try again later."

func TestOpenAIEffectiveUpstreamStatus(t *testing.T) {
	require.Equal(t, 529, openAIEffectiveUpstreamStatus(http.StatusOK, []byte(openAIOverloadMsg)), "200 流终态过载 → 529")
	require.Equal(t, 529, openAIEffectiveUpstreamStatus(http.StatusBadGateway, []byte(`{"error":{"message":"`+openAIOverloadMsg+`"}}`)), "502 过载文案 → 529")
	require.Equal(t, http.StatusServiceUnavailable, openAIEffectiveUpstreamStatus(http.StatusServiceUnavailable, []byte("service unavailable")), "非过载 5xx 原样")
	require.Equal(t, http.StatusTooManyRequests, openAIEffectiveUpstreamStatus(http.StatusTooManyRequests, []byte("overloaded")), "4xx 不归一")
	require.Equal(t, http.StatusOK, openAIEffectiveUpstreamStatus(http.StatusOK, []byte("content policy")), "非过载文案原样")
}

// HTTP 502 + 过载文案:以前只记 warn;现在命中「529 | overloaded」规则 → 临时不可调度。
func TestOpenAIOverload_HTTP5xxBodyHitsTempUnschedRule(t *testing.T) {
	repo := &openAIOverloadRepoStub{}
	svc := &OpenAIGatewayService{rateLimitService: NewRateLimitService(repo, nil, &config.Config{}, nil, nil)}
	acc := openAIOverloadTestAccount()
	shouldDisable := svc.handleOpenAIAccountUpstreamError(context.Background(), acc, http.StatusBadGateway, http.Header{}, []byte(`{"error":{"message":"`+openAIOverloadMsg+`"}}`))
	// 规则命中 → 与 429/503 规则同口径:返回 true 触发换号,并在 OpenAI 侧加一段运行时封禁桥接。
	require.True(t, shouldDisable)
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(acc))
	require.Equal(t, 1, repo.tempCalls, "过载号应被临时摘除")
	require.Zero(t, repo.setErrorCalls)
}

// 200 流终态 response.failed 过载(失败转移前 / 透传时都会调):同样摘号;非过载文案不动。
func TestOpenAIOverload_StreamFailedEventPenalizes(t *testing.T) {
	repo := &openAIOverloadRepoStub{}
	svc := &OpenAIGatewayService{rateLimitService: NewRateLimitService(repo, nil, &config.Config{}, nil, nil)}
	acc := openAIOverloadTestAccount()
	payload := []byte(`{"type":"response.failed","response":{"error":{"code":"server_error","message":"` + openAIOverloadMsg + `"}}}`)
	svc.penalizeOpenAIStreamFailure(nil, acc, payload, openAIOverloadMsg)
	require.Equal(t, 1, repo.tempCalls)

	svc.penalizeOpenAIStreamFailure(nil, acc, nil, "An error occurred while processing your request")
	require.Equal(t, 1, repo.tempCalls, "非过载失败不摘号")
	svc.penalizeOpenAIStreamFailure(nil, nil, payload, openAIOverloadMsg) // nil account 安全
}
