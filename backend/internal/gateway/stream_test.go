package gateway

import (
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
