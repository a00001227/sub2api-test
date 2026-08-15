package admin

import (
	"errors"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// UserRiskHandler 处理反蒸馏 Phase 0（仅观测/影子模式）的 admin API。
// 仅读取评分/维护 allowlist·manual_tier·config；绝不触发任何请求拦截。
type UserRiskHandler struct {
	svc *service.RiskAdminService
}

// NewUserRiskHandler 构造 UserRiskHandler。
func NewUserRiskHandler(svc *service.RiskAdminService) *UserRiskHandler {
	return &UserRiskHandler{svc: svc}
}

// ListUsers GET /admin/risk/users?sort=score&limit=&tier=
func (h *UserRiskHandler) ListUsers(c *gin.Context) {
	tier := strings.TrimSpace(c.Query("tier"))
	limit := 100
	if v := strings.TrimSpace(c.Query("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	views, err := h.svc.ListUsers(c.Request.Context(), tier, limit)
	if err != nil {
		response.InternalError(c, "failed to list risk users")
		return
	}
	response.Success(c, gin.H{"items": views})
}

// GetUser GET /admin/risk/users/:id
func (h *UserRiskHandler) GetUser(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "invalid user id")
		return
	}
	view, err := h.svc.GetUser(c.Request.Context(), id)
	if err != nil {
		response.InternalError(c, "failed to get risk user")
		return
	}
	response.Success(c, view)
}

type allowlistRequest struct {
	On bool `json:"on"`
}

// SetAllowlist POST /admin/risk/users/:id/allowlist  body {on:bool}
func (h *UserRiskHandler) SetAllowlist(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "invalid user id")
		return
	}
	var req allowlistRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if err := h.svc.SetAllowlist(c.Request.Context(), id, req.On); err != nil {
		response.InternalError(c, "failed to set allowlist")
		return
	}
	response.Success(c, gin.H{"user_id": id, "allowlisted": req.On})
}

type manualTierRequest struct {
	// Tier 为 null 时清除 manual_tier；否则必须是 watch/medium/high。
	Tier *string `json:"tier"`
}

// SetManualTier POST /admin/risk/users/:id/manual-tier  body {tier:string|null}
func (h *UserRiskHandler) SetManualTier(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "invalid user id")
		return
	}
	var req manualTierRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if req.Tier != nil {
		t := strings.TrimSpace(*req.Tier)
		req.Tier = &t
	}
	if err := h.svc.SetManualTier(c.Request.Context(), id, req.Tier); err != nil {
		if errors.Is(err, service.ErrInvalidRiskTier) {
			response.BadRequest(c, "invalid tier (allowed: watch, medium, high, or null)")
			return
		}
		response.InternalError(c, "failed to set manual tier")
		return
	}
	response.Success(c, gin.H{"user_id": id, "manual_tier": req.Tier})
}

// GetConfig GET /admin/risk/config
func (h *UserRiskHandler) GetConfig(c *gin.Context) {
	response.Success(c, h.svc.GetConfig())
}

// PatchConfig PATCH /admin/risk/config
func (h *UserRiskHandler) PatchConfig(c *gin.Context) {
	var in service.PatchConfigInput
	if err := c.ShouldBindJSON(&in); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	updated := h.svc.PatchConfig(in)
	response.Success(c, updated)
}
