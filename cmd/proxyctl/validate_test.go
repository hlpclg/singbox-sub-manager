package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
	"github.com/hlpclg/singbox-sub-manager/internal/render"
	"gopkg.in/yaml.v3"
)

// clashGroups is a minimal decode target for R2: only what's needed
// to assert each group's type and member list.
type clashGroups struct {
	ProxyGroups []struct {
		Name    string   `yaml:"name"`
		Type    string   `yaml:"type"`
		Proxies []string `yaml:"proxies"`
	} `yaml:"proxy-groups"`
}

// R2: `merge` output (production path, filtered through nodes.Enabled)
// has each group's type and member list equal to design §3.1/§3.2,
// with the disabled node absent from every group. Transcribed
// literally from the design, not derived from the render package.
func TestMergeProxyGroupsMatchTemplate(t *testing.T) {
	tmp := t.TempDir()
	nodesFile := filepath.Join(tmp, "nodes.conf")
	content := "[JP-HY2]\nSERVER=1.1.1.1\nPORT=443\nPASSWORD=p1\nOBFS_PASSWORD=o1\nSNI=www.bing.com\nENABLED=true\n\n" +
		"[SG-HY2]\nSERVER=2.2.2.2\nPORT=443\nPASSWORD=p2\nOBFS_PASSWORD=o2\nSNI=www.bing.com\nENABLED=true\n\n" +
		"[US-HY2]\nSERVER=3.3.3.3\nPORT=443\nPASSWORD=p3\nOBFS_PASSWORD=o3\nSNI=www.bing.com\nENABLED=false\n"
	if err := os.WriteFile(nodesFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write nodes.conf: %v", err)
	}
	outDir := filepath.Join(tmp, "out")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"merge", "--nodes", nodesFile, "--output", outDir}, &stdout, &stderr); code != 0 {
		t.Fatalf("merge failed with code %d: %s", code, stderr.String())
	}

	data, err := os.ReadFile(filepath.Join(outDir, "clash.yaml"))
	if err != nil {
		t.Fatalf("failed to read generated clash.yaml: %v", err)
	}

	var cfg clashGroups
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("generated clash.yaml is not valid YAML: %v", err)
	}

	wantGroups := []struct {
		Name    string
		Type    string
		Proxies []string
	}{
		{"节点选择", "select", []string{"自动选择", "故障转移", "JP-HY2", "SG-HY2", "DIRECT"}},
		{"自动选择", "url-test", []string{"JP-HY2", "SG-HY2"}},
		{"故障转移", "fallback", []string{"JP-HY2", "SG-HY2"}},
		{"AI服务", "select", []string{"节点选择", "自动选择", "故障转移", "JP-HY2", "SG-HY2", "DIRECT"}},
		{"GitHub", "select", []string{"节点选择", "自动选择", "故障转移", "JP-HY2", "SG-HY2", "DIRECT"}},
		{"流媒体", "select", []string{"节点选择", "自动选择", "故障转移", "JP-HY2", "SG-HY2", "DIRECT"}},
		{"Disney", "select", []string{"节点选择", "自动选择", "故障转移", "JP-HY2", "SG-HY2", "DIRECT"}},
		{"TikTok", "select", []string{"节点选择", "自动选择", "故障转移", "JP-HY2", "SG-HY2", "DIRECT"}},
		{"Telegram", "select", []string{"节点选择", "自动选择", "故障转移", "JP-HY2", "SG-HY2", "DIRECT"}},
		{"Google", "select", []string{"节点选择", "自动选择", "故障转移", "JP-HY2", "SG-HY2", "DIRECT"}},
		{"Bilibili", "select", []string{"DIRECT", "节点选择", "自动选择", "故障转移", "JP-HY2", "SG-HY2"}},
		{"Apple", "select", []string{"DIRECT", "节点选择", "自动选择", "故障转移", "JP-HY2", "SG-HY2"}},
		{"Microsoft", "select", []string{"DIRECT", "节点选择", "自动选择", "故障转移", "JP-HY2", "SG-HY2"}},
		{"游戏", "select", []string{"DIRECT", "节点选择", "自动选择", "故障转移", "JP-HY2", "SG-HY2"}},
	}

	if len(cfg.ProxyGroups) != len(wantGroups) {
		t.Fatalf("expected %d proxy-groups, got %d: %+v", len(wantGroups), len(cfg.ProxyGroups), cfg.ProxyGroups)
	}
	for i, want := range wantGroups {
		got := cfg.ProxyGroups[i]
		if got.Name != want.Name {
			t.Errorf("proxy-groups[%d].name = %q, want %q", i, got.Name, want.Name)
			continue
		}
		if got.Type != want.Type {
			t.Errorf("group %q type = %q, want %q", want.Name, got.Type, want.Type)
		}
		if len(got.Proxies) != len(want.Proxies) {
			t.Errorf("group %q proxies = %v, want %v", want.Name, got.Proxies, want.Proxies)
			continue
		}
		for j, p := range want.Proxies {
			if got.Proxies[j] != p {
				t.Errorf("group %q proxies[%d] = %q, want %q", want.Name, j, got.Proxies[j], p)
			}
		}
		for _, p := range got.Proxies {
			if p == "US-HY2" {
				t.Errorf("disabled node US-HY2 leaked into group %q: %v", want.Name, got.Proxies)
			}
		}
	}
}

func writeNodesConf(t *testing.T, path, name string, enabled bool) {
	t.Helper()
	content := "[" + name + "]\nSERVER=1.2.3.4\nPORT=443\nPASSWORD=p1\nOBFS_PASSWORD=o1\nSNI=www.bing.com\nENABLED=" + boolStr(enabled) + "\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write nodes.conf: %v", err)
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// C1: merge with an enabled node named after a reserved name fails,
// stderr contains the node name, and preexisting output files are
// left byte-unchanged.
func TestMergeReservedNameEnabledFails(t *testing.T) {
	tmp := t.TempDir()
	nodesFile := filepath.Join(tmp, "nodes.conf")
	writeNodesConf(t, nodesFile, "GitHub", true)

	outDir := filepath.Join(tmp, "out")
	if err := os.MkdirAll(outDir, 0755); err != nil {
		t.Fatalf("failed to create output dir: %v", err)
	}
	wantClash := []byte("preexisting clash.yaml\n")
	wantSR := []byte("preexisting sr.txt\n")
	if err := os.WriteFile(filepath.Join(outDir, "clash.yaml"), wantClash, 0644); err != nil {
		t.Fatalf("failed to seed clash.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "sr.txt"), wantSR, 0644); err != nil {
		t.Fatalf("failed to seed sr.txt: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"merge", "--nodes", nodesFile, "--output", outDir}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "GitHub") {
		t.Errorf("expected stderr to contain conflicting node name %q, got %q", "GitHub", stderr.String())
	}

	gotClash, err := os.ReadFile(filepath.Join(outDir, "clash.yaml"))
	if err != nil {
		t.Fatalf("failed to read clash.yaml: %v", err)
	}
	if !bytes.Equal(gotClash, wantClash) {
		t.Errorf("clash.yaml changed after merge conflict: got %q, want %q", gotClash, wantClash)
	}
	gotSR, err := os.ReadFile(filepath.Join(outDir, "sr.txt"))
	if err != nil {
		t.Fatalf("failed to read sr.txt: %v", err)
	}
	if !bytes.Equal(gotSR, wantSR) {
		t.Errorf("sr.txt changed after merge conflict: got %q, want %q", gotSR, wantSR)
	}
}

// C2: a reserved name on a disabled node does not block merge.
func TestMergeReservedNameDisabledSucceeds(t *testing.T) {
	tmp := t.TempDir()
	nodesFile := filepath.Join(tmp, "nodes.conf")
	content := "[GitHub]\nSERVER=1.2.3.4\nPORT=443\nPASSWORD=p1\nOBFS_PASSWORD=o1\nSNI=www.bing.com\nENABLED=false\n\n" +
		"[JP-HY2]\nSERVER=5.6.7.8\nPORT=443\nPASSWORD=p2\nOBFS_PASSWORD=o2\nSNI=www.bing.com\nENABLED=true\n"
	if err := os.WriteFile(nodesFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write nodes.conf: %v", err)
	}
	outDir := filepath.Join(tmp, "out")

	var stdout, stderr bytes.Buffer
	code := run([]string{"merge", "--nodes", nodesFile, "--output", outDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d (stderr=%s)", code, stderr.String())
	}
}

// C3: validate with an enabled node named after a reserved name fails.
func TestValidateReservedNameEnabledFails(t *testing.T) {
	tmp := t.TempDir()
	nodesFile := filepath.Join(tmp, "nodes.conf")
	writeNodesConf(t, nodesFile, "GitHub", true)

	var stdout, stderr bytes.Buffer
	code := run([]string{"validate", "--nodes", nodesFile}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d (stderr=%s)", code, stderr.String())
	}
	if !strings.HasPrefix(stderr.String(), "validation failed: ") {
		t.Errorf("expected stderr to start with %q, got %q", "validation failed: ", stderr.String())
	}
}

// baseValidDoc is a minimal, hand-built clash.yaml used as a scaffold
// for the structural-layer negative tests (C4-C9). It is independent
// of render.Clash, per design §5/§6.2.
const baseValidDocTemplate = `proxies:
  - name: N1
    type: hysteria2
proxy-groups:
  - name: G1
    type: select
    proxies:
%s
rule-providers:
  rp1:
    type: http
    behavior: domain
    url: https://example.com/rp1.yaml
    path: ./ruleset/rp1.yaml
    interval: 86400
rules:
%s
`

func buildDoc(groupMembers []string, rules []string) string {
	var members strings.Builder
	for _, m := range groupMembers {
		members.WriteString("      - " + m + "\n")
	}
	var ruleLines strings.Builder
	for _, r := range rules {
		ruleLines.WriteString("  - " + r + "\n")
	}
	return fillDocTemplate(baseValidDocTemplate, members.String(), ruleLines.String())
}

// fillDocTemplate fills the two %s placeholders in the template.
func fillDocTemplate(tpl, members, rules string) string {
	out := strings.Replace(tpl, "%s", strings.TrimRight(members, "\n"), 1)
	out = strings.Replace(out, "%s", strings.TrimRight(rules, "\n"), 1)
	return out
}

// C4: a group member references a group name that is not defined in
// the YAML (existence is decided solely by the input YAML, not by
// render.ReservedNames()).
func TestValidateClashYAMLUndefinedGroupMember(t *testing.T) {
	doc := buildDoc([]string{"N1", "故障转移", "DIRECT"}, []string{"RULE-SET,rp1,G1", "MATCH,G1"})
	if err := validateClashYAML(doc); err == nil {
		t.Fatal("expected error for group member referencing an undefined group, got nil")
	}
}

// C5: a rule targets a plausible group name that is not defined in
// the YAML, and a rule with too few comma-separated fields.
func TestValidateClashYAMLBadRuleTarget(t *testing.T) {
	t.Run("undefined target group", func(t *testing.T) {
		doc := buildDoc([]string{"N1", "DIRECT"}, []string{"RULE-SET,rp1,G1", "RULE-SET,rp1,流媒体"})
		if err := validateClashYAML(doc); err == nil {
			t.Fatal("expected error for rule targeting an undefined group, got nil")
		}
	})
	t.Run("too few fields", func(t *testing.T) {
		doc := buildDoc([]string{"N1", "DIRECT"}, []string{"RULE-SET,rp1,G1", "MATCH"})
		if err := validateClashYAML(doc); err == nil {
			t.Fatal("expected error for MATCH rule with fewer than 2 fields, got nil")
		}
	})
}

// C6: a group member references a proxy that is not defined anywhere.
func TestValidateClashYAMLUndefinedProxyMember(t *testing.T) {
	doc := buildDoc([]string{"N1", "UnknownProxy", "DIRECT"}, []string{"RULE-SET,rp1,G1", "MATCH,G1"})
	if err := validateClashYAML(doc); err == nil {
		t.Fatal("expected error for group member referencing an undefined proxy, got nil")
	}
}

// C7: a proxy name equal to a group name, and a proxy named DIRECT.
func TestValidateClashYAMLReservedOrConflictingNames(t *testing.T) {
	t.Run("proxy name equals group name", func(t *testing.T) {
		doc := `proxies:
  - name: G1
    type: hysteria2
proxy-groups:
  - name: G1
    type: select
    proxies:
      - DIRECT
rule-providers:
  rp1:
    type: http
    behavior: domain
    url: https://example.com/rp1.yaml
    path: ./ruleset/rp1.yaml
    interval: 86400
rules:
  - RULE-SET,rp1,G1
  - MATCH,G1
`
		if err := validateClashYAML(doc); err == nil {
			t.Fatal("expected error when a proxy name equals a group name, got nil")
		}
	})
	t.Run("proxy name is DIRECT", func(t *testing.T) {
		doc := `proxies:
  - name: DIRECT
    type: hysteria2
proxy-groups:
  - name: G1
    type: select
    proxies:
      - DIRECT
rule-providers:
  rp1:
    type: http
    behavior: domain
    url: https://example.com/rp1.yaml
    path: ./ruleset/rp1.yaml
    interval: 86400
rules:
  - RULE-SET,rp1,G1
  - MATCH,G1
`
		if err := validateClashYAML(doc); err == nil {
			t.Fatal("expected error when a proxy is named DIRECT, got nil")
		}
	})
}

// C8: a RULE-SET rule references a provider not defined in
// rule-providers.
func TestValidateClashYAMLUnknownRuleSetProvider(t *testing.T) {
	doc := buildDoc([]string{"N1", "DIRECT"}, []string{"RULE-SET,unknown,G1", "MATCH,G1"})
	if err := validateClashYAML(doc); err == nil {
		t.Fatal("expected error for RULE-SET referencing an unknown provider, got nil")
	}
}

// C9: a rule-providers entry is never referenced by any RULE-SET rule.
func TestValidateClashYAMLUnreferencedProvider(t *testing.T) {
	doc := `proxies:
  - name: N1
    type: hysteria2
proxy-groups:
  - name: G1
    type: select
    proxies:
      - N1
      - DIRECT
rule-providers:
  rp1:
    type: http
    behavior: domain
    url: https://example.com/rp1.yaml
    path: ./ruleset/rp1.yaml
    interval: 86400
  rp2:
    type: http
    behavior: domain
    url: https://example.com/rp2.yaml
    path: ./ruleset/rp2.yaml
    interval: 86400
rules:
  - RULE-SET,rp1,G1
  - MATCH,G1
`
	if err := validateClashYAML(doc); err == nil {
		t.Fatal("expected error for an unreferenced rule-provider, got nil")
	}
}

// C10: validateClashYAML accepts render.Clash's output for normal
// nodes.
func TestValidateClashYAMLAcceptsRenderedOutput(t *testing.T) {
	ns := []nodes.Node{
		{Name: "JP-HY2", Server: "1.1.1.1", Port: 443, Password: "p1", ObfsPassword: "o1", SNI: "www.bing.com", Enabled: true},
		{Name: "SG-HY2", Server: "2.2.2.2", Port: 443, Password: "p2", ObfsPassword: "o2", SNI: "www.bing.com", Enabled: true},
	}
	if err := validateClashYAML(render.Clash(ns)); err != nil {
		t.Errorf("expected nil error for render.Clash output, got %v", err)
	}
}
