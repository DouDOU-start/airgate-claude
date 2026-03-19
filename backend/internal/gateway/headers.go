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
	"X-App":                                     "cli",
	"Anthropic-Dangerous-Direct-Browser-Access": "true",
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

	case "oauth", "session_key":
		token := account.Credentials["access_token"]
		req.Header.Set("Authorization", "Bearer "+token)

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
		req.Header.Set("anthropic-beta", beta)

		// 设置 Claude Code 默认伪装头
		for k, v := range DefaultHeaders {
			req.Header.Set(k, v)
		}
	}

	// 设置 anthropic-version
	version := clientHeaders.Get("anthropic-version")
	if version == "" {
		version = DefaultAnthropicVersion
	}
	req.Header.Set("anthropic-version", version)

	req.Header.Set("Content-Type", "application/json")
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
