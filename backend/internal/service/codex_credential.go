package service

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// #94: 共享的 Codex/OpenAI 凭据解析。与 admin 的 ImportCodexSession
// (handler/admin/account_codex_import.go)"同样的方式":解析 access_token(JWT)
// / codex session JSON —— 纯解析,不走网络交换(claude 的 sessionKey 才需 CookieAuth)。
// 从 JWT 提取身份(chatgpt_account_id/user_id/email/plan/org)与过期时间,产出
// CreateConnectedAccount 需要的 credentials。供 provider-internal 导入(codex 分支)复用。
//
// 注:admin 的批量导入仍有自己的一份实现(账号去重/合并等 admin 编排),此处为
// provider 单条导入的独立解析,行为对齐;后续可择机抽公共库去重。

const codexCredentialClockSkewSeconds int64 = 120

// CodexCredential 是解析结果:可直接落库的 credentials + 身份/过期信息 + 警告。
type CodexCredential struct {
	Credentials    map[string]any
	Email          string
	AccountID      string
	UserID         string
	PlanType       string
	Organization   string
	RefreshToken   string
	TokenExpiresAt *time.Time
	Warnings       []string
}

type codexCredJWTClaims struct {
	Sub        string                    `json:"sub"`
	Email      string                    `json:"email"`
	Exp        int64                     `json:"exp"`
	OpenAIAuth *codexCredJWTOpenAIClaims `json:"https://api.openai.com/auth,omitempty"`
}

type codexCredJWTOpenAIClaims struct {
	ChatGPTAccountID string                     `json:"chatgpt_account_id"`
	ChatGPTUserID    string                     `json:"chatgpt_user_id"`
	ChatGPTPlanType  string                     `json:"chatgpt_plan_type"`
	UserID           string                     `json:"user_id"`
	POID             string                     `json:"poid"`
	Organizations    []openai.OrganizationClaim `json:"organizations"`
}

// ParseCodexCredentialString 解析单条凭据字符串:JSON(codex session)或裸 access_token。
func ParseCodexCredentialString(content string) (*CodexCredential, error) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return nil, errors.New("empty codex credential")
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		var value any
		dec := json.NewDecoder(strings.NewReader(trimmed))
		dec.UseNumber()
		if err := dec.Decode(&value); err != nil {
			return nil, fmt.Errorf("codex JSON 解析失败: %w", err)
		}
		if arr, ok := value.([]any); ok {
			if len(arr) == 0 {
				return nil, errors.New("empty codex JSON array")
			}
			value = arr[0] // provider 单条导入:取第一条
		}
		return parseCodexCredentialValue(value)
	}
	return parseCodexCredentialValue(trimmed)
}

// parseCodexCredentialValue 把 string(access_token)或 map(session JSON)解析成 CodexCredential。
func parseCodexCredentialValue(value any) (*CodexCredential, error) {
	now := time.Now().UTC()
	out := &CodexCredential{Credentials: map[string]any{}}
	var accessToken, idToken string

	switch raw := value.(type) {
	case string:
		accessToken = strings.TrimSpace(raw)
	case map[string]any:
		accessToken = firstCodexCredString(raw,
			[]string{"tokens", "access_token"}, []string{"tokens", "accessToken"},
			[]string{"access_token"}, []string{"accessToken"}, []string{"token"})
		out.RefreshToken = firstCodexCredString(raw,
			[]string{"tokens", "refresh_token"}, []string{"tokens", "refreshToken"},
			[]string{"refresh_token"}, []string{"refreshToken"})
		idToken = firstCodexCredString(raw,
			[]string{"tokens", "id_token"}, []string{"tokens", "idToken"},
			[]string{"id_token"}, []string{"idToken"})
		out.Email = firstCodexCredString(raw, []string{"email"}, []string{"user", "email"})
		out.AccountID = firstCodexCredString(raw,
			[]string{"chatgpt_account_id"}, []string{"chatgptAccountId"},
			[]string{"account_id"}, []string{"accountId"},
			[]string{"account", "id"}, []string{"account", "account_id"},
			[]string{"account", "chatgpt_account_id"})
		out.UserID = firstCodexCredString(raw,
			[]string{"chatgpt_user_id"}, []string{"chatgptUserId"},
			[]string{"user_id"}, []string{"userId"}, []string{"user", "id"})
		out.PlanType = firstCodexCredString(raw,
			[]string{"plan_type"}, []string{"planType"},
			[]string{"account", "plan_type"}, []string{"account", "planType"})
		out.Organization = firstCodexCredString(raw,
			[]string{"organization_id"}, []string{"organizationId"},
			[]string{"org_id"}, []string{"orgId"})
		if tokenExpiresAt, ok := firstCodexCredTime(raw,
			[]string{"tokens", "expires_at"}, []string{"tokens", "expiresAt"},
			[]string{"expires_at"}, []string{"expiresAt"}); ok {
			if tokenExpiresAt.Unix() <= now.Unix()-codexCredentialClockSkewSeconds {
				return nil, fmt.Errorf("access_token 已过期: %s", tokenExpiresAt.Format(time.RFC3339))
			}
			t := tokenExpiresAt
			out.TokenExpiresAt = &t
			out.Credentials["expires_at"] = tokenExpiresAt.Format(time.RFC3339)
		}
	default:
		return nil, errors.New("codex 凭据格式不支持(需 access_token 字符串或 session JSON)")
	}

	if accessToken == "" {
		return nil, errors.New("缺少 accessToken/access_token")
	}
	out.Credentials["access_token"] = accessToken
	if out.RefreshToken != "" {
		out.Credentials["refresh_token"] = out.RefreshToken
		out.Credentials["client_id"] = openai.ClientID
	}
	if idToken != "" {
		out.Credentials["id_token"] = idToken
		_ = enrichCodexCredFromJWT(out, idToken, false, now)
	}
	if err := enrichCodexCredFromJWT(out, accessToken, true, now); err != nil {
		return nil, err
	}
	if _, ok := out.Credentials["expires_at"]; !ok {
		out.Warnings = append(out.Warnings, "无法从 accessToken 解析过期时间,导入后需自行确认令牌有效性")
	}
	if out.RefreshToken == "" {
		out.Warnings = append(out.Warnings, "未包含 refresh_token,accessToken 过期后无法自动续期")
	}

	setCodexCredIfNotEmpty(out.Credentials, "email", out.Email)
	setCodexCredIfNotEmpty(out.Credentials, "chatgpt_account_id", out.AccountID)
	setCodexCredIfNotEmpty(out.Credentials, "chatgpt_user_id", out.UserID)
	setCodexCredIfNotEmpty(out.Credentials, "organization_id", out.Organization)
	setCodexCredIfNotEmpty(out.Credentials, "plan_type", out.PlanType)
	return out, nil
}

func enrichCodexCredFromJWT(out *CodexCredential, token string, validateExpiry bool, now time.Time) error {
	claims, err := decodeCodexCredJWTClaims(token)
	if err != nil {
		if validateExpiry {
			out.Warnings = append(out.Warnings, "accessToken 不是可解析 JWT,无法校验过期时间和账号身份")
		}
		return nil
	}
	if validateExpiry && claims.Exp > 0 {
		if now.Unix() > claims.Exp+codexCredentialClockSkewSeconds {
			return fmt.Errorf("access_token 已过期: %s", time.Unix(claims.Exp, 0).UTC().Format(time.RFC3339))
		}
		expiresAt := time.Unix(claims.Exp, 0).UTC()
		out.TokenExpiresAt = &expiresAt
		out.Credentials["expires_at"] = expiresAt.Format(time.RFC3339)
	}
	if out.Email == "" {
		out.Email = strings.TrimSpace(claims.Email)
	}
	if claims.OpenAIAuth == nil {
		if out.UserID == "" {
			out.UserID = strings.TrimSpace(claims.Sub)
		}
		return nil
	}
	if out.AccountID == "" {
		out.AccountID = strings.TrimSpace(claims.OpenAIAuth.ChatGPTAccountID)
	}
	if out.UserID == "" {
		out.UserID = strings.TrimSpace(claims.OpenAIAuth.ChatGPTUserID)
	}
	if out.UserID == "" {
		out.UserID = strings.TrimSpace(claims.OpenAIAuth.UserID)
	}
	if out.PlanType == "" {
		out.PlanType = strings.TrimSpace(claims.OpenAIAuth.ChatGPTPlanType)
	}
	if out.Organization == "" {
		out.Organization = strings.TrimSpace(claims.OpenAIAuth.POID)
	}
	if out.Organization == "" {
		for _, org := range claims.OpenAIAuth.Organizations {
			if org.IsDefault {
				out.Organization = org.ID
				break
			}
		}
	}
	if out.Organization == "" && len(claims.OpenAIAuth.Organizations) > 0 {
		out.Organization = claims.OpenAIAuth.Organizations[0].ID
	}
	if out.UserID == "" {
		out.UserID = strings.TrimSpace(claims.Sub)
	}
	return nil
}

func decodeCodexCredJWTClaims(token string) (*codexCredJWTClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid JWT format")
	}
	payload, err := decodeCodexCredJWTSegment(parts[1])
	if err != nil {
		return nil, err
	}
	var claims codexCredJWTClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, err
	}
	return &claims, nil
}

func decodeCodexCredJWTSegment(segment string) ([]byte, error) {
	if decoded, err := base64.RawURLEncoding.DecodeString(segment); err == nil {
		return decoded, nil
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(segment); err == nil {
		return decoded, nil
	}
	padded := segment
	if rem := len(padded) % 4; rem > 0 {
		padded += strings.Repeat("=", 4-rem)
	}
	if decoded, err := base64.URLEncoding.DecodeString(padded); err == nil {
		return decoded, nil
	}
	return base64.StdEncoding.DecodeString(padded)
}

func firstCodexCredString(obj map[string]any, paths ...[]string) string {
	for _, path := range paths {
		if value, ok := codexCredPathValue(obj, path); ok {
			if str := codexCredStringValue(value); str != "" {
				return str
			}
		}
	}
	return ""
}

func firstCodexCredTime(obj map[string]any, paths ...[]string) (time.Time, bool) {
	for _, path := range paths {
		if value, ok := codexCredPathValue(obj, path); ok {
			if t, ok := parseCodexCredTimeValue(value); ok {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

func codexCredPathValue(obj map[string]any, path []string) (any, bool) {
	var current any = obj
	for _, key := range path {
		currentObj, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		value, ok := currentObj[key]
		if !ok {
			return nil, false
		}
		current = value
	}
	return current, true
}

func codexCredStringValue(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return strings.TrimSpace(v.String())
	case float64:
		return strings.TrimSpace(strconv.FormatFloat(v, 'f', -1, 64))
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	default:
		return ""
	}
}

func parseCodexCredTimeValue(value any) (time.Time, bool) {
	switch v := value.(type) {
	case string:
		v = strings.TrimSpace(v)
		if v == "" {
			return time.Time{}, false
		}
		if parsed, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return parsed.UTC(), true
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return codexCredUnixTime(n), true
		}
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return codexCredUnixTime(n), true
		}
		if f, err := v.Float64(); err == nil {
			return codexCredUnixTime(int64(f)), true
		}
	case float64:
		return codexCredUnixTime(int64(v)), true
	case int:
		return codexCredUnixTime(int64(v)), true
	case int64:
		return codexCredUnixTime(v), true
	}
	return time.Time{}, false
}

func codexCredUnixTime(value int64) time.Time {
	if value > 1_000_000_000_000 {
		return time.UnixMilli(value).UTC()
	}
	return time.Unix(value, 0).UTC()
}

func setCodexCredIfNotEmpty(credentials map[string]any, key, value string) {
	value = strings.TrimSpace(value)
	if value != "" {
		credentials[key] = value
	}
}
