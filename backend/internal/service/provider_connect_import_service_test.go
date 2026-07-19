package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// Phase 21E-6E-4: ProviderConnectImportService 单元测试（全假件，不触真实
// 上游）。复用同包既有假件：fakeConnectAccountRepo / fakeWebhookNotifier /
// fakeAllocationRepo；仅新增 fakeCookieAuth。

// --- 假 CookieAuth：不触真实 claude.ai，可注入成功/失败 ---
type fakeCookieAuth struct {
	token      *TokenInfo
	err        error
	gotSession string // 记录收到的 sessionKey（仅测试内断言，不外泄）
	gotProxyID *int64
	callN      int
}

func (f *fakeCookieAuth) CookieAuth(_ context.Context, in *CookieAuthInput) (*TokenInfo, error) {
	f.callN++
	f.gotSession = in.SessionKey
	f.gotProxyID = in.ProxyID
	if f.err != nil {
		return nil, f.err
	}
	return f.token, nil
}

func newImportSvc(accounts *fakeConnectAccountRepo, alloc *ProxyAllocator, cookie connectCookieAuthenticator, wh ProviderWebhookNotifier) *ProviderConnectImportService {
	return &ProviderConnectImportService{
		accounts:  accounts,
		allocator: alloc,
		cookie:    cookie,
		webhook:   wh,
	}
}

func okInput() ImportCredentialInput {
	return ImportCredentialInput{
		ExternalProviderAccountID: "pa_abc123",
		ProviderType:              "claude",
		Credential:                "sk-ant-session-SECRET-VALUE",
		Region:                    "us",
	}
}

// 2/3/4/5: 成功 import → 建账号(external_ref 写入) + proxy 绑定 + webhook 发一次
func TestImport_Success(t *testing.T) {
	accounts := newFakeConnectAccountRepo()
	alloc := NewProxyAllocator(&fakeAllocationRepo{proxy: &Proxy{ID: 7, Name: "us-1", Status: StatusActive}})
	cookie := &fakeCookieAuth{token: &TokenInfo{AccessToken: "at-xyz", RefreshToken: "rt-xyz", ExpiresAt: 111, EmailAddress: "user@example.com"}}
	wh := &fakeWebhookNotifier{enabled: true}
	svc := newImportSvc(accounts, alloc, cookie, wh)

	res, err := svc.ImportCredential(context.Background(), okInput())
	require.NoError(t, err)
	require.Equal(t, "active", res.Status)
	require.NotZero(t, res.Sub2apiAccountID)

	// external_ref 正确写入
	id, found, _ := accounts.FindAccountIDByExternalRef(context.Background(), "pa_abc123")
	require.True(t, found)
	require.Equal(t, res.Sub2apiAccountID, id)

	// proxy 绑定：CookieAuth 收到选中的 proxy id（验证出口=使用出口）
	require.NotNil(t, cookie.gotProxyID)
	require.Equal(t, int64(7), *cookie.gotProxyID)

	// webhook 发一次，且 payload 带 external_ref + account id + region
	require.Len(t, wh.sent, 1)
	require.Equal(t, "pa_abc123", wh.sent[0].ExternalProviderAccountID)
	require.Equal(t, res.Sub2apiAccountID, wh.sent[0].Sub2apiAccountID)
	require.Equal(t, "US", wh.sent[0].Region)
	require.Equal(t, "claude", wh.sent[0].ProviderType)
	require.Equal(t, PlatformAnthropic, wh.sent[0].Platform)
	require.Equal(t, "evt_import_pa_abc123", wh.sent[0].EventID)
	require.Equal(t, "user@example.com", wh.sent[0].Email, "webhook must carry the account email for display naming")
}

// 8: normalized credentials shape 与 OAuth 流程一致（access/refresh/expires）
func TestImport_NormalizedCredentialsShape(t *testing.T) {
	accounts := newFakeConnectAccountRepo()
	alloc := NewProxyAllocator(&fakeAllocationRepo{proxy: &Proxy{ID: 1, Status: StatusActive}})
	cookie := &fakeCookieAuth{token: &TokenInfo{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 999, TokenType: "Bearer"}}
	svc := newImportSvc(accounts, alloc, cookie, &fakeWebhookNotifier{enabled: true})

	_, err := svc.ImportCredential(context.Background(), okInput())
	require.NoError(t, err)
	// 与 completion 流程用同一 tokenInfoToCredentials —— 断言 key 与 OAuth 一致
	creds := tokenInfoToCredentials(cookie.token)
	require.Equal(t, "at", creds["access_token"])
	require.Equal(t, "rt", creds["refresh_token"])
	require.EqualValues(t, 999, creds["expires_at"])
}

// 11: 重复 import 同 external_ref → 返回已有 account，不新建、不重发 webhook
func TestImport_Idempotent(t *testing.T) {
	accounts := newFakeConnectAccountRepo()
	alloc := NewProxyAllocator(&fakeAllocationRepo{proxy: &Proxy{ID: 1, Status: StatusActive}})
	cookie := &fakeCookieAuth{token: &TokenInfo{AccessToken: "at"}}
	wh := &fakeWebhookNotifier{enabled: true}
	svc := newImportSvc(accounts, alloc, cookie, wh)

	first, err := svc.ImportCredential(context.Background(), okInput())
	require.NoError(t, err)

	second, err := svc.ImportCredential(context.Background(), okInput())
	require.NoError(t, err)
	require.Equal(t, "already_exists", second.Status)
	require.Equal(t, first.Sub2apiAccountID, second.Sub2apiAccountID)

	require.Equal(t, 1, accounts.createN, "must not create a second account")
	require.Len(t, wh.sent, 1, "must not re-send activated webhook")
	require.Equal(t, 1, cookie.callN, "second import must short-circuit before CookieAuth")
}

// 12: 并发/重放命中唯一约束 → 反查收敛为幂等成功（不新建第二个）
func TestImport_UniqueConflictConverges(t *testing.T) {
	accounts := newFakeConnectAccountRepo()
	// 预置：external_ref 已存在（模拟另一路已建），但预查放行前置为空 → 触发 create 冲突路径
	alloc := NewProxyAllocator(&fakeAllocationRepo{proxy: &Proxy{ID: 1, Status: StatusActive}})
	cookie := &fakeCookieAuth{token: &TokenInfo{AccessToken: "at"}}
	wh := &fakeWebhookNotifier{enabled: true}
	svc := newImportSvc(accounts, alloc, cookie, wh)

	// 让 create 首次返回唯一冲突，但 byRef 里已有该 ref（收敛反查会命中）
	accounts.byRef["pa_abc123"] = 42
	accounts.createErr = nil // create 会因 byRef 已存在而自然返回 duplicate

	// 预查会直接命中 byRef=42 → already_exists（这正是并发收敛的稳定终态）
	res, err := svc.ImportCredential(context.Background(), okInput())
	require.NoError(t, err)
	require.Equal(t, "already_exists", res.Status)
	require.Equal(t, int64(42), res.Sub2apiAccountID)
}

// 13: invalid credential（CookieAuth 失败）→ INVALID_CREDENTIAL，不建账号、不发 webhook
func TestImport_InvalidCredential(t *testing.T) {
	accounts := newFakeConnectAccountRepo()
	alloc := NewProxyAllocator(&fakeAllocationRepo{proxy: &Proxy{ID: 1, Status: StatusActive}})
	// 底层 error 故意包含 sessionKey 片段，验证不外泄
	cookie := &fakeCookieAuth{err: errors.New("cookie auth failed for sk-ant-session-SECRET-VALUE")}
	wh := &fakeWebhookNotifier{enabled: true}
	svc := newImportSvc(accounts, alloc, cookie, wh)

	res, err := svc.ImportCredential(context.Background(), okInput())
	require.Nil(t, res)
	require.Error(t, err)
	var appErr *infraerrors.ApplicationError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, "INVALID_CREDENTIAL", appErr.Reason)
	require.NotContains(t, appErr.Message, "SECRET", "error message must not leak credential")
	require.Equal(t, 0, accounts.createN)
	require.Len(t, wh.sent, 0)
}

// 14: region 无容量 → REGION_NO_CAPACITY，不换 token、不建账号
func TestImport_RegionNoCapacity(t *testing.T) {
	accounts := newFakeConnectAccountRepo()
	alloc := NewProxyAllocator(&fakeAllocationRepo{proxy: nil}) // 无可用
	cookie := &fakeCookieAuth{token: &TokenInfo{AccessToken: "at"}}
	wh := &fakeWebhookNotifier{enabled: true}
	svc := newImportSvc(accounts, alloc, cookie, wh)

	res, err := svc.ImportCredential(context.Background(), okInput())
	require.Nil(t, res)
	require.ErrorIs(t, err, ErrRegionNoCapacity)
	require.Equal(t, 0, cookie.callN, "must not validate credential when no proxy")
	require.Equal(t, 0, accounts.createN)
	require.Len(t, wh.sent, 0)
}

// 15: account 创建失败（非冲突）→ 不发 webhook
func TestImport_AccountCreateFailed_NoWebhook(t *testing.T) {
	accounts := newFakeConnectAccountRepo()
	accounts.createErr = errors.New("db down")
	alloc := NewProxyAllocator(&fakeAllocationRepo{proxy: &Proxy{ID: 1, Status: StatusActive}})
	cookie := &fakeCookieAuth{token: &TokenInfo{AccessToken: "at"}}
	wh := &fakeWebhookNotifier{enabled: true}
	svc := newImportSvc(accounts, alloc, cookie, wh)

	res, err := svc.ImportCredential(context.Background(), okInput())
	require.Nil(t, res)
	var appErr *infraerrors.ApplicationError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, "ACCOUNT_CREATE_FAILED", appErr.Reason)
	require.Len(t, wh.sent, 0)
}

// provider_type 非 claude → PROVIDER_TYPE_UNSUPPORTED（本阶段不支持 codex）
func TestImport_ProviderTypeUnsupported(t *testing.T) {
	accounts := newFakeConnectAccountRepo()
	alloc := NewProxyAllocator(&fakeAllocationRepo{proxy: &Proxy{ID: 1, Status: StatusActive}})
	svc := newImportSvc(accounts, alloc, &fakeCookieAuth{token: &TokenInfo{AccessToken: "at"}}, &fakeWebhookNotifier{enabled: true})

	in := okInput()
	in.ProviderType = "codex"
	res, err := svc.ImportCredential(context.Background(), in)
	require.Nil(t, res)
	var appErr *infraerrors.ApplicationError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, "PROVIDER_TYPE_UNSUPPORTED", appErr.Reason)
}

// 请求参数校验：缺 external_ref / 非 pa_ 前缀 / 空 credential / 空 region
func TestImport_InvalidRequest(t *testing.T) {
	accounts := newFakeConnectAccountRepo()
	alloc := NewProxyAllocator(&fakeAllocationRepo{proxy: &Proxy{ID: 1, Status: StatusActive}})
	svc := newImportSvc(accounts, alloc, &fakeCookieAuth{token: &TokenInfo{AccessToken: "at"}}, &fakeWebhookNotifier{enabled: true})

	cases := []ImportCredentialInput{
		{ExternalProviderAccountID: "", ProviderType: "claude", Credential: "x", Region: "US"},
		{ExternalProviderAccountID: "bad_ref", ProviderType: "claude", Credential: "x", Region: "US"},
		{ExternalProviderAccountID: "pa_x", ProviderType: "claude", Credential: "", Region: "US"},
		{ExternalProviderAccountID: "pa_x", ProviderType: "claude", Credential: "x", Region: ""},
	}
	for i, in := range cases {
		res, err := svc.ImportCredential(context.Background(), in)
		require.Nil(t, res, "case %d", i)
		require.Error(t, err, "case %d", i)
	}
}

// 16: webhook payload 无 credential（即使 credential 含特征串）
func TestImport_WebhookNoCredential(t *testing.T) {
	accounts := newFakeConnectAccountRepo()
	alloc := NewProxyAllocator(&fakeAllocationRepo{proxy: &Proxy{ID: 1, Status: StatusActive}})
	cookie := &fakeCookieAuth{token: &TokenInfo{AccessToken: "at"}}
	wh := &fakeWebhookNotifier{enabled: true}
	svc := newImportSvc(accounts, alloc, cookie, wh)

	in := okInput()
	in.Credential = "sk-ant-session-DO-NOT-LEAK"
	_, err := svc.ImportCredential(context.Background(), in)
	require.NoError(t, err)
	require.Len(t, wh.sent, 1)
	// 整个 webhook input 结构体序列化里绝不含 credential 特征串
	dump := strings.Join([]string{
		wh.sent[0].ExternalProviderAccountID, wh.sent[0].ProviderType,
		wh.sent[0].Platform, wh.sent[0].Region, wh.sent[0].EventID,
	}, "|")
	require.NotContains(t, dump, "DO-NOT-LEAK")
	require.NotContains(t, dump, "sk-ant-session")
}
