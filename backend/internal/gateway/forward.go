package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

const (
	defaultBaseURL       = "https://api.anthropic.com"
	defaultOAuthBaseURL  = "https://api.anthropic.com"
	httpTimeout          = 5 * time.Minute
	httpDialTimeout      = 30 * time.Second
	httpTLSTimeout       = 15 * time.Second
	httpIdleTimeout      = 90 * time.Second
	httpMaxIdleConns     = 100
	httpIdleConnsPerHost = 20
)

// ──────────────────────────────────────────────────────
// 转发入口
// ──────────────────────────────────────────────────────

// forwardHTTP 根据请求路径和账号类型分发
func (g *AnthropicGateway) forwardHTTP(ctx context.Context, req *sdk.ForwardRequest) (*sdk.ForwardResult, error) {
	path := resolveRequestPath(req)

	// /v1/models 路径：直接返回硬编码模型列表
	if path == "/v1/models" {
		return g.handleModelsRequest(req)
	}

	// /v1/messages/count_tokens 路径：使用专用的 count_tokens 转发逻辑
	if path == "/v1/messages/count_tokens" {
		return g.forwardCountTokens(ctx, req)
	}

	account := req.Account

	switch account.Type {
	case "apikey":
		return g.forwardAPIKey(ctx, req, path)
	case "oauth":
		return g.forwardOAuth(ctx, req, path)
	case "session_key":
		return g.forwardSessionKey(ctx, req, path)
	default:
		return nil, fmt.Errorf("未知的账号类型: %s", account.Type)
	}
}

// ──────────────────────────────────────────────────────
// API Key 模式：直接转发到上游
// ──────────────────────────────────────────────────────

func (g *AnthropicGateway) forwardAPIKey(ctx context.Context, req *sdk.ForwardRequest, path string) (*sdk.ForwardResult, error) {
	start := time.Now()
	account := req.Account

	// 构建目标 URL
	targetURL := buildBaseURL(account) + path

	// 预处理请求体：规范化模型 ID（不修改 metadata.user_id）
	body := preprocessBody(req.Body)

	// 构建 HTTP 请求
	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构建上游请求失败: %w", err)
	}

	// 设置认证与协议头
	setAnthropicAuthHeaders(upstreamReq, account, req.Headers, req.Model)

	// 发送请求
	client := buildHTTPClient(account)
	resp, err := client.Do(upstreamReq)
	if err != nil {
		return nil, fmt.Errorf("请求上游失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 上游返回错误
	if resp.StatusCode >= 400 {
		return handleErrorResponse(resp, req.Writer, start)
	}

	// 流式/非流式分发
	if req.Stream && req.Writer != nil {
		return handleStreamResponse(resp, req.Writer, start)
	}
	return handleNonStreamResponse(resp, req.Writer, start)
}

// ──────────────────────────────────────────────────────
// OAuth 模式：使用 OAuth token 转发
// ──────────────────────────────────────────────────────

func (g *AnthropicGateway) forwardOAuth(ctx context.Context, req *sdk.ForwardRequest, path string) (*sdk.ForwardResult, error) {
	start := time.Now()
	account := req.Account

	// 检查并自动刷新过期 token
	updatedCreds, err := g.ensureValidToken(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("token 刷新失败: %w", err)
	}

	if account.Credentials["access_token"] == "" {
		return nil, fmt.Errorf("OAuth 账号缺少 access_token")
	}

	targetURL := defaultOAuthBaseURL + path

	// 预处理请求体：规范化模型 ID（不修改 metadata.user_id）
	body := preprocessBody(req.Body)

	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构建上游请求失败: %w", err)
	}

	setAnthropicAuthHeaders(upstreamReq, account, req.Headers, req.Model)

	client := buildHTTPClient(account)
	resp, err := client.Do(upstreamReq)
	if err != nil {
		return nil, fmt.Errorf("请求上游失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		result, fwdErr := handleErrorResponse(resp, req.Writer, start)
		if result != nil && len(updatedCreds) > 0 {
			result.UpdatedCredentials = updatedCreds
		}
		return result, fwdErr
	}

	if req.Stream && req.Writer != nil {
		result, fwdErr := handleStreamResponse(resp, req.Writer, start)
		if result != nil && len(updatedCreds) > 0 {
			result.UpdatedCredentials = updatedCreds
		}
		return result, fwdErr
	}
	result, fwdErr := handleNonStreamResponse(resp, req.Writer, start)
	if result != nil && len(updatedCreds) > 0 {
		result.UpdatedCredentials = updatedCreds
	}
	return result, fwdErr
}

// ──────────────────────────────────────────────────────
// Session Key 模式：自动换 token 或刷新后走 OAuth 流程
// ──────────────────────────────────────────────────────

func (g *AnthropicGateway) forwardSessionKey(ctx context.Context, req *sdk.ForwardRequest, path string) (*sdk.ForwardResult, error) {
	account := req.Account

	// Session Key 账号没有 access_token 时，自动通过 Session Key 换取
	if account.Credentials["access_token"] == "" {
		sessionKey := account.Credentials["session_key"]
		if sessionKey == "" {
			return nil, fmt.Errorf("Session Key 账号缺少 session_key")
		}

		tokenResp, err := g.ExchangeSessionKeyForToken(ctx, sessionKey, account.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("Session Key 换取 token 失败: %w", err)
		}

		expiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)
		account.Credentials["access_token"] = tokenResp.AccessToken
		account.Credentials["refresh_token"] = tokenResp.RefreshToken
		account.Credentials["expires_at"] = expiresAt
	}

	// 复用 OAuth 转发逻辑
	return g.forwardOAuth(ctx, req, path)
}

// ──────────────────────────────────────────────────────
// Token 自动刷新
// ──────────────────────────────────────────────────────

const tokenRefreshSkew = 3 * time.Minute

// ensureValidToken 检查 token 过期状态，必要时自动刷新
// 返回更新后的凭证（用于回传 Core 持久化），如果没有刷新则为 nil
func (g *AnthropicGateway) ensureValidToken(ctx context.Context, account *sdk.Account) (map[string]string, error) {
	refreshToken := account.Credentials["refresh_token"]
	if refreshToken == "" {
		return nil, nil // 没有 refresh_token，无法刷新
	}

	expiresAtStr := account.Credentials["expires_at"]
	if expiresAtStr == "" {
		return nil, nil // 没有过期时间信息，假设有效
	}

	expiresAt, err := time.Parse(time.RFC3339, expiresAtStr)
	if err != nil {
		g.logger.Warn("解析 expires_at 失败", "expires_at", expiresAtStr, "error", err)
		return nil, nil // 解析失败，不阻断请求
	}

	// 提前 3 分钟刷新
	if time.Until(expiresAt) > tokenRefreshSkew {
		return nil, nil // 未过期，无需刷新
	}

	g.logger.Info("Token 即将过期，自动刷新", "account_id", account.ID, "expires_at", expiresAtStr)

	tokenResp, err := g.RefreshToken(ctx, refreshToken, account.ProxyURL)
	if err != nil {
		g.logger.Warn("Token 自动刷新失败，使用现有 token", "account_id", account.ID, "error", err)
		return nil, nil // 刷新失败，不阻断请求，尝试用现有 token
	}

	newExpiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)

	// 更新内存中的 credentials
	account.Credentials["access_token"] = tokenResp.AccessToken
	account.Credentials["expires_at"] = newExpiresAt
	if tokenResp.RefreshToken != "" {
		account.Credentials["refresh_token"] = tokenResp.RefreshToken
	}

	// 构建回传给 Core 的更新凭证
	updated := map[string]string{
		"access_token": tokenResp.AccessToken,
		"expires_at":   newExpiresAt,
	}
	if tokenResp.RefreshToken != "" {
		updated["refresh_token"] = tokenResp.RefreshToken
	}

	g.logger.Info("Token 自动刷新成功", "account_id", account.ID, "new_expires_at", newExpiresAt)
	return updated, nil
}

// ──────────────────────────────────────────────────────
// /v1/models 处理
// ──────────────────────────────────────────────────────

func (g *AnthropicGateway) handleModelsRequest(req *sdk.ForwardRequest) (*sdk.ForwardResult, error) {
	body := buildModelsResponse()

	if req.Writer != nil {
		req.Writer.Header().Set("Content-Type", "application/json")
		req.Writer.WriteHeader(http.StatusOK)
		_, _ = req.Writer.Write(body)
	}

	return &sdk.ForwardResult{
		StatusCode: http.StatusOK,
		Body:       body,
	}, nil
}

// ──────────────────────────────────────────────────────
// 工具函数
// ──────────────────────────────────────────────────────

// resolveRequestPath 从请求头中提取原始路径
func resolveRequestPath(req *sdk.ForwardRequest) string {
	// Core 透传请求时可能在 X-Original-Path 头中保留原始路径
	if path := req.Headers.Get("X-Original-Path"); path != "" {
		return path
	}
	// 回退：根据请求体检测
	if len(req.Body) > 0 {
		// 有 body 的 POST 请求默认走 /v1/messages
		return "/v1/messages"
	}
	return "/v1/models"
}

// buildBaseURL 构建上游 API 基础 URL
func buildBaseURL(account *sdk.Account) string {
	baseURL := strings.TrimSpace(account.Credentials["base_url"])
	if baseURL == "" {
		return defaultBaseURL
	}
	return strings.TrimSuffix(baseURL, "/")
}

// preprocessBody 预处理请求体：仅规范化模型 ID
// 不修改 metadata.user_id，保持原始请求体发送给上游
func preprocessBody(body []byte) []byte {
	return normalizeRequestBody(body)
}

// normalizeRequestBody 预处理请求体，规范化模型 ID
func normalizeRequestBody(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	modelID := gjson.GetBytes(body, "model").String()
	if modelID == "" {
		return body
	}

	normalized := NormalizeModelID(modelID)
	if normalized == modelID {
		return body
	}

	// 替换 model 字段（简单字符串替换，避免 sjson 依赖）
	return bytes.Replace(body, []byte(`"model":"`+modelID+`"`), []byte(`"model":"`+normalized+`"`), 1)
}

// handleErrorResponse 处理上游错误响应，同时将错误响应写入客户端 Writer
// 4xx 返回 (result, nil) 让 Core 透传；5xx 返回 (result, error) 让 Core 报告失败
func handleErrorResponse(resp *http.Response, w http.ResponseWriter, start time.Time) (*sdk.ForwardResult, error) {
	respBody, _ := io.ReadAll(resp.Body)

	// 将上游错误响应透传给客户端
	if w != nil {
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
	}

	result := &sdk.ForwardResult{
		StatusCode:    resp.StatusCode,
		Duration:      time.Since(start),
		AccountStatus: accountStatusFromCode(resp.StatusCode),
		RetryAfter:    extractRetryAfterHeader(resp.Header),
		Body:          respBody,
		Headers:       resp.Header,
	}

	// 4xx：上游明确拒绝，Core 应透传给客户端而非返回通用 502
	if resp.StatusCode < 500 {
		return result, nil
	}

	// 5xx：上游服务异常
	return result, fmt.Errorf("上游返回 %d: %s", resp.StatusCode, truncate(string(respBody), 200))
}

// buildHTTPClient 构建 HTTP 客户端（支持代理和 TLS 指纹）
// OAuth/session_key 账号使用 TLS 指纹模拟 Claude CLI
// API Key 账号使用标准 TLS
func buildHTTPClient(account *sdk.Account) *http.Client {
	// OAuth/session_key 使用 TLS 指纹
	if account.Type == "oauth" || account.Type == "session_key" {
		transport := buildFingerprintTransport(account.ProxyURL)
		return &http.Client{
			Timeout:   httpTimeout,
			Transport: transport,
		}
	}

	// API Key 使用标准 TLS
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   httpDialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: httpTLSTimeout,
		MaxIdleConns:        httpMaxIdleConns,
		MaxIdleConnsPerHost: httpIdleConnsPerHost,
		IdleConnTimeout:     httpIdleTimeout,
		ForceAttemptHTTP2:   true,
	}

	if account.ProxyURL != "" {
		if proxyURL, err := url.Parse(account.ProxyURL); err == nil {
			transport.Proxy = http.ProxyURL(proxyURL)
		}
	}

	return &http.Client{
		Timeout:   httpTimeout,
		Transport: transport,
	}
}
