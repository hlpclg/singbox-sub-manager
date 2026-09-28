// Package realitynode: config.go implements design spec §8.1's parameter
// validation, the local-address reject list (§8.1 "本机地址"), the
// service-config JSON (§8.1's fixed structure, field-for-field identical to
// Task 1's real sing-box-1.14.1-verified renderServerConfig — see
// probe_integration_test.go and docs/reality-node-validation.md), the fixed
// systemd unit template (§8.1, byte-identical to
// testdata/probe/reality.service with <v> substituted), and §8.1's
// selfcheck_port selection.
package realitynode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
)

// Spec §8.1 fixed defaults and bounds.
const (
	DefaultPort    = 443
	DefaultSNI     = "www.microsoft.com"
	maxHostnameLen = 253
	maxLabelLen    = 63
)

var hostnameLabelRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$`)

// InstallParams are install/reinstall's validated parameters (spec §8.1's
// --name/--server/--port/--sni).
type InstallParams struct {
	Name   string
	Server string
	Port   int
	SNI    string
}

// ValidateInstallParams enforces spec §8.1's parameter rules:
//   - Name: nodes.ValidateName's charset rule, and must not start with '-'.
//   - Server: an IPv4 literal, or a hostname (total length <= 253; each
//     label 1-63 chars of [A-Za-z0-9-], not starting/ending with '-'; at
//     least one '.'). An IPv6 literal is rejected.
//   - Port: 1-65535.
//   - SNI: the same hostname rule as Server, but never an IP literal (v4 or
//     v6).
func ValidateInstallParams(p InstallParams) error {
	if err := nodes.ValidateName(p.Name); err != nil {
		return fmt.Errorf("name: %w", err)
	}
	if strings.HasPrefix(p.Name, "-") {
		return fmt.Errorf("name: must not start with '-'")
	}
	if err := validateServer(p.Server); err != nil {
		return fmt.Errorf("server: %w", err)
	}
	if p.Port < 1 || p.Port > 65535 {
		return fmt.Errorf("port: %d out of range 1-65535", p.Port)
	}
	if err := validateSNI(p.SNI); err != nil {
		return fmt.Errorf("sni: %w", err)
	}
	return nil
}

func validateServer(s string) error {
	if s == "" {
		return errors.New("empty")
	}
	if isIPv6Literal(s) {
		return errors.New("IPv6 literal is not supported")
	}
	if isIPv4Literal(s) {
		return nil
	}
	return validateHostname(s)
}

func validateSNI(s string) error {
	if s == "" {
		return errors.New("empty")
	}
	if isIPv4Literal(s) || isIPv6Literal(s) {
		return errors.New("must be a hostname, not an IP literal")
	}
	return validateHostname(s)
}

func isIPv4Literal(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && strings.Contains(s, ".") && ip.To4() != nil
}

func isIPv6Literal(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && strings.Contains(s, ":")
}

func validateHostname(s string) error {
	if len(s) > maxHostnameLen {
		return fmt.Errorf("hostname %q exceeds %d characters", s, maxHostnameLen)
	}
	if !strings.Contains(s, ".") {
		return fmt.Errorf("hostname %q must contain at least one '.'", s)
	}
	for _, label := range strings.Split(s, ".") {
		if len(label) < 1 || len(label) > maxLabelLen {
			return fmt.Errorf("hostname label %q must be 1-%d characters", label, maxLabelLen)
		}
		if !hostnameLabelRE.MatchString(label) {
			return fmt.Errorf("hostname label %q has invalid characters or leading/trailing '-'", label)
		}
	}
	return nil
}

// --- Local address reject list (spec §8.1 "本机地址") ---

// InterfaceLister returns every address on every non-loopback network
// interface. Production is defaultInterfaceLister; tests inject a fake so
// no test depends on the machine's real network configuration (spec §14.1:
// "接口枚举可注入").
type InterfaceLister func() ([]net.Addr, error)

func defaultInterfaceLister() ([]net.Addr, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []net.Addr
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			return nil, fmt.Errorf("realitynode: list addresses on %s: %w", ifc.Name, err)
		}
		out = append(out, addrs...)
	}
	return out, nil
}

// Resolver resolves a hostname to every A/AAAA address it has (production:
// net.DefaultResolver.LookupIPAddr, bounded by dnsResolveTimeout in addition
// to whatever budget ctx already carries — spec §14.1: "DNS...可注入,DNS受
// 主context取消且有限等待"). Tests inject a fake so no test needs real DNS.
type Resolver func(ctx context.Context, host string) ([]net.IP, error)

// dnsResolveTimeout bounds a single hostname resolution in addition to
// whatever deadline the caller's ctx already carries. The spec does not fix
// an exact figure for this lookup (unlike the other §13.2 timeouts); this
// value is this Task's own implementation choice, documented here rather
// than silently assumed.
const dnsResolveTimeout = 5 * time.Second

func defaultResolver(ctx context.Context, host string) ([]net.IP, error) {
	cctx, cancel := context.WithTimeout(ctx, dnsResolveTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(cctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, len(addrs))
	for i, a := range addrs {
		ips[i] = a.IP
	}
	return ips, nil
}

// LocalAddressCIDRs implements spec §8.1's local-address reject list: every
// address on every non-loopback interface (as /32 or /128), plus every
// address that server resolves to — server, already validated by
// ValidateInstallParams, is either an IPv4 literal (added directly) or a
// hostname (every resolved A/AAAA record added). The result is sorted for
// determinism; duplicates are removed.
func LocalAddressCIDRs(ctx context.Context, server string, ifaces InterfaceLister, resolve Resolver) ([]string, error) {
	if ifaces == nil {
		ifaces = defaultInterfaceLister
	}
	if resolve == nil {
		resolve = defaultResolver
	}

	seen := map[string]bool{}
	var out []string
	add := func(ip net.IP) {
		cidr := ipCIDR(ip)
		if cidr == "" || seen[cidr] {
			return
		}
		seen[cidr] = true
		out = append(out, cidr)
	}

	addrs, err := ifaces()
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		ip := addrIP(a)
		if ip == nil || ip.IsLoopback() {
			continue
		}
		add(ip)
	}

	if isIPv4Literal(server) {
		add(net.ParseIP(server))
	} else {
		resolved, err := resolve(ctx, server)
		if err != nil {
			return nil, fmt.Errorf("realitynode: resolve server %q: %w", server, err)
		}
		for _, ip := range resolved {
			add(ip)
		}
	}

	sort.Strings(out)
	return out, nil
}

func addrIP(a net.Addr) net.IP {
	switch v := a.(type) {
	case *net.IPNet:
		return v.IP
	case *net.IPAddr:
		return v.IP
	default:
		return nil
	}
}

func ipCIDR(ip net.IP) string {
	if ip == nil {
		return ""
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String() + "/32"
	}
	return ip.String() + "/128"
}

// --- Service config JSON (spec §8.1) ---

// rejectCIDRs is spec §8.1's fixed private/reserved-range reject list,
// field-for-field identical to Task 1's real sing-box-1.14.1-verified
// renderServerConfig (probe_integration_test.go) — this exact list has been
// confirmed to parse and enforce correctly against the pinned binary.
var rejectCIDRs = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16",
	"224.0.0.0/4", "240.0.0.0/4",
	"::/128", "::1/128", "::ffff:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48",
	"fc00::/7", "fe80::/10", "ff00::/8",
}

// ServerConfigParams are the runtime values GenerateServerConfig substitutes
// into the fixed structure.
type ServerConfigParams struct {
	ListenPort    int
	SelfcheckPort int
	SNI           string
	UUID          string
	PrivateKey    string
	ShortID       string
	// LocalCIDRs is spec §8.1's local-address reject list (LocalAddressCIDRs'
	// result); a nil/empty slice omits that rule entirely, matching Task 1's
	// renderServerConfig ("if len(p.LocalCIDRs) > 0").
	LocalCIDRs []string
}

// GenerateServerConfig produces spec §8.1's fixed service-config JSON,
// structurally identical to Task 1's real, sing-box-1.14.1-verified
// renderServerConfig (probe_integration_test.go): same inbound/outbound/
// route shape, same rule ordering (selfcheck allow, fixed reject, local
// reject if non-empty, ip_is_private catch-all).
func GenerateServerConfig(p ServerConfigParams) ([]byte, error) {
	rules := []interface{}{
		map[string]interface{}{
			"ip_cidr":  []string{"127.0.0.1/32"},
			"port":     []int{p.SelfcheckPort},
			"outbound": "direct",
		},
		map[string]interface{}{
			"ip_cidr": rejectCIDRs,
			"action":  "reject",
		},
	}
	if len(p.LocalCIDRs) > 0 {
		rules = append(rules, map[string]interface{}{
			"ip_cidr": p.LocalCIDRs,
			"action":  "reject",
		})
	}
	rules = append(rules, map[string]interface{}{
		"ip_is_private": true,
		"action":        "reject",
	})

	cfg := map[string]interface{}{
		"log": map[string]interface{}{"level": "warn", "timestamp": true},
		"inbounds": []interface{}{
			map[string]interface{}{
				"type": "vless", "tag": "reality-in",
				"listen": "0.0.0.0", "listen_port": p.ListenPort,
				"users": []interface{}{
					map[string]interface{}{"uuid": p.UUID, "flow": nodes.RealityFlow},
				},
				"tls": map[string]interface{}{
					"enabled":     true,
					"server_name": p.SNI,
					"reality": map[string]interface{}{
						"enabled":     true,
						"handshake":   map[string]interface{}{"server": p.SNI, "server_port": 443},
						"private_key": p.PrivateKey,
						"short_id":    []string{p.ShortID},
					},
				},
			},
		},
		"outbounds": []interface{}{
			map[string]interface{}{"type": "direct", "tag": "direct"},
		},
		"route": map[string]interface{}{
			"rules": rules,
			"final": "direct",
		},
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// --- systemd unit (spec §8.1) ---

// unitTemplate is byte-identical to testdata/probe/reality.service, the
// Task 1 fixture the plan requires this Task's output stay pinned to
// ("unit字节必须等于T1模板替换<v>后的结果"). Do not edit this string without
// re-running Task 1's real-target validation: any change here is a change
// to the fixed unit spec §8.1 explicitly forbids modifying without
// re-testing on real target OSes.
const unitTemplate = `[Unit]
Description=proxyctl managed VLESS Reality (sing-box)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/lib/proxyctl-reality/versions/<v>/sing-box run -c /etc/proxyctl-reality/config.json
Restart=on-failure
RestartSec=5
TimeoutStartSec=10
TimeoutStopSec=5
LimitNOFILE=1048576
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadOnlyPaths=/etc/proxyctl-reality /usr/local/lib/proxyctl-reality

[Install]
WantedBy=multi-user.target
`

// RenderUnit substitutes version for unitTemplate's <v> placeholder,
// producing spec §8.1's fixed unit content byte-for-byte (verified against
// testdata/probe/reality.service in config_test.go).
func RenderUnit(version string) []byte {
	return []byte(strings.Replace(unitTemplate, "<v>", version, 1))
}

// --- selfcheck_port selection (spec §8.1) ---

var (
	errEmptyPortRange       = errors.New("realitynode: ephemeral port range leaves no room below 10000")
	errSelfcheckPortFailure = errors.New("realitynode: could not find a bindable selfcheck_port in 10 attempts")
)

// PortRangeReader reads the kernel's ephemeral port range (spec §8.1:
// "读取/proc/sys/net/ipv4/ip_local_port_range"). Production is
// defaultPortRangeReader (Linux-only, see config_linux.go); tests inject a
// fake.
type PortRangeReader func() (lo, hi int, err error)

// PortBinder reports whether 127.0.0.1:port can currently be bound.
// Production dials net.Listen("tcp4", ...) and closes immediately; tests
// inject a fake.
type PortBinder func(port int) bool

func defaultPortBinder(port int) bool {
	l, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// RandIntn returns a pseudo-random, non-negative int < n. Production uses
// math/rand's auto-seeded top-level source; tests inject a deterministic
// sequence.
type RandIntn func(n int) int

func defaultRandIntn(n int) int { return rand.Intn(n) }

// SelectSelfcheckPort implements spec §8.1: pick a random port in
// [10000, lo-1] (lo from portRange), avoiding listenPort, confirm it binds
// on 127.0.0.1, at most 10 attempts. An empty range (lo-1 < 10000) or 10
// failed attempts is an error (mapped by the caller to exit code 1).
func SelectSelfcheckPort(listenPort int, portRange PortRangeReader, canBind PortBinder, randIntn RandIntn) (int, error) {
	if portRange == nil {
		portRange = defaultPortRangeReader
	}
	if canBind == nil {
		canBind = defaultPortBinder
	}
	if randIntn == nil {
		randIntn = defaultRandIntn
	}

	lo, _, err := portRange()
	if err != nil {
		return 0, fmt.Errorf("realitynode: read ephemeral port range: %w", err)
	}
	upper := lo - 1
	if upper < 10000 {
		return 0, errEmptyPortRange
	}
	span := upper - 10000 + 1
	for i := 0; i < 10; i++ {
		candidate := 10000 + randIntn(span)
		if candidate == listenPort {
			continue
		}
		if canBind(candidate) {
			return candidate, nil
		}
	}
	return 0, errSelfcheckPortFailure
}
