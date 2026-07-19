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
// budget is the legacy alias for budgetVirtual; paidAmount defaults to
// budgetVirtual (multiplier 1) when omitted.
type createSubKeyRequest struct {
	Label         string   `json:"label"`
	Budget        *float64 `json:"budget"`
	BudgetVirtual *float64 `json:"budgetVirtual"`
	PaidAmount    *float64 `json:"paidAmount"`
	// GroupID 主通道（可选，默认继承账号密钥分组）
	GroupID *int64 `json:"groupId"`
	// AllowedGroupIDs 额外允许的通道白名单（可选）
	AllowedGroupIDs []int64 `json:"allowedGroupIds"`
}

// subKeyDTO renders a sub key with both the customer-facing virtual amounts
// and the account's real amounts. Includes the full key: the account key can
// always retrieve its own sub keys' secrets (same policy as the account key).
func subKeyDTO(k *service.APIKey) gin.H {
	multiplier := service.EffectiveDisplayMultiplier(k)
	remaining := k.Quota - k.QuotaUsed
	if remaining < 0 {
		remaining = 0
	}
	return gin.H{
		"id":     k.ID,
		"label":  k.Name,
		"key":    k.Key,
		"key_id": maskedKey(k.Key),

		"budgetVirtual":    k.Quota * multiplier,
		"spentVirtual":     k.QuotaUsed * multiplier,
		"remainingVirtual": remaining * multiplier,

		"paidAmount":        k.Quota,
		"spentAmount":       k.QuotaUsed,
		"remainingAmount":   remaining,
		"displayMultiplier": multiplier,

		"group_id":          k.GroupID,
		"allowed_group_ids": k.AllowedGroupIDs,
		"status":            k.Status,
		"createdAt":         k.CreatedAt,
		"updatedAt":         k.UpdatedAt,
		"expires_at":        k.ExpiresAt,
	}
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

	// budget (legacy) and budgetVirtual are aliases
	budgetVirtual := 0.0
	if req.BudgetVirtual != nil {
		budgetVirtual = *req.BudgetVirtual
	} else if req.Budget != nil {
		budgetVirtual = *req.Budget
	}
	if budgetVirtual <= 0 {
		middleware.AbortWithError(c, 400, "INVALID_BUDGET", "budgetVirtual must be greater than 0")
		return
	}

	// paidAmount defaults to budgetVirtual (multiplier 1)
	paidAmount := budgetVirtual
	if req.PaidAmount != nil {
		paidAmount = *req.PaidAmount
	}

	subKey, err := h.apiKeyService.CreateSubKey(c.Request.Context(), accountKey, req.Label, budgetVirtual, paidAmount, service.CreateSubKeyOptions{
		GroupID:         req.GroupID,
		AllowedGroupIDs: req.AllowedGroupIDs,
	})
	if err != nil {
		switch {
		case errors.Is(err, service.ErrAccountKeyRequired):
			middleware.AbortWithError(c, 403, "ACCOUNT_KEY_REQUIRED", err.Error())
		case errors.Is(err, service.ErrAccountKeyGroupRequired):
			middleware.AbortWithError(c, 400, "ACCOUNT_KEY_GROUP_REQUIRED", err.Error())
		case errors.Is(err, service.ErrGroupNotAllowed):
			middleware.AbortWithError(c, 403, "GROUP_NOT_ALLOWED", err.Error())
		case errors.Is(err, service.ErrGroupNotFound):
			middleware.AbortWithError(c, 404, "GROUP_NOT_FOUND", err.Error())
		case errors.Is(err, service.ErrInvalidBudget):
			middleware.AbortWithError(c, 400, "INVALID_BUDGET", err.Error())
		case errors.Is(err, service.ErrInvalidMultiplier):
			middleware.AbortWithError(c, 400, "INVALID_MULTIPLIER", err.Error())
		case errors.Is(err, service.ErrInsufficientAvailableBalance):
			middleware.AbortWithError(c, 400, "INSUFFICIENT_AVAILABLE_BALANCE", err.Error())
		default:
			middleware.AbortWithError(c, 500, "INTERNAL_ERROR", "Failed to create sub key")
		}
		return
	}

	c.JSON(http.StatusCreated, subKeyDTO(subKey))
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
	for i := range keys {
		items[i] = subKeyDTO(&keys[i])
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
	PaidAmount    *float64 `json:"paidAmount"`
	Status        *string  `json:"status"`
	// GroupID 主通道变更（缺省 = 不变）
	GroupID *int64 `json:"groupId"`
	// AllowedGroupIDs 通道白名单整体替换（缺省 = 不变；[] = 清空锁回主通道）
	AllowedGroupIDs *[]int64 `json:"allowedGroupIds"`
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
		Label:           req.Label,
		BudgetVirtual:   req.BudgetVirtual,
		PaidAmount:      req.PaidAmount,
		Status:          req.Status,
		GroupID:         req.GroupID,
		AllowedGroupIDs: req.AllowedGroupIDs,
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
		case errors.Is(err, service.ErrInvalidMultiplier):
			middleware.AbortWithError(c, 400, "INVALID_MULTIPLIER", err.Error())
		case errors.Is(err, service.ErrBudgetLessThanSpent):
			middleware.AbortWithError(c, 400, "BUDGET_LESS_THAN_SPENT", err.Error())
		case errors.Is(err, service.ErrInsufficientAvailableBalance):
			middleware.AbortWithError(c, 400, "INSUFFICIENT_AVAILABLE_BALANCE", err.Error())
		case errors.Is(err, service.ErrInvalidStatus):
			middleware.AbortWithError(c, 400, "INVALID_STATUS", err.Error())
		case errors.Is(err, service.ErrGroupNotAllowed):
			middleware.AbortWithError(c, 403, "GROUP_NOT_ALLOWED", err.Error())
		case errors.Is(err, service.ErrGroupNotFound):
			middleware.AbortWithError(c, 404, "GROUP_NOT_FOUND", err.Error())
		case errors.Is(err, service.ErrAccountKeyGroupRequired):
			middleware.AbortWithError(c, 400, "ACCOUNT_KEY_GROUP_REQUIRED", err.Error())
		default:
			middleware.AbortWithError(c, 500, "INTERNAL_ERROR", "Failed to update sub key")
		}
		return
	}

	c.JSON(http.StatusOK, subKeyDTO(updated))
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
	multiplier := service.EffectiveDisplayMultiplier(apiKey)

	if quota <= 0 {
		// 历史兼容：quota=0 表示无限制
		c.JSON(http.StatusOK, gin.H{
			"budget":    nil,
			"spent":     quotaUsed * multiplier,
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
		"budget":    quota * multiplier,
		"spent":     quotaUsed * multiplier,
		"remaining": remaining * multiplier,
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
