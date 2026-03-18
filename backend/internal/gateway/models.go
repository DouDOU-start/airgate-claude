package gateway

import (
	"encoding/json"
	"sort"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// Spec 单个模型的完整元数据
type Spec struct {
	Name            string  // 展示名称
	ContextWindow   int     // 上下文窗口（tokens）
	MaxOutputTokens int     // 最大输出 tokens
	InputPrice      float64 // 输入价格（$/1M tokens）
	CachedPrice     float64 // 缓存输入价格（$/1M tokens）
	OutputPrice     float64 // 输出价格（$/1M tokens）
}

// modelRegistry 全局模型注册表
// 字段顺序：Name, ContextWindow, MaxOutputTokens, InputPrice, CachedPrice, OutputPrice
var modelRegistry = map[string]Spec{
	"claude-opus-4-6":            {"Claude Opus 4.6", 200000, 32000, 15.0, 1.5, 75.0},
	"claude-opus-4-5-20251101":   {"Claude Opus 4.5", 200000, 32000, 15.0, 1.5, 75.0},
	"claude-sonnet-4-6":          {"Claude Sonnet 4.6", 200000, 64000, 3.0, 0.3, 15.0},
	"claude-sonnet-4-5-20250929": {"Claude Sonnet 4.5", 200000, 64000, 3.0, 0.3, 15.0},
	"claude-haiku-4-5-20251001":  {"Claude Haiku 4.5", 200000, 8192, 0.8, 0.08, 4.0},
}

// ModelIDOverrides 短名到长名的映射
var ModelIDOverrides = map[string]string{
	"claude-sonnet-4-5": "claude-sonnet-4-5-20250929",
	"claude-opus-4-5":   "claude-opus-4-5-20251101",
	"claude-haiku-4-5":  "claude-haiku-4-5-20251001",
}

// ModelIDReverseOverrides 长名到短名的映射
var ModelIDReverseOverrides = map[string]string{
	"claude-sonnet-4-5-20250929": "claude-sonnet-4-5",
	"claude-opus-4-5-20251101":   "claude-opus-4-5",
	"claude-haiku-4-5-20251001":  "claude-haiku-4-5",
}

// NormalizeModelID 将短模型名映射为完整 ID
func NormalizeModelID(id string) string {
	if mapped, ok := ModelIDOverrides[id]; ok {
		return mapped
	}
	return id
}

// AllModelSpecs 返回所有注册模型的 SDK ModelInfo 列表
func AllModelSpecs() []sdk.ModelInfo {
	models := make([]sdk.ModelInfo, 0, len(modelRegistry))
	for id, spec := range modelRegistry {
		models = append(models, sdk.ModelInfo{
			ID:          id,
			Name:        spec.Name,
			MaxTokens:   spec.ContextWindow,
			InputPrice:  spec.InputPrice,
			OutputPrice: spec.OutputPrice,
			CachePrice:  spec.CachedPrice,
		})
	}
	sort.Slice(models, func(i, j int) bool {
		return models[i].ID < models[j].ID
	})
	return models
}

// claudeModelListEntry Anthropic /v1/models 接口返回的单个模型
type claudeModelListEntry struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

// defaultModelList 默认模型列表（Anthropic API 格式）
var defaultModelList = []claudeModelListEntry{
	{ID: "claude-opus-4-6", Type: "model", DisplayName: "Claude Opus 4.6", CreatedAt: "2026-02-06T00:00:00Z"},
	{ID: "claude-opus-4-5-20251101", Type: "model", DisplayName: "Claude Opus 4.5", CreatedAt: "2025-11-01T00:00:00Z"},
	{ID: "claude-sonnet-4-6", Type: "model", DisplayName: "Claude Sonnet 4.6", CreatedAt: "2026-02-18T00:00:00Z"},
	{ID: "claude-sonnet-4-5-20250929", Type: "model", DisplayName: "Claude Sonnet 4.5", CreatedAt: "2025-09-29T00:00:00Z"},
	{ID: "claude-haiku-4-5-20251001", Type: "model", DisplayName: "Claude Haiku 4.5", CreatedAt: "2025-10-01T00:00:00Z"},
}

// buildModelsResponse 构建 Anthropic /v1/models 响应体
func buildModelsResponse() []byte {
	resp := map[string]any{
		"data":     defaultModelList,
		"has_more": false,
		"first_id": defaultModelList[0].ID,
		"last_id":  defaultModelList[len(defaultModelList)-1].ID,
	}
	b, _ := json.Marshal(resp)
	return b
}
