package main

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// clashStructure is the decode target for validateClashYAML.
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

// validateClashYAML implements the YAML structural layer of design
// §5: it operates purely on the given YAML string and does not read
// render.ReservedNames() or any other renderer-internal data. "Does a
// group exist" is decided solely by the proxy-groups defined in doc.
func validateClashYAML(doc string) error {
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
