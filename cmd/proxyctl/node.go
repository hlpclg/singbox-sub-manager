package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
	"github.com/hlpclg/singbox-sub-manager/internal/health/remote"
	"context"
	"time"
	"golang.org/x/term"
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
	case "test":
		return cmdNodeTest(args[1:], stdout, stderr)
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
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(stderr, "error: unknown flag: %s\n", a)
			return "", nil, false
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

func isTTY(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

func promptLine(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func promptSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// resolveField returns the flag value if non-empty; otherwise prompts on a TTY,
// or records a missing-field error when non-interactive.
func resolveField(name, val string, secret bool, missing *[]string) string {
	if val != "" {
		return val
	}
	if !isTTY(os.Stdin) {
		*missing = append(*missing, name)
		return ""
	}
	var got string
	if secret {
		got, _ = promptSecret(fmt.Sprintf("%s: ", name))
	} else {
		got, _ = promptLine(fmt.Sprintf("%s: ", name))
	}
	return got
}

func cmdNodeAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("node add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("nodes", defaultNodesPath, "node configuration file")
	name := fs.String("name", "", "node name")
	server := fs.String("server", "", "server address")
	port := fs.Int("port", 0, "port")
	password := fs.String("password", "", "password")
	obfs := fs.String("obfs-password", "", "obfs password")
	sni := fs.String("sni", "", "sni")
	enabled := fs.Bool("enabled", true, "enabled")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var missing []string
	nName := resolveField("name", *name, false, &missing)
	nServer := resolveField("server", *server, false, &missing)
	nSNI := resolveField("sni", *sni, false, &missing)
	nPassword := resolveField("password", *password, true, &missing)
	nObfs := resolveField("obfs-password", *obfs, true, &missing)
	nPort := *port
	if nPort == 0 {
		if !isTTY(os.Stdin) {
			missing = append(missing, "port")
		} else if v, err := promptLine("port: "); err == nil {
			nPort, _ = strconv.Atoi(v)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(stderr, "error: missing required field(s): %s\n", strings.Join(missing, ", "))
		return 2
	}

	ns, ok := loadForMutation(*path, stderr)
	if !ok {
		return 1
	}
	updated, err := nodes.Add(ns, nodes.Node{
		Name: nName, Server: nServer, Port: nPort,
		Password: nPassword, ObfsPassword: nObfs, SNI: nSNI, Enabled: *enabled,
	})
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if err := nodes.WriteFile(*path, updated); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "added node %q\n", nName)
	return 0
}

// cmdNodeEdit expects the node name as a leading positional argument, e.g.
// "node edit JP --port 8888 --nodes PATH". flag.FlagSet.Parse stops at the
// first non-flag argument, so calling fs.Parse on the full args slice would
// treat "JP" as ending the flag section and silently ignore every flag that
// follows it. To avoid that, the positional name is pulled off args[0]
// ourselves before handing the remainder to fs.Parse.
func cmdNodeEdit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("node edit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("nodes", defaultNodesPath, "node configuration file")
	newName := fs.String("name", "", "new node name")
	server := fs.String("server", "", "server address")
	port := fs.Int("port", 0, "port")
	password := fs.String("password", "", "password")
	obfs := fs.String("obfs-password", "", "obfs password")
	sni := fs.String("sni", "", "sni")
	var enabled bool
	fs.BoolVar(&enabled, "enabled", true, "enabled")

	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, "error: node name required")
		return 2
	}
	target := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	// Track which flags were explicitly set so unset flags leave fields untouched.
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	ns, ok := loadForMutation(*path, stderr)
	if !ok {
		return 1
	}
	idx, found := nodes.Find(ns, target)
	if !found {
		fmt.Fprintf(stderr, "error: node %q not found\n", target)
		return 1
	}
	n := ns[idx]
	if set["name"] {
		n.Name = *newName
	}
	if set["server"] {
		n.Server = *server
	}
	if set["port"] {
		n.Port = *port
	}
	if set["password"] {
		n.Password = *password
	}
	if set["obfs-password"] {
		n.ObfsPassword = *obfs
	}
	if set["sni"] {
		n.SNI = *sni
	}
	if set["enabled"] {
		n.Enabled = enabled
	}

	updated, err := nodes.Replace(ns, target, n)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if err := nodes.WriteFile(*path, updated); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "updated node %q\n", n.Name)
	return 0
}

func cmdNodeMigrate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("node migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("nodes", defaultNodesPath, "node configuration file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ns, format, err := nodes.Load(*path)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	switch format {
	case nodes.FormatSectioned:
		fmt.Fprintln(stdout, "already in sectioned format; nothing to do")
		return 0
	case nodes.FormatEmpty:
		fmt.Fprintln(stderr, "error: no nodes to migrate")
		return 1
	}
	if err := nodes.WriteFile(*path, ns); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "migrated %d node(s); backup at %s.bak\n", len(ns), *path)
	return 0
}

func cmdNodeTest(args []string, stdout, stderr io.Writer) int {
	path, rest, ok := extractNodesFlag(args, stderr)
	if !ok {
		return 2
	}
	if len(rest) < 1 {
		fmt.Fprintln(stderr, "error: node name required")
		return 2
	}
	name := rest[0]
	ns, err := nodes.ParseFile(path)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 3
	}
	idx, found := nodes.Find(ns, name)
	if !found {
		fmt.Fprintf(stderr, "error: node %q not found\n", name)
		return 3
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = remote.CheckNode(ctx, ns[idx])
	if err != nil {
		fmt.Fprintf(stdout, "FAIL  %s  %v\n", ns[idx].Name, err)
		return 1
	}
	fmt.Fprintf(stdout, "PASS  %s  available\n", ns[idx].Name)
	return 0
}
