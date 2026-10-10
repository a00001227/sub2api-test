package service

import (
	"context"
	"errors"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func importAppErr(t *testing.T, err error) *infraerrors.ApplicationError {
	t.Helper()
	var appErr *infraerrors.ApplicationError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, "INVALID_CREDENTIAL", appErr.Reason)
	return appErr
}

// sessionKey 前缀不对:不打上游,直接给出格式原因。
func TestImport_ClaudeSessionKeyPrefixRejected(t *testing.T) {
	accounts := newFakeConnectAccountRepo()
	alloc := NewProxyAllocator(&fakeAllocationRepo{proxy: &Proxy{ID: 1, Status: StatusActive}}, nil)
	cookie := &fakeCookieAuth{token: &TokenInfo{AccessToken: "x"}}
	svc := newImportSvc(accounts, alloc, cookie, &fakeWebhookNotifier{})

	in := okInput()
	in.Credential = "sk-ant-api03-not-a-session-key"
	_, err := svc.ImportCredential(context.Background(), in)
	appErr := importAppErr(t, err)
	require.Contains(t, appErr.Message, "sk-ant-sid01-")
	require.Equal(t, "format", appErr.Metadata["stage"])
	require.Equal(t, 0, cookie.callN, "格式错不应打上游")
	require.Equal(t, 0, accounts.createN)
}

// 上游原因要原样(脱敏后)透出:Cloudflare 挑战页 / 会话不够新 / 网络层失败,各自可区分。
func TestImport_InvalidCredential_SurfacesRealReason(t *testing.T) {
	cases := []struct {
		name         string
		cookieErr    string
		wantStage    string
		wantStatus   string
		wantContains string
		wantAbsent   string
	}{
		{
			name:         "cloudflare challenge on org lookup",
			cookieErr:    `failed to get organization info: failed to get organizations: status 403, body: <!DOCTYPE html><html lang="en-US"><head><title>Just a moment...</title></head></html>`,
			wantStage:    "org_lookup",
			wantStatus:   "403",
			wantContains: "Cloudflare",
		},
		{
			name:         "session not fresh on authorize",
			cookieErr:    `failed to get authorization code: failed to get authorization code: status 403, body: {"type":"error","error":{"type":"permission_error","message":"Session is not fresh enough to grant elevated access. Sign in again."}}`,
			wantStage:    "authorize",
			wantStatus:   "403",
			wantContains: "Session is not fresh enough",
		},
		{
			name:         "transport failure hides proxy credentials",
			cookieErr:    `failed to get organization info: request failed: Get "https://claude.ai/api/organizations": socks5://user1:pass1@10.0.0.1:1080: dial tcp: connection refused`,
			wantStage:    "org_lookup",
			wantStatus:   "transport_error",
			wantContains: "connection refused",
			wantAbsent:   "pass1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			accounts := newFakeConnectAccountRepo()
			alloc := NewProxyAllocator(&fakeAllocationRepo{proxy: &Proxy{ID: 1, Status: StatusActive}}, nil)
			svc := newImportSvc(accounts, alloc, &fakeCookieAuth{err: errors.New(tc.cookieErr)}, &fakeWebhookNotifier{})

			_, err := svc.ImportCredential(context.Background(), okInput())
			appErr := importAppErr(t, err)
			require.Contains(t, appErr.Message, tc.wantContains)
			require.Equal(t, tc.wantStage, appErr.Metadata["stage"])
			require.Equal(t, tc.wantStatus, appErr.Metadata["upstream_status"])
			require.Equal(t, "true", appErr.Metadata["proxy_used"])
			if tc.wantAbsent != "" {
				require.NotContains(t, appErr.Message, tc.wantAbsent)
			}
			require.NotContains(t, appErr.Message, "SECRET")
		})
	}
}

// codex 解析失败也要给真实原因。
func TestImport_CodexParseErrorSurfaced(t *testing.T) {
	accounts := newFakeConnectAccountRepo()
	alloc := NewProxyAllocator(&fakeAllocationRepo{proxy: &Proxy{ID: 1, Status: StatusActive}}, nil)
	svc := newImportSvc(accounts, alloc, &fakeCookieAuth{}, &fakeWebhookNotifier{})

	in := okInput()
	in.ProviderType = "codex"
	in.Credential = "{not json"
	_, err := svc.ImportCredential(context.Background(), in)
	appErr := importAppErr(t, err)
	require.Contains(t, appErr.Message, "codex credential rejected")
	require.Equal(t, "parse", appErr.Metadata["stage"])
}

// 自有代理地区在本 cell 找不到代理 → 明确报错,边缘模式也不降级直连。
func TestSelectProxy_OwnedRegionNeverFallsBackToDirect(t *testing.T) {
	repo := &fakeAllocationRepo{proxy: nil}
	a := NewProxyAllocator(repo, nil)
	a.SetEdgeMode(true)

	_, err := a.SelectProxy(context.Background(), "own-b62acd35", "anthropic")
	require.ErrorIs(t, err, ErrOwnedProxyNotSynced)
	var appErr *infraerrors.ApplicationError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, "OWN-B62ACD35", appErr.Metadata["region"])

	// 普通地区在边缘模式仍保持原行为(直连)
	p, err := a.SelectProxy(context.Background(), "jnb", "anthropic")
	require.NoError(t, err)
	require.Nil(t, p)
}

func TestRedactImportDetail(t *testing.T) {
	got := redactImportDetail(`cookie auth failed for sk-ant-sid01-ABCDEFGH12345 via socks5://u:p@1.2.3.4:1080`)
	require.NotContains(t, got, "ABCDEFGH")
	require.NotContains(t, got, "u:p@")
	require.Contains(t, got, "sk-***")
	require.Contains(t, got, "socks5://***@1.2.3.4:1080")
}
