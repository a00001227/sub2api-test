package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"

	"github.com/gin-gonic/gin"
)

// Phase 21E-6C-2B-1: Provider Portal 内部路由（机器对机器）。
// 独立于 /api/v1 用户/管理员面；鉴权用 ProviderInternalAuth（独立
// secret，fail-closed）。handler 为 nil（服务未装配）时不注册任何
// 路由 —— 该内部面默认关闭。
func RegisterProviderInternalRoutes(
	r *gin.Engine,
	h *handler.ProviderConnectHandler,
	auth middleware.ProviderInternalAuthMiddleware,
) {
	if h == nil {
		return
	}
	internal := r.Group("/internal/provider-accounts")
	internal.Use(gin.HandlerFunc(auth))
	{
		internal.POST("/onboarding-sessions", h.CreateOnboardingSession)
		// Phase 21E-6E-4: 单条 credential 导入（同鉴权、同内部面）。
		internal.POST("/import-credentials", h.ImportCredentials)
		// Phase 21E-6E proxy-exclusive: 脱敏 region 容量查询。
		internal.GET("/available-regions", h.AvailableRegions)
		// Phase 21E-6E account-metrics: 单账号脱敏运行指标（带参路径，
		// 在静态路径之后注册）。
		internal.GET("/:external_ref/metrics", h.AccountMetrics)
	}

	// 完成流程挂在 /internal/provider/connect（同一鉴权）。
	connect := r.Group("/internal/provider/connect")
	connect.Use(gin.HandlerFunc(auth))
	{
		connect.POST("/complete", h.CompleteAuthorization)
	}
}
