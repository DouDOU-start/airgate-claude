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
// Claude CLI TLS 指纹配置
// JA3: 1a28e69016765d92e3b381168d68922c
// 模拟 Claude CLI 2.x / Node.js 20.x + OpenSSL 3.x
// ──────────────────────────────────────────────────────

// defaultCipherSuites Claude CLI 的密码套件列表（59 个，顺序重要）
var defaultCipherSuites = []uint16{
	utls.GREASE_PLACEHOLDER, // GREASE
	// TLS 1.3
	0x1302, 0x1303, 0x1301,
	// ECDHE+AES-GCM
	0xc02f, 0xc02b, 0xc030, 0xc02c,
	// DHE+AES-GCM
	0x009e,
	// ECDHE/DHE+AES-CBC-SHA256/384
	0xc027, 0x0067, 0xc028, 0x006b,
	// DHE-DSS/RSA+AES-GCM
	0x00a3, 0x009f,
	// ChaCha20-Poly1305
	0xcca9, 0xcca8, 0xccaa,
	// AES-CCM
	0xc0af, 0xc0ad, 0xc0a3, 0xc09f, 0xc0ae, 0xc0ac, 0xc0a2, 0xc09e,
	// ARIA
	0xc05d, 0xc061, 0xc057, 0xc053, 0xc05c, 0xc060, 0xc056, 0xc052,
	// DHE-DSS+AES-GCM
	0x00a2,
	// ECDHE/DHE+AES-CBC-SHA384/256
	0xc024, 0x006a, 0xc023, 0x0040,
	// Legacy ECDHE/DHE+AES-CBC-SHA
	0xc00a, 0xc014, 0x0039, 0x0038, 0xc009, 0xc013, 0x0033, 0x0032,
	// RSA suites (256-bit)
	0x009d, 0xc0a1, 0xc09d, 0xc051,
	// RSA suites (128-bit)
	0x009c, 0xc0a0, 0xc09c, 0xc050,
	// Legacy RSA+AES-CBC
	0x003d, 0x003c, 0x0035, 0x002f,
	// SCSV
	0x00ff,
}

// defaultCurves 椭圆曲线列表
var defaultCurves = []utls.CurveID{
	utls.X25519,    // 0x001d
	utls.CurveP256, // 0x0017
	0x001e,         // x448
	utls.CurveP521, // 0x0019
	utls.CurveP384, // 0x0018
	0x0100,         // ffdhe2048
	0x0101,         // ffdhe3072
	0x0102,         // ffdhe4096
	0x0103,         // ffdhe6144
	0x0104,         // ffdhe8192
	utls.GREASE_PLACEHOLDER, // GREASE
}

// defaultPointFormats EC 点格式
var defaultPointFormats = []uint8{0, 1, 2} // uncompressed, compressed_prime, compressed_char2

// defaultSignatureAlgorithms 签名算法列表
var defaultSignatureAlgorithms = []utls.SignatureScheme{
	0x0403, 0x0503, 0x0603,
	0x0807, 0x0808, 0x0809, 0x080a, 0x080b,
	0x0804, 0x0805, 0x0806,
	0x0401, 0x0501, 0x0601,
	0x0303, 0x0301, 0x0302,
	0x0402, 0x0502, 0x0602,
}

// buildClientHelloSpec 构建模拟 Claude CLI 的 ClientHello 规格
func buildClientHelloSpec() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		TLSVersMax: utls.VersionTLS13,
		TLSVersMin: utls.VersionTLS10,
		CipherSuites: defaultCipherSuites,
		CompressionMethods: []uint8{0}, // null only
		Extensions: []utls.TLSExtension{
			&utls.UtlsGREASEExtension{},                  // GREASE (first)
			&utls.SNIExtension{},
			&utls.SupportedPointsExtension{SupportedPoints: defaultPointFormats},
			&utls.SupportedCurvesExtension{Curves: defaultCurves},
			&utls.SessionTicketExtension{},
			&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
			&utls.ExtendedMasterSecretExtension{},         // Bug 3: correct type
			&utls.GenericExtension{Id: 22},                // encrypt_then_mac (Bug 2)
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: defaultSignatureAlgorithms},
			&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519}}},
			&utls.UtlsGREASEExtension{},                  // GREASE (near end)
			&utls.UtlsPaddingExtension{GetPaddingLen: utls.BoringPaddingStyle},
		},
	}
}

// ──────────────────────────────────────────────────────
// TLS 指纹 Transport 构建
// ──────────────────────────────────────────────────────

// buildFingerprintTransport 构建带 TLS 指纹的 HTTP Transport
func buildFingerprintTransport(proxyURL string) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   httpDialTimeout,
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{
		MaxIdleConns:        httpMaxIdleConns,
		MaxIdleConnsPerHost: httpIdleConnsPerHost,
		IdleConnTimeout:     httpIdleTimeout,
		ForceAttemptHTTP2:   false, // uTLS 不完全支持 HTTP/2 negotiation
	}

	if proxyURL != "" {
		if parsedProxy, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(parsedProxy)
		}
	}

	transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		// 如果有代理，先通过代理建立 TCP 连接
		var rawConn net.Conn
		var err error

		if proxyURL != "" {
			rawConn, err = dialThroughProxy(ctx, proxyURL, addr, dialer)
		} else {
			rawConn, err = dialer.DialContext(ctx, network, addr)
		}
		if err != nil {
			return nil, fmt.Errorf("TCP 连接失败: %w", err)
		}

		// 提取主机名（去掉端口）
		host, _, _ := net.SplitHostPort(addr)
		if host == "" {
			host = addr
		}

		// 使用 uTLS 建立指纹化 TLS 连接
		tlsConn := utls.UClient(rawConn, &utls.Config{
			ServerName:         host,
			InsecureSkipVerify: false,
			MinVersion:         tls.VersionTLS12,
		}, utls.HelloCustom)

		if err := tlsConn.ApplyPreset(buildClientHelloSpec()); err != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("应用 TLS 指纹失败: %w", err)
		}

		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("TLS 握手失败: %w", err)
		}

		return tlsConn, nil
	}

	return transport
}

// dialThroughProxy 通过代理建立隧道连接
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

	// Bug 1: SOCKS5 代理使用 golang.org/x/net/proxy 正确隧道
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
			return nil, fmt.Errorf("创建 SOCKS5 代理失败: %w", err)
		}
		conn, err := socksDialer.Dial("tcp", targetAddr)
		if err != nil {
			return nil, fmt.Errorf("SOCKS5 代理连接失败: %w", err)
		}
		return conn, nil
	}

	// Bug 5: HTTP CONNECT 隧道使用 stdlib http.Request + http.ReadResponse
	conn, err := dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("连接代理失败: %w", err)
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
		return nil, fmt.Errorf("发送 CONNECT 失败: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, connectReq)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("读取代理响应失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("代理 CONNECT 失败: %s", resp.Status)
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
