package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

func TestExtractAnthropicUsage_MessageStartFallsBackToCachedTokens(t *testing.T) {
	usage := &sdk.Usage{Currency: usageCurrencyUSD}
	var tokens tokenUsage

	data := `{"type":"message_start","message":{"model":"claude-opus-4-7","usage":{"input_tokens":12,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cached_tokens":9,"cache_creation":{"ephemeral_5m_input_tokens":3,"ephemeral_1h_input_tokens":4}}}}`

	extractAnthropicUsage(data, "message_start", usage, &tokens)

	if usage.Model != "claude-opus-4-7" {
		t.Fatalf("model = %q, want claude-opus-4-7", usage.Model)
	}
	requireUsageMetric(t, usage, usageMetricInputTokens, 12)
	requireUsageMetric(t, usage, usageMetricCachedInputTokens, 9)
	requireUsageMetric(t, usage, usageMetricCacheCreationTokens, 7)
	requireUsageMetric(t, usage, usageMetricCacheCreation5mTokens, 3)
	requireUsageMetric(t, usage, usageMetricCacheCreation1hTokens, 4)
	requireUsageMetric(t, usage, usageMetricTotalTokens, 28)
}

func TestExtractAnthropicUsage_MessageDeltaKeepsStartValues(t *testing.T) {
	usage := &sdk.Usage{Currency: usageCurrencyUSD}
	var tokens tokenUsage

	start := `{"type":"message_start","message":{"model":"claude-opus-4-7","usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":6,"cache_creation":{"ephemeral_5m_input_tokens":3,"ephemeral_1h_input_tokens":4}}}}`
	extractAnthropicUsage(start, "message_start", usage, &tokens)

	delta := `{"type":"message_delta","usage":{"input_tokens":0,"output_tokens":1046,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cached_tokens":11,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"reasoning_output_tokens":128}}`
	extractAnthropicUsage(delta, "message_delta", usage, &tokens)

	requireUsageMetric(t, usage, usageMetricInputTokens, 10)
	requireUsageMetric(t, usage, usageMetricCachedInputTokens, 6)
	requireUsageMetric(t, usage, usageMetricCacheCreationTokens, 7)
	requireUsageMetric(t, usage, usageMetricCacheCreation5mTokens, 3)
	requireUsageMetric(t, usage, usageMetricCacheCreation1hTokens, 4)
	requireUsageMetric(t, usage, usageMetricOutputTokens, 1046)
	requireUsageMetric(t, usage, usageMetricReasoningOutputTokens, 128)
	requireUsageMetric(t, usage, usageMetricTotalTokens, 1069)
}

func TestExtractAnthropicUsage_MessageDeltaFallsBackToCachedTokens(t *testing.T) {
	usage := &sdk.Usage{Currency: usageCurrencyUSD}
	var tokens tokenUsage

	start := `{"type":"message_start","message":{"model":"claude-opus-4-7","usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":3,"ephemeral_1h_input_tokens":4}}}}`
	extractAnthropicUsage(start, "message_start", usage, &tokens)

	delta := `{"type":"message_delta","usage":{"output_tokens":42,"cache_read_input_tokens":0,"cached_tokens":11}}`
	extractAnthropicUsage(delta, "message_delta", usage, &tokens)

	requireUsageMetric(t, usage, usageMetricCachedInputTokens, 11)
	requireUsageMetric(t, usage, usageMetricOutputTokens, 42)
	requireUsageMetric(t, usage, usageMetricTotalTokens, 70)
}

func TestHandleNonStreamResponseParsesAnthropicUsageFallbacks(t *testing.T) {
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"model":"claude-opus-4-7","usage":{"input_tokens":123,"output_tokens":456,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cached_tokens":7,"cache_creation":{"ephemeral_5m_input_tokens":5,"ephemeral_1h_input_tokens":2}}}`)),
	}

	rec := httptest.NewRecorder()
	outcome, err := handleNonStreamResponse(resp, rec, time.Now().Add(-250*time.Millisecond))
	if err != nil {
		t.Fatalf("handleNonStreamResponse returned error: %v", err)
	}
	if outcome.Usage == nil {
		t.Fatalf("usage is nil")
	}
	if outcome.Upstream.StatusCode != 200 {
		t.Fatalf("status code = %d, want 200", outcome.Upstream.StatusCode)
	}
	wantBody := `{"model":"claude-opus-4-7","usage":{"input_tokens":123,"output_tokens":456,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cached_tokens":7,"cache_creation":{"ephemeral_5m_input_tokens":5,"ephemeral_1h_input_tokens":2}}}`
	if got := strings.TrimSpace(rec.Body.String()); got != wantBody {
		t.Fatalf("response body = %s, want %s", got, wantBody)
	}

	usage := outcome.Usage
	if usage.Model != "claude-opus-4-7" {
		t.Fatalf("model = %q, want claude-opus-4-7", usage.Model)
	}
	requireUsageMetric(t, usage, usageMetricInputTokens, 123)
	requireUsageMetric(t, usage, usageMetricCachedInputTokens, 7)
	requireUsageMetric(t, usage, usageMetricCacheCreationTokens, 7)
	requireUsageMetric(t, usage, usageMetricCacheCreation5mTokens, 5)
	requireUsageMetric(t, usage, usageMetricCacheCreation1hTokens, 2)
	requireUsageMetric(t, usage, usageMetricOutputTokens, 456)
	if diff := usage.AccountCost - 0.01206975; diff < -1e-9 || diff > 1e-9 {
		t.Fatalf("account cost = %.12f, want %.12f", usage.AccountCost, 0.01206975)
	}
}

func requireUsageMetric(t *testing.T, usage *sdk.Usage, key string, want int) {
	t.Helper()
	if got := usageMetricInt(usage, key); got != want {
		t.Fatalf("%s = %d, want %d", key, got, want)
	}
}

// fakeStreamBody 用 channel 模拟上游响应体：每次 Read 阻塞直到有数据或 ctx 取消，
// 行为对齐真实 net 连接——请求 ctx 取消时 Read 立即返回 ctx 错误。
type fakeStreamBody struct {
	ctx context.Context
	ch  chan []byte
	buf []byte
}

func (b *fakeStreamBody) Read(p []byte) (int, error) {
	for len(b.buf) == 0 {
		select {
		case <-b.ctx.Done():
			return 0, b.ctx.Err()
		case chunk, ok := <-b.ch:
			if !ok {
				return 0, io.EOF
			}
			b.buf = chunk
		}
	}
	n := copy(p, b.buf)
	b.buf = b.buf[n:]
	return n, nil
}

func (b *fakeStreamBody) Close() error { return nil }

type streamResult struct {
	outcome sdk.ForwardOutcome
	err     error
}

// TestHandleStreamResponse_ActiveLongStreamNotAborted 回归：只要上游持续吐字节，
// 即便总耗时超过 idle 阈值也不应被读空闲守卫掐断（流一旦开始就别因"耗时长"中止）。
func TestHandleStreamResponse_ActiveLongStreamNotAborted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	body := &fakeStreamBody{ctx: ctx, ch: make(chan []byte, 1)}
	resp := &http.Response{StatusCode: 200, Body: body}
	rec := httptest.NewRecorder()

	const idle = 250 * time.Millisecond
	const chunks = 10
	const interval = 40 * time.Millisecond // 间隔远小于 idle，持续输出

	go func() {
		for i := 0; i < chunks; i++ {
			time.Sleep(interval)
			body.ch <- []byte("data: {\"type\":\"content_block_delta\"}\n\n")
		}
		close(body.ch) // 正常收尾 → EOF
	}()

	done := make(chan streamResult, 1)
	go func() {
		o, e := handleStreamResponse(resp, rec, time.Now(), idle, cancel)
		done <- streamResult{o, e}
	}()

	select {
	case r := <-done:
		// 活跃时长(~400ms) > idle(250ms)，但每次间隔 << idle，不应被掐断
		if r.err != nil {
			t.Fatalf("活跃长流被误判中断: err=%v reason=%s", r.err, r.outcome.Reason)
		}
		if r.outcome.Kind != sdk.OutcomeSuccess {
			t.Fatalf("Kind = %v, want OutcomeSuccess", r.outcome.Kind)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handleStreamResponse 未在预期时间内返回")
	}
}

// TestHandleStreamResponse_StalledStreamAborted 回归：上游持续静默超过 idle（真正卡死）
// 时，读空闲守卫应取消上游 ctx 并中止，返回 OutcomeStreamAborted。
func TestHandleStreamResponse_StalledStreamAborted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	body := &fakeStreamBody{ctx: ctx, ch: make(chan []byte, 2)}
	resp := &http.Response{StatusCode: 200, Body: body}
	rec := httptest.NewRecorder()

	const idle = 80 * time.Millisecond

	// 先发两条，随后彻底静默（既不发也不关闭）→ 触发读空闲中止
	body.ch <- []byte("data: {\"type\":\"message_start\"}\n\n")
	body.ch <- []byte("data: {\"type\":\"content_block_delta\"}\n\n")

	done := make(chan streamResult, 1)
	startedAt := time.Now()
	go func() {
		o, e := handleStreamResponse(resp, rec, time.Now(), idle, cancel)
		done <- streamResult{o, e}
	}()

	select {
	case r := <-done:
		if r.err == nil {
			t.Fatal("卡死流应返回错误")
		}
		if r.outcome.Kind != sdk.OutcomeStreamAborted {
			t.Fatalf("Kind = %v, want OutcomeStreamAborted", r.outcome.Kind)
		}
		if !strings.Contains(r.outcome.Reason, "卡死") {
			t.Fatalf("reason 应标注卡死中止，实际: %s", r.outcome.Reason)
		}
		if elapsed := time.Since(startedAt); elapsed > time.Second {
			t.Fatalf("卡死检测耗时过长: %v", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("卡死流未被中止")
	}
}

// TestStreamAbortedOutcomeFillsUsageCost 回归：流中断（上游挂起超时等）时，
// message_start 已产生的输入/缓存写入 token 上游已实际计费，
// streamAbortedOutcome 必须补填费用，否则落库为"有 token、金额全 0"的漏计费记录。
func TestStreamAbortedOutcomeFillsUsageCost(t *testing.T) {
	usage := newTokenUsage("claude-fable-5", tokenUsage{
		inputTokens:         2,
		outputTokens:        53,
		cacheCreationTokens: 41900,
	}, 0)

	outcome := streamAbortedOutcome(200, "读取上游 SSE 失败: timeout", usage)

	if outcome.Kind != sdk.OutcomeStreamAborted {
		t.Fatalf("Kind = %v, want OutcomeStreamAborted", outcome.Kind)
	}
	if outcome.Usage == nil {
		t.Fatal("中断 outcome 应携带 Usage")
	}
	if outcome.Usage.AccountCost <= 0 {
		t.Fatalf("中断前已产生的用量必须计费，AccountCost = %v", outcome.Usage.AccountCost)
	}
}
