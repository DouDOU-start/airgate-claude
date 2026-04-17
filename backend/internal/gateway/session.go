package gateway

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

// ──────────────────────────────────────────────────────
// Session 身份：UUIDv4 + 30 min sticky 复用
// ──────────────────────────────────────────────────────
//
// 真实 Claude CLI 在同一会话内多次请求会复用同一 metadata.user_id；
// 每次都重新生成会被 Anthropic 识别为非 CLI 流量。
// 这里以 accountID + messages 前缀哈希为 key，TTL 30 min。

const sessionTTL = 30 * time.Minute

// newUUIDv4 生成符合 RFC 4122 的 UUIDv4 字符串（小写带连字符）
func newUUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	// version 4
	b[6] = (b[6] & 0x0f) | 0x40
	// variant RFC 4122
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// sessionEntry sticky 会话条目
type sessionEntry struct {
	userID    string
	expiresAt time.Time
}

// sessionCache accountID+conversation 指纹 → 固定 user_id
type sessionCache struct {
	mu      sync.Mutex
	entries map[string]sessionEntry
}

var defaultSessionCache = &sessionCache{entries: make(map[string]sessionEntry)}

// conversationFingerprint 以 messages 数组的稳定切片计算指纹
// 使用首条消息 + 倒数第二条消息（保持多轮对话一致，对末尾问句不敏感）
func conversationFingerprint(body []byte) string {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return ""
	}
	arr := msgs.Array()
	if len(arr) == 0 {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(arr[0].Raw))
	if len(arr) >= 2 {
		// 倒数第二条（最后一条通常是本次问句，每轮都不同）
		h.Write([]byte(arr[len(arr)-2].Raw))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// stickyUserID 返回当前请求对应的 metadata.user_id 段（仅 session UUID 部分）
// 命中缓存返回已有值；未命中生成新 UUIDv4 并写入。
func (c *sessionCache) stickyUserID(accountID int64, fingerprint string) string {
	if fingerprint == "" {
		// 无法生成指纹（如空会话）→ 退化为每次新 UUID
		return newUUIDv4()
	}
	key := fmt.Sprintf("%d:%s", accountID, fingerprint)
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	// 清理过期项（低频访问时顺带 GC）
	if len(c.entries) > 1024 {
		for k, e := range c.entries {
			if now.After(e.expiresAt) {
				delete(c.entries, k)
			}
		}
	}

	if e, ok := c.entries[key]; ok && now.Before(e.expiresAt) {
		return e.userID
	}

	id := newUUIDv4()
	c.entries[key] = sessionEntry{userID: id, expiresAt: now.Add(sessionTTL)}
	return id
}
