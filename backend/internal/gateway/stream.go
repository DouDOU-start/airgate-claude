package gateway

import (
	"bufio"
	"context"
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

// upstreamSSEMaxLineBytes 是上游 SSE 单行最大字节数。
// 大工具入参（写大文件 / review 整段代码等）的 input_json_delta 单行可能远超 1 MB，
// 过小会触发 bufio.Scanner: token too long 中断流 → tool_use 入参被截断 →
// 客户端报 "Invalid tool parameters"。与 airgate-openai 对齐取 8 MB。
const upstreamSSEMaxLineBytes = 8 * 1024 * 1024

// stallGuardReader 包装上游响应体，实现"读空闲"判活：只要持续读到字节就续命，
// 仅当连续 idle 时长内没有任何新数据时才取消上游请求 ctx，使阻塞中的读取立即返回。
// 目的：中断"真正卡死"的流，但绝不打断仍在持续输出的长响应（流耗时再长也不掐断）。
type stallGuardReader struct {
	rc     io.Reader
	cancel context.CancelFunc
	idle   time.Duration

	mu      sync.Mutex
	timer   *time.Timer
	stalled bool
	stopped bool
}

// newStallGuardReader 创建读空闲守卫。idle <= 0 时不启用（退化为透明包装）。
func newStallGuardReader(rc io.Reader, idle time.Duration, cancel context.CancelFunc) *stallGuardReader {
	g := &stallGuardReader{rc: rc, cancel: cancel, idle: idle}
	if idle > 0 && cancel != nil {
		g.timer = time.AfterFunc(idle, g.onIdle)
	}
	return g
}

func (g *stallGuardReader) onIdle() {
	g.mu.Lock()
	if g.stopped {
		g.mu.Unlock()
		return
	}
	g.stalled = true
	g.mu.Unlock()
	g.cancel()
}

func (g *stallGuardReader) Read(p []byte) (int, error) {
	n, err := g.rc.Read(p)
	if n > 0 {
		g.mu.Lock()
		if g.timer != nil && !g.stopped && !g.stalled {
			g.timer.Reset(g.idle)
		}
		g.mu.Unlock()
	}
	return n, err
}

// stalledFlag 报告是否因读空闲超时主动中止（用于区分 reason 文案）。
func (g *stallGuardReader) stalledFlag() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stalled
}

// stop 解除守卫（流读完或函数返回时调用），停止计时器避免泄漏。
func (g *stallGuardReader) stop() {
	g.mu.Lock()
	g.stopped = true
	g.mu.Unlock()
	if g.timer != nil {
		g.timer.Stop()
	}
}

// handleStreamResponse 透传 Anthropic SSE 流，同时累加 usage 字段。
//
// 超时语义：流式不按"总耗时"掐断——只要上游持续吐字节就一直透传；仅当连续 idle 时长
// 内无任何新数据（真正卡死）才经 cancel 中止。流中途断开返回 OutcomeStreamAborted
// （字节已开写，Core 不会 failover）。
func handleStreamResponse(resp *http.Response, w http.ResponseWriter, start time.Time, idle time.Duration, cancel context.CancelFunc) (sdk.ForwardOutcome, error) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)

	usage := &sdk.Usage{Currency: usageCurrencyUSD}
	var tokens tokenUsage
	var firstTokenOnce sync.Once

	guard := newStallGuardReader(resp.Body, idle, cancel)
	defer guard.stop()

	scanner := bufio.NewScanner(guard)
	scanner.Buffer(make([]byte, 64*1024), upstreamSSEMaxLineBytes)

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
		reason := err.Error()
		if guard.stalledFlag() {
			reason = fmt.Sprintf("上游流连续 %s 无数据，判定卡死中止: %v", idle, err)
		}
		return streamAbortedOutcome(resp.StatusCode, reason, usage), fmt.Errorf("读取上游 SSE 失败: %w", err)
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
