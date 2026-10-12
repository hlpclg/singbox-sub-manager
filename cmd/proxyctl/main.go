package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
	"github.com/hlpclg/singbox-sub-manager/internal/render"
	"github.com/hlpclg/singbox-sub-manager/internal/subscriptionvalidate"
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
	case "node":
		return cmdNode(args[1:], stdout, stderr)
	case "merge":
		return cmdMerge(args[1:], stdout, stderr)
	case "validate":
		return cmdValidate(args[1:], stdout, stderr)
	case "subscription":
		return cmdSubscription(args[1:], stdout, stderr)
	case "monitor":
		return cmdMonitor(args[1:], stdout, stderr)
	case "health":
		return cmdHealth(args[1:], stdout, stderr)
	case "backup":
		return cmdBackup(args[1:], stdout, stderr)
	case "restore":
		return cmdRestore(args[1:], stdout, stderr)
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
	fmt.Fprintln(stderr, "  node     list|add|edit|remove|enable|disable|migrate|publish")
	fmt.Fprintln(stderr, "  merge    --nodes nodes.conf --output DIR")
	fmt.Fprintln(stderr, "  validate --nodes nodes.conf")
	fmt.Fprintln(stderr, "  subscription build --nodes nodes.conf --output DIR")
	fmt.Fprintln(stderr, "  health   [--json] [--verbose] [--domain DOMAIN] [--remote --nodes PATH]")
	fmt.Fprintln(stderr, "  monitor  [pause|resume|status]")
	fmt.Fprintln(stderr, "  backup   [--out PATH] [--keep N] | list")
	fmt.Fprintln(stderr, "  restore  <archive-path> [--dry-run] [--no-restart]")
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
	ns = nodes.Enabled(ns)
	if len(ns) == 0 {
		fmt.Fprintln(stderr, "error: no enabled nodes")
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
	ns = nodes.Enabled(ns)
	if len(ns) == 0 {
		fmt.Fprintln(stderr, "validation failed: no enabled nodes")
		return 1
	}

	if _, err := subscriptionvalidate.Validate(ns); err != nil {
		fmt.Fprintln(stderr, "validation failed:", err)
		return 1
	}

	fmt.Fprintln(stdout, "validation passed")
	return 0
}

func cmdSubscription(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 || args[0] != "build" {
		fmt.Fprintln(stderr, "usage: proxyctl subscription build --nodes nodes.conf --output DIR")
		return 2
	}
	return cmdMerge(args[1:], stdout, stderr)
}
