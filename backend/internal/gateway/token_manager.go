package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// ──────────────────────────────────────────────────────
// Token 刷新管理器：进程内锁 + double-check + 错误分类重试
// 参考 sub2api 的 OAuthRefreshAPI.RefreshIfNeeded() 模式
// ──────────────────────────────────────────────────────

const (
	tokenRefreshSkew    = 3 * time.Minute  // 提前刷新窗口
	refreshCooldown     = 60 * time.Second // 已知失败的冷却窗口
	maxRefreshRetries   = 2                // 最大重试次数
	refreshRetryBackoff = 1 * time.Second  // 重试退避间隔
)

// tokenManager 管理 OAuth token 的并发安全刷新
type tokenManager struct {
	gateway *AnthropicGateway
	logger  *slog.Logger
	locks   sync.Map // accountID (int64) -> *accountRefreshState
}

// accountRefreshState 单个账号的刷新状态
type accountRefreshState struct {
	mu            sync.Mutex
	lastRefreshAt time.Time
	lastToken     string // 上次刷新后的 access_token（用于 double-check）
	lastError     error
	lastErrorAt   time.Time
}

// newTokenManager 创建 token 管理器
func newTokenManager(gw *AnthropicGateway, logger *slog.Logger) *tokenManager {
	return &tokenManager{
		gateway: gw,
		logger:  logger,
	}
}

// getState 获取或创建账号的刷新状态
func (m *tokenManager) getState(accountID int64) *accountRefreshState {
	val, _ := m.locks.LoadOrStore(accountID, &accountRefreshState{})
	return val.(*accountRefreshState)
}

// ensureValidToken 检查 token 过期状态，必要时自动刷新
// 返回更新后的凭证（用于回传 Core 持久化），如果没有刷新则为 nil
func (m *tokenManager) ensureValidToken(ctx context.Context, account *sdk.Account) (map[string]string, error) {
	// 对于 session_key 类型且没有 access_token 的情况，需要先做 exchange
	if account.Type == "session_key" && account.Credentials["access_token"] == "" {
		return m.ensureSessionKeyExchange(ctx, account)
	}

	refreshToken := account.Credentials["refresh_token"]
	if refreshToken == "" {
		return nil, nil // 没有 refresh_token，无法刷新
	}

	expiresAtStr := account.Credentials["expires_at"]
	if expiresAtStr == "" {
		return nil, nil // 没有过期时间信息，假设有效
	}

	expiresAt, err := time.Parse(time.RFC3339, expiresAtStr)
	if err != nil {
		m.logger.Warn("token_expires_at_parse_failed",
			sdk.LogFieldAccountID, account.ID,
			"expires_at", expiresAtStr,
			sdk.LogFieldError, err,
		)
		return nil, nil
	}

	// 未过期，无需刷新
	if time.Until(expiresAt) > tokenRefreshSkew {
		return nil, nil
	}

	return m.doRefresh(ctx, account)
}

// ensureSessionKeyExchange 使用 session_key 换取 OAuth token（加锁保护）
func (m *tokenManager) ensureSessionKeyExchange(ctx context.Context, account *sdk.Account) (map[string]string, error) {
	logger := sdk.LoggerFromContext(ctx)
	if logger == nil {
		logger = m.logger
	}
	sessionKey := account.Credentials["session_key"]
	if sessionKey == "" {
		err := fmt.Errorf("session key 账号缺少 session_key")
		logger.Warn("session_key_exchange_failed",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldError, err,
		)
		return nil, err
	}

	state := m.getState(account.ID)
	state.mu.Lock()
	defer state.mu.Unlock()

	// Double-check: 另一个 goroutine 可能已经完成了 exchange
	if account.Credentials["access_token"] != "" {
		return nil, nil
	}

	logger.Debug("session_key_exchange_start", sdk.LogFieldAccountID, account.ID)

	exchangeStart := time.Now()
	tokenResp, err := m.gateway.ExchangeSessionKeyForToken(ctx, sessionKey, account.ProxyURL)
	if err != nil {
		logger.Warn("session_key_exchange_failed",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldDurationMs, time.Since(exchangeStart).Milliseconds(),
			sdk.LogFieldError, err,
		)
		return nil, fmt.Errorf("session key 换取 token 失败: %w", err)
	}

	expiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)

	// 更新内存中的 credentials
	account.Credentials["access_token"] = tokenResp.AccessToken
	account.Credentials["refresh_token"] = tokenResp.RefreshToken
	account.Credentials["expires_at"] = expiresAt

	updated := map[string]string{
		"access_token":  tokenResp.AccessToken,
		"refresh_token": tokenResp.RefreshToken,
		"expires_at":    expiresAt,
	}

	state.lastRefreshAt = time.Now()
	state.lastToken = tokenResp.AccessToken

	logger.Debug("session_key_exchange_completed",
		sdk.LogFieldAccountID, account.ID,
		sdk.LogFieldDurationMs, time.Since(exchangeStart).Milliseconds(),
		"expires_at", expiresAt,
	)
	return updated, nil
}

// doRefresh 执行实际的 token 刷新（加锁 + double-check + 重试）
func (m *tokenManager) doRefresh(ctx context.Context, account *sdk.Account) (map[string]string, error) {
	logger := sdk.LoggerFromContext(ctx)
	if logger == nil {
		logger = m.logger
	}
	state := m.getState(account.ID)
	state.mu.Lock()
	defer state.mu.Unlock()

	// Double-check: 另一个 goroutine 可能已经刷新了
	if state.lastToken != "" && state.lastToken == account.Credentials["access_token"] {
		// token 没变，检查是否刚刚刷新过
	} else if state.lastToken != "" && state.lastToken != account.Credentials["access_token"] {
		// token 已被其他路径更新，重新检查过期时间
		if expiresAt, err := time.Parse(time.RFC3339, account.Credentials["expires_at"]); err == nil {
			if time.Until(expiresAt) > tokenRefreshSkew {
				return nil, nil // 已被刷新且未过期
			}
		}
	}

	// 检查冷却窗口：最近的错误是不可重试的，不重复刷新
	if state.lastError != nil && time.Since(state.lastErrorAt) < refreshCooldown {
		if isNonRetryableRefreshError(state.lastError) {
			logger.Warn("token_refresh_cooldown",
				sdk.LogFieldAccountID, account.ID,
				sdk.LogFieldError, state.lastError,
				"cooldown_remaining_ms", (refreshCooldown - time.Since(state.lastErrorAt)).Milliseconds(),
			)
			return nil, nil // 不阻断请求，使用现有 token
		}
	}

	logger.Debug("token_refresh_start", sdk.LogFieldAccountID, account.ID)
	refreshStart := time.Now()

	refreshToken := account.Credentials["refresh_token"]
	proxyURL := account.ProxyURL

	// 带重试的刷新
	var lastErr error
	for attempt := 0; attempt <= maxRefreshRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(refreshRetryBackoff * time.Duration(attempt)):
			}
		}

		tokenResp, err := m.gateway.RefreshToken(ctx, refreshToken, proxyURL)
		if err != nil {
			lastErr = err

			// 不可重试错误：立即停止
			if isNonRetryableRefreshError(err) {
				state.lastError = err
				state.lastErrorAt = time.Now()
				logger.Warn("token_refresh_failed",
					sdk.LogFieldAccountID, account.ID,
					sdk.LogFieldDurationMs, time.Since(refreshStart).Milliseconds(),
					sdk.LogFieldError, err,
					"attempt", attempt+1,
					sdk.LogFieldReason, "non_retryable",
				)
				// 不阻断请求，使用现有 token（匹配 sub2api Claude 策略）
				return nil, nil
			}

			logger.Warn("token_refresh_retry",
				sdk.LogFieldAccountID, account.ID,
				sdk.LogFieldError, err,
				"attempt", attempt+1,
				"max_retries", maxRefreshRetries,
			)
			continue
		}

		// 刷新成功
		newExpiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)

		// 更新内存中的 credentials
		account.Credentials["access_token"] = tokenResp.AccessToken
		account.Credentials["expires_at"] = newExpiresAt
		if tokenResp.RefreshToken != "" {
			account.Credentials["refresh_token"] = tokenResp.RefreshToken
		}

		// 更新状态
		state.lastRefreshAt = time.Now()
		state.lastToken = tokenResp.AccessToken
		state.lastError = nil
		state.lastErrorAt = time.Time{}

		// 构建回传给 Core 的更新凭证
		updated := map[string]string{
			"access_token": tokenResp.AccessToken,
			"expires_at":   newExpiresAt,
		}
		if tokenResp.RefreshToken != "" {
			updated["refresh_token"] = tokenResp.RefreshToken
		}

		logger.Debug("token_refresh_completed",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldDurationMs, time.Since(refreshStart).Milliseconds(),
			"new_expires_at", newExpiresAt,
			"attempt", attempt+1,
		)
		return updated, nil
	}

	// 重试耗尽：记录错误，但不阻断请求
	state.lastError = lastErr
	state.lastErrorAt = time.Now()
	logger.Error("token_refresh_exhausted",
		sdk.LogFieldAccountID, account.ID,
		sdk.LogFieldDurationMs, time.Since(refreshStart).Milliseconds(),
		sdk.LogFieldError, lastErr,
	)
	return nil, nil
}

// isNonRetryableRefreshError 判断刷新错误是否不可重试
// 移植自 sub2api token_refresh_service.go
func isNonRetryableRefreshError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	nonRetryableKeywords := []string{
		"invalid_grant",
		"invalid_client",
		"unauthorized_client",
		"access_denied",
		"missing_project_id",
		"no refresh token available",
	}
	for _, keyword := range nonRetryableKeywords {
		if strings.Contains(msg, keyword) {
			return true
		}
	}
	return false
}
