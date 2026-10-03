package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// stubBalanceCache 只实现余额读取;余额 ≤ 0 时资格检查在第一步就拒绝,不会碰其它方法。
type stubBalanceCache struct {
	service.BillingCache
	balance float64
}

func (s *stubBalanceCache) GetUserBalance(context.Context, int64) (float64, error) {
	return s.balance, nil
}

func newForwardEligibilityCtx(t *testing.T, platform string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID:     11,
		UserID: 7,
		User:   &service.User{ID: 7},
		Group:  &service.Group{ID: 3, Platform: platform},
	})
	return c, w
}

func newZeroBalanceBillingService(t *testing.T) *service.BillingCacheService {
	t.Helper()
	svc := service.NewBillingCacheService(&stubBalanceCache{balance: 0}, nil, nil, nil, nil, nil,
		&config.Config{RunMode: config.RunModeStandard}, nil)
	t.Cleanup(svc.Stop)
	return svc
}

// 转发路径必须和本地 handler 一样拒绝余额为 0 的用户(以前只查 RPM,余额为 0 照样转发执行)。
func TestCheckForwardEligibility_RejectsZeroBalance_Claude(t *testing.T) {
	h := &GatewayHandler{billingCacheService: newZeroBalanceBillingService(t)}
	c, w := newForwardEligibilityCtx(t, service.PlatformAnthropic)

	require.False(t, h.CheckForwardEligibility(c))
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "billing_error")
}

func TestCheckForwardEligibility_RejectsZeroBalance_OpenAI(t *testing.T) {
	h := &OpenAIGatewayHandler{billingCacheService: newZeroBalanceBillingService(t)}
	c, w := newForwardEligibilityCtx(t, service.PlatformOpenAI)

	require.False(t, h.CheckForwardEligibility(c))
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "billing_error")
}

// 简易模式跳过计费检查 → 放行且不写响应(与本地 handler 口径一致)。
func TestCheckForwardEligibility_SimpleModePasses(t *testing.T) {
	svc := service.NewBillingCacheService(&stubBalanceCache{balance: 0}, nil, nil, nil, nil, nil,
		&config.Config{RunMode: config.RunModeSimple}, nil)
	t.Cleanup(svc.Stop)
	h := &GatewayHandler{billingCacheService: svc}
	c, w := newForwardEligibilityCtx(t, service.PlatformAnthropic)

	require.True(t, h.CheckForwardEligibility(c))
	require.False(t, c.IsAborted())
	require.Zero(t, w.Body.Len())
}

// 没有 key / 没有计费服务时不拦(与原 CheckForwardRPM 一致,避免误伤非转发调用)。
func TestCheckForwardEligibility_NoKeyOrServicePasses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	require.True(t, (&GatewayHandler{}).CheckForwardEligibility(c))
	require.True(t, (&OpenAIGatewayHandler{}).CheckForwardEligibility(c))
}
