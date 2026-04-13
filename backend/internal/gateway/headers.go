package gateway

import (
	"net/http"
	"strings"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// ──────────────────────────────────────────────────────
// Anthropic Beta Header 常量
// ──────────────────────────────────────────────────────

const (
	BetaOAuth                   = "oauth-2025-04-20"
	BetaClaudeCode              = "claude-code-20250219"
	BetaInterleavedThinking     = "interleaved-thinking-2025-05-14"
	BetaFineGrainedToolStreaming = "fine-grained-tool-streaming-2025-05-14"
	BetaTokenCounting           = "token-counting-2024-11-01"
	BetaContext1M               = "context-1m-2025-08-07"
	BetaFastMode                = "fast-mode-2026-02-01"

	DefaultAnthropicVersion = "2023-06-01"
)

// DroppedBetas 转发时从 anthropic-beta header 中移除的 beta token（客户端专用）
var DroppedBetas = []string{BetaContext1M, BetaFastMode}

// OAuthBetaHeader OAuth 账号的默认 beta header
const OAuthBetaHeader = BetaClaudeCode + "," + BetaOAuth + "," + BetaInterleavedThinking + "," + BetaFineGrainedToolStreaming

// APIKeyBetaHeader API Key 账号的默认 beta header
const APIKeyBetaHeader = BetaClaudeCode + "," + BetaInterleavedThinking + "," + BetaFineGrainedToolStreaming

// APIKeyHaikuBetaHeader Haiku 模型在 API Key 账号下的 beta header（不含 oauth / claude-code）
const APIKeyHaikuBetaHeader = BetaInterleavedThinking

// CountTokensBetaHeader count_tokens 请求使用的 beta header
const CountTokensBetaHeader = BetaClaudeCode + "," + BetaOAuth + "," + BetaInterleavedThinking + "," + BetaTokenCounting

// HaikuBetaHeader Haiku 模型使用的 beta header（不需要 claude-code beta）
const HaikuBetaHeader = BetaOAuth + "," + BetaInterleavedThinking

// DefaultHeaders Claude Code 客户端默认请求头（用于 OAuth 模式伪装）
// key 使用真实 Claude CLI wire-format 大小写（来自抓包）
var DefaultHeaders = map[string]string{
	"User-Agent":                                "claude-cli/2.1.22 (external, cli)",
	"X-Stainless-Lang":                          "js",
	"X-Stainless-Package-Version":               "0.70.0",
	"X-Stainless-OS":                            "Linux",
	"X-Stainless-Arch":                          "arm64",
	"X-Stainless-Runtime":                       "node",
	"X-Stainless-Runtime-Version":               "v24.13.0",
	"X-Stainless-Retry-Count":                   "0",
	"X-Stainless-Timeout":                       "600",
	"x-app":                                     "cli",                // 小写（SDK 原始格式）
	"anthropic-dangerous-direct-browser-access": "true",              // 小写（SDK 原始格式）
}

// ──────────────────────────────────────────────────────
// Wire-format Header 设置
// ──────────────────────────────────────────────────────
//
// Anthropic 会检测 HTTP header 的大小写是否与真实 Claude CLI 一致。
// Go 的 req.Header.Set() 会自动转为 Title-Case（如 "authorization" → "Authorization"），
// 但真实 Claude CLI (Node.js) 发送的是小写形式。必须绕过 Go 的规范化。
// 参考 sub2api 的 header_util.go 实现。

// setRawHeader 绕过 Go 的 header 大小写规范化，直接设置原始 key
func setRawHeader(h http.Header, key, value string) {
	// 先删除 Go canonical 形式
	h.Del(key)
	// 再删除 raw key 形式（防止重复）
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
// 使用 wire-format 大小写（与真实 Claude CLI 流量一致）
func setAnthropicAuthHeaders(req *http.Request, account *sdk.Account, clientHeaders http.Header, model string) {
	switch account.Type {
	case "apikey":
		apiKey := account.Credentials["api_key"]
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("x-api-key", apiKey)

		// API Key 模式：使用客户端的 beta header 或默认值
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

	case "oauth", "session_key", "setup_token":
		token := account.Credentials["access_token"]
		// OAuth 使用小写 header key（与真实 Claude CLI 一致）
		setRawHeader(req.Header, "authorization", "Bearer "+token)

		// OAuth 模式：根据模型选择 beta header
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

		// 设置 Claude Code 默认伪装头（保持 wire-format 大小写）
		for k, v := range DefaultHeaders {
			if isRawCaseHeader(k) {
				setRawHeader(req.Header, k, v)
			} else {
				req.Header.Set(k, v)
			}
		}

		// 流式请求加上 helper-method
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
	// Anthropic SDK 自设的 header 使用小写
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

// anthropicAllowedHeaders 允许透传的请求头白名单
var anthropicAllowedHeaders = map[string]bool{
	"anthropic-version": true,
	"anthropic-beta":    true,
	"accept-language":   true,
	"x-request-id":      true,
}

// passAnthropicHeaders 透传白名单中的客户端头（认证头由 setAnthropicAuthHeaders 处理）
func passAnthropicHeaders(src, dst http.Header) {
	for key, values := range src {
		lowerKey := strings.ToLower(key)
		if anthropicAllowedHeaders[lowerKey] {
			for _, v := range values {
				dst.Add(key, v)
			}
		}
	}
}
