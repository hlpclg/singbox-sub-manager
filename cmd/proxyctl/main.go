package main

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
	"github.com/hlpclg/singbox-sub-manager/internal/render"
	"gopkg.in/yaml.v3"
)

var Version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	switch os.Args[1] {
	case "merge":
		cmdMerge(os.Args[2:])
	case "validate":
		cmdValidate(os.Args[2:])
	case "version":
		fmt.Println(Version)
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: proxyctl <command> [args]")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  merge    --nodes nodes.conf --output DIR")
	fmt.Fprintln(os.Stderr, "  validate --nodes nodes.conf")
	fmt.Fprintln(os.Stderr, "  version  -- show version")
	os.Exit(2)
}

func cmdMerge(args []string) {
	fs := flag.NewFlagSet("merge", flag.ExitOnError)
	nodeFile := fs.String("nodes", "nodes.conf", "node configuration file")
	output := fs.String("output", ".", "output directory")
	_ = fs.Parse(args)

	ns, err := nodes.ParseFile(*nodeFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if err := render.Write(*output, ns); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("generated %d nodes in %s\n", len(ns), *output)
}

func cmdValidate(args []string) {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	nodeFile := fs.String("nodes", "nodes.conf", "node configuration file")
	_ = fs.Parse(args)

	ns, err := nodes.ParseFile(*nodeFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "validation failed: nodes.conf error:", err)
		os.Exit(1)
	}

	for _, n := range ns {
		if strings.Contains(n.Password, "CHANGE_ME") || strings.Contains(n.ObfsPassword, "CHANGE_ME") {
			fmt.Fprintf(os.Stderr, "validation failed: placeholder 'CHANGE_ME' found in node %q\n", n.Name)
			os.Exit(1)
		}
		if n.Server == "" || n.Port <= 0 || n.Port > 65535 || n.Password == "" || n.SNI == "" {
			fmt.Fprintf(os.Stderr, "validation failed: node %q is missing required hysteria2 fields or has invalid port\n", n.Name)
			os.Exit(1)
		}
	}

	clashYaml := render.Clash(ns)
	if strings.Contains(clashYaml, "CHANGE_ME") {
		fmt.Fprintln(os.Stderr, "validation failed: generated clash.yaml contains placeholder 'CHANGE_ME'")
		os.Exit(1)
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
	dec.KnownFields(false) // We want to parse specifically those fields but others are okay for now
	if err := dec.Decode(&config); err != nil {
		fmt.Fprintln(os.Stderr, "validation failed: generated clash.yaml is invalid YAML:", err)
		os.Exit(1)
	}

	proxySet := make(map[string]bool)
	proxySet["DIRECT"] = true
	proxySet["REJECT"] = true
	proxySet["自动选择"] = true
	proxySet["节点选择"] = true

	for _, p := range config.Proxies {
		if proxySet[p.Name] {
			fmt.Fprintf(os.Stderr, "validation failed: duplicate proxy name %q found in clash.yaml\n", p.Name)
			os.Exit(1)
		}
		proxySet[p.Name] = true
	}

	for _, pg := range config.ProxyGroups {
		for _, p := range pg.Proxies {
			if !proxySet[p] {
				fmt.Fprintf(os.Stderr, "validation failed: proxy group %q references unknown proxy %q\n", pg.Name, p)
				os.Exit(1)
			}
		}
	}

	for _, r := range config.Rules {
		parts := strings.Split(r, ",")
		if len(parts) >= 2 && parts[0] == "RULE-SET" {
			if _, ok := config.RuleProviders[parts[1]]; !ok {
				fmt.Fprintf(os.Stderr, "validation failed: rule references unknown RULE-SET %q\n", parts[1])
				os.Exit(1)
			}
		}
	}

	sr := render.Shadowrocket(ns)
	lines := strings.Split(strings.TrimSpace(sr), "\n")
	for i, l := range lines {
		u, err := url.Parse(l)
		if err != nil {
			fmt.Fprintf(os.Stderr, "validation failed: generated Shadowrocket URI at line %d is invalid: %v\n", i+1, err)
			os.Exit(1)
		}
		if u.Scheme != "hysteria2" {
			fmt.Fprintf(os.Stderr, "validation failed: generated Shadowrocket URI at line %d has wrong scheme\n", i+1)
			os.Exit(1)
		}
	}

	fmt.Println("validation passed")
}
