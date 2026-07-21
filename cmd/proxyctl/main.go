package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/YOUR_GITHUB_USERNAME/proxy-installer/internal/nodes"
	"github.com/YOUR_GITHUB_USERNAME/proxy-installer/internal/render"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "merge" {
		fmt.Fprintln(os.Stderr, "usage: proxyctl merge --nodes nodes.conf --output DIR")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("merge", flag.ExitOnError)
	nodeFile := fs.String("nodes", "nodes.conf", "node configuration file")
	output := fs.String("output", ".", "output directory")
	_ = fs.Parse(os.Args[2:])

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
