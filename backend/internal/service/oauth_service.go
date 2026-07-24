package service

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/oauth"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// OpenAIOAuthClient interface for OpenAI OAuth operations
type OpenAIOAuthClient interface {
	ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI, proxyURL, clientID string) (*openai.TokenResponse, error)
	RefreshToken(ctx context.Context, refreshToken, proxyURL string) (*openai.TokenResponse, error)
	RefreshTokenWithClientID(ctx context.Context, refreshToken, proxyURL string, clientID string) (*openai.TokenResponse, error)
}

// ClaudeOAuthClient handles HTTP requests for Claude OAuth flows
type ClaudeOAuthClient interface {
	GetOrganizationUUID(ctx context.Context, sessionKey, proxyURL string) (string, error)
	GetAuthorizationCode(ctx context.Context, sessionKey, orgUUID, scope, codeChallenge, state, proxyURL string) (string, error)
	ExchangeCodeForToken(ctx context.Context, code, codeVerifier, state, proxyURL string, isSetupToken bool) (*oauth.TokenResponse, error)
	RefreshToken(ctx context.Context, refreshToken, proxyURL string) (*oauth.TokenResponse, error)
	// FetchRateLimitTier best-effort 拉取账号订阅等级（如 "default_claude_max_20x"）。
	// 用 OAuth access token 调 Anthropic profile 接口；失败返回空串+error，调用方
	// 不得因此阻断登录流程。
	FetchRateLimitTier(ctx context.Context, accessToken, proxyURL string) (string, error)
}

// OAuthService handles OAuth authentication flows
type OAuthService struct {
	sessionStore *oauth.SessionStore
	proxyRepo    ProxyRepository
	oauthClient  ClaudeOAuthClient
}

// NewOAuthService creates a new OAuth service
func NewOAuthService(proxyRepo ProxyRepository, oauthClient ClaudeOAuthClient) *OAuthService {
	return &OAuthService{
		sessionStore: oauth.NewSessionStore(),
		proxyRepo:    proxyRepo,
		oauthClient:  oauthClient,
	}
}

// GenerateAuthURLResult contains the authorization URL and session info
type GenerateAuthURLResult struct {
	AuthURL   string `json:"auth_url"`
	SessionID string `json:"session_id"`
}

// GenerateAuthURL generates an OAuth authorization URL with full scope
func (s *OAuthService) GenerateAuthURL(ctx context.Context, proxyID *int64) (*GenerateAuthURLResult, error) {
	return s.generateAuthURLWithScope(ctx, oauth.ScopeOAuth, proxyID)
}

// GenerateSetupTokenURL generates an OAuth authorization URL for setup token (inference only)
func (s *OAuthService) GenerateSetupTokenURL(ctx context.Context, proxyID *int64) (*GenerateAuthURLResult, error) {
	scope := oauth.ScopeInference
	return s.generateAuthURLWithScope(ctx, scope, proxyID)
}

func (s *OAuthService) generateAuthURLWithScope(ctx context.Context, scope string, proxyID *int64) (*GenerateAuthURLResult, error) {
	// Generate PKCE values
	state, err := oauth.GenerateState()
	if err != nil {
		return nil, fmt.Errorf("failed to generate state: %w", err)
	}

	codeVerifier, err := oauth.GenerateCodeVerifier()
	if err != nil {
		return nil, fmt.Errorf("failed to generate code verifier: %w", err)
	}

	codeChallenge := oauth.GenerateCodeChallenge(codeVerifier)

	// Generate session ID
	sessionID, err := oauth.GenerateSessionID()
	if err != nil {
		return nil, fmt.Errorf("failed to generate session ID: %w", err)
	}

	// Get proxy URL if specified
	var proxyURL string
	if proxyID != nil {
		proxy, err := s.proxyRepo.GetByID(ctx, *proxyID)
		if err == nil && proxy != nil {
			proxyURL = proxy.URL()
		}
	}

	// Store session
	session := &oauth.OAuthSession{
		State:        state,
		CodeVerifier: codeVerifier,
		Scope:        scope,
		ProxyURL:     proxyURL,
		CreatedAt:    time.Now(),
	}
	s.sessionStore.Set(sessionID, session)

	// Build authorization URL
	authURL := oauth.BuildAuthorizationURL(state, codeChallenge, scope)

	return &GenerateAuthURLResult{
		AuthURL:   authURL,
		SessionID: sessionID,
	}, nil
}

// ExchangeCodeInput represents the input for code exchange
type ExchangeCodeInput struct {
	SessionID string
	Code      string
	ProxyID   *int64
}

// TokenInfo represents the token information stored in credentials
type TokenInfo struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	ExpiresAt    int64  `json:"expires_at"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
	OrgUUID      string `json:"org_uuid,omitempty"`
	AccountUUID  string `json:"account_uuid,omitempty"`
	EmailAddress string `json:"email_address,omitempty"`
	RateLimitTier string `json:"rate_limit_tier,omitempty"` // Anthropic 订阅等级，如 default_claude_max_20x
}

// ExchangeCode exchanges authorization code for tokens
func (s *OAuthService) ExchangeCode(ctx context.Context, input *ExchangeCodeInput) (*TokenInfo, error) {
	// Get session
	session, ok := s.sessionStore.Get(input.SessionID)
	if !ok {
		return nil, fmt.Errorf("session not found or expired")
	}

	// Get proxy URL
	proxyURL := session.ProxyURL
	if input.ProxyID != nil {
		proxy, err := s.proxyRepo.GetByID(ctx, *input.ProxyID)
		if err == nil && proxy != nil {
			proxyURL = proxy.URL()
		}
	}

	// Determine if this is a setup token (scope is inference only)
	isSetupToken := session.Scope == oauth.ScopeInference

	// Exchange code for token
	tokenInfo, err := s.exchangeCodeForToken(ctx, input.Code, session.CodeVerifier, session.State, proxyURL, isSetupToken)
	if err != nil {
		return nil, err
	}

	// Delete session after successful exchange
	s.sessionStore.Delete(input.SessionID)

	return tokenInfo, nil
}

// CookieAuthInput represents the input for cookie-based authentication
type CookieAuthInput struct {
	SessionKey string
	ProxyID    *int64
	Scope      string // "full" or "inference"
}

// CookieAuth performs OAuth using sessionKey (cookie-based auto-auth)
func (s *OAuthService) CookieAuth(ctx context.Context, input *CookieAuthInput) (*TokenInfo, error) {
	// Get proxy URL if specified
	var proxyURL string
	if input.ProxyID != nil {
		proxy, err := s.proxyRepo.GetByID(ctx, *input.ProxyID)
		if err == nil && proxy != nil {
			proxyURL = proxy.URL()
		}
	}

	// Determine scope and if this is a setup token
	// Internal API call uses ScopeAPI (org:create_api_key not supported)
	scope := oauth.ScopeAPI
	isSetupToken := false
	if input.Scope == "inference" {
		scope = oauth.ScopeInference
		isSetupToken = true
	}

	// Step 1: Get organization info using sessionKey
	orgUUID, err := s.getOrganizationUUID(ctx, input.SessionKey, proxyURL)
	if err != nil {
		return nil, fmt.Errorf("failed to get organization info: %w", err)
	}

	// Step 2: Generate PKCE values
	codeVerifier, err := oauth.GenerateCodeVerifier()
	if err != nil {
		return nil, fmt.Errorf("failed to generate code verifier: %w", err)
	}
	codeChallenge := oauth.GenerateCodeChallenge(codeVerifier)

	state, err := oauth.GenerateState()
	if err != nil {
		return nil, fmt.Errorf("failed to generate state: %w", err)
	}

	// Step 3: Get authorization code using cookie
	authCode, err := s.getAuthorizationCode(ctx, input.SessionKey, orgUUID, scope, codeChallenge, state, proxyURL)
	if err != nil {
		return nil, fmt.Errorf("failed to get authorization code: %w", err)
	}

	// Step 4: Exchange code for token
	tokenInfo, err := s.exchangeCodeForToken(ctx, authCode, codeVerifier, state, proxyURL, isSetupToken)
	if err != nil {
		return nil, fmt.Errorf("failed to exchange code: %w", err)
	}

	// Ensure org_uuid is set (from step 1 if not from token response)
	if tokenInfo.OrgUUID == "" && orgUUID != "" {
		tokenInfo.OrgUUID = orgUUID
		log.Printf("[OAuth] Set org_uuid from cookie auth")
	}

	return tokenInfo, nil
}

// getOrganizationUUID gets the organization UUID from claude.ai using sessionKey
func (s *OAuthService) getOrganizationUUID(ctx context.Context, sessionKey, proxyURL string) (string, error) {
	return s.oauthClient.GetOrganizationUUID(ctx, sessionKey, proxyURL)
}

// getAuthorizationCode gets the authorization code using sessionKey
func (s *OAuthService) getAuthorizationCode(ctx context.Context, sessionKey, orgUUID, scope, codeChallenge, state, proxyURL string) (string, error) {
	return s.oauthClient.GetAuthorizationCode(ctx, sessionKey, orgUUID, scope, codeChallenge, state, proxyURL)
}

// exchangeCodeForToken exchanges authorization code for tokens
func (s *OAuthService) exchangeCodeForToken(ctx context.Context, code, codeVerifier, state, proxyURL string, isSetupToken bool) (*TokenInfo, error) {
	tokenResp, err := s.oauthClient.ExchangeCodeForToken(ctx, code, codeVerifier, state, proxyURL, isSetupToken)
	if err != nil {
		return nil, err
	}

	tokenInfo := &TokenInfo{
		AccessToken:  tokenResp.AccessToken,
		TokenType:    tokenResp.TokenType,
		ExpiresIn:    tokenResp.ExpiresIn,
		ExpiresAt:    time.Now().Unix() + tokenResp.ExpiresIn,
		RefreshToken: tokenResp.RefreshToken,
		Scope:        tokenResp.Scope,
	}

	if tokenResp.Organization != nil && tokenResp.Organization.UUID != "" {
		tokenInfo.OrgUUID = tokenResp.Organization.UUID
		log.Printf("[OAuth] Got org_uuid")
	}
	if tokenResp.Account != nil {
		if tokenResp.Account.UUID != "" {
			tokenInfo.AccountUUID = tokenResp.Account.UUID
			log.Printf("[OAuth] Got account_uuid")
		}
		if tokenResp.Account.EmailAddress != "" {
			tokenInfo.EmailAddress = tokenResp.Account.EmailAddress
			log.Printf("[OAuth] Got email_address")
		}
	}

	// Best-effort: 拉取订阅等级（如 default_claude_max_20x）存入 credentials，
	// 供 Portal 展示。失败绝不阻断登录 —— 最坏结果只是没 tier。
	if tokenInfo.AccessToken != "" {
		if tier, terr := s.oauthClient.FetchRateLimitTier(ctx, tokenInfo.AccessToken, proxyURL); terr == nil && tier != "" {
			tokenInfo.RateLimitTier = tier
			log.Printf("[OAuth] Got rate_limit_tier")
		} else if terr != nil {
			log.Printf("[OAuth] rate_limit_tier fetch skipped: %v", terr)
		}
	}

	return tokenInfo, nil
}
func (s *OAuthService) RefreshToken(ctx context.Context, refreshToken string, proxyURL string) (*TokenInfo, error) {
	tokenResp, err := s.oauthClient.RefreshToken(ctx, refreshToken, proxyURL)
	if err != nil {
		return nil, err
	}

	return &TokenInfo{
		AccessToken:  tokenResp.AccessToken,
		TokenType:    tokenResp.TokenType,
		ExpiresIn:    tokenResp.ExpiresIn,
		ExpiresAt:    time.Now().Unix() + tokenResp.ExpiresIn,
		RefreshToken: tokenResp.RefreshToken,
		Scope:        tokenResp.Scope,
	}, nil
}

// RefreshAccountToken refreshes token for an account
func (s *OAuthService) RefreshAccountToken(ctx context.Context, account *Account) (*TokenInfo, error) {
	refreshToken := account.GetCredential("refresh_token")
	if refreshToken == "" {
		return nil, fmt.Errorf("no refresh token available")
	}

	var proxyURL string
	if account.ProxyID != nil {
		proxy, err := s.proxyRepo.GetByID(ctx, *account.ProxyID)
		if err == nil && proxy != nil {
			proxyURL = proxy.URL()
		}
	}

	return s.RefreshToken(ctx, refreshToken, proxyURL)
}

// BackfillClaudeTier 用账号已存的 access_token 拉取 Anthropic 订阅等级
// （如 default_claude_max_20x），供老账号在不重新登录的情况下补上 tier。
// 返回原始 tier 值；无 access_token 或拉取失败时返回错误。仅用于 anthropic 账号。
//
// NOTE(临时)：这是线上验收用的回填入口，验收确认 profile 字段结构后，连同
// FetchRateLimitTier 里的 temp debug 日志一并清理。
func (s *OAuthService) BackfillClaudeTier(ctx context.Context, account *Account) (string, error) {
	accessToken := account.GetCredential("access_token")
	if accessToken == "" {
		return "", fmt.Errorf("no access token available")
	}

	var proxyURL string
	if account.ProxyID != nil {
		proxy, err := s.proxyRepo.GetByID(ctx, *account.ProxyID)
		if err == nil && proxy != nil {
			proxyURL = proxy.URL()
		}
	}

	tier, err := s.oauthClient.FetchRateLimitTier(ctx, accessToken, proxyURL)
	if err != nil {
		return "", err
	}
	return tier, nil
}

// Stop stops the session store cleanup goroutine
func (s *OAuthService) Stop() {
	s.sessionStore.Stop()
}
