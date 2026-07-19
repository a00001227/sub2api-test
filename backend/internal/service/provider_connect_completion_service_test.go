package service

import (
	"context"
	"errors"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// Phase 21E-6C-2C: ProviderConnectCompletionService 单元测试（全假件）。

// --- 假 session repo：内存态，支持状态机 ---
type fakeSessionStore struct {
	byID       map[int64]*ProviderConnectSession
	completeN  int // MarkCompleted 成功次数
	failN      int
	failCreate bool
}

func newFakeSessionStore() *fakeSessionStore {
	return &fakeSessionStore{byID: map[int64]*ProviderConnectSession{}}
}
func (f *fakeSessionStore) Create(_ context.Context, s *ProviderConnectSession) (*ProviderConnectSession, error) {
	cp := *s
	cp.ID = int64(len(f.byID) + 1)
	f.byID[cp.ID] = &cp
	return &cp, nil
}
func (f *fakeSessionStore) GetByID(_ context.Context, id int64) (*ProviderConnectSession, error) {
	s, ok := f.byID[id]
	if !ok {
		return nil, nil
	}
	cp := *s
	return &cp, nil
}
func (f *fakeSessionStore) MarkCompleted(_ context.Context, id, accID int64, at time.Time) (int64, error) {
	s, ok := f.byID[id]
	if !ok || s.Status != "pending" {
		return 0, nil
	}
	s.Status = "completed"
	s.Sub2apiAccountID = &accID
	s.CompletedAt = &at
	f.completeN++
	return 1, nil
}
func (f *fakeSessionStore) MarkFailed(_ context.Context, id int64) error {
	if s, ok := f.byID[id]; ok && s.Status == "pending" {
		s.Status = "failed"
		f.failN++
	}
	return nil
}

// --- 假 account repo：内存 + 唯一约束模拟 ---
type fakeConnectAccountRepo struct {
	byRef        map[string]int64
	seq          int64
	createN      int
	createErr    error
	releasedRefs []string
}

func newFakeConnectAccountRepo() *fakeConnectAccountRepo {
	return &fakeConnectAccountRepo{byRef: map[string]int64{}}
}
func (f *fakeConnectAccountRepo) CreateConnectedAccount(_ context.Context, in CreateConnectedAccountInput) (int64, error) {
	if f.createErr != nil {
		return 0, f.createErr
	}
	if _, exists := f.byRef[in.ExternalProviderAccountID]; exists {
		return 0, errors.New("duplicate key: external_provider_account_id") // 唯一约束
	}
	f.seq++
	f.byRef[in.ExternalProviderAccountID] = f.seq
	f.createN++
	return f.seq, nil
}
func (f *fakeConnectAccountRepo) FindAccountIDByExternalRef(_ context.Context, ref string) (int64, bool, error) {
	id, ok := f.byRef[ref]
	return id, ok, nil
}

func (f *fakeConnectAccountRepo) ReleaseAllocationByExternalRef(_ context.Context, ref, _ string) error {
	f.releasedRefs = append(f.releasedRefs, ref)
	return nil
}

// --- 假 OAuth 换 token ---
type fakeExchanger struct {
	gotSessionID string
	gotProxyID   *int64
	token        *TokenInfo
	err          error
	calls        int
}

func (f *fakeExchanger) ExchangeCode(_ context.Context, in *ExchangeCodeInput) (*TokenInfo, error) {
	f.calls++
	f.gotSessionID = in.SessionID
	f.gotProxyID = in.ProxyID
	if f.err != nil {
		return nil, f.err
	}
	return f.token, nil
}

func seedPendingSession(store *fakeSessionStore, now time.Time) *ProviderConnectSession {
	oauthID := "oauth-sess-1"
	proxyID := int64(9)
	region := "US"
	s, _ := store.Create(context.Background(), &ProviderConnectSession{
		ExternalProviderAccountID: "pa_abc",
		ProviderType:              "claude",
		Region:                    &region,
		ProxyID:                   &proxyID,
		Status:                    "pending",
		OAuthSessionID:            &oauthID,
		CallbackURL:               "https://portal/cb",
		ExpiresAt:                 now.Add(5 * time.Minute),
	})
	return s
}

// --- 假 webhook notifier：记录被触发的 activated 事件 ---
type fakeWebhookNotifier struct {
	enabled bool
	sent    []ActivatedWebhookInput
}

func (f *fakeWebhookNotifier) Enabled() bool { return f.enabled }
func (f *fakeWebhookNotifier) SendActivatedAsync(in ActivatedWebhookInput) {
	f.sent = append(f.sent, in)
}

func newCompletionSvc(store *fakeSessionStore, accounts *fakeConnectAccountRepo, ex *fakeExchanger, now time.Time) *ProviderConnectCompletionService {
	return &ProviderConnectCompletionService{
		sessions: store,
		accounts: accounts,
		oauth:    ex,
		now:      func() time.Time { return now },
	}
}

func newCompletionSvcWithWebhook(store *fakeSessionStore, accounts *fakeConnectAccountRepo, ex *fakeExchanger, wh ProviderWebhookNotifier, now time.Time) *ProviderConnectCompletionService {
	return &ProviderConnectCompletionService{
		sessions: store,
		accounts: accounts,
		oauth:    ex,
		webhook:  wh,
		now:      func() time.Time { return now },
	}
}

// 2) 完整成功流程
func TestComplete_Success(t *testing.T) {
	now := time.Now()
	store := newFakeSessionStore()
	accounts := newFakeConnectAccountRepo()
	ex := &fakeExchanger{token: &TokenInfo{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 111, TokenType: "Bearer"}}
	sess := seedPendingSession(store, now)
	svc := newCompletionSvc(store, accounts, ex, now)

	res, err := svc.CompleteAuthorization(context.Background(), CompleteAuthorizationInput{SessionID: sess.ID, Code: "code123"})
	require.NoError(t, err)
	require.Equal(t, "completed", res.Status)
	require.NotZero(t, res.AccountID)

	// OAuth 用了会话的 oauth_session_id + proxy
	require.Equal(t, "oauth-sess-1", ex.gotSessionID)
	require.NotNil(t, ex.gotProxyID)
	require.Equal(t, int64(9), *ex.gotProxyID)

	// 账号建了一个，带归属引用
	require.Equal(t, 1, accounts.createN)
	require.Contains(t, accounts.byRef, "pa_abc")

	// 会话 completed + sub2api_account_id
	after, _ := store.GetByID(context.Background(), sess.ID)
	require.Equal(t, "completed", after.Status)
	require.NotNil(t, after.Sub2apiAccountID)
	require.Equal(t, res.AccountID, *after.Sub2apiAccountID)
}

// 3) 重复 complete：只创建一个账号
func TestComplete_Idempotent(t *testing.T) {
	now := time.Now()
	store := newFakeSessionStore()
	accounts := newFakeConnectAccountRepo()
	ex := &fakeExchanger{token: &TokenInfo{AccessToken: "at"}}
	sess := seedPendingSession(store, now)
	svc := newCompletionSvc(store, accounts, ex, now)

	r1, err := svc.CompleteAuthorization(context.Background(), CompleteAuthorizationInput{SessionID: sess.ID, Code: "c"})
	require.NoError(t, err)
	r2, err := svc.CompleteAuthorization(context.Background(), CompleteAuthorizationInput{SessionID: sess.ID, Code: "c"})
	require.NoError(t, err)

	require.Equal(t, r1.AccountID, r2.AccountID, "same account returned on replay")
	require.Equal(t, 1, accounts.createN, "must not create a second account")
	require.Equal(t, 1, ex.calls, "second call must not re-exchange")
}

// 4) OAuth 失败 → 会话 failed，不建账号
func TestComplete_OAuthFailure(t *testing.T) {
	now := time.Now()
	store := newFakeSessionStore()
	accounts := newFakeConnectAccountRepo()
	ex := &fakeExchanger{err: errors.New("bad code")}
	sess := seedPendingSession(store, now)
	svc := newCompletionSvc(store, accounts, ex, now)

	_, err := svc.CompleteAuthorization(context.Background(), CompleteAuthorizationInput{SessionID: sess.ID, Code: "c"})
	require.Error(t, err)
	require.Equal(t, 0, accounts.createN, "no account on OAuth failure")
	after, _ := store.GetByID(context.Background(), sess.ID)
	require.Equal(t, "failed", after.Status)
}

// 5) 会话异常态
func TestComplete_SessionStates(t *testing.T) {
	now := time.Now()

	t.Run("not found", func(t *testing.T) {
		svc := newCompletionSvc(newFakeSessionStore(), newFakeConnectAccountRepo(), &fakeExchanger{}, now)
		_, err := svc.CompleteAuthorization(context.Background(), CompleteAuthorizationInput{SessionID: 999, Code: "c"})
		require.True(t, infraerrors.IsNotFound(err))
	})

	t.Run("expired", func(t *testing.T) {
		store := newFakeSessionStore()
		s := seedPendingSession(store, now)
		s.ExpiresAt = now.Add(-time.Minute)
		store.byID[s.ID] = s
		svc := newCompletionSvc(store, newFakeConnectAccountRepo(), &fakeExchanger{}, now)
		_, err := svc.CompleteAuthorization(context.Background(), CompleteAuthorizationInput{SessionID: s.ID, Code: "c"})
		require.ErrorIs(t, err, ErrConnectSessionExpired)
		after, _ := store.GetByID(context.Background(), s.ID)
		require.Equal(t, "failed", after.Status)
	})

	t.Run("failed terminal", func(t *testing.T) {
		store := newFakeSessionStore()
		s := seedPendingSession(store, now)
		s.Status = "failed"
		store.byID[s.ID] = s
		svc := newCompletionSvc(store, newFakeConnectAccountRepo(), &fakeExchanger{}, now)
		_, err := svc.CompleteAuthorization(context.Background(), CompleteAuthorizationInput{SessionID: s.ID, Code: "c"})
		require.ErrorIs(t, err, ErrConnectSessionFailed)
	})

	t.Run("already completed returns existing account", func(t *testing.T) {
		store := newFakeSessionStore()
		s := seedPendingSession(store, now)
		accID := int64(77)
		s.Status = "completed"
		s.Sub2apiAccountID = &accID
		store.byID[s.ID] = s
		accounts := newFakeConnectAccountRepo()
		ex := &fakeExchanger{}
		svc := newCompletionSvc(store, accounts, ex, now)
		res, err := svc.CompleteAuthorization(context.Background(), CompleteAuthorizationInput{SessionID: s.ID, Code: "c"})
		require.NoError(t, err)
		require.Equal(t, int64(77), res.AccountID)
		require.Equal(t, 0, accounts.createN, "completed session must not create account")
		require.Equal(t, 0, ex.calls, "completed session must not re-exchange")
	})
}

func TestComplete_CodeRequired(t *testing.T) {
	now := time.Now()
	store := newFakeSessionStore()
	seedPendingSession(store, now)
	svc := newCompletionSvc(store, newFakeConnectAccountRepo(), &fakeExchanger{}, now)
	_, err := svc.CompleteAuthorization(context.Background(), CompleteAuthorizationInput{SessionID: 1, Code: "  "})
	require.ErrorIs(t, err, ErrConnectCodeRequired)
}

// --- Phase 21E-6C-2D-1: webhook 接入点测试 ---

// 成功完成 → 触发 activated 事件（enabled 时）
func TestComplete_FiresActivatedWebhook(t *testing.T) {
	now := time.Now()
	store := newFakeSessionStore()
	accounts := newFakeConnectAccountRepo()
	ex := &fakeExchanger{token: &TokenInfo{AccessToken: "at"}}
	wh := &fakeWebhookNotifier{enabled: true}
	sess := seedPendingSession(store, now)
	svc := newCompletionSvcWithWebhook(store, accounts, ex, wh, now)

	res, err := svc.CompleteAuthorization(context.Background(), CompleteAuthorizationInput{SessionID: sess.ID, Code: "c"})
	require.NoError(t, err)
	require.Len(t, wh.sent, 1, "activated webhook must fire on success")
	got := wh.sent[0]
	require.Equal(t, "pa_abc", got.ExternalProviderAccountID)
	require.Equal(t, res.AccountID, got.Sub2apiAccountID)
	require.Equal(t, "claude", got.ProviderType)
	require.Equal(t, "anthropic", got.Platform) // claude → anthropic
	require.Equal(t, "US", got.Region)
	require.Equal(t, sess.ID, got.SessionID)
}

// webhook 关闭（Enabled=false）→ 不发送，业务照常成功
func TestComplete_WebhookDisabledStillSucceeds(t *testing.T) {
	now := time.Now()
	store := newFakeSessionStore()
	accounts := newFakeConnectAccountRepo()
	ex := &fakeExchanger{token: &TokenInfo{AccessToken: "at"}}
	wh := &fakeWebhookNotifier{enabled: false}
	sess := seedPendingSession(store, now)
	svc := newCompletionSvcWithWebhook(store, accounts, ex, wh, now)

	res, err := svc.CompleteAuthorization(context.Background(), CompleteAuthorizationInput{SessionID: sess.ID, Code: "c"})
	require.NoError(t, err)
	require.NotZero(t, res.AccountID, "account creation must succeed regardless of webhook")
	require.Len(t, wh.sent, 0, "disabled webhook must not fire")
}

// OAuth 失败 → 不发 webhook
func TestComplete_NoWebhookOnFailure(t *testing.T) {
	now := time.Now()
	store := newFakeSessionStore()
	accounts := newFakeConnectAccountRepo()
	ex := &fakeExchanger{err: errRepoDown}
	wh := &fakeWebhookNotifier{enabled: true}
	sess := seedPendingSession(store, now)
	svc := newCompletionSvcWithWebhook(store, accounts, ex, wh, now)

	_, err := svc.CompleteAuthorization(context.Background(), CompleteAuthorizationInput{SessionID: sess.ID, Code: "c"})
	require.Error(t, err)
	require.Len(t, wh.sent, 0, "no webhook when completion fails")
}

var errRepoDown = errorString("oauth down")

type errorString string

func (e errorString) Error() string { return string(e) }
