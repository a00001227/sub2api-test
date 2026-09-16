package handler

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// HvoyPricingHandler 对外发布 hvoyai.com Provider Pricing API(schema 1.1)格式的价格数据。
// 无鉴权(运营决定不开签名校验);响应体不走 response.Success 的 {code,message,data} 包装,
// 而是 hvoy 规定的 {schema_version,success,message,data} 顶层结构。
type HvoyPricingHandler struct {
	svc *service.HvoyPricingService
}

// NewHvoyPricingHandler creates a HvoyPricingHandler.
func NewHvoyPricingHandler(svc *service.HvoyPricingService) *HvoyPricingHandler {
	return &HvoyPricingHandler{svc: svc}
}

// GetPricing handles GET /api/provider/pricing.
func (h *HvoyPricingHandler) GetPricing(c *gin.Context) {
	resp, err := h.svc.Build(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"schema_version": "1.1",
			"success":        false,
			"message":        "failed to build pricing data",
		})
		return
	}
	// 价格很少变,给 CDN / 抓取方一个短缓存窗口(与 /public/pricing-display 一致)。
	c.Header("Cache-Control", "public, max-age=300, s-maxage=300, stale-while-revalidate=600")
	c.JSON(http.StatusOK, resp)
}
