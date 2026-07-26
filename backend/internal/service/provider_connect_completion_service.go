package service

import (
	"context"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Phase 21E-6C-2C: Provider Connect 完成流程（授权码 → token → 账号）。
//
// 编排层，零 OAuth/账号业务逻辑复制：
//   - 换 token 复用 OAuthService.ExchangeCode（禁改，仅调用）
//   - 建账号复用 ProviderConnectAccountRepository（ent 直写带归属引用，
//     不经 AccountService.Create —— 后者不接受 external_provider_account_id
//     且属禁改重构范围）
//
// OAuth Session 限制（重要，不在本阶段解决）：ExchangeCode 依赖
// OAuthService 的内存 SessionStore。因此 Provider Connect 的授权 URL
// 生成与完成必须在【同一运行实例】、且在 session TTL 内完成。多实例
// 部署前需要把 OAuth session 外置化（Redis）—— 那需要改 OAuthService，
// 留待后续阶段。

var (
	// ErrConnectSessionNotFound 会话不存在。
	ErrConnectSessionNotFound = infraerrors.NotFound(
		"CONNECT_SESSION_NOT_FOUND", "provider connect session not found")
	// ErrConnectSessionExpired 会话已过期。
	ErrConnectSessionExpired = infraerrors.BadRequest(
		"CONNECT_SESSION_EXPIRED", "provider connect session has expired")
	// ErrConnectSessionFailed 会话处于 failed 终态。
	ErrConnectSessionFailed = infraerrors.Conflict(
		"CONNECT_SESSION_FAILED", "provider connect session has failed; start a new one")
	// ErrConnectSessionNotAuthorizable 会话状态不允许完成（非 pending / completed）。
	ErrConnectSessionNotAuthorizable = infraerrors.Conflict(
		"CONNECT_SESSION_NOT_AUTHORIZABLE", "provider connect session is not in a completable state")
	// ErrConnectMissingOAuthSession 会话缺少 oauth_session_id（异常数据）。
	ErrConnectMissingOAuthSession = infraerrors.Conflict(
		"CONNECT_MISSING_OAUTH_SESSION", "provider connect session has no oauth session bound")
	// ErrConnectCodeRequired 授权码为空。
	ErrConnectCodeRequired = infraerrors.BadRequest(
		"CONNECT_CODE_REQUIRED", "authorization code is required")
)

// providerTypeToPlatform 把 Portal 的 provider_type 映射到 Sub2API platform。
// claude→anthropic，codex→openai（Codex 属 OpenAI），openai/gemini 同名。
var providerTypeToPlatform = map[string]string{
	"claude": PlatformAnthropic,
	"codex":  PlatformOpenAI,
	"openai": PlatformOpenAI,
	"gemini": PlatformGemini,
}

// PlatformForProviderType 把 Portal 的 provider_type 解析为 Sub2API 账号平台
// （容量按平台分桶用）。未知类型返回 ok=false。大小写不敏感。
func PlatformForProviderType(providerType string) (string, bool) {
	p, ok := providerTypeToPlatform[strings.ToLower(strings.TrimSpace(providerType))]
	return p, ok
}

// connectTokenExchanger 是完成流程对 OAuthService 的最小依赖面。
type connectTokenExchanger interface {
	ExchangeCode(ctx context.Context, input *ExchangeCodeInput) (*TokenInfo, error)
}

// connectReauthAccountUpdater 重授权完成时更新既有账号的最小依赖面
// （Phase 21G；具体账号 repo 天然满足 GetByID + Update）。
type connectReauthAccountUpdater interface {
	GetByID(ctx context.Context, id int64) (*Account, error)
	Update(ctx context.Context, account *Account) error
}

// ProviderWebhookNotifier 是完成流程对 webhook 发送器的最小依赖面
// （*providerwebhook.Sender 天然满足；接口化仅为可测性与可选性）。
// SendAsync 必须非阻塞——Portal 故障绝不能影响账号创建。
type ProviderWebhookNotifier interface {
	Enabled() bool
	SendActivatedAsync(in ActivatedWebhookInput)
}

// ActivatedWebhookInput 完成流程传给 notifier 的 activated 事件数据。
type ActivatedWebhookInput struct {
	SessionID                 int64
	ExternalProviderAccountID string
	Sub2apiAccountID          int64
	ProviderType              string
	Platform                  string
	Region                    string
	// EventID (可选，Phase 21E-6E-4): 非空时 notifier 直接用它作为稳定
	// event id；为空时回落到 ConnectActivatedEventID(SessionID)（OAuth 流程
	// 既有行为，不受影响）。Import 流程无 session id，用此字段传入
	// ImportActivatedEventID(external_ref)。
	EventID string
	// Email (可选，Phase 21E-6E email-name): 上游 AI 账号邮箱，回传给 Portal
	// 作为账号展示名。非凭证标识；为空时 Portal 回退序号名。
	Email string
	// Plan (可选): 上游订阅等级快照（如 default_claude_max_20x），Portal 存快照展示。
	Plan string
}

// ProviderConnectAccountRepository 完成流程创建账号的仓储面。
// 直写一个 type=oauth、带 external_provider_account_id 的账号行。
// external_provider_account_id 的部分唯一约束保证幂等：并发/重放的第二
// 次创建会命中唯一冲突。
type ProviderConnectAccountRepository interface {
	CreateConnectedAccount(ctx context.Context, in CreateConnectedAccountInput) (accountID int64, err error)
	// FindAccountIDByExternalRef 用归属引用反查已存在的账号 id（幂等重入）。
	FindAccountIDByExternalRef(ctx context.Context, externalRef string) (accountID int64, found bool, err error)
	// ReleaseAllocationByExternalRef 释放某 provider 账号的活跃代理占用，代理
	// 归还可分配池（Phase 21E-6E proxy-exclusive）。幂等：无活跃占用时 no-op。
	// 能力就绪：待 Portal→Sub2API 的账号 removed 通道接线后调用。
	ReleaseAllocationByExternalRef(ctx context.Context, externalRef, reason string) error
}

// CreateConnectedAccountInput 创建接入账号的入参。
type CreateConnectedAccountInput struct {
	Name                      string
	Platform                  string
	Credentials               map[string]any
	ProxyID                   *int64
	ExternalProviderAccountID string
	// Region: egress region label snapshot for the proxy allocation record
	// (Phase 21E-6E proxy-exclusive). Optional; nil when no proxy is bound.
	Region *string
}

// CompleteAuthorizationInput 完成请求。
type CompleteAuthorizationInput struct {
	SessionID int64
	Code      string
}

// CompleteAuthorizationResult 完成结果。Portal 用这些字段在 /complete 请求内
// 同步建账号（无需等 webhook）。幂等分支（会话已完成）只填 external_ref/region，
// email/plan 可能为空（未重新换 token），此时 Portal 账号通常已存在。
type CompleteAuthorizationResult struct {
	Status                    string `json:"status"`
	AccountID                 int64  `json:"account_id"`
	ExternalProviderAccountID string `json:"external_provider_account_id,omitempty"`
	Email                     string `json:"email,omitempty"`
	Region                    string `json:"region,omitempty"`
	Plan                      string `json:"plan,omitempty"`
}

// ProviderConnectCompletionService 完成 Provider Connect 授权。
type ProviderConnectCompletionService struct {
	sessions ProviderConnectSessionRepository
	accounts ProviderConnectAccountRepository
	// reauthAccounts (Phase 21G): 重授权会话（Sub2apiAccountID 预填）完成时
	// 更新既有账号凭证并恢复 active/schedulable，而非新建账号。
	reauthAccounts connectReauthAccountUpdater
	oauth          connectTokenExchanger
	webhook        ProviderWebhookNotifier // 可为 nil（未配置时不通知）
	now            func() time.Time
}

// NewProviderConnectCompletionService creates the service.
func NewProviderConnectCompletionService(
	sessions ProviderConnectSessionRepository,
	accounts ProviderConnectAccountRepository,
	reauthAccounts connectReauthAccountUpdater,
	oauth *OAuthService,
	webhook ProviderWebhookNotifier,
) *ProviderConnectCompletionService {
	return &ProviderConnectCompletionService{
		sessions:       sessions,
		accounts:       accounts,
		reauthAccounts: reauthAccounts,
		oauth:          oauth,
		webhook:        webhook,
		now:            time.Now,
	}
}

// CompleteAuthorization 完成一个 pending 会话：换 token → 建账号 →
// 置 completed。幂等：重复提交同一已 completed 会话返回既有 account_id，
// 不创建第二个账号。
func (s *ProviderConnectCompletionService) CompleteAuthorization(
	ctx context.Context, input CompleteAuthorizationInput,
) (*CompleteAuthorizationResult, error) {
	if strings.TrimSpace(input.Code) == "" {
		return nil, ErrConnectCodeRequired
	}

	// Step 1: 读取会话 + 状态校验
	session, err := s.sessions.GetByID(ctx, input.SessionID)
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, ErrConnectSessionNotFound
	}

	switch session.Status {
	case "completed":
		// 幂等：已完成，返回既有账号（不重新换 token / 不建第二个账号）
		if session.Sub2apiAccountID != nil {
			return &CompleteAuthorizationResult{
				Status:                    "completed",
				AccountID:                 *session.Sub2apiAccountID,
				ExternalProviderAccountID: session.ExternalProviderAccountID,
				Region:                    derefString(session.Region),
			}, nil
		}
		// 数据异常：completed 却无账号 id —— 用归属引用兜底反查
		id, found, ferr := s.accounts.FindAccountIDByExternalRef(ctx, session.ExternalProviderAccountID)
		if ferr != nil {
			return nil, ferr
		}
		if found {
			return &CompleteAuthorizationResult{
				Status:                    "completed",
				AccountID:                 id,
				ExternalProviderAccountID: session.ExternalProviderAccountID,
				Region:                    derefString(session.Region),
			}, nil
		}
		return nil, ErrConnectSessionNotAuthorizable
	case "failed":
		return nil, ErrConnectSessionFailed
	case "pending":
		// 继续处理
	default:
		return nil, ErrConnectSessionNotAuthorizable
	}

	// 过期保护（pending 但已过 expires_at）
	if s.now().After(session.ExpiresAt) {
		_ = s.sessions.MarkFailed(ctx, session.ID)
		return nil, ErrConnectSessionExpired
	}

	// Step 2/3: 复用 OAuthService 换 token（禁改，仅调用）
	if session.OAuthSessionID == nil || *session.OAuthSessionID == "" {
		return nil, ErrConnectMissingOAuthSession
	}
	platform, ok := providerTypeToPlatform[strings.ToLower(session.ProviderType)]
	if !ok {
		return nil, ErrConnectInvalidProviderType
	}

	tokenInfo, err := s.oauth.ExchangeCode(ctx, &ExchangeCodeInput{
		SessionID: *session.OAuthSessionID,
		Code:      strings.TrimSpace(input.Code),
		ProxyID:   session.ProxyID,
	})
	if err != nil {
		// OAuth 失败：会话置 failed，不保存任何 token
		_ = s.sessions.MarkFailed(ctx, session.ID)
		return nil, err
	}

	// Reauth 分支（Phase 21G）：pending 会话预填了 sub2api_account_id → 这是
	// 对既有账号的重授权。更新其凭证并恢复 active/schedulable，不新建账号、
	// 不做容量检查（账号本来就占着自己的 proxy 名额）。
	if session.Sub2apiAccountID != nil {
		return s.completeReauthorization(ctx, session, tokenInfo)
	}

	// Step 4: 复用账号创建仓储建 type=oauth 账号（带归属引用 + proxy 绑定）
	accountID, err := s.accounts.CreateConnectedAccount(ctx, CreateConnectedAccountInput{
		Name:                      session.ExternalProviderAccountID,
		Platform:                  platform,
		Credentials:               tokenInfoToCredentials(tokenInfo),
		ProxyID:                   session.ProxyID,
		ExternalProviderAccountID: session.ExternalProviderAccountID,
		Region:                    session.Region,
	})
	if err != nil {
		// 账号创建失败：可能是并发重放命中唯一约束 —— 反查已存在账号做幂等收敛
		if id, found, ferr := s.accounts.FindAccountIDByExternalRef(ctx, session.ExternalProviderAccountID); ferr == nil && found {
			// 已有账号：把会话收敛到 completed 并返回既有账号
			_, _ = s.sessions.MarkCompleted(ctx, session.ID, id, s.now())
			return &CompleteAuthorizationResult{
				Status:                    "completed",
				AccountID:                 id,
				ExternalProviderAccountID: session.ExternalProviderAccountID,
				Region:                    derefString(session.Region),
			}, nil
		}
		// 真实失败：会话置 failed，不留半成品（账号创建是原子的，失败即无行）
		_ = s.sessions.MarkFailed(ctx, session.ID)
		return nil, err
	}

	// Step 5: 会话 pending → completed（条件更新，幂等保护）
	affected, err := s.sessions.MarkCompleted(ctx, session.ID, accountID, s.now())
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		// 并发：另一路已把会话完成 —— 以既有会话结果为准，不重复计数、
		// 不重复发事件（另一路已发）。
		if cur, gerr := s.sessions.GetByID(ctx, session.ID); gerr == nil && cur != nil && cur.Sub2apiAccountID != nil {
			return &CompleteAuthorizationResult{
				Status:                    "completed",
				AccountID:                 *cur.Sub2apiAccountID,
				ExternalProviderAccountID: session.ExternalProviderAccountID,
				Region:                    derefString(session.Region),
			}, nil
		}
	}

	// Step 6: 通知 Portal（异步、best-effort）。event_id 由 session id
	// 派生保持稳定；Portal 故障绝不影响此处已成功的账号创建/会话完成。
	if s.webhook != nil && s.webhook.Enabled() {
		s.webhook.SendActivatedAsync(ActivatedWebhookInput{
			SessionID:                 session.ID,
			ExternalProviderAccountID: session.ExternalProviderAccountID,
			Sub2apiAccountID:          accountID,
			ProviderType:              strings.ToLower(session.ProviderType),
			Platform:                  platform,
			Region:                    derefString(session.Region),
			Email:                     tokenInfo.EmailAddress,
			Plan:                      tokenInfo.RateLimitTier,
		})
	}

	return &CompleteAuthorizationResult{
		Status:                    "completed",
		AccountID:                 accountID,
		ExternalProviderAccountID: session.ExternalProviderAccountID,
		Email:                     tokenInfo.EmailAddress,
		Region:                    derefString(session.Region),
		Plan:                      tokenInfo.RateLimitTier,
	}, nil
}

// completeReauthorization 把新换到的 token 写回既有账号并恢复其可用性
// （Phase 21G）。凭证整体替换为新 token；状态 error/disabled → active、
// error_message 清空、schedulable 恢复。会话置 completed（幂等：并发第二路
// MarkCompleted 影响行数为 0，直接返回同一账号结果，无重复写害处——凭证
// 内容相同）。失败路径与 onboarding 一致：置 failed、不留半新半旧凭证
// （Update 是单行原子写）。
func (s *ProviderConnectCompletionService) completeReauthorization(
	ctx context.Context, session *ProviderConnectSession, tokenInfo *TokenInfo,
) (*CompleteAuthorizationResult, error) {
	accountID := *session.Sub2apiAccountID
	acc, err := s.reauthAccounts.GetByID(ctx, accountID)
	if err != nil {
		_ = s.sessions.MarkFailed(ctx, session.ID)
		return nil, err
	}
	if acc == nil {
		_ = s.sessions.MarkFailed(ctx, session.ID)
		return nil, ErrReauthAccountNotFound
	}

	acc.Credentials = tokenInfoToCredentials(tokenInfo)
	acc.Status = StatusActive
	acc.ErrorMessage = ""
	acc.Schedulable = true
	if err := s.reauthAccounts.Update(ctx, acc); err != nil {
		_ = s.sessions.MarkFailed(ctx, session.ID)
		return nil, err
	}

	_, err = s.sessions.MarkCompleted(ctx, session.ID, accountID, s.now())
	if err != nil {
		return nil, err
	}

	return &CompleteAuthorizationResult{
		Status:                    "completed",
		AccountID:                 accountID,
		ExternalProviderAccountID: session.ExternalProviderAccountID,
		Email:                     tokenInfo.EmailAddress,
		Region:                    derefString(session.Region),
		Plan:                      tokenInfo.RateLimitTier,
	}, nil
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// tokenInfoToCredentials 把 OAuth token 映射为账号 credentials JSONB。
// 结构与 Sub2API OAuth 账号约定一致：access_token / refresh_token /
// expires_at（见 ent/schema/account.go 注释）。
func tokenInfoToCredentials(t *TokenInfo) map[string]any {
	creds := map[string]any{
		"access_token": t.AccessToken,
	}
	if t.RefreshToken != "" {
		creds["refresh_token"] = t.RefreshToken
	}
	if t.ExpiresAt > 0 {
		creds["expires_at"] = t.ExpiresAt
	}
	if t.TokenType != "" {
		creds["token_type"] = t.TokenType
	}
	if t.Scope != "" {
		creds["scope"] = t.Scope
	}
	if t.RateLimitTier != "" {
		creds["rate_limit_tier"] = t.RateLimitTier
	}
	return creds
}
