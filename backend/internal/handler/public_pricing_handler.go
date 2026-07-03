package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// PublicPricingHandler handles the unauthenticated public pricing endpoint.
type PublicPricingHandler struct {
	svc *service.PublicPricingService
}

// NewPublicPricingHandler creates a PublicPricingHandler.
func NewPublicPricingHandler(svc *service.PublicPricingService) *PublicPricingHandler {
	return &PublicPricingHandler{svc: svc}
}

// GetPricing handles GET /api/v1/public/pricing.
// No authentication required.
func (h *PublicPricingHandler) GetPricing(c *gin.Context) {
	pricing, err := h.svc.GetPublicPricing(c.Request.Context())
	if err != nil {
		response.InternalError(c, "failed to load pricing data")
		return
	}
	response.Success(c, pricing)
}
