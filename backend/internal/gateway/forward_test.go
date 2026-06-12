package gateway

import (
	"testing"

	"github.com/tidwall/gjson"
)

// TestPreprocessAPIKeyBody_NoLossyRewrite 锁定 API Key 路径的无损/最小化预处理不变量：
// 不注入 cache_control、不剥离 thinking、保留反斜杠转义，仅补 max_tokens 默认值。
// 这些正是历史上误用 OAuth 预处理导致工具调用 400/500 的根因点。
func TestPreprocessAPIKeyBody_NoLossyRewrite(t *testing.T) {
	t.Run("不给 tools 注入 cache_control，补 max_tokens，保留 Windows 路径反斜杠", func(t *testing.T) {
		// 末项 tool 无 cache_control；text 含 Windows 路径（JSON 转义为 \\）。
		body := []byte(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":[{"type":"text","text":"open C:\\Users\\2024\\a.txt"}]}],"tools":[{"name":"a"},{"name":"b"}]}`)

		out := preprocessAPIKeyBody(body)

		if got := gjson.GetBytes(out, "max_tokens").Int(); got != 4096 {
			t.Fatalf("max_tokens = %d, want 4096", got)
		}
		if gjson.GetBytes(out, "tools.1.cache_control").Exists() {
			t.Fatalf("末项 tool 不应被注入 cache_control（会与客户端 1h TTL 冲突 → 400）")
		}
		// 反斜杠未被 json 往返破坏：gjson 解码应还原出原始 Windows 路径。
		if got := gjson.GetBytes(out, "messages.0.content.0.text").String(); got != `open C:\Users\2024\a.txt` {
			t.Fatalf("反斜杠转义被破坏：text = %q", got)
		}
	})

	t.Run("不剥离 tool_use 之前的 thinking 块，已存在的 max_tokens 不改", func(t *testing.T) {
		body := []byte(`{"model":"m","max_tokens":100,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"s"},{"type":"tool_use","id":"t1","name":"a","input":{}}]}]}`)

		out := preprocessAPIKeyBody(body)

		if got := gjson.GetBytes(out, "max_tokens").Int(); got != 100 {
			t.Fatalf("已存在的 max_tokens 被改写：max_tokens = %d, want 100", got)
		}
		blocks := gjson.GetBytes(out, "messages.0.content").Array()
		if len(blocks) != 2 {
			t.Fatalf("assistant content 块数 = %d, want 2（thinking 不应被剥离）", len(blocks))
		}
		if t0 := blocks[0].Get("type").String(); t0 != "thinking" {
			t.Fatalf("首块 type = %q, want thinking", t0)
		}
		if t1 := blocks[1].Get("type").String(); t1 != "tool_use" {
			t.Fatalf("次块 type = %q, want tool_use", t1)
		}
	})

	t.Run("空 body 原样返回", func(t *testing.T) {
		if out := preprocessAPIKeyBody(nil); out != nil {
			t.Fatalf("空 body 应原样返回 nil，得到 %q", out)
		}
	})
}
