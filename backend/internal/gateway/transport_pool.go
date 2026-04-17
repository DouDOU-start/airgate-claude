package gateway

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// ──────────────────────────────────────────────────────
// 传输连接池：复用 HTTP Transport，避免每次请求创建新连接
// ──────────────────────────────────────────────────────

// StandardTransportPool API Key 账号的标准 TLS 连接池
// 按 proxyURL 分组缓存 Transport
type StandardTransportPool struct {
	mu    sync.RWMutex
	pool  map[string]*http.Transport // key = proxyURL (空字符串表示直连)
	dialer *net.Dialer
}

// NewStandardTransportPool 创建标准 Transport 连接池
func NewStandardTransportPool() *StandardTransportPool {
	return &StandardTransportPool{
		pool: make(map[string]*http.Transport),
		dialer: &net.Dialer{
			Timeout:   httpDialTimeout,
			KeepAlive: 30 * time.Second,
		},
	}
}

// Get 获取或创建 Transport
func (p *StandardTransportPool) Get(proxyURL string) *http.Transport {
	p.mu.RLock()
	if t, ok := p.pool[proxyURL]; ok {
		p.mu.RUnlock()
		return t
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()

	// Double-check
	if t, ok := p.pool[proxyURL]; ok {
		return t
	}

	t := &http.Transport{
		DialContext:         p.dialer.DialContext,
		TLSClientConfig:    &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: httpTLSTimeout,
		MaxIdleConns:        httpMaxIdleConns,
		MaxIdleConnsPerHost: httpIdleConnsPerHost,
		IdleConnTimeout:     httpIdleTimeout,
		ForceAttemptHTTP2:   true,
	}

	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			t.Proxy = http.ProxyURL(u)
		}
	}

	p.pool[proxyURL] = t
	return t
}

// Close 关闭所有 Transport
func (p *StandardTransportPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, t := range p.pool {
		t.CloseIdleConnections()
	}
	p.pool = make(map[string]*http.Transport)
}

// FingerprintTransportPool OAuth/session_key 的 uTLS 指纹连接池
// 按 (accountID, proxyURL) 分桶：不同账号持有独立 Transport，
// 互不共享连接 / session ticket / PSK，使得多账号在上游看来是独立 CLI 实例。
type FingerprintTransportPool struct {
	mu   sync.RWMutex
	pool map[string]*http.Transport // key = "accountID|proxyURL"
}

// NewFingerprintTransportPool 创建 TLS 指纹 Transport 连接池
func NewFingerprintTransportPool() *FingerprintTransportPool {
	return &FingerprintTransportPool{
		pool: make(map[string]*http.Transport),
	}
}

// fpKey 生成池 key：tls_profile 变化时自动换 bucket
func fpKey(accountID int64, proxyURL, profile string) string {
	return fmt.Sprintf("%d|%s|%s", accountID, proxyURL, profile)
}

// Get 获取或创建指纹化 Transport（按账号 + profile 隔离）
func (p *FingerprintTransportPool) Get(accountID int64, proxyURL, profile string) *http.Transport {
	key := fpKey(accountID, proxyURL, profile)

	p.mu.RLock()
	if t, ok := p.pool[key]; ok {
		p.mu.RUnlock()
		return t
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()

	if t, ok := p.pool[key]; ok {
		return t
	}

	t := buildFingerprintTransportWithProfile(proxyURL, profile)
	p.pool[key] = t
	return t
}

// Close 关闭所有 Transport
func (p *FingerprintTransportPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, t := range p.pool {
		t.CloseIdleConnections()
	}
	p.pool = make(map[string]*http.Transport)
}

// getHTTPClient 根据账号类型从连接池获取 HTTP Client
func getHTTPClient(stdPool *StandardTransportPool, fpPool *FingerprintTransportPool, accountID int64, accountType, proxyURL, tlsProfile string) *http.Client {
	var transport http.RoundTripper
	switch accountType {
	case "oauth", "session_key":
		if fpPool != nil {
			transport = fpPool.Get(accountID, proxyURL, tlsProfile)
		} else {
			transport = buildFingerprintTransportWithProfile(proxyURL, tlsProfile)
		}
	default:
		if stdPool != nil {
			transport = stdPool.Get(proxyURL)
		} else {
			transport = &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   httpDialTimeout,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
				TLSHandshakeTimeout: httpTLSTimeout,
				MaxIdleConns:        httpMaxIdleConns,
				MaxIdleConnsPerHost: httpIdleConnsPerHost,
				IdleConnTimeout:     httpIdleTimeout,
				ForceAttemptHTTP2:   true,
			}
		}
	}

	return &http.Client{
		Timeout:   httpTimeout,
		Transport: transport,
	}
}

// poolStats 返回连接池统计（用于调试日志）
func poolStats(stdPool *StandardTransportPool, fpPool *FingerprintTransportPool) string {
	stdCount, fpCount := 0, 0
	if stdPool != nil {
		stdPool.mu.RLock()
		stdCount = len(stdPool.pool)
		stdPool.mu.RUnlock()
	}
	if fpPool != nil {
		fpPool.mu.RLock()
		fpCount = len(fpPool.pool)
		fpPool.mu.RUnlock()
	}
	return fmt.Sprintf("standard=%d, fingerprint=%d", stdCount, fpCount)
}
