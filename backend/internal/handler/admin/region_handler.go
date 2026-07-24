package admin

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// RegionHandler handles admin egress-region dictionary management.
type RegionHandler struct {
	regionService *service.RegionService
}

// NewRegionHandler creates the admin region handler.
func NewRegionHandler(regionService *service.RegionService) *RegionHandler {
	return &RegionHandler{regionService: regionService}
}

type regionResponse struct {
	ID        int64  `json:"id"`
	Code      string `json:"code"`
	NameEn    string `json:"name_en"`
	NameZh    string `json:"name_zh"`
	SortOrder int    `json:"sort_order"`
	Enabled   bool   `json:"enabled"`
}

func regionToResponse(r *service.Region) regionResponse {
	return regionResponse{
		ID:        r.ID,
		Code:      r.Code,
		NameEn:    r.NameEn,
		NameZh:    r.NameZh,
		SortOrder: r.SortOrder,
		Enabled:   r.Enabled,
	}
}

// List handles listing regions. ?enabled_only=1 hides disabled ones.
// GET /api/v1/admin/regions
func (h *RegionHandler) List(c *gin.Context) {
	enabledOnly := c.Query("enabled_only") == "1" || c.Query("enabled_only") == "true"
	regions, err := h.regionService.List(c.Request.Context(), enabledOnly)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	out := make([]regionResponse, 0, len(regions))
	for i := range regions {
		out = append(out, regionToResponse(&regions[i]))
	}
	response.Success(c, gin.H{"regions": out})
}

type createRegionRequest struct {
	Code      string `json:"code" binding:"required"`
	NameEn    string `json:"name_en" binding:"required"`
	NameZh    string `json:"name_zh" binding:"required"`
	SortOrder int    `json:"sort_order"`
	Enabled   *bool  `json:"enabled"`
}

// Create handles creating a region.
// POST /api/v1/admin/regions
func (h *RegionHandler) Create(c *gin.Context) {
	var req createRegionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request body")
		return
	}
	r, err := h.regionService.Create(c.Request.Context(), service.CreateRegionInput{
		Code:      req.Code,
		NameEn:    req.NameEn,
		NameZh:    req.NameZh,
		SortOrder: req.SortOrder,
		Enabled:   req.Enabled,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, regionToResponse(r))
}

type updateRegionRequest struct {
	Code      *string `json:"code"`
	NameEn    *string `json:"name_en"`
	NameZh    *string `json:"name_zh"`
	SortOrder *int    `json:"sort_order"`
	Enabled   *bool   `json:"enabled"`
}

// Update handles updating a region.
// PUT /api/v1/admin/regions/:id
func (h *RegionHandler) Update(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "invalid region id")
		return
	}
	var req updateRegionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request body")
		return
	}
	r, err := h.regionService.Update(c.Request.Context(), id, service.UpdateRegionInput{
		Code:      req.Code,
		NameEn:    req.NameEn,
		NameZh:    req.NameZh,
		SortOrder: req.SortOrder,
		Enabled:   req.Enabled,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, regionToResponse(r))
}

// Delete handles deleting a region.
// DELETE /api/v1/admin/regions/:id
func (h *RegionHandler) Delete(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "invalid region id")
		return
	}
	if err := h.regionService.Delete(c.Request.Context(), id); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"deleted": true})
}
