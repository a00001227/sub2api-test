package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// PricingDisplayHandler handles the public pricing-display endpoint.
// portal-ui calls this endpoint to render pricing; no business logic in frontend.
type PricingDisplayHandler struct {
	svc *service.PricingDisplayService
}

// NewPricingDisplayHandler creates a PricingDisplayHandler.
func NewPricingDisplayHandler(svc *service.PricingDisplayService) *PricingDisplayHandler {
	return &PricingDisplayHandler{svc: svc}
}

// GetPricingDisplay handles GET /api/v1/public/pricing-display.
// No authentication required.
//
// The result is cached in-memory by the service (invalidated on admin edits).
// We also send an HTTP Cache-Control header so CDNs/browsers can absorb most of
// the traffic; pricing changes rarely, so a short shared cache window is safe.
func (h *PricingDisplayHandler) GetPricingDisplay(c *gin.Context) {
	items, err := h.svc.GetPublicPricingDisplay(c.Request.Context())
	if err != nil {
		response.InternalError(c, "failed to load pricing display data")
		return
	}
	// public: cacheable by shared caches (CDN); max-age for browsers,
	// s-maxage for CDNs; stale-while-revalidate keeps it snappy during refresh.
	c.Header("Cache-Control", "public, max-age=300, s-maxage=300, stale-while-revalidate=600")
	response.Success(c, items)
}
