//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newAnthropicTransportErrTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	return c, rec
}

func anthropicTestUpstreamReq(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	require.NoError(t, err)
	return req
}

// isConnectPhaseTransportError 的分类表：连接建立阶段=true，读/响应侧与取消=false。
func TestIsConnectPhaseTransportError_Classification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"dial timeout OpError", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("i/o timeout")}, true},
		{"connection refused (syscall)", fmt.Errorf("connect: %w", syscall.ECONNREFUSED), true},
		{"host unreachable (syscall)", fmt.Errorf("connect: %w", syscall.EHOSTUNREACH), true},
		{"network unreachable (syscall)", fmt.Errorf("connect: %w", syscall.ENETUNREACH), true},
		{"dns not found", &net.DNSError{Err: "no such host", Name: "bad.proxy", IsNotFound: true}, true},
		{"connection refused (string)", errors.New(`dial tcp 1.2.3.4:443: connect: connection refused`), true},
		{"no route to host (string)", errors.New(`dial tcp 1.2.3.4:443: connect: no route to host`), true},
		{"tls handshake timeout", errors.New(`net/http: TLS handshake timeout`), true},  // 握手阶段(HTTP 请求未发出)→ 换号安全
		{"tls: handshake failure", errors.New(`remote error: tls: handshake failure`), true},
		// 响应头之前连接被掐（死号被上游边缘 RST / 代理断连）→ 换号安全。
		// 本分类器只在 DoWithTLS 返回 err（尚无响应头）时被调用，故这些 EOF 必在响应头之前。
		{"io.EOF", io.EOF, true},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, true},
		{"url.Error wrapping io.EOF (real symptom)", &url.Error{Op: "Post", URL: "https://api.anthropic.com/v1/messages?beta=true", Err: io.EOF}, true},
		{"connection reset by peer (syscall)", fmt.Errorf("read: %w", syscall.ECONNRESET), true},
		{"connection reset by peer (string)", errors.New(`read tcp 1.2.3.4:443: connection reset by peer`), true},
		{"unexpected eof (string)", errors.New("unexpected EOF"), true},
		// 读/响应侧：不换号。
		{"context canceled", context.Canceled, false},
		{"wrapped context canceled", fmt.Errorf("do: %w", context.Canceled), false},
		{"context deadline (read side)", context.DeadlineExceeded, false},
		{"read OpError", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("i/o timeout")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isConnectPhaseTransportError(tc.err))
		})
	}
}

// 连接建立阶段失败：返回 *UpstreamFailoverError（handler 换号），本函数不写响应体。
func TestHandleAnthropicUpstreamTransportError_ConnectPhase_FailsOverNoBody(t *testing.T) {
	connectErrs := []error{
		&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("i/o timeout")},
		fmt.Errorf("connect: %w", syscall.ECONNREFUSED),
		&net.DNSError{Err: "no such host", Name: "bad.proxy", IsNotFound: true},
		errors.New(`dial tcp 1.2.3.4:443: connect: connection refused`),
	}
	for _, e := range connectErrs {
		t.Run(e.Error(), func(t *testing.T) {
			account := &Account{ID: 4627, Name: "flaky", Platform: PlatformAnthropic}
			c, rec := newAnthropicTransportErrTestContext()

			retErr := handleAnthropicUpstreamTransportError(c, account, anthropicTestUpstreamReq(t), e, false)

			var fo *UpstreamFailoverError
			require.True(t, errors.As(retErr, &fo), "connect-phase error must return *UpstreamFailoverError")
			require.Equal(t, http.StatusBadGateway, fo.StatusCode)
			require.Contains(t, string(fo.ResponseBody), "upstream_error")
			// 本函数不写响应，交由 handler（换号 / 换号耗尽后写协议正确错误）。
			require.Equal(t, 0, rec.Body.Len(), "connect-phase must NOT write a response body")
		})
	}
}

// 读侧超时：不换号，本函数直接写 502 + 返回普通 error。
func TestHandleAnthropicUpstreamTransportError_ReadTimeout_Writes502NoFailover(t *testing.T) {
	account := &Account{ID: 99, Name: "slow", Platform: PlatformAnthropic}
	c, rec := newAnthropicTransportErrTestContext()

	retErr := handleAnthropicUpstreamTransportError(c, account, anthropicTestUpstreamReq(t), context.DeadlineExceeded, false)

	var fo *UpstreamFailoverError
	require.False(t, errors.As(retErr, &fo), "read-side deadline must NOT fail over")
	require.Error(t, retErr)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), "upstream_error")
}

// 客户端断开：不换号，写 502。
func TestHandleAnthropicUpstreamTransportError_Canceled_Writes502NoFailover(t *testing.T) {
	account := &Account{ID: 77, Name: "healthy", Platform: PlatformAnthropic}
	c, rec := newAnthropicTransportErrTestContext()

	retErr := handleAnthropicUpstreamTransportError(c, account, anthropicTestUpstreamReq(t),
		fmt.Errorf("do: %w", context.Canceled), false)

	var fo *UpstreamFailoverError
	require.False(t, errors.As(retErr, &fo), "context.Canceled must NOT fail over")
	require.Error(t, retErr)
	require.Contains(t, rec.Body.String(), "upstream_error")
}

// 预算耗尽：即便是连接级失败也不再换号，写 502。
func TestHandleAnthropicUpstreamTransportError_BudgetExhausted_Writes502(t *testing.T) {
	account := &Account{ID: 88, Name: "budget-gone", Platform: PlatformAnthropic}
	c, rec := newAnthropicTransportErrTestContext()
	// 伪造本请求早已开始（超出 60s 预算窗）。
	c.Set(anthropicTransportFailoverBudgetKey, time.Now().Add(-2*upstreamFailoverBudget))

	connectErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("i/o timeout")}
	retErr := handleAnthropicUpstreamTransportError(c, account, anthropicTestUpstreamReq(t), connectErr, false)

	var fo *UpstreamFailoverError
	require.False(t, errors.As(retErr, &fo), "budget-exhausted must NOT fail over even for connect-phase error")
	require.Error(t, retErr)
	require.Contains(t, rec.Body.String(), "upstream_error")
}

// 预算闸跨账号累计：首次放行并记录起点；窗口内第二次仍放行；起点被设为过去则拒绝。
func TestUpstreamFailoverWithinBudget_Accumulates(t *testing.T) {
	c, _ := newAnthropicTransportErrTestContext()

	require.True(t, upstreamFailoverWithinBudget(c), "first transport failure always within budget")
	// 起点已记录，窗口内仍放行。
	require.True(t, upstreamFailoverWithinBudget(c), "second failure shortly after must still be within budget")

	// 把起点推到预算窗之外 → 拒绝。
	c.Set(anthropicTransportFailoverBudgetKey, time.Now().Add(-2*upstreamFailoverBudget))
	require.False(t, upstreamFailoverWithinBudget(c), "beyond budget window must be rejected")
}

// 透传分支 passthrough=true 也走同一决策（连接级 → 换号不写体）。
func TestHandleAnthropicUpstreamTransportError_Passthrough_ConnectPhaseFailsOver(t *testing.T) {
	account := &Account{ID: 4628, Name: "passthrough", Platform: PlatformAnthropic}
	c, rec := newAnthropicTransportErrTestContext()

	retErr := handleAnthropicUpstreamTransportError(c, account, anthropicTestUpstreamReq(t),
		&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("i/o timeout")}, true)

	var fo *UpstreamFailoverError
	require.True(t, errors.As(retErr, &fo))
	require.Equal(t, 0, rec.Body.Len())
	require.True(t, strings.Contains(string(fo.ResponseBody), "upstream_error"))
}
