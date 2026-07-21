package render

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/YOUR_GITHUB_USERNAME/proxy-installer/internal/nodes"
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
	b.WriteString("\nproxy-groups:\n  - name: 节点选择\n    type: select\n    proxies:\n      - 自动选择\n")
	for _, n := range ns {
		fmt.Fprintf(&b, "      - %q\n", n.Name)
	}
	b.WriteString("      - DIRECT\n\n  - name: 自动选择\n    type: url-test\n    url: http://www.gstatic.com/generate_204\n    interval: 300\n    tolerance: 50\n    proxies:\n")
	for _, n := range ns {
		fmt.Fprintf(&b, "      - %q\n", n.Name)
	}
	b.WriteString(`

rule-providers:
  private:
    type: http
    behavior: domain
    url: https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/private.yaml
    path: ./ruleset/private.yaml
    interval: 86400
  cn:
    type: http
    behavior: domain
    url: https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/cn.yaml
    path: ./ruleset/cn.yaml
    interval: 86400
  geolocation-cn:
    type: http
    behavior: domain
    url: https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/geolocation-cn.yaml
    path: ./ruleset/geolocation-cn.yaml
    interval: 86400
  geolocation-not-cn:
    type: http
    behavior: domain
    url: https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/geolocation-!cn.yaml
    path: ./ruleset/geolocation-not-cn.yaml
    interval: 86400
  google:
    type: http
    behavior: domain
    url: https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/google.yaml
    path: ./ruleset/google.yaml
    interval: 86400
  openai:
    type: http
    behavior: domain
    url: https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/openai.yaml
    path: ./ruleset/openai.yaml
    interval: 86400
  anthropic:
    type: http
    behavior: domain
    url: https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/anthropic.yaml
    path: ./ruleset/anthropic.yaml
    interval: 86400
  github:
    type: http
    behavior: domain
    url: https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/github.yaml
    path: ./ruleset/github.yaml
    interval: 86400
  apple-cn:
    type: http
    behavior: domain
    url: https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/apple-cn.yaml
    path: ./ruleset/apple-cn.yaml
    interval: 86400

rules:
  - DOMAIN,play.googleapis.com,节点选择
  - DOMAIN,android.clients.google.com,节点选择
  - DOMAIN,android.googleapis.com,节点选择
  - DOMAIN-SUFFIX,play.google.com,节点选择
  - DOMAIN-SUFFIX,googleplay.com,节点选择
  - DOMAIN-SUFFIX,googleapis.com,节点选择
  - DOMAIN-SUFFIX,googleapis.cn,节点选择
  - DOMAIN-SUFFIX,gvt1.com,节点选择
  - DOMAIN-SUFFIX,gvt2.com,节点选择
  - DOMAIN-SUFFIX,ggpht.com,节点选择
  - DOMAIN-SUFFIX,googleusercontent.com,节点选择
  - DOMAIN-SUFFIX,googleusercontent.cn,节点选择
  - DOMAIN-SUFFIX,android.com,节点选择
  - DOMAIN-SUFFIX,google.com,节点选择
  - DOMAIN-SUFFIX,chatgpt.com,节点选择
  - DOMAIN-SUFFIX,openai.com,节点选择
  - DOMAIN-SUFFIX,oaistatic.com,节点选择
  - DOMAIN-SUFFIX,oaiusercontent.com,节点选择
  - DOMAIN-SUFFIX,anthropic.com,节点选择
  - DOMAIN-SUFFIX,claude.ai,节点选择
  - DOMAIN-SUFFIX,github.com,节点选择
  - DOMAIN-SUFFIX,githubusercontent.com,节点选择
  - RULE-SET,google,节点选择
  - RULE-SET,openai,节点选择
  - RULE-SET,anthropic,节点选择
  - RULE-SET,github,节点选择
  - RULE-SET,private,DIRECT
  - RULE-SET,apple-cn,DIRECT
  - RULE-SET,cn,DIRECT
  - RULE-SET,geolocation-cn,DIRECT
  - RULE-SET,geolocation-not-cn,节点选择
  - GEOIP,CN,DIRECT
  - MATCH,节点选择
`)
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
	if err := os.MkdirAll(output, 0755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(output, "clash.yaml"), []byte(Clash(ns)), 0644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(output, "sr.txt"), []byte(Shadowrocket(ns)), 0644)
}
