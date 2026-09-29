package realitynode

// service_crash_test.go: plan Task 7's "crash 子进程测试" (design spec
// §14.2). Every point Install/Reinstall reports through lifecycleHook — a
// discovery run over the same scenario collects them — becomes a real,
// independent OS process (this test binary re-exec'd as TestCrashChild)
// that os.Exit(3)s at exactly that point, simulating a hard crash with no
// deferred cleanup. The parent then reopens the leftover transaction area
// through the production constructor (OpenRecoverer, the one Task 8's
// `recover` reuses) and recovers, asserting:
//
//   - a crash at any point before the durable phase=committed flip
//     converges to the pre-transaction tree (bytes, mode, owner,
//     existence), the manifest included — i.e. no new manifest is ever
//     left committed early;
//   - a crash at/after the flip converges forward to a valid installed
//     state and, for the version-window scenario, the superseded version
//     is cleaned up;
//   - the transaction area and tombstone are gone in every case.
//
// Primitive-internal crash points (mid-rename, mid-fsync, ...) stay with
// internal/transaction's own crash matrix (Task 4/5); this file adds the
// lifecycle-step granularity that only the instance orchestration knows.
import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hlpclg/singbox-sub-manager/internal/transaction"
)

const (
	envCrashCase = "RTNODE_CRASH_CASE"
	envCrashBase = "RTNODE_CRASH_BASE"
	envCrashPt   = "RTNODE_CRASH_POINT"
	envCrashPort = "RTNODE_CRASH_LISTEN_PORT"
	envCrashSelf = "RTNODE_CRASH_SELFCHECK_PORT"
	envCrashSysd = "RTNODE_CRASH_SYSD_STATE"

	crashExitCode = 3
	doneExitCode  = 7
)

func rootsUnder(base string) Roots {
	etc := filepath.Join(base, "etc")
	varLib := filepath.Join(base, "var-lib")
	usrLib := filepath.Join(base, "usr-local-lib")
	systemdDir := filepath.Join(base, "systemd")
	return Roots{
		SystemdUnit:   filepath.Join(systemdDir, "proxyctl-reality.service"),
		ConfigDir:     filepath.Join(etc, "proxyctl-reality"),
		Config:        filepath.Join(etc, "proxyctl-reality", "config.json"),
		StateDir:      filepath.Join(varLib, "proxyctl-reality"),
		Secrets:       filepath.Join(varLib, "proxyctl-reality", "secrets.json"),
		Instance:      filepath.Join(varLib, "proxyctl-reality", "instance.json"),
		Manifest:      filepath.Join(varLib, "proxyctl-reality", "manifest.json"),
		BinDir:        filepath.Join(usrLib, "proxyctl-reality"),
		TxnDir:        filepath.Join(varLib, "proxyctl-reality-txn"),
		TombstoneGlob: filepath.Join(varLib, ".proxyctl-reality-txn.tomb-*"),
	}
}

func makeRootDirs(t *testing.T, r Roots) {
	t.Helper()
	for _, d := range []string{filepath.Dir(r.ConfigDir), filepath.Dir(r.StateDir), filepath.Dir(r.BinDir), filepath.Dir(r.SystemdUnit)} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
}

type crashPorts struct{ listen, selfcheck int }

var crashPortCounter atomic.Uint64

// crashPortsFor picks fixed-range ports that cannot collide with the
// kernel's ephemeral range (32768+), which the concurrent crash children's
// own temporary clients and E2 ports are allocated from: freeTCPPort-style
// "bind :0 and release" ports can be handed to another process before the
// child that was promised them binds. selfcheck ports come from 10000-19999
// (spec §8.1's own domain), listen ports from 20000-29999.
func crashPortsFor(t *testing.T) crashPorts {
	t.Helper()
	pick := func(lo, hi int) int {
		for i := 0; i < 2000; i++ {
			n := int((crashPortCounter.Add(1)*7919 + uint64(os.Getpid())*104729) % uint64(hi-lo))
			p := lo + n
			l, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", p))
			if err != nil {
				continue
			}
			l.Close()
			l, err = net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", p))
			if err != nil {
				continue
			}
			l.Close()
			return p
		}
		t.Fatalf("no free port in %d-%d", lo, hi)
		return 0
	}
	return crashPorts{selfcheck: pick(10000, 20000), listen: pick(20000, 30000)}
}

// crashScenario describes one lifecycle operation to crash.
type crashScenario struct {
	name string
	// setup builds the pre-transaction state (in the parent, before the
	// child is spawned).
	setup func(t *testing.T, roots Roots, ws workingSingbox, ports crashPorts)
	// run performs the operation under test (in the discovery run and in
	// the child).
	run func(t *testing.T, env *Env, roots Roots, ports crashPorts) Result
	// active reports whether the unit is active before the operation.
	active bool
	// recoverExe is the binary path the rollback gate will expect (the
	// pre-transaction version's).
	recoverExe func(roots Roots) string
	// committedPoints are the hook points after which the durable
	// phase=committed flip has happened.
	committedPoints map[string]bool
	// cleanup is set when the scenario removes a superseded version (the
	// lifecycle then reports a cleanup-registered point).
	cleanup bool
	// afterCommit asserts the state a committed recovery must leave.
	afterCommit func(t *testing.T, env *Env, roots Roots)
}

func crashScenarios() []crashScenario {
	afterFlip := map[string]bool{"after-commit-flip": true}
	return []crashScenario{
		{
			name:  "install",
			setup: func(*testing.T, Roots, workingSingbox, crashPorts) {},
			run: func(t *testing.T, env *Env, roots Roots, p crashPorts) Result {
				return Install(context.Background(), env, InstallParams{Name: "jp1", Server: "203.0.113.10", Port: p.listen, SNI: "example.com"})
			},
			recoverExe:      func(r Roots) string { return versionBinaryPath(r, "1.14.1") },
			committedPoints: afterFlip,
		},
		{
			name: "reinstall_installed",
			setup: func(t *testing.T, roots Roots, ws workingSingbox, p crashPorts) {
				writeInstalledFixtureOpts(t, roots, fixtureOpts{binContent: readBytes(t, ws.path), listenPort: p.listen, selfcheckPort: p.selfcheck})
			},
			run: func(t *testing.T, env *Env, roots Roots, p crashPorts) Result {
				return Reinstall(context.Background(), env, ReinstallParams{})
			},
			active:          true,
			recoverExe:      func(r Roots) string { return versionBinaryPath(r, "1.14.1") },
			committedPoints: afterFlip,
		},
		{
			name: "reinstall_uninstalled",
			setup: func(t *testing.T, roots Roots, ws workingSingbox, p crashPorts) {
				writeUninstalledFixtureOpts(t, roots, fixtureOpts{listenPort: p.listen, selfcheckPort: p.selfcheck})
			},
			run: func(t *testing.T, env *Env, roots Roots, p crashPorts) Result {
				return Reinstall(context.Background(), env, ReinstallParams{})
			},
			recoverExe:      func(r Roots) string { return versionBinaryPath(r, "1.14.1") },
			committedPoints: afterFlip,
		},
		{
			name: "reinstall_window",
			setup: func(t *testing.T, roots Roots, ws workingSingbox, p crashPorts) {
				// Window v2 (current) + v1 (previous); the crashing
				// operation moves to v3, so v1 falls out of the window.
				writeInstalledFixtureOpts(t, roots, fixtureOpts{
					binContent: readBytes(t, ws.path), version: "1.14.2", prevVersion: "1.14.1",
					listenPort: p.listen, selfcheckPort: p.selfcheck,
				})
			},
			run: func(t *testing.T, env *Env, roots Roots, p crashPorts) Result {
				return reinstallTo(t, context.Background(), env, roots, "1.14.3")
			},
			active:          true,
			recoverExe:      func(r Roots) string { return versionBinaryPath(r, "1.14.2") },
			committedPoints: afterFlip,
			cleanup:         true,
			afterCommit: func(t *testing.T, env *Env, roots Roots) {
				assertNotExist(t, filepath.Join(roots.BinDir, "versions", "1.14.1"))
				assertVersionEntries(t, env, roots, "1.14.3", "1.14.2", "1.14.1")
			},
		},
		{
			// current v2 + previous v1, plain reinstall of the current
			// version: v1 must survive every crash point and the recovery.
			name: "reinstall_same_version_window",
			setup: func(t *testing.T, roots Roots, ws workingSingbox, p crashPorts) {
				writeInstalledFixtureOpts(t, roots, fixtureOpts{
					binContent: readBytes(t, ws.path), version: "1.14.2", prevVersion: "1.14.1",
					listenPort: p.listen, selfcheckPort: p.selfcheck,
				})
			},
			run: func(t *testing.T, env *Env, roots Roots, p crashPorts) Result {
				ws2 := buildWorkingFakeSingbox(t, "1.14.2", false)
				useRelease(t, env, roots, "1.14.2", ws2.path)
				return Reinstall(context.Background(), env, ReinstallParams{})
			},
			active:          true,
			recoverExe:      func(r Roots) string { return versionBinaryPath(r, "1.14.2") },
			committedPoints: afterFlip,
			afterCommit: func(t *testing.T, env *Env, roots Roots) {
				assertVersionEntries(t, env, roots, "1.14.2", "1.14.1")
			},
		},
	}
}

// runScenarioEnv builds the scenario's Env. sysdState is the path of the
// JSON file mirroring the fake unit's state, shared by parent and child so
// the recovering parent sees the systemd state the crashed child left
// behind, exactly as real systemd would persist it.
func runScenarioEnv(t *testing.T, sc crashScenario, roots Roots, sysdState string) (*Env, workingSingbox) {
	t.Helper()
	ws := buildWorkingFakeSingbox(t, "1.14.1", false)
	env, sysd := testEnvAt(t, roots, ws.path, "1.14.1")
	env.DataPath.ClientStartGrace = 15 * time.Second
	if sc.active {
		sysd.loaded, sysd.unitFileState, sysd.activeState = true, "enabled", "active"
	}
	if os.Getenv(envCrashCase) == "" {
		sysd.persistTo(sysdState) // the parent seeds the shared file
	} else {
		sysd.persistPath = sysdState // the child adopts what the parent seeded
	}
	return env, ws
}

// TestCrashChild is the re-exec'd crashing side; a no-op in a normal run.
func TestCrashChild(t *testing.T) {
	name := os.Getenv(envCrashCase)
	if name == "" {
		return
	}
	var sc crashScenario
	for _, s := range crashScenarios() {
		if s.name == name {
			sc = s
		}
	}
	roots := rootsUnder(os.Getenv(envCrashBase))
	point := os.Getenv(envCrashPt)
	ports := crashPorts{}
	ports.listen, _ = strconv.Atoi(os.Getenv(envCrashPort))
	ports.selfcheck, _ = strconv.Atoi(os.Getenv(envCrashSelf))

	env, _ := runScenarioEnv(t, sc, roots, os.Getenv(envCrashSysd))
	lifecycleHook = func(p string) error {
		if p == point {
			os.Exit(crashExitCode)
		}
		return nil
	}
	res := sc.run(t, env, roots, ports)
	fmt.Fprintf(os.Stderr, "crash child finished without reaching %q: result=%+v cause=%v\n", point, res, res.Cause)
	os.Exit(doneExitCode) // the point was never reached
}

// discoverPoints runs the scenario once to completion, in-process, and
// returns every lifecycle point it passed (in order, deduplicated).
func discoverPoints(t *testing.T, sc crashScenario) []string {
	t.Helper()
	base := t.TempDir()
	roots := rootsUnder(base)
	makeRootDirs(t, roots)
	ports := crashPortsFor(t)
	env, ws := runScenarioEnv(t, sc, roots, filepath.Join(t.TempDir(), "sysd.json"))
	sc.setup(t, roots, ws, ports)

	var points []string
	seen := map[string]bool{}
	orig := lifecycleHook
	lifecycleHook = func(p string) error {
		if !seen[p] {
			seen[p] = true
			points = append(points, p)
		}
		return nil
	}
	res := sc.run(t, env, roots, ports)
	lifecycleHook = orig
	assertCommitted(t, res)
	return points
}

func TestServiceCrashRecovery(t *testing.T) {
	if os.Getenv(envCrashCase) != "" {
		t.Skip("running as the crash child")
	}
	for _, sc := range crashScenarios() {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			points := discoverPoints(t, sc)
			// The lifecycle must expose a crash point around every kind of
			// step; a refactor that silently dropped one would weaken the
			// matrix, so the essential ones are pinned.
			must := []string{"began", "downloaded", "checked", "gate-passed", "selfcheck-passed", "before-commit", "after-commit-flip"}
			if sc.name == "install" {
				must = append(must, "reg:mkdir-etc", "done:mkdir-etc", "reg:write-config", "done:write-config", "reg:write-secrets", "done:write-secrets",
					"reg:write-instance", "done:write-instance", "reg:mkdir-version", "done:write-binary", "reg:write-unit", "done:write-unit",
					"reg:systemd-daemon-reload", "started:systemd-enable", "called:systemd-enable", "done:systemd-enable", "started:systemd-start",
					"called:systemd-start", "reg:write-manifest", "done:write-manifest")
			} else {
				must = append(must, "reg:manifest-snap", "done:manifest-snap", "reg:manifest-write", "done:manifest-write",
					"started:systemd-restart", "called:systemd-restart", "mkdir-snap")
			}
			if sc.cleanup {
				must = append(must, "cleanup-registered")
			}
			have := map[string]bool{}
			for _, p := range points {
				have[p] = true
			}
			for _, m := range must {
				if !have[m] {
					t.Errorf("lifecycle never reported crash point %q (got %v)", m, points)
				}
			}
			t.Logf("%s: %d crash points", sc.name, len(points))

			for _, point := range points {
				point := point
				t.Run(strings.NewReplacer(":", "_", "/", "_").Replace(point), func(t *testing.T) {
					t.Parallel()
					crashAndRecover(t, sc, point)
				})
			}
		})
	}
}

// crashSlots bounds how many crash children run at once: each starts a
// full test binary and, late in the lifecycle, a real selfcheck, and an
// unbounded fan-out under -race starves the selfcheck's own timeouts.
var crashSlots = make(chan struct{}, 4)

func crashAndRecover(t *testing.T, sc crashScenario, point string) {
	crashSlots <- struct{}{}
	defer func() { <-crashSlots }()
	base := t.TempDir()
	roots := rootsUnder(base)
	makeRootDirs(t, roots)
	ports := crashPortsFor(t)
	sysdState := filepath.Join(t.TempDir(), "sysd.json")
	env, ws := runScenarioEnv(t, sc, roots, sysdState)
	sc.setup(t, roots, ws, ports)
	stateBefore := DetectInstanceState(env.FS, roots).Status
	before := treeSnapshot(t, roots)
	manifestBefore, hadManifest := []byte(nil), false
	if b, err := os.ReadFile(roots.Manifest); err == nil {
		manifestBefore, hadManifest = b, true
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashChild$", "-test.timeout=5m")
	cmd.Env = append(os.Environ(),
		envCrashCase+"="+sc.name, envCrashBase+"="+base, envCrashPt+"="+point,
		envCrashPort+"="+strconv.Itoa(ports.listen), envCrashSelf+"="+strconv.Itoa(ports.selfcheck),
		envCrashSysd+"="+sysdState)
	out, err := cmd.CombinedOutput()
	code := -1
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err == nil {
		code = 0
	}
	if code != crashExitCode {
		t.Fatalf("child exit code = %d, want %d (crash at %q)\n%s", code, crashExitCode, point, out)
	}

	// --- recover in the parent through the production constructor ---
	env.Proc.(*fakeProc).exePath = sc.recoverExe(roots)
	outcome, recErr := recoverLeftover(t, env, roots)
	if recErr != nil {
		t.Fatalf("recover after crash at %q: %v", point, recErr)
	}
	assertNoTxnArea(t, roots)

	if sc.committedPoints[point] {
		if outcome != transaction.OutcomeCommitted {
			t.Fatalf("crash at %q (after the commit flip): outcome = %v, want committed", point, outcome)
		}
		assertInstalledState(t, env)
		if _, err := os.ReadFile(roots.Manifest); err != nil {
			t.Errorf("after a committed recovery the new manifest must be in place: %v", err)
		}
		if sc.afterCommit != nil {
			sc.afterCommit(t, env, roots)
		}
		return
	}

	if outcome != transaction.OutcomeNoChange && outcome != transaction.OutcomeRolledBack {
		t.Fatalf("crash at %q (before the commit flip): outcome = %v, want no_change or rolled_back", point, outcome)
	}
	assertTreeEqual(t, before, treeSnapshot(t, roots))
	// no new manifest was committed early
	if hadManifest {
		if b, err := os.ReadFile(roots.Manifest); err != nil || string(b) != string(manifestBefore) {
			t.Errorf("manifest changed by a transaction that never committed (err=%v)", err)
		}
	} else {
		assertNotExist(t, roots.Manifest)
	}
	if st := DetectInstanceState(env.FS, roots).Status; st != stateBefore {
		t.Errorf("instance state after recovery = %v, want the pre-transaction %v", st, stateBefore)
	}
}

// recoverLeftover drives the leftover transaction area to completion the
// way Task 8's `recover` will: an area without journal.json is discarded
// (§7.6), otherwise the shared Recoverer resumes it.
func recoverLeftover(t *testing.T, env *Env, roots Roots) (transaction.Outcome, error) {
	t.Helper()
	if _, err := os.Stat(roots.TxnDir); err != nil {
		return transaction.OutcomeNoChange, nil // crashed before the area existed
	}
	if _, err := os.Stat(filepath.Join(roots.TxnDir, "journal.json")); err != nil {
		or, err := openAllRoots(roots, env.TrustedOwnerUID)
		if err != nil {
			return 0, err
		}
		defer or.closeAll()
		if err := transaction.RecoverEmptyTransactionArea(or.varLib, txnDirName(roots)); err != nil {
			return 0, err
		}
		return transaction.OutcomeNoChange, nil
	}
	rec, closeFn, err := OpenRecoverer(env)
	if err != nil {
		return 0, err
	}
	defer closeFn()
	outcome, err := rec.RecoverDetached(context.Background())
	if err != nil {
		return 0, fmt.Errorf("RecoverDetached: %w", err)
	}
	return outcome, nil
}
