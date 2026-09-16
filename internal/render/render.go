package render

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
)

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
		fmt.Fprintf(&b, "  - name: %q\n    type: hysteria2\n    server: %q\n    port: %d\n    password: %q\n    obfs: salamander\n    obfs-password: %q\n    sni: %q\n    skip-cert-verify: true\n    alpn: [h3]\n", n.Name, n.Server, n.Port, n.Password, n.ObfsPassword, n.SNI)
	}

	writeProxyGroups(&b, ns)
	writeRuleProviders(&b)
	writeRules(&b)

	return b.String()
}

func Shadowrocket(ns []nodes.Node) string {
	var b strings.Builder
	for _, n := range ns {
		u := url.URL{Scheme: "hysteria2", User: url.User(n.Password), Host: fmt.Sprintf("%s:%d", n.Server, n.Port), Path: "/", Fragment: n.Name}
		q := u.Query()
		q.Set("sni", n.SNI)
		q.Set("insecure", "1")
		q.Set("obfs", "salamander")
		q.Set("obfs-password", n.ObfsPassword)
		u.RawQuery = q.Encode()
		b.WriteString(u.String())
		b.WriteByte('\n')
	}
	return b.String()
}

func Write(output string, ns []nodes.Node) error {
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
