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
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// AnthropicGateway Claude 网关插件
type AnthropicGateway struct {
	logger   *slog.Logger
	ctx      sdk.PluginContext
	tokenMgr *tokenManager
	stdPool  *StandardTransportPool
	fpPool   *FingerprintTransportPool
	registry *accountRegistry
	sidecar  *sidecarRunner
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

	// 账号注册表 + sidecar 运行器
	g.registry = newAccountRegistry()
	g.sidecar = newSidecarRunner(g)

	// 初始化 token 刷新管理器
	g.tokenMgr = newTokenManager(g, g.logger)

	g.logger.Info("Claude 网关插件初始化")
	return nil
}

func (g *AnthropicGateway) Start(_ context.Context) error {
	g.logger.Info("Claude 网关插件启动", "pool_stats", poolStats(g.stdPool, g.fpPool))
	if g.sidecar != nil {
		g.sidecar.start()
	}
	return nil
}

func (g *AnthropicGateway) Stop(_ context.Context) error {
	g.logger.Info("Claude 网关插件停止")
	if g.sidecar != nil {
		g.sidecar.stop()
	}
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

func (g *AnthropicGateway) Forward(ctx context.Context, req *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
	// 抽取/生成 request_id 并派生请求级 logger，注入 ctx 供下游使用
	rid := sdk.ExtractOrGenerateRequestID(req.Headers)
	logger := sdk.LoggerFromContext(ctx).With(sdk.LogFieldRequestID, rid)
	if logger == nil {
		logger = g.logger.With(sdk.LogFieldRequestID, rid)
	}
	ctx = sdk.WithLogger(ctx, logger)
	ctx = sdk.WithRequestID(ctx, rid)

	method := http.MethodPost
	path := resolveRequestPath(req)
	logger.Debug("plugin_request_received",
		sdk.LogFieldMethod, method,
		sdk.LogFieldPath, path,
		sdk.LogFieldModel, req.Model,
		"stream", req.Stream,
	)
	return g.forwardHTTP(ctx, req)
}

// HandleWebSocket 不支持 WebSocket
func (g *AnthropicGateway) HandleWebSocket(_ context.Context, _ sdk.WebSocketConn) (sdk.ForwardOutcome, error) {
	return sdk.ForwardOutcome{}, sdk.ErrNotSupported
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
			tokenResp, err = g.ExchangeSessionKeyForToken(ctx, raw.SessionKey, raw.ProxyURL)
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

			usageResp, err := g.fetchUsage(ctx, accessToken, a.Credentials["proxy_url"])
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

		accountType := "oauth"
		tokenResp, err := g.ExchangeSessionKeyForToken(ctx, raw.SessionKey, raw.ProxyURL)
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
		// 批量 Cookie Auth：通过多个 session_key 换取 OAuth Token 并返回完整凭证
		var raw struct {
			SessionKeys []string `json:"session_keys"`
			ProxyURL    string   `json:"proxy_url"`
			Scope       string   `json:"scope"`
		}
		if err := json.Unmarshal(body, &raw); err != nil || len(raw.SessionKeys) == 0 {
			return http.StatusBadRequest, nil, jsonError("缺少 session_keys 参数"), nil
		}

		type batchResult struct {
			AccountType string            `json:"account_type,omitempty"`
			AccountName string            `json:"account_name,omitempty"`
			Credentials map[string]string `json:"credentials,omitempty"`
			Status      string            `json:"status"`
			Error       string            `json:"error,omitempty"`
		}

		results := make([]batchResult, 0, len(raw.SessionKeys))
		for _, sk := range raw.SessionKeys {
			tokenResp, err := g.ExchangeSessionKeyForToken(ctx, sk, raw.ProxyURL)
			if err != nil {
				results = append(results, batchResult{Status: "failed", Error: err.Error()})
				continue
			}

			expiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)
			credentials := map[string]string{
				"access_token":  tokenResp.AccessToken,
				"refresh_token": tokenResp.RefreshToken,
				"expires_at":    expiresAt,
			}

			r := batchResult{Status: "ok", AccountType: "oauth", Credentials: credentials}
			if tokenResp.Account != nil {
				r.AccountName = tokenResp.Account.EmailAddress
				credentials["email"] = tokenResp.Account.EmailAddress
				credentials["account_uuid"] = tokenResp.Account.UUID
			}
			if tokenResp.Organization != nil {
				credentials["org_uuid"] = tokenResp.Organization.UUID
			}
			results = append(results, r)
		}

		return http.StatusOK, nil, jsonMarshal(map[string]any{"results": results}), nil

	default:
		return http.StatusNotFound, nil, jsonError("未知的操作: " + path), nil
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

// fetchUsage 从 Anthropic API 获取 OAuth 账号用量
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

// buildCountTokensHeaders 为 count_tokens 请求构建特殊的 beta header
func buildCountTokensHeaders(req *http.Request, account *sdk.Account) {
	switch account.Type {
	case "apikey":
		apiKey := account.Credentials["api_key"]
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-beta", APIKeyBetaHeader+","+BetaTokenCounting)
	case "oauth", "session_key":
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

// forwardCountTokens 转发 count_tokens 请求。
func (g *AnthropicGateway) forwardCountTokens(ctx context.Context, req *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
	start := time.Now()
	account := req.Account
	logger := sdk.LoggerFromContext(ctx)

	targetURL := resolveBaseURL(account.Credentials) + "/v1/messages/count_tokens?beta=true"
	body := normalizeRequestBody(req.Body)

	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		reason := fmt.Sprintf("构建上游请求失败: %v", err)
		logger.Warn("upstream_request_build_failed",
			sdk.LogFieldAccountID, account.ID,
			"url", redactURL(targetURL),
			"op", "count_tokens",
			sdk.LogFieldError, err,
		)
		return transientOutcome(reason), fmt.Errorf("%s", reason)
	}
	buildCountTokensHeaders(upstreamReq, account)

	logger.Debug("upstream_request_start",
		sdk.LogFieldAccountID, account.ID,
		sdk.LogFieldModel, req.Model,
		"url", redactURL(targetURL),
		sdk.LogFieldMethod, http.MethodPost,
		"op", "count_tokens",
	)

	client := g.getHTTPClient(account)
	resp, err := client.Do(upstreamReq)
	if err != nil {
		dur := time.Since(start)
		logger.Warn("upstream_request_failed",
			sdk.LogFieldAccountID, account.ID,
			"op", "count_tokens",
			sdk.LogFieldDurationMs, dur.Milliseconds(),
			sdk.LogFieldError, err,
		)
		return transientOutcome(err.Error()), fmt.Errorf("请求上游失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		reason := fmt.Sprintf("读取上游响应失败: %v", err)
		logger.Warn("upstream_response_read_failed",
			sdk.LogFieldAccountID, account.ID,
			"op", "count_tokens",
			sdk.LogFieldError, err,
		)
		return transientOutcome(reason), fmt.Errorf("%s", reason)
	}
	if req.Writer != nil {
		req.Writer.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		req.Writer.WriteHeader(resp.StatusCode)
		_, _ = req.Writer.Write(respBody)
	}

	elapsed := time.Since(start)
	if resp.StatusCode >= 400 {
		msg := extractErrorMessage(respBody)
		if msg == "" {
			msg = truncate(string(respBody), 200)
		}
		logger.Warn("upstream_request_non_2xx",
			sdk.LogFieldAccountID, account.ID,
			"op", "count_tokens",
			sdk.LogFieldStatus, resp.StatusCode,
			sdk.LogFieldDurationMs, elapsed.Milliseconds(),
			sdk.LogFieldReason, msg,
		)
		outcome := failureOutcome(resp.StatusCode, respBody, resp.Header.Clone(), msg, extractRetryAfterHeader(resp.Header))
		outcome.Duration = elapsed
		return outcome, nil
	}
	logger.Debug("upstream_request_completed",
		sdk.LogFieldAccountID, account.ID,
		"op", "count_tokens",
		sdk.LogFieldStatus, resp.StatusCode,
		sdk.LogFieldDurationMs, elapsed.Milliseconds(),
		"content_length", int64(len(respBody)),
	)
	return sdk.ForwardOutcome{
		Kind:     sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{StatusCode: resp.StatusCode, Headers: resp.Header.Clone(), Body: respBody},
		Duration: elapsed,
	}, nil
}
