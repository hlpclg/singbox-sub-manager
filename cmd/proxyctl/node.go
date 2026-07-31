package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
)

const defaultNodesPath = "/etc/singbox-sub-manager/nodes.conf"

func nodeUsage(stderr io.Writer) int {
	fmt.Fprintln(stderr, "usage: proxyctl node <list|add|edit|remove|enable|disable|migrate> [args]")
	return 2
}

func cmdNode(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		return nodeUsage(stderr)
	}
	switch args[0] {
	case "list":
		return cmdNodeList(args[1:], stdout, stderr)
	case "enable":
		return cmdNodeSetEnabled(args[1:], stdout, stderr, true)
	case "disable":
		return cmdNodeSetEnabled(args[1:], stdout, stderr, false)
	case "remove":
		return cmdNodeRemove(args[1:], stdout, stderr)
	case "add":
		return cmdNodeAdd(args[1:], stdout, stderr)
	case "edit":
		return cmdNodeEdit(args[1:], stdout, stderr)
	case "migrate":
		return cmdNodeMigrate(args[1:], stdout, stderr)
	default:
		return nodeUsage(stderr)
	}
}

// extractNodesFlag pulls a "--nodes PATH" (or "--nodes=PATH") flag out of args
// wherever it appears and returns the remaining positional args. This is
// needed because commands like "node disable NAME --nodes PATH" place a
// positional argument before the flag, which the standard library's
// flag.FlagSet cannot parse in a single pass (it stops at the first non-flag
// argument and treats everything after it as positional too).
func extractNodesFlag(args []string, stderr io.Writer) (path string, rest []string, ok bool) {
	path = defaultNodesPath
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--nodes" || a == "-nodes":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, "error: flag needs an argument: -nodes")
				return "", nil, false
			}
			path = args[i+1]
			i++
		case strings.HasPrefix(a, "--nodes="):
			path = strings.TrimPrefix(a, "--nodes=")
		case strings.HasPrefix(a, "-nodes="):
			path = strings.TrimPrefix(a, "-nodes=")
		default:
			rest = append(rest, a)
		}
	}
	return path, rest, true
}

// loadForMutation reads the node file and refuses to proceed on legacy format.
func loadForMutation(path string, stderr io.Writer) ([]nodes.Node, bool) {
	ns, format, err := nodes.Load(path)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return nil, false
	}
	if format == nodes.FormatLegacy {
		fmt.Fprintln(stderr, "error: legacy nodes.conf detected; run 'proxyctl node migrate' first")
		return nil, false
	}
	return ns, true
}

func cmdNodeList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("node list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("nodes", defaultNodesPath, "node configuration file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ns, _, err := nodes.Load(*path)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSERVER:PORT\tSNI\tENABLED")
	for _, n := range ns {
		fmt.Fprintf(tw, "%s\t%s:%d\t%s\t%t\n", n.Name, n.Server, n.Port, n.SNI, n.Enabled)
	}
	tw.Flush()
	return 0
}

func cmdNodeSetEnabled(args []string, stdout, stderr io.Writer, enabled bool) int {
	path, rest, ok := extractNodesFlag(args, stderr)
	if !ok {
		return 2
	}
	if len(rest) < 1 {
		fmt.Fprintln(stderr, "error: node name required")
		return 2
	}
	name := rest[0]
	ns, ok := loadForMutation(path, stderr)
	if !ok {
		return 1
	}
	updated, err := nodes.SetEnabled(ns, name, enabled)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if err := nodes.WriteFile(path, updated); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "node %q enabled=%t\n", name, enabled)
	return 0
}

func cmdNodeRemove(args []string, stdout, stderr io.Writer) int {
	path, rest, ok := extractNodesFlag(args, stderr)
	if !ok {
		return 2
	}
	if len(rest) < 1 {
		fmt.Fprintln(stderr, "error: node name required")
		return 2
	}
	name := rest[0]
	ns, ok := loadForMutation(path, stderr)
	if !ok {
		return 1
	}
	updated, err := nodes.Remove(ns, name)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if err := nodes.WriteFile(path, updated); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "removed node %q\n", name)
	return 0
}

// The following are TEMPORARY stubs for commands implemented in later tasks
// (Task 4: add/edit, Task 5: migrate). They exist only so this package
// compiles and the node dispatcher above can route to them.

func cmdNodeAdd(args []string, stdout, stderr io.Writer) int {
	fmt.Fprintln(stderr, "not implemented")
	return 1
}

func cmdNodeEdit(args []string, stdout, stderr io.Writer) int {
	fmt.Fprintln(stderr, "not implemented")
	return 1
}

func cmdNodeMigrate(args []string, stdout, stderr io.Writer) int {
	fmt.Fprintln(stderr, "not implemented")
	return 1
}
