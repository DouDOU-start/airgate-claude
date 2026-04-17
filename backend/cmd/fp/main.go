// Command fp — TLS/HTTP 指纹取证与对比工具
//
// 子命令：
//
//	fp capture --out <file>             # 导出当前 buildBunClientHelloSpec 的 baseline
//	fp verify  --baseline <a> --sample <b>   # 对比两份 baseline，失败时非零退出
//
// 典型用法：
//
//	go run ./backend/cmd/fp capture --out baselines/2.1.112.json
//	go run ./backend/cmd/fp verify --baseline baselines/2.1.112.json \
//	    --sample baselines/current.json
//
// 设计说明：
//   - capture 不做真实拨号，只把"我们即将发送的 ClientHello 规格"落盘，
//     用来在 CI 中防止 uTLS 升级 / Bun 版本迁移偷偷改动指纹。
//   - verify 做字段级 diff：cipher 顺序、extension 顺序、曲线、签名算法、ALPN、
//     TLS version 边界。
//
// 真实在线抓包对照放到后续 P3.1：利用 Wireshark 导出的 pcap 反向验证。
package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"

	utls "github.com/refraction-networking/utls"

	"github.com/DouDOU-start/airgate-claude/backend/internal/gateway"
)

// snapshot 即将写盘的指纹快照
type snapshot struct {
	CLIVersion          string   `json:"cli_version"`
	Runtime             string   `json:"runtime"`
	RuntimeVersion      string   `json:"runtime_version"`
	TLSVersionMin       uint16   `json:"tls_version_min"`
	TLSVersionMax       uint16   `json:"tls_version_max"`
	CipherSuites        []uint16 `json:"cipher_suites"`
	Curves              []uint16 `json:"curves"`
	SignatureAlgorithms []uint16 `json:"signature_algorithms"`
	ALPNProtocols       []string `json:"alpn_protocols"`
	Extensions          []string `json:"extensions"`
	ExtensionIDs        []uint16 `json:"extension_ids"`
	JA3                 string   `json:"ja3"`
	JA3Hash             string   `json:"ja3_hash"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "capture":
		runCapture(os.Args[2:])
	case "verify":
		runVerify(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage:
  fp capture --out <file>
  fp verify  --baseline <file> --sample <file>`)
}

// ──────────────────────────────────────────────────────
// capture
// ──────────────────────────────────────────────────────

func runCapture(args []string) {
	fs := flag.NewFlagSet("capture", flag.ExitOnError)
	out := fs.String("out", "", "output JSON file path")
	_ = fs.Parse(args)

	if *out == "" {
		fmt.Fprintln(os.Stderr, "--out is required")
		os.Exit(2)
	}

	snap := snapshotFromSpec()
	buf, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, buf, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d bytes)\n", *out, len(buf))
}

// extensionID 把 uTLS 扩展对象映射到 IANA 扩展 ID（用于 JA3）
func extensionID(e utls.TLSExtension) (uint16, bool) {
	switch e.(type) {
	case *utls.SNIExtension:
		return 0, true
	case *utls.StatusRequestExtension:
		return 5, true
	case *utls.SupportedCurvesExtension:
		return 10, true
	case *utls.SupportedPointsExtension:
		return 11, true
	case *utls.SignatureAlgorithmsExtension:
		return 13, true
	case *utls.ALPNExtension:
		return 16, true
	case *utls.SCTExtension:
		return 18, true
	case *utls.ExtendedMasterSecretExtension:
		return 23, true
	case *utls.SessionTicketExtension:
		return 35, true
	case *utls.SupportedVersionsExtension:
		return 43, true
	case *utls.PSKKeyExchangeModesExtension:
		return 45, true
	case *utls.KeyShareExtension:
		return 51, true
	case *utls.GREASEEncryptedClientHelloExtension:
		return 65037, true
	case *utls.RenegotiationInfoExtension:
		return 65281, true
	}
	return 0, false
}

func snapshotFromSpec() snapshot {
	spec := gateway.ExportBunClientHelloSpec()

	curves := []uint16{}
	sigAlgs := []uint16{}
	alpn := []string{}
	exts := []string{}
	extIDs := []uint16{}

	for _, e := range spec.Extensions {
		exts = append(exts, fmt.Sprintf("%T", e))
		if id, ok := extensionID(e); ok {
			extIDs = append(extIDs, id)
		}
		switch v := e.(type) {
		case *utls.SupportedCurvesExtension:
			for _, c := range v.Curves {
				curves = append(curves, uint16(c))
			}
		case *utls.SignatureAlgorithmsExtension:
			for _, s := range v.SupportedSignatureAlgorithms {
				sigAlgs = append(sigAlgs, uint16(s))
			}
		case *utls.ALPNExtension:
			alpn = append(alpn, v.AlpnProtocols...)
		}
	}

	ja3 := buildJA3(spec.TLSVersMax, spec.CipherSuites, extIDs, curves)
	sum := md5.Sum([]byte(ja3))

	return snapshot{
		CLIVersion:          gateway.ClaudeCliVersion,
		Runtime:             "bun",
		RuntimeVersion:      gateway.BunRuntimeVersion,
		TLSVersionMin:       spec.TLSVersMin,
		TLSVersionMax:       spec.TLSVersMax,
		CipherSuites:        append([]uint16(nil), spec.CipherSuites...),
		Curves:              curves,
		SignatureAlgorithms: sigAlgs,
		ALPNProtocols:       alpn,
		Extensions:          exts,
		ExtensionIDs:        extIDs,
		JA3:                 ja3,
		JA3Hash:             hex.EncodeToString(sum[:]),
	}
}

// buildJA3 按 salesforce/ja3 规范拼装：
// TLSVersion,Ciphers,Extensions,EllipticCurves,EllipticCurvePointFormats
// 其中 EllipticCurvePointFormats 固定 "0"（uncompressed）— Bun 行为。
func buildJA3(version uint16, ciphers []uint16, exts []uint16, curves []uint16) string {
	// ClientHello legacy_version 字段：TLSVersMax=1.3 时实际发送 0x0303 (771)
	// JA3 字符串的第一段是这个 legacy version。
	legacy := uint16(0x0303)
	if version < 0x0303 {
		legacy = version
	}

	// JA3 要求过滤 GREASE 值（uTLS 运行时生成，取样期不稳定）。
	// 但 extensionID 我们没返回 GREASE（*utls.UtlsGREASEExtension 未映射），
	// curves/ciphers 本身也不含 GREASE，所以这里直接拼即可。
	join := func(xs []uint16) string {
		parts := make([]string, 0, len(xs))
		for _, x := range xs {
			parts = append(parts, strconv.Itoa(int(x)))
		}
		return strings.Join(parts, "-")
	}

	return fmt.Sprintf("%d,%s,%s,%s,0",
		legacy,
		join(ciphers),
		join(exts),
		join(curves),
	)
}

// ──────────────────────────────────────────────────────
// verify
// ──────────────────────────────────────────────────────

func runVerify(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	baseline := fs.String("baseline", "", "baseline JSON")
	sample := fs.String("sample", "", "sample JSON (缺省时对 live spec 取样)")
	_ = fs.Parse(args)

	if *baseline == "" {
		fmt.Fprintln(os.Stderr, "--baseline is required")
		os.Exit(2)
	}

	b, err := loadSnapshot(*baseline)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load baseline: %v\n", err)
		os.Exit(1)
	}

	var s snapshot
	if *sample != "" {
		s, err = loadSnapshot(*sample)
		if err != nil {
			fmt.Fprintf(os.Stderr, "load sample: %v\n", err)
			os.Exit(1)
		}
	} else {
		s = snapshotFromSpec()
	}

	diffs := diffSnapshots(b, s)
	if len(diffs) == 0 {
		fmt.Println("OK: fingerprint matches baseline")
		return
	}

	fmt.Fprintf(os.Stderr, "FAIL: %d field(s) diverged from baseline\n", len(diffs))
	sort.Strings(diffs)
	for _, d := range diffs {
		fmt.Fprintln(os.Stderr, "  -", d)
	}
	os.Exit(1)
}

func loadSnapshot(path string) (snapshot, error) {
	var s snapshot
	buf, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(buf, &s); err != nil {
		return s, err
	}
	return s, nil
}

func diffSnapshots(a, b snapshot) []string {
	var out []string
	check := func(name string, x, y any) {
		if !reflect.DeepEqual(x, y) {
			out = append(out, fmt.Sprintf("%s: baseline=%v sample=%v", name, x, y))
		}
	}

	check("cli_version", a.CLIVersion, b.CLIVersion)
	check("runtime", a.Runtime, b.Runtime)
	check("runtime_version", a.RuntimeVersion, b.RuntimeVersion)
	check("tls_version_min", a.TLSVersionMin, b.TLSVersionMin)
	check("tls_version_max", a.TLSVersionMax, b.TLSVersionMax)
	check("cipher_suites", a.CipherSuites, b.CipherSuites)
	check("curves", a.Curves, b.Curves)
	check("signature_algorithms", a.SignatureAlgorithms, b.SignatureAlgorithms)
	check("alpn_protocols", a.ALPNProtocols, b.ALPNProtocols)
	check("extensions", a.Extensions, b.Extensions)
	check("extension_ids", a.ExtensionIDs, b.ExtensionIDs)
	check("ja3", a.JA3, b.JA3)
	check("ja3_hash", a.JA3Hash, b.JA3Hash)
	return out
}
