package gateway

import (
	"encoding/json"
	"sort"
	"strings"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// Spec 单个模型的完整元数据
type Spec struct {
	Name                 string  // 展示名称
	ContextWindow        int     // 上下文窗口（tokens）
	MaxOutputTokens      int     // 最大输出 tokens
	InputPrice           float64 // 输入价格（$/1M tokens）
	CachedPrice          float64 // 缓存读取价格（$/1M tokens，0.1x input）
	CacheCreationPrice   float64 // 缓存写入价格 5m TTL（$/1M tokens，1.25x input）
	CacheCreation1hPrice float64 // 缓存写入价格 1h TTL（$/1M tokens，2.00x input）
	OutputPrice          float64 // 输出价格（$/1M tokens）
}

// modelRegistry 全局模型注册表（价格对齐 Anthropic 官方 2026-04 定价）
// 字段顺序：Name, ContextWindow, MaxOutputTokens, InputPrice, CachedPrice, CacheCreationPrice, CacheCreation1hPrice, OutputPrice
// 价格单位：美元 / 百万 token
// CachedPrice          = Cache Read / Hits & Refreshes（0.1x base input）
// CacheCreationPrice   = Cache Write 5m TTL（1.25x base input）
// CacheCreation1hPrice = Cache Write 1h TTL（2.00x base input）
var modelRegistry = map[string]Spec{
	// Opus — input $5 / cache_read $0.50 / write_5m $6.25 / write_1h $10 / output $25
	"claude-opus-4-7":          {"Claude Opus 4.7", 1000000, 128000, 5.0, 0.5, 6.25, 10.0, 25.0},
	"claude-opus-4-6":          {"Claude Opus 4.6", 1000000, 128000, 5.0, 0.5, 6.25, 10.0, 25.0},
	"claude-opus-4-5-20251101": {"Claude Opus 4.5", 200000, 64000, 5.0, 0.5, 6.25, 10.0, 25.0},
	"claude-opus-4-1-20250805": {"Claude Opus 4.1", 200000, 32000, 15.0, 1.5, 18.75, 30.0, 75.0},
	// Sonnet — input $3 / cache_read $0.30 / write_5m $3.75 / write_1h $6 / output $15
	"claude-sonnet-4-6":          {"Claude Sonnet 4.6", 1000000, 64000, 3.0, 0.3, 3.75, 6.0, 15.0},
	"claude-sonnet-4-5-20250929": {"Claude Sonnet 4.5", 200000, 64000, 3.0, 0.3, 3.75, 6.0, 15.0},
	"claude-sonnet-4-20250514":   {"Claude Sonnet 4", 200000, 64000, 3.0, 0.3, 3.75, 6.0, 15.0},
	// Haiku — input $1 / cache_read $0.10 / write_5m $1.25 / write_1h $2 / output $5
	"claude-haiku-4-5-20251001": {"Claude Haiku 4.5", 200000, 64000, 1.0, 0.1, 1.25, 2.0, 5.0},
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
			ID:                   id,
			Name:                 spec.Name,
			ContextWindow:        spec.ContextWindow,
			MaxOutputTokens:      spec.MaxOutputTokens,
			InputPrice:           spec.InputPrice,
			OutputPrice:          spec.OutputPrice,
			CachedInputPrice:     spec.CachedPrice,
			CacheCreationPrice:   spec.CacheCreationPrice,
			CacheCreation1hPrice: spec.CacheCreation1hPrice,
		})
	}
	sort.Slice(models, func(i, j int) bool {
		return models[i].ID < models[j].ID
	})
	return models
}

// fallbackModel 兜底模型（未知模型按 Sonnet 4.6 计费，最常用的中端模型）
var fallbackModel = sdk.ModelInfo{
	ID:                   "claude-sonnet-4-6",
	Name:                 "Claude Sonnet 4.6 (fallback)",
	ContextWindow:        1000000,
	MaxOutputTokens:      64000,
	InputPrice:           3.0,
	OutputPrice:          15.0,
	CachedInputPrice:     0.3,
	CacheCreationPrice:   3.75,
	CacheCreation1hPrice: 6.0,
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
		if spec, ok := modelRegistry["claude-opus-4-7"]; ok {
			return specToModelInfo("claude-opus-4-7", spec)
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
		ID:                   id,
		Name:                 spec.Name,
		ContextWindow:        spec.ContextWindow,
		MaxOutputTokens:      spec.MaxOutputTokens,
		InputPrice:           spec.InputPrice,
		OutputPrice:          spec.OutputPrice,
		CachedInputPrice:     spec.CachedPrice,
		CacheCreationPrice:   spec.CacheCreationPrice,
		CacheCreation1hPrice: spec.CacheCreation1hPrice,
	}
}

// fillUsageCost 根据 Usage 中的 token 数和模型价格填充费用 / 单价字段。
// 未知 model 会通过 LookupModel 兜底到 Sonnet 价格。
func fillUsageCost(usage *sdk.Usage) {
	if usage == nil || usage.Model == "" {
		return
	}
	model := LookupModel(usage.Model)
	if model == nil {
		return
	}

	cost := sdk.CalculateCost(sdk.CostInput{
		InputTokens:           usage.InputTokens,
		OutputTokens:          usage.OutputTokens,
		CachedInputTokens:     usage.CachedInputTokens,
		CacheCreationTokens:   usage.CacheCreationTokens,
		CacheCreation5mTokens: usage.CacheCreation5mTokens,
		CacheCreation1hTokens: usage.CacheCreation1hTokens,
		ServiceTier:           usage.ServiceTier,
	}, *model)

	usage.InputCost = cost.InputCost
	usage.OutputCost = cost.OutputCost
	usage.CachedInputCost = cost.CachedInputCost
	usage.CacheCreationCost = cost.CacheCreationCost
	usage.InputPrice = model.InputPrice
	usage.OutputPrice = model.OutputPrice
	usage.CachedInputPrice = model.CachedInputPrice
	usage.CacheCreationPrice = model.CacheCreationPrice
	usage.CacheCreation1hPrice = model.CacheCreation1hPrice
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
	{ID: "claude-opus-4-7", Type: "model", DisplayName: "Claude Opus 4.7", CreatedAt: "2026-04-15T00:00:00Z"},
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
