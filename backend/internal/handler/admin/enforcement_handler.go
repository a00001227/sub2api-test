package admin

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 蒸馏执行层 Admin API：查看 HIGH 名单/状态、豁免名单增删、人工封禁/解封 user/key。
// RBAC 复用 admin 路由组（未认证 401 / 非 admin 403 由上层中间件处理）。
// 自动限速在请求路径中间件里做；本 handler 只做管理面（豁免/封禁/查看），封禁只走人工。

const (
	enforcementErrBadRequest  = "ENFORCEMENT_BAD_REQUEST"
	enforcementErrUnavailable = "ENFORCEMENT_UNAVAILABLE"
	enforcementErrInternal    = "ENFORCEMENT_INTERNAL"
)

// EnforcementHandler 执行层 Admin Handler。
type EnforcementHandler struct {
	svc   *service.EnforcementService
	admin service.AdminService
}

// NewEnforcementHandler 构造。svc 为 nil 时路由不注册（见 registerEnforcementRoutes）。
func NewEnforcementHandler(svc *service.EnforcementService, admin service.AdminService) *EnforcementHandler {
	return &EnforcementHandler{svc: svc, admin: admin}
}

// Middleware 返回请求路径限速中间件；未接线时返回放行 no-op（供网关路由统一挂载）。
func (h *EnforcementHandler) Middleware() gin.HandlerFunc {
	if h == nil {
		return func(c *gin.Context) { c.Next() }
	}
	return middleware2.Enforcement(h.svc)
}

type enforcementAPIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func enforcementErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrEnforcementBadRequest):
		c.AbortWithStatusJSON(http.StatusBadRequest, enforcementAPIError{enforcementErrBadRequest, "invalid params"})
	case errors.Is(err, service.ErrEnforcementUnavailable):
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, enforcementAPIError{enforcementErrUnavailable, "enforcement unavailable"})
	default:
		c.AbortWithStatusJSON(http.StatusInternalServerError, enforcementAPIError{enforcementErrInternal, "internal error"})
	}
}

func enforcementAdminID(c *gin.Context) int64 {
	if subj, ok := middleware2.GetAuthSubjectFromContext(c); ok {
		return subj.UserID
	}
	return 0
}

// GetStatus GET /admin/enforcement/status —— 开关/阈值/HIGH 数/豁免数/刷新时间。
func (h *EnforcementHandler) GetStatus(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"status": h.svc.Status()})
}

// ListHighUsers GET /admin/enforcement/high-users —— 当前 HIGH 用户名单（含限速/豁免标注）。
func (h *EnforcementHandler) ListHighUsers(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	users, err := h.svc.ListHighUsers(c.Request.Context())
	if err != nil {
		enforcementErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"users": users, "count": len(users)})
}

type allowlistRequest struct {
	UserID int64 `json:"user_id"`
}

// AddAllowlist POST /admin/enforcement/allowlist —— 将某用户加入豁免名单。
func (h *EnforcementHandler) AddAllowlist(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var req allowlistRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.UserID <= 0 {
		enforcementErr(c, service.ErrEnforcementBadRequest)
		return
	}
	if err := h.svc.AddAllowlist(c.Request.Context(), req.UserID, enforcementAdminID(c)); err != nil {
		enforcementErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"allowlisted": req.UserID})
}

// RemoveAllowlist DELETE /admin/enforcement/allowlist/:userId —— 将某用户移出豁免名单。
func (h *EnforcementHandler) RemoveAllowlist(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	userID, err := strconv.ParseInt(c.Param("userId"), 10, 64)
	if err != nil || userID <= 0 {
		enforcementErr(c, service.ErrEnforcementBadRequest)
		return
	}
	if err := h.svc.RemoveAllowlist(c.Request.Context(), userID, enforcementAdminID(c)); err != nil {
		enforcementErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"removed": userID})
}

// ListModelRules GET /admin/enforcement/model-rules —— 当前受限模型规则（model → block|throttle）。
func (h *EnforcementHandler) ListModelRules(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	rules := h.svc.ListModelRules()
	c.JSON(http.StatusOK, gin.H{"rules": rules, "count": len(rules)})
}

type modelRuleRequest struct {
	Model  string `json:"model"`
	Action string `json:"action"` // block | throttle
}

// SetModelRule POST /admin/enforcement/model-rules —— 设置某受限模型的处置动作。
func (h *EnforcementHandler) SetModelRule(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var req modelRuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		enforcementErr(c, service.ErrEnforcementBadRequest)
		return
	}
	if err := h.svc.SetModelRule(c.Request.Context(), req.Model, req.Action, enforcementAdminID(c)); err != nil {
		enforcementErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"model": req.Model, "action": req.Action})
}

// DeleteModelRule DELETE /admin/enforcement/model-rules/:model —— 移除某受限模型规则。
func (h *EnforcementHandler) DeleteModelRule(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	model := c.Param("model")
	if model == "" {
		enforcementErr(c, service.ErrEnforcementBadRequest)
		return
	}
	if err := h.svc.DeleteModelRule(c.Request.Context(), model, enforcementAdminID(c)); err != nil {
		enforcementErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"removed": model})
}

type banRequest struct {
	TargetType string `json:"target_type"` // "user" | "key"
	TargetID   int64  `json:"target_id"`
}

// Ban POST /admin/enforcement/ban —— 人工封禁某 user/key（status→disabled，可逆）。
func (h *EnforcementHandler) Ban(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	h.setStatus(c, domain.StatusDisabled, "ban")
}

// Unban POST /admin/enforcement/unban —— 解封某 user/key（status→active）。
func (h *EnforcementHandler) Unban(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	h.setStatus(c, domain.StatusActive, "unban")
}

func (h *EnforcementHandler) setStatus(c *gin.Context, status, action string) {
	var req banRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.TargetID <= 0 {
		enforcementErr(c, service.ErrEnforcementBadRequest)
		return
	}
	adminID := enforcementAdminID(c)
	ctx := c.Request.Context()
	switch req.TargetType {
	case "user":
		if _, err := h.admin.UpdateUser(ctx, req.TargetID, &service.UpdateUserInput{Status: status}); err != nil {
			enforcementErr(c, err)
			return
		}
	case "key":
		if _, err := h.admin.AdminUpdateAPIKeyStatus(ctx, req.TargetID, status); err != nil {
			enforcementErr(c, err)
			return
		}
	default:
		enforcementErr(c, service.ErrEnforcementBadRequest)
		return
	}
	h.svc.Audit(adminID, action, req.TargetType, req.TargetID, nil)
	c.JSON(http.StatusOK, gin.H{"action": action, "target_type": req.TargetType, "target_id": req.TargetID})
}
