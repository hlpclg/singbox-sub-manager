package realitynode

// service_recover_test.go: plan Task 8's `recover [--abandon]` (design spec
// §7.4-§7.6, §8.6, §13, §14.1-§14.2). TestServiceRecoverMatrix is the plan's
// named test. Leftovers are produced the way they occur in production: a real
// child process os.Exit()s in the middle of an uninstall/purge/reinstall (the
// lifecycleHook seam of service_crash_test.go), or — for the two states no
// lifecycle point can leave — the area is written directly.
//
// Every recovery here goes through RunRecover, the production entry point
// `proxyctl node service recover` calls after taking its locks; nothing
// re-implements recovery, so a passing test proves the single shared engine
// (OpenRecoverer -> Recoverer) is what recover uses.
import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hlpclg/singbox-sub-manager/internal/transaction"
)

// spawnTdCrash leaves a real crashed teardown transaction under base.
func spawnTdCrash(t *testing.T, sc tdScenario, base, point, sysdState string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestTeardownCrashChild$", "-test.timeout=5m")
	cmd.Env = append(os.Environ(), envTdCase+"="+sc.name, envCrashBase+"="+base, envCrashPt+"="+point, envCrashSysd+"="+sysdState)
	out, err := cmd.CombinedOutput()
	code := -1
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err == nil {
		code = 0
	}
	if code != crashExitCode {
		t.Fatalf("crash child exit code = %d, want %d (crash at %q)\n%s", code, crashExitCode, point, out)
	}
}

// crashedTd is a leftover transaction: fixture written, child crashed at
// point, the parent's Env/fake systemd ready to recover it.
type crashedTd struct {
	env    *Env
	sysd   *unitFileSystemd
	roots  Roots
	base   string
	state  string // sysd persist file
	before map[string]string
}

func newCrashedTd(t *testing.T, scenario, point string) *crashedTd {
	t.Helper()
	sc, ok := tdScenarioByName(scenario)
	if !ok {
		t.Fatalf("unknown scenario %s", scenario)
	}
	base := t.TempDir()
	roots := rootsUnder(base)
	makeRootDirs(t, roots)
	writeTeardownFixture(t, roots, sc.kind, buildFakeSingboxScript(t, "1.14.1"))
	sysdState := filepath.Join(t.TempDir(), "sysd.json")
	env, sysd := buildTeardownEnv(t, roots, sc.kind, sysdState, false)
	before := treeSnapshot(t, roots)
	spawnTdCrash(t, sc, base, point, sysdState)
	if _, err := os.Stat(roots.TxnDir); err != nil {
		t.Fatalf("the crash at %q left no transaction area: %v", point, err)
	}
	return &crashedTd{env: env, sysd: sysd, roots: roots, base: base, state: sysdState, before: before}
}

func fileBytesOrEmpty(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

// --- the recover child: RunRecover interrupted at the k-th systemd call ---

const (
	envRcCase  = "RTNODE_RC_SCENARIO"
	envRcK     = "RTNODE_RC_K"
	envRcPhase = "RTNODE_RC_PHASE"
)

// TestRecoverCrashChild is the re-exec'd side that crashes *during recover*.
func TestRecoverCrashChild(t *testing.T) {
	name := os.Getenv(envRcCase)
	if name == "" {
		return
	}
	sc, _ := tdScenarioByName(name)
	roots := rootsUnder(os.Getenv(envCrashBase))
	env, sysd := buildTeardownEnv(t, roots, sc.kind, os.Getenv(envCrashSysd), true)
	k, _ := strconv.Atoi(os.Getenv(envRcK))
	phase := os.Getenv(envRcPhase)
	n := 0
	hit := func(at string) func(string) {
		return func(call string) {
			if phase != at {
				return
			}
			n++
			if n == k {
				os.Exit(crashExitCode)
			}
		}
	}
	sysd.onCall = hit("before")
	sysd.onDone = hit("done")
	// a phase counts every state-changing call of its own kind only, so
	// "before k" and "done k" index the same call
	res := RunRecover(context.Background(), env, RecoverParams{})
	fmt.Fprintf(os.Stderr, "recover child finished without reaching call %d (%s): %+v\n", k, phase, res)
	os.Exit(doneExitCode)
}

func TestServiceRecoverMatrix(t *testing.T) {
	if os.Getenv(envTdCase) != "" || os.Getenv(envRcCase) != "" {
		t.Skip("running as a crash child")
	}
	setUmask077(t)

	// ---- states recover is not for (spec §4.3 table: only 未完成事务) ----
	t.Run("refused unless a transaction is unfinished", func(t *testing.T) {
		mk := map[string]func(t *testing.T) (*Env, Roots){
			"not installed": func(t *testing.T) (*Env, Roots) {
				roots := testTempRoots(t)
				env, _ := buildTeardownEnv(t, roots, tdUninstalled, "", false)
				return env, roots
			},
			"installed": func(t *testing.T) (*Env, Roots) {
				r := newTdRun(t, tdInstalled)
				return r.env, r.roots
			},
			"uninstalled with identity": func(t *testing.T) (*Env, Roots) {
				r := newTdRun(t, tdUninstalled)
				return r.env, r.roots
			},
			"inconsistent": func(t *testing.T) (*Env, Roots) {
				r := newTdRun(t, tdInstalled)
				if err := os.WriteFile(r.roots.Config, []byte("tampered"), 0600); err != nil {
					t.Fatal(err)
				}
				return r.env, r.roots
			},
		}
		for name, build := range mk {
			t.Run(strings.ReplaceAll(name, " ", "_"), func(t *testing.T) {
				env, roots := build(t)
				before := treeSnapshot(t, roots)
				for _, params := range []RecoverParams{{}, {Abandon: true}} {
					res := RunRecover(context.Background(), env, params)
					if res.ExitCode() != 1 || !res.PreTransaction || res.Err == nil {
						t.Fatalf("params %+v: want exit 1, got %+v", params, res)
					}
				}
				assertTreeEqual(t, before, treeSnapshot(t, roots))
				if err := PrecheckRecover(env); err == nil {
					t.Fatal("PrecheckRecover must refuse")
				}
				if sd, ok := env.Systemd.(*unitFileSystemd); ok {
					if got := mutatingSystemdCalls(sd.Calls()); len(got) != 0 {
						t.Fatalf("systemd must not be touched: %v", got)
					}
				}
			})
		}
	})

	// ---- phase=running ----
	t.Run("running: nothing modified yet is discarded (exit 0)", func(t *testing.T) {
		c := newCrashedTd(t, "uninstall", "mkdir-snap")
		res := RunRecover(context.Background(), c.env, RecoverParams{})
		if res.ExitCode() != 0 || !res.Recovered || res.RecoveredOutcome != transaction.OutcomeNoChange {
			t.Fatalf("got %+v", res)
		}
		assertTreeEqual(t, c.before, treeSnapshot(t, c.roots))
		assertNoTxnArea(t, c.roots)
		if got := mutatingSystemdCalls(c.sysd.Calls()); len(got) != 0 {
			t.Errorf("no modification was made, systemd must not be touched: %v", got)
		}
		if strings.TrimSpace(res.Stdout) != "" {
			t.Errorf("recover prints nothing on stdout, got %q", res.Stdout)
		}
	})

	t.Run("running: modified is rolled back with the service restored (exit 0)", func(t *testing.T) {
		for _, point := range []string{"started:systemd-disable", "done:remove-unit", "done:remove-configdir", "done:remove-bindir", "done:manifest-write"} {
			c := newCrashedTd(t, "uninstall_prev", point)
			res := RunRecover(context.Background(), c.env, RecoverParams{})
			if res.ExitCode() != 0 || !res.Recovered || res.RecoveredOutcome != transaction.OutcomeRolledBack {
				t.Fatalf("%s: got %+v", point, res)
			}
			assertTreeEqual(t, c.before, treeSnapshot(t, c.roots))
			assertUnitActiveEnabled(t, c.sysd)
			assertNoTxnArea(t, c.roots)
			if st := DetectInstanceState(c.env.FS, c.roots).Status; st != StatusInstalled {
				t.Fatalf("%s: state = %v", point, st)
			}
		}
		for _, point := range []string{"done:remove-secrets", "done:remove-statedir"} {
			c := newCrashedTd(t, "purge_installed", point)
			res := RunRecover(context.Background(), c.env, RecoverParams{})
			if res.ExitCode() != 0 || res.RecoveredOutcome != transaction.OutcomeRolledBack {
				t.Fatalf("purge %s: got %+v", point, res)
			}
			assertTreeEqual(t, c.before, treeSnapshot(t, c.roots))
			assertUnitActiveEnabled(t, c.sysd)
			assertNoTxnArea(t, c.roots)
		}
		c := newCrashedTd(t, "purge_uninstalled", "done:remove-instance")
		res := RunRecover(context.Background(), c.env, RecoverParams{})
		if res.ExitCode() != 0 || res.RecoveredOutcome != transaction.OutcomeRolledBack {
			t.Fatalf("purge of an uninstalled instance: got %+v", res)
		}
		assertTreeEqual(t, c.before, treeSnapshot(t, c.roots))
		assertNoTxnArea(t, c.roots)
	})

	// ---- phase=committed: forward cleanup only ----
	t.Run("committed: forward cleanup, never a rollback (exit 0)", func(t *testing.T) {
		c := newCrashedTd(t, "uninstall", "after-commit-flip")
		res := RunRecover(context.Background(), c.env, RecoverParams{})
		if res.ExitCode() != 0 || res.RecoveredOutcome != transaction.OutcomeCommitted {
			t.Fatalf("got %+v", res)
		}
		assertKeptIdentityOnly(t, &tdRun{env: c.env, roots: c.roots})
		c2 := newCrashedTd(t, "purge_installed", "after-commit-flip")
		res = RunRecover(context.Background(), c2.env, RecoverParams{})
		if res.ExitCode() != 0 || res.RecoveredOutcome != transaction.OutcomeCommitted {
			t.Fatalf("purge: got %+v", res)
		}
		assertPurged(t, &tdRun{env: c2.env, roots: c2.roots})
	})

	// A committed reinstall with a superseded version still to delete: recover
	// completes that cleanup (spec §7.5) — the one lifecycle with real forward
	// cleanup steps, crashed by Task 7's own scenario.
	t.Run("committed: a pending version cleanup is completed", func(t *testing.T) {
		var sc crashScenario
		for _, s := range crashScenarios() {
			if s.name == "reinstall_window" {
				sc = s
			}
		}
		base := t.TempDir()
		roots := rootsUnder(base)
		makeRootDirs(t, roots)
		ports := crashPortsFor(t)
		sysdState := filepath.Join(t.TempDir(), "sysd.json")
		env, ws := runScenarioEnv(t, sc, roots, sysdState)
		sc.setup(t, roots, ws, ports)
		cmd := exec.Command(os.Args[0], "-test.run=^TestCrashChild$", "-test.timeout=5m")
		// crash right after the flip, with the version-1.14.1 removal still
		// registered but not executed
		cmd.Env = append(os.Environ(), envCrashCase+"="+sc.name, envCrashBase+"="+base, envCrashPt+"=after-commit-flip",
			envCrashPort+"="+strconv.Itoa(ports.listen), envCrashSelf+"="+strconv.Itoa(ports.selfcheck), envCrashSysd+"="+sysdState)
		out, err := cmd.CombinedOutput()
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != crashExitCode {
			t.Fatalf("child did not crash as expected: %v\n%s", err, out)
		}
		// the superseded version is still on disk until recover completes it
		if _, err := os.Stat(filepath.Join(roots.BinDir, "versions", "1.14.1")); err != nil {
			t.Fatalf("precondition: the superseded version must still exist: %v", err)
		}
		env.Proc.(*fakeProc).exePath = sc.recoverExe(roots)
		res := RunRecover(context.Background(), env, RecoverParams{})
		if res.ExitCode() != 0 || res.RecoveredOutcome != transaction.OutcomeCommitted {
			t.Fatalf("got %+v", res)
		}
		sc.afterCommit(t, env, roots)
		assertNoTxnArea(t, roots)
	})

	// ---- tombstone ----
	t.Run("tombstone: deleted wholesale without reading it (exit 0)", func(t *testing.T) {
		r := newTdRun(t, tdInstalled)
		tomb := filepath.Join(filepath.Dir(r.roots.StateDir), ".proxyctl-reality-txn.tomb-0123abcd")
		if err := os.MkdirAll(filepath.Join(tomb, "snap", "deep"), 0700); err != nil {
			t.Fatal(err)
		}
		// junk that must never be interpreted: an invalid "journal", a
		// snapshot holding key-like text, an empty file
		writeFile(t, filepath.Join(tomb, "journal.json"), []byte("{not json"), 0600)
		writeFile(t, filepath.Join(tomb, "snap", "x.snap"), []byte(r.fx.privateKey), 0600)
		writeFile(t, filepath.Join(tomb, "snap", "deep", "empty"), nil, 0600)
		if st := DetectInstanceState(r.env.FS, r.roots).Status; st != StatusUnfinishedTxn {
			t.Fatalf("precondition: a tombstone means an unfinished transaction, state = %v", st)
		}
		res := RunRecover(context.Background(), r.env, RecoverParams{})
		if res.ExitCode() != 0 || res.Err != nil {
			t.Fatalf("got %+v", res)
		}
		assertNotExist(t, tomb)
		assertTreeEqual(t, r.before, treeSnapshot(t, r.roots))
		if st := DetectInstanceState(r.env.FS, r.roots).Status; st != StatusInstalled {
			t.Fatalf("state after = %v", st)
		}
		if got := mutatingSystemdCalls(r.sysd.Calls()); len(got) != 0 {
			t.Errorf("systemd must not be touched: %v", got)
		}
	})

	t.Run("tombstone next to abandon is deleted the same way", func(t *testing.T) {
		r := newTdRun(t, tdUninstalled)
		tomb := filepath.Join(filepath.Dir(r.roots.StateDir), ".proxyctl-reality-txn.tomb-feed0001")
		if err := os.MkdirAll(filepath.Join(tomb, "snap"), 0700); err != nil {
			t.Fatal(err)
		}
		res := RunRecover(context.Background(), r.env, RecoverParams{Abandon: true})
		if res.ExitCode() != 0 {
			t.Fatalf("got %+v", res)
		}
		assertNotExist(t, tomb)
	})

	// ---- no journal.json (spec §7.6) ----
	t.Run("no journal: an empty area or journal.json.tmp alone is discarded (exit 0)", func(t *testing.T) {
		for name, files := range map[string]map[string]string{
			"empty":                  {},
			"journal.json.tmp":       {"journal.json.tmp": "{\"half written"},
			"empty journal.json.tmp": {"journal.json.tmp": ""},
		} {
			r := newTdRun(t, tdInstalled)
			if err := os.Mkdir(r.roots.TxnDir, 0700); err != nil {
				t.Fatal(err)
			}
			for n, c := range files {
				writeFile(t, filepath.Join(r.roots.TxnDir, n), []byte(c), 0600)
			}
			res := RunRecover(context.Background(), r.env, RecoverParams{})
			if res.ExitCode() != 0 || res.Err != nil {
				t.Fatalf("%s: got %+v", name, res)
			}
			assertNoTxnArea(t, r.roots)
			assertTreeEqual(t, r.before, treeSnapshot(t, r.roots))
			if st := DetectInstanceState(r.env.FS, r.roots).Status; st != StatusInstalled {
				t.Fatalf("%s: state after = %v", name, st)
			}
		}
	})

	t.Run("no journal: anything else is exit 5 and untouched", func(t *testing.T) {
		cases := map[string]func(t *testing.T, dir string){
			"dl directory": func(t *testing.T, dir string) {
				if err := os.Mkdir(filepath.Join(dir, "dl"), 0700); err != nil {
					t.Fatal(err)
				}
			},
			"journal.json.tmp plus a snapshot": func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "journal.json.tmp"), []byte("x"), 0600)
				if err := os.Mkdir(filepath.Join(dir, "snap"), 0700); err != nil {
					t.Fatal(err)
				}
			},
			"a stray file": func(t *testing.T, dir string) { writeFile(t, filepath.Join(dir, "notes"), []byte("x"), 0600) },
		}
		for name, mk := range cases {
			r := newTdRun(t, tdInstalled)
			if err := os.Mkdir(r.roots.TxnDir, 0700); err != nil {
				t.Fatal(err)
			}
			mk(t, r.roots.TxnDir)
			areaBefore := dirTree(t, r.roots.TxnDir)
			sceneBefore := treeSnapshot(t, r.roots)
			for _, params := range []RecoverParams{{}, {Abandon: true}} {
				res := RunRecover(context.Background(), r.env, params)
				if res.ExitCode() != 5 || res.PreTransaction || res.TxnDir != r.roots.TxnDir {
					t.Fatalf("%s %+v: want exit 5 naming the area, got %+v", name, params, res)
				}
				if !errors.Is(res.Err, transaction.ErrTampered) {
					t.Errorf("%s: want ErrTampered, got %v", name, res.Err)
				}
				if got := dirTree(t, r.roots.TxnDir); got != areaBefore {
					t.Fatalf("%s: the area was modified:\nbefore %s\nafter  %s", name, areaBefore, got)
				}
			}
			assertTreeEqual(t, sceneBefore, treeSnapshot(t, r.roots))
		}
	})

	// ---- tamper / unknown schema (spec §7.1, §7.4 step 1) ----
	t.Run("tamper: exit 5, the scene and the area are kept untouched", func(t *testing.T) {
		c := newCrashedTd(t, "uninstall", "done:remove-unit")
		if err := os.WriteFile(c.roots.Config, []byte("edited by someone else"), 0600); err != nil {
			t.Fatal(err)
		}
		journalBefore := fileBytesOrEmpty(filepath.Join(c.roots.TxnDir, "journal.json"))
		sceneBefore := treeSnapshot(t, c.roots)
		areaBefore := dirTree(t, c.roots.TxnDir)

		res := RunRecover(context.Background(), c.env, RecoverParams{})
		if res.ExitCode() != 5 || res.TxnDir != c.roots.TxnDir || !errors.Is(res.Err, transaction.ErrTampered) {
			t.Fatalf("got %+v", res)
		}
		assertTreeEqual(t, sceneBefore, treeSnapshot(t, c.roots))
		if fileBytesOrEmpty(filepath.Join(c.roots.TxnDir, "journal.json")) != journalBefore || dirTree(t, c.roots.TxnDir) != areaBefore {
			t.Fatalf("recover must not modify the journal or the area when it detects tampering")
		}
		if got := mutatingSystemdCalls(c.sysd.Calls()); len(got) != 0 {
			t.Errorf("recover must not touch systemd when it detects tampering: %v", got)
		}
	})

	t.Run("unknown journal schema: exit 5, names the writing version, nothing changes", func(t *testing.T) {
		c := newCrashedTd(t, "uninstall", "done:remove-unit")
		jp := filepath.Join(c.roots.TxnDir, "journal.json")
		if err := os.WriteFile(jp, []byte(`{"schema":2,"proxyctl_version":"v9.9.9"}`), 0600); err != nil {
			t.Fatal(err)
		}
		sceneBefore := treeSnapshot(t, c.roots)
		areaBefore := dirTree(t, c.roots.TxnDir)
		res := RunRecover(context.Background(), c.env, RecoverParams{})
		if res.ExitCode() != 5 || res.TxnDir != c.roots.TxnDir || res.Err == nil || !strings.Contains(res.Err.Error(), "v9.9.9") {
			t.Fatalf("got %+v", res)
		}
		assertTreeEqual(t, sceneBefore, treeSnapshot(t, c.roots))
		if dirTree(t, c.roots.TxnDir) != areaBefore {
			t.Fatalf("the area was modified")
		}
	})

	// ---- abandon (spec §7.4 人工放弃出口) ----
	t.Run("abandon: everything already at the pre-transaction state, no restart, no gate", func(t *testing.T) {
		c := newCrashedTd(t, "uninstall", "mkdir-snap")
		res := RunRecover(context.Background(), c.env, RecoverParams{Abandon: true})
		if res.ExitCode() != 0 || !res.Recovered || res.RecoveredOutcome != transaction.OutcomeAbandoned {
			t.Fatalf("got %+v", res)
		}
		assertTreeEqual(t, c.before, treeSnapshot(t, c.roots))
		assertNoTxnArea(t, c.roots)
		if got := c.sysd.Calls(); len(mutatingSystemdCalls(got)) != 0 {
			t.Errorf("abandon must not run systemctl mutations: %v", got)
		}
		warn := strings.Join(res.Warnings, "\n")
		if !strings.Contains(warn, "may not be running") {
			t.Errorf("abandon must warn on stderr that the service may not be running: %q", warn)
		}
	})

	t.Run("abandon after a rollback whose gate failed: files restored, service left as is", func(t *testing.T) {
		r := newTdRun(t, tdInstalled)
		r.env.Proc.(*fakeProc).exePath = "/not/the/private/binary" // the rollback's §9.2 gate cannot pass
		setHook(t, func(p string) error {
			if p == "done:remove-configdir" {
				return errors.New("injected fault")
			}
			return nil
		})
		res := Uninstall(context.Background(), r.env, UninstallParams{})
		setHook(t, func(string) error { return nil })
		if res.ExitCode() != 5 || !errors.Is(res.Err, transaction.ErrGateFailed) {
			t.Fatalf("precondition: want exit 5 (gate failed), got %+v", res)
		}
		assertTreeEqual(t, withoutPrefix(treeSnapshot(t, r.roots), r.roots.TxnDir), r.before) // all files restored (the area itself is still there)
		if _, err := os.Stat(r.roots.TxnDir); err != nil {
			t.Fatalf("the area must be preserved: %v", err)
		}
		callsBefore := len(r.sysd.Calls())

		ab := RunRecover(context.Background(), r.env, RecoverParams{Abandon: true})
		if ab.ExitCode() != 0 || ab.RecoveredOutcome != transaction.OutcomeAbandoned {
			t.Fatalf("got %+v", ab)
		}
		assertNoTxnArea(t, r.roots)
		assertTreeEqual(t, r.before, treeSnapshot(t, r.roots))
		if got := mutatingSystemdCalls(r.sysd.Calls()[callsBefore:]); len(got) != 0 {
			t.Errorf("abandon must neither restart nor otherwise touch systemd, saw %v", got)
		}
		if st := DetectInstanceState(r.env.FS, r.roots).Status; st != StatusInstalled {
			t.Fatalf("state = %v", st)
		}
	})

	t.Run("abandon is refused (exit 1, nothing changed) unless every path is at the pre-transaction state", func(t *testing.T) {
		for _, point := range []string{"done:remove-unit", "after-commit-flip"} {
			c := newCrashedTd(t, "uninstall", point)
			journalBefore := fileBytesOrEmpty(filepath.Join(c.roots.TxnDir, "journal.json"))
			sceneBefore := treeSnapshot(t, c.roots)
			areaBefore := dirTree(t, c.roots.TxnDir)
			res := RunRecover(context.Background(), c.env, RecoverParams{Abandon: true})
			if res.ExitCode() != 1 || !res.PreTransaction || !errors.Is(res.Err, transaction.ErrAbandonNotEligible) {
				t.Fatalf("%s: got %+v", point, res)
			}
			assertTreeEqual(t, sceneBefore, treeSnapshot(t, c.roots))
			if fileBytesOrEmpty(filepath.Join(c.roots.TxnDir, "journal.json")) != journalBefore || dirTree(t, c.roots.TxnDir) != areaBefore {
				t.Fatalf("%s: a refused abandon must change nothing", point)
			}
			if got := mutatingSystemdCalls(c.sysd.Calls()); len(got) != 0 {
				t.Errorf("%s: %v", point, got)
			}
			// the same leftover is still recoverable the normal way
			res = RunRecover(context.Background(), c.env, RecoverParams{})
			if res.ExitCode() != 0 {
				t.Fatalf("%s: plain recover after a refused abandon: %+v", point, res)
			}
		}
	})

	t.Run("an unregistered temp name makes abandon ineligible", func(t *testing.T) {
		c := newCrashedTd(t, "uninstall", "done:unit-snap") // the unit's directory is now a managed scan directory
		stray := filepath.Join(filepath.Dir(c.roots.SystemdUnit), transaction.TempName("someoneelse", 1))
		writeFile(t, stray, []byte("x"), 0600)
		res := RunRecover(context.Background(), c.env, RecoverParams{Abandon: true})
		if res.ExitCode() != 1 || !errors.Is(res.Err, transaction.ErrAbandonNotEligible) {
			t.Fatalf("got %+v", res)
		}
		if _, err := os.Stat(stray); err != nil {
			t.Errorf("abandon must not delete anything: %v", err)
		}
		if _, err := os.Stat(c.roots.TxnDir); err != nil {
			t.Errorf("the area must stay: %v", err)
		}
	})

	// ---- recover itself interrupted, repeatedly ----
	t.Run("recover interrupted at every systemd call converges", func(t *testing.T) {
		// "started:systemd-disable": the unit is still loaded, so the rollback
		// demotes it (stop, disable) before restoring; "done:remove-bindir":
		// the unit is already gone and reloaded, so only the restoring half
		// (daemon-reload, enable, restart) runs.
		for _, tc := range []struct {
			point     string
			wantCalls int
		}{{"started:systemd-disable", 5}, {"done:remove-bindir", 3}} {
			tc := tc
			const scenario = "uninstall_prev"
			t.Run(strings.NewReplacer(":", "_").Replace(tc.point), func(t *testing.T) {
				// discover how many state-changing calls a full recover makes
				disc := newCrashedTd(t, scenario, tc.point)
				calls := 0
				disc.sysd.onCall = func(string) { calls++ }
				if res := RunRecover(context.Background(), disc.env, RecoverParams{}); res.ExitCode() != 0 {
					t.Fatalf("discovery recover: %+v", res)
				}
				if calls != tc.wantCalls {
					t.Fatalf("a full recover from %s makes %d state-changing systemd calls, want %d", tc.point, calls, tc.wantCalls)
				}
				for _, phase := range []string{"before", "done"} {
					for k := 1; k <= calls; k++ {
						phase, k := phase, k
						t.Run(fmt.Sprintf("%s_call_%d", phase, k), func(t *testing.T) {
							c := newCrashedTd(t, scenario, tc.point)
							cmd := exec.Command(os.Args[0], "-test.run=^TestRecoverCrashChild$", "-test.timeout=5m")
							cmd.Env = append(os.Environ(), envRcCase+"="+scenario, envRcK+"="+strconv.Itoa(k), envRcPhase+"="+phase,
								envCrashBase+"="+c.base, envCrashSysd+"="+c.state)
							out, err := cmd.CombinedOutput()
							if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != crashExitCode {
								t.Fatalf("recover child did not crash at call %d (%s): %v\n%s", k, phase, err, out)
							}
							res := RunRecover(context.Background(), c.env, RecoverParams{})
							if res.ExitCode() != 0 || res.Err != nil {
								t.Fatalf("recover after an interrupted recover (call %d %s): %+v", k, phase, res)
							}
							assertTreeEqual(t, c.before, treeSnapshot(t, c.roots))
							assertUnitActiveEnabled(t, c.sysd)
							assertNoTxnArea(t, c.roots)
						})
					}
				}
			})
		}
	})

	t.Run("a recover that fails midway keeps the area and a retry converges", func(t *testing.T) {
		c := newCrashedTd(t, "uninstall", "done:remove-configdir")
		c.sysd.failRestarts = 1
		res := RunRecover(context.Background(), c.env, RecoverParams{})
		if res.ExitCode() != 5 || res.PreTransaction || res.TxnDir != c.roots.TxnDir {
			t.Fatalf("got %+v", res)
		}
		if _, err := os.Stat(c.roots.TxnDir); err != nil {
			t.Fatalf("the area must stay after a failed recover: %v", err)
		}
		res = RunRecover(context.Background(), c.env, RecoverParams{})
		if res.ExitCode() != 0 {
			t.Fatalf("retry: %+v", res)
		}
		assertTreeEqual(t, c.before, treeSnapshot(t, c.roots))
		assertUnitActiveEnabled(t, c.sysd)
		assertNoTxnArea(t, c.roots)

		// and once it converged there is nothing left to recover
		res = RunRecover(context.Background(), c.env, RecoverParams{})
		if res.ExitCode() != 1 || !res.PreTransaction {
			t.Fatalf("a third recover: want exit 1 (no unfinished transaction), got %+v", res)
		}
	})

	// ---- cancellation (spec §13.3) ----
	t.Run("cancelled before recovery starts: exit 1, nothing touched", func(t *testing.T) {
		c := newCrashedTd(t, "uninstall", "done:remove-unit")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		sceneBefore := treeSnapshot(t, c.roots)
		areaBefore := dirTree(t, c.roots.TxnDir)
		res := RunRecover(ctx, c.env, RecoverParams{})
		if res.ExitCode() != 1 || !res.PreTransaction {
			t.Fatalf("got %+v", res)
		}
		assertTreeEqual(t, sceneBefore, treeSnapshot(t, c.roots))
		if dirTree(t, c.roots.TxnDir) != areaBefore {
			t.Fatalf("the area was modified")
		}
	})

	t.Run("a signal during recovery does not interrupt it", func(t *testing.T) {
		c := newCrashedTd(t, "uninstall", "done:remove-unit")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c.sysd.onCall = func(name string) {
			if name == "stop" {
				cancel()
			}
		}
		res := RunRecover(ctx, c.env, RecoverParams{})
		if ctx.Err() == nil {
			t.Fatal("never cancelled")
		}
		if res.ExitCode() != 0 || res.RecoveredOutcome != transaction.OutcomeRolledBack {
			t.Fatalf("got %+v", res)
		}
		assertTreeEqual(t, c.before, treeSnapshot(t, c.roots))
		assertUnitActiveEnabled(t, c.sysd)
	})
}

// dirTree renders a directory tree (names, kinds, modes and file bytes) as
// one string, for asserting an area was not modified.
func dirTree(t *testing.T, root string) string {
	t.Helper()
	var sb strings.Builder
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		fmt.Fprintf(&sb, "%s %v", rel, fi.Mode())
		if fi.Mode().IsRegular() {
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			fmt.Fprintf(&sb, " %s", hashHex(b))
		}
		sb.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return sb.String()
}

// withoutPrefix drops every entry at or below prefix from a tree snapshot.
func withoutPrefix(m map[string]string, prefix string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if k == prefix || strings.HasPrefix(k, prefix+"/") {
			continue
		}
		out[k] = v
	}
	return out
}
