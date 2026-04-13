package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/imroc/req/v3"
)

// ──────────────────────────────────────────────────────
// OAuth 常量（来自 Anthropic/Claude 官方 OAuth 配置）
// ──────────────────────────────────────────────────────

const (
	OAuthClientID     = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	OAuthAuthorizeURL = "https://claude.ai/oauth/authorize"
	OAuthTokenURL     = "https://platform.claude.com/v1/oauth/token"
	OAuthRedirectURI  = "https://platform.claude.com/oauth/code/callback"
	OAuthScopeBrowser   = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	OAuthScopeAPI       = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"

	// Session Key 通过 claude.ai 获取 org 和 authorization code 的端点
	claudeAIBaseURL = "https://claude.ai"

	// PKCE 字符集
	codeVerifierCharset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"

	// OAuth Session TTL
	oauthSessionTTL = 30 * time.Minute
)

// ──────────────────────────────────────────────────────
// OAuth Session 管理
// ──────────────────────────────────────────────────────

// OAuthSession 存储 OAuth 流程状态
type OAuthSession struct {
	State        string
	CodeVerifier string
	CreatedAt    time.Time
}

// oauthSessionStore OAuth 会话存储
type oauthSessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*OAuthSession
}

var sessionStore = &oauthSessionStore{
	sessions: make(map[string]*OAuthSession),
}

func (s *oauthSessionStore) Set(state string, session *OAuthSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[state] = session
}

func (s *oauthSessionStore) Get(state string) (*OAuthSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, ok := s.sessions[state]
	if !ok {
		return nil, false
	}
	if time.Since(session.CreatedAt) > oauthSessionTTL {
		return nil, false
	}
	return session, true
}

func (s *oauthSessionStore) Delete(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, state)
}

// ──────────────────────────────────────────────────────
// 浏览器 OAuth PKCE 流程（用户手动授权）
// ──────────────────────────────────────────────────────

// OAuthStartResponse OAuth 授权发起响应
type OAuthStartResponse struct {
	AuthorizeURL string `json:"authorize_url"`
	State        string `json:"state"`
}

// StartOAuth 发起 OAuth 授权，生成 PKCE 参数和授权 URL
func (g *AnthropicGateway) StartOAuth() (*OAuthStartResponse, error) {
	// 清理过期会话
	sessionStore.CleanExpired()

	// 生成 PKCE 参数
	codeVerifier, err := generateCodeVerifier()
	if err != nil {
		return nil, fmt.Errorf("生成 code_verifier 失败: %w", err)
	}
	codeChallenge := generateCodeChallenge(codeVerifier)

	state, err := generateState()
	if err != nil {
		return nil, fmt.Errorf("生成 state 失败: %w", err)
	}

	// 保存会话
	sessionStore.Set(state, &OAuthSession{
		State:        state,
		CodeVerifier: codeVerifier,
		CreatedAt:    time.Now(),
	})

	// 构建授权 URL
	q := url.Values{}
	q.Set("code", "true")
	q.Set("client_id", OAuthClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", OAuthRedirectURI)
	q.Set("scope", OAuthScopeBrowser)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	authorizeURL := OAuthAuthorizeURL + "?" + q.Encode()

	g.logger.Info("OAuth 授权发起", "authorize_url", authorizeURL)

	return &OAuthStartResponse{
		AuthorizeURL: authorizeURL,
		State:        state,
	}, nil
}

// HandleOAuthCallback 处理 OAuth 回调，用 code+state 交换 token
func (g *AnthropicGateway) HandleOAuthCallback(ctx context.Context, code, state, proxyURL string) (*TokenResponse, error) {
	session, ok := sessionStore.Get(state)
	if !ok {
		return nil, fmt.Errorf("无效或已过期的 OAuth 会话")
	}
	sessionStore.Delete(state)

	if time.Since(session.CreatedAt) > oauthSessionTTL {
		return nil, fmt.Errorf("OAuth 会话已过期")
	}

	client := g.buildOAuthClient(proxyURL)

	// 直接用 code + 保存的 code_verifier 换 token
	tokenResp, err := g.exchangeCodeForToken(ctx, client, code, session.CodeVerifier, state)
	if err != nil {
		return nil, fmt.Errorf("token 交换失败: %w", err)
	}
	return tokenResp, nil
}

// CleanExpired 清理过期会话
func (s *oauthSessionStore) CleanExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for state, session := range s.sessions {
		if time.Since(session.CreatedAt) > oauthSessionTTL {
			delete(s.sessions, state)
		}
	}
}

// ──────────────────────────────────────────────────────
// Session Key → OAuth Token 完整流程
// ──────────────────────────────────────────────────────

// TokenResponse OAuth token 响应
type TokenResponse struct {
	AccessToken  string       `json:"access_token"`
	TokenType    string       `json:"token_type"`
	ExpiresIn    int64        `json:"expires_in"`
	RefreshToken string       `json:"refresh_token,omitempty"`
	Scope        string       `json:"scope,omitempty"`
	Organization *OrgInfo     `json:"organization,omitempty"`
	Account      *AccountInfo `json:"account,omitempty"`
}

// OrgInfo 组织信息
type OrgInfo struct {
	UUID string `json:"uuid"`
}

// AccountInfo 账户信息
type AccountInfo struct {
	UUID         string `json:"uuid"`
	EmailAddress string `json:"email_address"`
}

// ExchangeSessionKeyForToken 通过 Session Key 获取 OAuth Token（完整 scope）
// 完整流程：
//  1. GET claude.ai/api/organizations → 获取 org UUID
//  2. POST claude.ai/v1/oauth/{orgUUID}/authorize → 获取 authorization code
//  3. POST platform.claude.com/v1/oauth/token → 用 code 换 access_token
func (g *AnthropicGateway) ExchangeSessionKeyForToken(ctx context.Context, sessionKey, proxyURL string) (*TokenResponse, error) {
	return g.exchangeSessionKeyWithScope(ctx, sessionKey, proxyURL, OAuthScopeAPI)
}

// buildOAuthReqClient 构建用于 claude.ai 请求的 req 客户端（Chrome TLS 指纹 + 绕过 Cloudflare）
func buildOAuthReqClient(proxyURL string) *req.Client {
	client := req.C().
		SetTimeout(60 * time.Second).
		ImpersonateChrome().
		SetCookieJar(nil) // 禁用自动 cookie 管理，每次请求手动设置
	if proxyURL != "" {
		client.SetProxyURL(proxyURL)
	}
	return client
}

// buildOAuthClient 构建用于 token 交换的标准 HTTP 客户端（platform.claude.com 无 Cloudflare）
func (g *AnthropicGateway) buildOAuthClient(proxyURL string) *http.Client {
	transport := buildFingerprintTransport(proxyURL)
	return &http.Client{
		Timeout:   60 * time.Second,
		Transport: transport,
	}
}

// exchangeSessionKeyWithScope 通用的 Session Key → OAuth Token 流程
func (g *AnthropicGateway) exchangeSessionKeyWithScope(ctx context.Context, sessionKey, proxyURL, scope string) (*TokenResponse, error) {
	// claude.ai 请求使用 Chrome 指纹客户端（绕过 Cloudflare）
	reqClient := buildOAuthReqClient(proxyURL)

	// Step 1: 获取组织 UUID
	orgUUID, err := g.getOrganizationUUID(ctx, reqClient, sessionKey)
	if err != nil {
		return nil, fmt.Errorf("获取组织 UUID 失败: %w", err)
	}

	// 生成 PKCE 参数
	codeVerifier, err := generateCodeVerifier()
	if err != nil {
		return nil, fmt.Errorf("生成 code_verifier 失败: %w", err)
	}
	codeChallenge := generateCodeChallenge(codeVerifier)

	state, err := generateState()
	if err != nil {
		return nil, fmt.Errorf("生成 state 失败: %w", err)
	}

	// Step 2: 获取 authorization code（使用指定 scope）
	authCode, err := g.getAuthorizationCodeWithScope(ctx, reqClient, sessionKey, orgUUID, codeChallenge, state, scope)
	if err != nil {
		return nil, fmt.Errorf("获取授权码失败: %w", err)
	}

	// Step 3: 用 code 换 token（platform.claude.com 无 Cloudflare，用标准 HTTP 客户端）
	httpClient := g.buildOAuthClient(proxyURL)
	tokenResp, err := g.exchangeCodeForToken(ctx, httpClient, authCode, codeVerifier, state)
	if err != nil {
		return nil, fmt.Errorf("换取 token 失败: %w", err)
	}

	return tokenResp, nil
}

// getOrganizationUUID 获取 Claude 组织 UUID（使用 req/v3 Chrome 指纹绕过 Cloudflare）
func (g *AnthropicGateway) getOrganizationUUID(ctx context.Context, client *req.Client, sessionKey string) (string, error) {
	var orgs []struct {
		UUID      string  `json:"uuid"`
		Name      string  `json:"name"`
		RavenType *string `json:"raven_type"`
	}

	resp, err := client.R().
		SetContext(ctx).
		SetCookies(&http.Cookie{Name: "sessionKey", Value: sessionKey}).
		SetHeader("Accept", "application/json").
		SetHeader("Accept-Language", "en-US,en;q=0.9").
		SetHeader("Cache-Control", "no-cache").
		SetSuccessResult(&orgs).
		Get(claudeAIBaseURL + "/api/organizations")
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(resp.String(), 200))
	}

	if len(orgs) == 0 {
		return "", fmt.Errorf("未找到组织")
	}

	// 优先选择 team 类型的组织
	for _, org := range orgs {
		if org.RavenType != nil && *org.RavenType == "team" {
			return org.UUID, nil
		}
	}
	return orgs[0].UUID, nil
}

// getAuthorizationCode 获取 OAuth 授权码（默认 API scope）
func (g *AnthropicGateway) getAuthorizationCode(ctx context.Context, client *req.Client, sessionKey, orgUUID, codeChallenge, state string) (string, error) {
	return g.getAuthorizationCodeWithScope(ctx, client, sessionKey, orgUUID, codeChallenge, state, OAuthScopeAPI)
}

// getAuthorizationCodeWithScope 获取 OAuth 授权码（使用 req/v3 Chrome 指纹）
func (g *AnthropicGateway) getAuthorizationCodeWithScope(ctx context.Context, client *req.Client, sessionKey, orgUUID, codeChallenge, state, scope string) (string, error) {
	authURL := fmt.Sprintf("%s/v1/oauth/%s/authorize", claudeAIBaseURL, orgUUID)

	reqBody := map[string]any{
		"response_type":         "code",
		"client_id":             OAuthClientID,
		"organization_uuid":     orgUUID,
		"redirect_uri":          OAuthRedirectURI,
		"scope":                 scope,
		"state":                 state,
		"code_challenge":        codeChallenge,
		"code_challenge_method": "S256",
	}

	var result struct {
		RedirectURI string `json:"redirect_uri"`
	}

	resp, err := client.R().
		SetContext(ctx).
		SetCookies(&http.Cookie{Name: "sessionKey", Value: sessionKey}).
		SetHeader("Accept", "application/json").
		SetHeader("Accept-Language", "en-US,en;q=0.9").
		SetHeader("Cache-Control", "no-cache").
		SetHeader("Origin", "https://claude.ai").
		SetHeader("Referer", "https://claude.ai/new").
		SetBody(reqBody).
		SetSuccessResult(&result).
		Post(authURL)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(resp.String(), 200))
	}

	if result.RedirectURI == "" {
		return "", fmt.Errorf("响应中缺少 redirect_uri")
	}

	// 从 redirect_uri 中提取 code 和 state
	parsedURL, err := url.Parse(result.RedirectURI)
	if err != nil {
		return "", fmt.Errorf("解析 redirect_uri 失败: %w", err)
	}

	authCode := parsedURL.Query().Get("code")
	responseState := parsedURL.Query().Get("state")
	if authCode == "" {
		return "", fmt.Errorf("redirect_uri 中缺少 code")
	}

	// 组合 code 和 state
	fullCode := authCode
	if responseState != "" {
		fullCode = authCode + "#" + responseState
	}
	return fullCode, nil
}

// exchangeCodeForToken 用授权码换取 token
func (g *AnthropicGateway) exchangeCodeForToken(ctx context.Context, client *http.Client, code, codeVerifier, state string) (*TokenResponse, error) {
	// 解析 code（可能包含 state: "authCode#state"）
	authCode := code
	codeState := ""
	if idx := strings.Index(code, "#"); idx != -1 {
		authCode = code[:idx]
		codeState = code[idx+1:]
	}

	reqBody := map[string]any{
		"code":          authCode,
		"grant_type":    "authorization_code",
		"client_id":     OAuthClientID,
		"redirect_uri":  OAuthRedirectURI,
		"code_verifier": codeVerifier,
	}
	if codeState != "" {
		reqBody["state"] = codeState
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, OAuthTokenURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "axios/1.8.4")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var tokenResp TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("解析 token 响应失败: %w", err)
	}
	return &tokenResp, nil
}

// RefreshToken 刷新 OAuth token
func (g *AnthropicGateway) RefreshToken(ctx context.Context, refreshToken, proxyURL string) (*TokenResponse, error) {
	client := g.buildOAuthClient(proxyURL)

	reqBody := map[string]any{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     OAuthClientID,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, OAuthTokenURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "axios/1.8.4")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var tokenResp TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("解析 token 响应失败: %w", err)
	}
	return &tokenResp, nil
}

// ──────────────────────────────────────────────────────
// PKCE 辅助函数
// ──────────────────────────────────────────────────────

func generateState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64URLEncode(b), nil
}

func generateCodeVerifier() (string, error) {
	const targetLen = 32
	charsetLen := len(codeVerifierCharset)
	limit := 256 - (256 % charsetLen)

	result := make([]byte, 0, targetLen)
	randBuf := make([]byte, targetLen*2)

	for len(result) < targetLen {
		if _, err := rand.Read(randBuf); err != nil {
			return "", err
		}
		for _, b := range randBuf {
			if int(b) < limit {
				result = append(result, codeVerifierCharset[int(b)%charsetLen])
				if len(result) >= targetLen {
					break
				}
			}
		}
	}

	return base64URLEncode(result), nil
}

func generateCodeChallenge(verifier string) string {
	hash := sha256.Sum256([]byte(verifier))
	return base64URLEncode(hash[:])
}

func base64URLEncode(data []byte) string {
	encoded := base64.URLEncoding.EncodeToString(data)
	return strings.TrimRight(encoded, "=")
}

func generateSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
