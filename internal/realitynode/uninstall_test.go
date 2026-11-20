package realitynode

// uninstall_test.go: plan Task 8's `uninstall` and `uninstall --purge`
// (design spec §8.4, §8.5, §13.1, §13.3, §14.1, §14.2), built — like every
// other lifecycle test in this package — on real temporary trusted roots
// (real fsync/rename/chmod/rmdir) with only systemd, the sing-box download
// source and the network faked.
//
// The fake systemd used here (unitFileSystemd) is unit-file aware: once the
// unit file is gone, a daemon-reload makes the unit not-found (LoadState
// "not loaded", UnitFileState ""), exactly what systemd reports for a unit
// whose file was removed and reloaded. The older fakeServiceSystemd keeps
// reporting the pre-removal UnitFileState forever, which would hide any
// mismatch between the journal's systemd chain and real systemd.
//
// Fault injection uses the same process-global lifecycleHook seam as
// service_crash_test.go: every lifecycle step reports a named point, a
// discovery run collects them, and each point then becomes one deterministic
// failure (in-process error) or hard crash (child process os.Exit).
import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hlpclg/singbox-sub-manager/internal/transaction"
)

// --- unit-file-aware fake systemd ---

type unitFileSystemd struct {
	*fakeServiceSystemd
	unitPath string
	// onCall, when set, runs at the top of every state-changing call with
	// its name ("stop", "disable", "enable", "daemon-reload", "restart",
	// "start"), before the call takes effect.
	onCall func(name string)
	// onDone, when set, runs after each state-changing call has taken
	// effect (and before the caller learns of it).
	onDone func(name string)
	// blockRestartN, when > 0, makes Restart number blockRestartN (1-based)
	// block until its context is done, then return the context's error (a
	// hung systemctl honouring its 15 s timeout / the rollback budget).
	blockRestartN int
	restartCalls  int
	// failRestarts makes the next failRestarts restarts fail.
	failRestarts int
}

func (u *unitFileSystemd) call(ctx context.Context, name string) error {
	if u.onCall != nil {
		u.onCall(name)
	}
	return ctx.Err()
}

func (u *unitFileSystemd) done(name string) {
	if u.onDone != nil {
		u.onDone(name)
	}
}

func (u *unitFileSystemd) Stop(ctx context.Context, unit string) error {
	if err := u.call(ctx, "stop"); err != nil {
		return err
	}
	err := u.fakeServiceSystemd.Stop(ctx, unit)
	u.done("stop")
	return err
}

func (u *unitFileSystemd) SetUnitFileState(ctx context.Context, unit, want string) error {
	name := "disable"
	if want == "enabled" {
		name = "enable"
	}
	if err := u.call(ctx, name); err != nil {
		return err
	}
	err := u.fakeServiceSystemd.SetUnitFileState(ctx, unit, want)
	u.done(name)
	return err
}

func (u *unitFileSystemd) DaemonReload(ctx context.Context) error {
	if err := u.call(ctx, "daemon-reload"); err != nil {
		return err
	}
	f := u.fakeServiceSystemd
	f.record("daemon-reload")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadLocked()
	if _, err := os.Lstat(u.unitPath); err != nil {
		f.loaded, f.unitFileState, f.activeState = false, "", "inactive"
	} else {
		f.loaded = true
		if f.unitFileState == "" {
			f.unitFileState = "disabled"
		}
	}
	f.saveLocked()
	u.done("daemon-reload")
	return nil
}

func (u *unitFileSystemd) Restart(ctx context.Context, unit string) error {
	u.restartCalls++
	if err := u.call(ctx, "restart"); err != nil {
		return err
	}
	if u.blockRestartN > 0 && u.restartCalls == u.blockRestartN {
		<-ctx.Done()
		return ctx.Err()
	}
	if u.failRestarts > 0 {
		u.failRestarts--
		return errors.New("injected: systemctl restart failed")
	}
	err := u.fakeServiceSystemd.Restart(ctx, unit)
	u.done("restart")
	return err
}

var _ SystemdFull = (*unitFileSystemd)(nil)

// mutatingSystemdCalls filters a recorded call list down to the calls that
// change host state.
func mutatingSystemdCalls(calls []string) []string {
	var out []string
	for _, c := range calls {
		switch c {
		case "stop", "disable", "enable", "daemon-reload", "restart", "start":
			out = append(out, c)
		}
	}
	return out
}

// --- fixtures ---

type tdKind string

const (
	tdInstalled     tdKind = "installed"      // one registered version
	tdInstalledPrev tdKind = "installed_prev" // current + retained previous version
	tdUninstalled   tdKind = "uninstalled"    // identity retained only
)

func (k tdKind) active() bool { return k != tdUninstalled }

// writeTeardownFixture writes the pre-transaction state for kind under
// roots. script is the executable fake sing-box, stored as the private
// binary so the rollback gate's `sing-box check` really runs.
func writeTeardownFixture(t *testing.T, roots Roots, kind tdKind, script string) installedFixture {
	t.Helper()
	switch kind {
	case tdInstalled:
		return writeInstalledFixtureOpts(t, roots, fixtureOpts{binContent: readBytes(t, script)})
	case tdInstalledPrev:
		return writeInstalledFixtureOpts(t, roots, fixtureOpts{binContent: readBytes(t, script), prevVersion: "1.14.0"})
	default:
		return writeUninstalledFixtureOpts(t, roots, fixtureOpts{})
	}
}

// buildTeardownEnv builds an Env for kind against roots, the fake systemd
// mirroring what real systemd would report for that state. When persist is
// set the fake's state lives in that JSON file (adopt=true: the process
// adopts what another process seeded; false: this process seeds it).
func buildTeardownEnv(t *testing.T, roots Roots, kind tdKind, persist string, adopt bool) (*Env, *unitFileSystemd) {
	t.Helper()
	script := buildFakeSingboxScript(t, "1.14.1")
	env, sysd := testEnvAt(t, roots, script, "1.14.1")
	if kind.active() {
		sysd.loaded, sysd.unitFileState, sysd.activeState = true, "enabled", "active"
	}
	wrap := &unitFileSystemd{fakeServiceSystemd: sysd, unitPath: roots.SystemdUnit}
	env.Systemd = wrap
	if persist != "" {
		if adopt {
			sysd.persistPath = persist
		} else {
			sysd.persistTo(persist)
		}
	}
	return env, wrap
}

type tdRun struct {
	env   *Env
	sysd  *unitFileSystemd
	roots Roots
	fx    installedFixture
	// before is the tree (bytes, mode, owner, existence) before the run.
	before map[string]string
}

func newTdRun(t *testing.T, kind tdKind) *tdRun {
	t.Helper()
	roots := testTempRoots(t)
	script := buildFakeSingboxScript(t, "1.14.1")
	fx := writeTeardownFixture(t, roots, kind, script)
	env, sysd := buildTeardownEnv(t, roots, kind, "", false)
	return &tdRun{env: env, sysd: sysd, roots: roots, fx: fx, before: treeSnapshot(t, roots)}
}

func setUmask077(t *testing.T) {
	t.Helper()
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })
}

func assertUnitActiveEnabled(t *testing.T, s *unitFileSystemd) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	if !s.loaded || s.unitFileState != "enabled" || s.activeState != "active" {
		t.Fatalf("systemd state = loaded:%v ufs:%q active:%q, want the pre-transaction loaded/enabled/active", s.loaded, s.unitFileState, s.activeState)
	}
}

func execLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if !strings.HasPrefix(l, "#") {
			lines = append(lines, l)
		}
	}
	return lines
}

// --- uninstall: the committed path (spec §8.4) ---

func TestUninstallCommit(t *testing.T) {
	setUmask077(t)
	for _, kind := range []tdKind{tdInstalled, tdInstalledPrev} {
		kind := kind
		t.Run(string(kind), func(t *testing.T) {
			r := newTdRun(t, kind)
			identity := map[string]string{
				"secrets":  string(readBytes(t, r.roots.Secrets)),
				"instance": string(readBytes(t, r.roots.Instance)),
			}
			identityBefore := treeSnapshotOf(t, r.roots.Secrets, r.roots.Instance)

			res := Uninstall(context.Background(), r.env, UninstallParams{})
			assertCommitted(t, res)
			if res.ExitCode() != 0 {
				t.Fatalf("exit = %d, want 0", res.ExitCode())
			}
			assertNoTxnArea(t, r.roots)

			// stopped, disabled, the unit file removed, then reloaded: in
			// exactly that order (spec §8.4 steps 2-3)
			if got := strings.Join(mutatingSystemdCalls(r.sysd.Calls()), ","); got != "stop,disable,daemon-reload" {
				t.Errorf("systemd call order = %s, want stop,disable,daemon-reload", got)
			}
			assertNotExist(t, r.roots.SystemdUnit)
			assertNotExist(t, r.roots.ConfigDir)
			assertNotExist(t, r.roots.BinDir)

			// identity kept byte for byte, mode and owner included
			if string(readBytes(t, r.roots.Secrets)) != identity["secrets"] || string(readBytes(t, r.roots.Instance)) != identity["instance"] {
				t.Errorf("secrets.json/instance.json must be kept unchanged")
			}
			assertTreeEqual(t, identityBefore, treeSnapshotOf(t, r.roots.Secrets, r.roots.Instance))
			assertOwnerMode(t, r.roots.StateDir, 0700)
			assertOwnerMode(t, r.roots.Secrets, 0600)
			assertOwnerMode(t, r.roots.Instance, 0600)
			assertOwnerMode(t, r.roots.Manifest, 0600)

			// manifest: uninstalled, only the state directory and the two
			// dynamic-state files remain registered (spec §5.2)
			m := readManifestT(t, r.roots)
			if m.State != ManifestStateUninstalled || m.SingboxPrevious != "" {
				t.Errorf("manifest = %+v, want state=uninstalled and no previous version", m)
			}
			var paths []string
			for _, e := range m.Entries {
				paths = append(paths, e.Path)
			}
			want := []string{r.roots.StateDir, r.roots.Secrets, r.roots.Instance}
			if strings.Join(sortedCopy(paths), "\n") != strings.Join(sortedCopy(want), "\n") {
				t.Errorf("uninstalled manifest entries = %v, want %v", paths, want)
			}
			if st := DetectInstanceState(r.env.FS, r.roots); st.Status != StatusUninstalledKeepIdentity {
				t.Fatalf("state = %v (%v), want uninstalled-keep-identity", st.Status, st.Reasons)
			}

			// paste block: one executable disable line, commented remove
			assertBlock(t, res.Stdout, wantNameLine("disable", r.fx.name), wantNameLine("remove", r.fx.name))
			if got := execLines(res.Stdout); len(got) != 1 {
				t.Errorf("executable lines = %v", got)
			}
			calls := runBlockInShell(t, res.Stdout)
			if len(calls) != 1 || !strings.Contains(calls[0], "node disable") {
				t.Errorf("pasting the block must run exactly the disable command, ran %v", calls)
			}

			// the shared proxyctl and bootstrap's previous copy are mentioned
			notice := strings.Join(res.Notices, "\n")
			if !strings.Contains(notice, "proxyctl") || !strings.Contains(notice, "bootstrap") {
				t.Errorf("notices must say proxyctl and the bootstrap previous copy were kept, got %q", notice)
			}
		})
	}
}

// treeSnapshotOf describes exactly the given files (bytes hash, mode,
// owner), for comparing kept files across an operation.
func treeSnapshotOf(t *testing.T, paths ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatalf("lstat %s: %v", p, err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		desc := fmt.Sprintf("%v %d:%d", fi.Mode(), st.Uid, st.Gid)
		if fi.Mode().IsRegular() {
			desc += " " + hashHex(readBytes(t, p))
		}
		out[p] = desc
	}
	return out
}

// An uninstalled-with-identity instance never generates new keys: install is
// refused before anything is touched, and reinstall reuses the retained
// identity (spec §4.3, §8.2, plan Task 8).
func TestUninstalledIdentityIsNeverRegenerated(t *testing.T) {
	r := newTdRun(t, tdInstalled)
	assertCommitted(t, Uninstall(context.Background(), r.env, UninstallParams{}))
	kept := treeSnapshotOf(t, r.roots.Secrets, r.roots.Instance, r.roots.Manifest)

	res := Install(context.Background(), r.env, validInstallParams())
	if !res.PreTransaction || res.ExitCode() != 1 {
		t.Fatalf("install over an uninstalled-with-identity instance must be refused with 1, got %+v", res)
	}
	res = Uninstall(context.Background(), r.env, UninstallParams{})
	if !res.PreTransaction || res.ExitCode() != 1 {
		t.Fatalf("a second plain uninstall must be refused with 1 (spec §4.3), got %+v", res)
	}
	assertTreeEqual(t, kept, treeSnapshotOf(t, r.roots.Secrets, r.roots.Instance, r.roots.Manifest))
	assertNoTxnArea(t, r.roots)
	assertNotExist(t, r.roots.SystemdUnit)
}

// --- eligibility (spec §4.3 state table) ---

func TestUninstallStateEligibility(t *testing.T) {
	type row struct {
		name   string
		setup  func(t *testing.T) (*Env, Roots)
		params UninstallParams
	}
	plainEnv := func(t *testing.T) (*Env, Roots) {
		roots := testTempRoots(t)
		env, _ := testEnvAt(t, roots, buildFakeSingboxScript(t, "1.14.1"), "1.14.1")
		return env, roots
	}
	rows := []row{
		{"uninstall: not installed", func(t *testing.T) (*Env, Roots) { return plainEnv(t) }, UninstallParams{}},
		{"purge: not installed", func(t *testing.T) (*Env, Roots) { return plainEnv(t) }, UninstallParams{Purge: true}},
		{"uninstall: already uninstalled", func(t *testing.T) (*Env, Roots) {
			env, roots := plainEnv(t)
			writeUninstalledFixture(t, roots)
			return env, roots
		}, UninstallParams{}},
		{"uninstall: unfinished transaction", func(t *testing.T) (*Env, Roots) {
			env, roots := plainEnv(t)
			writeInstalledFixture(t, roots, nil)
			if err := os.Mkdir(roots.TxnDir, 0700); err != nil {
				t.Fatal(err)
			}
			return env, roots
		}, UninstallParams{}},
		{"purge: unfinished transaction", func(t *testing.T) (*Env, Roots) {
			env, roots := plainEnv(t)
			writeInstalledFixture(t, roots, nil)
			if err := os.Mkdir(roots.TxnDir, 0700); err != nil {
				t.Fatal(err)
			}
			return env, roots
		}, UninstallParams{Purge: true}},
		{"purge: inconsistent (unit missing)", func(t *testing.T) (*Env, Roots) {
			env, roots := plainEnv(t)
			writeInstalledFixture(t, roots, nil)
			if err := os.Remove(roots.SystemdUnit); err != nil {
				t.Fatal(err)
			}
			return env, roots
		}, UninstallParams{Purge: true}},
		{"uninstall: inconsistent (config tampered)", func(t *testing.T) (*Env, Roots) {
			env, roots := plainEnv(t)
			writeInstalledFixture(t, roots, nil)
			if err := os.WriteFile(roots.Config, []byte("tampered"), 0600); err != nil {
				t.Fatal(err)
			}
			return env, roots
		}, UninstallParams{}},
	}
	for _, tc := range rows {
		t.Run(strings.ReplaceAll(tc.name, " ", "_"), func(t *testing.T) {
			env, roots := tc.setup(t)
			before := treeSnapshot(t, roots)
			res := Uninstall(context.Background(), env, tc.params)
			if !res.PreTransaction || res.Err == nil || res.ExitCode() != 1 {
				t.Fatalf("want exit 1 with nothing modified, got %+v", res)
			}
			assertTreeEqual(t, before, treeSnapshot(t, roots))
			if env.Systemd != nil {
				if fs, ok := env.Systemd.(*fakeServiceSystemd); ok && len(fs.Calls()) != 0 {
					t.Fatalf("systemd must not be touched: %v", fs.Calls())
				}
			}
		})
	}
}

// Content the manifest does not register inside an instance directory would
// make the final rmdir fail after modifications had started; it is refused
// up front instead (exit 1, nothing modified).
func TestUninstallRefusesUnregisteredContent(t *testing.T) {
	cases := map[string]func(t *testing.T, r *tdRun) UninstallParams{
		"stray file in the config directory": func(t *testing.T, r *tdRun) UninstallParams {
			writeFile(t, filepath.Join(r.roots.ConfigDir, "extra.json"), []byte("x"), 0600)
			return UninstallParams{}
		},
		"stray version directory": func(t *testing.T, r *tdRun) UninstallParams {
			if err := os.Mkdir(filepath.Join(r.roots.BinDir, "versions", "9.9.9"), 0755); err != nil {
				t.Fatal(err)
			}
			return UninstallParams{}
		},
		"stray file next to the binary": func(t *testing.T, r *tdRun) UninstallParams {
			writeFile(t, filepath.Join(r.roots.BinDir, "versions", "1.14.1", "notes.txt"), []byte("x"), 0644)
			return UninstallParams{}
		},
		"purge: stray file in the state directory": func(t *testing.T, r *tdRun) UninstallParams {
			writeFile(t, filepath.Join(r.roots.StateDir, "note.txt"), []byte("x"), 0600)
			return UninstallParams{Purge: true}
		},
	}
	for name, mutate := range cases {
		t.Run(strings.ReplaceAll(name, " ", "_"), func(t *testing.T) {
			r := newTdRun(t, tdInstalled)
			params := mutate(t, r)
			before := treeSnapshot(t, r.roots)
			res := Uninstall(context.Background(), r.env, params)
			if !res.PreTransaction || res.ExitCode() != 1 {
				t.Fatalf("want exit 1, got %+v", res)
			}
			assertTreeEqual(t, before, treeSnapshot(t, r.roots))
			assertNoTxnArea(t, r.roots)
			if got := mutatingSystemdCalls(r.sysd.Calls()); len(got) != 0 {
				t.Fatalf("systemd must not be touched: %v", got)
			}
		})
	}

	t.Run("insufficient disk space", func(t *testing.T) {
		r := newTdRun(t, tdInstalled)
		r.env.CheckDiskSpace = func(string, uint64) error { return errors.New("simulated: no space left") }
		res := Uninstall(context.Background(), r.env, UninstallParams{Purge: true})
		if !res.PreTransaction || res.ExitCode() != 1 {
			t.Fatalf("want exit 1, got %+v", res)
		}
		assertTreeEqual(t, r.before, treeSnapshot(t, r.roots))
		assertNoTxnArea(t, r.roots)
	})
}

// --- fault at every lifecycle step: full restoration (spec §8.4/§8.5,
// §14.2 "uninstall、purge在删除目录之后失败，回滚重建目录") ---

type tdScenario struct {
	name   string
	kind   tdKind
	params UninstallParams
	// afterCommit asserts the committed state.
	afterCommit func(t *testing.T, r *tdRun)
}

func tdScenarios() []tdScenario {
	return []tdScenario{
		{"uninstall", tdInstalled, UninstallParams{}, func(t *testing.T, r *tdRun) { assertKeptIdentityOnly(t, r) }},
		{"uninstall_prev", tdInstalledPrev, UninstallParams{}, func(t *testing.T, r *tdRun) { assertKeptIdentityOnly(t, r) }},
		{"purge_installed", tdInstalled, UninstallParams{Purge: true}, func(t *testing.T, r *tdRun) { assertPurged(t, r) }},
		{"purge_installed_prev", tdInstalledPrev, UninstallParams{Purge: true}, func(t *testing.T, r *tdRun) { assertPurged(t, r) }},
		{"purge_uninstalled", tdUninstalled, UninstallParams{Purge: true}, func(t *testing.T, r *tdRun) { assertPurged(t, r) }},
	}
}

func assertKeptIdentityOnly(t *testing.T, r *tdRun) {
	t.Helper()
	if st := DetectInstanceState(r.env.FS, r.roots); st.Status != StatusUninstalledKeepIdentity {
		t.Fatalf("state = %v (%v), want uninstalled-keep-identity", st.Status, st.Reasons)
	}
	assertNotExist(t, r.roots.SystemdUnit)
	assertNotExist(t, r.roots.ConfigDir)
	assertNotExist(t, r.roots.BinDir)
	assertNoTxnArea(t, r.roots)
}

func assertPurged(t *testing.T, r *tdRun) {
	t.Helper()
	assertNoInstanceAssets(t, r.roots)
	assertNotExist(t, r.roots.Secrets)
	assertNotExist(t, r.roots.Instance)
	assertNotExist(t, r.roots.Manifest)
	assertNoTxnArea(t, r.roots)
	if st := DetectInstanceState(r.env.FS, r.roots); st.Status != StatusNotInstalled {
		t.Fatalf("state after purge = %v (%v), want not installed", st.Status, st.Reasons)
	}
}

// discoverTdPoints runs the scenario to completion once and returns every
// lifecycle point it passed, in order.
func discoverTdPoints(t *testing.T, sc tdScenario) []string {
	t.Helper()
	r := newTdRun(t, sc.kind)
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
	res := Uninstall(context.Background(), r.env, sc.params)
	lifecycleHook = orig // restored before any parallel child runs
	assertCommitted(t, res)
	sc.afterCommit(t, r)
	return points
}

// modificationBoundary returns the index of the first point at which a
// managed path (or systemd) counts as modified, per spec §7.4: the moment
// the first systemd step is journalled `started` or the first removal step is
// done (snapshots never count).
func modificationBoundary(points []string) int {
	for i, p := range points {
		if p == "started:systemd-stop" {
			return i
		}
	}
	for i, p := range points {
		if strings.HasPrefix(p, "done:remove-") || strings.HasPrefix(p, "done:manifest-write") {
			return i
		}
	}
	return len(points)
}

func TestUninstallFaultRollback(t *testing.T) {
	setUmask077(t)
	for _, sc := range tdScenarios() {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			points := discoverTdPoints(t, sc)
			must := []string{"began", "mkdir-snap", "before-commit", "after-commit-flip"}
			if sc.kind.active() {
				must = append(must, "reg:systemd-stop", "started:systemd-stop", "called:systemd-stop", "started:systemd-disable",
					"done:systemd-disable", "reg:remove-unit", "done:remove-unit", "started:systemd-daemon-reload", "done:systemd-daemon-reload",
					"reg:remove-config", "done:remove-configdir", "done:remove-binary-1.14.1", "done:remove-versiondir-1.14.1", "done:remove-versionsdir", "done:remove-bindir")
			}
			if sc.params.Purge {
				must = append(must, "reg:remove-secrets", "done:remove-secrets", "done:remove-instance", "done:remove-manifest", "done:remove-statedir", "reg:manifest-snap")
			} else {
				must = append(must, "reg:manifest-snap", "done:manifest-snap", "reg:manifest-write", "done:manifest-write")
			}
			have := map[string]bool{}
			for _, p := range points {
				have[p] = true
			}
			for _, m := range must {
				if !have[m] {
					t.Errorf("lifecycle never reported point %q (got %v)", m, points)
				}
			}
			boundary := modificationBoundary(points)
			t.Logf("%s: %d points, modification boundary at %q", sc.name, len(points), points[boundary])

			for i, point := range points {
				i, point := i, point
				if point == "after-commit-flip" {
					continue // committed: nothing may abort forward cleanup (covered by TestUninstallCancellation)
				}
				t.Run(strings.NewReplacer(":", "_", "/", "_").Replace(point), func(t *testing.T) {
					r := newTdRun(t, sc.kind)
					fired := false
					setHook(t, func(p string) error {
						if p == point && !fired {
							fired = true
							return fmt.Errorf("injected fault at %s", p)
						}
						return nil
					})
					res := Uninstall(context.Background(), r.env, sc.params)
					if !fired {
						t.Fatalf("point %q was never reached", point)
					}
					wantExit := 4
					if i < boundary {
						wantExit = 1
					}
					if res.ExitCode() != wantExit || (wantExit == 1 && !res.PreTransaction) || (wantExit == 4 && res.Err != nil) {
						t.Fatalf("fault at %q: exit = %d (want %d), result %+v", point, res.ExitCode(), wantExit, res)
					}
					assertTreeEqual(t, r.before, treeSnapshot(t, r.roots))
					assertNoTxnArea(t, r.roots)
					if sc.kind.active() {
						assertUnitActiveEnabled(t, r.sysd)
					}
					if wantExit == 1 {
						if got := mutatingSystemdCalls(r.sysd.Calls()); len(got) != 0 {
							t.Errorf("a failure before the first modification must not touch systemd, saw %v", got)
						}
					}
					want := DetectInstanceState(r.env.FS, r.roots)
					if want.Status != StatusInstalled && sc.kind.active() {
						t.Errorf("state after the failure = %v, want the pre-transaction installed state", want.Status)
					}
					if sc.kind == tdUninstalled && want.Status != StatusUninstalledKeepIdentity {
						t.Errorf("state after the failure = %v, want uninstalled-keep-identity", want.Status)
					}
				})
			}
		})
	}
}

// A rollback re-creates removed directories with the recorded mode and owner
// even under umask 077 (spec §14.2; the engine registers each re-created
// directory's new identity before touching anything inside it).
func TestUninstallRollbackRecreatesDirectories(t *testing.T) {
	setUmask077(t)
	r := newTdRun(t, tdInstalledPrev)
	verDir := filepath.Join(r.roots.BinDir, "versions", "1.14.1")
	setHook(t, func(p string) error {
		if p == "done:remove-bindir" { // everything under the binary tree is gone by now
			return errors.New("injected fault after the private binary tree was removed")
		}
		return nil
	})
	res := Uninstall(context.Background(), r.env, UninstallParams{})
	if res.ExitCode() != 4 {
		t.Fatalf("exit = %d, want 4 (%+v)", res.ExitCode(), res)
	}
	assertTreeEqual(t, r.before, treeSnapshot(t, r.roots))
	for _, d := range []string{r.roots.BinDir, filepath.Join(r.roots.BinDir, "versions"), verDir, filepath.Join(r.roots.BinDir, "versions", "1.14.0")} {
		assertOwnerMode(t, d, 0755)
	}
	assertOwnerMode(t, r.roots.ConfigDir, 0700)
	assertOwnerMode(t, versionBinaryPath(r.roots, "1.14.1"), 0755)
}

// --- purge (spec §8.5) ---

func TestPurgeBothSources(t *testing.T) {
	setUmask077(t)
	for _, kind := range []tdKind{tdInstalled, tdInstalledPrev, tdUninstalled} {
		kind := kind
		t.Run(string(kind), func(t *testing.T) {
			r := newTdRun(t, kind)
			privateKey := r.fx.privateKey

			res := Uninstall(context.Background(), r.env, UninstallParams{Purge: true})
			assertCommitted(t, res)
			assertPurged(t, r)
			for _, p := range []string{filepath.Dir(r.roots.ConfigDir), filepath.Dir(r.roots.StateDir), filepath.Dir(r.roots.BinDir), filepath.Dir(r.roots.SystemdUnit)} {
				assertNoFileContains(t, p, privateKey)
			}
			assertBlock(t, res.Stdout, wantNameLine("remove", r.fx.name), wantNameLine("disable", r.fx.name))
			calls := runBlockInShell(t, res.Stdout)
			if len(calls) != 1 || !strings.Contains(calls[0], "node remove") {
				t.Errorf("pasting the block must run exactly the remove command, ran %v", calls)
			}
			notice := strings.Join(res.Notices, "\n")
			if !strings.Contains(notice, "proxyctl") || !strings.Contains(notice, "bootstrap") {
				t.Errorf("notices must say proxyctl and the bootstrap previous copy were kept, got %q", notice)
			}
			if kind.active() {
				if got := strings.Join(mutatingSystemdCalls(r.sysd.Calls()), ","); got != "stop,disable,daemon-reload" {
					t.Errorf("systemd call order = %s, want stop,disable,daemon-reload", got)
				}
			} else if got := mutatingSystemdCalls(r.sysd.Calls()); len(got) != 0 {
				t.Errorf("purging an uninstalled instance has no unit to stop: %v", got)
			}
			// the trusted parents themselves stay
			for _, p := range []string{filepath.Dir(r.roots.ConfigDir), filepath.Dir(r.roots.StateDir), filepath.Dir(r.roots.BinDir), filepath.Dir(r.roots.SystemdUnit)} {
				if _, err := os.Stat(p); err != nil {
					t.Errorf("trusted root %s must remain: %v", p, err)
				}
			}
		})
	}
}

// assertNoFileContains fails if any regular file under dir contains needle
// (e.g. an implicit private-key backup left behind by purge).
func assertNoFileContains(t *testing.T, dir, needle string) {
	t.Helper()
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			b, rerr := os.ReadFile(p)
			if rerr == nil && strings.Contains(string(b), needle) {
				t.Errorf("%s still contains the private key", p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
}

// --- cancellation (spec §13.3) ---

func TestUninstallCancellation(t *testing.T) {
	setUmask077(t)
	cancelAt := func(t *testing.T, kind tdKind, params UninstallParams, point string) (*tdRun, Result, context.Context) {
		r := newTdRun(t, kind)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		setHook(t, func(p string) error {
			if p == point {
				cancel()
			}
			return nil
		})
		return r, Uninstall(ctx, r.env, params), ctx
	}

	t.Run("before the first modification: exit 1, area removed, nothing changed", func(t *testing.T) {
		for _, point := range []string{"began", "mkdir-snap", "done:unit-snap"} {
			r, res, ctx := cancelAt(t, tdInstalled, UninstallParams{}, point)
			if ctx.Err() == nil {
				t.Fatalf("%s: never cancelled", point)
			}
			if res.ExitCode() != 1 || !res.PreTransaction {
				t.Fatalf("cancel at %s: want exit 1, got %+v", point, res)
			}
			assertTreeEqual(t, r.before, treeSnapshot(t, r.roots))
			assertNoTxnArea(t, r.roots)
			if got := mutatingSystemdCalls(r.sysd.Calls()); len(got) != 0 {
				t.Errorf("cancel at %s touched systemd: %v", point, got)
			}
		}
	})

	t.Run("mid-transaction: detached rollback completes, exit 4", func(t *testing.T) {
		for _, point := range []string{"done:remove-unit", "done:remove-configdir", "done:systemd-daemon-reload"} {
			r, res, ctx := cancelAt(t, tdInstalled, UninstallParams{}, point)
			if ctx.Err() == nil {
				t.Fatalf("%s: never cancelled", point)
			}
			if res.ExitCode() != 4 || res.Err != nil {
				t.Fatalf("cancel at %s: want exit 4, got %+v", point, res)
			}
			assertTreeEqual(t, r.before, treeSnapshot(t, r.roots))
			assertUnitActiveEnabled(t, r.sysd)
			assertNoTxnArea(t, r.roots)
		}
	})

	t.Run("purge mid-transaction", func(t *testing.T) {
		r, res, _ := cancelAt(t, tdInstalled, UninstallParams{Purge: true}, "done:remove-secrets")
		if res.ExitCode() != 4 {
			t.Fatalf("want exit 4, got %+v", res)
		}
		assertTreeEqual(t, r.before, treeSnapshot(t, r.roots))
		assertUnitActiveEnabled(t, r.sysd)
		assertNoTxnArea(t, r.roots)
	})

	t.Run("a hung systemctl stop cancelled by the signal is rolled back", func(t *testing.T) {
		r := newTdRun(t, tdInstalled)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r.sysd.onCall = func(name string) {
			if name == "stop" && ctx.Err() == nil {
				cancel() // the signal arrives while `systemctl stop` runs; the call sees a dead context
			}
		}
		res := Uninstall(ctx, r.env, UninstallParams{})
		if res.ExitCode() != 4 || res.Err != nil {
			t.Fatalf("want exit 4, got %+v", res)
		}
		assertTreeEqual(t, r.before, treeSnapshot(t, r.roots))
		assertUnitActiveEnabled(t, r.sysd)
		assertNoTxnArea(t, r.roots)
	})

	t.Run("committed: only moves forward, exit 0", func(t *testing.T) {
		for _, params := range []UninstallParams{{}, {Purge: true}} {
			r, res, ctx := cancelAt(t, tdInstalled, params, "after-commit-flip")
			if ctx.Err() == nil {
				t.Fatalf("never cancelled")
			}
			assertCommitted(t, res)
			assertNoTxnArea(t, r.roots)
			if params.Purge {
				assertPurged(t, r)
			} else {
				assertKeptIdentityOnly(t, r)
			}
		}
	})
}

// The rollback runs under its own 120 s budget (spec §13.2): a systemctl that
// hangs during the restoring restart ends in exit 5 with the transaction area
// preserved, and a later recover converges. The budget is injected; nothing
// waits 120 real seconds.
func TestUninstallRollbackTimeout(t *testing.T) {
	setUmask077(t)
	r := newTdRun(t, tdInstalled)
	r.env.RollbackBudget = 200 * time.Millisecond
	r.sysd.blockRestartN = 1 // the only restart of this run is the rollback's
	setHook(t, func(p string) error {
		if p == "done:remove-configdir" {
			return errors.New("injected fault")
		}
		return nil
	})
	res := Uninstall(context.Background(), r.env, UninstallParams{})
	if res.ExitCode() != 5 || res.PreTransaction || res.TxnDir != r.roots.TxnDir {
		t.Fatalf("want exit 5 naming the transaction area, got %+v", res)
	}
	if !errors.Is(res.Err, transaction.ErrRollbackTimeout) {
		t.Fatalf("want ErrRollbackTimeout, got %v", res.Err)
	}
	if _, err := os.Stat(r.roots.TxnDir); err != nil {
		t.Fatalf("the transaction area must be preserved: %v", err)
	}

	setHook(t, func(string) error { return nil })
	r.sysd.blockRestartN = 0
	r.env.RollbackBudget = 0
	rec := RunRecover(context.Background(), r.env, RecoverParams{})
	if rec.ExitCode() != 0 {
		t.Fatalf("recover after the timeout: exit = %d (%+v)", rec.ExitCode(), rec)
	}
	assertTreeEqual(t, r.before, treeSnapshot(t, r.roots))
	assertUnitActiveEnabled(t, r.sysd)
	assertNoTxnArea(t, r.roots)
}

// --- crash matrix (spec §14.2): a child process os.Exit()s at every
// lifecycle point; the parent recovers through the production `recover` path
// and compares the tree to the pre-transaction state ---

const envTdCase = "RTNODE_TD_CASE"

func tdScenarioByName(name string) (tdScenario, bool) {
	for _, s := range tdScenarios() {
		if s.name == name {
			return s, true
		}
	}
	return tdScenario{}, false
}

// TestTeardownCrashChild is the re-exec'd crashing side; a no-op in a normal
// run.
func TestTeardownCrashChild(t *testing.T) {
	name := os.Getenv(envTdCase)
	if name == "" {
		return
	}
	sc, ok := tdScenarioByName(name)
	if !ok {
		t.Fatalf("unknown scenario %q", name)
	}
	roots := rootsUnder(os.Getenv(envCrashBase))
	env, _ := buildTeardownEnv(t, roots, sc.kind, os.Getenv(envCrashSysd), true)
	point := os.Getenv(envCrashPt)
	lifecycleHook = func(p string) error {
		if p == point {
			os.Exit(crashExitCode)
		}
		return nil
	}
	res := Uninstall(context.Background(), env, sc.params)
	fmt.Fprintf(os.Stderr, "crash child finished without reaching %q: result=%+v cause=%v\n", point, res, res.Cause)
	os.Exit(doneExitCode)
}

func TestUninstallCrashMatrix(t *testing.T) {
	if os.Getenv(envTdCase) != "" {
		t.Skip("running as the crash child")
	}
	setUmask077(t)
	for _, sc := range tdScenarios() {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			points := discoverTdPoints(t, sc)
			boundary := modificationBoundary(points)
			flip := -1
			for i, p := range points {
				if p == "after-commit-flip" {
					flip = i
				}
			}
			if flip < 0 {
				t.Fatalf("no after-commit-flip point in %v", points)
			}
			t.Logf("%s: %d crash points", sc.name, len(points))
			for i, point := range points {
				i, point := i, point
				t.Run(strings.NewReplacer(":", "_", "/", "_").Replace(point), func(t *testing.T) {
					t.Parallel()
					tdCrashAndRecover(t, sc, point, i >= boundary, i >= flip)
				})
			}
		})
	}
}

var tdCrashSlots = make(chan struct{}, 4)

func tdCrashAndRecover(t *testing.T, sc tdScenario, point string, modified, committed bool) {
	tdCrashSlots <- struct{}{}
	defer func() { <-tdCrashSlots }()
	base := t.TempDir()
	roots := rootsUnder(base)
	makeRootDirs(t, roots)
	script := buildFakeSingboxScript(t, "1.14.1")
	writeTeardownFixture(t, roots, sc.kind, script)
	sysdState := filepath.Join(t.TempDir(), "sysd.json")
	env, sysd := buildTeardownEnv(t, roots, sc.kind, sysdState, false)
	before := treeSnapshot(t, roots)

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
		t.Fatalf("child exit code = %d, want %d (crash at %q)\n%s", code, crashExitCode, point, out)
	}

	rec := RunRecover(context.Background(), env, RecoverParams{})
	if rec.ExitCode() != 0 || rec.Err != nil {
		t.Fatalf("recover after a crash at %q: exit %d, %+v", point, rec.ExitCode(), rec)
	}
	assertNoTxnArea(t, roots)
	if committed {
		if !rec.Recovered || rec.RecoveredOutcome != transaction.OutcomeCommitted {
			t.Fatalf("crash at %q (after the commit flip): recovered outcome = %v, want committed", point, rec.RecoveredOutcome)
		}
		r := &tdRun{env: env, sysd: sysd, roots: roots}
		sc.afterCommit(t, r)
		return
	}
	wantOutcome := transaction.OutcomeNoChange
	if modified {
		wantOutcome = transaction.OutcomeRolledBack
	}
	if !rec.Recovered || rec.RecoveredOutcome != wantOutcome {
		t.Fatalf("crash at %q: recovered outcome = %v, want %v", point, rec.RecoveredOutcome, wantOutcome)
	}
	assertTreeEqual(t, before, treeSnapshot(t, roots))
	if sc.kind.active() {
		assertUnitActiveEnabled(t, sysd)
	}
	if !modified {
		if got := mutatingSystemdCalls(sysd.Calls()); len(got) != 0 {
			t.Errorf("crash before the first modification: recover must not touch systemd, saw %v", got)
		}
	}
	if st := DetectInstanceState(env.FS, roots).Status; sc.kind == tdUninstalled && st != StatusUninstalledKeepIdentity || sc.kind.active() && st != StatusInstalled {
		t.Errorf("state after recovery = %v, want the pre-transaction state", st)
	}
}
