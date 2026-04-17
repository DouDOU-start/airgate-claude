package gateway

import (
	"net/http"
	"testing"
)

func TestValidateClaudeCodeRequest(t *testing.T) {
	cliHeaders := func() http.Header {
		h := http.Header{}
		h.Set("User-Agent", "claude-cli/2.1.112 (external, cli)")
		h.Set("X-App", "cli")
		h.Set("anthropic-beta", "oauth-2025-04-20")
		h.Set("anthropic-version", "2023-06-01")
		return h
	}

	validUserID := "user_a1b2c3d4-e5f6-4789-abcd-ef0123456789_account_fedcba98-7654-4321-0fed-cba987654321_session_01234567-89ab-4cde-8f01-23456789abcd"

	tests := []struct {
		name    string
		path    string
		body    []byte
		headers http.Header
		wantOK  bool
	}{
		{
			name:    "internal test request bypasses validator",
			path:    "/v1/messages",
			body:    []byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":"hi"}],"stream":true}`),
			headers: http.Header{"Content-Type": {"application/json"}, "X-Airgate-Internal": {"test"}},
			wantOK:  true,
		},
		{
			name:    "valid CC /messages request",
			path:    "/v1/messages",
			body:    []byte(`{"model":"claude-sonnet-4-5-20250929","max_tokens":100,"metadata":{"user_id":"` + validUserID + `"},"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}]}`),
			headers: cliHeaders(),
			wantOK:  true,
		},
		{
			name:    "bad UA",
			path:    "/v1/messages",
			body:    []byte(`{}`),
			headers: http.Header{"User-Agent": []string{"python-requests/2.31"}},
			wantOK:  false,
		},
		{
			name:    "missing UA entirely",
			path:    "/v1/messages",
			body:    []byte(`{}`),
			headers: http.Header{},
			wantOK:  false,
		},
		{
			name:    "/v1/models — UA-only sufficient",
			path:    "/v1/models",
			body:    nil,
			headers: cliHeaders(),
			wantOK:  true,
		},
		{
			name:    "haiku startup probe bypasses body checks",
			path:    "/v1/messages",
			body:    []byte(`{"model":"claude-haiku-4-5-20251001","max_tokens":1}`),
			headers: cliHeaders(),
			wantOK:  true,
		},
		{
			name:    "missing metadata.user_id",
			path:    "/v1/messages",
			body:    []byte(`{"model":"claude-sonnet-4-5","max_tokens":100,"system":"You are Claude Code, Anthropic's official CLI for Claude."}`),
			headers: cliHeaders(),
			wantOK:  false,
		},
		{
			name:    "malformed user_id",
			path:    "/v1/messages",
			body:    []byte(`{"metadata":{"user_id":"some-random-id"},"system":"You are Claude Code, Anthropic's official CLI for Claude."}`),
			headers: cliHeaders(),
			wantOK:  false,
		},
		{
			name:    "unrelated system prompt",
			path:    "/v1/messages",
			body:    []byte(`{"metadata":{"user_id":"` + validUserID + `"},"system":"You are a helpful pirate, arrrr."}`),
			headers: cliHeaders(),
			wantOK:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := validateClaudeCodeRequest(tc.path, tc.body, tc.headers)
			if got.OK != tc.wantOK {
				t.Errorf("OK = %v, want %v (reason=%q)", got.OK, tc.wantOK, got.Reason)
			}
		})
	}
}

func TestDiceCoefficient(t *testing.T) {
	tests := []struct {
		a, b  string
		wantGe float64 // 期望 ≥ 此值
	}{
		{"You are Claude Code, Anthropic's official CLI for Claude.",
			"You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK.",
			0.5},
		{"You are Claude Code, Anthropic's official CLI for Claude.", "", 0},
		{"abc", "xyz", 0},
	}
	for _, tc := range tests {
		got := diceCoefficient(tc.a, tc.b)
		if got < tc.wantGe {
			t.Errorf("diceCoefficient(%q,%q) = %v, want ≥ %v", tc.a, tc.b, got, tc.wantGe)
		}
	}
}

func TestSanitizeBody_StripEmptyTextAndMidThinking(t *testing.T) {
	in := []byte(`{"messages":[
		{"role":"user","content":[{"type":"text","text":""},{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[{"type":"thinking","thinking":"..."},{"type":"text","text":"ok"}]}
	]}`)

	out := sanitizeBody(in)
	// 第一条 message 的空 text 应被剥离
	// 第二条 message 的中间 thinking（不在末位）应被剥离
	got := string(out)
	if !contains(got, `"text":"hi"`) {
		t.Errorf("expected 'hi' text to survive; got: %s", got)
	}
	if contains(got, `"text":""`) {
		t.Errorf("expected empty text block to be stripped; got: %s", got)
	}
	if contains(got, `"thinking":"..."`) {
		t.Errorf("expected mid-position thinking block to be stripped; got: %s", got)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && findSubstring(s, sub))
}

func findSubstring(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
