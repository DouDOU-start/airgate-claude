package gateway

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

// SSE 流式响应透传 + usage 提取。调用者保证 resp.StatusCode 是 2xx（4xx/5xx 由 handleErrorResponse 处理）。

// handleStreamResponse 透传 Anthropic SSE 流，同时累加 usage 字段。
// 流中途断开时返回 OutcomeStreamAborted（字节已开写，Core 不会 failover）。
func handleStreamResponse(resp *http.Response, w http.ResponseWriter, start time.Time) (sdk.ForwardOutcome, error) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)

	usage := &sdk.Usage{Currency: usageCurrencyUSD}
	var tokens tokenUsage
	var firstTokenOnce sync.Once

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if _, err := fmt.Fprintf(w, "%s\n", line); err != nil {
			break
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}

		data, ok := extractSSEData(line)
		if !ok || data == "" {
			continue
		}
		eventType := gjson.Get(data, "type").String()

		if eventType == "content_block_delta" {
			firstTokenOnce.Do(func() {
				usage.FirstTokenMs = time.Since(start).Milliseconds()
			})
		}
		extractAnthropicUsage(data, eventType, usage, &tokens)
	}

	elapsed := time.Since(start)
	if err := scanner.Err(); err != nil {
		return streamAbortedOutcome(resp.StatusCode, err.Error(), usage), fmt.Errorf("读取上游 SSE 失败: %w", err)
	}

	fillUsageCost(usage)
	return sdk.ForwardOutcome{
		Kind:     sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{StatusCode: resp.StatusCode},
		Usage:    usage,
		Duration: elapsed,
	}, nil
}

// handleNonStreamResponse 处理非流式响应。resp.StatusCode 预设 2xx。
func handleNonStreamResponse(resp *http.Response, w http.ResponseWriter, start time.Time) (sdk.ForwardOutcome, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		reason := fmt.Sprintf("读取上游响应失败: %v", err)
		return transientOutcome(reason), fmt.Errorf("%s", reason)
	}

	var tokens tokenUsage
	applyAnthropicUsageNode(gjson.GetBytes(body, "usage"), &tokens, true)
	usage := newTokenUsage(gjson.GetBytes(body, "model").String(), tokens, time.Since(start).Milliseconds())
	fillUsageCost(usage)

	headers := resp.Header.Clone()
	if w != nil {
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
	}

	outcome := sdk.ForwardOutcome{
		Kind:     sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{StatusCode: resp.StatusCode, Headers: headers},
		Usage:    usage,
		Duration: time.Since(start),
	}
	if w == nil {
		outcome.Upstream.Body = body
	}
	return outcome, nil
}

// extractSSEData 从 SSE 行中提取 data 字段的内容。
func extractSSEData(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "data:") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")), true
}

// extractAnthropicUsage 把 Anthropic SSE data 中的 usage 字段累加到通用 Usage。
func extractAnthropicUsage(data string, eventType string, usage *sdk.Usage, tokens *tokenUsage) {
	if usage == nil || tokens == nil {
		return
	}
	switch eventType {
	case "message_start":
		usage.Model = gjson.Get(data, "message.model").String()
		setUsageModelAttribute(usage, usage.Model)
		if applyAnthropicUsageNode(gjson.Get(data, "message.usage"), tokens, true) {
			setUsageTokens(usage, *tokens)
		}

	case "message_delta":
		if applyAnthropicUsageNode(gjson.Get(data, "usage"), tokens, false) {
			setUsageTokens(usage, *tokens)
		}
	}
}

func applyAnthropicUsageNode(node gjson.Result, tokens *tokenUsage, allowZero bool) bool {
	if tokens == nil || !node.Exists() {
		return false
	}
	updated := false

	if v := node.Get("input_tokens"); v.Exists() {
		if allowZero || v.Int() > 0 {
			tokens.inputTokens = int(v.Int())
			updated = true
		}
	}
	if v := node.Get("output_tokens"); v.Exists() {
		if allowZero || v.Int() > 0 {
			tokens.outputTokens = int(v.Int())
			updated = true
		}
	}
	if v := node.Get("cache_read_input_tokens"); v.Exists() {
		if allowZero || v.Int() > 0 {
			tokens.cachedInputTokens = int(v.Int())
			updated = true
		}
	}
	if v := node.Get("cache_creation_input_tokens"); v.Exists() {
		if allowZero || v.Int() > 0 {
			tokens.cacheCreationTokens = int(v.Int())
			updated = true
		}
	}
	if v := node.Get("cache_creation.ephemeral_5m_input_tokens"); v.Exists() {
		if allowZero || v.Int() > 0 {
			tokens.cacheCreation5mTokens = int(v.Int())
			updated = true
		}
	}
	if v := node.Get("cache_creation.ephemeral_1h_input_tokens"); v.Exists() {
		if allowZero || v.Int() > 0 {
			tokens.cacheCreation1hTokens = int(v.Int())
			updated = true
		}
	}
	if v := node.Get("reasoning_output_tokens"); v.Exists() {
		if allowZero || v.Int() > 0 {
			tokens.reasoningOutputTokens = int(v.Int())
			updated = true
		}
	}

	if tokens.cachedInputTokens == 0 {
		if v := node.Get("cached_tokens"); v.Exists() && v.Int() > 0 {
			tokens.cachedInputTokens = int(v.Int())
			updated = true
		}
	}
	if tokens.cacheCreationTokens == 0 {
		cc5m := int(node.Get("cache_creation.ephemeral_5m_input_tokens").Int())
		cc1h := int(node.Get("cache_creation.ephemeral_1h_input_tokens").Int())
		if cc5m > 0 || cc1h > 0 {
			tokens.cacheCreationTokens = cc5m + cc1h
			updated = true
		}
	}

	return updated
}
