package gateway

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// ──────────────────────────────────────────────────────
// SSE 流式响应透传 + usage 提取
// ──────────────────────────────────────────────────────

// handleStreamResponse 透传 Anthropic SSE 流式响应，同时提取 usage 信息
func handleStreamResponse(resp *http.Response, w http.ResponseWriter, start time.Time) (*sdk.ForwardResult, error) {
	// 设置 SSE 响应头
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)

	result := &sdk.ForwardResult{
		StatusCode: resp.StatusCode,
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()

		// 写入到客户端
		if _, err := fmt.Fprintf(w, "%s\n", line); err != nil {
			break
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}

		// 提取 SSE data 行中的 usage 信息
		data, ok := extractSSEData(line)
		if !ok || data == "" {
			continue
		}

		// 从 message_start 事件提取 input_tokens
		// 从 message_delta 事件提取 output_tokens
		extractAnthropicUsage(data, result)
	}

	if err := scanner.Err(); err != nil {
		result.Duration = time.Since(start)
		return result, fmt.Errorf("读取上游 SSE 失败: %w", err)
	}

	result.Duration = time.Since(start)
	return result, nil
}

// handleNonStreamResponse 处理非流式响应
func handleNonStreamResponse(resp *http.Response, w http.ResponseWriter, start time.Time) (*sdk.ForwardResult, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取上游响应失败: %w", err)
	}

	// 提取 usage 信息
	inputTokens := int(gjson.GetBytes(body, "usage.input_tokens").Int())
	outputTokens := int(gjson.GetBytes(body, "usage.output_tokens").Int())
	cacheTokens := int(gjson.GetBytes(body, "usage.cache_read_input_tokens").Int())
	model := gjson.GetBytes(body, "model").String()

	if w != nil {
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
	}

	return &sdk.ForwardResult{
		StatusCode:   resp.StatusCode,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		CacheTokens:  cacheTokens,
		Model:        model,
		Duration:     time.Since(start),
		Body:         body,
		Headers:      resp.Header,
	}, nil
}

// extractSSEData 从 SSE 行中提取 data 字段的内容
func extractSSEData(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "data:") {
		return "", false
	}
	data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	return data, true
}

// extractAnthropicUsage 从 Anthropic SSE data 中提取 usage 信息
func extractAnthropicUsage(data string, result *sdk.ForwardResult) {
	eventType := gjson.Get(data, "type").String()

	switch eventType {
	case "message_start":
		// message_start 包含初始 usage（input_tokens）
		result.InputTokens = int(gjson.Get(data, "message.usage.input_tokens").Int())
		result.CacheTokens = int(gjson.Get(data, "message.usage.cache_read_input_tokens").Int())
		result.Model = gjson.Get(data, "message.model").String()

	case "message_delta":
		// message_delta 包含最终 output_tokens
		result.OutputTokens = int(gjson.Get(data, "usage.output_tokens").Int())
	}
}
