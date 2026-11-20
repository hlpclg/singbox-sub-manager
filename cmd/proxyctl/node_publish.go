package main

// node_publish.go implements design spec §10's CLI: `proxyctl node publish
// [--recover [--abandon]]` and the strict-mode (`--publish`) forms of `node
// add|edit|enable|disable|remove` (Task 9). Everything here is strict mode
// (§4.5): argv is parsed in full -- unknown/duplicate flags, missing values,
// positional counts, mode conflicts, malformed --min-version -- before the
// root check, any lock, any network call or any write, and a parse failure is
// exit 2. Role detection (internal/role) and lock orchestration
// (internal/hostlock) live here, at the CLI layer; internal/publish never
// imports either (the same one-directional layering node_service.go uses).
//
// The unadorned `node add|edit|enable|disable|remove` (no `--publish` in flag
// position) never reach this file: node.go's dispatcher keeps routing them to
// the original, compat-mode functions, whose parsing, output, `.bak`, `--nodes`
// semantics and exit codes are unchanged (§4.1).
import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/hlpclg/singbox-sub-manager/internal/hostlock"
	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
	"github.com/hlpclg/singbox-sub-manager/internal/publish"
	"github.com/hlpclg/singbox-sub-manager/internal/role"
)

// publishRuntime is every process-level dependency the publish commands touch
// besides internal/publish's own Env: the effective uid, subscription-center
// role detection, host lock acquisition, construction of the publish.Env and
// the signal-cancelled context. Production wires the real ones; tests replace
// newPublishRuntime wholesale (spec §14.1/§14.3).
type publishRuntime struct {
	geteuid    func() int
	detectRole func() role.Result
	acquire    func(ctx context.Context, levels ...hostlock.Level) (*hostlock.Lease, error)
	newEnv     func() *publish.Env
	newContext func() (context.Context, context.CancelFunc)
}

var newPublishRuntime = productionPublishRuntime

func productionPublishRuntime() publishRuntime {
	return publishRuntime{
		geteuid: os.Geteuid,
		detectRole: func() role.Result {
			return role.DetectSubscriptionCenter(role.OSFS(), role.ProdPaths())
		},
		acquire: func(ctx context.Context, levels ...hostlock.Level) (*hostlock.Lease, error) {
			return hostlock.Acquire(ctx, hostlock.ProdPaths(), hostlock.AcquireOptions{}, levels...)
		},
		newEnv:     func() *publish.Env { return publish.ProdEnv(Version) },
		newContext: installBaseContext,
	}
}

func (rt publishRuntime) requireRoot(stderr io.Writer) bool {
	if rt.geteuid() != 0 {
		fmt.Fprintln(stderr, "error: this command must be run as root (sudo)")
		return false
	}
	return true
}

// requireSubscriptionCenterYes implements spec §10.3 step 1's "订阅中心角色为
// 是", used before the locks (quick precheck) and again after them. The role
// line goes to stderr only (spec §4.1); stdout stays the command's own output.
func (rt publishRuntime) requireSubscriptionCenterYes(stderr io.Writer) bool {
	result := rt.detectRole()
	fmt.Fprintf(stderr, "subscription center role: %s\n", roleStatusString(result))
	if result.Status != role.StatusYes {
		fmt.Fprintln(stderr, "error: this host's subscription-center role is not 'yes'; refusing to publish here")
		return false
	}
	return true
}

func nodePublishUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: proxyctl node publish [--min-version vX.Y.Z]")
	fmt.Fprintln(w, "       proxyctl node publish --recover [--abandon] [--min-version vX.Y.Z]")
	fmt.Fprintln(w, "       proxyctl node add|edit|enable|disable|remove --publish [--min-version vX.Y.Z] [flags] [NAME]")
	fmt.Fprintln(w, "  publish          re-render and re-publish nodes.conf, clash.yaml and sr.txt (no node change)")
	fmt.Fprintln(w, "  --recover        finish or roll back a publish transaction an earlier run left behind")
	fmt.Fprintln(w, "  --abandon        with --recover: drop the leftover transaction when every file is already back")
	fmt.Fprintln(w, "                   to its pre-transaction state")
	fmt.Fprintln(w, "  --min-version V  refuse to run on a proxyctl older than V")
}

// --- strict CRUD flag tables (spec §4.5) ---

// crudValueFlags lists, per compat-capable command, the flags that take a
// value token: DetermineStrictMode needs them so the value of "--name" is not
// mistaken for a flag ("--name --publish" must not trigger strict mode). These
// are the legacy command's own flags plus --min-version, which only strict
// mode honors.
var crudValueFlags = map[string]map[string]bool{
	"add": {"nodes": true, "name": true, "type": true, "server": true, "port": true, "password": true,
		"obfs-password": true, "sni": true, "uuid": true, "public-key": true, "short-id": true, "min-version": true},
	"edit": {"nodes": true, "name": true, "server": true, "port": true, "password": true,
		"obfs-password": true, "sni": true, "uuid": true, "public-key": true, "short-id": true, "min-version": true},
	"enable":  {"nodes": true, "min-version": true},
	"disable": {"nodes": true, "min-version": true},
	"remove":  {"nodes": true, "min-version": true},
}

// isStrictCRUD reports whether args (everything after the verb) put the
// command in strict mode.
func isStrictCRUD(verb string, args []string) bool {
	return DetermineStrictMode(args, crudValueFlags[verb])
}

func publishSpec() FlagSpec {
	return FlagSpec{Name: "publish", Kind: FlagExactBool, DoubleDashOnly: true}
}

func crudSpecs(verb string) []FlagSpec {
	specs := []FlagSpec{
		publishSpec(),
		{Name: "min-version", Kind: FlagString},
		// --nodes is recognised only so that combining it with --publish gets
		// CheckPublishNodesConflict's specific message; --abandon only to
		// reject it with CheckAbandonPlacement's.
		{Name: "nodes", Kind: FlagString},
		{Name: "abandon", Kind: FlagBool},
	}
	switch verb {
	case "add", "edit":
		for _, n := range []string{"name", "server", "port", "password", "obfs-password", "sni", "uuid", "public-key", "short-id"} {
			specs = append(specs, FlagSpec{Name: n, Kind: FlagString})
		}
		specs = append(specs, FlagSpec{Name: "enabled", Kind: FlagBool})
		if verb == "add" {
			specs = append(specs, FlagSpec{Name: "type", Kind: FlagString})
		}
	}
	return specs
}

// publishRequest is what a successful parse hands to the runner. preErr, when
// set, is a state-independent semantic rejection (an invalid node) that is
// reported as exit 1 before root detection, locks or the network are touched.
type publishRequest struct {
	params publish.Params
	preErr error
}

// parseCRUD parses a strict-mode node add/edit/enable/disable/remove. A non-2
// zero return code means "parsed"; a usage error is exit 2 with its message
// already written.
func parseCRUD(verb string, args []string, stderr io.Writer) (publishRequest, int) {
	want := 1
	if verb == "add" {
		want = 0
	}
	parsed, err := StrictScan(args, crudSpecs(verb), want)
	if err == nil {
		err = CheckPublishNodesConflict(parsed)
	}
	if err == nil {
		err = CheckAbandonPlacement(parsed, false)
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return publishRequest{}, 2
	}
	usage := func(format string, a ...interface{}) (publishRequest, int) {
		fmt.Fprintf(stderr, "error: "+format+"\n", a...)
		return publishRequest{}, 2
	}
	var req publishRequest
	if parsed.Set["min-version"] {
		if err := publish.ValidMinVersion(parsed.Values["min-version"]); err != nil {
			return usage("invalid --min-version: %v", err)
		}
		req.params.MinVersion = parsed.Values["min-version"]
	}

	switch verb {
	case "add":
		node, code := parseAddNode(parsed, stderr)
		if code != 0 {
			return publishRequest{}, code
		}
		req.params.Mutation = publish.Mutation{Kind: publish.KindAdd, Node: node}
		req.preErr = nodes.Validate(node)

	case "edit":
		name := parsed.Positional[0]
		if err := RejectLeadingDash(name); err != nil {
			return usage("%v", err)
		}
		e, code := parseEdit(parsed, stderr)
		if code != 0 {
			return publishRequest{}, code
		}
		req.params.Mutation = publish.Mutation{Kind: publish.KindEdit, Name: name, Edit: e}

	default:
		name := parsed.Positional[0]
		if err := RejectLeadingDash(name); err != nil {
			return usage("%v", err)
		}
		kind := map[string]publish.MutationKind{"enable": publish.KindEnable, "disable": publish.KindDisable, "remove": publish.KindRemove}[verb]
		req.params.Mutation = publish.Mutation{Kind: kind, Name: name}
	}
	return req, 0
}

func parseAddNode(a StrictArgs, stderr io.Writer) (nodes.Node, int) {
	fail := func(format string, args ...interface{}) (nodes.Node, int) {
		fmt.Fprintf(stderr, "error: "+format+"\n", args...)
		return nodes.Node{}, 2
	}
	typ := nodes.NodeType(a.Values["type"])
	if !a.Set["type"] {
		typ = nodes.TypeHysteria2
	}
	if typ != nodes.TypeHysteria2 && typ != nodes.TypeVlessReality {
		return fail("unknown --type %q", a.Values["type"])
	}
	switch typ {
	case nodes.TypeHysteria2:
		if a.Set["uuid"] || a.Set["public-key"] || a.Set["short-id"] {
			return fail("--uuid/--public-key/--short-id are not allowed with --type hysteria2")
		}
	case nodes.TypeVlessReality:
		if a.Set["password"] || a.Set["obfs-password"] {
			return fail("--password/--obfs-password are not allowed with --type vless-reality")
		}
	}

	var missing []string
	need := func(name string) string {
		if a.Values[name] == "" {
			missing = append(missing, name)
		}
		return a.Values[name]
	}
	n := nodes.Node{Type: typ, Enabled: true}
	n.Name = need("name")
	n.Server = need("server")
	n.SNI = need("sni")
	portText := need("port")
	switch typ {
	case nodes.TypeHysteria2:
		n.Password = need("password")
		n.ObfsPassword = need("obfs-password")
	case nodes.TypeVlessReality:
		n.UUID = need("uuid")
		n.PublicKey = need("public-key")
		n.ShortID = a.Values["short-id"]
	}
	// Strict mode never prompts (spec §4.5), even on a TTY.
	if len(missing) > 0 {
		return fail("missing required field(s): %s", strings.Join(missing, ", "))
	}
	if err := RejectLeadingDash(n.Name); err != nil {
		return fail("%v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return fail("--port must be an integer")
	}
	n.Port = port
	if a.Set["enabled"] {
		n.Enabled = a.Bool("enabled")
	}
	return n, 0
}

func parseEdit(a StrictArgs, stderr io.Writer) (publish.Edit, int) {
	var e publish.Edit
	str := func(name string, dst **string) {
		if a.Set[name] {
			v := a.Values[name]
			*dst = &v
		}
	}
	str("name", &e.Name)
	str("server", &e.Server)
	str("sni", &e.SNI)
	str("password", &e.Password)
	str("obfs-password", &e.ObfsPassword)
	str("uuid", &e.UUID)
	str("public-key", &e.PublicKey)
	str("short-id", &e.ShortID)
	if e.Name != nil {
		if err := RejectLeadingDash(*e.Name); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return e, 2
		}
	}
	if a.Set["port"] {
		p, err := strconv.Atoi(a.Values["port"])
		if err != nil {
			fmt.Fprintln(stderr, "error: --port must be an integer")
			return e, 2
		}
		e.Port = &p
	}
	if a.Set["enabled"] {
		v := a.Bool("enabled")
		e.Enabled = &v
	}
	return e, 0
}

// --- entry points ---

// cmdNodePublishCRUD runs a strict-mode node add|edit|enable|disable|remove.
func cmdNodePublishCRUD(verb string, args []string, stdout, stderr io.Writer) int {
	req, code := parseCRUD(verb, args, stderr)
	if code != 0 {
		return code
	}
	return runPublish(req, stdout, stderr)
}

// cmdNodePublish runs `node publish` (a republish, or --recover).
func cmdNodePublish(args []string, stdout, stderr io.Writer) int {
	if IsHelpOnly(args) {
		nodePublishUsage(stdout)
		return 0
	}
	specs := []FlagSpec{
		{Name: "min-version", Kind: FlagString},
		{Name: "recover", Kind: FlagBool},
		{Name: "abandon", Kind: FlagBool},
	}
	parsed, err := StrictScan(args, specs, 0)
	if err == nil {
		err = CheckAbandonPlacement(parsed, true)
	}
	if err == nil && parsed.Bool("abandon") && !parsed.Bool("recover") {
		err = strictErr("--abandon is only valid together with --recover")
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	minVersion := ""
	if parsed.Set["min-version"] {
		minVersion = parsed.Values["min-version"]
		if err := publish.ValidMinVersion(minVersion); err != nil {
			fmt.Fprintf(stderr, "error: invalid --min-version: %v\n", err)
			return 2
		}
	}
	if parsed.Bool("recover") {
		return runPublishRecover(publish.RecoverParams{Abandon: parsed.Bool("abandon"), MinVersion: minVersion}, stdout, stderr)
	}
	return runPublish(publishRequest{params: publish.Params{Mutation: publish.Mutation{Kind: publish.KindPublish}, MinVersion: minVersion}}, stdout, stderr)
}

// runPublish is spec §10.3's order: root -> role -> quick precheck -> L1 then
// L2 -> role again -> the transaction (publish.Run repeats the precheck
// itself).
func runPublish(req publishRequest, stdout, stderr io.Writer) int {
	if req.preErr != nil {
		fmt.Fprintln(stderr, "error:", req.preErr)
		return 1
	}
	rt := newPublishRuntime()
	if !rt.requireRoot(stderr) {
		return 1
	}
	if !rt.requireSubscriptionCenterYes(stderr) {
		return 1
	}
	ctx, cancel := rt.newContext()
	defer cancel()

	env := rt.newEnv()
	if _, err := publish.Precheck(env, req.params.MinVersion); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	lease, err := rt.acquire(ctx, hostlock.L1, hostlock.L2)
	if err != nil {
		fmt.Fprintln(stderr, "error: another operation is in progress:", err)
		return 1
	}
	defer lease.Release()

	if !rt.requireSubscriptionCenterYes(stderr) {
		return 1
	}
	return reportPublishResult(publish.Run(ctx, env, req.params), stdout, stderr)
}

// runPublishRecover is `node publish --recover [--abandon]`: the same order
// (root -> role -> layout precheck -> L1 then L2 -> role again), then the one
// shared recovery routine.
func runPublishRecover(params publish.RecoverParams, stdout, stderr io.Writer) int {
	rt := newPublishRuntime()
	if !rt.requireRoot(stderr) {
		return 1
	}
	if !rt.requireSubscriptionCenterYes(stderr) {
		return 1
	}
	ctx, cancel := rt.newContext()
	defer cancel()

	env := rt.newEnv()
	if _, err := publish.PrecheckRecover(env, params); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	lease, err := rt.acquire(ctx, hostlock.L1, hostlock.L2)
	if err != nil {
		fmt.Fprintln(stderr, "error: another operation is in progress:", err)
		return 1
	}
	defer lease.Release()

	if !rt.requireSubscriptionCenterYes(stderr) {
		return 1
	}
	return reportPublishResult(publish.RunRecover(ctx, env, params), stdout, stderr)
}

// reportPublishResult maps a publish.Result to this command's exit code and
// output (spec §13.1's table), never by matching error text:
//
//	PreTransaction                       -> 1 (nothing managed was modified)
//	Err != nil (a transaction had begun) -> 5 (could not confirm recovery or
//	                                         cleanup; the area is named)
//	Outcome rolled back                  -> 4
//	Outcome committed / recovered        -> 0
func reportPublishResult(r publish.Result, stdout, stderr io.Writer) int {
	for _, w := range r.Warnings {
		fmt.Fprintln(stderr, "warning:", w)
	}
	code := r.ExitCode()
	switch code {
	case 1:
		if r.Err != nil {
			fmt.Fprintln(stderr, "error:", r.Err)
		}
	case 5:
		fmt.Fprintln(stderr, "error:", r.Err)
		if r.TxnDir != "" {
			fmt.Fprintf(stderr, "the transaction area %s was preserved and needs manual attention; run `proxyctl node publish --recover`, or see docs/troubleshooting.md\n", r.TxnDir)
		}
	case 4:
		if r.Cause != nil {
			fmt.Fprintln(stderr, "error:", r.Cause)
		}
		fmt.Fprintln(stderr, "the publish was automatically rolled back to its pre-transaction state")
	default:
		if r.Summary != "" {
			fmt.Fprintln(stdout, r.Summary)
		}
		for _, n := range r.Notices {
			fmt.Fprintln(stderr, n)
		}
	}
	return code
}
