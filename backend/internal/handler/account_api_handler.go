package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// AccountAPIHandler 处理账号密钥（Account Key）相关的 API 接口。
// 这些接口通过 API Key 鉴权（不使用 JWT），面向持有账号密钥的调用方。
type AccountAPIHandler struct {
	userService   *service.UserService
	apiKeyService *service.APIKeyService
	usageService  *service.UsageService
}

// NewAccountAPIHandler 创建 AccountAPIHandler。
func NewAccountAPIHandler(userService *service.UserService, apiKeyService *service.APIKeyService, usageService *service.UsageService) *AccountAPIHandler {
	return &AccountAPIHandler{
		userService:   userService,
		apiKeyService: apiKeyService,
		usageService:  usageService,
	}
}

// accountKeyFromContext extracts the authenticated account key from context.
// Returns nil and aborts with 401 if not found, or 403 if it is a sub key.
func (h *AccountAPIHandler) accountKeyFromContext(c *gin.Context) *service.APIKey {
	raw, exists := c.Get(string(middleware.ContextKeyAPIKey))
	if !exists {
		middleware.AbortWithError(c, 401, "API_KEY_REQUIRED", "API key not found in context")
		return nil
	}
	apiKey, ok := raw.(*service.APIKey)
	if !ok || apiKey == nil {
		middleware.AbortWithError(c, 401, "API_KEY_REQUIRED", "Invalid API key in context")
		return nil
	}
	if apiKey.ParentKeyID != nil {
		middleware.AbortWithError(c, 403, "ACCOUNT_KEY_REQUIRED", "This endpoint requires an account key, not a sub key")
		return nil
	}
	return apiKey
}

// Balance 查询账户余额。
//
// GET /balance
// Authorization: Bearer <account_key>
func (h *AccountAPIHandler) Balance(c *gin.Context) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		middleware.AbortWithError(c, 401, "USER_NOT_FOUND", "Could not resolve user from API key")
		return
	}

	user, err := h.userService.GetByID(c.Request.Context(), subject.UserID)
	if err != nil {
		if errors.Is(err, service.ErrUserNotFound) {
			middleware.AbortWithError(c, 401, "USER_NOT_FOUND", "User not found")
			return
		}
		middleware.AbortWithError(c, 500, "INTERNAL_ERROR", "Failed to retrieve user")
		return
	}

	if !user.IsActive() {
		middleware.AbortWithError(c, 401, "USER_INACTIVE", "User account is inactive")
		return
	}

	balance := user.Balance
	if balance < 0 {
		balance = 0
	}

	locked, err := h.apiKeyService.GetLockedBalance(c.Request.Context(), subject.UserID)
	if err != nil {
		middleware.AbortWithError(c, 500, "INTERNAL_ERROR", "Failed to compute locked balance")
		return
	}

	available := balance - locked
	if available < 0 {
		available = 0
	}

	c.JSON(http.StatusOK, gin.H{
		"balance":           balance,
		"locked_balance":    locked,
		"available_balance": available,
		"unit":              "USD",
	})
}

// createSubKeyRequest is the request body for POST /sub-keys.
type createSubKeyRequest struct {
	Label  string  `json:"label" binding:"required"`
	Budget float64 `json:"budget" binding:"required"`
}

// CreateSubKey 创建客户密钥。
//
// POST /sub-keys
// Authorization: Bearer <account_key>
func (h *AccountAPIHandler) CreateSubKey(c *gin.Context) {
	accountKey := h.accountKeyFromContext(c)
	if accountKey == nil {
		return
	}

	var req createSubKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.AbortWithError(c, 400, "INVALID_REQUEST", err.Error())
		return
	}

	subKey, err := h.apiKeyService.CreateSubKey(c.Request.Context(), accountKey, req.Label, req.Budget)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrAccountKeyRequired):
			middleware.AbortWithError(c, 403, "ACCOUNT_KEY_REQUIRED", err.Error())
		case errors.Is(err, service.ErrAccountKeyGroupRequired):
			middleware.AbortWithError(c, 400, "ACCOUNT_KEY_GROUP_REQUIRED", err.Error())
		case errors.Is(err, service.ErrInvalidBudget):
			middleware.AbortWithError(c, 400, "INVALID_BUDGET", err.Error())
		case errors.Is(err, service.ErrInsufficientAvailableBalance):
			middleware.AbortWithError(c, 400, "INSUFFICIENT_AVAILABLE_BALANCE", err.Error())
		default:
			middleware.AbortWithError(c, 500, "INTERNAL_ERROR", "Failed to create sub key")
		}
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"id":         subKey.ID,
		"key":        subKey.Key,
		"label":      subKey.Name,
		"budget":     subKey.Quota,
		"group_id":   subKey.GroupID,
		"status":     subKey.Status,
		"created_at": subKey.CreatedAt,
	})
}

// ListSubKeys 列出当前账号密钥下的所有客户密钥。
//
// GET /sub-keys?page=1&limit=20
// Authorization: Bearer <account_key>
func (h *AccountAPIHandler) ListSubKeys(c *gin.Context) {
	accountKey := h.accountKeyFromContext(c)
	if accountKey == nil {
		return
	}

	page := parseInt(c.Query("page"), 1)
	limit := parseInt(c.Query("limit"), 20)
	if limit > 100 {
		limit = 100
	}

	keys, total, err := h.apiKeyService.ListSubKeys(c.Request.Context(), accountKey, page, limit)
	if err != nil {
		middleware.AbortWithError(c, 500, "INTERNAL_ERROR", "Failed to list sub keys")
		return
	}

	items := make([]gin.H, len(keys))
	for i, k := range keys {
		remaining := k.Quota - k.QuotaUsed
		if remaining < 0 {
			remaining = 0
		}
		items[i] = gin.H{
			"id":               k.ID,
			"label":            k.Name,
			"budget":           k.Quota,
			"budget_used":      k.QuotaUsed,
			"budget_remaining": remaining,
			"group_id":         k.GroupID,
			"status":           k.Status,
			"created_at":       k.CreatedAt,
			"expires_at":       k.ExpiresAt,
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"items": items,
		"total": total,
		"page":  page,
		"limit": limit,
	})
}

// parseInt parses s as an int, returning def on failure.
func parseInt(s string, def int) int {
	v := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return def
		}
		v = v*10 + int(ch-'0')
	}
	if v == 0 {
		return def
	}
	return v
}

// parseSubKeyID extracts :id from path and aborts with 400 on failure.
func parseSubKeyID(c *gin.Context) (int64, bool) {
	raw := c.Param("id")
	v := int64(0)
	for _, ch := range raw {
		if ch < '0' || ch > '9' {
			middleware.AbortWithError(c, 400, "INVALID_ID", "id must be a positive integer")
			return 0, false
		}
		v = v*10 + int64(ch-'0')
	}
	if v <= 0 {
		middleware.AbortWithError(c, 400, "INVALID_ID", "id must be a positive integer")
		return 0, false
	}
	return v, true
}

// maskedKey returns a masked representation of a key string (shows last 4 chars).
func maskedKey(key string) string {
	if len(key) <= 4 {
		return "••••"
	}
	return "sk-••••••••" + key[len(key)-4:]
}

// updateSubKeyRequest is the request body for PUT /sub-keys/:id.
type updateSubKeyRequest struct {
	Label         *string  `json:"label"`
	BudgetVirtual *float64 `json:"budgetVirtual"`
	Status        *string  `json:"status"`
}

// UpdateSubKey 修改客户密钥。
//
// PUT /sub-keys/:id
// Authorization: Bearer <account_key>
func (h *AccountAPIHandler) UpdateSubKey(c *gin.Context) {
	accountKey := h.accountKeyFromContext(c)
	if accountKey == nil {
		return
	}

	subKeyID, ok := parseSubKeyID(c)
	if !ok {
		return
	}

	var req updateSubKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.AbortWithError(c, 400, "INVALID_REQUEST", err.Error())
		return
	}

	svcReq := service.UpdateSubKeyRequest{
		Label:         req.Label,
		BudgetVirtual: req.BudgetVirtual,
		Status:        req.Status,
	}

	updated, err := h.apiKeyService.UpdateSubKey(c.Request.Context(), accountKey, subKeyID, svcReq)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrAccountKeyRequired):
			middleware.AbortWithError(c, 403, "ACCOUNT_KEY_REQUIRED", err.Error())
		case errors.Is(err, service.ErrSubKeyNotFound):
			middleware.AbortWithError(c, 404, "SUB_KEY_NOT_FOUND", err.Error())
		case errors.Is(err, service.ErrInvalidBudget):
			middleware.AbortWithError(c, 400, "INVALID_BUDGET", err.Error())
		case errors.Is(err, service.ErrBudgetLessThanSpent):
			middleware.AbortWithError(c, 400, "BUDGET_LESS_THAN_SPENT", err.Error())
		case errors.Is(err, service.ErrInsufficientAvailableBalance):
			middleware.AbortWithError(c, 400, "INSUFFICIENT_AVAILABLE_BALANCE", err.Error())
		case errors.Is(err, service.ErrAccountKeyGroupRequired):
			middleware.AbortWithError(c, 400, "ACCOUNT_KEY_GROUP_REQUIRED", err.Error())
		default:
			middleware.AbortWithError(c, 500, "INTERNAL_ERROR", "Failed to update sub key")
		}
		return
	}

	remaining := updated.Quota - updated.QuotaUsed
	if remaining < 0 {
		remaining = 0
	}

	c.JSON(http.StatusOK, gin.H{
		"id":               updated.ID,
		"label":            updated.Name,
		"key_id":           maskedKey(updated.Key),
		"budgetVirtual":    updated.Quota,
		"spentVirtual":     updated.QuotaUsed,
		"remainingVirtual": remaining,
		"status":           updated.Status,
		"createdAt":        updated.CreatedAt,
		"updatedAt":        updated.UpdatedAt,
	})
}

// DeleteSubKey 删除客户密钥（软删除）。
//
// DELETE /sub-keys/:id
// Authorization: Bearer <account_key>
func (h *AccountAPIHandler) DeleteSubKey(c *gin.Context) {
	accountKey := h.accountKeyFromContext(c)
	if accountKey == nil {
		return
	}

	subKeyID, ok := parseSubKeyID(c)
	if !ok {
		return
	}

	err := h.apiKeyService.DeleteSubKey(c.Request.Context(), accountKey, subKeyID)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrAccountKeyRequired):
			middleware.AbortWithError(c, 403, "ACCOUNT_KEY_REQUIRED", err.Error())
		case errors.Is(err, service.ErrSubKeyNotFound):
			middleware.AbortWithError(c, 404, "SUB_KEY_NOT_FOUND", err.Error())
		default:
			middleware.AbortWithError(c, 500, "INTERNAL_ERROR", "Failed to delete sub key")
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// GetSubKeyBalance 查询客户密钥自身的预算状态。
//
// GET /sub-key/balance
// Authorization: Bearer <client_key>
//
// 只允许客户密钥（parent_key_id IS NOT NULL）访问。
// 账号密钥访问返回 403 CLIENT_KEY_REQUIRED。
func (h *AccountAPIHandler) GetSubKeyBalance(c *gin.Context) {
	raw, exists := c.Get(string(middleware.ContextKeyAPIKey))
	if !exists {
		middleware.AbortWithError(c, 401, "API_KEY_REQUIRED", "API key not found in context")
		return
	}
	apiKey, ok := raw.(*service.APIKey)
	if !ok || apiKey == nil {
		middleware.AbortWithError(c, 401, "API_KEY_REQUIRED", "Invalid API key in context")
		return
	}
	if apiKey.ParentKeyID == nil {
		middleware.AbortWithError(c, 403, "CLIENT_KEY_REQUIRED", service.ErrClientKeyRequired.Error())
		return
	}

	quota := apiKey.Quota
	quotaUsed := apiKey.QuotaUsed

	if quota <= 0 {
		// 历史兼容：quota=0 表示无限制
		c.JSON(http.StatusOK, gin.H{
			"budget":    nil,
			"spent":     quotaUsed,
			"remaining": nil,
			"unlimited": true,
			"status":    apiKey.Status,
			"label":     apiKey.Name,
			"unit":      "USD",
		})
		return
	}

	remaining := quota - quotaUsed
	if remaining < 0 {
		remaining = 0
	}

	c.JSON(http.StatusOK, gin.H{
		"budget":    quota,
		"spent":     quotaUsed,
		"remaining": remaining,
		"status":    apiKey.Status,
		"label":     apiKey.Name,
		"unit":      "USD",
	})
}

// GetUsageLogs 查询账号密钥下的用量日志。
//
// GET /usage-logs?page=&limit=&startDate=&endDate=&model=&status=&requestId=&subKeyId=
// Authorization: Bearer <account_key>
//
// 只允许账号密钥（parent_key_id IS NULL）访问；subKeyId 可选，需验证所属权。
// 不记录 usage log；不扣余额；不返回完整 key。
func (h *AccountAPIHandler) GetUsageLogs(c *gin.Context) {
	accountKey := h.accountKeyFromContext(c)
	if accountKey == nil {
		return
	}

	page := parseInt(c.Query("page"), 1)
	limit := parseInt(c.Query("limit"), 20)
	if limit > 100 {
		limit = 100
	}

	filters := usagestats.UsageLogFilters{
		UserID:     accountKey.UserID,
		Model:      strings.TrimSpace(c.Query("model")),
		RequestID:  strings.TrimSpace(c.Query("requestId")),
		ExactTotal: true,
	}

	// Optional subKeyId filter — validate ownership
	if subKeyIDStr := strings.TrimSpace(c.Query("subKeyId")); subKeyIDStr != "" {
		subKeyID, ok := parseInt64Param(c, subKeyIDStr, "subKeyId")
		if !ok {
			return
		}
		subKey, err := h.apiKeyService.GetSubKeyByIDForUser(c.Request.Context(), accountKey.UserID, subKeyID)
		if err != nil {
			if errors.Is(err, service.ErrSubKeyNotFound) || errors.Is(err, service.ErrAPIKeyNotFound) {
				middleware.AbortWithError(c, 404, "SUB_KEY_NOT_FOUND", "sub key not found")
			} else {
				middleware.AbortWithError(c, 500, "INTERNAL_ERROR", "failed to validate sub key")
			}
			return
		}
		filters.APIKeyID = subKey.ID
	}

	// Date range
	if s := strings.TrimSpace(c.Query("startDate")); s != "" {
		t, err := timezone.ParseInUserLocation("2006-01-02", s, "")
		if err != nil {
			middleware.AbortWithError(c, 400, "INVALID_START_DATE", "startDate must be YYYY-MM-DD")
			return
		}
		filters.StartTime = &t
	}
	if s := strings.TrimSpace(c.Query("endDate")); s != "" {
		t, err := timezone.ParseInUserLocation("2006-01-02", s, "")
		if err != nil {
			middleware.AbortWithError(c, 400, "INVALID_END_DATE", "endDate must be YYYY-MM-DD")
			return
		}
		t = t.AddDate(0, 0, 1)
		filters.EndTime = &t
	}

	params := pagination.PaginationParams{
		Page:      page,
		PageSize:  limit,
		SortBy:    "created_at",
		SortOrder: "desc",
	}

	records, result, err := h.usageService.ListWithFilters(c.Request.Context(), params, filters)
	if err != nil {
		middleware.AbortWithError(c, 500, "INTERNAL_ERROR", "failed to list usage logs")
		return
	}

	items := make([]*dto.UsageLog, 0, len(records))
	for i := range records {
		item := dto.UsageLogFromService(&records[i])
		// Do not expose API key value in usage log responses
		if item != nil && item.APIKey != nil {
			item.APIKey.Key = ""
		}
		items = append(items, item)
	}

	c.JSON(http.StatusOK, gin.H{
		"items": items,
		"total": result.Total,
		"page":  page,
		"limit": limit,
	})
}

// GetSubKeyUsageLogs 查询客户密钥自身的用量日志。
//
// GET /sub-key/usage-logs?page=&limit=&startDate=&endDate=&model=&requestId=
// Authorization: Bearer <client_key>
//
// 只允许客户密钥（parent_key_id IS NOT NULL）访问；账号密钥返回 403。
// quota_exhausted 状态的 key 也可查询（skipBilling 覆盖）。
func (h *AccountAPIHandler) GetSubKeyUsageLogs(c *gin.Context) {
	raw, exists := c.Get(string(middleware.ContextKeyAPIKey))
	if !exists {
		middleware.AbortWithError(c, 401, "API_KEY_REQUIRED", "API key not found in context")
		return
	}
	apiKey, ok := raw.(*service.APIKey)
	if !ok || apiKey == nil {
		middleware.AbortWithError(c, 401, "API_KEY_REQUIRED", "invalid API key in context")
		return
	}
	if apiKey.ParentKeyID == nil {
		middleware.AbortWithError(c, 403, "CLIENT_KEY_REQUIRED", service.ErrClientKeyRequired.Error())
		return
	}

	// subKeyId is not applicable on this endpoint
	if strings.TrimSpace(c.Query("subKeyId")) != "" {
		middleware.AbortWithError(c, 400, "INVALID_PARAM", "subKeyId is not supported on this endpoint; this key can only query its own logs")
		return
	}

	page := parseInt(c.Query("page"), 1)
	limit := parseInt(c.Query("limit"), 20)
	if limit > 100 {
		limit = 100
	}

	filters := usagestats.UsageLogFilters{
		UserID:     apiKey.UserID,
		APIKeyID:   apiKey.ID,
		Model:      strings.TrimSpace(c.Query("model")),
		RequestID:  strings.TrimSpace(c.Query("requestId")),
		ExactTotal: true,
	}

	if s := strings.TrimSpace(c.Query("startDate")); s != "" {
		t, err := timezone.ParseInUserLocation("2006-01-02", s, "")
		if err != nil {
			middleware.AbortWithError(c, 400, "INVALID_START_DATE", "startDate must be YYYY-MM-DD")
			return
		}
		filters.StartTime = &t
	}
	if s := strings.TrimSpace(c.Query("endDate")); s != "" {
		t, err := timezone.ParseInUserLocation("2006-01-02", s, "")
		if err != nil {
			middleware.AbortWithError(c, 400, "INVALID_END_DATE", "endDate must be YYYY-MM-DD")
			return
		}
		t = t.AddDate(0, 0, 1)
		filters.EndTime = &t
	}

	params := pagination.PaginationParams{
		Page:      page,
		PageSize:  limit,
		SortBy:    "created_at",
		SortOrder: "desc",
	}

	records, result, err := h.usageService.ListWithFilters(c.Request.Context(), params, filters)
	if err != nil {
		middleware.AbortWithError(c, 500, "INTERNAL_ERROR", "failed to list usage logs")
		return
	}

	items := make([]*dto.UsageLog, 0, len(records))
	for i := range records {
		item := dto.UsageLogFromService(&records[i])
		if item != nil && item.APIKey != nil {
			item.APIKey.Key = ""
		}
		items = append(items, item)
	}

	c.JSON(http.StatusOK, gin.H{
		"items": items,
		"total": result.Total,
		"page":  page,
		"limit": limit,
	})
}

// parseInt64Param parses a string as positive int64 and aborts with 400 on failure.
func parseInt64Param(c *gin.Context, s, paramName string) (int64, bool) {
	v := int64(0)
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			middleware.AbortWithError(c, 400, "INVALID_PARAM", paramName+" must be a positive integer")
			return 0, false
		}
		v = v*10 + int64(ch-'0')
	}
	if v <= 0 {
		middleware.AbortWithError(c, 400, "INVALID_PARAM", paramName+" must be a positive integer")
		return 0, false
	}
	return v, true
}
