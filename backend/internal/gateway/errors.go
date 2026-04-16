package gateway

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"
	"github.com/tidwall/gjson"
)

// accountStatusFromCode 根据 HTTP 状态码推断账号状态
func accountStatusFromCode(statusCode int) sdk.AccountStatus {
	switch statusCode {
	case 429:
		return sdk.AccountStatusRateLimited
	case 401:
		return sdk.AccountStatusExpired
	case 403:
		return sdk.AccountStatusDisabled
	default:
		return sdk.AccountStatusOK
	}
}

// accountStatusFromBody 从响应体推断账号状态。
// Anthropic 某些账号级错误（如组织被封禁）走 400，accountStatusFromCode 无法识别，
// 需额外检查 error.message 内容。
func accountStatusFromBody(statusCode int, body []byte) sdk.AccountStatus {
	base := accountStatusFromCode(statusCode)
	if base != sdk.AccountStatusOK || statusCode != 400 {
		return base
	}
	msg := strings.ToLower(gjson.GetBytes(body, "error.message").String())
	if strings.Contains(msg, "disabled") || strings.Contains(msg, "deactivated") || strings.Contains(msg, "suspended") {
		return sdk.AccountStatusDisabled
	}
	return base
}

// extractErrorMessage 从 Anthropic JSON 错误响应中提取 error.type + error.message
func extractErrorMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	errType := gjson.GetBytes(body, "error.type").String()
	errMsg := gjson.GetBytes(body, "error.message").String()
	if errType != "" && errMsg != "" {
		return errType + ": " + errMsg
	}
	if errMsg != "" {
		return errMsg
	}
	if errType != "" {
		return errType
	}
	return ""
}

// extractRetryAfterHeader 从响应头提取 Retry-After 时间
func extractRetryAfterHeader(headers http.Header) time.Duration {
	val := headers.Get("Retry-After")
	if val == "" {
		return 0
	}
	// 尝试解析为秒数
	if seconds, err := strconv.Atoi(val); err == nil {
		return time.Duration(seconds) * time.Second
	}
	// 尝试从错误消息中提取延迟
	return parseRetryDelay(val)
}

var retryDelayPattern = regexp.MustCompile(`(\d+)\s*s`)

// parseRetryDelay 从文本中提取重试延迟秒数
func parseRetryDelay(text string) time.Duration {
	matches := retryDelayPattern.FindStringSubmatch(text)
	if len(matches) >= 2 {
		if seconds, err := strconv.Atoi(matches[1]); err == nil {
			return time.Duration(seconds) * time.Second
		}
	}
	return 0
}

// truncate 截断字符串到指定长度
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// jsonError 返回 JSON 格式的错误消息
func jsonError(msg string) []byte {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return b
}

// jsonMarshal 将对象序列化为 JSON
func jsonMarshal(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}
