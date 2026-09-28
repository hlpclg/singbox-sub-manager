package realitynode

// This file implements the "re-exec the test binary as a fake external
// process" idiom (the same one Go's own os/exec tests use for
// TestHelperProcess), shared by download_test.go and selfcheck_test.go.
// Every test that needs a real subprocess — to prove ExecRunner's and
// managedProc's kill+Wait/process-group semantics against something that
// can genuinely ignore signals, fork grandchildren, or hold a port open —
// uses this instead of a mock, per AGENTS.md's "并发...资源生命周期相关实
// 现...接受独立并发审查" and the plan's "子进程生命周期用真实子进程...验证
// kill+Wait，而不是只用mock".
//
// Production code always execs a *path*, not this test binary directly
// (e.g. "sing-box version", "sing-box run -c <path>"). So tests point the
// production code at a tiny generated shell script (buildFakeBinary) that
// re-execs this same compiled test binary in "helper" mode, forwarding the
// real argv after "--". TestHelperProcess is a no-op under a normal `go
// test` run (the env var gate below), so it never affects real test
// results or counts.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	helperEnvGate     = "RTNODE_HELPER"
	helperEnvMode     = "RTNODE_HELPER_MODE"
	helperEnvExtra1   = "RTNODE_HELPER_ARG1"
	helperEnvExtra2   = "RTNODE_HELPER_ARG2"
	helperEnvHoldPrt  = "RTNODE_HOLDER_PORT"
	helperEnvPIDFile  = "RTNODE_PIDFILE"       // review finding F5
	helperEnvChildPID = "RTNODE_CHILD_PIDFILE" // review finding F5
)

// TestHelperProcess is not a real test: run under `go test`, it returns
// immediately unless helperEnvGate is set, in which case it behaves as
// whatever helperEnvMode says and calls os.Exit itself (never returning to
// the testing package).
func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperEnvGate) != "1" {
		return
	}
	mode := os.Getenv(helperEnvMode)
	switch mode {
	case "version":
		fmt.Println(os.Getenv(helperEnvExtra1))
		os.Exit(0)
	case "check_ok":
		os.Exit(0)
	case "check_fail":
		fmt.Fprintln(os.Stderr, "simulated check failure")
		os.Exit(1)
	case "hang":
		if pidFile := os.Getenv(helperEnvPIDFile); pidFile != "" {
			writePIDFileAtomic(pidFile, os.Getpid())
		}
		select {}
	case "sleep_then_exit":
		ms, _ := strconv.Atoi(os.Getenv(helperEnvExtra1))
		time.Sleep(time.Duration(ms) * time.Millisecond)
		os.Exit(0)
	case "hang_until_signal":
		// Polls for helperEnvExtra1 (a file path) to appear, then exits 0.
		// Used to let a test decide the exact moment a real, genuinely-
		// hung-until-then subprocess completes successfully (review
		// finding R2), rather than approximating it with a fixed sleep.
		if pidFile := os.Getenv(helperEnvPIDFile); pidFile != "" {
			writePIDFileAtomic(pidFile, os.Getpid())
		}
		signalFile := os.Getenv(helperEnvExtra1)
		for {
			if _, err := os.Stat(signalFile); err == nil {
				os.Exit(0)
			}
			time.Sleep(5 * time.Millisecond)
		}
	case "run_proxy":
		runFakeProxyFromArgs(os.Args, proxyModeForward)
	case "run_proxy_broken":
		runFakeProxyFromArgs(os.Args, proxyModeBroken)
	case "run_proxy_hang":
		runFakeProxyFromArgs(os.Args, proxyModeHang)
	case "spawn_holder":
		port := os.Getenv(helperEnvHoldPrt)
		self, _ := os.Executable()
		holder := exec.Command(self, "-test.run=^TestHelperProcess$", "--")
		holder.Env = append(os.Environ(), helperEnvMode+"=holder", helperEnvHoldPrt+"="+port)
		holder.Stdout = os.Stdout
		holder.Stderr = os.Stderr
		// Deliberately not setting Setpgid here: the grandchild must inherit
		// this process's own process group so a kill(-pgid) targeting this
		// process's group reaches it too — that inheritance is exactly the
		// property TestExecRunnerKillsProcessGroup verifies.
		_ = holder.Start()
		select {}
	case "holder":
		port := os.Getenv(helperEnvHoldPrt)
		l, err := net.Listen("tcp4", "127.0.0.1:"+port)
		if err != nil {
			os.Exit(1)
		}
		defer l.Close()
		select {}
	case "escape_holder":
		// Forks a grandchild that (a) inherits this process's stdout — the
		// write end of ExecRunner's internal output-capture pipe — and
		// (b) escapes into its own new process group (Setpgid), then
		// exits itself immediately and normally. Used only to prove
		// review finding F6's cmd.WaitDelay fix: a process-group kill
		// aimed at the direct child can never reach this escaped
		// grandchild, so if it keeps the pipe's write end open forever,
		// only WaitDelay (not process-group signaling) can keep Wait from
		// blocking forever.
		self, _ := os.Executable()
		holder := exec.Command(self, "-test.run=^TestHelperProcess$", "--")
		holder.Env = append(os.Environ(), helperEnvMode+"=hang")
		holder.Stdout = os.Stdout
		holder.Stderr = os.Stderr
		holder.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		_ = holder.Start()
		os.Exit(0)
	case "pid_child":
		// review finding F5: a grandchild that only records its own pid
		// (inheriting its parent's process group, see runFakeProxyFromArgs)
		// and then holds on, so a test can prove process-group-wide
		// termination reaches it too.
		if pidFile := os.Getenv(helperEnvPIDFile); pidFile != "" {
			writePIDFileAtomic(pidFile, os.Getpid())
		}
		select {}
	default:
		os.Exit(2)
	}
}

// buildFakeBinary writes an executable shell script at path that re-execs
// this compiled test binary in TestHelperProcess mode with helper mode
// "mode" and extraEnv set, forwarding whatever argv production code passes
// it (e.g. "version", or "run -c <path>") after "--".
func buildFakeBinary(t *testing.T, mode string, extraEnv ...string) string {
	t.Helper()
	testBin, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-sing-box")

	env := ""
	for _, kv := range append([]string{helperEnvGate + "=1", helperEnvMode + "=" + mode}, extraEnv...) {
		env += "export " + shQuote(kv) + "\n"
	}
	content := "#!/bin/sh\n" + env + "exec " + shQuote(testBin) + " -test.run=^TestHelperProcess$ -- \"$@\"\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return script
}

func shQuote(s string) string {
	return "'" + s + "'"
}

// writePIDFileAtomic writes pid to path atomically: it writes to a temp
// file in path's own directory first (so the final rename is on the same
// filesystem and therefore atomic), then renames it into place. Plain
// os.WriteFile is create-then-write-then-close, not atomic: a reader that
// only checks "does the file exist" (as waitForPIDFileReady's predecessor
// and assertProcessGone both used to) can observe the file after it is
// created but before its content is written, and see it as empty — a real
// (if normally very narrow) race, deterministically reproduced and fixed
// here rather than papered over by widening a sleep. Best effort, same as
// the plain os.WriteFile calls this replaces: errors are not fatal to the
// calling fake process.
func writePIDFileAtomic(path string, pid int) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	_, werr := tmp.WriteString(strconv.Itoa(pid))
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
	}
}

// fakeSystemctlClock drives a TimeoutFunc (its NewTimeout method) whose
// derived contexts end only when this clock's own virtual time is
// explicitly Advance()'d to or past their deadline — never for real. This
// is review finding R2's "假时钟"，used together with a real, genuinely
// hanging (or held-hanging-until-signaled) subprocess so a test can prove
// "systemctl advancing 10 (virtual) seconds is still within a 15-second
// budget" and "advancing to/past 15 (virtual) seconds ends it" without
// ever waiting 10 or 15 real seconds, and without conflating the virtual
// clock with the real wall-clock time a subprocess actually takes to
// start/exit.
type fakeSystemctlClock struct {
	mu       sync.Mutex
	now      time.Time
	watchers []fakeClockWatcher

	// Registered receives a value each time NewTimeout registers a new
	// watcher, so a test can synchronize on "the code under test has
	// asked for its timeout context" before calling Advance — otherwise
	// there is an inherent race between the goroutine running the code
	// under test and the test's own Advance call.
	Registered chan struct{}

	// LastRequested records the most recent d passed to NewTimeout, so a
	// test can assert it equals the real spec constant (e.g.
	// systemctlTimeout) even though the derived context's actual
	// cancellation is driven by Advance, not by d as a real duration.
	LastRequested time.Duration
}

type fakeClockWatcher struct {
	deadline time.Time
	cancel   context.CancelFunc
}

func newFakeSystemctlClock() *fakeSystemctlClock {
	return &fakeSystemctlClock{now: time.Now(), Registered: make(chan struct{}, 8)}
}

// NewTimeout is a TimeoutFunc: it derives a cancelable context from parent
// and registers it to be cancelled once c.now reaches or passes c.now (at
// registration time) + d — driven entirely by Advance, never by a real
// timer.
func (c *fakeSystemctlClock) NewTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	c.mu.Lock()
	c.LastRequested = d
	deadline := c.now.Add(d)
	due := !c.now.Before(deadline)
	if !due {
		c.watchers = append(c.watchers, fakeClockWatcher{deadline: deadline, cancel: cancel})
	}
	c.mu.Unlock()
	select {
	case c.Registered <- struct{}{}:
	default:
	}
	if due {
		cancel()
	}
	return ctx, cancel
}

// Advance moves the virtual clock forward by d and synchronously cancels
// every outstanding context whose deadline that reaches or passes.
func (c *fakeSystemctlClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []context.CancelFunc
	remaining := c.watchers[:0]
	for _, w := range c.watchers {
		if !c.now.Before(w.deadline) {
			due = append(due, w.cancel)
		} else {
			remaining = append(remaining, w)
		}
	}
	c.watchers = remaining
	c.mu.Unlock()
	for _, cancel := range due {
		cancel()
	}
}

// recordingShortTimeout returns a TimeoutFunc that records every requested
// duration into *got (last call wins) and derives the returned context
// from parent using fire — a short real duration — instead of d. This
// proves two things at once (review finding F1): (a) exactly which
// duration production code requested, asserted separately against the
// spec constant; and (b) that THIS seam's own budget is what terminates
// the operation, not whatever deadline parent already carries — a parent
// given no deadline at all (context.Background()) would otherwise never
// terminate anything by itself, so an operation that still completes
// quickly must have been bounded by the substituted fire duration.
func recordingShortTimeout(got *time.Duration, fire time.Duration) TimeoutFunc {
	return func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		*got = d
		return context.WithTimeout(parent, fire)
	}
}

// waitForPIDFileReady blocks (up to timeout) until path both exists AND
// its content parses as a positive integer pid, returning that pid.
// writePIDFileAtomic's rename already makes "the name exists" imply
// "fully written" on its own, but this additionally gates on successful
// parsing — not merely on existence — per the explicit requirement that
// readiness means "content parses as a positive integer", not just "a
// directory entry is there" (defense in depth against any other source of
// a transiently-unparseable read, not only the non-atomic-write race this
// was written to fix).
func waitForPIDFileReady(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				t.Fatalf("read %s: %v", path, err)
			}
			lastErr = err
		} else if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && pid > 0 {
			return pid
		} else {
			lastErr = perr
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never became a valid pid file (last error: %v)", path, lastErr)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// assertProcessGone waits for a pid previously written to pidFile (by a
// fake process using helperEnvPIDFile/helperEnvChildPID, via
// writePIDFileAtomic) to become readable and parseable, then requires
// that, by the time this is called, that pid no longer exists — kill(pid,
// 0) reports ESRCH. Bounded, brief retrying accommodates the real (but
// short) time between a killed process's signal delivery and the OS
// actually reaping/recycling its pid. Review finding F5.
func assertProcessGone(t *testing.T, pidFile string) {
	t.Helper()
	// A generous bound (not one of the spec's own timeouts): -race and a
	// loaded CI machine both slow down a freshly-forked grandchild's Go
	// runtime bootstrap, and this is pure test-infra tolerance, not a
	// stand-in for any spec-mandated wait.
	deadline := time.Now().Add(5 * time.Second)

	pid := waitForPIDFileReady(t, pidFile, 5*time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d (from %s) still alive after the caller returned: kill(pid,0) = %v", pid, pidFile, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// buildFakeSystemctl is buildFakeBinary, plus a symlink named "systemctl"
// on a directory prepended to this test's PATH (via t.Setenv, auto-
// restored) — so that Systemctl's hardcoded exec.Command("systemctl", ...)
// resolves to the fake (review finding F1's "假systemctl" tests, which need
// a genuine subprocess reachable under the literal name "systemctl").
func buildFakeSystemctl(t *testing.T, mode string, extraEnv ...string) {
	t.Helper()
	script := buildFakeBinary(t, mode, extraEnv...)
	dir := filepath.Dir(script)
	link := filepath.Join(dir, "systemctl")
	if err := os.Symlink(script, link); err != nil {
		t.Fatalf("symlink systemctl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

type proxyMode int

const (
	proxyModeForward proxyMode = iota // relay to whatever the request's absolute URI names
	proxyModeBroken                   // always answer 502, regardless of target
	proxyModeHang                     // never answer (used to exercise request-timeout teardown)
)

// runFakeProxyFromArgs implements the "run_proxy*" modes: argv (after
// "--") is expected to be ["run", "-c", "<configPath>"], matching the real
// sing-box CLI convention production code relies on. It reads the
// mixed-inbound listen_port out of the config JSON and serves a
// bare-bones HTTP endpoint on 127.0.0.1:<that port> until killed —
// sufficient to exercise RunSelfcheck's E2/E3 orchestration, retries,
// failure classification and cleanup without needing a real Reality
// handshake (already proven for real in Task 1).
func runFakeProxyFromArgs(args []string, mode proxyMode) {
	var configPath string
	for i, a := range args {
		if a == "-c" && i+1 < len(args) {
			configPath = args[i+1]
		}
	}
	if configPath == "" {
		os.Exit(2)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		os.Exit(2)
	}
	var parsed struct {
		Inbounds []struct {
			ListenPort int `json:"listen_port"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil || len(parsed.Inbounds) == 0 {
		os.Exit(2)
	}
	port := parsed.Inbounds[0].ListenPort

	// review finding F5: record this process's own pid, and optionally
	// spawn a grandchild that records its pid too and then just holds on,
	// so a test can assert both are truly gone (kill(pid,0)==ESRCH) after
	// RunSelfcheck returns — not just that the port was freed.
	if pidFile := os.Getenv(helperEnvPIDFile); pidFile != "" {
		writePIDFileAtomic(pidFile, os.Getpid())
	}
	if childPIDFile := os.Getenv(helperEnvChildPID); childPIDFile != "" {
		self, _ := os.Executable()
		child := exec.Command(self, "-test.run=^TestHelperProcess$", "--")
		child.Env = append(os.Environ(), helperEnvMode+"=pid_child", helperEnvPIDFile+"="+childPIDFile)
		// Deliberately not setting Setpgid: the grandchild must inherit
		// this process's own process group so that killing this process's
		// group (which is what killProcessGroup/managedProc.stop do) also
		// reaches it — the exact property this test is proving.
		_ = child.Start()
	}

	l, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		os.Exit(1)
	}
	var handler http.HandlerFunc
	switch mode {
	case proxyModeBroken:
		handler = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }
	case proxyModeHang:
		handler = func(w http.ResponseWriter, r *http.Request) { select {} }
	default:
		handler = proxyHandler
	}
	srv := &http.Server{Handler: handler}
	_ = srv.Serve(l)
	os.Exit(0)
}

// proxyHandler is a minimal HTTP forward proxy: it treats r.URL as
// absolute (as Go's http.Transport sends it when routed through
// Proxy: http.ProxyURL for a plain http:// target) and relays the request
// to whatever host:port it names.
func proxyHandler(w http.ResponseWriter, r *http.Request) {
	outReq, err := http.NewRequest(r.Method, r.URL.String(), r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	outReq.Header = r.Header
	resp, err := http.DefaultTransport.RoundTrip(outReq)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
