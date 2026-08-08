package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// #95 openai 手动授权:完成流程走 openai 分支 —— 用 session_id+code+state 换 token
// (state 必须透传),BuildAccountCredentials 产凭据,建 platform=openai 的号。
type fakeOpenAIExchanger struct {
	tok      *OpenAITokenInfo
	err      error
	gotState string
	gotCode  string
}

func (f *fakeOpenAIExchanger) ExchangeCode(_ context.Context, in *OpenAIExchangeCodeInput) (*OpenAITokenInfo, error) {
	f.gotState = in.State
	f.gotCode = in.Code
	if f.err != nil {
		return nil, f.err
	}
	return f.tok, nil
}

func (f *fakeOpenAIExchanger) BuildAccountCredentials(t *OpenAITokenInfo) map[string]any {
	return map[string]any{"access_token": t.AccessToken, "email": t.Email}
}

func TestComplete_OpenAI_ManualAuth_Success(t *testing.T) {
	now := time.Now()
	store := newFakeSessionStore()
	accounts := newFakeConnectAccountRepo()
	oauthID := "oa-sess-1"
	region := "US"
	sess, _ := store.Create(context.Background(), &ProviderConnectSession{
		ExternalProviderAccountID: "pa_oai",
		ProviderType:              "codex",
		Region:                    &region,
		Status:                    "pending",
		OAuthSessionID:            &oauthID,
		ExpiresAt:                 now.Add(5 * time.Minute),
	})
	oaEx := &fakeOpenAIExchanger{tok: &OpenAITokenInfo{AccessToken: "oat", Email: "e@x.com", PlanType: "plus"}}
	svc := &ProviderConnectCompletionService{
		sessions:    store,
		accounts:    accounts,
		openaiOAuth: oaEx,
		now:         func() time.Time { return now },
	}

	res, err := svc.CompleteAuthorization(context.Background(), CompleteAuthorizationInput{
		SessionID: sess.ID,
		Code:      "code123",
		State:     "st123",
	})
	require.NoError(t, err)
	require.Equal(t, "completed", res.Status)
	require.Equal(t, "st123", oaEx.gotState, "state 必须透传给 openai ExchangeCode(CSRF)")
	require.Equal(t, "code123", oaEx.gotCode)
	require.Equal(t, "e@x.com", res.Email)
	require.Equal(t, "plus", res.Plan)
	require.Equal(t, 1, accounts.createN, "应建一个 openai 号")
}

// openai 分支未装配 OpenAIOAuthService → 明确报错(不静默)。
func TestComplete_OpenAI_MissingService(t *testing.T) {
	now := time.Now()
	store := newFakeSessionStore()
	oauthID := "oa-sess-2"
	sess, _ := store.Create(context.Background(), &ProviderConnectSession{
		ExternalProviderAccountID: "pa_oai2",
		ProviderType:              "openai",
		Status:                    "pending",
		OAuthSessionID:            &oauthID,
		ExpiresAt:                 now.Add(5 * time.Minute),
	})
	svc := &ProviderConnectCompletionService{
		sessions: store,
		accounts: newFakeConnectAccountRepo(),
		now:      func() time.Time { return now },
	}
	_, err := svc.CompleteAuthorization(context.Background(), CompleteAuthorizationInput{
		SessionID: sess.ID, Code: "c", State: "s",
	})
	require.Error(t, err)
}
