package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"sync"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// ──────────────────────────────────────────────────────
// Sidecar：三条独立后台循环，模拟真实 Claude Code 的后台流量特征
// ──────────────────────────────────────────────────────
//
//  1. usagePoller       — 5~15 min 抖动轮询 /api/oauth/usage
//  2. postRefreshProbe  — token 刷新后 max_tokens=1 最小探测
//  3. countTokensWorker — 真实 /v1/messages 成功后异步补发 count_tokens
//
// 三条循环互不阻塞转发主流程，失败仅告警不下线账号。

const (
	usagePollerBaseInterval   = 10 * time.Minute
	usagePollerJitter         = 5 * time.Minute // ± 5 min → 实际 [5min, 15min]
	countTokensChanBuffer     = 128
	countTokensRequestTimeout = 20 * time.Second
	probeRequestTimeout       = 15 * time.Second
	probeModel                = "claude-haiku-4-5-20251001"
)

// ──────────────────────────────────────────────────────
// accountRegistry
// ──────────────────────────────────────────────────────

// accountSnapshot 记录一个 oauth 账号的最小上下文（给 sidecar 用）
type accountSnapshot struct {
	id          int64
	accountType string
	proxyURL    string
	credentials map[string]string
	lastSeenAt  time.Time
}

// accountRegistry 记录被 forward 路径见到过的 oauth/session_key 账号
// sidecar 只对 registry 里的账号发起流量，避免并发冲击所有账号
type accountRegistry struct {
	mu       sync.RWMutex
	accounts map[int64]*accountSnapshot
}

func newAccountRegistry() *accountRegistry {
	return &accountRegistry{accounts: make(map[int64]*accountSnapshot)}
}

// register 登记/更新账号快照（凭证按值拷贝，避免 race）
func (r *accountRegistry) register(account *sdk.Account) {
	if account == nil {
		return
	}
	if account.Type != "oauth" && account.Type != "session_key" {
		return
	}
	snap := &accountSnapshot{
		id:          account.ID,
		accountType: account.Type,
		proxyURL:    account.ProxyURL,
		credentials: make(map[string]string, len(account.Credentials)),
		lastSeenAt:  time.Now(),
	}
	for k, v := range account.Credentials {
		snap.credentials[k] = v
	}
	r.mu.Lock()
	r.accounts[account.ID] = snap
	r.mu.Unlock()
}

// snapshot 返回当前所有账号快照的切片拷贝
func (r *accountRegistry) snapshot() []*accountSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*accountSnapshot, 0, len(r.accounts))
	for _, a := range r.accounts {
		out = append(out, a)
	}
	return out
}

// ──────────────────────────────────────────────────────
// sidecarRunner
// ──────────────────────────────────────────────────────

// countTokensJob 异步补发 count_tokens 的任务
type countTokensJob struct {
	accountID int64
	body      []byte
}

// sidecarRunner 三路 sidecar 的聚合启动器
type sidecarRunner struct {
	gateway *AnthropicGateway
	jobs    chan countTokensJob
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func newSidecarRunner(g *AnthropicGateway) *sidecarRunner {
	return &sidecarRunner{
		gateway: g,
		jobs:    make(chan countTokensJob, countTokensChanBuffer),
	}
}

// start 启动 usagePoller + countTokensWorker 两条常驻循环
// postRefreshProbe 是事件驱动（由 tokenManager 成功刷新后触发），不占用常驻 goroutine
func (s *sidecarRunner) start() {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	s.wg.Add(2)
	go s.usagePollerLoop(ctx)
	go s.countTokensLoop(ctx)
}

// stop 停止所有 sidecar
func (s *sidecarRunner) stop() {
	if s.cancel != nil {
		s.cancel()
	}
	close(s.jobs)
	s.wg.Wait()
}

// scheduleCountTokens 非阻塞投递 count_tokens 任务；队列满则丢弃（sidecar 流量不应反压主链路）
func (s *sidecarRunner) scheduleCountTokens(accountID int64, body []byte) {
	if len(body) == 0 {
		return
	}
	bodyCopy := make([]byte, len(body))
	copy(bodyCopy, body)
	select {
	case s.jobs <- countTokensJob{accountID: accountID, body: bodyCopy}:
	default:
		// 队列满，安静丢弃
	}
}

// ──────────────────────────────────────────────────────
// usagePoller
// ──────────────────────────────────────────────────────

func (s *sidecarRunner) usagePollerLoop(ctx context.Context) {
	defer s.wg.Done()
	logger := s.gateway.logger

	// 启动后先等一段时间再开工，避免 cold start 抖动
	initial := time.Duration(rand.Int63n(int64(usagePollerJitter))) + 1*time.Minute
	timer := time.NewTimer(initial)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		for _, snap := range s.gateway.registry.snapshot() {
			if ctx.Err() != nil {
				return
			}
			s.pollOne(ctx, snap, logger)
		}

		// 下一轮：base ± jitter
		next := usagePollerBaseInterval + time.Duration(rand.Int63n(int64(usagePollerJitter*2))) - usagePollerJitter
		if next < 1*time.Minute {
			next = 1 * time.Minute
		}
		timer.Reset(next)
	}
}

func (s *sidecarRunner) pollOne(ctx context.Context, snap *accountSnapshot, logger interface {
	Debug(msg string, args ...any)
	Warn(msg string, args ...any)
}) {
	token := snap.credentials["access_token"]
	if token == "" {
		return
	}

	pollCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	_, err := s.gateway.fetchUsage(pollCtx, token, snap.credentials["proxy_url"])
	if err != nil {
		logger.Warn("sidecar usage poll failed", "account_id", snap.id, "error", err)
		return
	}
	logger.Debug("sidecar usage poll ok", "account_id", snap.id)
}

// ──────────────────────────────────────────────────────
// countTokensWorker
// ──────────────────────────────────────────────────────

func (s *sidecarRunner) countTokensLoop(ctx context.Context) {
	defer s.wg.Done()
	logger := s.gateway.logger

	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-s.jobs:
			if !ok {
				return
			}
			s.runCountTokens(ctx, job, logger)
		}
	}
}

func (s *sidecarRunner) runCountTokens(ctx context.Context, job countTokensJob, logger interface {
	Debug(msg string, args ...any)
	Warn(msg string, args ...any)
}) {
	// 在 registry 中查账号（可能已失效）
	s.gateway.registry.mu.RLock()
	snap, ok := s.gateway.registry.accounts[job.accountID]
	s.gateway.registry.mu.RUnlock()
	if !ok {
		return
	}

	reqCtx, cancel := context.WithTimeout(ctx, countTokensRequestTimeout)
	defer cancel()

	// 复用现有 forwardCountTokens 构造，但不写入 client writer
	fakeAccount := &sdk.Account{
		ID:          snap.id,
		Type:        snap.accountType,
		ProxyURL:    snap.proxyURL,
		Credentials: snap.credentials,
	}
	fwdReq := &sdk.ForwardRequest{
		Account: fakeAccount,
		Body:    job.body,
		Writer:  nil, // 不回写
	}
	if _, err := s.gateway.forwardCountTokens(reqCtx, fwdReq); err != nil {
		logger.Warn("sidecar count_tokens failed", "account_id", snap.id, "error", err)
		return
	}
	logger.Debug("sidecar count_tokens ok", "account_id", snap.id)
}

// ──────────────────────────────────────────────────────
// postRefreshProbe：token 刷新成功后，发起最小探测
// ──────────────────────────────────────────────────────

// fireRefreshProbe 异步发起一次 max_tokens=1 的 /v1/messages 请求
// 目的：新 token 刚刷新，立即用最低代价验证上游接受该 token，
// 同时给 Anthropic 侧留下"CLI 启动探测"的行为轨迹。
func (s *sidecarRunner) fireRefreshProbe(account *sdk.Account) {
	if account == nil {
		return
	}
	snap := &accountSnapshot{
		id:          account.ID,
		accountType: account.Type,
		proxyURL:    account.ProxyURL,
		credentials: make(map[string]string, len(account.Credentials)),
	}
	for k, v := range account.Credentials {
		snap.credentials[k] = v
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), probeRequestTimeout)
		defer cancel()
		s.runProbe(ctx, snap)
	}()
}

func (s *sidecarRunner) runProbe(ctx context.Context, snap *accountSnapshot) {
	logger := s.gateway.logger

	probeBody := map[string]any{
		"model":      probeModel,
		"max_tokens": 1,
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
		},
	}
	body, _ := json.Marshal(probeBody)

	// 复用 OAuth forward 的 body 预处理（注入 metadata.user_id、system 等）
	fakeAccount := &sdk.Account{
		ID:          snap.id,
		Type:        snap.accountType,
		ProxyURL:    snap.proxyURL,
		Credentials: snap.credentials,
	}
	body = preprocessBody(body)
	body = preprocessOAuthBody(body, fakeAccount)

	targetURL := resolveBaseURL(snap.credentials) + "/v1/messages?beta=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		logger.Warn("sidecar probe build request failed", "account_id", snap.id, "error", err)
		return
	}

	setAnthropicAuthHeaders(req, fakeAccount, http.Header{}, probeModel)

	client := getHTTPClient(s.gateway.stdPool, s.gateway.fpPool, snap.id, snap.accountType, snap.proxyURL, snap.credentials["tls_profile"])
	resp, err := client.Do(req)
	if err != nil {
		logger.Warn("sidecar probe request failed", "account_id", snap.id, "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 400 {
		logger.Warn("sidecar probe non-2xx", "account_id", snap.id, "status", resp.StatusCode)
		return
	}
	logger.Debug("sidecar probe ok", "account_id", snap.id, "status", resp.StatusCode)
}
