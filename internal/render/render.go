package render

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
)

// Clash renders ns as a Mihomo/Clash Meta config. It assumes every
// node in ns has already passed nodes.Validate; render.Write
// guarantees this in the one place this package calls it from disk.
func Clash(ns []nodes.Node) string {
	var b strings.Builder
	b.WriteString(`mixed-port: 7890
allow-lan: false
mode: rule
log-level: info
ipv6: false

tun:
  enable: true
  stack: mixed
  auto-route: true
  auto-redirect: true
  strict-route: true
  mtu: 1400
  dns-hijack:
    - any:53

dns:
  enable: true
  ipv6: false
  enhanced-mode: fake-ip
  fake-ip-range: 198.18.0.1/16
  nameserver:
    - https://dns.alidns.com/dns-query
    - https://doh.pub/dns-query
  fallback:
    - https://1.1.1.1/dns-query
    - https://8.8.8.8/dns-query
  proxy-server-nameserver:
    - https://1.1.1.1/dns-query
    - https://8.8.8.8/dns-query
  fake-ip-filter:
    - "*.lan"
    - "*.local"
    - "*.msftconnecttest.com"
    - "*.msftncsi.com"

proxies:
`)
	for _, n := range ns {
		switch n.Type {
		case nodes.TypeVlessReality:
			fmt.Fprintf(&b, "  - name: %q\n    type: vless\n    server: %q\n    port: %d\n    uuid: %q\n    network: %s\n    tls: true\n    servername: %q\n    flow: %s\n    client-fingerprint: %s\n    reality-opts:\n      public-key: %q\n      short-id: %q\n",
				n.Name, n.Server, n.Port, n.UUID, nodes.RealityNetwork, n.SNI, nodes.RealityFlow, nodes.RealityFingerprint, n.PublicKey, n.ShortID)
		default:
			fmt.Fprintf(&b, "  - name: %q\n    type: hysteria2\n    server: %q\n    port: %d\n    password: %q\n    obfs: salamander\n    obfs-password: %q\n    sni: %q\n    skip-cert-verify: true\n    alpn: [h3]\n", n.Name, n.Server, n.Port, n.Password, n.ObfsPassword, n.SNI)
		}
	}

	writeProxyGroups(&b, ns)
	writeRuleProviders(&b)
	writeRules(&b)

	return b.String()
}

// Shadowrocket renders ns as a newline-separated list of share URIs,
// one per node in input order. It assumes every node in ns has
// already passed nodes.Validate; render.Write guarantees this in the
// one place this package calls it from disk.
func Shadowrocket(ns []nodes.Node) string {
	var b strings.Builder
	for _, n := range ns {
		host := net.JoinHostPort(n.Server, strconv.Itoa(n.Port))
		var u url.URL
		switch n.Type {
		case nodes.TypeVlessReality:
			u = url.URL{Scheme: "vless", User: url.User(n.UUID), Host: host, Fragment: n.Name}
			q := u.Query()
			q.Set("encryption", "none")
			q.Set("security", "reality")
			q.Set("sni", n.SNI)
			q.Set("fp", nodes.RealityFingerprint)
			q.Set("pbk", n.PublicKey)
			q.Set("sid", n.ShortID)
			q.Set("spx", nodes.RealitySpiderX)
			q.Set("flow", nodes.RealityFlow)
			q.Set("type", nodes.RealityNetwork)
			u.RawQuery = q.Encode()
		default:
			u = url.URL{Scheme: "hysteria2", User: url.User(n.Password), Host: host, Path: "/", Fragment: n.Name}
			q := u.Query()
			q.Set("sni", n.SNI)
			q.Set("insecure", "1")
			q.Set("obfs", "salamander")
			q.Set("obfs-password", n.ObfsPassword)
			u.RawQuery = q.Encode()
		}
		b.WriteString(u.String())
		b.WriteByte('\n')
	}
	return b.String()
}

func Write(output string, ns []nodes.Node) error {
	for _, n := range ns {
		if err := nodes.Validate(n); err != nil {
			return err
		}
	}
	if err := CheckNodeNames(ns); err != nil {
		return err
	}
	if err := os.MkdirAll(output, 0755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(output, "clash.yaml"), []byte(Clash(ns)), 0644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(output, "sr.txt"), []byte(Shadowrocket(ns)), 0644)
}
