package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

var errAwaitHeaders = &url.Error{Op: "Post", URL: "https://api.anthropic.com/v1/messages", Err: errors.New("net/http: timeout awaiting response headers")}

func newHeaderTimeoutCtx(t *testing.T) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	return c
}

func TestIsResponseHeaderTimeout(t *testing.T) {
	require.True(t, isResponseHeaderTimeout(errAwaitHeaders))
	require.False(t, isResponseHeaderTimeout(nil))
	require.False(t, isResponseHeaderTimeout(context.Canceled))
	require.False(t, isResponseHeaderTimeout(context.DeadlineExceeded))
	require.False(t, isResponseHeaderTimeout(errors.New("connection refused")))
	// 响应头超时不属于连接建立阶段错误(原分类器保持不变)
	require.False(t, isConnectPhaseTransportError(errAwaitHeaders))
}

// 响应头超时:第 1、2 次允许再试(同号重试 → 换号),第 3 次放弃;不受连接级 60s 预算约束。
func TestHeaderTimeout_FailoverTwiceThenGiveUp(t *testing.T) {
	c := newHeaderTimeoutCtx(t)
	acct := &Account{ID: 20, Name: "pa_x", Platform: PlatformAnthropic}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)

	for i := 1; i <= 2; i++ {
		err := handleAnthropicUpstreamTransportError(c, acct, req, errAwaitHeaders, false)
		var fe *UpstreamFailoverError
		require.True(t, errors.As(err, &fe), "attempt %d should fail over", i)
		require.Equal(t, http.StatusBadGateway, fe.StatusCode)
		require.True(t, fe.RetryableOnSameAccount, "header timeout retries the same account first")
		require.Equal(t, 1, fe.SameAccountRetryLimit, "but only once")
		require.False(t, c.Writer.Written(), "failover path must not write a response")
	}

	// 第 3 次:放弃,写 502。
	rec := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(rec)
	c2.Request = c.Request
	c2.Set(anthropicHeaderTimeoutAttemptsKey, anthropicHeaderTimeoutMaxAttempts)
	err := handleAnthropicUpstreamTransportError(c2, acct, req, errAwaitHeaders, false)
	var fe *UpstreamFailoverError
	require.False(t, errors.As(err, &fe))
	require.Equal(t, http.StatusBadGateway, rec.Code)
}

// 客户端已断开时,响应头超时不再重试(重试只会对上游重复执行)。
func TestHeaderTimeout_ClientGone_NoFailover(t *testing.T) {
	c := newHeaderTimeoutCtx(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Request = c.Request.WithContext(ctx)
	acct := &Account{ID: 20, Platform: PlatformAnthropic}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	err := handleAnthropicUpstreamTransportError(c, acct, req, errAwaitHeaders, false)
	var fe *UpstreamFailoverError
	require.False(t, errors.As(err, &fe))
}
