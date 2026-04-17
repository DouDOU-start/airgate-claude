package gateway

import (
	"encoding/json"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ──────────────────────────────────────────────────────
// 请求体净化（移植自 sub2api gateway_request.go 的 Strip/Filter 组合）
//
// Anthropic 对上游请求有若干"合法性红线"，第三方客户端常命中，转发后
// 会直接 400，在我们这里先过滤掉：
//   1. 空 text block 会被拒（sub2api: StripEmptyTextBlocks）
//   2. thinking block 不能在 assistant 回合末尾以外的位置出现
//      （sub2api: FilterThinkingBlocks / FilterThinkingBlocksForRetry）
// ──────────────────────────────────────────────────────

// sanitizeBody 对 messages 数组做净化，返回可能被重写的 body
// 不修改输入；净化后的结果为新 byte slice
func sanitizeBody(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return body
	}

	changed := false
	newMessages := make([]json.RawMessage, 0, len(msgs.Array()))

	for _, msg := range msgs.Array() {
		content := msg.Get("content")
		// content 为 string 格式：直接透传，非空才保留
		if content.Type == gjson.String {
			if content.String() == "" {
				// 空 string content 会被上游拒，跳过
				changed = true
				continue
			}
			newMessages = append(newMessages, json.RawMessage(msg.Raw))
			continue
		}
		if !content.IsArray() {
			newMessages = append(newMessages, json.RawMessage(msg.Raw))
			continue
		}

		// content 为 array：逐块过滤
		role := msg.Get("role").String()
		filtered := filterContentBlocks(content.Array(), role)
		if len(filtered) == 0 {
			// 整条消息净化后为空：丢弃
			changed = true
			continue
		}
		if len(filtered) != len(content.Array()) {
			changed = true
			newRaw, err := sjson.SetRawBytes([]byte(msg.Raw), "content", rawJSONArray(filtered))
			if err != nil {
				newMessages = append(newMessages, json.RawMessage(msg.Raw))
				continue
			}
			newMessages = append(newMessages, json.RawMessage(newRaw))
			continue
		}
		newMessages = append(newMessages, json.RawMessage(msg.Raw))
	}

	if !changed {
		return body
	}

	newBody, err := sjson.SetBytes(body, "messages", newMessages)
	if err != nil {
		return body
	}
	return newBody
}

// filterContentBlocks 对单条 message 的 content 数组做块级过滤
//
//	role == "assistant" 且非末位的 thinking 块 → 丢弃
//	所有位置的空 text 块              → 丢弃
//	其它块                           → 保留
//
// 对 thinking 的保留策略保留真实官方 CLI 行为：assistant 末位可以有 thinking。
func filterContentBlocks(blocks []gjson.Result, role string) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(blocks))
	for i, b := range blocks {
		t := b.Get("type").String()

		// 空 text 块：所有位置都剥离
		if t == "text" {
			if b.Get("text").String() == "" {
				continue
			}
		}

		// thinking 块：assistant 只允许在末位
		if t == "thinking" || t == "redacted_thinking" {
			if role != "assistant" {
				// 非 assistant 角色不应该有 thinking，删掉
				continue
			}
			if i != len(blocks)-1 {
				// assistant 中间位置的 thinking 上游会 400
				continue
			}
		}

		out = append(out, json.RawMessage(b.Raw))
	}
	return out
}

// rawJSONArray 把 []json.RawMessage 拼回 JSON 数组字节（sjson.SetRawBytes 需要完整值）
func rawJSONArray(items []json.RawMessage) []byte {
	buf, _ := json.Marshal(items)
	return buf
}
