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

// classifyHTTPFailure 根据 HTTP 状态码 + 错误文本归一化为 OutcomeKind。
//
//	429              → AccountRateLimited
//	401 / 403        → AccountDead
//	400 含账号级关键字（disabled / deactivated / suspended）→ AccountDead
//	5xx              → UpstreamTransient
//	其它 4xx         → ClientError
func classifyHTTPFailure(statusCode int, message string) sdk.OutcomeKind {
	if statusCode == 400 && isDisabledAccountText(message) {
		return sdk.OutcomeAccountDead
	}
	switch statusCode {
	case 429:
		return sdk.OutcomeAccountRateLimited
	case 401, 403:
		return sdk.OutcomeAccountDead
	}
	if statusCode >= 500 {
		return sdk.OutcomeUpstreamTransient
	}
	if statusCode >= 400 {
		return sdk.OutcomeClientError
	}
	return sdk.OutcomeSuccess
}

// classifyAnthropicBody 从 Anthropic 错误响应体归一化 OutcomeKind（处理 400 账号级错误）。
func classifyAnthropicBody(statusCode int, body []byte) sdk.OutcomeKind {
	msg := gjson.GetBytes(body, "error.message").String()
	if msg == "" {
		msg = string(body)
	}
	return classifyHTTPFailure(statusCode, msg)
}

func isDisabledAccountText(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "disabled") ||
		strings.Contains(lower, "deactivated") ||
		strings.Contains(lower, "suspended")
}

// extractErrorMessage 从 Anthropic JSON 错误响应中提取 error.type + error.message。
func extractErrorMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	errType := gjson.GetBytes(body, "error.type").String()
	errMsg := gjson.GetBytes(body, "error.message").String()
	switch {
	case errType != "" && errMsg != "":
		return errType + ": " + errMsg
	case errMsg != "":
		return errMsg
	case errType != "":
		return errType
	}
	return ""
}

// extractRetryAfterHeader 从响应头提取 Retry-After。
func extractRetryAfterHeader(headers http.Header) time.Duration {
	val := headers.Get("Retry-After")
	if val == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(val); err == nil {
		return time.Duration(seconds) * time.Second
	}
	return parseRetryDelay(val)
}

var retryDelayPattern = regexp.MustCompile(`(\d+)\s*s`)

// parseRetryDelay 从文本中提取重试延迟秒数。
func parseRetryDelay(text string) time.Duration {
	matches := retryDelayPattern.FindStringSubmatch(text)
	if len(matches) >= 2 {
		if seconds, err := strconv.Atoi(matches[1]); err == nil {
			return time.Duration(seconds) * time.Second
		}
	}
	return 0
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func jsonError(msg string) []byte {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return b
}

func jsonMarshal(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}
