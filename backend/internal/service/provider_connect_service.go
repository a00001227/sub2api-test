package service

import (
	"context"
	"net/url"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Phase 21E-6C-2B-1: Provider Portal 渠道商账号接入会话。
//
// 这是 Provider Connect 扩展层的编排入口：分配代理 → 落库会话 →
// 复用现有 OAuthService 生成授权 URL。OAuthService 本体零改动 ——
// 本服务只是它的一个新调用方（与 admin handler 平级）。
//
// 本阶段不含：webhook 外发、授权码交换、账号最终创建（后续阶段）。

// providerConnectSessionTTL 会话有效期。与 Portal 侧 onboarding 会话
// 短时单次的约定对齐（Portal mock 会话为 10 分钟）。
const providerConnectSessionTTL = 10 * time.Minute

// providerConnectAllowedTypes 允许接入的平台类型（与 Portal 侧
// CreateProviderAccountDto 的 IsIn 白名单对齐）。
var providerConnectAllowedTypes = map[string]struct{}{
	"claude": {}, "codex": {}, "openai": {}, "gemini": {},
}

var (
	// ErrConnectInvalidAccountRef external_provider_account_id 非法。
	ErrConnectInvalidAccountRef = infraerrors.BadRequest(
		"CONNECT_INVALID_ACCOUNT_REF", "external_provider_account_id is required (pa_ prefixed, <=64 chars)")
	// ErrConnectInvalidProviderType 平台类型不在白名单。
	ErrConnectInvalidProviderType = infraerrors.BadRequest(
		"CONNECT_INVALID_PROVIDER_TYPE", "provider_type must be one of claude/codex/openai/gemini")
	// ErrConnectInvalidCallbackURL 回调地址非法。
	ErrConnectInvalidCallbackURL = infraerrors.BadRequest(
		"CONNECT_INVALID_CALLBACK_URL", "callback_url must be an absolute http(s) URL (<=512 chars)")
)

// ProviderConnectSession 是会话的领域对象（与 provider_connect_sessions 行对应）。
type ProviderConnectSession struct {
	ID                        int64
	ExternalProviderAccountID string
	ProviderType              string
	Region                    *string
	ProxyID                   *int64
	Status                    string
	OAuthSessionID            *string
	Sub2apiAccountID          *int64
	CallbackURL               string
	ExpiresAt                 time.Time
	CompletedAt               *time.Time
	CreatedAt                 time.Time
}

// ProviderConnectSessionRepository 会话仓储面。
type ProviderConnectSessionRepository interface {
	Create(ctx context.Context, s *ProviderConnectSession) (*ProviderConnectSession, error)
	GetByID(ctx context.Context, id int64) (*ProviderConnectSession, error)
	// MarkCompleted 原子地把 pending 会话置为 completed 并写入
	// sub2api_account_id / completed_at。返回受影响行数——0 表示会话
	// 已非 pending（幂等保护）。
	MarkCompleted(ctx context.Context, id int64, sub2apiAccountID int64, completedAt time.Time) (int64, error)
	// MarkFailed 把会话置为 failed（best-effort）。
	MarkFailed(ctx context.Context, id int64) error
}

// connectOAuthURLGenerator 是本服务对 OAuthService 的最小依赖面
// （*OAuthService 天然满足；接口化仅为可测性）。
type connectOAuthURLGenerator interface {
	GenerateAuthURL(ctx context.Context, proxyID *int64) (*GenerateAuthURLResult, error)
}

// CreateOnboardingSessionInput Portal 发起的接入请求。
type CreateOnboardingSessionInput struct {
	ExternalProviderAccountID string
	ProviderType              string
	Region                    string
	CallbackURL               string
}

// CreateOnboardingSessionResult 返回给 Portal 的会话信息。
// 字段名与 Portal 侧 sub2api-client 期望的响应契约对齐。
type CreateOnboardingSessionResult struct {
	OnboardingSessionID string    `json:"onboarding_session_id"`
	OnboardingURL       string    `json:"onboarding_url"`
	ExpiresAt           time.Time `json:"expires_at"`
}

// ProviderConnectService 编排渠道商接入会话的创建。
type ProviderConnectService struct {
	sessions  ProviderConnectSessionRepository
	allocator *ProxyAllocator
	oauth     connectOAuthURLGenerator
	now       func() time.Time // 可注入时钟，便于测试
}

// NewProviderConnectService creates the service.
func NewProviderConnectService(
	sessions ProviderConnectSessionRepository,
	allocator *ProxyAllocator,
	oauth *OAuthService,
) *ProviderConnectService {
	return &ProviderConnectService{
		sessions:  sessions,
		allocator: allocator,
		oauth:     oauth,
		now:       time.Now,
	}
}

// CreateOnboardingSession 创建一个渠道商接入会话：
// 校验参数 → 按 region 分配代理 → 落库 pending 会话 → 生成 OAuth URL。
func (s *ProviderConnectService) CreateOnboardingSession(
	ctx context.Context, input CreateOnboardingSessionInput,
) (*CreateOnboardingSessionResult, error) {
	// 1) 参数校验（fail-fast，全部机器码错误）
	accountRef := strings.TrimSpace(input.ExternalProviderAccountID)
	if accountRef == "" || len(accountRef) > 64 || !strings.HasPrefix(accountRef, "pa_") {
		return nil, ErrConnectInvalidAccountRef
	}
	providerType := strings.ToLower(strings.TrimSpace(input.ProviderType))
	if _, ok := providerConnectAllowedTypes[providerType]; !ok {
		return nil, ErrConnectInvalidProviderType
	}
	// 容量按 platform 分桶（B3）：把 provider_type 解析为账号平台
	// （claude→anthropic、codex→openai），供分配器按平台各自计数。
	platform, ok := providerTypeToPlatform[providerType]
	if !ok {
		return nil, ErrConnectInvalidProviderType
	}
	if err := validateConnectCallbackURL(input.CallbackURL); err != nil {
		return nil, err
	}

	// 2) 分配代理（region 校验在 allocator 内；无容量即失败，不降级直连）
	proxy, err := s.allocator.SelectProxy(ctx, input.Region, platform)
	if err != nil {
		return nil, err
	}

	// 3) 复用现有 OAuthService 生成授权 URL（零改动调用）。
	//    先于会话落库：GenerateAuthURL 在 OAuth 内存 SessionStore 里创建
	//    了带 state/codeVerifier/proxyURL 的会话，其 SessionID 是后续
	//    ExchangeCode 的必需入参（21E-6C-2B-1 的遗漏修复：必须保存它）。
	//    OAuth 失败则整体失败、不留孤儿会话。
	authRes, err := s.oauth.GenerateAuthURL(ctx, &proxy.ID)
	if err != nil {
		return nil, err
	}

	// 4) 落库 pending 会话（会话是完成流程的事实锚点），保存
	//    oauth_session_id 以便 completion 阶段调用 ExchangeCode。
	region := strings.ToUpper(strings.TrimSpace(input.Region))
	expiresAt := s.now().Add(providerConnectSessionTTL)
	oauthSessionID := authRes.SessionID
	session, err := s.sessions.Create(ctx, &ProviderConnectSession{
		ExternalProviderAccountID: accountRef,
		ProviderType:              providerType,
		Region:                    &region,
		ProxyID:                   &proxy.ID,
		Status:                    "pending",
		OAuthSessionID:            &oauthSessionID,
		CallbackURL:               strings.TrimSpace(input.CallbackURL),
		ExpiresAt:                 expiresAt,
	})
	if err != nil {
		return nil, err
	}

	return &CreateOnboardingSessionResult{
		OnboardingSessionID: formatConnectSessionID(session.ID),
		OnboardingURL:       authRes.AuthURL,
		ExpiresAt:           expiresAt,
	}, nil
}

// formatConnectSessionID 对外暴露带前缀的会话标识（obs_ = onboarding session）。
func formatConnectSessionID(id int64) string {
	return "obs_" + itoa(id)
}

func itoa(v int64) string {
	// 小工具，避免为一处转换引入 strconv 的显式依赖歧义
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func validateConnectCallbackURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 512 {
		return ErrConnectInvalidCallbackURL
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return ErrConnectInvalidCallbackURL
	}
	return nil
}
