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
		internal.GET("/regions", h.Regions)
		// Model catalog (official reference prices, micros) for the Portal to
		// cache + let admins pick billable models. Read-only.
		internal.GET("/model-catalog", h.ModelCatalog)
		// Phase 21E-6E account-metrics: 单账号脱敏运行指标（带参路径，
		// 在静态路径之后注册）。
		internal.GET("/:external_ref/metrics", h.AccountMetrics)
		// Phase 21F provider-account-deactivate: 停用/解绑账号（幂等）。
		internal.POST("/:external_ref/deactivate", h.Deactivate)
		// Phase 21G provider-reauth: 为凭证失效的既有账号创建重授权会话(OAuth)。
		internal.POST("/:external_ref/reauth-sessions", h.CreateReauthSession)
		// sessionKey 就地重认证(claude):粘新 sessionKey → 换 token 更新既有号。
		internal.POST("/:external_ref/reauth-sessionkey", h.ReauthSessionKey)
		// Phase 21I provider-pacing: 设置 DeRouter 五档调度档位。
		internal.POST("/:external_ref/pacing-mode", h.SetPacingMode)
		// Phase 21I provider-scheduling: 可逆暂停/恢复账号调度（只切 schedulable）。
		internal.POST("/:external_ref/scheduling", h.SetScheduling)
		// #90 admin-only 测试连接: 触发真实连通性测试, 返回 JSON 结论。
		// "仅 admin" 由 Portal 侧 AdminGuard 把关; 本端同 provider-internal 鉴权。
		internal.POST("/:external_ref/test", h.TestConnection)
		// #90-A2/B 账号配置: 读/改可配置项(先接 model_mapping 白名单)。
		internal.GET("/:external_ref/config", h.GetAccountConfig)
		internal.POST("/:external_ref/config", h.SetAccountConfig)
	}

	// 完成流程挂在 /internal/provider/connect（同一鉴权）。
	connect := r.Group("/internal/provider/connect")
	connect.Use(gin.HandlerFunc(auth))
	{
		connect.POST("/complete", h.CompleteAuthorization)
	}

	// 多出口(Option A): Portal 推送本 cell 的 IP 池 → 灌进本地 proxies 表。
	// 同一 provider-internal 鉴权;edge 存活(本函数被无条件注册)。
	proxies := r.Group("/internal/proxies")
	proxies.Use(gin.HandlerFunc(auth))
	{
		proxies.POST("/sync", h.SyncProxies)
		// account→proxy occupancy (which IP is used by which account).
		proxies.GET("/bindings", h.ProxyBindings)
	}
}
