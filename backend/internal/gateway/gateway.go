package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// usageCacheEntry 用量缓存条目
type usageCacheEntry struct {
	data      *UsageResponse
	fetchedAt time.Time
}

// AnthropicGateway Claude 网关插件
type AnthropicGateway struct {
	logger     *slog.Logger
	ctx        sdk.PluginContext
	tokenMgr   *tokenManager
	stdPool    *StandardTransportPool
	fpPool     *FingerprintTransportPool
	usageCache sync.Map // accountID (string) -> *usageCacheEntry
}

func (g *AnthropicGateway) Info() sdk.PluginInfo {
	return BuildPluginInfo()
}

func (g *AnthropicGateway) Init(ctx sdk.PluginContext) error {
	g.ctx = ctx
	if ctx != nil {
		g.logger = ctx.Logger()
	}
	if g.logger == nil {
		g.logger = slog.Default()
	}

	// 初始化连接池
	g.stdPool = NewStandardTransportPool()
	g.fpPool = NewFingerprintTransportPool()

	// 初始化 token 刷新管理器
	g.tokenMgr = newTokenManager(g, g.logger)

	g.logger.Info("Claude 网关插件初始化")
	return nil
}

func (g *AnthropicGateway) Start(_ context.Context) error {
	g.logger.Info("Claude 网关插件启动", "pool_stats", poolStats(g.stdPool, g.fpPool))
	return nil
}

func (g *AnthropicGateway) Stop(_ context.Context) error {
	g.logger.Info("Claude 网关插件停止")
	if g.stdPool != nil {
		g.stdPool.Close()
	}
	if g.fpPool != nil {
		g.fpPool.Close()
	}
	return nil
}

func (g *AnthropicGateway) Platform() string {
	return PluginPlatform
}

func (g *AnthropicGateway) Models() []sdk.ModelInfo {
	return AllModelSpecs()
}

func (g *AnthropicGateway) Routes() []sdk.RouteDefinition {
	return PluginRouteDefinitions()
}

func (g *AnthropicGateway) Forward(ctx context.Context, req *sdk.ForwardRequest) (*sdk.ForwardResult, error) {
	return g.forwardHTTP(ctx, req)
}

// HandleWebSocket 不支持 WebSocket
func (g *AnthropicGateway) HandleWebSocket(_ context.Context, _ sdk.WebSocketConn) (*sdk.ForwardResult, error) {
	return nil, sdk.ErrNotSupported
}

// ValidateAccount 验证凭证有效性
func (g *AnthropicGateway) ValidateAccount(ctx context.Context, credentials map[string]string) error {
	apiKey := credentials["api_key"]
	accessToken := credentials["access_token"]
	sessionKey := credentials["session_key"]

	if apiKey == "" && accessToken == "" && sessionKey == "" {
		return fmt.Errorf("缺少 api_key、access_token 或 session_key")
	}

	// API Key 模式：调用 /v1/models 验证
	if apiKey != "" {
		baseURL := resolveBaseURL(credentials)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/models", nil)
		if err != nil {
			return fmt.Errorf("构建验证请求失败: %w", err)
		}
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", DefaultAnthropicVersion)

		client := &http.Client{Timeout: 30 * time.Second}
		if proxyURL := credentials["proxy_url"]; proxyURL != "" {
			if u, err := url.Parse(proxyURL); err == nil {
				client.Transport = &http.Transport{Proxy: http.ProxyURL(u)}
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("验证请求失败: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode == 401 {
			return fmt.Errorf("API Key 无效")
		}
		if resp.StatusCode >= 400 {
			return fmt.Errorf("API Key 验证失败: HTTP %d", resp.StatusCode)
		}
		return nil
	}

	// OAuth / Session Key 模式：有 access_token 或 session_key 即通过
	return nil
}

// QueryQuota 查询账号额度
func (g *AnthropicGateway) QueryQuota(ctx context.Context, credentials map[string]string) (*sdk.QuotaInfo, error) {
	accessToken := credentials["access_token"]
	if accessToken == "" {
		return nil, sdk.ErrNotSupported
	}

	// 调用 Anthropic usage API
	usageResp, err := g.fetchUsage(ctx, accessToken, credentials["proxy_url"])
	if err != nil {
		return nil, fmt.Errorf("查询用量失败: %w", err)
	}

	extra := map[string]string{}
	if usageResp != nil {
		extra["five_hour_utilization"] = fmt.Sprintf("%.2f", usageResp.FiveHour.Utilization)
		extra["seven_day_utilization"] = fmt.Sprintf("%.2f", usageResp.SevenDay.Utilization)
		extra["seven_day_sonnet_utilization"] = fmt.Sprintf("%.2f", usageResp.SevenDaySonnet.Utilization)
		if usageResp.FiveHour.ResetsAt != "" {
			extra["five_hour_resets_at"] = usageResp.FiveHour.ResetsAt
		}
		if usageResp.SevenDay.ResetsAt != "" {
			extra["seven_day_resets_at"] = usageResp.SevenDay.ResetsAt
		}
		if usageResp.SevenDaySonnet.ResetsAt != "" {
			extra["seven_day_sonnet_resets_at"] = usageResp.SevenDaySonnet.ResetsAt
		}
	}

	return &sdk.QuotaInfo{
		Extra: extra,
	}, nil
}

// HandleRequest 处理 Core 透传的自定义请求
func (g *AnthropicGateway) HandleRequest(ctx context.Context, _, path, _ string, _ http.Header, body []byte) (int, http.Header, []byte, error) {
	switch path {
	case "oauth/start":
		// 生成 OAuth 授权链接（浏览器 PKCE 流程）
		resp, err := g.StartOAuth()
		if err != nil {
			return http.StatusInternalServerError, nil, jsonError(err.Error()), nil
		}
		return http.StatusOK, nil, jsonMarshal(map[string]string{
			"authorize_url": resp.AuthorizeURL,
			"state":         resp.State,
		}), nil

	case "oauth/exchange":
		// 支持三种模式：
		// 1. callback_url 模式（OAuth 浏览器授权回调）
		// 2. session_key 模式（Session Key 自动换 Token）
		// 3. session_key + scope=inference 模式（Setup Token）
		var raw struct {
			CallbackURL string `json:"callback_url"`
			SessionKey  string `json:"session_key"`
			ProxyURL    string `json:"proxy_url"`
			Scope       string `json:"scope"` // "full" 或 "inference"，默认 "full"
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return http.StatusBadRequest, nil, jsonError("请求参数格式错误"), nil
		}

		// Core 的 oauth.exchange(str) 统一把参数放进 callback_url 字段。
		// 前端 Session Key 模式传的是 JSON（如 {"session_key":"..."}），需要检测并提取。
		if raw.CallbackURL != "" && raw.SessionKey == "" && strings.HasPrefix(strings.TrimSpace(raw.CallbackURL), "{") {
			var inner struct {
				SessionKey string `json:"session_key"`
				Scope      string `json:"scope"`
			}
			if json.Unmarshal([]byte(raw.CallbackURL), &inner) == nil && inner.SessionKey != "" {
				raw.SessionKey = inner.SessionKey
				if inner.Scope != "" {
					raw.Scope = inner.Scope
				}
				raw.CallbackURL = "" // 清空，走 session_key 分支
			}
		}

		var tokenResp *TokenResponse
		var err error
		accountType := "oauth"

		if raw.CallbackURL != "" {
			// 浏览器 OAuth 回调模式：从 callback_url 中提取 code 和 state
			parsed, parseErr := url.Parse(raw.CallbackURL)
			if parseErr != nil {
				return http.StatusBadRequest, nil, jsonError("callback_url 格式无效"), nil
			}
			code := parsed.Query().Get("code")
			state := parsed.Query().Get("state")
			if code == "" || state == "" {
				return http.StatusBadRequest, nil, jsonError("callback_url 缺少 code 或 state 参数"), nil
			}
			tokenResp, err = g.HandleOAuthCallback(ctx, code, state, raw.ProxyURL)
		} else if raw.SessionKey != "" {
			if raw.Scope == "inference" {
				// Setup Token 模式
				tokenResp, err = g.ExchangeSessionKeyForSetupToken(ctx, raw.SessionKey, raw.ProxyURL)
				accountType = "setup_token"
			} else {
				// 完整 OAuth 模式
				tokenResp, err = g.ExchangeSessionKeyForToken(ctx, raw.SessionKey, raw.ProxyURL)
			}
		} else {
			return http.StatusBadRequest, nil, jsonError("缺少 callback_url 或 session_key 参数"), nil
		}

		if err != nil {
			return http.StatusInternalServerError, nil, jsonError(err.Error()), nil
		}

		expiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)

		credentials := map[string]string{
			"access_token":  tokenResp.AccessToken,
			"refresh_token": tokenResp.RefreshToken,
			"expires_at":    expiresAt,
		}

		var accountName string
		if tokenResp.Account != nil {
			accountName = tokenResp.Account.EmailAddress
			credentials["email"] = tokenResp.Account.EmailAddress
			credentials["account_uuid"] = tokenResp.Account.UUID
		}
		if tokenResp.Organization != nil {
			credentials["org_uuid"] = tokenResp.Organization.UUID
		}

		return http.StatusOK, nil, jsonMarshal(map[string]any{
			"account_type": accountType,
			"credentials":  credentials,
			"account_name": accountName,
		}), nil

	case "oauth/refresh":
		// 刷新 OAuth Token
		var raw struct {
			RefreshToken string `json:"refresh_token"`
			ProxyURL     string `json:"proxy_url"`
		}
		if err := json.Unmarshal(body, &raw); err != nil || raw.RefreshToken == "" {
			return http.StatusBadRequest, nil, jsonError("缺少 refresh_token 参数"), nil
		}

		tokenResp, err := g.RefreshToken(ctx, raw.RefreshToken, raw.ProxyURL)
		if err != nil {
			return http.StatusInternalServerError, nil, jsonError(err.Error()), nil
		}

		expiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)

		return http.StatusOK, nil, jsonMarshal(map[string]string{
			"access_token":  tokenResp.AccessToken,
			"refresh_token": tokenResp.RefreshToken,
			"expires_at":    expiresAt,
		}), nil

	case "usage/accounts":
		// 查询多个账号的用量（使用 SDK 标准 AccountUsageAccountsResponse 格式）
		var accounts []struct {
			ID          int64             `json:"id"`
			Credentials map[string]string `json:"credentials"`
		}
		if err := json.Unmarshal(body, &accounts); err != nil {
			return http.StatusBadRequest, nil, jsonError("invalid request body"), nil
		}

		now := time.Now()
		resp := sdk.AccountUsageAccountsResponse{
			Accounts: make(map[string]sdk.AccountUsageInfo),
		}

		for _, a := range accounts {
			accessToken := a.Credentials["access_token"]
			if accessToken == "" {
				continue
			}

			usageResp, err := g.fetchUsageWithCache(ctx, strconv.FormatInt(a.ID, 10), accessToken, a.Credentials["proxy_url"])
			if err != nil {
				resp.Errors = append(resp.Errors, sdk.AccountUsageError{
					ID:      a.ID,
					Message: err.Error(),
				})
				continue
			}
			if usageResp == nil {
				continue
			}

			info := sdk.AccountUsageInfo{
				UpdatedAt: now.UTC().Format(time.RFC3339),
			}

			// 5h 窗口（Current session）
			appendUsageWindow := func(key, label string, utilization float64, resetsAt string) {
				if resetsAt != "" {
					if resetAt, err := time.Parse(time.RFC3339, resetsAt); err == nil {
						info.Windows = append(info.Windows, sdk.NewAccountUsageWindow(key, label, utilization, &resetAt, now))
						return
					}
				}
				info.Windows = append(info.Windows, sdk.NewAccountUsageWindow(key, label, utilization, nil, now))
			}

			appendUsageWindow("5h", "5h", usageResp.FiveHour.Utilization, usageResp.FiveHour.ResetsAt)

			// 7d 窗口（全部模型）
			appendUsageWindow("7d", "7d", usageResp.SevenDay.Utilization, usageResp.SevenDay.ResetsAt)

			// 7d Sonnet 窗口（仅当 API 返回了该窗口数据）
			if usageResp.SevenDaySonnet.ResetsAt != "" || usageResp.SevenDaySonnet.Utilization > 0 {
				appendUsageWindow("7d_sonnet", "7d Sonnet", usageResp.SevenDaySonnet.Utilization, usageResp.SevenDaySonnet.ResetsAt)
			}

			resp.Accounts[strconv.FormatInt(a.ID, 10)] = info
		}

		return http.StatusOK, nil, jsonMarshal(resp), nil

	case "console/cookie-auth":
		// Cookie Auth：通过 session_key 一键创建 OAuth 账号
		var raw struct {
			SessionKey string `json:"session_key"`
			ProxyURL   string `json:"proxy_url"`
			Scope      string `json:"scope"` // "full" 或 "inference"，默认 "full"
		}
		if err := json.Unmarshal(body, &raw); err != nil || raw.SessionKey == "" {
			return http.StatusBadRequest, nil, jsonError("缺少 session_key 参数"), nil
		}

		var tokenResp *TokenResponse
		var err error
		accountType := "oauth"

		if raw.Scope == "inference" {
			tokenResp, err = g.ExchangeSessionKeyForSetupToken(ctx, raw.SessionKey, raw.ProxyURL)
			accountType = "setup_token"
		} else {
			tokenResp, err = g.ExchangeSessionKeyForToken(ctx, raw.SessionKey, raw.ProxyURL)
		}
		if err != nil {
			return http.StatusInternalServerError, nil, jsonError(err.Error()), nil
		}

		expiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)
		credentials := map[string]string{
			"access_token":  tokenResp.AccessToken,
			"refresh_token": tokenResp.RefreshToken,
			"expires_at":    expiresAt,
		}

		var accountName string
		if tokenResp.Account != nil {
			accountName = tokenResp.Account.EmailAddress
			credentials["email"] = tokenResp.Account.EmailAddress
			credentials["account_uuid"] = tokenResp.Account.UUID
		}
		if tokenResp.Organization != nil {
			credentials["org_uuid"] = tokenResp.Organization.UUID
		}

		return http.StatusOK, nil, jsonMarshal(map[string]any{
			"account_type": accountType,
			"credentials":  credentials,
			"account_name": accountName,
		}), nil

	case "console/batch-cookie-auth":
		// 批量 Cookie Auth
		var raw struct {
			SessionKeys []string `json:"session_keys"`
			ProxyURL    string   `json:"proxy_url"`
			Scope       string   `json:"scope"`
		}
		if err := json.Unmarshal(body, &raw); err != nil || len(raw.SessionKeys) == 0 {
			return http.StatusBadRequest, nil, jsonError("缺少 session_keys 参数"), nil
		}

		type batchResult struct {
			Email       string `json:"email,omitempty"`
			AccountUUID string `json:"account_uuid,omitempty"`
			AccountType string `json:"account_type,omitempty"`
			Status      string `json:"status"`
			Error       string `json:"error,omitempty"`
		}

		results := make([]batchResult, 0, len(raw.SessionKeys))
		for _, sk := range raw.SessionKeys {
			var tokenResp *TokenResponse
			var err error
			acctType := "oauth"

			if raw.Scope == "inference" {
				tokenResp, err = g.ExchangeSessionKeyForSetupToken(ctx, sk, raw.ProxyURL)
				acctType = "setup_token"
			} else {
				tokenResp, err = g.ExchangeSessionKeyForToken(ctx, sk, raw.ProxyURL)
			}

			if err != nil {
				results = append(results, batchResult{Status: "failed", Error: err.Error()})
				continue
			}

			r := batchResult{Status: "ok", AccountType: acctType}
			if tokenResp.Account != nil {
				r.Email = tokenResp.Account.EmailAddress
				r.AccountUUID = tokenResp.Account.UUID
			}
			results = append(results, r)
		}

		return http.StatusOK, nil, jsonMarshal(map[string]any{"results": results}), nil

	default:
		return http.StatusNotFound, nil, jsonError("未知的操作: "+path), nil
	}
}

// ──────────────────────────────────────────────────────
// Usage API
// ──────────────────────────────────────────────────────

const usageAPIURL = "https://api.anthropic.com/api/oauth/usage"

// UsageResponse Anthropic API 返回的 usage 结构
type UsageResponse struct {
	FiveHour struct {
		Utilization float64 `json:"utilization"`
		ResetsAt    string  `json:"resets_at"`
	} `json:"five_hour"`
	SevenDay struct {
		Utilization float64 `json:"utilization"`
		ResetsAt    string  `json:"resets_at"`
	} `json:"seven_day"`
	SevenDaySonnet struct {
		Utilization float64 `json:"utilization"`
		ResetsAt    string  `json:"resets_at"`
	} `json:"seven_day_sonnet"`
}

const usageCacheTTL = 3 * time.Minute // 用量缓存 3 分钟（参考 sub2api）

// fetchUsageWithCache 带缓存的用量查询，同一 token 3 分钟内不重复请求
func (g *AnthropicGateway) fetchUsageWithCache(ctx context.Context, accountID string, accessToken, proxyURL string) (*UsageResponse, error) {
	// 检查缓存
	if val, ok := g.usageCache.Load(accountID); ok {
		entry := val.(*usageCacheEntry)
		if time.Since(entry.fetchedAt) < usageCacheTTL {
			return entry.data, nil
		}
	}

	// 缓存过期或不存在，发起请求
	resp, err := g.fetchUsage(ctx, accessToken, proxyURL)
	if err != nil {
		return nil, err
	}

	// 写入缓存
	g.usageCache.Store(accountID, &usageCacheEntry{data: resp, fetchedAt: time.Now()})
	return resp, nil
}

// fetchUsage 从 Anthropic API 获取 OAuth 账号用量（无缓存）
func (g *AnthropicGateway) fetchUsage(ctx context.Context, accessToken, proxyURL string) (*UsageResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, usageAPIURL, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+accessToken)
	httpReq.Header.Set("anthropic-beta", BetaOAuth)
	httpReq.Header.Set("Accept", "application/json, text/plain, */*")
	httpReq.Header.Set("Content-Type", "application/json")
	// 使用 Claude Code 伪装头
	for k, v := range DefaultHeaders {
		httpReq.Header.Set(k, v)
	}

	// 使用 TLS 指纹客户端（api.anthropic.com 用 Claude CLI 指纹）
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildFingerprintTransport(proxyURL),
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var usageResp UsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&usageResp); err != nil {
		return nil, fmt.Errorf("解析 usage 响应失败: %w", err)
	}
	return &usageResp, nil
}

// ──────────────────────────────────────────────────────
// 工具函数
// ──────────────────────────────────────────────────────

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// extractUsageFromBody 从响应 body 中提取 usage（非流式响应使用）
func extractUsageFromBody(body []byte) (inputTokens, outputTokens, cacheTokens int) {
	inputTokens = int(gjson.GetBytes(body, "usage.input_tokens").Int())
	outputTokens = int(gjson.GetBytes(body, "usage.output_tokens").Int())
	cacheTokens = int(gjson.GetBytes(body, "usage.cache_read_input_tokens").Int())
	return
}

// validateForwardRequest 验证请求基本参数（前置检查）
func validateForwardRequest(body []byte) error {
	if len(body) == 0 {
		return nil
	}
	model := gjson.GetBytes(body, "model").String()
	if model == "" {
		return fmt.Errorf("请求体缺少 model 字段")
	}
	return nil
}

// buildCountTokensHeaders 为 count_tokens 请求构建特殊的 beta header
func buildCountTokensHeaders(req *http.Request, account *sdk.Account) {
	switch account.Type {
	case "apikey":
		apiKey := account.Credentials["api_key"]
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-beta", APIKeyBetaHeader+","+BetaTokenCounting)
	case "oauth", "session_key", "setup_token":
		token := account.Credentials["access_token"]
		setRawHeader(req.Header, "authorization", "Bearer "+token)
		setRawHeader(req.Header, "anthropic-beta", CountTokensBetaHeader)
		for k, v := range DefaultHeaders {
			if isRawCaseHeader(k) {
				setRawHeader(req.Header, k, v)
			} else {
				req.Header.Set(k, v)
			}
		}
		setRawHeader(req.Header, "Accept", "application/json")
	}
	setRawHeader(req.Header, "anthropic-version", DefaultAnthropicVersion)
	setRawHeader(req.Header, "content-type", "application/json")
}

// isCountTokensRequest 检查是否为 count_tokens 请求
func isCountTokensRequest(req *sdk.ForwardRequest) bool {
	path := req.Headers.Get("X-Original-Path")
	return path == "/v1/messages/count_tokens"
}

// forwardCountTokens 转发 count_tokens 请求
func (g *AnthropicGateway) forwardCountTokens(ctx context.Context, req *sdk.ForwardRequest) (*sdk.ForwardResult, error) {
	start := time.Now()
	account := req.Account

	targetURL := resolveBaseURL(account.Credentials) + "/v1/messages/count_tokens?beta=true"

	body := normalizeRequestBody(req.Body)

	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构建上游请求失败: %w", err)
	}

	buildCountTokensHeaders(upstreamReq, account)

	client := g.getHTTPClient(account)
	resp, err := client.Do(upstreamReq)
	if err != nil {
		return nil, fmt.Errorf("请求上游失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取上游响应失败: %w", err)
	}

	if req.Writer != nil {
		req.Writer.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		req.Writer.WriteHeader(resp.StatusCode)
		_, _ = req.Writer.Write(respBody)
	}

	if resp.StatusCode >= 400 {
		return &sdk.ForwardResult{
			StatusCode:    resp.StatusCode,
			Duration:      time.Since(start),
			AccountStatus: accountStatusFromCode(resp.StatusCode),
		}, fmt.Errorf("上游返回 %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	return &sdk.ForwardResult{
		StatusCode: resp.StatusCode,
		Duration:   time.Since(start),
		Body:       respBody,
		Headers:    resp.Header,
	}, nil
}
