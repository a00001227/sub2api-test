package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Phase 21E-6C-2B-1: ProviderConnectService 单元测试。
// 用假 repo + 假 OAuth 生成器，避免触碰真实 OAuthService/DB。

type fakeConnectSessionRepo struct {
	saved *ProviderConnectSession
	err   error
}

func (f *fakeConnectSessionRepo) Create(_ context.Context, s *ProviderConnectSession) (*ProviderConnectSession, error) {
	if f.err != nil {
		return nil, f.err
	}
	cp := *s
	cp.ID = 42
	cp.CreatedAt = time.Now()
	f.saved = &cp
	return &cp, nil
}

// 下列方法仅为满足接口；2B-1 的创建流程不使用它们。
func (f *fakeConnectSessionRepo) GetByID(context.Context, int64) (*ProviderConnectSession, error) {
	return f.saved, nil
}
func (f *fakeConnectSessionRepo) MarkCompleted(context.Context, int64, int64, time.Time) (int64, error) {
	return 0, nil
}
func (f *fakeConnectSessionRepo) MarkFailed(context.Context, int64) error { return nil }

type fakeOAuthURLGen struct {
	gotProxyID *int64
	result     *GenerateAuthURLResult
	err        error
}

func (f *fakeOAuthURLGen) GenerateAuthURL(_ context.Context, proxyID *int64) (*GenerateAuthURLResult, error) {
	f.gotProxyID = proxyID
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

// newTestConnectService 组装一个用假件驱动的 service，proxy 分配由
// fakeAllocationRepo 提供。
func newTestConnectService(
	sessionRepo ProviderConnectSessionRepository,
	allocProxy *Proxy,
	allocErr error,
	oauth connectOAuthURLGenerator,
) *ProviderConnectService {
	allocator := NewProxyAllocator(&fakeAllocationRepo{proxy: allocProxy, err: allocErr})
	return &ProviderConnectService{
		sessions:  sessionRepo,
		allocator: allocator,
		oauth:     oauth,
		now:       func() time.Time { return time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC) },
	}
}

func validConnectInput() CreateOnboardingSessionInput {
	return CreateOnboardingSessionInput{
		ExternalProviderAccountID: "pa_abc123",
		ProviderType:              "claude",
		Region:                    "US",
		CallbackURL:               "https://portal.example.com/internal/sub2api/events",
	}
}

func TestConnect_Success(t *testing.T) {
	repo := &fakeConnectSessionRepo{}
	oauth := &fakeOAuthURLGen{result: &GenerateAuthURLResult{AuthURL: "https://claude.ai/oauth/authorize?x=1", SessionID: "s1"}}
	svc := newTestConnectService(repo, &Proxy{ID: 9, Status: StatusActive}, nil, oauth)

	res, err := svc.CreateOnboardingSession(context.Background(), validConnectInput())
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, "obs_42", res.OnboardingSessionID)
	require.Equal(t, "https://claude.ai/oauth/authorize?x=1", res.OnboardingURL)
	require.Equal(t, time.Date(2026, 7, 14, 12, 10, 0, 0, time.UTC), res.ExpiresAt)

	// session 落库：pending + proxy 绑定 + region 归一化
	require.NotNil(t, repo.saved)
	require.Equal(t, "pending", repo.saved.Status)
	require.Equal(t, "pa_abc123", repo.saved.ExternalProviderAccountID)
	require.Equal(t, "claude", repo.saved.ProviderType)
	require.NotNil(t, repo.saved.ProxyID)
	require.Equal(t, int64(9), *repo.saved.ProxyID)
	require.NotNil(t, repo.saved.Region)
	require.Equal(t, "US", *repo.saved.Region)

	// 21E-6C-2B-1 修复：OAuth 内存 session id 必须落库，供后续 ExchangeCode。
	require.NotNil(t, repo.saved.OAuthSessionID)
	require.Equal(t, "s1", *repo.saved.OAuthSessionID)

	// OAuth 生成用了分配到的 proxy id（授权出口=使用出口）
	require.NotNil(t, oauth.gotProxyID)
	require.Equal(t, int64(9), *oauth.gotProxyID)
}

func TestConnect_ProxyAllocationBubblesUp(t *testing.T) {
	repo := &fakeConnectSessionRepo{}
	oauth := &fakeOAuthURLGen{result: &GenerateAuthURLResult{}}
	// allocProxy=nil → allocator 返回 ErrRegionNoCapacity
	svc := newTestConnectService(repo, nil, nil, oauth)

	_, err := svc.CreateOnboardingSession(context.Background(), validConnectInput())
	require.ErrorIs(t, err, ErrRegionNoCapacity)
	require.Nil(t, repo.saved, "no session must be persisted when allocation fails")
}

func TestConnect_InvalidAccountRef(t *testing.T) {
	svc := newTestConnectService(&fakeConnectSessionRepo{}, &Proxy{ID: 1}, nil, &fakeOAuthURLGen{})
	for _, ref := range []string{"", "acc_missing_prefix", strings.Repeat("pa_", 30)} {
		in := validConnectInput()
		in.ExternalProviderAccountID = ref
		_, err := svc.CreateOnboardingSession(context.Background(), in)
		require.ErrorIs(t, err, ErrConnectInvalidAccountRef, "ref=%q", ref)
	}
}

func TestConnect_InvalidProviderType(t *testing.T) {
	svc := newTestConnectService(&fakeConnectSessionRepo{}, &Proxy{ID: 1}, nil, &fakeOAuthURLGen{})
	in := validConnectInput()
	in.ProviderType = "midjourney"
	_, err := svc.CreateOnboardingSession(context.Background(), in)
	require.ErrorIs(t, err, ErrConnectInvalidProviderType)
}

func TestConnect_InvalidCallbackURL(t *testing.T) {
	svc := newTestConnectService(&fakeConnectSessionRepo{}, &Proxy{ID: 1}, nil, &fakeOAuthURLGen{})
	for _, cb := range []string{"", "not-a-url", "ftp://x.y/z", "//no-scheme"} {
		in := validConnectInput()
		in.CallbackURL = cb
		_, err := svc.CreateOnboardingSession(context.Background(), in)
		require.ErrorIs(t, err, ErrConnectInvalidCallbackURL, "cb=%q", cb)
	}
}

func TestConnect_OAuthFailureLeavesNoSession(t *testing.T) {
	// OAuth 现在先于会话落库执行（需要它的 SessionID）：OAuth 失败则
	// 整体失败、不留孤儿会话（21E-6C-2B-1 修复后的顺序）。
	repo := &fakeConnectSessionRepo{}
	oauth := &fakeOAuthURLGen{err: errors.New("oauth upstream down")}
	svc := newTestConnectService(repo, &Proxy{ID: 3, Status: StatusActive}, nil, oauth)

	_, err := svc.CreateOnboardingSession(context.Background(), validConnectInput())
	require.Error(t, err)
	require.Nil(t, repo.saved, "no orphan session when OAuth URL generation fails")
}
