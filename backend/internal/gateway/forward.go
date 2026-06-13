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

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

const (
	defaultBaseURL       = "https://api.anthropic.com"
	httpDialTimeout      = 30 * time.Second
	httpTLSTimeout       = 15 * time.Second
	httpIdleTimeout      = 90 * time.Second
	httpMaxIdleConns     = 100
	httpIdleConnsPerHost = 20

	// 流式超时模型（详见 doUpstreamAndDispatch / handleStreamResponse）：
	// 流一旦开始就不按"总耗时"掐断，只在"等首响应头过久"或"流中途持续静默"时中止。
	// 三者均可经插件 config 覆盖：default_timeout / first_byte_timeout / stream_idle_timeout。
	defaultNonStreamTotalTimeout = 300 * time.Second // 非流式总超时（无渐进输出，可按总时长封顶；对齐 X-Stainless-Timeout=300）
	defaultFirstByteTimeout      = 60 * time.Second  // 流式等首响应头上限（头到即解除，不波及 body 读取）
	defaultStreamIdleTimeout     = 60 * time.Second  // 流式读空闲上限：连续此时长无任何新数据才判定卡死中止
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

	targetURL := resolveBaseURL(account.Credentials) + path + "?beta=true"
	// API Key 为第一方直连凭证：最小化、无损预处理，不做 OAuth 伪装/净化，
	// 避免破坏 cache_control TTL 顺序、tool_use 前的 thinking 块与反斜杠转义。
	body := preprocessAPIKeyBody(req.Body)

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
		"account_type", "apikey",
	)

	return g.doUpstreamAndDispatch(ctx, upstreamReq, req, start)
}

// doUpstreamAndDispatch 发起上游请求并按流式/非流式分发，统一管理超时。
//
// 超时模型：
//   - 非流式：复用 client 的总超时（nonStreamTotalTimeout）。
//   - 流式：client 不设总超时；仅用首字节计时器约束"等首响应头"阶段（头到即解除）。
//     此阶段超时尚未写出任何字节，归 transient（Core 可 failover）；进入 body 读取后
//     改由 handleStreamResponse 内的读空闲守卫判活——流持续输出就不掐断，连续静默超
//     stream_idle_timeout 才中止。
//   - 客户端断开：经请求 ctx 立即传播。
func (g *AnthropicGateway) doUpstreamAndDispatch(ctx context.Context, upstreamReq *http.Request, req *sdk.ForwardRequest, start time.Time) (sdk.ForwardOutcome, error) {
	account := req.Account
	logger := sdk.LoggerFromContext(ctx)

	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	upstreamReq = upstreamReq.WithContext(reqCtx)

	// 流式：仅约束"建连 + 等首响应头"阶段；拿到响应头即解除，不波及 body 流式读取。
	var firstByteTimer *time.Timer
	if req.Stream {
		firstByteTimer = time.AfterFunc(g.firstByteTimeout(), cancel)
	}

	client := g.getHTTPClient(account, req.Stream)
	resp, err := client.Do(upstreamReq)
	if firstByteTimer != nil {
		firstByteTimer.Stop()
	}
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
		return handleStreamResponse(resp, req.Writer, start, g.streamIdleTimeout(), cancel)
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

	outcome, err := g.doUpstreamAndDispatch(ctx, upstreamReq, req, start)
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

// preprocessAPIKeyBody 对 API Key（第一方 Anthropic 直连凭证）账号做最小化、无损预处理。
//
// API Key 不需要 OAuth 反作弊伪装，也不应改写客户端请求体：system / messages /
// tools / cache_control 一律原样透传。历史上 apikey 路径误用了 preprocessBody +
// preprocessOAuthBody，会带来三类工具调用故障：
//   - appendToolsEphemeralCache 给 tools 注入默认 5m cache_control，与客户端
//     system 上的 1h TTL 冲突（处理顺序 tools→system）→ 上游 400
//     "ttl='1h' cache_control block must not come after a ttl='5m'"
//   - sanitizeBody 剥离 tool_use 之前的 thinking 块，破坏 interleaved-thinking +
//     工具调用的回合结构 → 上游 400
//   - sanitizeBody / 消息注入对含反斜杠（Windows 路径 C:\\…、正则 \d）的 body 做
//     json 往返，转义被损坏 → "invalid character ... in string escape code"
//
// 这里只做两件转发必需且转义安全的事，其余原样透传：
//  1. 规范化 model ID（仅替换 model 字段，不触碰其它字符串）
//  2. 补 max_tokens 默认值（Anthropic 必填；sjson 原地插入，不解码已有字符串）
func preprocessAPIKeyBody(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	body = normalizeRequestBody(body)
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
	// 1. 替换 system prompt 为 Claude Code 标准格式
	//    真实 Claude Code 始终以 [{type: "text", text: "...", cache_control: {type: "ephemeral"}}] 格式发送
	//    上游代理对所有模型（含 Haiku）的非 probe 请求都做 system prompt Dice 系数校验
	existingSystem := gjson.GetBytes(body, "system")
	needsRewrite := !existingSystem.Exists() ||
		(existingSystem.Type == gjson.String && !strings.Contains(existingSystem.String(), "Claude Code"))

	if needsRewrite {
		var originalSystem string
		if existingSystem.Type == gjson.String {
			originalSystem = existingSystem.String()
		}

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

	// 2. 注入 metadata.user_id（如果缺失）
	//    CLI >= 2.1.78 使用 JSON 格式；使用 sticky session 30 min 内复用同一 session_id
	if !gjson.GetBytes(body, "metadata.user_id").Exists() {
		accountUUID := account.Credentials["account_uuid"]
		if accountUUID == "" {
			accountUUID = newUUIDv4()
		}
		fingerprint := conversationFingerprint(body)
		sessionID := defaultSessionCache.stickyUserID(account.ID, fingerprint)
		deviceID := newDeviceID(account.ID)
		userIDJSON, _ := json.Marshal(map[string]string{
			"device_id":    deviceID,
			"account_uuid": accountUUID,
			"session_id":   sessionID,
		})
		body, _ = sjson.SetBytes(body, "metadata.user_id", string(userIDJSON))
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
//   - ClientError（普通 4xx）：只返回 Upstream，Core 最终负责透传
//   - AccountRateLimited / AccountDead（429/401/403/400 含账号级文本）：不写 w，保持 Writer
//     unwritten 让 Core canFailover 不被短路；Core 会 failover + 触发状态机
//   - UpstreamTransient（5xx）：不写 w，让 Core failover
func handleErrorResponse(resp *http.Response, _ http.ResponseWriter, start time.Time) sdk.ForwardOutcome {
	respBody, _ := io.ReadAll(resp.Body)
	msg := extractErrorMessage(respBody)
	if msg == "" {
		msg = truncate(string(respBody), 200)
	}

	outcome := failureOutcome(resp.StatusCode, respBody, resp.Header.Clone(), msg, extractRetryAfterHeader(resp.Header))
	outcome.Duration = time.Since(start)
	return outcome
}

// rejectNonCCRequest 网关主动拒绝非 Claude Code 客户端，归为 ClientError。
// 账号本身没问题，不应关闭调度——Core 看到 ClientError 就不会罚账号。
func rejectNonCCRequest(_ http.ResponseWriter, reason string, start time.Time) sdk.ForwardOutcome {
	body := ccRejectBody(reason)
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

// getHTTPClient 从连接池获取 HTTP 客户端（按账号 ID + tls_profile 分桶）。
// stream=true 时不设总超时（Timeout=0）：流式由首字节计时器 + 读空闲守卫判活，
// 避免仍在持续输出的长响应被"总耗时"掐断。
func (g *AnthropicGateway) getHTTPClient(account *sdk.Account, stream bool) *http.Client {
	total := g.nonStreamTotalTimeout()
	if stream {
		total = 0
	}
	return getHTTPClient(g.stdPool, g.fpPool, account.ID, account.Type, account.ProxyURL, account.Credentials["tls_profile"], total)
}

// durationConfig 从插件 config 读取时长配置，缺失或非正值时回退默认。
func (g *AnthropicGateway) durationConfig(key string, fallback time.Duration) time.Duration {
	if g == nil || g.ctx == nil || g.ctx.Config() == nil {
		return fallback
	}
	if d := g.ctx.Config().GetDuration(key); d > 0 {
		return d
	}
	return fallback
}

// nonStreamTotalTimeout 非流式请求总超时（可经 config default_timeout 覆盖）。
func (g *AnthropicGateway) nonStreamTotalTimeout() time.Duration {
	return g.durationConfig("default_timeout", defaultNonStreamTotalTimeout)
}

// firstByteTimeout 流式等首响应头上限（可经 config first_byte_timeout 覆盖）。
func (g *AnthropicGateway) firstByteTimeout() time.Duration {
	return g.durationConfig("first_byte_timeout", defaultFirstByteTimeout)
}

// streamIdleTimeout 流式读空闲上限（可经 config stream_idle_timeout 覆盖）。
func (g *AnthropicGateway) streamIdleTimeout() time.Duration {
	return g.durationConfig("stream_idle_timeout", defaultStreamIdleTimeout)
}
