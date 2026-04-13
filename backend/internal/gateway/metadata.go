package gateway

import sdk "github.com/DouDOU-start/airgate-sdk"

//go:generate go run ../../cmd/genmanifest

const (
	PluginID             = "gateway-anthropic"
	PluginDisplayName    = "Claude 网关"
	PluginVersion        = "1.0.0"
	PluginDescription    = "Claude Messages API 网关"
	PluginAuthor         = "airgate"
	PluginPlatform       = "anthropic"
	PluginMode           = "simple"
	PluginMinCoreVersion = "1.0.0"
)

func PluginDependencies() []string {
	return []string{}
}

func BuildPluginInfo() sdk.PluginInfo {
	return sdk.PluginInfo{
		ID:          PluginID,
		Name:        PluginDisplayName,
		Version:     PluginVersion,
		Description: PluginDescription,
		Author:      PluginAuthor,
		Type:        sdk.PluginTypeGateway,
		AccountTypes: []sdk.AccountType{
			{
				Key:         "apikey",
				Label:       "API Key",
				Description: "使用 Anthropic API Key 直接访问",
				Fields: []sdk.CredentialField{
					{Key: "api_key", Label: "API Key", Type: "password", Required: true, Placeholder: "sk-ant-..."},
					{Key: "base_url", Label: "API 地址", Type: "text", Required: false, Placeholder: "https://api.anthropic.com"},
				},
			},
			{
				Key:         "oauth",
				Label:       "OAuth 令牌",
				Description: "使用 OAuth Access Token 访问（完整 scope，支持 session/mcp）",
				Fields: []sdk.CredentialField{
					{Key: "access_token", Label: "Access Token", Type: "password", Required: false, Placeholder: "自动获取"},
					{Key: "refresh_token", Label: "Refresh Token", Type: "password", Required: false, Placeholder: "自动获取"},
					{Key: "expires_at", Label: "过期时间", Type: "text", Required: false, Placeholder: "自动填充"},
					{Key: "base_url", Label: "API 地址", Type: "text", Required: false, Placeholder: "https://api.anthropic.com"},
				},
			},
			{
				Key:         "setup_token",
				Label:       "Setup Token",
				Description: "仅推理 scope 的长期 OAuth 令牌（有效期 1 年）",
				Fields: []sdk.CredentialField{
					{Key: "access_token", Label: "Access Token", Type: "password", Required: false, Placeholder: "自动获取"},
					{Key: "refresh_token", Label: "Refresh Token", Type: "password", Required: false, Placeholder: "自动获取"},
					{Key: "expires_at", Label: "过期时间", Type: "text", Required: false, Placeholder: "自动填充"},
					{Key: "base_url", Label: "API 地址", Type: "text", Required: false, Placeholder: "https://api.anthropic.com"},
				},
			},
			{
				Key:         "session_key",
				Label:       "Session Key",
				Description: "使用 claude.ai 的 Session Key 自动获取 OAuth 令牌",
				Fields: []sdk.CredentialField{
					{Key: "session_key", Label: "Session Key", Type: "password", Required: true, Placeholder: "sk-ant-sid01-..."},
					{Key: "access_token", Label: "Access Token", Type: "password", Required: false, Placeholder: "自动获取", EditDisabled: true},
					{Key: "refresh_token", Label: "Refresh Token", Type: "password", Required: false, Placeholder: "自动获取", EditDisabled: true},
					{Key: "expires_at", Label: "过期时间", Type: "text", Required: false, Placeholder: "自动填充", EditDisabled: true},
					{Key: "base_url", Label: "API 地址", Type: "text", Required: false, Placeholder: "https://api.anthropic.com"},
				},
			},
		},
		FrontendWidgets: []sdk.FrontendWidget{
			{Slot: sdk.SlotAccountForm, EntryFile: "index.js", Title: "账号表单"},
		},
	}
}

func PluginRouteDefinitions() []sdk.RouteDefinition {
	return []sdk.RouteDefinition{
		{Method: "POST", Path: "/v1/messages", Description: "Messages API"},
		{Method: "POST", Path: "/v1/messages/count_tokens", Description: "Token 计数"},
		{Method: "GET", Path: "/v1/models", Description: "模型列表"},
	}
}
