package gateway

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
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

const (
	usageCurrencyUSD = "USD"

	usageMetaCacheCreation5mTokens = "claude.cache_creation_5m_tokens"
	usageMetaCacheCreation1hTokens = "claude.cache_creation_1h_tokens"
	usageMetaCacheCreation1hPrice  = "claude.cache_creation_1h_price"
)

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
	for _, item := range AllPricingSpecs() {
		models = append(models, specToModelInfo(item.ID, item.Spec))
	}
	return models
}

// NamedSpec 是带模型 ID 的插件私有规格。
type NamedSpec struct {
	ID   string
	Spec Spec
}

// AllPricingSpecs 返回带价格的插件私有模型规格，用于 manifest 生成和计费。
func AllPricingSpecs() []NamedSpec {
	items := make([]NamedSpec, 0, len(modelRegistry))
	for id, spec := range modelRegistry {
		items = append(items, NamedSpec{ID: id, Spec: spec})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].ID < items[j].ID
	})
	return items
}

// fallbackModel 兜底模型（未知模型按 Sonnet 4.6 计费，最常用的中端模型）
var fallbackSpec = Spec{
	Name:                 "Claude Sonnet 4.6",
	ContextWindow:        1000000,
	MaxOutputTokens:      64000,
	InputPrice:           3.0,
	CachedPrice:          0.3,
	CacheCreationPrice:   3.75,
	CacheCreation1hPrice: 6.0,
	OutputPrice:          15.0,
}

// LookupModelSpec 查找模型计费规格，未知模型返回兜底规格。
func LookupModelSpec(modelID string) (string, Spec) {
	// 精确匹配
	if spec, ok := modelRegistry[modelID]; ok {
		return modelID, spec
	}
	// 规范化后匹配
	normalized := NormalizeModelID(modelID)
	if spec, ok := modelRegistry[normalized]; ok {
		return normalized, spec
	}
	// 前缀模糊匹配（如 claude-opus-4-6-xxx 匹配 claude-opus-4-6）
	for id, spec := range modelRegistry {
		if strings.HasPrefix(modelID, id) {
			return id, spec
		}
	}
	// 关键词匹配（从模型名推断系列）
	lower := strings.ToLower(modelID)
	switch {
	case strings.Contains(lower, "opus"):
		if spec, ok := modelRegistry["claude-opus-4-7"]; ok {
			return "claude-opus-4-7", spec
		}
	case strings.Contains(lower, "haiku"):
		if spec, ok := modelRegistry["claude-haiku-4-5-20251001"]; ok {
			return "claude-haiku-4-5-20251001", spec
		}
	case strings.Contains(lower, "sonnet"):
		if spec, ok := modelRegistry["claude-sonnet-4-6"]; ok {
			return "claude-sonnet-4-6", spec
		}
	}
	// 兜底：按 Sonnet 4.6 计费
	return "claude-sonnet-4-6", fallbackSpec
}

func specToModelInfo(id string, spec Spec) sdk.ModelInfo {
	return sdk.ModelInfo{
		ID:              id,
		Name:            spec.Name,
		ContextWindow:   spec.ContextWindow,
		MaxOutputTokens: spec.MaxOutputTokens,
		Capabilities:    []string{sdk.ModelCapChat, sdk.ModelCapReasoning},
	}
}

type tokenUsage struct {
	inputTokens           int
	outputTokens          int
	cachedInputTokens     int
	cacheCreationTokens   int
	cacheCreation5mTokens int
	cacheCreation1hTokens int
	reasoningOutputTokens int
}

func newTokenUsage(modelID string, tokens tokenUsage, firstTokenMs int64) *sdk.Usage {
	usage := &sdk.Usage{
		Model:        modelID,
		Currency:     usageCurrencyUSD,
		FirstTokenMs: firstTokenMs,
	}
	setUsageTokens(usage, tokens)
	return usage
}

func setUsageTokens(usage *sdk.Usage, tokens tokenUsage) {
	if usage == nil {
		return
	}
	usage.InputTokens = tokens.inputTokens
	usage.OutputTokens = tokens.outputTokens
	usage.CachedInputTokens = tokens.cachedInputTokens
	usage.CacheCreationTokens = tokens.cacheCreationTokens
	usage.ReasoningOutputTokens = tokens.reasoningOutputTokens
	setUsageMetadataInt(usage, usageMetaCacheCreation5mTokens, tokens.cacheCreation5mTokens)
	setUsageMetadataInt(usage, usageMetaCacheCreation1hTokens, tokens.cacheCreation1hTokens)
}

func usageCacheCreation5mTokens(usage *sdk.Usage) int {
	return int(usageMetadataFloat(usage, usageMetaCacheCreation5mTokens))
}

func usageCacheCreation1hTokens(usage *sdk.Usage) int {
	return int(usageMetadataFloat(usage, usageMetaCacheCreation1hTokens))
}

func usageTotalTokens(usage *sdk.Usage) int {
	if usage == nil {
		return 0
	}
	return usage.InputTokens + usage.CachedInputTokens + usage.CacheCreationTokens + usage.OutputTokens
}

func setUsageMetadata(usage *sdk.Usage, key, value string) {
	if usage == nil {
		return
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	if usage.Metadata == nil {
		usage.Metadata = map[string]string{}
	}
	usage.Metadata[key] = value
}

func setUsageMetadataInt(usage *sdk.Usage, key string, value int) {
	if value <= 0 {
		return
	}
	setUsageMetadata(usage, key, strconv.Itoa(value))
}

func setUsageMetadataFloat(usage *sdk.Usage, key string, value float64) {
	if value <= 0 {
		return
	}
	setUsageMetadata(usage, key, strconv.FormatFloat(value, 'f', -1, 64))
}

func usageMetadataFloat(usage *sdk.Usage, key string) float64 {
	if usage == nil {
		return 0
	}
	raw := strings.TrimSpace(usage.Metadata[key])
	if raw == "" {
		return 0
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0
	}
	return value
}

func recomputeUsageAccountCost(usage *sdk.Usage) {
	if usage == nil {
		return
	}
	total := usage.InputCost + usage.OutputCost + usage.CachedInputCost + usage.CacheCreationCost
	usage.AccountCost = total
	if usage.Currency == "" {
		usage.Currency = usageCurrencyUSD
	}
}

func tokenCost(tokens int, pricePerMillion float64) float64 {
	if tokens <= 0 || pricePerMillion <= 0 {
		return 0
	}
	return float64(tokens) * pricePerMillion / 1_000_000
}

// fillUsageCost 根据 Usage 中的 token 计量和插件私有价格表填充费用。
// 未知 model 只在计费规格上兜底，Usage.Model 仍保持上游实际返回值。
func fillUsageCost(usage *sdk.Usage) {
	if usage == nil || usage.Model == "" {
		return
	}

	_, spec := LookupModelSpec(usage.Model)
	inputTokens := usage.InputTokens
	outputTokens := usage.OutputTokens
	cachedInputTokens := usage.CachedInputTokens
	cacheCreationTokens := usage.CacheCreationTokens
	cacheCreation5mTokens := usageCacheCreation5mTokens(usage)
	cacheCreation1hTokens := usageCacheCreation1hTokens(usage)

	genericCacheCreationTokens := cacheCreationTokens - cacheCreation5mTokens - cacheCreation1hTokens
	if genericCacheCreationTokens < 0 {
		genericCacheCreationTokens = 0
	}
	billableCacheCreation5mTokens := cacheCreation5mTokens + genericCacheCreationTokens

	inputCost := tokenCost(inputTokens, spec.InputPrice)
	cachedCost := tokenCost(cachedInputTokens, spec.CachedPrice)
	cacheCreation5mCost := tokenCost(billableCacheCreation5mTokens, spec.CacheCreationPrice)
	cacheCreation1hCost := tokenCost(cacheCreation1hTokens, spec.CacheCreation1hPrice)
	outputCost := tokenCost(outputTokens, spec.OutputPrice)
	usage.InputPrice = spec.InputPrice
	usage.CachedInputPrice = spec.CachedPrice
	usage.CacheCreationPrice = spec.CacheCreationPrice
	usage.OutputPrice = spec.OutputPrice
	usage.InputCost = inputCost
	usage.CachedInputCost = cachedCost
	usage.CacheCreationCost = cacheCreation5mCost + cacheCreation1hCost
	usage.OutputCost = outputCost
	setUsageMetadataFloat(usage, usageMetaCacheCreation1hPrice, spec.CacheCreation1hPrice)
	recomputeUsageAccountCost(usage)

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
