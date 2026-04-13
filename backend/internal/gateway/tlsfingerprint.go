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
// Node.js 24.x / Claude Code TLS 指纹配置
// 移植自 sub2api tlsfingerprint/dialer.go
// JA3: 44f88fca027f27bab4bb08d4af15f23e
// JA4: t13d1714h1_5b57614c22b0_7baf387fc6ff
// ──────────────────────────────────────────────────────

// defaultCipherSuites Node.js 24.x 的 17 个 cipher suites（顺序关键）
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

// buildClientHelloSpec 构建 Node.js 24.x 的 ClientHello
func buildClientHelloSpec() *utls.ClientHelloSpec {
	// Node.js 24.x extension 顺序
	extensions := []utls.TLSExtension{
		&utls.SNIExtension{},                                                                       // 0: server_name
		&utls.GREASEEncryptedClientHelloExtension{},                                                // 65037: ECH (GREASE)
		&utls.ExtendedMasterSecretExtension{},                                                      // 23: extended_master_secret
		&utls.RenegotiationInfoExtension{},                                                         // 65281: renegotiation_info
		&utls.SupportedCurvesExtension{Curves: defaultCurves},                                      // 10: supported_groups
		&utls.SupportedPointsExtension{SupportedPoints: []uint8{0}},                                // 11: ec_point_formats (uncompressed only)
		&utls.SessionTicketExtension{},                                                             // 35: session_ticket
		&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},                                  // 16: alpn
		&utls.StatusRequestExtension{},                                                             // 5: status_request (OCSP)
		&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: defaultSignatureAlgorithms}, // 13: signature_algorithms
		&utls.SCTExtension{},                                                                       // 18: signed_certificate_timestamp
		&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519}}},                  // 51: key_share
		&utls.PSKKeyExchangeModesExtension{Modes: []uint8{uint8(utls.PskModeDHE)}},                // 45: psk_key_exchange_modes
		&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}}, // 43: supported_versions
	}

	return &utls.ClientHelloSpec{
		CipherSuites:       defaultCipherSuites,
		CompressionMethods: []uint8{0}, // null only
		Extensions:         extensions,
		TLSVersMax:         utls.VersionTLS13,
		TLSVersMin:         utls.VersionTLS10,
	}
}

// ──────────────────────────────────────────────────────
// TLS 指纹 Transport 构建
// ──────────────────────────────────────────────────────

// buildFingerprintTransport 构建带 Node.js 24.x TLS 指纹的 HTTP Transport
func buildFingerprintTransport(proxyURL string) *http.Transport {
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
		}, utls.HelloCustom)

		if err := tlsConn.ApplyPreset(buildClientHelloSpec()); err != nil {
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
	defer resp.Body.Close()

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
