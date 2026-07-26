package handler

import (
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Phase 21E-6C-2B-1 / 2C: Provider Portal 内部接入面。
// 承载 onboarding-session 创建与授权完成；鉴权由
// ProviderInternalAuthMiddleware 在路由层完成。响应体形状与 Portal
// 侧 sub2api-client 的契约对齐。
// Phase 21E-6E-4: 追加单条 credential 导入（import-credentials）。
type ProviderConnectHandler struct {
	connect    *service.ProviderConnectService
	completion *service.ProviderConnectCompletionService
	importSvc  *service.ProviderConnectImportService
	allocator  *service.ProxyAllocator
	metrics    *service.ProviderAccountMetricsService
	regions    *service.RegionService
	deactivate *service.ProviderAccountDeactivationService
}

// NewProviderConnectHandler creates the handler.
func NewProviderConnectHandler(
	connect *service.ProviderConnectService,
	completion *service.ProviderConnectCompletionService,
	importSvc *service.ProviderConnectImportService,
	allocator *service.ProxyAllocator,
	metrics *service.ProviderAccountMetricsService,
	regions *service.RegionService,
	deactivate *service.ProviderAccountDeactivationService,
) *ProviderConnectHandler {
	return &ProviderConnectHandler{connect: connect, completion: completion, importSvc: importSvc, allocator: allocator, metrics: metrics, regions: regions, deactivate: deactivate}
}

// Regions handles
// GET /internal/provider-accounts/regions
//
// 返回启用的 region 字典（code/name_en/name_zh），供 Portal 解析账号卡上的
// region 徽章（code → 中/英名）。脱敏：只含展示名，不含任何代理/IP 信息。
func (h *ProviderConnectHandler) Regions(c *gin.Context) {
	list, err := h.regions.List(c.Request.Context(), true)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	out := make([]gin.H, 0, len(list))
	for i := range list {
		out = append(out, gin.H{
			"code":    list[i].Code,
			"name_en": list[i].NameEn,
			"name_zh": list[i].NameZh,
		})
	}
	response.Success(c, gin.H{"regions": out})
}

// AccountMetrics handles
// GET /internal/provider-accounts/:external_ref/metrics
//
// 返回单账号的脱敏运行指标（状态/并发/用量窗口/配额/订阅等级/今日请求）。
// 绝不含 proxy/IP/host/凭证。由 external_provider_account_id 定位。
func (h *ProviderConnectHandler) AccountMetrics(c *gin.Context) {
	externalRef := strings.TrimSpace(c.Param("external_ref"))
	if externalRef == "" || !strings.HasPrefix(externalRef, "pa_") {
		response.ErrorFrom(c, infraerrors.BadRequest("INVALID_REQUEST", "invalid external_provider_account_id"))
		return
	}
	m, err := h.metrics.Metrics(c.Request.Context(), externalRef)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, m)
}

// deactivateRequest is the (optional) body for Deactivate.
type deactivateRequest struct {
	Reason string `json:"reason"`
}

// Deactivate handles
// POST /internal/provider-accounts/:external_ref/deactivate
//
// 停用并解绑一个 Provider 账号：停止调度（schedulable=false）+ 置为 disabled，
// 立即移出调度池；历史用量/账单不动。容量按"活跃账号计数"实时回收，无需显式
// 释放。幂等：未知 ref / 已停用均为成功。由 external_provider_account_id 定位。
func (h *ProviderConnectHandler) Deactivate(c *gin.Context) {
	externalRef := strings.TrimSpace(c.Param("external_ref"))
	if externalRef == "" || !strings.HasPrefix(externalRef, "pa_") {
		response.ErrorFrom(c, infraerrors.BadRequest("INVALID_REQUEST", "invalid external_provider_account_id"))
		return
	}
	var req deactivateRequest
	_ = c.ShouldBindJSON(&req) // body optional; ignore parse errors
	result, err := h.deactivate.Deactivate(c.Request.Context(), externalRef, req.Reason)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

// AvailableRegions handles
// GET /internal/provider-accounts/available-regions?provider_type=claude
//
// 返回脱敏的 region 能力（id/label/capacity 档位），容量按 provider_type 对应
// 的平台分桶（claude 与 codex 各自独立）。绝不含 proxy/IP/host，供 Portal 后端
// 透传给 Provider 前端，浏览器不得直连本接口。
func (h *ProviderConnectHandler) AvailableRegions(c *gin.Context) {
	platform, ok := service.PlatformForProviderType(c.Query("provider_type"))
	if !ok {
		response.ErrorFrom(c, infraerrors.BadRequest("CONNECT_INVALID_PROVIDER_TYPE", "invalid provider_type"))
		return
	}
	regions, err := h.allocator.AvailableRegions(c.Request.Context(), platform)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if regions == nil {
		regions = []service.AvailableRegion{}
	}
	response.Success(c, gin.H{"regions": regions})
}

type createOnboardingSessionRequest struct {
	ExternalProviderAccountID string `json:"external_provider_account_id" binding:"required"`
	ProviderType              string `json:"provider_type" binding:"required"`
	Region                    string `json:"region" binding:"required"`
	CallbackURL               string `json:"callback_url" binding:"required"`
}

// CreateOnboardingSession handles
// POST /internal/provider-accounts/onboarding-sessions
func (h *ProviderConnectHandler) CreateOnboardingSession(c *gin.Context) {
	var req createOnboardingSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.ErrorFrom(c, infraerrors.BadRequest("CONNECT_INVALID_BODY", "invalid request body"))
		return
	}
	result, err := h.connect.CreateOnboardingSession(c.Request.Context(), service.CreateOnboardingSessionInput{
		ExternalProviderAccountID: req.ExternalProviderAccountID,
		ProviderType:              req.ProviderType,
		Region:                    req.Region,
		CallbackURL:               req.CallbackURL,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

type completeAuthorizationRequest struct {
	SessionID string `json:"session_id" binding:"required"`
	Code      string `json:"code" binding:"required"`
}

// CompleteAuthorization handles
// POST /internal/provider/connect/complete
func (h *ProviderConnectHandler) CompleteAuthorization(c *gin.Context) {
	var req completeAuthorizationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.ErrorFrom(c, infraerrors.BadRequest("CONNECT_INVALID_BODY", "invalid request body"))
		return
	}
	sessionID, ok := parseConnectSessionID(req.SessionID)
	if !ok {
		response.ErrorFrom(c, infraerrors.BadRequest("CONNECT_INVALID_SESSION_ID", "invalid session_id"))
		return
	}
	result, err := h.completion.CompleteAuthorization(c.Request.Context(), service.CompleteAuthorizationInput{
		SessionID: sessionID,
		Code:      req.Code,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

// importCredentialsRequest 单条 credential 导入请求（Phase 21E-6E-4）。
// 字段名与 Portal 侧 sub2api-client 的实际请求体对齐。credential 是敏感
// 字段：只在内存流转，绝不入日志/错误/响应。callback_url 与 onboarding
// 一致由 Portal 传入（本阶段 webhook 目标由 Sub2API 配置决定，接收但不强依赖）。
type importCredentialsRequest struct {
	ExternalProviderAccountID string `json:"external_provider_account_id" binding:"required"`
	ProviderType              string `json:"provider_type" binding:"required"`
	Credential                string `json:"credential" binding:"required"`
	Region                    string `json:"region" binding:"required"`
	CallbackURL               string `json:"callback_url"`
}

// importCredentialsResponse 安全响应体（Portal 读取 sub2api_account_id）。
// sub2api_account_id 用字符串形态，与 activated webhook / Portal client
// 的 { sub2api_account_id?: string } 契约一致。
type importCredentialsResponse struct {
	Status           string `json:"status"`
	Sub2apiAccountID string `json:"sub2api_account_id"`
}

// ImportCredentials handles
// POST /internal/provider-accounts/import-credentials
//
// 单条导入。批量/失败隔离/限额由 Portal 编排；此处只处理一条。
func (h *ProviderConnectHandler) ImportCredentials(c *gin.Context) {
	var req importCredentialsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		// 绝不回显 body（含 credential）——只给稳定错误码。
		response.ErrorFrom(c, infraerrors.BadRequest("INVALID_REQUEST", "invalid request body"))
		return
	}
	result, err := h.importSvc.ImportCredential(c.Request.Context(), service.ImportCredentialInput{
		ExternalProviderAccountID: req.ExternalProviderAccountID,
		ProviderType:              req.ProviderType,
		Credential:                req.Credential,
		Region:                    req.Region,
	})
	if err != nil {
		// service 层已把底层错误统一成安全错误码（无 credential）。
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, importCredentialsResponse{
		Status:           result.Status,
		Sub2apiAccountID: strconv.FormatInt(result.Sub2apiAccountID, 10),
	})
}
func parseConnectSessionID(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "obs_")
	if s == "" {
		return 0, false
	}
	var id int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		id = id*10 + int64(r-'0')
	}
	return id, true
}
