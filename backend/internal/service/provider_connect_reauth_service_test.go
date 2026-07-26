package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var errReauthTest = errors.New("boom")

// Phase 21G provider-reauth: 重授权会话创建 + 完成分支单元测试（全假件）。

// --- 假账号读写：满足 reauthAccountReader / connectReauthAccountUpdater ---
type fakeReauthAccountStore struct {
	byID     map[int64]*Account
	updated  []*Account
	updateErr error
}

func newFakeReauthAccountStore() *fakeReauthAccountStore {
	return &fakeReauthAccountStore{byID: map[int64]*Account{}}
}
func (f *fakeReauthAccountStore) GetByID(_ context.Context, id int64) (*Account, error) {
	a, ok := f.byID[id]
	if !ok {
		return nil, nil
	}
	cp := *a
	return &cp, nil
}
func (f *fakeReauthAccountStore) Update(_ context.Context, a *Account) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	cp := *a
	f.updated = append(f.updated, &cp)
	f.byID[a.ID] = &cp
	return nil
}

type fakeReauthLocator struct {
	byRef map[string]int64
}

func (f *fakeReauthLocator) FindAccountIDByExternalRef(_ context.Context, ref string) (int64, bool, error) {
	id, ok := f.byRef[ref]
	return id, ok, nil
}

func newReauthSvc(
	sessions ProviderConnectSessionRepository,
	locator *fakeReauthLocator,
	accounts *fakeReauthAccountStore,
	oauth connectOAuthURLGenerator,
) *ProviderConnectReauthService {
	return &ProviderConnectReauthService{
		sessions: sessions,
		locator:  locator,
		accounts: accounts,
		oauth:    oauth,
		now:      func() time.Time { return time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC) },
	}
}

func validReauthInput() CreateReauthSessionInput {
	return CreateReauthSessionInput{
		ExternalProviderAccountID: "pa_reauth1",
		ProviderType:              "claude",
		CallbackURL:               "https://portal.example.com/internal/sub2api/events",
	}
}

func TestReauthSession_Success_PrefillsAccountAndReusesProxy(t *testing.T) {
	store := newFakeSessionStore()
	proxyID := int64(7)
	accounts := newFakeReauthAccountStore()
	accounts.byID[55] = &Account{ID: 55, Platform: PlatformAnthropic, ProxyID: &proxyID, Status: StatusError}
	locator := &fakeReauthLocator{byRef: map[string]int64{"pa_reauth1": 55}}
	oauth := &fakeOAuthURLGen{result: &GenerateAuthURLResult{AuthURL: "https://claude.ai/oauth?x=1", SessionID: "os_1"}}

	svc := newReauthSvc(store, locator, accounts, oauth)
	res, err := svc.CreateReauthSession(context.Background(), validReauthInput())
	require.NoError(t, err)
	require.NotEmpty(t, res.OnboardingSessionID)
	require.Equal(t, "https://claude.ai/oauth?x=1", res.OnboardingURL)

	// OAuth URL 必须走该账号已绑定的 proxy（同出口环境）。
	require.NotNil(t, oauth.gotProxyID)
	require.Equal(t, proxyID, *oauth.gotProxyID)

	// 会话预填 sub2api_account_id —— 完成流程据此走换凭证分支。
	sess := store.byID[1]
	require.NotNil(t, sess.Sub2apiAccountID)
	require.Equal(t, int64(55), *sess.Sub2apiAccountID)
	require.Equal(t, "pending", sess.Status)
	require.Equal(t, proxyID, *sess.ProxyID)
}

func TestReauthSession_UnknownRef_NotFound(t *testing.T) {
	store := newFakeSessionStore()
	svc := newReauthSvc(store, &fakeReauthLocator{byRef: map[string]int64{}}, newFakeReauthAccountStore(),
		&fakeOAuthURLGen{result: &GenerateAuthURLResult{AuthURL: "u", SessionID: "s"}})
	_, err := svc.CreateReauthSession(context.Background(), validReauthInput())
	require.ErrorIs(t, err, ErrReauthAccountNotFound)
	require.Empty(t, store.byID, "no session on failure")
}

func TestReauthSession_InvalidRef_Rejected(t *testing.T) {
	svc := newReauthSvc(newFakeSessionStore(), &fakeReauthLocator{}, newFakeReauthAccountStore(), &fakeOAuthURLGen{})
	in := validReauthInput()
	in.ExternalProviderAccountID = "not-prefixed"
	_, err := svc.CreateReauthSession(context.Background(), in)
	require.ErrorIs(t, err, ErrConnectInvalidAccountRef)
}

// --- 完成分支 ---

func seedReauthPendingSession(store *fakeSessionStore, now time.Time, accountID int64) *ProviderConnectSession {
	oauthID := "oauth-sess-r"
	proxyID := int64(7)
	s, _ := store.Create(context.Background(), &ProviderConnectSession{
		ExternalProviderAccountID: "pa_reauth1",
		ProviderType:              "claude",
		ProxyID:                   &proxyID,
		Status:                    "pending",
		OAuthSessionID:            &oauthID,
		Sub2apiAccountID:          &accountID,
		CallbackURL:               "https://portal/cb",
		ExpiresAt:                 now.Add(5 * time.Minute),
	})
	return s
}

func newReauthCompletionSvc(
	store *fakeSessionStore, accounts *fakeConnectAccountRepo,
	reauthAccounts *fakeReauthAccountStore, ex *fakeExchanger, now time.Time,
) *ProviderConnectCompletionService {
	return &ProviderConnectCompletionService{
		sessions:       store,
		accounts:       accounts,
		reauthAccounts: reauthAccounts,
		oauth:          ex,
		now:            func() time.Time { return now },
	}
}

func TestCompleteReauth_UpdatesCredentialsAndRestoresActive(t *testing.T) {
	now := time.Now()
	store := newFakeSessionStore()
	accounts := newFakeConnectAccountRepo()
	reauthAccounts := newFakeReauthAccountStore()
	proxyID := int64(7)
	reauthAccounts.byID[55] = &Account{
		ID: 55, Platform: PlatformAnthropic, ProxyID: &proxyID,
		Status: StatusError, ErrorMessage: "token expired", Schedulable: false,
		Credentials: map[string]any{"access_token": "OLD"},
	}
	ex := &fakeExchanger{token: &TokenInfo{
		AccessToken: "NEW-AT", RefreshToken: "NEW-RT", ExpiresAt: 999,
		TokenType: "Bearer", EmailAddress: "p@example.com", RateLimitTier: "default_claude_max_20x",
	}}
	sess := seedReauthPendingSession(store, now, 55)

	svc := newReauthCompletionSvc(store, accounts, reauthAccounts, ex, now)
	res, err := svc.CompleteAuthorization(context.Background(), CompleteAuthorizationInput{SessionID: sess.ID, Code: "code-1"})
	require.NoError(t, err)
	require.Equal(t, "completed", res.Status)
	require.Equal(t, int64(55), res.AccountID)
	require.Equal(t, "p@example.com", res.Email)
	require.Equal(t, "default_claude_max_20x", res.Plan)

	// 不新建账号 —— 更新既有账号。
	require.Equal(t, 0, accounts.createN, "reauth must not create a second account")
	require.Len(t, reauthAccounts.updated, 1)
	upd := reauthAccounts.updated[0]
	require.Equal(t, "NEW-AT", upd.Credentials["access_token"])
	require.Equal(t, "NEW-RT", upd.Credentials["refresh_token"])
	require.Equal(t, StatusActive, upd.Status)
	require.Empty(t, upd.ErrorMessage)
	require.True(t, upd.Schedulable)

	// 会话收敛 completed。
	require.Equal(t, "completed", store.byID[sess.ID].Status)
}

func TestCompleteReauth_ExchangeFails_AccountUntouched(t *testing.T) {
	now := time.Now()
	store := newFakeSessionStore()
	reauthAccounts := newFakeReauthAccountStore()
	reauthAccounts.byID[55] = &Account{ID: 55, Status: StatusError, Credentials: map[string]any{"access_token": "OLD"}}
	ex := &fakeExchanger{err: errReauthTest}
	sess := seedReauthPendingSession(store, now, 55)

	svc := newReauthCompletionSvc(store, newFakeConnectAccountRepo(), reauthAccounts, ex, now)
	_, err := svc.CompleteAuthorization(context.Background(), CompleteAuthorizationInput{SessionID: sess.ID, Code: "bad"})
	require.Error(t, err)
	require.Empty(t, reauthAccounts.updated, "credentials must stay untouched on exchange failure")
	require.Equal(t, "failed", store.byID[sess.ID].Status)
}

func TestCompleteReauth_AccountGone_FailsSession(t *testing.T) {
	now := time.Now()
	store := newFakeSessionStore()
	reauthAccounts := newFakeReauthAccountStore() // 账号不存在（已被删）
	ex := &fakeExchanger{token: &TokenInfo{AccessToken: "at"}}
	sess := seedReauthPendingSession(store, now, 55)

	svc := newReauthCompletionSvc(store, newFakeConnectAccountRepo(), reauthAccounts, ex, now)
	_, err := svc.CompleteAuthorization(context.Background(), CompleteAuthorizationInput{SessionID: sess.ID, Code: "code"})
	require.ErrorIs(t, err, ErrReauthAccountNotFound)
	require.Equal(t, "failed", store.byID[sess.ID].Status)
}
