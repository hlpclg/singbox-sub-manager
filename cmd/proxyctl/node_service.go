package main

// node_service.go implements design spec §8's `proxyctl node service
// install|reinstall|show` (Task 7). All three commands are unconditionally
// strict mode (§4.5); parsing always completes, in full, before any root
// check, lock, network call or write (§4.5's "执行任何修改、网络请求或加锁
// 之前，先完整解析全部argv" and G2). Role detection (internal/role) and
// lock orchestration (internal/hostlock) live here, at the CLI layer —
// internal/realitynode's Install/Reinstall/Show never import either
// package, so the layering stays one-directional (CLI orchestrates role +
// lock + realitynode; realitynode never reaches back for them).
import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hlpclg/singbox-sub-manager/internal/hostlock"
	"github.com/hlpclg/singbox-sub-manager/internal/realitynode"
	"github.com/hlpclg/singbox-sub-manager/internal/role"
)

// showLockWait is spec §8's "show只取L3,最长等待10秒".
const showLockWait = 10 * time.Second

func nodeServiceUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: proxyctl node service <install|reinstall|show> [args]")
	fmt.Fprintln(w, "  install   --name NAME --server ADDR [--port PORT] [--sni SNI]")
	fmt.Fprintln(w, "  reinstall [--singbox-version V --singbox-sha256 H]")
	fmt.Fprintln(w, "  show")
}

func cmdNodeService(args []string, stdout, stderr io.Writer) int {
	if IsHelpOnly(args) {
		nodeServiceUsage(stdout)
		return 0
	}
	if len(args) < 1 {
		nodeServiceUsage(stderr)
		return 2
	}
	switch args[0] {
	case "install":
		return cmdNodeServiceInstall(args[1:], stdout, stderr)
	case "reinstall":
		return cmdNodeServiceReinstall(args[1:], stdout, stderr)
	case "show":
		return cmdNodeServiceShow(args[1:], stdout, stderr)
	default:
		nodeServiceUsage(stderr)
		return 2
	}
}

// --- shared plumbing ---

// serviceRuntime is every process-level dependency the node service
// commands touch besides realitynode's own Env: the effective uid, subscription
// center role detection, host lock acquisition, construction of the
// realitynode.Env (which is where network, systemd and the filesystem come
// from) and the signal-cancelled context. Production wires the real ones;
// tests replace newServiceRuntime wholesale so they need neither root, nor
// /run/lock, nor a real host (spec §14.1/§14.3).
type serviceRuntime struct {
	geteuid    func() int
	detectRole func() role.Result
	acquire    func(ctx context.Context, levels ...hostlock.Level) (*hostlock.Lease, error)
	newEnv     func() *realitynode.Env
	newContext func() (context.Context, context.CancelFunc)
}

var newServiceRuntime = productionServiceRuntime

func productionServiceRuntime() serviceRuntime {
	return serviceRuntime{
		geteuid: os.Geteuid,
		detectRole: func() role.Result {
			return role.DetectSubscriptionCenter(role.OSFS(), role.ProdPaths())
		},
		acquire: func(ctx context.Context, levels ...hostlock.Level) (*hostlock.Lease, error) {
			return hostlock.Acquire(ctx, hostlock.ProdPaths(), hostlock.AcquireOptions{}, levels...)
		},
		newEnv:     productionServiceEnv,
		newContext: installBaseContext,
	}
}

// installBaseContext wires SIGINT/SIGTERM cancellation (spec §13.3:
// "收到SIGINT或SIGTERM时,取消主context") into the context every node
// service command passes straight through to realitynode.
func installBaseContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
		signal.Stop(sigCh)
	}()
	return ctx, cancel
}

// requireRoot implements spec §8's "检查root" step.
func (rt serviceRuntime) requireRoot(stderr io.Writer) bool {
	if rt.geteuid() != 0 {
		fmt.Fprintln(stderr, "error: this command must be run as root (sudo)")
		return false
	}
	return true
}

// requireSubscriptionCenterNo implements spec §8.1/§8.2's role check
// ("订阅中心为否"), used both before locking (quick precheck) and again
// after (取锁后复查, per §8's own table). The role line itself goes to
// stderr only (spec §4.1: "角色行写到stderr").
func (rt serviceRuntime) requireSubscriptionCenterNo(stderr io.Writer) bool {
	result := rt.detectRole()
	fmt.Fprintf(stderr, "subscription center role: %s\n", roleStatusString(result))
	if result.Status != role.StatusNo {
		fmt.Fprintln(stderr, "error: this host's subscription-center role is not 'no'; refusing to run a node service command here")
		return false
	}
	return true
}

func roleStatusString(r role.Result) string {
	switch r.Status {
	case role.StatusYes:
		return "yes"
	case role.StatusNo:
		return "no"
	default:
		if len(r.Reasons) > 0 {
			return "uncertain (" + strings.Join(r.Reasons, "; ") + ")"
		}
		return "uncertain"
	}
}

// productionServiceEnv builds the real *realitynode.Env for node service
// commands: every dependency realitynode.Env declares defaults to the
// real implementation when left zero-valued (documented on each field in
// service.go), so only what actually needs a concrete production value is
// set here.
func productionServiceEnv() *realitynode.Env {
	runner := realitynode.ExecRunner{}
	return &realitynode.Env{
		Roots:            realitynode.ProdRoots(),
		FS:               realitynode.OSStateFS(),
		TrustedOwnerUID:  0,
		TrustedOwnerGID:  0,
		GOOS:             "linux",
		GOARCH:           runtime.GOARCH,
		SystemdAvailable: systemdAvailable,
		OSRelease:        readOSRelease,
		UFWActive:        ufwActive,
		Downloader:       realitynode.HTTPDownloader{},
		Runner:           runner,
		Systemd:          realitynode.Systemctl{Runner: runner},
		ProxyctlVersion:  Version,
	}
}

// systemdAvailable/readOSRelease/ufwActive are tiny seams kept as plain
// functions (not struct fields) since production node.go has exactly one
// call site for each; realitynode.Env's own fields are what test code
// injects instead.
func systemdAvailable() bool {
	_, err := os.Stat("/run/systemd/system")
	return err == nil
}

func readOSRelease() (id, versionID string, err error) {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "", "", err
	}
	i, v := realitynode.ParseOSRelease(string(data))
	return i, v, nil
}

func ufwActive() (bool, error) {
	out, err := exec.Command("ufw", "status").CombinedOutput()
	if err != nil {
		// ufw not installed, or not permitted to query it: spec §8.1 only
		// wants a best-effort warning, never a hard failure.
		return false, err
	}
	return strings.Contains(string(out), "Status: active"), nil
}

// reportServiceResult maps a realitynode.Result to this command's exit
// code and stdout/stderr output (spec §13.1's table), without ever
// pattern-matching an error string:
//
//	PreTransaction                       -> 1 (nothing managed was modified)
//	Err != nil (transaction had begun)   -> 5 (could not confirm recovery;
//	                                         includes tamper / gate failed /
//	                                         rollback timeout and any other
//	                                         Recover failure); the
//	                                         transaction area is named
//	Outcome rolled back                  -> 4
//	Outcome committed / plain success    -> 0
func reportServiceResult(r realitynode.Result, stdout, stderr io.Writer) int {
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
			fmt.Fprintf(stderr, "the transaction area %s was preserved and needs manual attention; see docs/troubleshooting.md\n", r.TxnDir)
		}
	case 4:
		if r.Cause != nil {
			fmt.Fprintln(stderr, "error:", r.Cause)
		}
		fmt.Fprintln(stderr, "the operation was automatically rolled back to its pre-transaction state")
	default:
		if r.Stdout != "" {
			fmt.Fprint(stdout, r.Stdout)
		}
	}
	return code
}

// --- install ---

func cmdNodeServiceInstall(args []string, stdout, stderr io.Writer) int {
	rt := newServiceRuntime()
	if IsHelpOnly(args) {
		fmt.Fprintln(stdout, "usage: proxyctl node service install --name NAME --server ADDR [--port PORT] [--sni SNI]")
		return 0
	}
	specs := []FlagSpec{
		{Name: "name", Kind: FlagString},
		{Name: "server", Kind: FlagString},
		{Name: "port", Kind: FlagString},
		{Name: "sni", Kind: FlagString},
	}
	parsed, err := StrictScan(args, specs, 0)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	name := parsed.Values["name"]
	server := parsed.Values["server"]
	sni := parsed.Values["sni"]
	if sni == "" {
		sni = realitynode.DefaultSNI
	}
	port := realitynode.DefaultPort
	if v, ok := parsed.Values["port"]; ok {
		p, perr := strconv.Atoi(v)
		if perr != nil {
			fmt.Fprintln(stderr, "error: --port must be an integer")
			return 2
		}
		port = p
	}
	if name == "" || server == "" {
		fmt.Fprintln(stderr, "error: --name and --server are required")
		return 2
	}
	if err := RejectLeadingDash(name); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	// Parameter validation belongs to the parsing stage (spec §4.5, G2): a
	// bad value is a usage error (2) reported before root detection, locks
	// or any network call.
	params := realitynode.InstallParams{Name: name, Server: server, Port: port, SNI: sni}
	if err := realitynode.ValidateInstallParams(params); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}

	// Spec §8 order: parse -> root -> quick precheck (role, OS/arch/
	// systemd, instance state, disk, ports, selfcheck_port, ufw, §9.1
	// dest) -> lock (L1 then L3) -> recheck -> transaction.
	if !rt.requireRoot(stderr) {
		return 1
	}
	if !rt.requireSubscriptionCenterNo(stderr) {
		return 1
	}

	ctx, cancel := rt.newContext()
	defer cancel()

	env := rt.newEnv()
	plan, err := realitynode.PrecheckInstall(ctx, env, params)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	lease, err := rt.acquire(ctx, hostlock.L1, hostlock.L3)
	if err != nil {
		fmt.Fprintln(stderr, "error: another operation is in progress:", err)
		return 1
	}
	defer lease.Release()

	if !rt.requireSubscriptionCenterNo(stderr) {
		return 1
	}
	result := realitynode.RunInstall(ctx, env, plan)
	return reportServiceResult(result, stdout, stderr)
}

// --- reinstall ---

func cmdNodeServiceReinstall(args []string, stdout, stderr io.Writer) int {
	rt := newServiceRuntime()
	if IsHelpOnly(args) {
		fmt.Fprintln(stdout, "usage: proxyctl node service reinstall [--singbox-version V --singbox-sha256 H]")
		return 0
	}
	specs := []FlagSpec{
		{Name: "singbox-version", Kind: FlagString},
		{Name: "singbox-sha256", Kind: FlagString},
		// name/server/port/sni are explicitly rejected (spec §8.2): listed
		// here so a strict scan reports them the same way any other known-
		// but-forbidden-here flag would, rather than a generic "unknown
		// flag" — the exit code (2) is identical either way.
	}
	parsed, err := StrictScan(args, specs, 0)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	// spec §6: "缺少任一个都是退出码2" — a usage error, checked at the CLI
	// layer (not left to realitynode.Reinstall's own defensive re-check,
	// whose failure is a precondition/state error and maps to exit 1).
	if (parsed.Values["singbox-version"] == "") != (parsed.Values["singbox-sha256"] == "") {
		fmt.Fprintln(stderr, "error: --singbox-version and --singbox-sha256 must both be given, or both omitted")
		return 2
	}

	if !rt.requireRoot(stderr) {
		return 1
	}
	if !rt.requireSubscriptionCenterNo(stderr) {
		return 1
	}

	ctx, cancel := rt.newContext()
	defer cancel()

	env := rt.newEnv()
	plan, err := realitynode.PrecheckReinstall(ctx, env, realitynode.ReinstallParams{
		SingboxVersion: parsed.Values["singbox-version"],
		SingboxSHA256:  parsed.Values["singbox-sha256"],
	})
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	lease, err := rt.acquire(ctx, hostlock.L1, hostlock.L3)
	if err != nil {
		fmt.Fprintln(stderr, "error: another operation is in progress:", err)
		return 1
	}
	defer lease.Release()

	if !rt.requireSubscriptionCenterNo(stderr) {
		return 1
	}
	result := realitynode.RunReinstall(ctx, env, plan)
	return reportServiceResult(result, stdout, stderr)
}

// --- show ---

func cmdNodeServiceShow(args []string, stdout, stderr io.Writer) int {
	rt := newServiceRuntime()
	if IsHelpOnly(args) {
		fmt.Fprintln(stdout, "usage: proxyctl node service show")
		return 0
	}
	if _, err := StrictScan(args, nil, 0); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	if !rt.requireRoot(stderr) {
		return 1
	}

	base, cancelBase := rt.newContext()
	defer cancelBase()
	ctx, cancel := context.WithTimeout(base, showLockWait)
	defer cancel()
	lease, err := rt.acquire(ctx, hostlock.L3)
	if err != nil {
		fmt.Fprintln(stderr, "error: another operation is in progress:", err)
		return 1
	}
	defer lease.Release()

	env := rt.newEnv()
	result := realitynode.Show(context.Background(), env)
	return reportServiceResult(result, stdout, stderr)
}
