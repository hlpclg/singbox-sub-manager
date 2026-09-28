package realitynode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- fake CommandRunner recording every call, for Systemctl unit tests ---

type recordedCall struct {
	timeout time.Duration
	name    string
	args    []string
}

type fakeRunner struct {
	calls   []recordedCall
	results []CommandResult // consumed in order, one per call; last one repeats if exhausted
}

func (f *fakeRunner) Run(ctx context.Context, timeout time.Duration, name string, args ...string) CommandResult {
	f.calls = append(f.calls, recordedCall{timeout: timeout, name: name, args: append([]string(nil), args...)})
	idx := len(f.calls) - 1
	if idx < len(f.results) {
		return f.results[idx]
	}
	if len(f.results) == 0 {
		return CommandResult{}
	}
	return f.results[len(f.results)-1]
}

func TestSystemctl(t *testing.T) {
	t.Run("LoadState parses loaded/not-loaded and uses 15s timeout", func(t *testing.T) {
		r := &fakeRunner{results: []CommandResult{{Stdout: "loaded\n"}}}
		s := Systemctl{Runner: r}
		loaded, err := s.LoadState(context.Background(), "proxyctl-reality.service")
		if err != nil {
			t.Fatalf("LoadState: %v", err)
		}
		if !loaded {
			t.Fatalf("loaded = false, want true")
		}
		if len(r.calls) != 1 {
			t.Fatalf("calls = %d, want 1", len(r.calls))
		}
		c := r.calls[0]
		if c.timeout != systemctlTimeout {
			t.Fatalf("timeout = %s, want %s", c.timeout, systemctlTimeout)
		}
		if c.name != "systemctl" || strings.Join(c.args, " ") != "show -p LoadState --value proxyctl-reality.service" {
			t.Fatalf("call = %+v", c)
		}
	})

	t.Run("UnitFileState trims output", func(t *testing.T) {
		r := &fakeRunner{results: []CommandResult{{Stdout: "  enabled\n"}}}
		s := Systemctl{Runner: r}
		v, err := s.UnitFileState(context.Background(), "u")
		if err != nil || v != "enabled" {
			t.Fatalf("UnitFileState = %q, err=%v", v, err)
		}
	})

	t.Run("Stop/Restart/Start/DaemonReload issue correct verbs", func(t *testing.T) {
		r := &fakeRunner{results: []CommandResult{{}, {}, {}, {}}}
		s := Systemctl{Runner: r}
		if err := s.Stop(context.Background(), "u"); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if err := s.Restart(context.Background(), "u"); err != nil {
			t.Fatalf("Restart: %v", err)
		}
		if err := s.Start(context.Background(), "u"); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if err := s.DaemonReload(context.Background()); err != nil {
			t.Fatalf("DaemonReload: %v", err)
		}
		want := []string{"stop u", "restart u", "start u", "daemon-reload"}
		for i, w := range want {
			if strings.Join(r.calls[i].args, " ") != w {
				t.Fatalf("call %d = %v, want %q", i, r.calls[i].args, w)
			}
		}
	})

	t.Run("SetUnitFileState enable/disable", func(t *testing.T) {
		r := &fakeRunner{results: []CommandResult{{}, {}}}
		s := Systemctl{Runner: r}
		if err := s.SetUnitFileState(context.Background(), "u", "enabled"); err != nil {
			t.Fatalf("enable: %v", err)
		}
		if err := s.SetUnitFileState(context.Background(), "u", "disabled"); err != nil {
			t.Fatalf("disable: %v", err)
		}
		if strings.Join(r.calls[0].args, " ") != "enable u" || strings.Join(r.calls[1].args, " ") != "disable u" {
			t.Fatalf("calls = %+v", r.calls)
		}
		if err := s.SetUnitFileState(context.Background(), "u", "bogus"); err == nil {
			t.Fatalf("expected error for unknown want value")
		}
	})

	t.Run("MainPID parses integer, propagates non-integer as error", func(t *testing.T) {
		r := &fakeRunner{results: []CommandResult{{Stdout: "1234\n"}}}
		s := Systemctl{Runner: r}
		pid, err := s.MainPID(context.Background(), "u")
		if err != nil || pid != 1234 {
			t.Fatalf("pid=%d err=%v", pid, err)
		}

		r2 := &fakeRunner{results: []CommandResult{{Stdout: "not-a-number\n"}}}
		s2 := Systemctl{Runner: r2}
		if _, err := s2.MainPID(context.Background(), "u"); err == nil {
			t.Fatalf("expected error for non-integer MainPID")
		}
	})

	t.Run("nonzero exit code reported as error", func(t *testing.T) {
		r := &fakeRunner{results: []CommandResult{{ExitCode: 1, Stderr: "boom"}}}
		s := Systemctl{Runner: r}
		if _, err := s.ActiveState(context.Background(), "u"); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("err = %v, want it to mention stderr", err)
		}
	})
}

// --- F1: systemctl's own 15-second budget, exercised with a real
// subprocess reachable under the literal name "systemctl" (via
// buildFakeSystemctl's PATH override) ---

// TestSystemctlStopBudget covers the plan's explicit review-focus
// scenario: "假 systemctl stop 推进 10 秒仍落在 15 秒预算，超时终止/回收".
// TestSystemctlStopBudget covers the plan's explicit review-focus scenario
// ("假 systemctl stop 推进 10 秒仍落在 15 秒预算，超时终止/回收") using a
// fake-clock-driven TimeoutFunc (fakeSystemctlClock, review finding R2):
// the derived context's cancellation is driven entirely by Advance calls
// on virtual time, never by a real timer, so both sub-tests below run in
// well under a second of real wall-clock time despite exercising "10
// virtual seconds" and "15+ virtual seconds" respectively — combined with
// a real, genuinely-hanging-until-told-otherwise subprocess so the
// success/kill+reap outcomes are real, not simulated.
func TestSystemctlStopBudget(t *testing.T) {
	t.Run("A: advancing 10 virtual seconds does not end the budget; a real subprocess then exits 0 and Stop succeeds", func(t *testing.T) {
		signalFile := filepath.Join(t.TempDir(), "exit-now")
		buildFakeSystemctl(t, "hang_until_signal", helperEnvExtra1+"="+signalFile)
		clock := newFakeSystemctlClock()
		s := Systemctl{Runner: ExecRunner{NewTimeout: clock.NewTimeout}}

		errCh := make(chan error, 1)
		go func() { errCh <- s.Stop(context.Background(), "proxyctl-reality.service") }()

		<-clock.Registered // ExecRunner has asked for its timeout context
		clock.Advance(10 * time.Second)

		// Stop must NOT have returned yet: 10 virtual seconds is within
		// the 15-second budget, and the fake process is still polling for
		// the signal file.
		select {
		case err := <-errCh:
			t.Fatalf("Stop returned early (err=%v) after only 10 virtual seconds; it should still be waiting", err)
		case <-time.After(200 * time.Millisecond):
		}

		if err := os.WriteFile(signalFile, []byte("go"), 0o600); err != nil {
			t.Fatalf("write signal file: %v", err)
		}

		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("Stop: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("Stop did not return after the fake process was signaled to exit")
		}

		if clock.LastRequested != systemctlTimeout {
			t.Fatalf("requested timeout = %s, want %s (spec §13.2 \"每次 systemctl 调用 15 秒\")", clock.LastRequested, systemctlTimeout)
		}
	})

	t.Run("B: advancing to 15 virtual seconds ends the budget; a real hanging subprocess (and its group) is killed and reaped", func(t *testing.T) {
		pidFile := filepath.Join(t.TempDir(), "systemctl.pid")
		buildFakeSystemctl(t, "hang", helperEnvPIDFile+"="+pidFile)
		clock := newFakeSystemctlClock()
		s := Systemctl{Runner: ExecRunner{NewTimeout: clock.NewTimeout}}

		errCh := make(chan error, 1)
		go func() { errCh <- s.Stop(context.Background(), "proxyctl-reality.service") }()

		<-clock.Registered
		// Make sure the real subprocess has actually started (and written
		// its pidfile) before ending its budget: otherwise a fast enough
		// Advance could kill it before its fresh fork/exec even reaches
		// that point, leaving assertProcessGone below with no pid to
		// check at all (a fork/exec race, not the thing this test means
		// to prove).
		waitForPIDFileReady(t, pidFile, 2*time.Second)
		clock.Advance(15 * time.Second)

		var stopErr error
		select {
		case stopErr = <-errCh:
		case <-time.After(5 * time.Second):
			t.Fatalf("Stop did not return after the virtual budget was exceeded")
		}
		if stopErr == nil {
			t.Fatalf("expected a timeout error")
		}
		if clock.LastRequested != systemctlTimeout {
			t.Fatalf("requested timeout = %s, want %s (spec §13.2 \"每次 systemctl 调用 15 秒\")", clock.LastRequested, systemctlTimeout)
		}
		// The real subprocess (which only ever `select{}`s, never exits on
		// its own) and its process group must actually be gone.
		assertProcessGone(t, pidFile)
	})
}

// --- BuildGate (spec §9.2) ---

type fakeGateSystemd struct {
	activeStateSeq []string // one per call; last repeats
	activeCalls    int
	mainPID        int
	mainPIDErr     error
}

func (f *fakeGateSystemd) ActiveState(ctx context.Context, unit string) (string, error) {
	i := f.activeCalls
	f.activeCalls++
	if i >= len(f.activeStateSeq) {
		i = len(f.activeStateSeq) - 1
	}
	return f.activeStateSeq[i], nil
}

func (f *fakeGateSystemd) MainPID(ctx context.Context, unit string) (int, error) {
	return f.mainPID, f.mainPIDErr
}

type fakeProc struct {
	inode    uint64
	inodeOK  bool
	inodeErr error
	ownerPID int // PIDHasSocketInode returns true only for this pid
	exePath  string
	exeErr   error
}

func (f *fakeProc) TCPListenInode(port int) (uint64, bool, error) {
	return f.inode, f.inodeOK, f.inodeErr
}
func (f *fakeProc) PIDHasSocketInode(pid int, inode uint64) (bool, error) {
	return pid == f.ownerPID && inode == f.inode, nil
}
func (f *fakeProc) Exe(pid int) (string, error) { return f.exePath, f.exeErr }

// fakeClock advances virtual time instantly on Sleep, so BuildGate's poll
// loop never actually waits in tests (AGENTS.md: "假时钟验证时间边界，禁止
// 真实等待10/30/120秒充当单元测试").
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time        { return c.now }
func (c *fakeClock) Sleep(d time.Duration) { c.now = c.now.Add(d) }

func TestBuildGate(t *testing.T) {
	const unit = "proxyctl-reality.service"
	const listenPort = 443
	const binPath = "/usr/local/lib/proxyctl-reality/versions/1.14.1/sing-box"

	newHealthyProc := func() *fakeProc {
		return &fakeProc{inode: 42, inodeOK: true, ownerPID: 100, exePath: binPath}
	}

	t.Run("success: all four conditions hold, then check runs once", func(t *testing.T) {
		checkBin := buildFakeBinary(t, "check_ok")
		proc := newHealthyProc()
		proc.exePath = checkBin // MainPID's /proc/<pid>/exe must equal BinaryPath()
		gate := BuildGate(GateConfig{
			Systemd:    &fakeGateSystemd{activeStateSeq: []string{"active"}, mainPID: 100},
			Unit:       unit,
			ListenPort: listenPort,
			ConfigPath: "/etc/proxyctl-reality/config.json",
			BinaryPath: func() string { return checkBin },
			Proc:       proc,
			Runner:     ExecRunner{},
			Clock:      &fakeClock{},
		})
		if err := gate(context.Background()); err != nil {
			t.Fatalf("gate: %v", err)
		}
	})

	t.Run("polls past initial failures without falsely succeeding, then succeeds", func(t *testing.T) {
		checkBin := buildFakeBinary(t, "check_ok")
		proc := newHealthyProc()
		proc.exePath = checkBin
		clock := &fakeClock{}
		attempts := 0
		systemd := &fakeGateSystemd{activeStateSeq: []string{"activating", "activating", "active"}, mainPID: 100}
		gate := BuildGate(GateConfig{
			Systemd:    systemd,
			Unit:       unit,
			ListenPort: listenPort,
			ConfigPath: "/etc/proxyctl-reality/config.json",
			BinaryPath: func() string { return checkBin },
			Proc:       proc,
			Runner:     ExecRunner{},
			Clock:      clock,
		})
		if err := gate(context.Background()); err != nil {
			t.Fatalf("gate: %v", err)
		}
		attempts = systemd.activeCalls
		if attempts < 3 {
			t.Fatalf("activeCalls = %d, want at least 3 (proves the loop actually retried, not a premature success)", attempts)
		}
	})

	t.Run("times out (fake clock, no real sleep) when ActiveState never becomes active", func(t *testing.T) {
		clock := &fakeClock{}
		gate := BuildGate(GateConfig{
			Systemd:    &fakeGateSystemd{activeStateSeq: []string{"activating"}, mainPID: 100},
			Unit:       unit,
			ListenPort: listenPort,
			ConfigPath: "/etc/proxyctl-reality/config.json",
			BinaryPath: func() string { return "/unused" },
			Proc:       newHealthyProc(),
			Runner:     ExecRunner{},
			Clock:      clock,
		})
		start := time.Now()
		err := gate(context.Background())
		elapsed := time.Since(start)
		if err == nil {
			t.Fatalf("expected timeout error")
		}
		if elapsed > time.Second {
			t.Fatalf("gate took %s of real wall-clock time; the 10s poll budget must be driven by the fake clock, not real sleep", elapsed)
		}
		if clock.now.Sub(time.Time{}) < gatePollBudget {
			t.Fatalf("fake clock only advanced %s, want at least %s (the poll budget)", clock.now.Sub(time.Time{}), gatePollBudget)
		}
	})

	t.Run("wrong MainPID owning the listening socket fails the gate", func(t *testing.T) {
		proc := newHealthyProc()
		proc.ownerPID = 999 // does not match MainPID 100
		gate := BuildGate(GateConfig{
			Systemd:    &fakeGateSystemd{activeStateSeq: []string{"active"}, mainPID: 100},
			Unit:       unit,
			ListenPort: listenPort,
			ConfigPath: "/etc/proxyctl-reality/config.json",
			BinaryPath: func() string { return "/unused" },
			Proc:       proc,
			Runner:     ExecRunner{},
			Clock:      &fakeClock{},
		})
		if err := gate(context.Background()); err == nil {
			t.Fatalf("expected error for mismatched socket owner")
		}
	})

	t.Run("wrong exe path fails the gate", func(t *testing.T) {
		proc := newHealthyProc()
		proc.exePath = "/some/other/binary"
		gate := BuildGate(GateConfig{
			Systemd:    &fakeGateSystemd{activeStateSeq: []string{"active"}, mainPID: 100},
			Unit:       unit,
			ListenPort: listenPort,
			ConfigPath: "/etc/proxyctl-reality/config.json",
			BinaryPath: func() string { return binPath },
			Proc:       proc,
			Runner:     ExecRunner{},
			Clock:      &fakeClock{},
		})
		if err := gate(context.Background()); err == nil {
			t.Fatalf("expected error for mismatched exe path")
		}
	})

	t.Run("only IPv6 listening (no IPv4 listener) fails the gate", func(t *testing.T) {
		proc := newHealthyProc()
		proc.inodeOK = false // TCPListenInode reports no IPv4 listener found
		gate := BuildGate(GateConfig{
			Systemd:    &fakeGateSystemd{activeStateSeq: []string{"active"}, mainPID: 100},
			Unit:       unit,
			ListenPort: listenPort,
			ConfigPath: "/etc/proxyctl-reality/config.json",
			BinaryPath: func() string { return binPath },
			Proc:       proc,
			Runner:     ExecRunner{},
			Clock:      &fakeClock{},
		})
		if err := gate(context.Background()); err == nil {
			t.Fatalf("expected error when there is no IPv4 listener")
		}
	})

	t.Run("sing-box check failure after healthy poll fails the gate", func(t *testing.T) {
		checkBin := buildFakeBinary(t, "check_fail")
		proc := newHealthyProc()
		proc.exePath = checkBin
		gate := BuildGate(GateConfig{
			Systemd:    &fakeGateSystemd{activeStateSeq: []string{"active"}, mainPID: 100},
			Unit:       unit,
			ListenPort: listenPort,
			ConfigPath: "/etc/proxyctl-reality/config.json",
			BinaryPath: func() string { return checkBin },
			Proc:       proc,
			Runner:     ExecRunner{},
			Clock:      &fakeClock{},
		})
		if err := gate(context.Background()); err == nil {
			t.Fatalf("expected error when sing-box check fails")
		}
	})

	t.Run("ctx cancellation stops the poll promptly", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		gate := BuildGate(GateConfig{
			Systemd:    &fakeGateSystemd{activeStateSeq: []string{"activating"}, mainPID: 100},
			Unit:       unit,
			ListenPort: listenPort,
			ConfigPath: "/etc/proxyctl-reality/config.json",
			BinaryPath: func() string { return "/unused" },
			Proc:       newHealthyProc(),
			Runner:     ExecRunner{},
			Clock:      &fakeClock{},
		})
		if err := gate(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})

	// review finding F2: each attempt's systemd calls must be bounded by
	// however much of the poll budget remains, not by their own full
	// (potentially much longer) individual timeout in isolation. A
	// systemd call that hangs until its ctx ends must not, by itself,
	// blow through the gate's own aggregate budget.
	t.Run("bounds each attempt by the remaining poll budget when systemd hangs", func(t *testing.T) {
		blocked := &fakeBlockingSystemd{}
		gate := BuildGate(GateConfig{
			Systemd:      blocked,
			Unit:         unit,
			ListenPort:   listenPort,
			ConfigPath:   "/etc/proxyctl-reality/config.json",
			BinaryPath:   func() string { return "/unused" },
			Proc:         newHealthyProc(),
			Runner:       ExecRunner{},
			PollBudget:   200 * time.Millisecond,
			PollInterval: 20 * time.Millisecond,
		})
		start := time.Now()
		err := gate(context.Background())
		elapsed := time.Since(start)
		if err == nil {
			t.Fatalf("expected an error")
		}
		if elapsed > 2*time.Second {
			t.Fatalf("gate took %s; a hanging systemd call must not be allowed to exceed the aggregate poll budget (review finding F2)", elapsed)
		}
		if blocked.calls == 0 {
			t.Fatalf("fake systemd was never called")
		}
	})
}

// fakeBlockingSystemd's ActiveState blocks until ctx ends, simulating a
// hung systemctl call, then reports ctx's own error — used only by the
// "bounds each attempt by the remaining poll budget" test above.
type fakeBlockingSystemd struct {
	calls int
}

func (f *fakeBlockingSystemd) ActiveState(ctx context.Context, unit string) (string, error) {
	f.calls++
	<-ctx.Done()
	return "", ctx.Err()
}

func (f *fakeBlockingSystemd) MainPID(ctx context.Context, unit string) (int, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}
