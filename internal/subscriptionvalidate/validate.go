// Package subscriptionvalidate holds the one implementation of the
// subscription-output checks behind `proxyctl validate`. The CLI wrapper
// (cmd/proxyctl) and the v0.10 publish transaction (internal/publish, design
// spec §10.3 step 5: "执行与 validate 相同的校验") both call it, so the two
// can never disagree about what a publishable set of nodes looks like. The
// error texts are exactly the ones `proxyctl validate` has always printed
// after its "validation failed: " prefix.
package subscriptionvalidate

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
	"github.com/hlpclg/singbox-sub-manager/internal/render"
)

// Rendered is the exact pair of subscription documents Validate checked: the
// bytes render.Write would put in clash.yaml and sr.txt for the same nodes.
type Rendered struct {
	Clash        string
	Shadowrocket string
}

// ErrNoEnabledNodes is returned for an empty node set.
var ErrNoEnabledNodes = errors.New("no enabled nodes")

// Validate checks the enabled nodes ns and, when every check passes, returns
// the rendered subscription documents. ns must already be the enabled subset
// (nodes.Enabled): the callers filter first, exactly as `proxyctl validate`
// and `proxyctl merge` always have. The checks and their order are those of
// `proxyctl validate`: non-empty set, per-node validation, reserved-name
// conflicts, the CHANGE_ME placeholder, the Clash structure, then the
// Shadowrocket line count and URI schemes.
func Validate(ns []nodes.Node) (Rendered, error) {
	if len(ns) == 0 {
		return Rendered{}, ErrNoEnabledNodes
	}
	for _, n := range ns {
		if err := nodes.Validate(n); err != nil {
			return Rendered{}, err
		}
	}
	if err := render.CheckNodeNames(ns); err != nil {
		return Rendered{}, err
	}

	clashYaml := render.Clash(ns)
	if strings.Contains(clashYaml, "CHANGE_ME") {
		return Rendered{}, errors.New("generated clash.yaml contains placeholder 'CHANGE_ME'")
	}
	if err := ClashYAML(clashYaml); err != nil {
		return Rendered{}, err
	}

	sr := render.Shadowrocket(ns)
	if strings.TrimSpace(sr) == "" {
		return Rendered{}, errors.New("generated Shadowrocket output is empty")
	}
	lines := strings.Split(strings.TrimSpace(sr), "\n")
	if len(lines) != len(ns) {
		return Rendered{}, fmt.Errorf("generated %d Shadowrocket URI line(s), want %d", len(lines), len(ns))
	}
	for i, l := range lines {
		u, err := url.Parse(l)
		if err != nil {
			return Rendered{}, fmt.Errorf("generated Shadowrocket URI at line %d is invalid: %v", i+1, err)
		}
		wantScheme := "hysteria2"
		if ns[i].Type == nodes.TypeVlessReality {
			wantScheme = "vless"
		}
		if u.Scheme != wantScheme {
			return Rendered{}, fmt.Errorf("generated Shadowrocket URI at line %d has wrong scheme", i+1)
		}
	}
	return Rendered{Clash: clashYaml, Shadowrocket: sr}, nil
}

// clashStructure is the decode target for ClashYAML.
type clashStructure struct {
	Proxies []struct {
		Name string `yaml:"name"`
	} `yaml:"proxies"`
	ProxyGroups []struct {
		Name    string   `yaml:"name"`
		Proxies []string `yaml:"proxies"`
	} `yaml:"proxy-groups"`
	RuleProviders map[string]interface{} `yaml:"rule-providers"`
	Rules         []string               `yaml:"rules"`
}

// ClashYAML implements the YAML structural layer of design §5: it operates
// purely on the given YAML string and does not read render.ReservedNames() or
// any other renderer-internal data. "Does a group exist" is decided solely by
// the proxy-groups defined in doc.
func ClashYAML(doc string) error {
	var cfg clashStructure
	dec := yaml.NewDecoder(strings.NewReader(doc))
	dec.KnownFields(false)
	if err := dec.Decode(&cfg); err != nil {
		return fmt.Errorf("invalid YAML: %w", err)
	}

	proxyNames := make(map[string]bool, len(cfg.Proxies))
	for _, p := range cfg.Proxies {
		if p.Name == "DIRECT" || p.Name == "REJECT" {
			return fmt.Errorf("proxy name %q is reserved", p.Name)
		}
		if proxyNames[p.Name] {
			return fmt.Errorf("duplicate proxy name %q", p.Name)
		}
		proxyNames[p.Name] = true
	}

	groupNames := make(map[string]bool, len(cfg.ProxyGroups))
	for _, g := range cfg.ProxyGroups {
		if g.Name == "DIRECT" || g.Name == "REJECT" {
			return fmt.Errorf("proxy group name %q is reserved", g.Name)
		}
		if proxyNames[g.Name] {
			return fmt.Errorf("proxy group name %q conflicts with a proxy name", g.Name)
		}
		if groupNames[g.Name] {
			return fmt.Errorf("duplicate proxy group name %q", g.Name)
		}
		groupNames[g.Name] = true
	}

	memberDefined := func(name string) bool {
		return proxyNames[name] || groupNames[name] || name == "DIRECT" || name == "REJECT"
	}
	for _, g := range cfg.ProxyGroups {
		for _, m := range g.Proxies {
			if !memberDefined(m) {
				return fmt.Errorf("proxy group %q references undefined member %q", g.Name, m)
			}
		}
	}

	referencedProviders := make(map[string]bool, len(cfg.RuleProviders))
	for _, r := range cfg.Rules {
		parts := strings.Split(r, ",")
		var target string
		if len(parts) > 0 && parts[0] == "MATCH" {
			if len(parts) < 2 {
				return fmt.Errorf("rule %q has too few fields", r)
			}
			target = parts[1]
		} else {
			if len(parts) < 3 {
				return fmt.Errorf("rule %q has too few fields", r)
			}
			target = parts[2]
		}
		if !(groupNames[target] || target == "DIRECT" || target == "REJECT") {
			return fmt.Errorf("rule %q targets undefined proxy group %q", r, target)
		}

		if parts[0] == "RULE-SET" {
			name := parts[1]
			if _, ok := cfg.RuleProviders[name]; !ok {
				return fmt.Errorf("rule references unknown RULE-SET %q", name)
			}
			referencedProviders[name] = true
		}
	}

	for name := range cfg.RuleProviders {
		if !referencedProviders[name] {
			return fmt.Errorf("rule-provider %q is never referenced by any RULE-SET rule", name)
		}
	}

	return nil
}
