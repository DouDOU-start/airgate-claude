package gateway

import (
	"math/rand"
	"net/http"
	"strings"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// ──────────────────────────────────────────────────────
// Claude CLI / 运行时 ground truth（对齐 2.1.112）
// ──────────────────────────────────────────────────────

const (
	ClaudeCliVersion = "2.1.112"
	// Bun 单文件 ELF（2.1.112 打包使用 Bun 1.3.13）
	BunRuntimeVersion       = "v1.3.13"
	StainlessPackageVersion = "0.88.0" // @anthropic-ai/sdk ≥ 0.88.0，精确值由 fp capture 确认
	DefaultAnthropicVersion = "2023-06-01"
)

// ──────────────────────────────────────────────────────
// anthropic-beta token 全表（从 2.1.112 binary 抽取）
// ──────────────────────────────────────────────────────

const (
	// 身份类
	BetaOAuth      = "oauth-2025-04-20"
	BetaClaudeCode = "claude-code-20250219"

	// 2025 H1
	BetaTokenEfficientTools      = "token-efficient-tools-2025-02-19"
	BetaWebSearch                = "web-search-2025-03-05"
	BetaFilesAPI                 = "files-api-2025-04-14"
	BetaFineGrainedToolStreaming = "fine-grained-tool-streaming-2025-05-14"
	BetaInterleavedThinking      = "interleaved-thinking-2025-05-14"
	BetaContextManagement        = "context-management-2025-06-27"
	BetaCCRByoc                  = "ccr-byoc-2025-07-29"
	BetaContext1M                = "context-1m-2025-08-07"
	BetaTokenCounting            = "token-counting-2024-11-01"

	// 2025 H2
	BetaSkills              = "skills-2025-10-02"
	BetaToolSearchTool      = "tool-search-tool-2025-10-19"
	BetaEnvironments        = "environments-2025-11-01"
	BetaStructuredOutputsV1 = "structured-outputs-2025-11-13"
	BetaAdvancedToolUse     = "advanced-tool-use-2025-11-20"
	BetaMCPClient           = "mcp-client-2025-11-20"
	BetaEffort              = "effort-2025-11-24"
	BetaMCPServers          = "mcp-servers-2025-12-04"
	BetaStructuredOutputsV2 = "structured-outputs-2025-12-15"

	// 2026
	BetaPromptCachingScope = "prompt-caching-scope-2026-01-05"
	BetaCompact            = "compact-2026-01-12"
	BetaCCRTriggers        = "ccr-triggers-2026-01-30"
	BetaAFKMode            = "afk-mode-2026-01-31"
	BetaFastMode           = "fast-mode-2026-02-01"
	BetaRedactThinking     = "redact-thinking-2026-02-12"
	BetaAdvisorTool        = "advisor-tool-2026-03-01"
	BetaTaskBudgets        = "task-budgets-2026-03-13"
	BetaManagedAgents      = "managed-agents-2026-04-01"
	BetaContextHint        = "context-hint-2026-04-09"
)

// DroppedBetas OAuth 转发时必须从客户端 anthropic-beta 中剥离的 token
//
// 前三项是客户端侧 beta，OAuth 携带会触发 Anthropic 反作弊；
// 后三项属"联动校验类"—— 不随附对应 tool / payload 就会 400。
// 网关统一剥离以防测试账号 / 简单请求意外触发。真正需要的客户端可以在
// tools 字段里声明后再由上层条件补上（留给后续 augmentBetasForBody）。
var DroppedBetas = []string{
	BetaContext1M,
	BetaFastMode,
	BetaRedactThinking,
	BetaSkills,
	BetaAdvisorTool,
	BetaToolSearchTool,
}

// ──────────────────────────────────────────────────────
// anthropic-beta 默认组合
// ──────────────────────────────────────────────────────

// OAuthBetaHeader 非 Haiku 模型 OAuth 账号默认 beta 组合
//
// 只保留"无条件安全"的 beta token。以下几个 Anthropic 会做联动校验，
// 不带对应 tool / payload 时会 400，必须按请求体内容条件附加，不能塞进默认集：
//   - skills-2025-10-02        → 必须随附 code_execution 工具
//   - advisor-tool-2026-03-01  → 必须随附 advisor 工具定义
//   - tool-search-tool-*       → 必须随附 tool_search 工具
//
// 未来通过 augmentBetasForBody（若引入）按 body 探测后补齐。
var OAuthBetaHeader = strings.Join([]string{
	BetaClaudeCode,
	BetaOAuth,
	BetaInterleavedThinking,
	BetaFineGrainedToolStreaming,
	BetaContextManagement,
	BetaPromptCachingScope,
}, ",")

// HaikuBetaHeader Haiku 模型 OAuth 账号默认 beta 组合（不带 claude-code / skills）
var HaikuBetaHeader = strings.Join([]string{
	BetaOAuth,
	BetaInterleavedThinking,
}, ",")

// APIKeyBetaHeader 非 Haiku 模型 API Key 账号默认 beta 组合
var APIKeyBetaHeader = strings.Join([]string{
	BetaClaudeCode,
	BetaInterleavedThinking,
	BetaFineGrainedToolStreaming,
	BetaContextManagement,
	BetaPromptCachingScope,
}, ",")

// APIKeyHaikuBetaHeader Haiku 模型 API Key 账号默认 beta（最小集）
var APIKeyHaikuBetaHeader = BetaInterleavedThinking

// CountTokensBetaHeader count_tokens 请求使用的 beta 组合
var CountTokensBetaHeader = strings.Join([]string{
	BetaClaudeCode,
	BetaOAuth,
	BetaInterleavedThinking,
	BetaTokenCounting,
}, ",")

// ──────────────────────────────────────────────────────
// DefaultHeaders — 2.1.112 Bun 运行时 wire-format
// ──────────────────────────────────────────────────────

// DefaultHeaders Claude Code 2.1.112 默认请求头（OAuth 模式伪装）
// key 保留真实 CLI wire-format 大小写（来自 binary 抓包）
var DefaultHeaders = map[string]string{
	"User-Agent":                                "claude-cli/" + ClaudeCliVersion + " (external, cli)",
	"X-Stainless-Lang":                          "js",
	"X-Stainless-Package-Version":               StainlessPackageVersion,
	"X-Stainless-OS":                            "Linux",
	"X-Stainless-Arch":                          "arm64",
	"X-Stainless-Runtime":                       "bun",
	"X-Stainless-Runtime-Version":               BunRuntimeVersion,
	"X-Stainless-Timeout":                       "300",
	"x-app":                                     "cli",
	"anthropic-dangerous-direct-browser-access": "true",
}

// ──────────────────────────────────────────────────────
// X-Stainless-Retry-Count 加权随机
// ──────────────────────────────────────────────────────

// pickRetryCount 按 97% "0" / 2.5% "1" / 0.5% "2" 分布抽样
// 模拟真实 CLI 重试分布，避免 Retry-Count 永远为 "0" 被识别
func pickRetryCount() string {
	n := rand.Intn(1000)
	switch {
	case n < 970:
		return "0"
	case n < 995:
		return "1"
	default:
		return "2"
	}
}

// ──────────────────────────────────────────────────────
// Wire-format Header 设置
// ──────────────────────────────────────────────────────
//
// Anthropic 会检测 HTTP header 的大小写是否与真实 Claude CLI 一致。
// Go 的 req.Header.Set() 会自动转为 Title-Case（如 "authorization" → "Authorization"），
// 但真实 Claude CLI (Bun) 发送的是小写形式。必须绕过 Go 的规范化。

// setRawHeader 绕过 Go 的 header 大小写规范化，直接设置原始 key
func setRawHeader(h http.Header, key, value string) {
	h.Del(key)
	delete(h, key)
	h[key] = []string{value}
}

// ──────────────────────────────────────────────────────
// 认证头设置
// ──────────────────────────────────────────────────────

// isHaikuModel 判断是否为 Haiku 模型
func isHaikuModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "haiku")
}

// setAnthropicAuthHeaders 根据账号类型和模型设置认证头
func setAnthropicAuthHeaders(req *http.Request, account *sdk.Account, clientHeaders http.Header, model string) {
	switch account.Type {
	case "apikey":
		apiKey := account.Credentials["api_key"]
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("x-api-key", apiKey)

		beta := clientHeaders.Get("anthropic-beta")
		if beta == "" {
			if isHaikuModel(model) {
				beta = APIKeyHaikuBetaHeader
			} else {
				beta = APIKeyBetaHeader
			}
		} else {
			beta = filterDroppedBetas(beta)
		}
		req.Header.Set("anthropic-beta", beta)

	case "oauth", "session_key":
		token := account.Credentials["access_token"]
		setRawHeader(req.Header, "authorization", "Bearer "+token)

		var beta string
		if isHaikuModel(model) {
			beta = HaikuBetaHeader
		} else {
			beta = OAuthBetaHeader
		}
		if clientBeta := clientHeaders.Get("anthropic-beta"); clientBeta != "" {
			beta = mergeBetas(beta, filterDroppedBetas(clientBeta))
		}
		setRawHeader(req.Header, "anthropic-beta", beta)

		// Claude Code 默认伪装头（保持 wire-format 大小写）
		for k, v := range DefaultHeaders {
			if isRawCaseHeader(k) {
				setRawHeader(req.Header, k, v)
			} else {
				req.Header.Set(k, v)
			}
		}

		// Retry-Count 按真实分布随机覆盖（DefaultHeaders 不含，这里动态注入）
		req.Header.Set("X-Stainless-Retry-Count", pickRetryCount())

		setRawHeader(req.Header, "Accept", "application/json")
	}

	// Anthropic SDK 自身设置的 header 全小写
	version := clientHeaders.Get("anthropic-version")
	if version == "" {
		version = DefaultAnthropicVersion
	}
	setRawHeader(req.Header, "anthropic-version", version)
	setRawHeader(req.Header, "content-type", "application/json")
}

// isRawCaseHeader 判断该 header key 是否需要保持原始大小写（小写形式）
func isRawCaseHeader(key string) bool {
	lower := strings.ToLower(key)
	switch lower {
	case "anthropic-dangerous-direct-browser-access", "x-app", "content-type",
		"anthropic-version", "anthropic-beta", "authorization",
		"accept-language", "accept-encoding", "x-client-request-id":
		return true
	}
	return false
}

// filterDroppedBetas 从 beta header 中移除需要丢弃的 beta token
func filterDroppedBetas(beta string) string {
	parts := strings.Split(beta, ",")
	filtered := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		dropped := false
		for _, d := range DroppedBetas {
			if part == d {
				dropped = true
				break
			}
		}
		if !dropped {
			filtered = append(filtered, part)
		}
	}
	return strings.Join(filtered, ",")
}

// mergeBetas 合并两个 beta header，去重
func mergeBetas(base, extra string) string {
	seen := make(map[string]bool)
	parts := make([]string, 0)
	for _, s := range strings.Split(base, ",") {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			parts = append(parts, s)
		}
	}
	for _, s := range strings.Split(extra, ",") {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ",")
}
