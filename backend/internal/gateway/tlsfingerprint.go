package gateway

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/proxy"
)

// ──────────────────────────────────────────────────────
// Claude Code 2.1.112 (Bun 1.3.13) TLS 指纹配置
// ALPN: h2, http/1.1
// 真实 JA3/JA4 baseline 由 backend/cmd/fp capture 产出
// ──────────────────────────────────────────────────────

// defaultCipherSuites Bun 1.3.13 BoringSSL cipher 顺序
// （与 Node 24.x 近似，后续由 fp capture 精确校正）
var defaultCipherSuites = []uint16{
	// TLS 1.3
	0x1301, // TLS_AES_128_GCM_SHA256
	0x1302, // TLS_AES_256_GCM_SHA384
	0x1303, // TLS_CHACHA20_POLY1305_SHA256
	// ECDHE + AES-GCM
	0xc02b, // TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256
	0xc02f, // TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256
	0xc02c, // TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384
	0xc030, // TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384
	// ECDHE + ChaCha20-Poly1305
	0xcca9, // TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256
	0xcca8, // TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256
	// ECDHE + AES-CBC-SHA (legacy)
	0xc009, // TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA
	0xc013, // TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA
	0xc00a, // TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA
	0xc014, // TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA
	// RSA + AES-GCM (non-PFS)
	0x009c, // TLS_RSA_WITH_AES_128_GCM_SHA256
	0x009d, // TLS_RSA_WITH_AES_256_GCM_SHA384
	// RSA + AES-CBC-SHA (non-PFS, legacy)
	0x002f, // TLS_RSA_WITH_AES_128_CBC_SHA
	0x0035, // TLS_RSA_WITH_AES_256_CBC_SHA
}

// defaultCurves Node.js 24.x 仅 3 条曲线
var defaultCurves = []utls.CurveID{
	utls.X25519,    // 0x001d
	utls.CurveP256, // 0x0017
	utls.CurveP384, // 0x0018
}

// defaultSignatureAlgorithms Node.js 24.x 的 9 个签名算法
var defaultSignatureAlgorithms = []utls.SignatureScheme{
	0x0403, // ecdsa_secp256r1_sha256
	0x0804, // rsa_pss_rsae_sha256
	0x0401, // rsa_pkcs1_sha256
	0x0503, // ecdsa_secp384r1_sha384
	0x0805, // rsa_pss_rsae_sha384
	0x0501, // rsa_pkcs1_sha384
	0x0806, // rsa_pss_rsae_sha512
	0x0601, // rsa_pkcs1_sha512
	0x0201, // rsa_pkcs1_sha1
}

// buildBunClientHelloSpec 构建 Bun 1.3.x 的 ClientHello（与真实 CLI 一致）
//
// Ground truth（由 bun 1.3.x + tls.peet.ws 抓包确认，JA3 hash=44f88fca027f27bab4bb08d4af15f23e）：
//   - cipher suites 17 项，无 GREASE 前缀
//   - 扩展顺序：SNI → ECH(65037) → EMS → reneg → curves → pts → ticket → ALPN →
//     status_req → sig_algs → SCT → key_share → PSK_modes → versions
//   - ALPN 只 http/1.1（Bun fetch 不自动协商 h2，Claude CLI 同样）
//   - curves / key_share / supported_versions 均不含 GREASE
//
// 多账号维度的差异由 uTLS 每连接独立的 ECH GREASE 值 + 独立 Transport 池承担，
// 不应通过人为加 GREASE 打乱 JA3。
func buildBunClientHelloSpec() *utls.ClientHelloSpec {
	extensions := []utls.TLSExtension{
		&utls.SNIExtension{},                                                                            // 0:  server_name
		&utls.GREASEEncryptedClientHelloExtension{},                                                     // 65037: ECH (boringssl GREASE)
		&utls.ExtendedMasterSecretExtension{},                                                           // 23
		&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},                   // 65281
		&utls.SupportedCurvesExtension{Curves: defaultCurves},                                           // 10
		&utls.SupportedPointsExtension{SupportedPoints: []uint8{0}},                                     // 11 (uncompressed)
		&utls.SessionTicketExtension{},                                                                  // 35
		&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},                                        // 16
		&utls.StatusRequestExtension{},                                                                  // 5
		&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: defaultSignatureAlgorithms},    // 13
		&utls.SCTExtension{},                                                                            // 18
		&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519}}},                       // 51
		&utls.PSKKeyExchangeModesExtension{Modes: []uint8{uint8(utls.PskModeDHE)}},                      // 45
		&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},      // 43
	}

	return &utls.ClientHelloSpec{
		CipherSuites:       defaultCipherSuites,
		CompressionMethods: []uint8{0}, // null only
		Extensions:         extensions,
		TLSVersMax:         utls.VersionTLS13,
		TLSVersMin:         utls.VersionTLS12, // Bun BoringSSL 起点
	}
}

// ExportBunClientHelloSpec 暴露给 fp CLI 使用，返回当前 Bun baseline 的 ClientHelloSpec。
// 仅供 backend/cmd/fp 等内部工具使用，不属于插件对外契约。
func ExportBunClientHelloSpec() *utls.ClientHelloSpec {
	return buildBunClientHelloSpec()
}

// selectClientHelloSpec 按账号配置的 tls_profile 返回对应 ClientHelloSpec。
//
// 当前仅有一个 baseline（bun-2.1.112），未知/空值均走 auto。
// 新增 baseline 时只需在此处追加 case。
func selectClientHelloSpec(profile string) *utls.ClientHelloSpec {
	switch profile {
	case "bun-2.1.112":
		return buildBunClientHelloSpec()
	default: // "", "auto", 未知值
		return buildBunClientHelloSpec()
	}
}

// ──────────────────────────────────────────────────────
// TLS 指纹 Transport 构建
// ──────────────────────────────────────────────────────

// buildFingerprintTransport 构建带 Bun 1.3.x TLS 指纹的 Transport
//
// Bun 默认 fetch 只协商 http/1.1（ground truth），因此：
//   - ALPN 只带 http/1.1
//   - ForceAttemptHTTP2=false，不启 http2.ConfigureTransport
//
// 不强制 h2 是刻意的：Anthropic 侧有行为模型对比，配置与真实 CLI 不一致会拉高识别分数。
func buildFingerprintTransport(proxyURL string) *http.Transport {
	return buildFingerprintTransportWithProfile(proxyURL, "")
}

// buildFingerprintTransportWithProfile 允许账号指定 tls_profile 精确选择 ClientHelloSpec
func buildFingerprintTransportWithProfile(proxyURL, profile string) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   httpDialTimeout,
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{
		MaxIdleConns:        httpMaxIdleConns,
		MaxIdleConnsPerHost: httpIdleConnsPerHost,
		IdleConnTimeout:     httpIdleTimeout,
		ForceAttemptHTTP2:   false,
	}

	if proxyURL != "" {
		if parsedProxy, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(parsedProxy)
		}
	}

	transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		var rawConn net.Conn
		var err error

		if proxyURL != "" {
			rawConn, err = dialThroughProxy(ctx, proxyURL, addr, dialer)
		} else {
			rawConn, err = dialer.DialContext(ctx, network, addr)
		}
		if err != nil {
			return nil, fmt.Errorf("TCP dial failed: %w", err)
		}

		host, _, _ := net.SplitHostPort(addr)
		if host == "" {
			host = addr
		}

		tlsConn := utls.UClient(rawConn, &utls.Config{
			ServerName:         host,
			InsecureSkipVerify: false,
			MinVersion:         tls.VersionTLS12,
			NextProtos:         []string{"http/1.1"},
		}, utls.HelloCustom)

		if err := tlsConn.ApplyPreset(selectClientHelloSpec(profile)); err != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("apply TLS preset: %w", err)
		}

		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("TLS handshake failed: %w", err)
		}

		return tlsConn, nil
	}

	return transport
}

// ──────────────────────────────────────────────────────
// 代理隧道
// ──────────────────────────────────────────────────────

func dialThroughProxy(ctx context.Context, proxyURL string, targetAddr string, dialer *net.Dialer) (net.Conn, error) {
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return nil, err
	}

	proxyAddr := parsed.Host
	if !hasPort(proxyAddr) {
		switch parsed.Scheme {
		case "http":
			proxyAddr += ":80"
		case "https":
			proxyAddr += ":443"
		case "socks5":
			proxyAddr += ":1080"
		}
	}

	// SOCKS5
	if parsed.Scheme == "socks5" {
		var auth *proxy.Auth
		if parsed.User != nil {
			password, _ := parsed.User.Password()
			auth = &proxy.Auth{
				User:     parsed.User.Username(),
				Password: password,
			}
		}
		socksDialer, err := proxy.SOCKS5("tcp", proxyAddr, auth, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("create SOCKS5 dialer: %w", err)
		}
		conn, err := socksDialer.Dial("tcp", targetAddr)
		if err != nil {
			return nil, fmt.Errorf("SOCKS5 connect: %w", err)
		}
		return conn, nil
	}

	// HTTP CONNECT
	conn, err := dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("connect to proxy: %w", err)
	}

	connectReq := &http.Request{
		Method: "CONNECT",
		URL:    &url.URL{Opaque: targetAddr},
		Host:   targetAddr,
		Header: make(http.Header),
	}

	if parsed.User != nil {
		password := getPassword(parsed.User)
		connectReq.Header.Set("Proxy-Authorization", "Basic "+basicAuth(parsed.User.Username(), password))
	}

	if err := connectReq.Write(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write CONNECT: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, connectReq)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read proxy response: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy CONNECT failed: %s", resp.Status)
	}

	return conn, nil
}

func hasPort(addr string) bool {
	_, _, err := net.SplitHostPort(addr)
	return err == nil
}

func getPassword(u *url.Userinfo) string {
	p, _ := u.Password()
	return p
}

func basicAuth(username, password string) string {
	auth := username + ":" + password
	return base64.StdEncoding.EncodeToString([]byte(auth))
}
