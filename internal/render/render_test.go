package render

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
	"gopkg.in/yaml.v3"
)

// twoEnabledNodes is the fixed input shared by R1, R3-R8: two enabled
// nodes, field values chosen arbitrarily but fixed for determinism.
var twoEnabledNodes = []nodes.Node{
	{Name: "JP-HY2", Server: "1.1.1.1", Port: 443, Password: "PASSWORD", ObfsPassword: "OBFS_PASSWORD", SNI: "www.bing.com", Enabled: true},
	{Name: "SG-HY2", Server: "2.2.2.2", Port: 443, Password: "PASSWORD2", ObfsPassword: "OBFS_PASSWORD2", SNI: "www.bing.com", Enabled: true},
}

// clashConfig is a minimal decode target for the tests in this file.
type clashConfig struct {
	Proxies []struct {
		Name string `yaml:"name"`
	} `yaml:"proxies"`
	ProxyGroups []struct {
		Name    string   `yaml:"name"`
		Type    string   `yaml:"type"`
		Proxies []string `yaml:"proxies"`
	} `yaml:"proxy-groups"`
	RuleProviders map[string]struct {
		Type     string `yaml:"type"`
		Behavior string `yaml:"behavior"`
		URL      string `yaml:"url"`
		Path     string `yaml:"path"`
		Interval int    `yaml:"interval"`
	} `yaml:"rule-providers"`
	Rules []string `yaml:"rules"`
}

func decodeClash(t *testing.T, out string) clashConfig {
	t.Helper()
	var cfg clashConfig
	if err := yaml.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("Clash output is not valid YAML: %v", err)
	}
	return cfg
}

// wantGroupNames is the literal 14-group name order transcribed from
// design §3.1 (基础组) and §3.2 (服务组).
var wantGroupNames = []string{
	"节点选择", "自动选择", "故障转移",
	"AI服务", "GitHub", "流媒体", "Disney", "TikTok", "Telegram", "Google",
	"Bilibili", "Apple", "Microsoft", "游戏",
}

// reservedNamesLiteral is the 16-item reserved name list transcribed
// from design §3.3: the 14 group names plus DIRECT and REJECT.
var reservedNamesLiteral = []string{
	"节点选择", "自动选择", "故障转移",
	"AI服务", "GitHub", "流媒体", "Disney", "TikTok", "Telegram", "Google",
	"Bilibili", "Apple", "Microsoft", "游戏",
	"DIRECT", "REJECT",
}

// R1: proxy-groups is exactly 14 entries, names and order per §3.1/§3.2.
func TestClashProxyGroupNamesAndOrder(t *testing.T) {
	cfg := decodeClash(t, Clash(twoEnabledNodes))
	if len(cfg.ProxyGroups) != len(wantGroupNames) {
		t.Fatalf("expected %d proxy-groups, got %d: %+v", len(wantGroupNames), len(cfg.ProxyGroups), cfg.ProxyGroups)
	}
	for i, name := range wantGroupNames {
		if cfg.ProxyGroups[i].Name != name {
			t.Errorf("proxy-groups[%d] = %q, want %q", i, cfg.ProxyGroups[i].Name, name)
		}
	}
}

// R3: the 11 service groups' first member equals the default egress
// from design §3.2.
func TestClashServiceGroupDefaultEgress(t *testing.T) {
	cfg := decodeClash(t, Clash(twoEnabledNodes))
	wantDefault := map[string]string{
		"AI服务":      "节点选择",
		"GitHub":    "节点选择",
		"流媒体":       "节点选择",
		"Disney":    "节点选择",
		"TikTok":    "节点选择",
		"Telegram":  "节点选择",
		"Google":    "节点选择",
		"Bilibili":  "DIRECT",
		"Apple":     "DIRECT",
		"Microsoft": "DIRECT",
		"游戏":        "DIRECT",
	}
	found := map[string]string{}
	for _, g := range cfg.ProxyGroups {
		if _, ok := wantDefault[g.Name]; ok {
			if len(g.Proxies) == 0 {
				t.Fatalf("group %q has no proxies", g.Name)
			}
			found[g.Name] = g.Proxies[0]
		}
	}
	for name, want := range wantDefault {
		got, ok := found[name]
		if !ok {
			t.Errorf("service group %q not found in output", name)
			continue
		}
		if got != want {
			t.Errorf("group %q first member = %q, want %q", name, got, want)
		}
	}
}

// wantRules is the complete 44-line rule list transcribed verbatim
// from design §4.2.
var wantRules = []string{
	"DOMAIN-SUFFIX,chatgpt.com,AI服务",
	"DOMAIN-SUFFIX,openai.com,AI服务",
	"DOMAIN-SUFFIX,oaistatic.com,AI服务",
	"DOMAIN-SUFFIX,oaiusercontent.com,AI服务",
	"DOMAIN-SUFFIX,anthropic.com,AI服务",
	"DOMAIN-SUFFIX,claude.ai,AI服务",
	"RULE-SET,openai,AI服务",
	"RULE-SET,anthropic,AI服务",
	"DOMAIN-SUFFIX,github.com,GitHub",
	"DOMAIN-SUFFIX,githubusercontent.com,GitHub",
	"RULE-SET,github,GitHub",
	"RULE-SET,youtube,流媒体",
	"RULE-SET,netflix,流媒体",
	"RULE-SET,spotify,流媒体",
	"RULE-SET,disney,Disney",
	"RULE-SET,tiktok,TikTok",
	"RULE-SET,telegram,Telegram",
	"RULE-SET,telegram-ip,Telegram,no-resolve",
	"DOMAIN,play.googleapis.com,Google",
	"DOMAIN,android.clients.google.com,Google",
	"DOMAIN,android.googleapis.com,Google",
	"DOMAIN-SUFFIX,play.google.com,Google",
	"DOMAIN-SUFFIX,googleplay.com,Google",
	"DOMAIN-SUFFIX,googleapis.com,Google",
	"DOMAIN-SUFFIX,googleapis.cn,Google",
	"DOMAIN-SUFFIX,gvt1.com,Google",
	"DOMAIN-SUFFIX,gvt2.com,Google",
	"DOMAIN-SUFFIX,ggpht.com,Google",
	"DOMAIN-SUFFIX,googleusercontent.com,Google",
	"DOMAIN-SUFFIX,googleusercontent.cn,Google",
	"DOMAIN-SUFFIX,android.com,Google",
	"DOMAIN-SUFFIX,google.com,Google",
	"RULE-SET,google,Google",
	"RULE-SET,bilibili,Bilibili",
	"RULE-SET,apple,Apple",
	"RULE-SET,microsoft,Microsoft",
	"RULE-SET,category-games,游戏",
	"RULE-SET,private,DIRECT",
	"RULE-SET,apple-cn,DIRECT",
	"RULE-SET,cn,DIRECT",
	"RULE-SET,geolocation-cn,DIRECT",
	"RULE-SET,geolocation-not-cn,节点选择",
	"GEOIP,CN,DIRECT",
	"MATCH,节点选择",
}

// R4: rules list matches design §4.2 exactly (count, content, order).
func TestClashRulesExactList(t *testing.T) {
	cfg := decodeClash(t, Clash(twoEnabledNodes))
	if len(cfg.Rules) != len(wantRules) {
		t.Fatalf("expected %d rules, got %d: %v", len(wantRules), len(cfg.Rules), cfg.Rules)
	}
	for i, want := range wantRules {
		if cfg.Rules[i] != want {
			t.Errorf("rules[%d] = %q, want %q", i, cfg.Rules[i], want)
		}
	}
}

// R5: order constraints from design §4.2.
func TestClashRuleOrderConstraints(t *testing.T) {
	cfg := decodeClash(t, Clash(twoEnabledNodes))
	rules := cfg.Rules

	indicesWithTargetSuffix := func(suffix string) []int {
		var idx []int
		for i, r := range rules {
			if strings.HasSuffix(r, suffix) {
				idx = append(idx, i)
			}
		}
		return idx
	}
	maxOf := func(idx []int) int {
		m := -1
		for _, i := range idx {
			if i > m {
				m = i
			}
		}
		return m
	}
	minOf := func(idx []int) int {
		m := len(rules)
		for _, i := range idx {
			if i < m {
				m = i
			}
		}
		return m
	}

	streaming := indicesWithTargetSuffix(",流媒体")
	google := indicesWithTargetSuffix(",Google")
	if len(streaming) == 0 || len(google) == 0 {
		t.Fatalf("expected both streaming and Google rules to exist: streaming=%v google=%v", streaming, google)
	}
	if maxOf(streaming) >= minOf(google) {
		t.Errorf("expected all 流媒体 rules before all Google rules: streaming=%v google=%v", streaming, google)
	}

	ai := indicesWithTargetSuffix(",AI服务")
	github := indicesWithTargetSuffix(",GitHub")
	microsoft := indicesWithTargetSuffix(",Microsoft")
	if len(ai) == 0 || len(github) == 0 || len(microsoft) == 0 {
		t.Fatalf("expected AI服务, GitHub and Microsoft rules to exist")
	}
	aiGithub := append(append([]int{}, ai...), github...)
	if maxOf(aiGithub) >= minOf(microsoft) {
		t.Errorf("expected AI服务/GitHub rules before Microsoft rules: aiGithub=%v microsoft=%v", aiGithub, microsoft)
	}

	games := indicesWithTargetSuffix(",游戏")
	if len(games) == 0 {
		t.Fatalf("expected 游戏 rules to exist")
	}
	if maxOf(microsoft) >= minOf(games) {
		t.Errorf("expected Microsoft rules before 游戏 rules: microsoft=%v games=%v", microsoft, games)
	}

	serviceGroupSuffixes := []string{",AI服务", ",GitHub", ",流媒体", ",Disney", ",TikTok", ",Telegram", ",Telegram,no-resolve", ",Google", ",Bilibili", ",Apple", ",Microsoft", ",游戏"}
	var service []int
	for _, s := range serviceGroupSuffixes {
		service = append(service, indicesWithTargetSuffix(s)...)
	}
	privateIdx := -1
	for i, r := range rules {
		if r == "RULE-SET,private,DIRECT" {
			privateIdx = i
			break
		}
	}
	if privateIdx < 0 {
		t.Fatalf("expected rule \"RULE-SET,private,DIRECT\" to exist")
	}
	if maxOf(service) >= privateIdx {
		t.Errorf("expected all service rules before RULE-SET,private,DIRECT (index %d): service=%v", privateIdx, service)
	}
}

// wantRuleProviders is the literal rule-provider table transcribed
// from design §4.1.
var wantRuleProviders = []struct {
	Name     string
	Behavior string
	URL      string
	Path     string
	Interval int
}{
	{"private", "domain", "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/private.yaml", "./ruleset/private.yaml", 86400},
	{"cn", "domain", "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/cn.yaml", "./ruleset/cn.yaml", 86400},
	{"geolocation-cn", "domain", "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/geolocation-cn.yaml", "./ruleset/geolocation-cn.yaml", 86400},
	{"geolocation-not-cn", "domain", "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/geolocation-!cn.yaml", "./ruleset/geolocation-not-cn.yaml", 86400},
	{"google", "domain", "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/google.yaml", "./ruleset/google.yaml", 86400},
	{"openai", "domain", "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/openai.yaml", "./ruleset/openai.yaml", 86400},
	{"anthropic", "domain", "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/anthropic.yaml", "./ruleset/anthropic.yaml", 86400},
	{"github", "domain", "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/github.yaml", "./ruleset/github.yaml", 86400},
	{"apple-cn", "domain", "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/apple-cn.yaml", "./ruleset/apple-cn.yaml", 86400},
	{"youtube", "domain", "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/youtube.yaml", "./ruleset/youtube.yaml", 86400},
	{"netflix", "domain", "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/netflix.yaml", "./ruleset/netflix.yaml", 86400},
	{"spotify", "domain", "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/spotify.yaml", "./ruleset/spotify.yaml", 86400},
	{"disney", "domain", "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/disney.yaml", "./ruleset/disney.yaml", 86400},
	{"tiktok", "domain", "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/tiktok.yaml", "./ruleset/tiktok.yaml", 86400},
	{"telegram", "domain", "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/telegram.yaml", "./ruleset/telegram.yaml", 86400},
	{"telegram-ip", "ipcidr", "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geoip/telegram.yaml", "./ruleset/telegram-ip.yaml", 86400},
	{"bilibili", "domain", "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/bilibili.yaml", "./ruleset/bilibili.yaml", 86400},
	{"apple", "domain", "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/apple.yaml", "./ruleset/apple.yaml", 86400},
	{"microsoft", "domain", "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/microsoft.yaml", "./ruleset/microsoft.yaml", 86400},
	{"category-games", "domain", "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/category-games.yaml", "./ruleset/category-games.yaml", 86400},
}

// R6: rule-providers name set, behavior, url, path, interval match
// design §4.1 exactly.
func TestClashRuleProvidersMatchTable(t *testing.T) {
	cfg := decodeClash(t, Clash(twoEnabledNodes))
	if len(cfg.RuleProviders) != len(wantRuleProviders) {
		t.Fatalf("expected %d rule-providers, got %d: %+v", len(wantRuleProviders), len(cfg.RuleProviders), cfg.RuleProviders)
	}
	for _, want := range wantRuleProviders {
		got, ok := cfg.RuleProviders[want.Name]
		if !ok {
			t.Errorf("rule-providers missing %q", want.Name)
			continue
		}
		if got.Type != "http" {
			t.Errorf("rule-provider %q type = %q, want %q", want.Name, got.Type, "http")
		}
		if got.Behavior != want.Behavior {
			t.Errorf("rule-provider %q behavior = %q, want %q", want.Name, got.Behavior, want.Behavior)
		}
		if got.URL != want.URL {
			t.Errorf("rule-provider %q url = %q, want %q", want.Name, got.URL, want.URL)
		}
		if got.Path != want.Path {
			t.Errorf("rule-provider %q path = %q, want %q", want.Name, got.Path, want.Path)
		}
		if got.Interval != want.Interval {
			t.Errorf("rule-provider %q interval = %d, want %d", want.Name, got.Interval, want.Interval)
		}
	}
}

// r7Baseline is the v0.7.1 render output (up to, but not including,
// "\nproxy-groups:") for twoEnabledNodes, generated from the
// unmodified render.go at commit 5e26383 (see task-1-report.md for
// the exact generation procedure). It must remain byte-identical in
// the v0.8 output (design §4.3).
const r7Baseline = `mixed-port: 7890
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
  - name: "JP-HY2"
    type: hysteria2
    server: "1.1.1.1"
    port: 443
    password: "PASSWORD"
    obfs: salamander
    obfs-password: "OBFS_PASSWORD"
    sni: "www.bing.com"
    skip-cert-verify: true
    alpn: [h3]
  - name: "SG-HY2"
    type: hysteria2
    server: "2.2.2.2"
    port: 443
    password: "PASSWORD2"
    obfs: salamander
    obfs-password: "OBFS_PASSWORD2"
    sni: "www.bing.com"
    skip-cert-verify: true
    alpn: [h3]
`

// R7: output before "\nproxy-groups:" equals the v0.7.1 baseline byte
// for byte (design §4.3, unchanged tun/dns/proxies).
func TestClashUnchangedPrefixMatchesV071Baseline(t *testing.T) {
	out := Clash(twoEnabledNodes)
	idx := strings.Index(out, "\nproxy-groups:")
	if idx < 0 {
		t.Fatalf("output does not contain \"\\nproxy-groups:\" marker")
	}
	got := out[:idx]
	if got != r7Baseline {
		t.Errorf("prefix before proxy-groups changed from v0.7.1 baseline\n--- got ---\n%s\n--- want ---\n%s", got, r7Baseline)
	}
}

// R8: same input renders byte-identical output twice.
func TestClashDeterministic(t *testing.T) {
	a := Clash(twoEnabledNodes)
	b := Clash(twoEnabledNodes)
	if a != b {
		t.Errorf("Clash output is not deterministic across repeated calls with the same input")
	}
}

// R9: each of the 16 reserved names used as an enabled node's name
// triggers a conflict error containing that name.
func TestCheckNodeNamesRejectsReservedNames(t *testing.T) {
	for _, name := range reservedNamesLiteral {
		name := name
		t.Run(name, func(t *testing.T) {
			ns := []nodes.Node{{Name: name, Server: "1.2.3.4", Port: 443, Password: "p", ObfsPassword: "o", SNI: "example.com", Enabled: true}}
			err := CheckNodeNames(ns)
			if err == nil {
				t.Fatalf("expected error for reserved name %q, got nil", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error %q does not contain node name %q", err.Error(), name)
			}
		})
	}
}

// R10: a reserved name on a disabled node does not conflict.
func TestCheckNodeNamesIgnoresDisabledNodes(t *testing.T) {
	ns := []nodes.Node{
		{Name: "GitHub", Server: "1.2.3.4", Port: 443, Password: "p", ObfsPassword: "o", SNI: "example.com", Enabled: false},
		{Name: "JP-HY2", Server: "5.6.7.8", Port: 443, Password: "p2", ObfsPassword: "o2", SNI: "example.com", Enabled: true},
	}
	if err := CheckNodeNames(ns); err != nil {
		t.Errorf("expected nil error when reserved name only used by a disabled node, got %v", err)
	}
}

// TestReservedNamesContract verifies the exported ReservedNames()
// contract: exact 16-item list per design §3.3, and a fresh slice on
// every call (mutating the returned slice must not affect the next
// call's result).
func TestReservedNamesContract(t *testing.T) {
	got := ReservedNames()
	if len(got) != len(reservedNamesLiteral) {
		t.Fatalf("ReservedNames() returned %d items, want %d: %v", len(got), len(reservedNamesLiteral), got)
	}
	for i, want := range reservedNamesLiteral {
		if got[i] != want {
			t.Errorf("ReservedNames()[%d] = %q, want %q", i, got[i], want)
		}
	}

	got[0] = "mutated"
	again := ReservedNames()
	if again[0] != reservedNamesLiteral[0] {
		t.Errorf("mutating a returned slice affected a subsequent ReservedNames() call: got %q, want %q", again[0], reservedNamesLiteral[0])
	}
}

// R11: Write does not touch the filesystem when node names conflict
// with a reserved name.
func TestWriteConflictNoFilesystemSideEffects(t *testing.T) {
	conflicting := []nodes.Node{
		{Name: "GitHub", Server: "1.2.3.4", Port: 443, Password: "p", ObfsPassword: "o", SNI: "example.com", Enabled: true},
	}

	t.Run("preexisting files unchanged", func(t *testing.T) {
		dir := t.TempDir()
		clashPath := filepath.Join(dir, "clash.yaml")
		srPath := filepath.Join(dir, "sr.txt")
		wantClash := []byte("preexisting clash.yaml\n")
		wantSR := []byte("preexisting sr.txt\n")
		if err := os.WriteFile(clashPath, wantClash, 0644); err != nil {
			t.Fatalf("failed to seed clash.yaml: %v", err)
		}
		if err := os.WriteFile(srPath, wantSR, 0644); err != nil {
			t.Fatalf("failed to seed sr.txt: %v", err)
		}

		if err := Write(dir, conflicting); err == nil {
			t.Fatalf("expected error from Write with a reserved node name, got nil")
		}

		gotClash, err := os.ReadFile(clashPath)
		if err != nil {
			t.Fatalf("failed to read clash.yaml after conflict: %v", err)
		}
		if !bytes.Equal(gotClash, wantClash) {
			t.Errorf("clash.yaml changed after Write conflict: got %q, want %q", gotClash, wantClash)
		}
		gotSR, err := os.ReadFile(srPath)
		if err != nil {
			t.Fatalf("failed to read sr.txt after conflict: %v", err)
		}
		if !bytes.Equal(gotSR, wantSR) {
			t.Errorf("sr.txt changed after Write conflict: got %q, want %q", gotSR, wantSR)
		}
	})

	t.Run("output directory not created", func(t *testing.T) {
		parent := t.TempDir()
		dir := filepath.Join(parent, "does-not-exist-yet")

		if err := Write(dir, conflicting); err == nil {
			t.Fatalf("expected error from Write with a reserved node name, got nil")
		}

		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("expected output directory %q to not exist after conflict, stat err=%v", dir, err)
		}
	})
}
