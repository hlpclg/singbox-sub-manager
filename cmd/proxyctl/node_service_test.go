package main

// node_service_test.go covers plan Task 7's CLI-layer strict parsing for
// `node service install|reinstall|show` (design spec §4.5, §8): every
// case here returns before requireRoot is ever reached, so none of these
// tests need root, a lock, or network access — exactly spec §4.5's "错误
// 用法返回2,且没有取锁、网络调用,目标文件bytes/mtime不变".
import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hlpclg/singbox-sub-manager/internal/hostlock"
	"github.com/hlpclg/singbox-sub-manager/internal/realitynode"
	"github.com/hlpclg/singbox-sub-manager/internal/role"
	"github.com/hlpclg/singbox-sub-manager/internal/transaction"
)

func TestNodeServiceHelp(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"node", "service", "--help"}, &out, &errb)
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "install") || !strings.Contains(out.String(), "reinstall") || !strings.Contains(out.String(), "show") {
		t.Fatalf("help output missing subcommands: %s", out.String())
	}

	out.Reset()
	errb.Reset()
	code = run([]string{"node", "service", "install", "--help"}, &out, &errb)
	if code != 0 || !strings.Contains(out.String(), "install") {
		t.Fatalf("install --help: code=%d out=%s err=%s", code, out.String(), errb.String())
	}
}

func TestNodeServiceUnknownSubcommand(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"node", "service", "bogus"}, &out, &errb)
	if code != 2 {
		t.Fatalf("code=%d want 2, err=%s", code, errb.String())
	}
}

func TestNodeServiceNoArgs(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"node", "service"}, &out, &errb)
	if code != 2 {
		t.Fatalf("code=%d want 2, err=%s", code, errb.String())
	}
}

func TestNodeServiceInstallStrictParsing(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"missing --name and --server", []string{"node", "service", "install"}},
		{"missing --server", []string{"node", "service", "install", "--name", "jp1"}},
		{"missing --name", []string{"node", "service", "install", "--server", "1.2.3.4"}},
		{"unknown flag", []string{"node", "service", "install", "--name", "jp1", "--server", "1.2.3.4", "--bogus", "x"}},
		{"duplicate flag", []string{"node", "service", "install", "--name", "a", "--name", "b", "--server", "1.2.3.4"}},
		{"positional argument not allowed", []string{"node", "service", "install", "--name", "jp1", "--server", "1.2.3.4", "extra"}},
		{"non-integer port", []string{"node", "service", "install", "--name", "jp1", "--server", "1.2.3.4", "--port", "notanum"}},
		{"name starting with dash", []string{"node", "service", "install", "--name", "-evil", "--server", "1.2.3.4"}},
		{"--publish is unknown here", []string{"node", "service", "install", "--name", "jp1", "--server", "1.2.3.4", "--publish"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := run(tc.args, &out, &errb)
			if code != 2 {
				t.Fatalf("code=%d, want 2; out=%s err=%s", code, out.String(), errb.String())
			}
			if out.String() != "" {
				t.Fatalf("want no stdout on a usage error, got %q", out.String())
			}
		})
	}
}

func TestNodeServiceReinstallStrictParsing(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"name is forbidden", []string{"node", "service", "reinstall", "--name", "jp1"}},
		{"server is forbidden", []string{"node", "service", "reinstall", "--server", "1.2.3.4"}},
		{"port is forbidden", []string{"node", "service", "reinstall", "--port", "443"}},
		{"sni is forbidden", []string{"node", "service", "reinstall", "--sni", "example.com"}},
		{"version without sha", []string{"node", "service", "reinstall", "--singbox-version", "1.14.2"}},
		{"sha without version", []string{"node", "service", "reinstall", "--singbox-sha256", strings.Repeat("a", 64)}},
		{"positional argument not allowed", []string{"node", "service", "reinstall", "extra"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := run(tc.args, &out, &errb)
			if code != 2 {
				t.Fatalf("code=%d, want 2; out=%s err=%s", code, out.String(), errb.String())
			}
			if out.String() != "" {
				t.Fatalf("want no stdout on a rejected reinstall, got %q", out.String())
			}
		})
	}
}

func TestNodeServiceShowStrictParsing(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"node", "service", "show", "extra"}, &out, &errb)
	if code != 2 {
		t.Fatalf("code=%d, want 2; out=%s err=%s", code, out.String(), errb.String())
	}
	if out.String() != "" {
		t.Fatalf("want no stdout, got %q", out.String())
	}
}

// --- runtime-injected tests (plan Task 7 P1-3f, review P1-4/P2-5/P2-6) ---
//
// Everything below replaces newServiceRuntime, so nothing needs root,
// /run/lock, a real systemd, or the network; every process-level effect is
// recorded in a callLog whose contents and order the tests assert.

type callLog struct {
	mu     sync.Mutex
	events []string
}

func (l *callLog) add(e string) {
	l.mu.Lock()
	l.events = append(l.events, e)
	l.mu.Unlock()
}

func (l *callLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

func (l *callLog) String() string { return strings.Join(l.list(), ",") }

type fakeRuntimeOpts struct {
	euid       int
	roles      []role.Status // successive detectRole results; the last repeats
	acquireErr error
	// acquireBlock, when set, makes acquire block until its ctx is done.
	acquireBlock chan struct{}
	ctx          context.Context
	cancel       context.CancelFunc
	destErr      error
	roots        realitynode.Roots
}

// installFakeRuntime replaces newServiceRuntime for the test's duration and
// returns the call log. The Env it builds passes every precheck with local
// fakes and logs its network-facing calls ("dest", "download").
func installFakeRuntime(t *testing.T, o fakeRuntimeOpts) *callLog {
	t.Helper()
	log := &callLog{}
	if len(o.roles) == 0 {
		o.roles = []role.Status{role.StatusNo}
	}
	roleCalls := 0

	destSrv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(destSrv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(destSrv.Certificate())

	base := t.TempDir()
	roots := o.roots
	if roots.StateDir == "" {
		roots = realitynode.Roots{
			SystemdUnit: filepath.Join(base, "systemd", "proxyctl-reality.service"),
			ConfigDir:   filepath.Join(base, "etc", "proxyctl-reality"), Config: filepath.Join(base, "etc", "proxyctl-reality", "config.json"),
			StateDir: filepath.Join(base, "var", "proxyctl-reality"), Secrets: filepath.Join(base, "var", "proxyctl-reality", "secrets.json"),
			Instance: filepath.Join(base, "var", "proxyctl-reality", "instance.json"), Manifest: filepath.Join(base, "var", "proxyctl-reality", "manifest.json"),
			BinDir: filepath.Join(base, "usr", "proxyctl-reality"), TxnDir: filepath.Join(base, "var", "proxyctl-reality-txn"),
			TombstoneGlob: filepath.Join(base, "var", ".proxyctl-reality-txn.tomb-*"),
		}
	}

	orig := newServiceRuntime
	t.Cleanup(func() { newServiceRuntime = orig })
	newServiceRuntime = func() serviceRuntime {
		return serviceRuntime{
			geteuid: func() int { log.add("geteuid"); return o.euid },
			detectRole: func() role.Result {
				log.add("role")
				i := roleCalls
				if i >= len(o.roles) {
					i = len(o.roles) - 1
				}
				roleCalls++
				return role.Result{Status: o.roles[i]}
			},
			acquire: func(ctx context.Context, levels ...hostlock.Level) (*hostlock.Lease, error) {
				var names []string
				for _, l := range levels {
					names = append(names, l.String())
				}
				log.add("lock:" + strings.Join(names, "+"))
				if o.acquireBlock != nil {
					close(o.acquireBlock)
					<-ctx.Done()
					return nil, ctx.Err()
				}
				if o.acquireErr != nil {
					return nil, o.acquireErr
				}
				return &hostlock.Lease{}, nil
			},
			newEnv: func() *realitynode.Env {
				log.add("newEnv")
				return &realitynode.Env{
					Roots: roots, FS: realitynode.OSStateFS(),
					GOARCH: "amd64", SystemdAvailable: func() bool { return true },
					CheckDiskSpace:    func(string, uint64) error { return nil },
					CheckPortBindable: func(int) (bool, string) { return true, "" },
					PortRange:         func() (int, int, error) { return 20000, 30000, nil },
					PortBind:          func(int) bool { return true },
					TLSDialer: func(ctx context.Context, network, _ string, cfg *tls.Config) (*tls.Conn, error) {
						log.add("dest")
						if o.destErr != nil {
							return nil, o.destErr
						}
						cfg.RootCAs = pool
						c, err := (&tls.Dialer{Config: cfg}).DialContext(ctx, network, destSrv.Listener.Addr().String())
						if err != nil {
							return nil, err
						}
						return c.(*tls.Conn), nil
					},
					Downloader: downloaderFunc(func() { log.add("download") }),
				}
			},
			newContext: func() (context.Context, context.CancelFunc) {
				if o.ctx != nil {
					return o.ctx, o.cancel
				}
				return context.WithCancel(context.Background())
			},
		}
	}
	return log
}

type downloaderFunc func()

func (f downloaderFunc) Get(ctx context.Context, url string) (io.ReadCloser, error) {
	f()
	return nil, errors.New("test downloader: network disabled")
}

func goodInstallArgs() []string {
	return []string{"node", "service", "install", "--name", "jp1", "--server", "203.0.113.10", "--port", "8443", "--sni", "example.com"}
}

// TestNodeServiceParseFailuresTouchNothing: a usage error returns 2 before
// root detection, role detection, locking, Env construction or any network
// call (spec §4.5, §14.1), and leaves files untouched.
func TestNodeServiceParseFailuresTouchNothing(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "nodes.conf")
	if err := os.WriteFile(sentinel, []byte("[JP]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fi0, _ := os.Stat(sentinel)

	with := func(extra ...string) []string { return append([]string{"node", "service", "install"}, extra...) }
	cases := map[string][]string{
		"unknown flag":          with("--name", "a", "--server", "1.2.3.4", "--bogus"),
		"duplicate flag":        with("--name", "a", "--name", "b", "--server", "1.2.3.4"),
		"missing value":         with("--name", "a", "--server"),
		"missing required":      with("--name", "a"),
		"positional":            with("--name", "a", "--server", "1.2.3.4", "x"),
		"port not a number":     with("--name", "a", "--server", "1.2.3.4", "--port", "abc"),
		"port too large":        with("--name", "a", "--server", "1.2.3.4", "--port", "70000"),
		"port zero":             with("--name", "a", "--server", "1.2.3.4", "--port", "0"),
		"port negative":         with("--name", "a", "--server", "1.2.3.4", "--port=-5"),
		"ipv6 server":           with("--name", "a", "--server", "2001:db8::1"),
		"ip sni":                with("--name", "a", "--server", "1.2.3.4", "--sni", "1.2.3.4"),
		"ipv6 sni":              with("--name", "a", "--server", "1.2.3.4", "--sni", "::1"),
		"dashed name":           with("--name", "-x", "--server", "1.2.3.4"),
		"invalid name chars":    with("--name", "bad name!", "--server", "1.2.3.4"),
		"server label too long": with("--name", "a", "--server", strings.Repeat("a", 64)+".com"),
		"server without dot":    with("--name", "a", "--server", "localhost"),
		"help mixed":            with("--help", "--name", "a"),
		"reinstall forbidden":   {"node", "service", "reinstall", "--name", "a"},
		"reinstall unpaired":    {"node", "service", "reinstall", "--singbox-version", "1.2.3"},
		"reinstall positional":  {"node", "service", "reinstall", "x"},
		"show positional":       {"node", "service", "show", "x"},
		"show flag":             {"node", "service", "show", "--bogus"},
	}
	for name, args := range cases {
		t.Run(strings.ReplaceAll(name, " ", "_"), func(t *testing.T) {
			log := installFakeRuntime(t, fakeRuntimeOpts{})
			var out, errb bytes.Buffer
			code := run(args, &out, &errb)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (stderr: %s)", code, errb.String())
			}
			if got := log.list(); len(got) != 0 {
				t.Fatalf("a usage error must touch nothing (no root check, role, lock, env, network); saw %v", got)
			}
			if out.String() != "" {
				t.Fatalf("stdout must be empty, got %q", out.String())
			}
			fi1, _ := os.Stat(sentinel)
			if !fi1.ModTime().Equal(fi0.ModTime()) {
				t.Fatalf("sentinel file changed")
			}
		})
	}
}

// TestNodeServiceInstallOrdering asserts spec §8's shared order: parse ->
// root -> quick precheck (incl. the §9.1 dest probe) -> lock -> recheck.
func TestNodeServiceInstallOrdering(t *testing.T) {
	t.Run("dest probe happens before the lock", func(t *testing.T) {
		log := installFakeRuntime(t, fakeRuntimeOpts{acquireErr: errors.New("simulated lock timeout")})
		var out, errb bytes.Buffer
		if code := run(goodInstallArgs(), &out, &errb); code != 1 {
			t.Fatalf("exit = %d, want 1 (stderr: %s)", code, errb.String())
		}
		ev := log.list()
		destIdx, lockIdx := -1, -1
		for i, e := range ev {
			if e == "dest" && destIdx < 0 {
				destIdx = i
			}
			if strings.HasPrefix(e, "lock:") && lockIdx < 0 {
				lockIdx = i
			}
		}
		if destIdx < 0 || lockIdx < 0 || destIdx > lockIdx {
			t.Fatalf("want the dest probe before the lock, events = %v", ev)
		}
		if got := ev[lockIdx]; got != "lock:L1+L3" {
			t.Errorf("install must take L1+L3, took %s", got)
		}
		if ev[0] != "geteuid" {
			t.Errorf("root check must come first, events = %v", ev)
		}
	})

	t.Run("a failed precheck never takes the lock", func(t *testing.T) {
		log := installFakeRuntime(t, fakeRuntimeOpts{destErr: errors.New("dest unreachable")})
		var out, errb bytes.Buffer
		if code := run(goodInstallArgs(), &out, &errb); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		for _, e := range log.list() {
			if strings.HasPrefix(e, "lock:") {
				t.Fatalf("lock taken despite a failed precheck: %v", log.list())
			}
		}
	})

	t.Run("role is rechecked under the lock and gates the transaction", func(t *testing.T) {
		log := installFakeRuntime(t, fakeRuntimeOpts{roles: []role.Status{role.StatusNo, role.StatusYes}})
		var out, errb bytes.Buffer
		if code := run(goodInstallArgs(), &out, &errb); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		ev := log.list()
		lockIdx, roleAfter := -1, false
		for i, e := range ev {
			if strings.HasPrefix(e, "lock:") {
				lockIdx = i
			}
			if e == "role" && lockIdx >= 0 && i > lockIdx {
				roleAfter = true
			}
			if e == "download" {
				t.Fatalf("the transaction started although the rechecked role is 'yes': %v", ev)
			}
		}
		if !roleAfter {
			t.Fatalf("role must be rechecked after the lock, events = %v", ev)
		}
	})

	t.Run("non-root stops at the root check", func(t *testing.T) {
		log := installFakeRuntime(t, fakeRuntimeOpts{euid: 1000})
		var out, errb bytes.Buffer
		if code := run(goodInstallArgs(), &out, &errb); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if got := log.String(); got != "geteuid" {
			t.Fatalf("events = %s, want only the root check", got)
		}
	})

	t.Run("role other than no stops before env construction and lock", func(t *testing.T) {
		log := installFakeRuntime(t, fakeRuntimeOpts{roles: []role.Status{role.StatusUncertain}})
		var out, errb bytes.Buffer
		if code := run(goodInstallArgs(), &out, &errb); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if got := log.String(); got != "geteuid,role" {
			t.Fatalf("events = %s, want geteuid,role", got)
		}
	})
}

func TestNodeServiceShowTakesOnlyL3(t *testing.T) {
	log := installFakeRuntime(t, fakeRuntimeOpts{})
	var out, errb bytes.Buffer
	code := run([]string{"node", "service", "show"}, &out, &errb)
	if code != 1 { // the fake host has no instance: show reports and exits 1
		t.Fatalf("exit = %d, want 1 (no instance); stderr: %s", code, errb.String())
	}
	var locks []string
	for _, e := range log.list() {
		if strings.HasPrefix(e, "lock:") {
			locks = append(locks, e)
		}
	}
	if len(locks) != 1 || locks[0] != "lock:L3" {
		t.Fatalf("show must take exactly L3, took %v", locks)
	}
	for _, e := range log.list() {
		if e == "role" {
			t.Errorf("show must not run role detection: %v", log.list())
		}
	}
}

func TestNodeServiceReinstallIneligibleNeverLocks(t *testing.T) {
	log := installFakeRuntime(t, fakeRuntimeOpts{})
	var out, errb bytes.Buffer
	if code := run([]string{"node", "service", "reinstall"}, &out, &errb); code != 1 {
		t.Fatalf("exit = %d, want 1 (not installed)", code)
	}
	for _, e := range log.list() {
		if strings.HasPrefix(e, "lock:") {
			t.Fatalf("lock taken for an ineligible reinstall: %v", log.list())
		}
	}
}

// TestNodeServiceLockWaitCancel: a signal while waiting for the lock ends
// the command immediately with exit 1 and starts nothing (spec §13.3).
func TestNodeServiceLockWaitCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	log := installFakeRuntime(t, fakeRuntimeOpts{ctx: ctx, cancel: cancel, acquireBlock: entered})
	done := make(chan int, 1)
	var out, errb bytes.Buffer
	go func() { done <- run(goodInstallArgs(), &out, &errb) }()

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the command never reached the lock")
	}
	cancel() // SIGINT while blocked on the lock
	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelling during the lock wait did not return promptly")
	}
	for _, e := range log.list() {
		if e == "download" {
			t.Fatalf("the transaction started after the cancelled lock wait: %v", log.list())
		}
	}
}

// TestReportServiceResultExitCodes: the spec §13.1 mapping, decided by
// Result's type-level fields, never by error text.
func TestReportServiceResultExitCodes(t *testing.T) {
	txnDir := "/var/lib/proxyctl-reality-txn"
	cases := []struct {
		name       string
		res        realitynode.Result
		want       int
		wantStdout string
		wantStderr string
	}{
		{"pre-transaction failure", realitynode.Result{PreTransaction: true, Err: errors.New("x")}, 1, "", "error: x"},
		{"no change removes the area", realitynode.Result{PreTransaction: true, HasOutcome: true, Outcome: transaction.OutcomeNoChange, Err: errors.New("x")}, 1, "", ""},
		{"rolled back", realitynode.Result{HasOutcome: true, Outcome: transaction.OutcomeRolledBack, Cause: errors.New("selfcheck failed")}, 4, "", "selfcheck failed"},
		{"committed", realitynode.Result{HasOutcome: true, Outcome: transaction.OutcomeCommitted, Stdout: "# c\ncmd\n"}, 0, "# c\ncmd\n", ""},
		{"plain success (show)", realitynode.Result{Stdout: "out\n", Warnings: []string{"hint"}}, 0, "out\n", "hint"},
		{"tampered", realitynode.Result{Err: transaction.ErrTampered, TxnDir: txnDir}, 5, "", txnDir},
		{"gate failed", realitynode.Result{Err: transaction.ErrGateFailed, TxnDir: txnDir}, 5, "", txnDir},
		{"rollback timeout", realitynode.Result{Err: transaction.ErrRollbackTimeout, TxnDir: txnDir}, 5, "", txnDir},
		{"unclassified recovery failure after the transaction began", realitynode.Result{Err: errors.New("stop failed"), TxnDir: txnDir}, 5, "", txnDir},
	}
	for _, tc := range cases {
		t.Run(strings.ReplaceAll(tc.name, " ", "_"), func(t *testing.T) {
			var out, errb bytes.Buffer
			if got := reportServiceResult(tc.res, &out, &errb); got != tc.want {
				t.Fatalf("exit = %d, want %d (stderr: %s)", got, tc.want, errb.String())
			}
			if out.String() != tc.wantStdout {
				t.Errorf("stdout = %q, want %q", out.String(), tc.wantStdout)
			}
			if tc.wantStderr != "" && !strings.Contains(errb.String(), tc.wantStderr) {
				t.Errorf("stderr %q does not contain %q", errb.String(), tc.wantStderr)
			}
		})
	}
}
