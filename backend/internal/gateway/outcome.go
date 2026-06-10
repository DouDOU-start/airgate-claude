package gateway

import (
	"fmt"
	"net/http"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

// 构造 ForwardOutcome 的 helper，让各转发路径不重复写 struct literal。

// successOutcome 构造 Success 判决。Usage 由调用方填；Duration 调用方填。
func successOutcome(statusCode int, body []byte, headers http.Header, usage *sdk.Usage) sdk.ForwardOutcome {
	return sdk.ForwardOutcome{
		Kind: sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{
			StatusCode: statusCode,
			Headers:    headers,
			Body:       body,
		},
		Usage: usage,
	}
}

// failureOutcome 从 HTTP 状态码 + 错误消息分类并构造非 Success 的 Outcome。
// 会保留 Upstream（Body/Headers/StatusCode）供 Core 在 ClientError 路径下透传。
func failureOutcome(statusCode int, body []byte, headers http.Header, message string, retryAfter time.Duration) sdk.ForwardOutcome {
	kind := classifyHTTPFailure(statusCode, message)
	reason := message
	if reason != "" {
		reason = fmt.Sprintf("HTTP %d: %s", statusCode, message)
	}
	return sdk.ForwardOutcome{
		Kind: kind,
		Upstream: sdk.UpstreamResponse{
			StatusCode: statusCode,
			Headers:    headers,
			Body:       body,
		},
		Reason:     reason,
		RetryAfter: retryAfter,
	}
}

// transientOutcome 网络层 / 连接失败（无上游 HTTP 响应），归类为 UpstreamTransient。
func transientOutcome(reason string) sdk.ForwardOutcome {
	return sdk.ForwardOutcome{
		Kind:     sdk.OutcomeUpstreamTransient,
		Upstream: sdk.UpstreamResponse{StatusCode: http.StatusBadGateway},
		Reason:   reason,
	}
}

// accountDeadOutcome 账号级确定性失败（凭证缺失 / 账号类型异常等）。核心会把账号打入 disabled。
func accountDeadOutcome(reason string) sdk.ForwardOutcome {
	return sdk.ForwardOutcome{
		Kind:     sdk.OutcomeAccountDead,
		Upstream: sdk.UpstreamResponse{StatusCode: http.StatusUnauthorized},
		Reason:   reason,
	}
}

// streamAbortedOutcome 流式响应已开写、中途断开。
//
// 中断前已产生的用量（message_start 的输入/缓存写入 token 等）上游已实际计费，
// 必须在此补填费用，否则会落一条"有 token、金额全 0"的漏计费记录。
func streamAbortedOutcome(statusCode int, reason string, usage *sdk.Usage) sdk.ForwardOutcome {
	fillUsageCost(usage)
	return sdk.ForwardOutcome{
		Kind: sdk.OutcomeStreamAborted,
		Upstream: sdk.UpstreamResponse{
			StatusCode: statusCode,
		},
		Reason: reason,
		Usage:  usage,
	}
}
