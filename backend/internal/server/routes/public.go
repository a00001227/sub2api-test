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
