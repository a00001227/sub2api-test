package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/gin-gonic/gin"
)

// RegisterPublicRoutes registers unauthenticated public API routes under /api/v1/public.
func RegisterPublicRoutes(v1 *gin.RouterGroup, h *handler.Handlers) {
	public := v1.Group("/public")
	{
		public.GET("/pricing", h.PublicPricing.GetPricing)
		// Unified pricing display system — portal-ui calls this endpoint only.
		public.GET("/pricing-display", h.PricingDisplay.GetPricingDisplay)
	}
}

// RegisterProviderPricingRoutes 注册对外的 hvoyai.com Provider Pricing API(无鉴权)。
// 路径按 hvoy 规范固定为 /api/provider/pricing(不在 /api/v1 下),故挂在引擎根上。
func RegisterProviderPricingRoutes(r *gin.Engine, h *handler.Handlers) {
	r.GET("/api/provider/pricing", h.HvoyPricing.GetPricing)
}
