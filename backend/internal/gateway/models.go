package gateway

import (
	"encoding/json"
	"sort"
	"strings"

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
// modelRegistry 全局模型注册表（价格对齐 Anthropic 官方 2026-04 定价）
// 字段顺序：Name, ContextWindow, MaxOutputTokens, InputPrice, CachedPrice, OutputPrice
// 价格单位：美元 / 百万 token
// CachedPrice = Cache Hits & Refreshes（0.1x base input）
var modelRegistry = map[string]Spec{
	// Opus — $5 / $0.50 / $25
	"claude-opus-4-6":          {"Claude Opus 4.6", 1000000, 128000, 5.0, 0.5, 25.0},
	"claude-opus-4-5-20251101": {"Claude Opus 4.5", 200000, 64000, 5.0, 0.5, 25.0},
	"claude-opus-4-1-20250805": {"Claude Opus 4.1", 200000, 32000, 15.0, 1.5, 75.0},
	// Sonnet — $3 / $0.30 / $15
	"claude-sonnet-4-6":          {"Claude Sonnet 4.6", 1000000, 64000, 3.0, 0.3, 15.0},
	"claude-sonnet-4-5-20250929": {"Claude Sonnet 4.5", 200000, 64000, 3.0, 0.3, 15.0},
	"claude-sonnet-4-20250514":   {"Claude Sonnet 4", 200000, 64000, 3.0, 0.3, 15.0},
	// Haiku — $1 / $0.10 / $5
	"claude-haiku-4-5-20251001": {"Claude Haiku 4.5", 200000, 64000, 1.0, 0.1, 5.0},
}

// ModelIDOverrides 短名到长名的映射
var ModelIDOverrides = map[string]string{
	"claude-sonnet-4-5": "claude-sonnet-4-5-20250929",
	"claude-sonnet-4-0": "claude-sonnet-4-20250514",
	"claude-opus-4-5":   "claude-opus-4-5-20251101",
	"claude-opus-4-1":   "claude-opus-4-1-20250805",
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
			ID:               id,
			Name:             spec.Name,
			ContextWindow:    spec.ContextWindow,
			MaxOutputTokens:  spec.MaxOutputTokens,
			InputPrice:       spec.InputPrice,
			OutputPrice:      spec.OutputPrice,
			CachedInputPrice: spec.CachedPrice,
		})
	}
	sort.Slice(models, func(i, j int) bool {
		return models[i].ID < models[j].ID
	})
	return models
}

// fallbackModel 兜底模型（未知模型按 Sonnet 4.6 计费，最常用的中端模型）
var fallbackModel = sdk.ModelInfo{
	ID:               "claude-sonnet-4-6",
	Name:             "Claude Sonnet 4.6 (fallback)",
	ContextWindow:    1000000,
	MaxOutputTokens:  64000,
	InputPrice:       3.0,
	OutputPrice:      15.0,
	CachedInputPrice: 0.3,
}

// LookupModel 查找模型元数据，未知模型返回兜底模型
func LookupModel(modelID string) *sdk.ModelInfo {
	// 精确匹配
	if spec, ok := modelRegistry[modelID]; ok {
		return specToModelInfo(modelID, spec)
	}
	// 规范化后匹配
	normalized := NormalizeModelID(modelID)
	if spec, ok := modelRegistry[normalized]; ok {
		return specToModelInfo(normalized, spec)
	}
	// 前缀模糊匹配（如 claude-opus-4-6-xxx 匹配 claude-opus-4-6）
	for id, spec := range modelRegistry {
		if strings.HasPrefix(modelID, id) {
			return specToModelInfo(id, spec)
		}
	}
	// 关键词匹配（从模型名推断系列）
	lower := strings.ToLower(modelID)
	switch {
	case strings.Contains(lower, "opus"):
		if spec, ok := modelRegistry["claude-opus-4-6"]; ok {
			return specToModelInfo("claude-opus-4-6", spec)
		}
	case strings.Contains(lower, "haiku"):
		if spec, ok := modelRegistry["claude-haiku-4-5-20251001"]; ok {
			return specToModelInfo("claude-haiku-4-5-20251001", spec)
		}
	case strings.Contains(lower, "sonnet"):
		if spec, ok := modelRegistry["claude-sonnet-4-6"]; ok {
			return specToModelInfo("claude-sonnet-4-6", spec)
		}
	}
	// 兜底：按 Sonnet 4.6 计费
	fb := fallbackModel
	return &fb
}

func specToModelInfo(id string, spec Spec) *sdk.ModelInfo {
	return &sdk.ModelInfo{
		ID:               id,
		Name:             spec.Name,
		ContextWindow:    spec.ContextWindow,
		MaxOutputTokens:  spec.MaxOutputTokens,
		InputPrice:       spec.InputPrice,
		OutputPrice:      spec.OutputPrice,
		CachedInputPrice: spec.CachedPrice,
	}
}

// fillCost 根据 ForwardResult 中的 token 数和模型价格填充费用字段
func fillCost(result *sdk.ForwardResult) {
	modelID := result.Model
	if modelID == "" {
		return
	}
	model := LookupModel(modelID)
	if model == nil {
		return
	}

	cost := sdk.CalculateCost(sdk.CostInput{
		InputTokens:       result.InputTokens,
		OutputTokens:      result.OutputTokens,
		CachedInputTokens: result.CachedInputTokens,
		ServiceTier:       result.ServiceTier,
	}, *model)

	result.InputCost = cost.InputCost
	result.OutputCost = cost.OutputCost
	result.CachedInputCost = cost.CachedInputCost
	result.InputPrice = model.InputPrice
	result.OutputPrice = model.OutputPrice
	result.CachedInputPrice = model.CachedInputPrice
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
	// Latest
	{ID: "claude-opus-4-6", Type: "model", DisplayName: "Claude Opus 4.6", CreatedAt: "2026-02-06T00:00:00Z"},
	{ID: "claude-sonnet-4-6", Type: "model", DisplayName: "Claude Sonnet 4.6", CreatedAt: "2026-02-18T00:00:00Z"},
	{ID: "claude-haiku-4-5-20251001", Type: "model", DisplayName: "Claude Haiku 4.5", CreatedAt: "2025-10-01T00:00:00Z"},
	// Legacy
	{ID: "claude-opus-4-5-20251101", Type: "model", DisplayName: "Claude Opus 4.5", CreatedAt: "2025-11-01T00:00:00Z"},
	{ID: "claude-opus-4-1-20250805", Type: "model", DisplayName: "Claude Opus 4.1", CreatedAt: "2025-08-05T00:00:00Z"},
	{ID: "claude-sonnet-4-5-20250929", Type: "model", DisplayName: "Claude Sonnet 4.5", CreatedAt: "2025-09-29T00:00:00Z"},
	{ID: "claude-sonnet-4-20250514", Type: "model", DisplayName: "Claude Sonnet 4", CreatedAt: "2025-05-14T00:00:00Z"},
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
