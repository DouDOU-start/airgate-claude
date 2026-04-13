package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

const (
	defaultBaseURL       = "https://api.anthropic.com"
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
	case "oauth", "setup_token":
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
	targetURL := resolveBaseURL(account.Credentials) + path

	// 预处理请求体：规范化模型 ID（不修改 metadata.user_id）
	body := preprocessBody(req.Body)

	// 构建 HTTP 请求
	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构建上游请求失败: %w", err)
	}

	// 设置认证与协议头
	setAnthropicAuthHeaders(upstreamReq, account, req.Headers, req.Model)

	// 发送请求（使用连接池）
	client := g.getHTTPClient(account)
	resp, err := client.Do(upstreamReq)
	if err != nil {
		return nil, fmt.Errorf("请求上游失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 上游返回错误
	if resp.StatusCode >= 400 {
		result, fwdErr := handleErrorResponse(resp, req.Writer, start)
		if result != nil {
			result.ErrorMessage = extractErrorMessage(result.Body)
			fillCost(result)
		}
		return result, fwdErr
	}

	// 流式/非流式分发
	var result *sdk.ForwardResult
	var fwdErr error
	if req.Stream && req.Writer != nil {
		result, fwdErr = handleStreamResponse(resp, req.Writer, start)
	} else {
		result, fwdErr = handleNonStreamResponse(resp, req.Writer, start)
	}
	if result != nil {
		fillCost(result)
	}
	return result, fwdErr
}

// ──────────────────────────────────────────────────────
// OAuth 模式：使用 OAuth token 转发
// ──────────────────────────────────────────────────────

func (g *AnthropicGateway) forwardOAuth(ctx context.Context, req *sdk.ForwardRequest, path string) (*sdk.ForwardResult, error) {
	start := time.Now()
	account := req.Account

	// 通过 tokenManager 检查并自动刷新过期 token（加锁 + double-check + 重试）
	updatedCreds, err := g.tokenMgr.ensureValidToken(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("token 刷新失败: %w", err)
	}

	if account.Credentials["access_token"] == "" {
		return nil, fmt.Errorf("OAuth 账号缺少 access_token")
	}

	// 统一使用 resolveBaseURL（支持自定义 base_url）
	// OAuth 账号必须带 ?beta=true 参数（参考 sub2api）
	targetURL := resolveBaseURL(account.Credentials) + path + "?beta=true"

	// 预处理请求体
	body := preprocessBody(req.Body)
	// OAuth 账号额外处理：注入 metadata.user_id、tools 等 Claude Code 伪装字段
	body = preprocessOAuthBody(body, account)

	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构建上游请求失败: %w", err)
	}

	setAnthropicAuthHeaders(upstreamReq, account, req.Headers, req.Model)

	// 流式请求加上 helper-method header（参考 sub2api）
	if req.Stream {
		setRawHeader(upstreamReq.Header, "x-stainless-helper-method", "stream")
	}

	// 使用连接池
	client := g.getHTTPClient(account)
	resp, err := client.Do(upstreamReq)
	if err != nil {
		return nil, fmt.Errorf("请求上游失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		result, fwdErr := handleErrorResponse(resp, req.Writer, start)
		if result != nil {
			result.ErrorMessage = extractErrorMessage(result.Body)
			fillCost(result)
			if len(updatedCreds) > 0 {
				result.UpdatedCredentials = updatedCreds
			}
		}
		return result, fwdErr
	}

	var result *sdk.ForwardResult
	var fwdErr error
	if req.Stream && req.Writer != nil {
		result, fwdErr = handleStreamResponse(resp, req.Writer, start)
	} else {
		result, fwdErr = handleNonStreamResponse(resp, req.Writer, start)
	}
	if result != nil {
		fillCost(result)
		if len(updatedCreds) > 0 {
			result.UpdatedCredentials = updatedCreds
		}
	}
	return result, fwdErr
}

// ──────────────────────────────────────────────────────
// Session Key 模式：自动换 token 或刷新后走 OAuth 流程
// ──────────────────────────────────────────────────────

func (g *AnthropicGateway) forwardSessionKey(ctx context.Context, req *sdk.ForwardRequest, path string) (*sdk.ForwardResult, error) {
	// Session Key 的 token exchange 已由 tokenManager.ensureValidToken 处理
	// 直接复用 OAuth 转发逻辑（tokenManager 会在 forwardOAuth 中自动检测并 exchange）
	return g.forwardOAuth(ctx, req, path)
}

// Token 刷新逻辑已迁移到 token_manager.go

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

// resolveBaseURL 从 credentials 解析 base_url，所有账号类型统一使用
func resolveBaseURL(creds map[string]string) string {
	if u := strings.TrimSpace(creds["base_url"]); u != "" {
		return strings.TrimRight(u, "/")
	}
	return defaultBaseURL
}

// preprocessBody 预处理请求体：规范化模型 ID + 补充必填字段
func preprocessBody(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	body = normalizeRequestBody(body)

	// Anthropic API 要求 max_tokens
	if !gjson.GetBytes(body, "max_tokens").Exists() {
		body, _ = sjson.SetBytes(body, "max_tokens", 4096)
	}

	return body
}

// claudeCodeSystemPrompt Claude Code 的标准 system prompt
// Anthropic 通过检测 system prompt 判断是否为合法的 Claude Code 请求
const claudeCodeSystemPrompt = "You are Claude Code, Anthropic's official CLI for Claude."

// preprocessOAuthBody 对 OAuth 账号的请求做 Claude Code 伪装处理
// 参考 sub2api：没有正确伪装的请求会被 Anthropic 判定为第三方流量并返回 rate_limit_error
func preprocessOAuthBody(body []byte, account *sdk.Account) []byte {
	modelID := gjson.GetBytes(body, "model").String()
	isHaiku := strings.Contains(strings.ToLower(modelID), "haiku")

	// 1. 非 Haiku 模型：替换 system prompt 为 Claude Code 标准格式
	//    真实 Claude Code 始终以 [{type: "text", text: "...", cache_control: {type: "ephemeral"}}] 格式发送
	//    使用 string 格式或不包含 Claude Code 提示词会被检测为第三方应用
	if !isHaiku {
		existingSystem := gjson.GetBytes(body, "system")
		needsRewrite := !existingSystem.Exists() ||
			(existingSystem.Type == gjson.String && !strings.Contains(existingSystem.String(), "Claude Code"))

		if needsRewrite {
			// 保存原始 system prompt
			var originalSystem string
			if existingSystem.Type == gjson.String {
				originalSystem = existingSystem.String()
			}

			// 替换为 Claude Code 标准 system（array 格式 + cache_control）
			ccSystem := []map[string]any{
				{
					"type":          "text",
					"text":          claudeCodeSystemPrompt,
					"cache_control": map[string]string{"type": "ephemeral"},
				},
			}
			body, _ = sjson.SetBytes(body, "system", ccSystem)

			// 如果有原始 system prompt，注入到 messages 开头作为 user/assistant 消息对
			originalSystem = strings.TrimSpace(originalSystem)
			if originalSystem != "" && originalSystem != claudeCodeSystemPrompt {
				messages := gjson.GetBytes(body, "messages")
				if messages.IsArray() {
					instrMsg := map[string]any{
						"role":    "user",
						"content": []map[string]any{{"type": "text", "text": "[System Instructions]\n" + originalSystem}},
					}
					ackMsg := map[string]any{
						"role":    "assistant",
						"content": []map[string]any{{"type": "text", "text": "Understood. I will follow these instructions."}},
					}
					// 重建 messages: [instruction, ack, ...original]
					newMsgs := []any{instrMsg, ackMsg}
					for _, msg := range messages.Array() {
						var m any
						_ = json.Unmarshal([]byte(msg.Raw), &m)
						newMsgs = append(newMsgs, m)
					}
					body, _ = sjson.SetBytes(body, "messages", newMsgs)
				}
			}
		}
	}

	// 2. 注入 metadata.user_id（如果缺失）
	if !gjson.GetBytes(body, "metadata.user_id").Exists() {
		accountUUID := account.Credentials["account_uuid"]
		if accountUUID == "" {
			accountUUID = fmt.Sprintf("%d", account.ID)
		}
		userID := fmt.Sprintf("user_%s_account_%s_session_%s",
			generateSessionID(),
			accountUUID,
			generateSessionID(),
		)
		body, _ = sjson.SetBytes(body, "metadata", map[string]string{"user_id": userID})
	}

	// 3. 确保 tools 字段存在（Claude Code 总是发送 tools，即使为空）
	if !gjson.GetBytes(body, "tools").Exists() {
		body, _ = sjson.SetRawBytes(body, "tools", []byte("[]"))
	}

	// 4. 删除 temperature（OAuth 模式下 Claude Code 不发送）
	if gjson.GetBytes(body, "temperature").Exists() {
		body, _ = sjson.DeleteBytes(body, "temperature")
	}

	// 5. 删除 tool_choice（如果 tools 为空）
	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() && len(tools.Array()) == 0 {
		if gjson.GetBytes(body, "tool_choice").Exists() {
			body, _ = sjson.DeleteBytes(body, "tool_choice")
		}
	}

	return body
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

// getHTTPClient 从连接池获取 HTTP 客户端
func (g *AnthropicGateway) getHTTPClient(account *sdk.Account) *http.Client {
	return getHTTPClient(g.stdPool, g.fpPool, account.Type, account.ProxyURL)
}
