package service

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func makeCodexJWT(t *testing.T, payload map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	pj, err := json.Marshal(payload)
	require.NoError(t, err)
	return header + "." + base64.RawURLEncoding.EncodeToString(pj) + ".sig"
}

// 裸 access_token(非 JWT):解析成 access_token,带"无过期/无 refresh"警告但成功。
func TestParseCodexCredential_PlainToken(t *testing.T) {
	out, err := ParseCodexCredentialString("opaque-access-token")
	require.NoError(t, err)
	require.Equal(t, "opaque-access-token", out.Credentials["access_token"])
	require.NotEmpty(t, out.Warnings)
}

// codex session JSON:提取 tokens.access_token / refresh_token(+client_id)/ id_token。
func TestParseCodexCredential_SessionJSON(t *testing.T) {
	content := `{"tokens":{"access_token":"AT","refresh_token":"RT","id_token":"ID"},"email":"x@y.com"}`
	out, err := ParseCodexCredentialString(content)
	require.NoError(t, err)
	require.Equal(t, "AT", out.Credentials["access_token"])
	require.Equal(t, "RT", out.Credentials["refresh_token"])
	require.NotEmpty(t, out.Credentials["client_id"])
	require.Equal(t, "ID", out.Credentials["id_token"])
	require.Equal(t, "x@y.com", out.Credentials["email"])
}

// JWT access_token:提取 exp(未过期)+ 身份(email/chatgpt_account_id/plan)。
func TestParseCodexCredential_JWTIdentity(t *testing.T) {
	exp := time.Now().Add(2 * time.Hour).Unix()
	jwt := makeCodexJWT(t, map[string]any{
		"exp":   exp,
		"email": "j@w.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acc1",
			"chatgpt_user_id":    "usr1",
			"chatgpt_plan_type":  "plus",
		},
	})
	out, err := ParseCodexCredentialString(jwt)
	require.NoError(t, err)
	require.Equal(t, "j@w.com", out.Credentials["email"])
	require.Equal(t, "acc1", out.Credentials["chatgpt_account_id"])
	require.Equal(t, "usr1", out.Credentials["chatgpt_user_id"])
	require.Equal(t, "plus", out.Credentials["plan_type"])
	require.NotEmpty(t, out.Credentials["expires_at"])
}

// 已过期的 JWT → 报错(拒绝导入)。
func TestParseCodexCredential_ExpiredJWT(t *testing.T) {
	jwt := makeCodexJWT(t, map[string]any{"exp": time.Now().Add(-2 * time.Hour).Unix()})
	_, err := ParseCodexCredentialString(jwt)
	require.Error(t, err)
}

func TestParseCodexCredential_Empty(t *testing.T) {
	_, err := ParseCodexCredentialString("   ")
	require.Error(t, err)
}
