package realitynode

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// --- §9.1 dest probe ---

func TestProbeDest(t *testing.T) {
	t.Run("success against a local TLS server", func(t *testing.T) {
		ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer ts.Close()
		addr := ts.Listener.Addr().String()
		pool := x509.NewCertPool()
		pool.AddCert(ts.Certificate())
		dialer := func(ctx context.Context, network, _ string, cfg *tls.Config) (*tls.Conn, error) {
			cfg.RootCAs = pool
			d := &tls.Dialer{Config: cfg}
			conn, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return conn.(*tls.Conn), nil
		}
		_, cat, err := ProbeDest(context.Background(), "example.com", dialer, nil)
		if err != nil {
			t.Fatalf("ProbeDest: %v (cat=%v)", err, cat)
		}
		if cat != CategoryNone {
			t.Fatalf("cat = %v, want CategoryNone", cat)
		}
	})

	t.Run("unreachable dest is CategoryDestUnreachable", func(t *testing.T) {
		dialer := func(ctx context.Context, network, addr string, cfg *tls.Config) (*tls.Conn, error) {
			return nil, errors.New("connection refused")
		}
		_, cat, err := ProbeDest(context.Background(), "example.com", dialer, nil)
		if err == nil {
			t.Fatalf("expected error")
		}
		if cat != CategoryDestUnreachable {
			t.Fatalf("cat = %v, want CategoryDestUnreachable", cat)
		}
	})

	t.Run("bounded by its own 10s timeout, no real wait beyond the caller's shorter deadline", func(t *testing.T) {
		dialer := func(ctx context.Context, network, addr string, cfg *tls.Config) (*tls.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, cat, err := ProbeDest(ctx, "example.com", dialer, nil)
		if err == nil {
			t.Fatalf("expected error")
		}
		if cat != CategoryDestUnreachable {
			t.Fatalf("cat = %v, want CategoryDestUnreachable", cat)
		}
		if time.Since(start) > 2*time.Second {
			t.Fatalf("ProbeDest took too long: %s", time.Since(start))
		}
	})

	// review finding F1: proves ProbeDest's own 10-second budget is what
	// terminates it even when the parent ctx carries no deadline at all —
	// distinct from the subtest above, which used a shorter external ctx
	// rather than exercising ProbeDest's internal default.
	t.Run("own 10-second budget fires despite a parent with no deadline", func(t *testing.T) {
		dialer := func(ctx context.Context, network, addr string, cfg *tls.Config) (*tls.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		var requested time.Duration
		nt := recordingShortTimeout(&requested, 50*time.Millisecond)
		start := time.Now()
		_, cat, err := ProbeDest(context.Background(), "example.com", dialer, nt)
		elapsed := time.Since(start)
		if err == nil {
			t.Fatalf("expected an error")
		}
		if cat != CategoryDestUnreachable {
			t.Fatalf("cat = %v, want CategoryDestUnreachable", cat)
		}
		if requested != destProbeTimeout {
			t.Fatalf("requested timeout = %s, want %s (spec §13.2 \"dest 探测 10 秒\")", requested, destProbeTimeout)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("ProbeDest took %s; its own budget should have fired", elapsed)
		}
	})
}

// --- §9.3/§9.4 RunSelfcheck lifecycle matrix ---

func countOpenFDs(t *testing.T) int {
	t.Helper()
	if runtime.GOOS != "linux" {
		return -1 // not countable on this platform; callers skip the assertion
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	return len(entries)
}

func assertNoFDLeak(t *testing.T, before int) {
	t.Helper()
	if before < 0 {
		t.Skip("fd counting only available on linux")
	}
	// Give any background goroutine's deferred Close a brief, bounded
	// moment to actually complete before recounting.
	deadline := time.Now().Add(time.Second)
	for {
		after := countOpenFDs(t)
		if after <= before {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("fd count grew from %d to %d after RunSelfcheck returned", before, after)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func freePortForTest(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePortForTest: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

func baseDataPathConfig(t *testing.T, clientBin string) DataPathConfig {
	t.Helper()
	dir := t.TempDir()
	f, err := os.Open(dir)
	if err != nil {
		t.Fatalf("open %s: %v", dir, err)
	}
	t.Cleanup(func() { f.Close() })
	return DataPathConfig{
		ListenPort:       443,
		SelfcheckPort:    freePortForTest(t),
		SNI:              "www.microsoft.com",
		UUID:             "11111111-1111-4111-8111-111111111111",
		PrivateKey:       mustGenPrivateKey(t),
		ShortID:          "0123456789abcdef",
		ClientBinaryPath: clientBin,
		ClientConfigDir:  f,
		ClientConfigName: "client.json",
		ClientStartGrace: 500 * time.Millisecond,
		EgressURL:        "", // set per-test
	}
}

func mustGenPrivateKey(t *testing.T) string {
	t.Helper()
	id, err := GenerateIdentity(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	return id.PrivateKey
}

func TestSelfcheckLifecycleMatrix(t *testing.T) {
	t.Run("success: 9.3 passes and 9.4 egress passes", func(t *testing.T) {
		egress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		defer egress.Close()

		pidFile := filepath.Join(t.TempDir(), "client.pid")
		clientBin := buildFakeBinary(t, "run_proxy", helperEnvPIDFile+"="+pidFile)
		cfg := baseDataPathConfig(t, clientBin)
		cfg.EgressURL = egress.URL + "/generate_204"
		before := countOpenFDs(t)

		out := RunSelfcheck(context.Background(), cfg)
		if out.Category != CategoryNone || out.Err != nil {
			t.Fatalf("out = %+v", out)
		}
		if !out.EgressOK {
			t.Fatalf("EgressOK = false, want true: warn=%q", out.EgressWarn)
		}
		if out.EgressCategory != CategoryNone {
			t.Fatalf("EgressCategory = %v, want CategoryNone on egress success", out.EgressCategory)
		}
		assertPortBindable(t, cfg.SelfcheckPort)
		assertNoFDLeak(t, before)
		// review finding F5: the temp client must be gone (kill+Wait), not
		// merely "the port is free". Grandchild (process-group) reaping is
		// asserted by the slower-running tests below (cancel-mid-flight,
		// request-timeout), which give a forked grandchild a comfortable
		// margin to finish starting before the group is killed — this
		// fast success path does not, and requiring it here would be
		// flaky under -race rather than testing anything this path
		// doesn't already cover via those other tests.
		assertProcessGone(t, pidFile)
	})

	t.Run("success at 9.3 but egress fails: still overall success, warning only, EgressCategory set", func(t *testing.T) {
		clientBin := buildFakeBinary(t, "run_proxy")
		cfg := baseDataPathConfig(t, clientBin)
		cfg.EgressURL = "http://127.0.0.1:1/unreachable" // nothing listens on port 1
		before := countOpenFDs(t)

		out := RunSelfcheck(context.Background(), cfg)
		if out.Category != CategoryNone || out.Err != nil {
			t.Fatalf("out = %+v, want overall success despite egress failure", out)
		}
		if out.EgressOK {
			t.Fatalf("EgressOK = true, want false")
		}
		if out.EgressWarn == "" {
			t.Fatalf("EgressWarn is empty, want a diagnostic message")
		}
		// review finding F9: the overall Category stays None (spec §9 row
		// 4: "只告警,不影响退出码"), but EgressCategory lets a caller see
		// this specific failure by type.
		if out.EgressCategory != CategoryEgress {
			t.Fatalf("EgressCategory = %v, want CategoryEgress", out.EgressCategory)
		}
		assertPortBindable(t, cfg.SelfcheckPort)
		assertNoFDLeak(t, before)
	})

	t.Run("E3 port already taken is CategoryLocalService and reports the occupant", func(t *testing.T) {
		clientBin := buildFakeBinary(t, "run_proxy")
		cfg := baseDataPathConfig(t, clientBin)
		occupier, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", cfg.SelfcheckPort))
		if err != nil {
			t.Fatalf("occupy port: %v", err)
		}
		defer occupier.Close()
		before := countOpenFDs(t)

		out := RunSelfcheck(context.Background(), cfg)
		if out.Category != CategoryLocalService {
			t.Fatalf("Category = %v, want CategoryLocalService", out.Category)
		}
		// review finding F3: this test process itself is listening (via
		// `occupier` above), so a real /proc-based lookup must find it.
		if !out.E3OccupantFound {
			t.Fatalf("E3OccupantFound = false, want true (this test process should be discoverable as the occupant)")
		}
		if out.E3OccupantPID != os.Getpid() {
			t.Fatalf("E3OccupantPID = %d, want %d (this test process)", out.E3OccupantPID, os.Getpid())
		}
		// No client should ever have been started, and no fd should leak,
		// on this path (bind failed before anything else was set up).
		assertNoFDLeak(t, before)
	})

	t.Run("E3 port occupied via 0.0.0.0 (not just 127.0.0.1) also reports the occupant", func(t *testing.T) {
		// review finding R3: a listener bound to the wildcard address
		// 0.0.0.0:<port> also makes 127.0.0.1:<port> unbindable (real OS
		// behavior — SO_REUSEADDR does not permit an overlapping specific-
		// address bind against an active wildcard listener on the same
		// port), so the occupant lookup must check 0.0.0.0 too, not only
		// 127.0.0.1.
		clientBin := buildFakeBinary(t, "run_proxy")
		cfg := baseDataPathConfig(t, clientBin)
		occupier, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", cfg.SelfcheckPort))
		if err != nil {
			t.Fatalf("occupy port via 0.0.0.0: %v", err)
		}
		defer occupier.Close()
		before := countOpenFDs(t)

		out := RunSelfcheck(context.Background(), cfg)
		if out.Category != CategoryLocalService {
			t.Fatalf("Category = %v, want CategoryLocalService", out.Category)
		}
		if !out.E3OccupantFound {
			t.Fatalf("E3OccupantFound = false, want true (this test process, listening on 0.0.0.0, should be discoverable)")
		}
		if out.E3OccupantPID != os.Getpid() {
			t.Fatalf("E3OccupantPID = %d, want %d (this test process)", out.E3OccupantPID, os.Getpid())
		}
		assertNoFDLeak(t, before)
	})

	t.Run("E2 exhausts 3 attempts when the client never starts listening", func(t *testing.T) {
		// check_fail exits immediately without opening any port, on every
		// attempt — forces all 3 E2 retries to fail.
		clientBin := buildFakeBinary(t, "check_fail")
		cfg := baseDataPathConfig(t, clientBin)
		cfg.ClientStartGrace = 100 * time.Millisecond
		before := countOpenFDs(t)

		out := RunSelfcheck(context.Background(), cfg)
		if out.Category != CategoryLocalService {
			t.Fatalf("Category = %v, want CategoryLocalService", out.Category)
		}
		assertPortBindable(t, cfg.SelfcheckPort)
		assertNoFDLeak(t, before)
	})

	t.Run("protocol failure (bad nonce response) is CategoryProtocol with a dest retest attached", func(t *testing.T) {
		// A proxy that always answers 502 regardless of target: the nonce
		// request will fail its body/status check deterministically.
		pidFile := filepath.Join(t.TempDir(), "client.pid")
		brokenBin := buildFakeBinary(t, "run_proxy_broken", helperEnvPIDFile+"="+pidFile)
		cfg := baseDataPathConfig(t, brokenBin)
		destErr := errors.New("fake dest dial failure")
		cfg.DestDialer = func(ctx context.Context, network, addr string, tcfg *tls.Config) (*tls.Conn, error) {
			return nil, destErr
		}
		before := countOpenFDs(t)

		out := RunSelfcheck(context.Background(), cfg)
		if out.Category != CategoryProtocol {
			t.Fatalf("Category = %v, want CategoryProtocol (err=%v)", out.Category, out.Err)
		}
		if out.DestRetestErr == nil {
			t.Fatalf("DestRetestErr is nil, want the dest retest's error to be attached")
		}
		assertPortBindable(t, cfg.SelfcheckPort)
		assertNoFDLeak(t, before)
		assertProcessGone(t, pidFile)
	})

	t.Run("already-cancelled context fails fast without leaking E3, error identifiable via errors.Is", func(t *testing.T) {
		clientBin := buildFakeBinary(t, "run_proxy")
		cfg := baseDataPathConfig(t, clientBin)
		before := countOpenFDs(t)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		start := time.Now()
		out := RunSelfcheck(ctx, cfg)
		if out.Category != CategoryProtocol {
			t.Fatalf("Category = %v, want CategoryProtocol for an already-cancelled context", out.Category)
		}
		// review finding F9: errors.Is must be able to identify the
		// cancellation, not just "some error".
		if !errors.Is(out.Err, context.Canceled) {
			t.Fatalf("out.Err = %v, want it to wrap context.Canceled (errors.Is)", out.Err)
		}
		if time.Since(start) > 2*time.Second {
			t.Fatalf("RunSelfcheck took too long after cancellation: %s", time.Since(start))
		}
		assertPortBindable(t, cfg.SelfcheckPort)
		assertNoFDLeak(t, before)
	})

	t.Run("cancel while the nonce request is in flight is context.Canceled, not DeadlineExceeded, and skips the dest retest", func(t *testing.T) {
		// review finding F9: the client must actually start (so this
		// exercises mid-flight cancellation of the *request*, not of the
		// E2 retry loop) before cancel() fires.
		pidFile := filepath.Join(t.TempDir(), "client.pid")
		childPIDFile := filepath.Join(t.TempDir(), "child.pid")
		hangingBin := buildFakeBinary(t, "run_proxy_hang", helperEnvPIDFile+"="+pidFile, helperEnvChildPID+"="+childPIDFile)
		cfg := baseDataPathConfig(t, hangingBin)
		before := countOpenFDs(t)

		// review finding R1: SIGINT-style cancellation must not leave the
		// dest retest running for up to 10 more seconds against a context
		// that is already dead — it must be skipped outright, and the
		// dialer must never even be invoked.
		var dialCalls int32
		cfg.DestDialer = func(ctx context.Context, network, addr string, tcfg *tls.Config) (*tls.Conn, error) {
			atomic.AddInt32(&dialCalls, 1)
			<-ctx.Done()
			return nil, ctx.Err()
		}

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			// Give the real subprocess time to fork/exec and start
			// listening (cfg.ClientStartGrace is 500ms in
			// baseDataPathConfig, comfortably longer than real subprocess
			// startup) before cancelling mid-request.
			time.Sleep(400 * time.Millisecond)
			cancel()
		}()

		start := time.Now()
		out := RunSelfcheck(ctx, cfg)
		elapsed := time.Since(start)

		if out.Category != CategoryProtocol {
			t.Fatalf("Category = %v, want CategoryProtocol", out.Category)
		}
		if !errors.Is(out.Err, context.Canceled) {
			t.Fatalf("out.Err = %v, want it to wrap context.Canceled, not DeadlineExceeded", out.Err)
		}
		if errors.Is(out.Err, context.DeadlineExceeded) {
			t.Fatalf("out.Err wraps context.DeadlineExceeded; this was a cancellation, not a budget timeout")
		}
		if !out.DestRetestSkipped {
			t.Fatalf("DestRetestSkipped = false, want true (parent ctx had already ended by the time the retest would run)")
		}
		if out.DestRetest != nil || out.DestRetestErr != nil {
			t.Fatalf("DestRetest=%v DestRetestErr=%v, want both nil when the retest is skipped", out.DestRetest, out.DestRetestErr)
		}
		if calls := atomic.LoadInt32(&dialCalls); calls != 0 {
			t.Fatalf("dest dialer was called %d times, want 0 (retest must be skipped, not attempted against an already-dead context)", calls)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("RunSelfcheck took %s; must return promptly rather than running a retest against a dead parent context", elapsed)
		}
		assertPortBindable(t, cfg.SelfcheckPort)
		assertNoFDLeak(t, before)
		assertProcessGone(t, pidFile)
		assertProcessGone(t, childPIDFile)
	})

	t.Run("dest retest runs against the live parent ctx when only the data-path's own budget fires", func(t *testing.T) {
		// review finding R1: a protocol failure caused purely by the
		// data-path phase's OWN 30-second budget elapsing (not by parent
		// ctx ending) must still attempt the retest, using parent ctx —
		// which is still alive — not a substitute detached context.
		var dialCalls int32
		cfg := baseDataPathConfig(t, buildFakeBinary(t, "run_proxy_hang"))
		cfg.DestDialer = func(ctx context.Context, network, addr string, tcfg *tls.Config) (*tls.Conn, error) {
			atomic.AddInt32(&dialCalls, 1)
			return nil, errors.New("fake dest dial (retest path)")
		}
		// Shorten only the data-path phase's own budget; leave every other
		// duration (including the retest's own 10s) at its real value —
		// the fake dialer above returns immediately regardless. 1.5s
		// (rather than a tighter substitute) leaves comfortable headroom
		// for the real E2 subprocess's fork/exec/listen to complete first
		// — found flaky at 100ms under `-count=50` load, where an
		// occasionally-slow subprocess start let dpCtx expire inside
		// startClientWithRetry itself (a real, legitimate CategoryProtocol
		// outcome) before ever reaching the request/retest path this test
		// means to exercise, leaving DestRetestErr nil for a different
		// reason than the one under test.
		cfg.NewTimeout = func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
			if d == dataPathRequestTimeout {
				return context.WithTimeout(parent, 1500*time.Millisecond)
			}
			return context.WithTimeout(parent, d)
		}

		out := RunSelfcheck(context.Background(), cfg)
		if out.Category != CategoryProtocol {
			t.Fatalf("Category = %v, want CategoryProtocol", out.Category)
		}
		if out.DestRetestSkipped {
			t.Fatalf("DestRetestSkipped = true, want false (parent ctx was still alive; only the data-path budget fired)")
		}
		if out.DestRetestErr == nil {
			t.Fatalf("DestRetestErr is nil, want the retest's own error to be attached")
		}
		if calls := atomic.LoadInt32(&dialCalls); calls == 0 {
			t.Fatalf("dest dialer was never called; the retest must run when parent ctx is still alive")
		}
	})

	t.Run("9.3 request timeout tears everything down", func(t *testing.T) {
		// A proxy that starts (so E2 retry succeeds) but never answers —
		// forces fetchExpect's 30s request budget... we override via a
		// short-lived parent ctx instead of waiting the real 30s, proving
		// the teardown path (not the exact spec duration, which is
		// exercised structurally by dataPathRequestTimeout's constant
		// value, not by a real 30s wait in this test).
		pidFile := filepath.Join(t.TempDir(), "client.pid")
		childPIDFile := filepath.Join(t.TempDir(), "child.pid")
		hangingBin := buildFakeBinary(t, "run_proxy_hang", helperEnvPIDFile+"="+pidFile, helperEnvChildPID+"="+childPIDFile)
		cfg := baseDataPathConfig(t, hangingBin)
		before := countOpenFDs(t)

		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		out := RunSelfcheck(ctx, cfg)
		if out.Category != CategoryProtocol {
			t.Fatalf("Category = %v, want CategoryProtocol", out.Category)
		}
		if !errors.Is(out.Err, context.DeadlineExceeded) {
			t.Fatalf("out.Err = %v, want it to wrap context.DeadlineExceeded", out.Err)
		}
		// review finding R1: the parent ctx itself ended here (not just
		// dpCtx's own budget), so the retest must be skipped, not run
		// against the now-dead parent.
		if !out.DestRetestSkipped {
			t.Fatalf("DestRetestSkipped = false, want true (parent ctx had already ended)")
		}
		assertPortBindable(t, cfg.SelfcheckPort)
		assertNoFDLeak(t, before)
		assertProcessGone(t, pidFile)
		assertProcessGone(t, childPIDFile)
	})
}

// --- F1/F4: the local-data-path (30s) and egress (10s) budgets ---

// TestSelfcheckTimeoutBudgets proves (review finding F1) that RunSelfcheck
// requests exactly spec §13.2's 30-second local-data-path and 10-second
// egress budgets, and (review finding F4) that the 30-second budget is
// applied exactly ONCE as a single shared context spanning both E2's
// retry loop and the nonce request — not reset between them.
func TestSelfcheckTimeoutBudgets(t *testing.T) {
	var calls []time.Duration
	spy := func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		calls = append(calls, d)
		return context.WithTimeout(parent, d)
	}

	egress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer egress.Close()

	clientBin := buildFakeBinary(t, "run_proxy")
	cfg := baseDataPathConfig(t, clientBin)
	cfg.EgressURL = egress.URL + "/generate_204"
	cfg.NewTimeout = spy

	out := RunSelfcheck(context.Background(), cfg)
	if out.Category != CategoryNone || out.Err != nil {
		t.Fatalf("out = %+v", out)
	}

	var dataPathCalls, egressCalls int
	for _, d := range calls {
		switch d {
		case dataPathRequestTimeout:
			dataPathCalls++
		case egressRequestTimeout:
			egressCalls++
		default:
			t.Fatalf("unexpected timeout duration requested: %s", d)
		}
	}
	if dataPathCalls != 1 {
		t.Fatalf("data-path 30s budget applied %d times, want exactly 1 (E2 retries and the nonce request must share one context — review finding F4)", dataPathCalls)
	}
	if egressCalls != 1 {
		t.Fatalf("egress 10s budget applied %d times, want exactly 1", egressCalls)
	}
}

// TestSelfcheckDataPathOwnBudgetFires proves the 30-second local-data-path
// budget is what terminates a hung request even when the parent ctx
// carries no deadline of its own (review finding F1's "own timer, not
// parent" requirement).
func TestSelfcheckDataPathOwnBudgetFires(t *testing.T) {
	// recordingShortTimeout's single *time.Duration would be overwritten
	// by the dest-retest's own ProbeDest call (which also goes through
	// cfg.NewTimeout, requesting destProbeTimeout) that follows the
	// data-path failure — so this test records every call instead of just
	// the last one, and asserts dataPathRequestTimeout appears among them.
	var calls []time.Duration
	nt := func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		calls = append(calls, d)
		return context.WithTimeout(parent, 100*time.Millisecond)
	}

	hangingBin := buildFakeBinary(t, "run_proxy_hang")
	cfg := baseDataPathConfig(t, hangingBin)
	cfg.NewTimeout = nt

	start := time.Now()
	out := RunSelfcheck(context.Background(), cfg)
	elapsed := time.Since(start)

	if out.Category != CategoryProtocol {
		t.Fatalf("Category = %v, want CategoryProtocol", out.Category)
	}
	found := false
	for _, d := range calls {
		if d == dataPathRequestTimeout {
			found = true
		}
	}
	if !found {
		t.Fatalf("requested timeouts = %v, want %s (spec §13.2 \"本地数据通路 30 秒\") to appear among them", calls, dataPathRequestTimeout)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("RunSelfcheck took %s; the data-path phase's own budget should have fired, not a real 30s wait", elapsed)
	}
	assertPortBindable(t, cfg.SelfcheckPort)
}

func assertPortBindable(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		l, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			l.Close()
			return
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("port %d still not bindable after RunSelfcheck returned (E3 leak?): %v", port, lastErr)
}
