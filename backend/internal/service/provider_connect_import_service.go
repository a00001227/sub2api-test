package service

import (
	"context"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service/providerwebhook"
)

// Phase 21E-6E-4: Provider Credential Import（渠道商凭证导入）。
//
// 单条 credential 导入的编排层 —— 与 OAuth 完成流程（ProviderConnect-
// CompletionService）共用同一账号体系，唯一区别是「凭证来源」：
//
//	OAuth:  code   → OAuthService.ExchangeCode → TokenInfo
//	Import: sessionKey → OAuthService.CookieAuth → TokenInfo
//
// 之后 TokenInfo → normalized credentials → CreateConnectedAccount →
// activated webhook 全部复用既有能力，零复制。因此 imported 账号与 OAuth
// 账号进入完全相同的生命周期（同 external_provider_account_id → 同
// sub2api account → 同 usage → 同 earnings）。
//
// 本阶段仅支持 claude（Portal 侧 provider_type=claude）。Codex/OpenAI 无
// 「cookie sessionKey → token」的现成能力，不在本阶段范围。
//
// 安全：credential 只在本服务的调用栈内存在（HTTP body → CookieAuth →
// 丢弃）。绝不落库明文（存储的是换取后的标准 OAuth token）、不入日志、
// 不入 error、不入 webhook。API 边界把底层可能含凭证的 error 统一转成
// 稳定错误码。

const (
	// providerImportMaxCredentialLen credential 的保守上限（16 KiB）。
	// 仓库现有 DTO 无更具体约定，取保守值防超大 body。
	providerImportMaxCredentialLen = 16 * 1024
)

var (
	// ErrImportInvalidRequest 请求参数非法（缺字段/格式错误）。
	ErrImportInvalidRequest = infraerrors.BadRequest(
		"INVALID_REQUEST", "invalid import request")
	// ErrImportProviderTypeUnsupported provider_type 不支持（本阶段仅 claude）。
	ErrImportProviderTypeUnsupported = infraerrors.BadRequest(
		"PROVIDER_TYPE_UNSUPPORTED", "provider_type is not supported for credential import")
	// ErrImportInvalidCredential 凭证无效（CookieAuth 失败）。
	// 注意：绝不携带底层 error 文本（可能含 sessionKey）。
	ErrImportInvalidCredential = infraerrors.BadRequest(
		"INVALID_CREDENTIAL", "the provided credential is invalid or could not be verified")
	// ErrImportAccountCreateFailed 账号创建失败（非幂等冲突的真实失败）。
	ErrImportAccountCreateFailed = infraerrors.InternalServer(
		"ACCOUNT_CREATE_FAILED", "failed to create the imported account")
)

// connectCookieAuthenticator 是 import 流程对 OAuthService 的最小依赖面。
// *OAuthService 天然满足；接口化仅为可测性（测试可注入 fake，不触真实上游）。
type connectCookieAuthenticator interface {
	CookieAuth(ctx context.Context, input *CookieAuthInput) (*TokenInfo, error)
}

// ImportCredentialInput 单条导入请求（已由 handler 从 HTTP body 解出）。
// Credential 是敏感字段，仅在内存流转。
type ImportCredentialInput struct {
	ExternalProviderAccountID string
	ProviderType              string
	Credential                string
	Region                    string
}

// ImportCredentialResult 安全响应（绝不含 credential）。
type ImportCredentialResult struct {
	Status           string `json:"status"`             // "active" | "already_exists"
	Sub2apiAccountID int64  `json:"sub2api_account_id"` // 供 Portal 回填
}

// ProviderConnectImportService 编排单条 credential 导入。
type ProviderConnectImportService struct {
	accounts  ProviderConnectAccountRepository
	allocator *ProxyAllocator
	cookie    connectCookieAuthenticator
	webhook   ProviderWebhookNotifier // 可为 nil（未配置时不通知）
}

// NewProviderConnectImportService creates the service.
func NewProviderConnectImportService(
	accounts ProviderConnectAccountRepository,
	allocator *ProxyAllocator,
	oauth *OAuthService,
	webhook ProviderWebhookNotifier,
) *ProviderConnectImportService {
	return &ProviderConnectImportService{
		accounts:  accounts,
		allocator: allocator,
		cookie:    oauth,
		webhook:   webhook,
	}
}

// ImportCredential 导入一条 credential：校验 → 幂等预查 → 分配 proxy →
// CookieAuth 验证/换 token → 建账号 → 发 activated webhook。
//
// 幂等：同一 external_provider_account_id 第二次导入直接返回既有账号，
// 不覆盖凭证、不重分配 proxy、不重复发 webhook。
func (s *ProviderConnectImportService) ImportCredential(
	ctx context.Context, in ImportCredentialInput,
) (*ImportCredentialResult, error) {
	// Step 1: 参数校验（fail-fast，全部机器码错误，绝不回显 credential）
	accountRef := strings.TrimSpace(in.ExternalProviderAccountID)
	if accountRef == "" || len(accountRef) > 64 || !strings.HasPrefix(accountRef, "pa_") {
		return nil, ErrImportInvalidRequest
	}
	providerType := strings.ToLower(strings.TrimSpace(in.ProviderType))
	// 本阶段仅 claude。
	if providerType != "claude" {
		return nil, ErrImportProviderTypeUnsupported
	}
	platform, ok := providerTypeToPlatform[providerType]
	if !ok {
		return nil, ErrImportProviderTypeUnsupported
	}
	if strings.TrimSpace(in.Credential) == "" || len(in.Credential) > providerImportMaxCredentialLen {
		return nil, ErrImportInvalidRequest
	}
	region := strings.ToUpper(strings.TrimSpace(in.Region))
	if region == "" {
		return nil, ErrImportInvalidRequest
	}

	// Step 2: 幂等预查 —— 同 external_ref 已有账号则直接返回（不覆盖任何东西）。
	if id, found, err := s.accounts.FindAccountIDByExternalRef(ctx, accountRef); err != nil {
		return nil, err
	} else if found {
		return &ImportCredentialResult{Status: "already_exists", Sub2apiAccountID: id}, nil
	}

	// Step 3: 按 region + platform 分配代理（无容量返回 REGION_NO_CAPACITY；不降级直连）。
	proxy, err := s.allocator.SelectProxy(ctx, region, platform)
	if err != nil {
		return nil, err // ErrRegionNoCapacity / ErrRegionRequired，均为安全错误码
	}
	var proxyID *int64
	if proxy != nil {
		pid := proxy.ID
		proxyID = &pid
	}

	// Step 4: 用选中的 proxy 执行 credential 验证/换 token（验证出口 = 后续
	// 使用出口）。CookieAuth: sessionKey → org → authcode → OAuth token。
	// 失败一律转成 INVALID_CREDENTIAL —— 底层 error 可能含 sessionKey 片段，
	// 绝不外传。
	tokenInfo, err := s.cookie.CookieAuth(ctx, &CookieAuthInput{
		SessionKey: in.Credential,
		ProxyID:    proxyID,
		Scope:      "full",
	})
	if err != nil || tokenInfo == nil || strings.TrimSpace(tokenInfo.AccessToken) == "" {
		return nil, ErrImportInvalidCredential
	}

	// Step 5: 建 type=oauth 账号（复用完成流程同一仓储；credentials 为换取
	// 后的标准 OAuth token 结构，Gateway/token refresh 直接识别）。
	accountID, err := s.accounts.CreateConnectedAccount(ctx, CreateConnectedAccountInput{
		Name:                      accountRef,
		Platform:                  platform,
		Credentials:               tokenInfoToCredentials(tokenInfo),
		ProxyID:                   proxyID,
		ExternalProviderAccountID: accountRef,
		Region:                    &region,
	})
	if err != nil {
		// 并发/重放命中唯一约束 → 反查收敛为幂等成功。
		if id, found, ferr := s.accounts.FindAccountIDByExternalRef(ctx, accountRef); ferr == nil && found {
			return &ImportCredentialResult{Status: "already_exists", Sub2apiAccountID: id}, nil
		}
		// 真实失败：账号创建原子，失败即无行，不发 webhook。
		return nil, ErrImportAccountCreateFailed
	}

	// Step 6: 账号创建成功后，发 activated webhook（异步、best-effort）。
	// event id 由 external_ref 稳定派生；Portal 故障不影响已成功的账号创建。
	if s.webhook != nil && s.webhook.Enabled() {
		s.webhook.SendActivatedAsync(ActivatedWebhookInput{
			ExternalProviderAccountID: accountRef,
			Sub2apiAccountID:          accountID,
			ProviderType:              providerType,
			Platform:                  platform,
			Region:                    region,
			EventID:                   providerwebhook.ImportActivatedEventID(accountRef),
			Email:                     tokenInfo.EmailAddress,
		})
	}

	return &ImportCredentialResult{Status: "active", Sub2apiAccountID: accountID}, nil
}
