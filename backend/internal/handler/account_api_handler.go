package handler

import (
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// AccountAPIHandler 处理账号密钥（Account Key）相关的 API 接口。
// 这些接口通过 API Key 鉴权（不使用 JWT），面向持有账号密钥的调用方。
type AccountAPIHandler struct {
	userService *service.UserService
}

// NewAccountAPIHandler 创建 AccountAPIHandler。
func NewAccountAPIHandler(userService *service.UserService) *AccountAPIHandler {
	return &AccountAPIHandler{userService: userService}
}

// Balance 查询账户余额。
//
// GET /balance
// Authorization: Bearer <account_key>
//
// 响应：
//
//	{
//	  "balance": 100.0,
//	  "locked_balance": 0,
//	  "available_balance": 100.0,
//	  "unit": "USD"
//	}
//
// locked_balance 本阶段恒为 0（客户密钥预算锁定在 /sub-keys 实现后再计算）。
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

	c.JSON(http.StatusOK, gin.H{
		"balance":           balance,
		"locked_balance":    0,
		"available_balance": balance,
		"unit":              "USD",
	})
}
