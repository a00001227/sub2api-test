package service

import (
	"context"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Phase 21G provider-reauth: 渠道商账号重新授权（凭证过期 → 原地换新令牌）。
//
// 与 onboarding 的差别只有一处：账号已存在。因此会话在创建时就预填
// sub2api_account_id —— 这既是"重授权会话"的标记（无需 schema 变更），也
// 让完成流程知道该更新哪个账号。完成复用同一个 /connect/complete 端点：
// CompleteAuthorization 看到 pending 会话带非空 sub2api_account_id 时走
// 换凭证分支（更新 credentials + 恢复 active/schedulable），不建第二个账号。
//
// 代理复用：重授权必须走该账号已绑定的 proxy（同一出口环境），既保持
// "一账号一出口"不变量，也不触碰容量计数（账号本来就占着这个位）。

var (
	// ErrReauthAccountNotFound 按 external_ref 找不到账号。
	ErrReauthAccountNotFound = infraerrors.NotFound(
		"REAUTH_ACCOUNT_NOT_FOUND", "no account exists for this external_provider_account_id")
)

// reauthAccountLocator 按 Portal 引用定位账号 id（账号 repo 天然满足）。
type reauthAccountLocator interface {
	FindAccountIDByExternalRef(ctx context.Context, externalRef string) (int64, bool, error)
}

// reauthAccountReader 读取账号（取已绑定的 proxy id）。
type reauthAccountReader interface {
	GetByID(ctx context.Context, id int64) (*Account, error)
}

// ProviderConnectReauthService 创建重授权会话。
type ProviderConnectReauthService struct {
	sessions ProviderConnectSessionRepository
	locator  reauthAccountLocator
	accounts reauthAccountReader
	oauth    connectOAuthURLGenerator
	now      func() time.Time
}

// NewProviderConnectReauthService builds the service.
func NewProviderConnectReauthService(
	sessions ProviderConnectSessionRepository,
	locator reauthAccountLocator,
	accounts reauthAccountReader,
	oauth *OAuthService,
) *ProviderConnectReauthService {
	return &ProviderConnectReauthService{
		sessions: sessions,
		locator:  locator,
		accounts: accounts,
		oauth:    oauth,
		now:      time.Now,
	}
}

// CreateReauthSessionInput Portal 发起的重授权请求。
type CreateReauthSessionInput struct {
	ExternalProviderAccountID string
	ProviderType              string
	CallbackURL               string
}

// CreateReauthSession 为一个已存在的账号创建重授权会话：
// 定位账号 → 复用其 proxy 生成 OAuth URL → 落 pending 会话（预填账号 id）。
func (s *ProviderConnectReauthService) CreateReauthSession(
	ctx context.Context, input CreateReauthSessionInput,
) (*CreateOnboardingSessionResult, error) {
	accountRef := strings.TrimSpace(input.ExternalProviderAccountID)
	if accountRef == "" || len(accountRef) > 64 || !strings.HasPrefix(accountRef, "pa_") {
		return nil, ErrConnectInvalidAccountRef
	}
	providerType := strings.ToLower(strings.TrimSpace(input.ProviderType))
	if _, ok := providerConnectAllowedTypes[providerType]; !ok {
		return nil, ErrConnectInvalidProviderType
	}
	if err := validateConnectCallbackURL(input.CallbackURL); err != nil {
		return nil, err
	}

	id, found, err := s.locator.FindAccountIDByExternalRef(ctx, accountRef)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrReauthAccountNotFound
	}
	acc, err := s.accounts.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if acc == nil {
		return nil, ErrReauthAccountNotFound
	}

	// 复用账号已绑定的 proxy（可为 nil —— 无代理账号照常直连授权）。
	authRes, err := s.oauth.GenerateAuthURL(ctx, acc.ProxyID)
	if err != nil {
		return nil, err
	}

	expiresAt := s.now().Add(providerConnectSessionTTL)
	oauthSessionID := authRes.SessionID
	accountID := acc.ID
	session, err := s.sessions.Create(ctx, &ProviderConnectSession{
		ExternalProviderAccountID: accountRef,
		ProviderType:              providerType,
		ProxyID:                   acc.ProxyID,
		Status:                    "pending",
		OAuthSessionID:            &oauthSessionID,
		Sub2apiAccountID:          &accountID, // 重授权标记：完成时更新此账号而非新建
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
