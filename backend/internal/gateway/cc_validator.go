package gateway

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

// ──────────────────────────────────────────────────────
// Claude Code 客户端识别
// 移植自 sub2api backend/internal/service/claude_code_validator.go
// （原作者再上游是 claude-relay-service）
//
// 作用：
//   在 OAuth / Session Key 账号开启 claude_code_only 后，只有"真正看起来
//   像官方 Claude Code CLI"的请求才被放行，其余返回 403。用于避免把 OAuth
//   额度暴露给任意客户端后被 Anthropic 行为模型识别。
//
// 识别维度：
//   1. UA 必须匹配 `claude-cli/{version}` 正则
//   2. /messages 请求额外要求：
//      - 必要 header 非空（X-App / anthropic-beta / anthropic-version）
//      - metadata.user_id 形如 user_{uuid}_account_{uuid}_session_{uuid}
//      - system prompt 与 6 条官方 Claude Code 标准 prompt 的 Dice 系数 ≥ 0.5
//   3. max_tokens=1 的 Haiku 探测请求直接放行（CLI 的 startup probe）
// ──────────────────────────────────────────────────────

// claudeCliUARegex CC 客户端 User-Agent 正则（不区分大小写）
var claudeCliUARegex = regexp.MustCompile(`(?i)^claude-cli/\d+\.\d+\.\d+`)

// metadataUserIDRegex metadata.user_id 的标准格式
// 真实 CLI 形如：user_{uuid}_account_{uuid}_session_{uuid}
var metadataUserIDRegex = regexp.MustCompile(`^user_[0-9a-f-]{8,}_account_[0-9a-fA-F-]{8,}_session_[0-9a-f-]{8,}$`)

// canonicalCCSystemPrompts Claude Code 的标准 system prompt 全集
// Dice 相似度 ≥ 0.5 即视为合法
var canonicalCCSystemPrompts = []string{
	// CLI 主体
	"You are Claude Code, Anthropic's official CLI for Claude.",
	"You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK.",
	// Agent SDK
	"You are a Claude agent, built on Anthropic's Claude Agent SDK.",
	// 总结 / compact 场景
	"You are a helpful AI assistant tasked with summarizing conversations.",
	// Context management 场景
	"You are an AI assistant helping manage a long-running conversation's context.",
	// Fallback（兜底）
	"You are Claude, a large language model trained by Anthropic.",
}

// ──────────────────────────────────────────────────────
// 公共入口
// ──────────────────────────────────────────────────────

// ccValidationResult 校验结果
type ccValidationResult struct {
	OK     bool
	Reason string // 失败原因（调试日志 / 响应）
}

// validateClaudeCodeRequest 对一次 OAuth forward 请求做 CC 客户端身份判定
//
// path: 真实请求路径（/v1/messages、/v1/messages/count_tokens、/v1/models）
// body: 请求体（可为空）
// clientHeaders: 客户端发来的 header（未被网关改写前）
func validateClaudeCodeRequest(path string, body []byte, clientHeaders http.Header) ccValidationResult {
	// 0. 内部流量豁免：Core 自家的测试 / 探测请求带 X-Airgate-Internal，
	//    直接放行，避免管理后台"测试账号"功能被自家 CC 闸误拦。
	if clientHeaders.Get("X-Airgate-Internal") != "" {
		return ccValidationResult{OK: true}
	}

	// 1. UA 检查（所有路径都要求）
	ua := clientHeaders.Get("User-Agent")
	if ua == "" {
		// Go 规范化后可能在小写版本
		ua = clientHeaders.Get("user-agent")
	}
	if !claudeCliUARegex.MatchString(ua) {
		return ccValidationResult{OK: false, Reason: "user-agent mismatch"}
	}

	// 2. 非 /v1/messages 路径 UA 合格即放行
	if !strings.HasSuffix(path, "/v1/messages") {
		return ccValidationResult{OK: true}
	}

	// 3. Haiku + max_tokens=1 = CLI 的 startup probe，放行
	if isStartupProbe(body) {
		return ccValidationResult{OK: true}
	}

	// 4. 必要 header 存在性（真实 CLI 总是带）
	for _, k := range []string{"X-App", "x-app", "anthropic-beta", "anthropic-version"} {
		if clientHeaders.Get(k) != "" {
			goto headersOK
		}
	}
	return ccValidationResult{OK: false, Reason: "missing required CC headers"}

headersOK:
	if clientHeaders.Get("anthropic-version") == "" {
		return ccValidationResult{OK: false, Reason: "missing anthropic-version"}
	}
	if clientHeaders.Get("anthropic-beta") == "" {
		return ccValidationResult{OK: false, Reason: "missing anthropic-beta"}
	}
	// X-App 两种大小写都算
	if clientHeaders.Get("X-App") == "" && clientHeaders.Get("x-app") == "" {
		return ccValidationResult{OK: false, Reason: "missing x-app"}
	}

	// 5. metadata.user_id 格式校验
	userID := gjson.GetBytes(body, "metadata.user_id").String()
	if userID == "" {
		return ccValidationResult{OK: false, Reason: "missing metadata.user_id"}
	}
	if !metadataUserIDRegex.MatchString(userID) {
		return ccValidationResult{OK: false, Reason: "metadata.user_id format mismatch"}
	}

	// 6. system prompt 相似度
	if !systemPromptMatchesCC(body) {
		return ccValidationResult{OK: false, Reason: "system prompt mismatch"}
	}

	return ccValidationResult{OK: true}
}

// isStartupProbe 判断是否为 CLI 的启动探测（max_tokens=1 + haiku 模型）
// sub2api 的放行规则，我们自家的 postRefreshProbe 也会命中这里
func isStartupProbe(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	model := strings.ToLower(gjson.GetBytes(body, "model").String())
	if !strings.Contains(model, "haiku") {
		return false
	}
	maxTokens := gjson.GetBytes(body, "max_tokens")
	return maxTokens.Exists() && maxTokens.Int() == 1
}

// ──────────────────────────────────────────────────────
// system prompt 相似度判定
// ──────────────────────────────────────────────────────

// systemPromptMatchesCC 提取请求体中所有 system text，任一与标准 prompt
// Dice 系数 ≥ 0.5 则判合法
func systemPromptMatchesCC(body []byte) bool {
	texts := extractSystemTexts(body)
	if len(texts) == 0 {
		return false
	}
	for _, text := range texts {
		for _, canon := range canonicalCCSystemPrompts {
			if diceCoefficient(text, canon) >= 0.5 {
				return true
			}
		}
	}
	return false
}

// extractSystemTexts 兼容 system 的两种形态：
//   - string："You are Claude Code..."
//   - array： [{type:"text", text:"...", cache_control:...}]
func extractSystemTexts(body []byte) []string {
	sys := gjson.GetBytes(body, "system")
	if !sys.Exists() {
		return nil
	}
	if sys.Type == gjson.String {
		return []string{sys.String()}
	}
	if !sys.IsArray() {
		return nil
	}
	out := make([]string, 0, 2)
	for _, item := range sys.Array() {
		if t := item.Get("text").String(); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// diceCoefficient 计算两段文本的 Sørensen–Dice 系数（基于小写 bigram 集合）
//
//	dice = 2|A ∩ B| / (|A| + |B|)
//
// 空串或短于 2 字符的退化情况返回 0。
func diceCoefficient(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	lowerA := strings.ToLower(a)
	lowerB := strings.ToLower(b)
	if lowerA == lowerB {
		return 1
	}
	setA := bigramSet(lowerA)
	setB := bigramSet(lowerB)
	if len(setA) == 0 || len(setB) == 0 {
		return 0
	}
	inter := 0
	for k := range setA {
		if _, ok := setB[k]; ok {
			inter++
		}
	}
	return 2 * float64(inter) / float64(len(setA)+len(setB))
}

// bigramSet 把字符串切成 rune 粒度 bigram 集合
func bigramSet(s string) map[string]struct{} {
	runes := []rune(s)
	if len(runes) < 2 {
		return nil
	}
	out := make(map[string]struct{}, len(runes)-1)
	for i := 0; i+2 <= len(runes); i++ {
		out[string(runes[i:i+2])] = struct{}{}
	}
	return out
}

// ──────────────────────────────────────────────────────
// 响应构造
// ──────────────────────────────────────────────────────

// ccRejectBody 返回 Anthropic 原生格式的 403 错误体
// 模拟上游响应格式，客户端无需改造即可识别
func ccRejectBody(reason string) []byte {
	return jsonMarshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "permission_error",
			"message": "This endpoint only accepts traffic from the official Claude Code CLI (" + reason + ").",
		},
	})
}
