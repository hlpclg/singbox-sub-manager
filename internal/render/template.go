package render

import (
	"fmt"
	"strings"

	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
)

// policyGroupNames is the fixed 14-group name order from design
// §3.1 (基础组) and §3.2 (服务组).
var policyGroupNames = []string{
	"节点选择", "自动选择", "故障转移",
	"AI服务", "GitHub", "流媒体", "Disney", "TikTok", "Telegram", "Google",
	"Bilibili", "Apple", "Microsoft", "游戏",
}

// serviceGroupDefaultDirect marks the service groups (design §3.2)
// whose default egress is DIRECT rather than 节点选择.
var serviceGroupDefaultDirect = map[string]bool{
	"Bilibili":  true,
	"Apple":     true,
	"Microsoft": true,
	"游戏":        true,
}

const healthCheckURL = "http://www.gstatic.com/generate_204"

type ruleProvider struct {
	Name     string
	Behavior string
	URLPath  string // relative to the MetaCubeX meta-rules-dat geo/ prefix
}

// ruleProviders is the rule-provider table from design §4.1, in table
// order. path is derived as "./ruleset/<Name>.yaml" for every entry.
var ruleProviders = []ruleProvider{
	{"private", "domain", "geosite/private.yaml"},
	{"cn", "domain", "geosite/cn.yaml"},
	{"geolocation-cn", "domain", "geosite/geolocation-cn.yaml"},
	{"geolocation-not-cn", "domain", "geosite/geolocation-!cn.yaml"},
	{"google", "domain", "geosite/google.yaml"},
	{"openai", "domain", "geosite/openai.yaml"},
	{"anthropic", "domain", "geosite/anthropic.yaml"},
	{"github", "domain", "geosite/github.yaml"},
	{"apple-cn", "domain", "geosite/apple-cn.yaml"},
	{"youtube", "domain", "geosite/youtube.yaml"},
	{"netflix", "domain", "geosite/netflix.yaml"},
	{"spotify", "domain", "geosite/spotify.yaml"},
	{"disney", "domain", "geosite/disney.yaml"},
	{"tiktok", "domain", "geosite/tiktok.yaml"},
	{"telegram", "domain", "geosite/telegram.yaml"},
	{"telegram-ip", "ipcidr", "geoip/telegram.yaml"},
	{"bilibili", "domain", "geosite/bilibili.yaml"},
	{"apple", "domain", "geosite/apple.yaml"},
	{"microsoft", "domain", "geosite/microsoft.yaml"},
	{"category-games", "domain", "geosite/category-games.yaml"},
}

const ruleProviderURLPrefix = "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/"

// clashRules is the complete 44-line rule list from design §4.2, in
// order.
var clashRules = []string{
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

type proxyGroup struct {
	Name    string
	Type    string
	Extra   []string // additional "key: value" lines, already unindented
	Members []string
}

// buildProxyGroups assembles the 14 policy groups (design §3.1, §3.2)
// for the given enabled nodes, in fixed order.
func buildProxyGroups(ns []nodes.Node) []proxyGroup {
	nodeMembers := make([]string, len(ns))
	for i, n := range ns {
		nodeMembers[i] = fmt.Sprintf("%q", n.Name)
	}

	groups := make([]proxyGroup, 0, len(policyGroupNames))

	groups = append(groups, proxyGroup{
		Name:    "节点选择",
		Type:    "select",
		Members: concatMembers([]string{"自动选择", "故障转移"}, nodeMembers, []string{"DIRECT"}),
	})
	groups = append(groups, proxyGroup{
		Name: "自动选择",
		Type: "url-test",
		Extra: []string{
			"url: " + healthCheckURL,
			"interval: 300",
			"tolerance: 50",
		},
		Members: nodeMembers,
	})
	groups = append(groups, proxyGroup{
		Name: "故障转移",
		Type: "fallback",
		Extra: []string{
			"url: " + healthCheckURL,
			"interval: 300",
		},
		Members: nodeMembers,
	})

	for _, name := range policyGroupNames[3:] {
		var members []string
		if serviceGroupDefaultDirect[name] {
			members = concatMembers([]string{"DIRECT", "节点选择", "自动选择", "故障转移"}, nodeMembers)
		} else {
			members = concatMembers([]string{"节点选择", "自动选择", "故障转移"}, nodeMembers, []string{"DIRECT"})
		}
		groups = append(groups, proxyGroup{Name: name, Type: "select", Members: members})
	}

	return groups
}

func concatMembers(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// writeProxyGroups writes the "proxy-groups:" section, preceded by a
// blank line, matching the v0.7.1 formatting style.
func writeProxyGroups(b *strings.Builder, ns []nodes.Node) {
	b.WriteString("\nproxy-groups:\n")
	for i, g := range buildProxyGroups(ns) {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("  - name: ")
		b.WriteString(g.Name)
		b.WriteString("\n    type: ")
		b.WriteString(g.Type)
		b.WriteByte('\n')
		for _, e := range g.Extra {
			b.WriteString("    ")
			b.WriteString(e)
			b.WriteByte('\n')
		}
		b.WriteString("    proxies:\n")
		for _, m := range g.Members {
			b.WriteString("      - ")
			b.WriteString(m)
			b.WriteByte('\n')
		}
	}
}

// writeRuleProviders writes the "rule-providers:" section, preceded
// by a blank line, matching the v0.7.1 formatting style.
func writeRuleProviders(b *strings.Builder) {
	b.WriteString("\nrule-providers:\n")
	for _, p := range ruleProviders {
		fmt.Fprintf(b, "  %s:\n    type: http\n    behavior: %s\n    url: %s%s\n    path: ./ruleset/%s.yaml\n    interval: 86400\n",
			p.Name, p.Behavior, ruleProviderURLPrefix, p.URLPath, p.Name)
	}
}

// writeRules writes the "rules:" section, preceded by a blank line,
// matching the v0.7.1 formatting style.
func writeRules(b *strings.Builder) {
	b.WriteString("\nrules:\n")
	for _, r := range clashRules {
		b.WriteString("  - ")
		b.WriteString(r)
		b.WriteByte('\n')
	}
}
