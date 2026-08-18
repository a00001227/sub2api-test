//go:build unit

package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 切片 5.1 §六：真实 RBAC —— 经 registerRiskV2Routes + 真实 admin route group + 现有 AdminAuthMiddleware。
// 不新建/不修改鉴权模型；只验证注册的 v2 路由继承 admin 鉴权（未认证 401 / 普通用户 403 / admin 200）。

// rbacStubUserRepo 只覆盖 GetByID，其余方法内嵌接口（不会被 admin-JWT 路径调用）。
type rbacStubUserRepo struct {
	service.UserRepository
	users map[int64]*service.User
}

func (r *rbacStubUserRepo) GetByID(ctx context.Context, id int64) (*service.User, error) {
	if u, ok := r.users[id]; ok {
		clone := *u
		return &clone, nil
	}
	return nil, service.ErrUserNotFound
}

// GetUserAvatar：GetByID 内部 hydrate 头像会调用；返回空即可（不影响 RBAC 判定）。
func (r *rbacStubUserRepo) GetUserAvatar(ctx context.Context, userID int64) (*service.UserAvatar, error) {
	return nil, nil
}

func TestRiskV2API_RealRBAC(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{JWT: config.JWTConfig{Secret: "test-secret", ExpireHour: 1}}
	authService := service.NewAuthService(nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil)

	adminUser := &service.User{ID: 1, Email: "admin@x", Role: service.RoleAdmin, Status: service.StatusActive, TokenVersion: 1}
	normalUser := &service.User{ID: 2, Email: "user@x", Role: service.RoleUser, Status: service.StatusActive, TokenVersion: 1}
	userRepo := &rbacStubUserRepo{users: map[int64]*service.User{1: adminUser, 2: normalUser}}
	userService := service.NewUserService(userRepo, nil, nil, nil)

	adminAuth := middleware.NewAdminAuthMiddleware(authService, userService, nil)

	// 真实 RiskV2 handler（status 走 nil status provider → disabled，无需 DB/Redis）。
	svc := service.NewRiskV2AdminService(nil, nil, nil, nil, time.Hour, time.Second, func() time.Time { return time.Unix(1000, 0) })
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{RiskV2: admin.NewRiskV2Handler(svc, 1000, 1000)}}

	engine := gin.New()
	adminGrp := engine.Group("/api/v1/admin")
	adminGrp.Use(gin.HandlerFunc(adminAuth))
	registerRiskV2Routes(adminGrp, handlers) // ← 被测：真实注册函数

	req := func(bearer string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/v1/admin/risk/v2/status", nil)
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		engine.ServeHTTP(w, r)
		return w.Code
	}

	// 未认证 → 401。
	require.Equal(t, http.StatusUnauthorized, req(""))

	// 普通用户 → 403。
	userTok, err := authService.GenerateToken(&service.User{ID: 2, Email: normalUser.Email, Role: service.RoleUser, TokenVersion: 1})
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, req(userTok))

	// 合法 admin → 200。
	adminTok, err := authService.GenerateToken(&service.User{ID: 1, Email: adminUser.Email, Role: service.RoleAdmin, TokenVersion: 1})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, req(adminTok))
}
