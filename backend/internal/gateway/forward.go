package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

const (
	defaultBaseURL       = "https://api.anthropic.com"
	httpTimeout          = 3 * time.Minute // 对齐 CC 2.1.112 X-Stainless-Timeout=300
	httpDialTimeout      = 30 * time.Second
	httpTLSTimeout       = 15 * time.Second
	httpIdleTimeout      = 90 * time.Second
	httpMaxIdleConns     = 100
	httpIdleConnsPerHost = 20
)

// ──────────────────────────────────────────────────────
// 转发入口
// ──────────────────────────────────────────────────────

// forwardHTTP 根据请求路径和账号类型分发。
func (g *AnthropicGateway) forwardHTTP(ctx context.Context, req *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
	logger := sdk.LoggerFromContext(ctx)
	path := resolveRequestPath(req)

	if path == "/v1/models" {
		return g.handleModelsRequest(req), nil
	}
	if path == "/v1/messages/count_tokens" {
		return g.forwardCountTokens(ctx, req)
	}

	account := req.Account
	switch account.Type {
	case "apikey":
		return g.forwardAPIKey(ctx, req, path)
	case "oauth", "session_key":
		return g.forwardOAuth(ctx, req, path)
	default:
		reason := fmt.Sprintf("未知的账号类型: %s", account.Type)
		logger.Warn("forward_dispatch_failed",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldReason, reason,
			sdk.LogFieldError, reason,
		)
		return accountDeadOutcome(reason), fmt.Errorf("%s", reason)
	}
}

// redactURL 去掉 query string，仅保留 host+path（避免 ?beta=true 之类敏感参数）
func redactURL(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u != nil {
		u.RawQuery = ""
		u.User = nil
		return u.String()
	}
	if idx := strings.Index(rawURL, "?"); idx >= 0 {
		return rawURL[:idx]
	}
	return rawURL
}

// ──────────────────────────────────────────────────────
// API Key 模式：直接转发到上游
// ──────────────────────────────────────────────────────

func (g *AnthropicGateway) forwardAPIKey(ctx context.Context, req *sdk.ForwardRequest, path string) (sdk.ForwardOutcome, error) {
	start := time.Now()
	account := req.Account
	logger := sdk.LoggerFromContext(ctx)

	targetURL := resolveBaseURL(account.Credentials) + path
	body := preprocessBody(req.Body)

	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		reason := fmt.Sprintf("构建上游请求失败: %v", err)
		logger.Warn("upstream_request_build_failed",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, req.Model,
			"url", redactURL(targetURL),
			sdk.LogFieldError, err,
		)
		return transientOutcome(reason), fmt.Errorf("%s", reason)
	}
	setAnthropicAuthHeaders(upstreamReq, account, req.Headers, req.Model)

	logger.Debug("upstream_request_start",
		sdk.LogFieldAccountID, account.ID,
		sdk.LogFieldModel, req.Model,
		"url", redactURL(targetURL),
		sdk.LogFieldMethod, http.MethodPost,
		"stream", req.Stream,
		"account_type", "apikey",
	)

	client := g.getHTTPClient(account)
	resp, err := client.Do(upstreamReq)
	if err != nil {
		dur := time.Since(start)
		logger.Warn("upstream_request_failed",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, req.Model,
			sdk.LogFieldDurationMs, dur.Milliseconds(),
			sdk.LogFieldError, err,
		)
		return transientOutcome(err.Error()), fmt.Errorf("请求上游失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	dur := time.Since(start)
	if resp.StatusCode >= 400 {
		logger.Warn("upstream_request_non_2xx",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, req.Model,
			sdk.LogFieldStatus, resp.StatusCode,
			sdk.LogFieldDurationMs, dur.Milliseconds(),
		)
		return handleErrorResponse(resp, req.Writer, start), nil
	}

	logger.Debug("upstream_request_completed",
		sdk.LogFieldAccountID, account.ID,
		sdk.LogFieldModel, req.Model,
		sdk.LogFieldStatus, resp.StatusCode,
		sdk.LogFieldDurationMs, dur.Milliseconds(),
		"content_length", resp.ContentLength,
		"stream", req.Stream,
	)

	if req.Stream && req.Writer != nil {
		return handleStreamResponse(resp, req.Writer, start)
	}
	return handleNonStreamResponse(resp, req.Writer, start)
}

// ──────────────────────────────────────────────────────
// OAuth 模式：使用 OAuth token 转发
// ──────────────────────────────────────────────────────

func (g *AnthropicGateway) forwardOAuth(ctx context.Context, req *sdk.ForwardRequest, path string) (sdk.ForwardOutcome, error) {
	start := time.Now()
	account := req.Account
	logger := sdk.LoggerFromContext(ctx)

	// Claude Code 客户端身份闸：X-Airgate-Plugin-Claude-Claude-Code-Only: true 或
	// 账号级凭证 claude_code_only=true。
	ccOnly := strings.EqualFold(req.Headers.Get("X-Airgate-Plugin-Claude-Claude-Code-Only"), "true") ||
		strings.EqualFold(account.Credentials["claude_code_only"], "true")
	if ccOnly {
		if result := validateClaudeCodeRequest(path, req.Body, req.Headers); !result.OK {
			logger.Warn("claude_code_only_reject",
				sdk.LogFieldAccountID, account.ID,
				sdk.LogFieldPath, path,
				sdk.LogFieldReason, result.Reason,
			)
			return rejectNonCCRequest(req.Writer, result.Reason, start), nil
		}
	}

	if g.registry != nil {
		g.registry.register(account)
	}

	updatedCreds, err := g.tokenMgr.ensureValidToken(ctx, account)
	if err != nil {
		// token 刷新失败的原因千差万别（refresh_token 吊销 → 账号死；网络抖动 → transient）；
		// 这里保守按 AccountDead 处理，让核心把账号打 disabled 等待人工介入重新授权。
		reason := fmt.Sprintf("token 刷新失败: %v", err)
		logger.Warn("token_ensure_failed",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldError, err,
		)
		return accountDeadOutcome(reason), fmt.Errorf("%s", reason)
	}
	if len(updatedCreds) > 0 && g.sidecar != nil {
		g.sidecar.fireRefreshProbe(account)
	}
	if account.Credentials["access_token"] == "" {
		reason := "OAuth 账号缺少 access_token"
		logger.Warn("oauth_missing_access_token",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldError, reason,
		)
		return accountDeadOutcome(reason), fmt.Errorf("%s", reason)
	}

	// OAuth 账号必须带 ?beta=true 参数（参考 sub2api）
	targetURL := resolveBaseURL(account.Credentials) + path + "?beta=true"
	body := preprocessBody(req.Body)
	body = preprocessOAuthBody(body, account)

	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		reason := fmt.Sprintf("构建上游请求失败: %v", err)
		logger.Warn("upstream_request_build_failed",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, req.Model,
			"url", redactURL(targetURL),
			sdk.LogFieldError, err,
		)
		return transientOutcome(reason), fmt.Errorf("%s", reason)
	}
	setAnthropicAuthHeaders(upstreamReq, account, req.Headers, req.Model)
	if req.Stream {
		setRawHeader(upstreamReq.Header, "x-stainless-helper-method", "stream")
	}

	logger.Debug("upstream_request_start",
		sdk.LogFieldAccountID, account.ID,
		sdk.LogFieldModel, req.Model,
		"url", redactURL(targetURL),
		sdk.LogFieldMethod, http.MethodPost,
		"stream", req.Stream,
		"account_type", account.Type,
	)

	client := g.getHTTPClient(account)
	resp, err := client.Do(upstreamReq)
	if err != nil {
		dur := time.Since(start)
		logger.Warn("upstream_request_failed",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, req.Model,
			sdk.LogFieldDurationMs, dur.Milliseconds(),
			sdk.LogFieldError, err,
		)
		return transientOutcome(err.Error()), fmt.Errorf("请求上游失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	dur := time.Since(start)
	if resp.StatusCode >= 400 {
		logger.Warn("upstream_request_non_2xx",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, req.Model,
			sdk.LogFieldStatus, resp.StatusCode,
			sdk.LogFieldDurationMs, dur.Milliseconds(),
		)
		outcome := handleErrorResponse(resp, req.Writer, start)
		if len(updatedCreds) > 0 {
			outcome.UpdatedCredentials = updatedCreds
		}
		return outcome, nil
	}

	logger.Debug("upstream_request_completed",
		sdk.LogFieldAccountID, account.ID,
		sdk.LogFieldModel, req.Model,
		sdk.LogFieldStatus, resp.StatusCode,
		sdk.LogFieldDurationMs, dur.Milliseconds(),
		"content_length", resp.ContentLength,
		"stream", req.Stream,
	)

	var outcome sdk.ForwardOutcome
	if req.Stream && req.Writer != nil {
		outcome, err = handleStreamResponse(resp, req.Writer, start)
	} else {
		outcome, err = handleNonStreamResponse(resp, req.Writer, start)
	}
	if len(updatedCreds) > 0 {
		outcome.UpdatedCredentials = updatedCreds
	}

	// 成功响应后异步补发 count_tokens，模拟真实 CLI 的 token 估算流量
	if err == nil && outcome.Kind == sdk.OutcomeSuccess && g.sidecar != nil {
		g.sidecar.scheduleCountTokens(account.ID, body)
	}
	return outcome, err
}

// Token 刷新逻辑已迁移到 token_manager.go

// ──────────────────────────────────────────────────────
// /v1/models 处理
// ──────────────────────────────────────────────────────

func (g *AnthropicGateway) handleModelsRequest(req *sdk.ForwardRequest) sdk.ForwardOutcome {
	body := buildModelsResponse()
	if req.Writer != nil {
		req.Writer.Header().Set("Content-Type", "application/json")
		req.Writer.WriteHeader(http.StatusOK)
		_, _ = req.Writer.Write(body)
	}
	headers := http.Header{"Content-Type": []string{"application/json"}}
	return successOutcome(http.StatusOK, body, headers, nil)
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

// preprocessBody 预处理请求体：规范化模型 ID + 补充必填字段 + 第三方红线净化
func preprocessBody(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	body = normalizeRequestBody(body)

	// Anthropic API 要求 max_tokens
	if !gjson.GetBytes(body, "max_tokens").Exists() {
		body, _ = sjson.SetBytes(body, "max_tokens", 4096)
	}

	// 剥离空 text / 非法位置 thinking 块，避免上游 400
	body = sanitizeBody(body)

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
	//    使用 sticky session：同会话 30 min 内复用同一 user_id，模拟真实 CLI 行为
	if !gjson.GetBytes(body, "metadata.user_id").Exists() {
		accountUUID := account.Credentials["account_uuid"]
		if accountUUID == "" {
			accountUUID = fmt.Sprintf("%d", account.ID)
		}
		fingerprint := conversationFingerprint(body)
		sessionUUID := defaultSessionCache.stickyUserID(account.ID, fingerprint)
		userID := fmt.Sprintf("user_%s_account_%s_session_%s",
			newUUIDv4(),
			accountUUID,
			sessionUUID,
		)
		body, _ = sjson.SetBytes(body, "metadata", map[string]string{"user_id": userID})
	}

	// 3. 确保 tools 字段存在（Claude Code 总是发送 tools，即使为空）
	//    若 tools 非空，在最后一项补 cache_control: ephemeral（提升 prompt cache 命中率）
	if !gjson.GetBytes(body, "tools").Exists() {
		body, _ = sjson.SetRawBytes(body, "tools", []byte("[]"))
	} else {
		body = appendToolsEphemeralCache(body)
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

// appendToolsEphemeralCache 在最后一个 tool 上加 cache_control: ephemeral
// CC 2.1.10x+ 行为：仅末尾工具贴 ephemeral 标记，显著提升 prompt cache 命中率。
// 若已存在 cache_control 则跳过，避免覆盖客户端更细粒度的缓存策略。
func appendToolsEphemeralCache(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}
	arr := tools.Array()
	if len(arr) == 0 {
		return body
	}
	lastIdx := len(arr) - 1
	if gjson.GetBytes(body, fmt.Sprintf("tools.%d.cache_control", lastIdx)).Exists() {
		return body
	}
	newBody, err := sjson.SetBytes(body, fmt.Sprintf("tools.%d.cache_control", lastIdx),
		map[string]string{"type": "ephemeral"})
	if err != nil {
		return body
	}
	return newBody
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

// handleErrorResponse 把上游 4xx/5xx 响应归类为 ForwardOutcome。
//
//   - ClientError（普通 4xx）：写 w 透传给客户端，Core 不 failover
//   - AccountRateLimited / AccountDead（429/401/403/400 含账号级文本）：不写 w，保持 Writer
//     unwritten 让 Core canFailover 不被短路；Core 会 failover + 触发状态机
//   - UpstreamTransient（5xx）：不写 w，让 Core failover
func handleErrorResponse(resp *http.Response, w http.ResponseWriter, start time.Time) sdk.ForwardOutcome {
	respBody, _ := io.ReadAll(resp.Body)
	msg := extractErrorMessage(respBody)
	if msg == "" {
		msg = truncate(string(respBody), 200)
	}

	outcome := failureOutcome(resp.StatusCode, respBody, resp.Header.Clone(), msg, extractRetryAfterHeader(resp.Header))
	outcome.Duration = time.Since(start)

	// 仅 ClientError 透传给客户端；账号级 / 上游抖动保持 w unwritten 以便 failover
	if outcome.Kind == sdk.OutcomeClientError && w != nil {
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
	}
	return outcome
}

// rejectNonCCRequest 网关主动拒绝非 Claude Code 客户端，归为 ClientError。
// 账号本身没问题，不应关闭调度——Core 看到 ClientError 就不会罚账号。
func rejectNonCCRequest(w http.ResponseWriter, reason string, start time.Time) sdk.ForwardOutcome {
	body := ccRejectBody(reason)
	if w != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(body)
	}
	return sdk.ForwardOutcome{
		Kind: sdk.OutcomeClientError,
		Upstream: sdk.UpstreamResponse{
			StatusCode: http.StatusForbidden,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       body,
		},
		Reason:   "claude_code_only: " + reason,
		Duration: time.Since(start),
	}
}

// getHTTPClient 从连接池获取 HTTP 客户端（按账号 ID + tls_profile 分桶）
func (g *AnthropicGateway) getHTTPClient(account *sdk.Account) *http.Client {
	return getHTTPClient(g.stdPool, g.fpPool, account.ID, account.Type, account.ProxyURL, account.Credentials["tls_profile"])
}
