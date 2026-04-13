package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/DouDOU-start/airgate-sdk/devserver"
)

// OAuthDevHandler devserver 的 OAuth HTTP handler
type OAuthDevHandler struct {
	Gateway *AnthropicGateway
	Store   *devserver.AccountStore
}

// RegisterRoutes 注册 OAuth 路由到 mux
func (h *OAuthDevHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/oauth/start", h.handleStart)
	mux.HandleFunc("/api/oauth/callback", h.handleCallback)
	mux.HandleFunc("/api/oauth/refresh", h.handleRefresh)
	mux.HandleFunc("/api/console/cookie-auth", h.handleCookieAuth)
	mux.HandleFunc("/api/console/batch-cookie-auth", h.handleBatchCookieAuth)
	mux.HandleFunc("/api/accounts/usage/", h.handleUsage)
}

// handleStart 处理 POST /api/oauth/start，返回授权链接
func (h *OAuthDevHandler) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	resp, err := h.Gateway.StartOAuth()
	if err != nil {
		log.Printf("StartOAuth 失败: %v", err)
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"authorize_url": resp.AuthorizeURL,
		"state":         resp.State,
	})
}

// handleCallback 处理 POST /api/oauth/callback，交换 code 获取 token
func (h *OAuthDevHandler) handleCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var raw struct {
		CallbackURL string `json:"callback_url"`
		ProxyURL    string `json:"proxy_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	parsed, err := url.Parse(raw.CallbackURL)
	if err != nil {
		http.Error(w, `{"error":"invalid callback_url"}`, http.StatusBadRequest)
		return
	}

	code := parsed.Query().Get("code")
	state := parsed.Query().Get("state")
	if code == "" || state == "" {
		http.Error(w, `{"error":"missing code or state"}`, http.StatusBadRequest)
		return
	}

	tokenResp, err := h.Gateway.HandleOAuthCallback(r.Context(), code, state, raw.ProxyURL)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	expiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)

	// 创建账号并存储
	account := devserver.DevAccount{
		Name:        "OAuth Account",
		AccountType: "oauth",
		Credentials: map[string]string{
			"access_token":  tokenResp.AccessToken,
			"refresh_token": tokenResp.RefreshToken,
			"expires_at":    expiresAt,
		},
	}
	if tokenResp.Account != nil {
		account.Name = tokenResp.Account.EmailAddress
	}

	id := h.Store.Create(account).ID

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":          id,
		"credentials": account.Credentials,
		"account_name": account.Name,
	})
}

// handleRefresh 处理 POST /api/oauth/refresh，手动刷新 token
func (h *OAuthDevHandler) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var raw struct {
		RefreshToken string `json:"refresh_token"`
		ProxyURL     string `json:"proxy_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil || raw.RefreshToken == "" {
		http.Error(w, `{"error":"missing refresh_token"}`, http.StatusBadRequest)
		return
	}

	tokenResp, err := h.Gateway.RefreshToken(r.Context(), raw.RefreshToken, raw.ProxyURL)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	expiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"access_token":  tokenResp.AccessToken,
		"refresh_token": tokenResp.RefreshToken,
		"expires_at":    expiresAt,
	})
}

// handleCookieAuth 处理 POST /api/console/cookie-auth，Session Key 一键导入
func (h *OAuthDevHandler) handleCookieAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var raw struct {
		SessionKey string `json:"session_key"`
		ProxyURL   string `json:"proxy_url"`
		Scope      string `json:"scope"`
	}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil || raw.SessionKey == "" {
		http.Error(w, `{"error":"missing session_key"}`, http.StatusBadRequest)
		return
	}

	accountType := "oauth"
	tokenResp, err := h.Gateway.ExchangeSessionKeyForToken(r.Context(), raw.SessionKey, raw.ProxyURL)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	expiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)

	account := devserver.DevAccount{
		Name:        "Cookie Auth Account",
		AccountType: accountType,
		Credentials: map[string]string{
			"access_token":  tokenResp.AccessToken,
			"refresh_token": tokenResp.RefreshToken,
			"expires_at":    expiresAt,
		},
	}
	if tokenResp.Account != nil {
		account.Name = tokenResp.Account.EmailAddress
	}

	id := h.Store.Create(account).ID

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":           id,
		"account_type": accountType,
		"credentials":  account.Credentials,
		"account_name": account.Name,
	})
}

// handleBatchCookieAuth 处理 POST /api/console/batch-cookie-auth，批量导入
func (h *OAuthDevHandler) handleBatchCookieAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var raw struct {
		SessionKeys []string `json:"session_keys"`
		ProxyURL    string   `json:"proxy_url"`
		Scope       string   `json:"scope"`
	}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil || len(raw.SessionKeys) == 0 {
		http.Error(w, `{"error":"missing session_keys"}`, http.StatusBadRequest)
		return
	}

	type batchResult struct {
		Email       string `json:"email,omitempty"`
		AccountUUID string `json:"account_uuid,omitempty"`
		AccountType string `json:"account_type,omitempty"`
		Status      string `json:"status"`
		Error       string `json:"error,omitempty"`
	}

	results := make([]batchResult, 0, len(raw.SessionKeys))
	for _, sk := range raw.SessionKeys {
		acctType := "oauth"
		tokenResp, err := h.Gateway.ExchangeSessionKeyForToken(r.Context(), sk, raw.ProxyURL)

		if err != nil {
			results = append(results, batchResult{Status: "failed", Error: err.Error()})
			continue
		}

		res := batchResult{Status: "ok", AccountType: acctType}
		if tokenResp.Account != nil {
			res.Email = tokenResp.Account.EmailAddress
			res.AccountUUID = tokenResp.Account.UUID
		}
		results = append(results, res)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
}

// handleUsage 处理 GET /api/accounts/usage/{id}，查询账号用量
func (h *OAuthDevHandler) handleUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// 提取 account ID
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/accounts/usage/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, `{"error":"missing account id"}`, http.StatusBadRequest)
		return
	}

	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, `{"error":"invalid account id"}`, http.StatusBadRequest)
		return
	}

	account := h.Store.Get(id)
	if account == nil {
		http.Error(w, `{"error":"account not found"}`, http.StatusNotFound)
		return
	}

	accessToken := account.Credentials["access_token"]
	if accessToken == "" {
		http.Error(w, `{"error":"account has no access_token"}`, http.StatusBadRequest)
		return
	}

	usageResp, err := h.Gateway.fetchUsage(context.Background(), accessToken, account.Credentials["proxy_url"])
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(usageResp)
}
