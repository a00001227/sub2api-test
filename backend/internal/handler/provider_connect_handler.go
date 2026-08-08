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
	reauth     *service.ProviderConnectReauthService
	pacing     *service.ProviderAccountPacingService
	scheduling *service.ProviderAccountSchedulingService
	test       *service.ProviderAccountTestService
	config     *service.ProviderAccountConfigService
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
	reauth *service.ProviderConnectReauthService,
	pacing *service.ProviderAccountPacingService,
	scheduling *service.ProviderAccountSchedulingService,
	test *service.ProviderAccountTestService,
	config *service.ProviderAccountConfigService,
) *ProviderConnectHandler {
	return &ProviderConnectHandler{connect: connect, completion: completion, importSvc: importSvc, allocator: allocator, metrics: metrics, regions: regions, deactivate: deactivate, reauth: reauth, pacing: pacing, scheduling: scheduling, test: test, config: config}
}

// accountConfigRuleRequest 是一条临时不可调度规则的请求体。
type accountConfigRuleRequest struct {
	ErrorCode       int      `json:"error_code"`
	Keywords        []string `json:"keywords"`
	DurationMinutes int      `json:"duration_minutes"`
	Description     string   `json:"description"`
}

// accountConfigRequest 是 SetAccountConfig 的部分更新体(字段缺省/nil=不改该项)。
type accountConfigRequest struct {
	// model_mapping 缺省(nil)=不动;提供(含 {})=覆盖白名单/映射。
	ModelMapping       map[string]string           `json:"model_mapping"`
	Priority           *int                        `json:"priority"`
	MaxSessions        *int                        `json:"max_sessions"`
	InterceptWarmup    *bool                       `json:"intercept_warmup"`
	TempUnschedEnabled *bool                       `json:"temp_unschedulable_enabled"`
	TempUnschedRules   *[]accountConfigRuleRequest `json:"temp_unschedulable_rules"`
}

// GetAccountConfig handles GET /internal/provider-accounts/:external_ref/config
// 回读单账号可配置项(脱敏,不含 token/凭据),供 Portal 编辑器展示当前值。
func (h *ProviderConnectHandler) GetAccountConfig(c *gin.Context) {
	externalRef := strings.TrimSpace(c.Param("external_ref"))
	if externalRef == "" || !strings.HasPrefix(externalRef, "pa_") {
		response.ErrorFrom(c, infraerrors.BadRequest("INVALID_REQUEST", "invalid external_provider_account_id"))
		return
	}
	cfg, err := h.config.GetConfig(c.Request.Context(), externalRef)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, cfg)
}

// SetAccountConfig handles POST /internal/provider-accounts/:external_ref/config
// 部分更新账号配置(#90-A2/B)。"仅 admin"由 Portal 侧 AdminGuard 把关。
// 目前接 model_mapping(平台默认白名单下发 + admin 手动改共用此端点)。
func (h *ProviderConnectHandler) SetAccountConfig(c *gin.Context) {
	externalRef := strings.TrimSpace(c.Param("external_ref"))
	if externalRef == "" || !strings.HasPrefix(externalRef, "pa_") {
		response.ErrorFrom(c, infraerrors.BadRequest("INVALID_REQUEST", "invalid external_provider_account_id"))
		return
	}
	var req accountConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.ErrorFrom(c, infraerrors.BadRequest("CONNECT_INVALID_BODY", "invalid request body"))
		return
	}
	input := service.ProviderAccountConfigInput{
		ModelMapping:       req.ModelMapping,
		Priority:           req.Priority,
		MaxSessions:        req.MaxSessions,
		InterceptWarmup:    req.InterceptWarmup,
		TempUnschedEnabled: req.TempUnschedEnabled,
	}
	if req.TempUnschedRules != nil {
		rules := make([]service.TempUnschedulableRule, 0, len(*req.TempUnschedRules))
		for _, r := range *req.TempUnschedRules {
			rules = append(rules, service.TempUnschedulableRule{
				ErrorCode:       r.ErrorCode,
				Keywords:        r.Keywords,
				DurationMinutes: r.DurationMinutes,
				Description:     r.Description,
			})
		}
		input.TempUnschedRules = &rules
	}
	cfg, err := h.config.SetConfig(c.Request.Context(), externalRef, input)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, cfg)
}

// testConnectionRequest is the OPTIONAL body for TestConnection (all fields
// default when absent: platform default model / prompt / mode).
type testConnectionRequest struct {
	ModelID string `json:"model_id"`
	Prompt  string `json:"prompt"`
	Mode    string `json:"mode"`
}

// TestConnection handles
// POST /internal/provider-accounts/:external_ref/test
//
// 触发单账号的真实连通性测试(#90)。"仅 admin"由 Portal 侧 AdminGuard 把关;
// 本端仍按 provider-internal token 鉴权。复用 sub2api 完整测试逻辑,归纳为 JSON。
// 注意:失败可能把账号置为 error/限流(与后台"测试"一致),并连带触发回流(#87)。
func (h *ProviderConnectHandler) TestConnection(c *gin.Context) {
	externalRef := strings.TrimSpace(c.Param("external_ref"))
	if externalRef == "" || !strings.HasPrefix(externalRef, "pa_") {
		response.ErrorFrom(c, infraerrors.BadRequest("INVALID_REQUEST", "invalid external_provider_account_id"))
		return
	}
	var req testConnectionRequest
	_ = c.ShouldBindJSON(&req) // body optional — empty body leaves zero values
	result, err := h.test.Test(
		c.Request.Context(),
		externalRef,
		strings.TrimSpace(req.ModelID),
		req.Prompt,
		strings.TrimSpace(req.Mode),
	)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
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

// reauthSessionRequest 重授权会话请求（Phase 21G）。
type reauthSessionRequest struct {
	ProviderType string `json:"provider_type" binding:"required"`
	CallbackURL  string `json:"callback_url" binding:"required"`
}

// CreateReauthSession handles
// POST /internal/provider-accounts/:external_ref/reauth-sessions
//
// 为一个已存在（凭证失效）的账号创建重授权会话：复用其绑定 proxy 生成
// OAuth URL，落 pending 会话并预填 sub2api_account_id。完成仍走统一的
// /internal/provider/connect/complete —— 完成流程见到预填账号 id 即更新
// 凭证而非新建账号。响应契约与 onboarding-sessions 相同。
func (h *ProviderConnectHandler) CreateReauthSession(c *gin.Context) {
	externalRef := strings.TrimSpace(c.Param("external_ref"))
	if externalRef == "" || !strings.HasPrefix(externalRef, "pa_") {
		response.ErrorFrom(c, infraerrors.BadRequest("INVALID_REQUEST", "invalid external_provider_account_id"))
		return
	}
	var req reauthSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.ErrorFrom(c, infraerrors.BadRequest("CONNECT_INVALID_BODY", "invalid request body"))
		return
	}
	result, err := h.reauth.CreateReauthSession(c.Request.Context(), service.CreateReauthSessionInput{
		ExternalProviderAccountID: externalRef,
		ProviderType:              req.ProviderType,
		CallbackURL:               req.CallbackURL,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

// pacingModeRequest 设置调度档位请求（Phase 21H）。
type pacingModeRequest struct {
	Mode string `json:"mode" binding:"required"`
}

// SetPacingMode handles
// POST /internal/provider-accounts/:external_ref/pacing-mode
//
// 设置账号的调度档位（DeRouter 五档 humanized/standard/speed_2x/3x/5x，
// 兼容旧名 steady/smart/burst），写 extra["pacing_mode"]，调度侧即时生效。幂等。
func (h *ProviderConnectHandler) SetPacingMode(c *gin.Context) {
	externalRef := strings.TrimSpace(c.Param("external_ref"))
	if externalRef == "" || !strings.HasPrefix(externalRef, "pa_") {
		response.ErrorFrom(c, infraerrors.BadRequest("INVALID_REQUEST", "invalid external_provider_account_id"))
		return
	}
	var req pacingModeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.ErrorFrom(c, infraerrors.BadRequest("CONNECT_INVALID_BODY", "invalid request body"))
		return
	}
	result, err := h.pacing.SetPacingMode(c.Request.Context(), externalRef, req.Mode)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

// schedulingRequest 暂停/恢复调度请求（Phase 21I）。
type schedulingRequest struct {
	Enabled *bool `json:"enabled" binding:"required"`
}

// SetScheduling handles
// POST /internal/provider-accounts/:external_ref/scheduling
//
// 可逆地暂停/恢复账号调度：enabled=false 暂停（schedulable=false），
// enabled=true 恢复。只切 schedulable 布尔，绝不动 status。幂等。
func (h *ProviderConnectHandler) SetScheduling(c *gin.Context) {
	externalRef := strings.TrimSpace(c.Param("external_ref"))
	if externalRef == "" || !strings.HasPrefix(externalRef, "pa_") {
		response.ErrorFrom(c, infraerrors.BadRequest("INVALID_REQUEST", "invalid external_provider_account_id"))
		return
	}
	var req schedulingRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Enabled == nil {
		response.ErrorFrom(c, infraerrors.BadRequest("CONNECT_INVALID_BODY", "invalid request body"))
		return
	}
	result, err := h.scheduling.SetScheduling(c.Request.Context(), externalRef, *req.Enabled)
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
