package main

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
	"github.com/hlpclg/singbox-sub-manager/internal/render"
	"gopkg.in/yaml.v3"
)

var Version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		return usage(stderr)
	}

	switch args[0] {
	case "merge":
		return cmdMerge(args[1:], stdout, stderr)
	case "validate":
		return cmdValidate(args[1:], stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, Version)
		return 0
	default:
		return usage(stderr)
	}
}

func usage(stderr io.Writer) int {
	fmt.Fprintln(stderr, "usage: proxyctl <command> [args]")
	fmt.Fprintln(stderr, "commands:")
	fmt.Fprintln(stderr, "  merge    --nodes nodes.conf --output DIR")
	fmt.Fprintln(stderr, "  validate --nodes nodes.conf")
	fmt.Fprintln(stderr, "  version  -- show version")
	return 2
}

func cmdMerge(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("merge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	nodeFile := fs.String("nodes", "nodes.conf", "node configuration file")
	output := fs.String("output", ".", "output directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ns, err := nodes.ParseFile(*nodeFile)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if err := render.Write(*output, ns); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "generated %d nodes in %s\n", len(ns), *output)
	return 0
}

func cmdValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	nodeFile := fs.String("nodes", "nodes.conf", "node configuration file")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ns, err := nodes.ParseFile(*nodeFile)
	if err != nil {
		fmt.Fprintln(stderr, "validation failed: nodes.conf error:", err)
		return 1
	}

	for _, n := range ns {
		if strings.Contains(n.Password, "CHANGE_ME") || strings.Contains(n.ObfsPassword, "CHANGE_ME") {
			fmt.Fprintf(stderr, "validation failed: placeholder 'CHANGE_ME' found in node %q\n", n.Name)
			return 1
		}
		if n.Server == "" || n.Port <= 0 || n.Port > 65535 || n.Password == "" || n.SNI == "" {
			fmt.Fprintf(stderr, "validation failed: node %q is missing required hysteria2 fields or has invalid port\n", n.Name)
			return 1
		}
	}

	clashYaml := render.Clash(ns)
	if strings.Contains(clashYaml, "CHANGE_ME") {
		fmt.Fprintln(stderr, "validation failed: generated clash.yaml contains placeholder 'CHANGE_ME'")
		return 1
	}

	var config struct {
		Proxies []struct {
			Name     string `yaml:"name"`
			Type     string `yaml:"type"`
			Server   string `yaml:"server"`
			Port     int    `yaml:"port"`
			Password string `yaml:"password"`
			SNI      string `yaml:"sni"`
		} `yaml:"proxies"`
		ProxyGroups []struct {
			Name    string   `yaml:"name"`
			Proxies []string `yaml:"proxies"`
		} `yaml:"proxy-groups"`
		RuleProviders map[string]interface{} `yaml:"rule-providers"`
		Rules         []string               `yaml:"rules"`
	}

	dec := yaml.NewDecoder(strings.NewReader(clashYaml))
	dec.KnownFields(false)
	if err := dec.Decode(&config); err != nil {
		fmt.Fprintln(stderr, "validation failed: generated clash.yaml is invalid YAML:", err)
		return 1
	}

	proxySet := make(map[string]bool)
	proxySet["DIRECT"] = true
	proxySet["REJECT"] = true
	proxySet["自动选择"] = true
	proxySet["节点选择"] = true

	for _, p := range config.Proxies {
		if proxySet[p.Name] {
			fmt.Fprintf(stderr, "validation failed: duplicate proxy name %q found in clash.yaml\n", p.Name)
			return 1
		}
		proxySet[p.Name] = true
	}

	for _, pg := range config.ProxyGroups {
		for _, p := range pg.Proxies {
			if !proxySet[p] {
				fmt.Fprintf(stderr, "validation failed: proxy group %q references unknown proxy %q\n", pg.Name, p)
				return 1
			}
		}
	}

	for _, r := range config.Rules {
		parts := strings.Split(r, ",")
		if len(parts) >= 2 && parts[0] == "RULE-SET" {
			if _, ok := config.RuleProviders[parts[1]]; !ok {
				fmt.Fprintf(stderr, "validation failed: rule references unknown RULE-SET %q\n", parts[1])
				return 1
			}
		}
	}

	sr := render.Shadowrocket(ns)
	lines := strings.Split(strings.TrimSpace(sr), "\n")
	for i, l := range lines {
		u, err := url.Parse(l)
		if err != nil {
			fmt.Fprintf(stderr, "validation failed: generated Shadowrocket URI at line %d is invalid: %v\n", i+1, err)
			return 1
		}
		if u.Scheme != "hysteria2" {
			fmt.Fprintf(stderr, "validation failed: generated Shadowrocket URI at line %d has wrong scheme\n", i+1)
			return 1
		}
	}

	fmt.Fprintln(stdout, "validation passed")
	return 0
}
