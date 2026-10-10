package service

import (
	"context"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service/providerwebhook"
	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
	"github.com/tidwall/gjson"
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
	platform, ok := providerTypeToPlatform[providerType]
	if !ok {
		return nil, ErrImportProviderTypeUnsupported
	}
	// 支持:claude(sessionKey → CookieAuth 网络交换)、codex/openai(access_token
	// JWT / codex session JSON 纯解析,不走交换)。gemini 暂不支持导入。
	switch providerType {
	case "claude", "codex", "openai":
	default:
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

	// Step 4: 按平台产出标准 OAuth 凭据。失败一律转 INVALID_CREDENTIAL —— 底层
	// error 可能含凭据片段,绝不外传。
	//   - claude:  sessionKey → CookieAuth 网络交换(验证出口 = 后续使用出口)。
	//   - codex/openai: 纯解析 access_token(JWT)/ codex session JSON,不走交换。
	var credentials map[string]any
	var email, plan string
	switch platform {
	case PlatformAnthropic:
		if !strings.HasPrefix(strings.TrimSpace(in.Credential), claudeSessionKeyPrefix) {
			return nil, importInvalidCredential("format",
				"claude sessionKey format invalid: expected a key starting with "+claudeSessionKeyPrefix+" (copied from claude.ai cookie sessionKey)",
				map[string]string{"region": region, "proxy_used": strconv.FormatBool(proxyID != nil)})
		}
		tokenInfo, cerr := s.cookie.CookieAuth(ctx, &CookieAuthInput{
			SessionKey: in.Credential,
			ProxyID:    proxyID,
			Scope:      "full",
		})
		if cerr != nil || tokenInfo == nil || strings.TrimSpace(tokenInfo.AccessToken) == "" {
			// The response stays a stable INVALID_CREDENTIAL (no leak). But the
			// import layer otherwise swallows WHY — so a failed sessionKey import is
			// undebuggable. Log a REDACTED classification (never the sessionKey): the
			// CookieAuth error wraps the failing stage ("failed to get organization
			// info" vs "…authorization code" vs "…exchange code"), separating a
			// bad/expired key from a proxy/OAuth-flow problem.
			reason := "empty_access_token"
			if cerr != nil {
				reason = logredact.RedactText(cerr.Error())
			}
			slog.WarnContext(ctx, "provider_connect.import.claude_cookie_auth_failed",
				"external_ref", accountRef,
				"region", region,
				"proxy_used", proxyID != nil,
				"reason", reason)
			stage, detail, md := classifyClaudeCookieAuthFailure(cerr)
			md["region"] = region
			md["proxy_used"] = strconv.FormatBool(proxyID != nil)
			return nil, importInvalidCredential(stage, detail, md)
		}
		credentials = tokenInfoToCredentials(tokenInfo)
		email = tokenInfo.EmailAddress
		plan = tokenInfo.RateLimitTier
	case PlatformOpenAI:
		parsed, perr := ParseCodexCredentialString(in.Credential)
		if perr != nil || parsed == nil {
			detail := "codex credential could not be parsed"
			if perr != nil {
				detail = "codex credential rejected: " + logredact.RedactText(perr.Error())
			}
			return nil, importInvalidCredential("parse", detail, map[string]string{"region": region})
		}
		credentials = parsed.Credentials
		email = parsed.Email
		plan = parsed.PlanType
	default:
		return nil, ErrImportProviderTypeUnsupported
	}

	// Step 5: 建 type=oauth 账号(复用完成流程同一仓储;credentials 为标准 OAuth
	// token 结构,Gateway/token refresh 直接识别)。A1 平台默认(anthropic+openai)
	// 在此叠加。
	accountID, err := s.accounts.CreateConnectedAccount(ctx, CreateConnectedAccountInput{
		Name:                      accountRef,
		Platform:                  platform,
		Credentials:               credentials,
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
			Email:                     email,
			Plan:                      plan,
		})
	}

	return &ImportCredentialResult{Status: "active", Sub2apiAccountID: accountID}, nil
}

// claudeSessionKeyPrefix claude.ai 的 sessionKey cookie 固定前缀。
const claudeSessionKeyPrefix = "sk-ant-sid01-"

// importInvalidCredential 构造带真实(已脱敏)原因的 INVALID_CREDENTIAL 错误。
//
// 以前导入失败一律回固定文案 "the provided credential is invalid",Portal 再把它翻译成
// "sessionKey 过期/被吊销" —— 代理缺失、出口被 Cloudflare 拦、会话不够新、格式错,全都长一个样,
// 只能翻 cell 日志。现在 reason 码不变(Portal 依赖),message 带上游原话/我们自己的真实原因,
// metadata 带 stage / upstream_status 等机器字段。凭证本身永远不进 message。
func importInvalidCredential(stage, detail string, md map[string]string) error {
	if md == nil {
		md = map[string]string{}
	}
	md["stage"] = stage
	detail = strings.TrimSpace(redactImportDetail(detail))
	if len(detail) > importErrorDetailMaxLen {
		detail = detail[:importErrorDetailMaxLen] + "…"
	}
	return infraerrors.BadRequest("INVALID_CREDENTIAL", detail).WithMetadata(md)
}

const importErrorDetailMaxLen = 400

var (
	// 凭证形态的 token(sk-ant-sid01-…、sk-…)和 URL 里的 user:pass@ —— logredact 只认键值对,
	// 这两类裸露在错误原话里的秘密要单独抹掉。
	reImportSecretToken = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`)
	reImportURLUserinfo = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s@]+@`)
)

// redactImportDetail 对导入错误原话做脱敏:通用键值脱敏 + 裸 token + URL 凭据。
func redactImportDetail(s string) string {
	s = logredact.RedactText(s)
	s = reImportSecretToken.ReplaceAllString(s, "sk-***")
	s = reImportURLUserinfo.ReplaceAllString(s, "${1}***@")
	return s
}

var cookieAuthUpstreamStatusRe = regexp.MustCompile(`status (\d{3}), body: `)

// classifyClaudeCookieAuthFailure 把 CookieAuth 三步(取组织 / 拿授权码 / 换 token)的错误拆成
// 阶段、人能读的原因和机器字段。上游 body 是 JSON 就取 error.message;是 Cloudflare 挑战页
// 就直接点名"出口 IP 被 claude.ai 拦";网络层失败保留 transport 错误原话。
func classifyClaudeCookieAuthFailure(err error) (stage, detail string, md map[string]string) {
	md = map[string]string{}
	if err == nil {
		return "token_exchange", "claude cookie auth returned an empty access token", md
	}
	text := err.Error()
	switch {
	case strings.HasPrefix(text, "failed to get organization info"):
		stage = "org_lookup"
	case strings.HasPrefix(text, "failed to get authorization code"):
		stage = "authorize"
	case strings.HasPrefix(text, "failed to exchange code"):
		stage = "token_exchange"
	default:
		stage = "cookie_auth"
	}
	stageLabel := map[string]string{
		"org_lookup":     "claude.ai organization lookup",
		"authorize":      "claude.ai OAuth authorize",
		"token_exchange": "claude.ai token exchange",
		"cookie_auth":    "claude cookie auth",
	}[stage]

	if m := cookieAuthUpstreamStatusRe.FindStringSubmatchIndex(text); m != nil {
		status := text[m[2]:m[3]]
		body := strings.TrimSpace(text[m[1]:])
		md["upstream_status"] = status
		return stage, stageLabel + " failed: upstream HTTP " + status + ": " + summarizeUpstreamErrorBody(body, md), md
	}
	if i := strings.Index(text, "request failed:"); i >= 0 {
		md["upstream_status"] = "transport_error"
		return stage, stageLabel + " failed before any upstream response (network/proxy): " + strings.TrimSpace(text[i+len("request failed:"):]), md
	}
	return stage, stageLabel + " failed: " + text, md
}

// summarizeUpstreamErrorBody 把上游错误 body 压成一句话:JSON 取 error.message / message,
// HTML(Cloudflare 挑战页)点名原因,其余截断原文。
func summarizeUpstreamErrorBody(body string, md map[string]string) string {
	trimmed := strings.TrimSpace(body)
	if strings.HasPrefix(trimmed, "{") {
		if msg := gjson.Get(trimmed, "error.message").String(); msg != "" {
			if typ := gjson.Get(trimmed, "error.type").String(); typ != "" {
				md["upstream_error_type"] = typ
			}
			return msg
		}
		if msg := gjson.Get(trimmed, "message").String(); msg != "" {
			return msg
		}
	}
	lower := strings.ToLower(trimmed)
	if strings.Contains(lower, "just a moment") || strings.Contains(lower, "cf-chl") || strings.Contains(lower, "challenge-platform") {
		md["upstream_error_type"] = "cloudflare_challenge"
		return "Cloudflare challenge page (claude.ai blocked this egress IP; the request did not reach the API)"
	}
	if strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html") {
		md["upstream_error_type"] = "html_response"
		return "HTML page instead of an API response (egress blocked or wrong endpoint)"
	}
	if len(trimmed) > 200 {
		trimmed = trimmed[:200] + "…"
	}
	return trimmed
}
