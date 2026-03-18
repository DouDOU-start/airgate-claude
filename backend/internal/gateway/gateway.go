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
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// AnthropicGateway Anthropic 网关插件
type AnthropicGateway struct {
	logger *slog.Logger
	ctx    sdk.PluginContext
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
	g.logger.Info("Anthropic 网关插件初始化")
	return nil
}

func (g *AnthropicGateway) Start(_ context.Context) error {
	g.logger.Info("Anthropic 网关插件启动")
	return nil
}

func (g *AnthropicGateway) Stop(_ context.Context) error {
	g.logger.Info("Anthropic 网关插件停止")
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
		baseURL := credentials["base_url"]
		if baseURL == "" {
			baseURL = defaultBaseURL
		}
		baseURL = trimTrailingSlash(baseURL)

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
		if usageResp.FiveHour.ResetsAt != "" {
			extra["five_hour_resets_at"] = usageResp.FiveHour.ResetsAt
		}
		if usageResp.SevenDay.ResetsAt != "" {
			extra["seven_day_resets_at"] = usageResp.SevenDay.ResetsAt
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
		// 支持两种模式：
		// 1. callback_url 模式（OAuth 浏览器授权回调）
		// 2. session_key 模式（Session Key 自动换 Token）
		var raw struct {
			CallbackURL string `json:"callback_url"`
			SessionKey  string `json:"session_key"`
			ProxyURL    string `json:"proxy_url"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return http.StatusBadRequest, nil, jsonError("请求参数格式错误"), nil
		}

		var tokenResp *TokenResponse
		var err error

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
			// Session Key 自动换 Token 模式
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
		}

		return http.StatusOK, nil, jsonMarshal(map[string]any{
			"account_type": "oauth",
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
		// 查询多个账号的用量
		var accounts []struct {
			ID          int64             `json:"id"`
			Credentials map[string]string `json:"credentials"`
		}
		if err := json.Unmarshal(body, &accounts); err != nil {
			return http.StatusBadRequest, nil, jsonError("invalid request body"), nil
		}

		type usageWindow struct {
			Label       string  `json:"label"`
			Utilization float64 `json:"utilization"`
			ResetsAt    string  `json:"resets_at,omitempty"`
		}
		type accountUsage struct {
			Windows []usageWindow `json:"windows"`
		}

		result := make(map[string]*accountUsage)
		for _, a := range accounts {
			accessToken := a.Credentials["access_token"]
			if accessToken == "" {
				continue
			}

			usageResp, err := g.fetchUsage(ctx, accessToken, a.Credentials["proxy_url"])
			if err != nil || usageResp == nil {
				continue
			}

			usage := &accountUsage{}
			usage.Windows = append(usage.Windows, usageWindow{
				Label:       "5h",
				Utilization: usageResp.FiveHour.Utilization,
				ResetsAt:    usageResp.FiveHour.ResetsAt,
			})
			usage.Windows = append(usage.Windows, usageWindow{
				Label:       "7d",
				Utilization: usageResp.SevenDay.Utilization,
				ResetsAt:    usageResp.SevenDay.ResetsAt,
			})

			result[strconv.FormatInt(a.ID, 10)] = usage
		}

		return http.StatusOK, nil, jsonMarshal(map[string]any{"accounts": result}), nil

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
}

// fetchUsage 从 Anthropic API 获取 OAuth 账号用量
func (g *AnthropicGateway) fetchUsage(ctx context.Context, accessToken, proxyURL string) (*UsageResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageAPIURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("anthropic-beta", BetaOAuth)
	req.Header.Set("User-Agent", "claude-code/2.1.7")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			client.Transport = &http.Transport{Proxy: http.ProxyURL(u)}
		}
	}

	resp, err := client.Do(req)
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
	case "oauth", "session_key":
		token := account.Credentials["access_token"]
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("anthropic-beta", CountTokensBetaHeader)
		for k, v := range DefaultHeaders {
			req.Header.Set(k, v)
		}
	}
	req.Header.Set("anthropic-version", DefaultAnthropicVersion)
	req.Header.Set("Content-Type", "application/json")
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

	targetURL := buildBaseURL(account) + "/v1/messages/count_tokens"
	if account.Type == "oauth" || account.Type == "session_key" {
		targetURL = defaultOAuthBaseURL + "/v1/messages/count_tokens"
	}

	body := normalizeRequestBody(req.Body)

	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构建上游请求失败: %w", err)
	}

	buildCountTokensHeaders(upstreamReq, account)

	client := buildHTTPClient(account)
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
