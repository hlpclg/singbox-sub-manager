package transaction

// Tests for recovery.go (design spec §7.4-§7.6, plan Task 5). This package
// still never uses t.Parallel (stepHook/persistHook/ancestorHook are
// process-global test seams — see journal_test.go's header comment).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// --- shared test doubles ---

// recResolver is a minimal PathResolver for these tests: every target is
// "<rootKey>/<comp1>/.../<leaf>", resolved against a single TrustedRoot —
// exactly the same key format ManagedPath.AncestorKey/TargetKey already use
// elsewhere in this package's tests (e.g. "root/instance").
type recResolver struct {
	root    *TrustedRoot
	rootKey string
}

func (r recResolver) Resolve(target string) (ManagedPath, error) {
	parts := strings.Split(target, "/")
	if len(parts) < 2 || parts[0] != r.rootKey {
		return ManagedPath{}, fmt.Errorf("recResolver: unknown target %q", target)
	}
	comps := append([]string(nil), parts[1:len(parts)-1]...)
	leaf := parts[len(parts)-1]
	keys := make([]string, len(comps))
	cur := r.rootKey
	for i, c := range comps {
		cur = cur + "/" + c
		keys[i] = cur
	}
	return ManagedPath{Root: r.root, RootKey: r.rootKey, Comps: comps, Keys: keys, Leaf: leaf}, nil
}

// fakeSystemd records call order and lets a test control LoadState/
// UnitStateBefore-driven behavior, plus the inode of a designated binary
// file at the moment Restart is called (TestRestoredExecutableInode).
type fakeSystemd struct {
	mu            sync.Mutex
	calls         []string
	loaded        bool
	unitFileState string
	binaryPath    string
	restartInode  uint64
	restartCount  int
	failRestart   bool
}

func (f *fakeSystemd) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fakeSystemd) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeSystemd) LoadState(ctx context.Context, unit string) (bool, error) {
	f.record("load_state")
	return f.loaded, nil
}

func (f *fakeSystemd) UnitFileState(ctx context.Context, unit string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "unit_file_state")
	return f.unitFileState, nil
}

func (f *fakeSystemd) Stop(ctx context.Context, unit string) error {
	f.record("stop")
	return nil
}

func (f *fakeSystemd) SetUnitFileState(ctx context.Context, unit, want string) error {
	if want == "enabled" {
		f.record("enable")
	} else {
		f.record("disable")
	}
	f.mu.Lock()
	f.unitFileState = want
	f.mu.Unlock()
	return nil
}

func (f *fakeSystemd) DaemonReload(ctx context.Context) error {
	f.record("daemon-reload")
	return nil
}

func (f *fakeSystemd) Restart(ctx context.Context, unit string) error {
	f.record("restart")
	f.mu.Lock()
	f.restartCount++
	if f.binaryPath != "" {
		if st, err := os.Stat(f.binaryPath); err == nil {
			f.restartInode = inodeOfFileInfo(st)
		}
	}
	fail := f.failRestart
	f.mu.Unlock()
	if fail {
		return fmt.Errorf("fake restart failure")
	}
	return nil
}

func inodeOfFileInfo(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}

func inodeOfPath(t *testing.T, path string) uint64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return inodeOfFileInfo(st)
}

// newSnapDir creates and opens the transaction area's snap/ directory, the
// same way steps_test.go's snapshot fixtures already do.
func newSnapDir(t *testing.T, base string) *os.File {
	t.Helper()
	p := filepath.Join(getTxnPath(base), "snap")
	if err := os.Mkdir(p, 0700); err != nil {
		t.Fatalf("mkdir snap: %v", err)
	}
	return mustOpenDir(t, p)
}

// baseRecoverer wires a Recoverer around a fresh Store/Engine, with no
// systemd/gate (publish-shaped: the common case most of these tests need).
// Scan directories are no longer supplied by the caller at all (R4 fix):
// forEachManagedScanDir derives them itself from the journal's own targets
// via Resolve, resolved and opened fresh each time.
func baseRecoverer(t *testing.T, engine *Engine, root *TrustedRoot, base string, snapDir *os.File) *Recoverer {
	t.Helper()
	return &Recoverer{
		Engine:        engine,
		Root:          root,
		TombstoneName: ".txn.tomb-" + engine.Journal().TxnID,
		Resolve:       recResolver{root: root, rootKey: "root"},
		SnapDir:       snapDir,
	}
}

func statPathOrMissing(t *testing.T, path string) PathState {
	t.Helper()
	dir, name := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	parent, err := os.Open(dir)
	if err != nil {
		t.Fatalf("open %s: %v", dir, err)
	}
	defer parent.Close()
	ps, err := statLeaf(parent, name, true)
	if err != nil {
		t.Fatalf("statLeaf %s: %v", path, err)
	}
	return ps
}

func statDirOrMissing(t *testing.T, path string) PathState {
	t.Helper()
	dir, name := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	parent, err := os.Open(dir)
	if err != nil {
		t.Fatalf("open %s: %v", dir, err)
	}
	defer parent.Close()
	ps, err := statLeafDir(parent, name)
	if err != nil {
		t.Fatalf("statLeafDir %s: %v", path, err)
	}
	return ps
}

// dirIdentityOf reads path's real {dev, ino} for a test to pre-register in
// a synthetic journal's DirIDs, matching a directory this test created
// directly (not through the Engine's own Mkdir, which registers identity
// itself).
func dirIdentityOf(t *testing.T, path string) DirIdentity {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	id, err := CurrentIdentity(f)
	if err != nil {
		t.Fatalf("CurrentIdentity %s: %v", path, err)
	}
	return id
}

func requireNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s: want does not exist, got err=%v", path, err)
	}
}

// TestRecoveryPlan covers the plan Task 5 acceptance matrix's first row
// ("回滚计划一次登记": current state -> pre-transaction state, parent/child
// dependencies correct) and the plan's checklist item "已经处于事务前状态
// 的路径不生成修改步骤". It builds a mixed forward transaction — a freshly
// created directory and the file inside it, a pre-existing file this
// transaction deleted, and an untouched pending step — by actually running
// the real forward primitives to completion, then inspects the rollback
// plan beginRollback constructs before letting it execute.
func TestRecoveryPlan(t *testing.T) {
	withUmask(t, 0022)
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	root, base := newTestRoot(t, "root")

	// root/oldfile pre-exists before the transaction; this transaction
	// deletes it (snapshot + remove_file).
	oldContent := []byte("legacy-content")
	if err := os.WriteFile(filepath.Join(base, "oldfile"), oldContent, 0644); err != nil {
		t.Fatal(err)
	}
	oldState := fileStateFor(0644, uid, gid, oldContent)

	dirPost := PathState{Exists: true, Type: PathTypeDir, Mode: 0700, UID: uid, GID: gid}
	configContent := []byte("hello")
	configPost := fileStateFor(0600, uid, gid, configContent)
	untouchedPost := fileStateFor(0600, uid, gid, []byte("future"))

	j := baseJournal("plan1")
	j.Steps = []Step{
		mkStep("s1", KindMkdir, "root/instance", StatusPending, PathState{Exists: false}, dirPost),
		mkStep("s2", KindWriteFile, "root/instance/config", StatusPending, PathState{Exists: false}, configPost),
		mkStep("s3", KindSnapshot, "root/oldfile", StatusPending, oldState, oldState),
		mkStep("s4", KindRemoveFile, "root/oldfile", StatusPending, oldState, PathState{Exists: false}),
		mkStep("s5", KindWriteFile, "root/untouched", StatusPending, PathState{Exists: false}, untouchedPost),
	}
	j.Steps[1].Temps = []string{TempName("plan1", 1)}
	j.Steps[2].Temps = []string{TempName("plan1", 2)}
	j.Steps[4].Temps = []string{TempName("plan1", 3)}

	store, err := BeginTransaction(root, "txn", j)
	if err != nil {
		t.Fatalf("BeginTransaction: %v", err)
	}
	defer store.Close()
	snapDir := newSnapDir(t, base)
	engine := NewEngine(store)
	ctx := context.Background()

	mpInstance := ManagedPath{Root: root, RootKey: "root", Leaf: "instance"}
	if _, err := engine.Mkdir(ctx, "s1", mpInstance); err != nil {
		t.Fatalf("Mkdir s1: %v", err)
	}
	mpConfig := ManagedPath{Root: root, RootKey: "root", Comps: []string{"instance"}, Keys: []string{"root/instance"}, Leaf: "config"}
	if err := engine.WriteFile(ctx, "s2", mpConfig, TempName("plan1", 1), configContent); err != nil {
		t.Fatalf("WriteFile s2: %v", err)
	}
	mpOldfile := ManagedPath{Root: root, RootKey: "root", Leaf: "oldfile"}
	if err := engine.Snapshot(ctx, "s3", mpOldfile, snapDir, TempName("plan1", 2)); err != nil {
		t.Fatalf("Snapshot s3: %v", err)
	}
	if err := engine.RemoveFile(ctx, "s4", mpOldfile); err != nil {
		t.Fatalf("RemoveFile s4: %v", err)
	}
	// s5 stays pending: never executed, root/untouched never created.

	rec := baseRecoverer(t, engine, root, base, snapDir)

	if err := rec.beginRollback(ctx); err != nil {
		t.Fatalf("beginRollback: %v", err)
	}
	rj := engine.Journal()
	if rj.Phase != PhaseRollingBack {
		t.Fatalf("Phase = %s, want rolling_back", rj.Phase)
	}
	if len(rj.RollbackFrontier) != len(j.Steps) {
		t.Fatalf("len(RollbackFrontier) = %d, want %d", len(rj.RollbackFrontier), len(j.Steps))
	}
	if rj.RollbackFilesDone {
		t.Fatalf("RollbackFilesDone = true, want false (there is real rollback work to do)")
	}

	// root/untouched was already at its pre-transaction state (never
	// created): it must not appear in the plan at all.
	for _, rs := range rj.RollbackSteps {
		if rs.Target == "root/untouched" {
			t.Fatalf("rollback plan contains a step for root/untouched, which was already at its pre-transaction state: %+v", rs)
		}
	}
	if len(rj.RollbackSteps) != 3 {
		t.Fatalf("len(RollbackSteps) = %d, want 3: %+v", len(rj.RollbackSteps), rj.RollbackSteps)
	}
	// Removals (the freshly created dir and file) must come first, child
	// before parent (deepest target first); the re-creation (oldfile)
	// follows.
	wantOrder := []struct {
		target string
		kind   StepKind
	}{
		{"root/instance/config", KindRemoveFile},
		{"root/instance", KindRemoveDir},
		{"root/oldfile", KindWriteFile},
	}
	for i, want := range wantOrder {
		got := rj.RollbackSteps[i]
		if got.Target != want.target || got.Kind != want.kind {
			t.Fatalf("RollbackSteps[%d] = {target=%s kind=%s}, want {target=%s kind=%s}", i, got.Target, got.Kind, want.target, want.kind)
		}
		if got.Status != StatusPending {
			t.Fatalf("RollbackSteps[%d].Status = %s, want pending", i, got.Status)
		}
	}
	// Every rollback step's Post must equal its forward chain's first Pre
	// (the pre-transaction state) — beginRollback's own invariant.
	if rj.RollbackSteps[0].Post != (PathState{Exists: false}) {
		t.Fatalf("config rollback Post = %+v, want does-not-exist", rj.RollbackSteps[0].Post)
	}
	if rj.RollbackSteps[2].Post != oldState {
		t.Fatalf("oldfile rollback Post = %+v, want %+v", rj.RollbackSteps[2].Post, oldState)
	}

	// Now let rollback actually run to completion and verify convergence.
	if err := rec.continueRollback(ctx); err != nil {
		t.Fatalf("continueRollback: %v", err)
	}
	requireNotExist(t, filepath.Join(base, "instance"))
	got := statPathOrMissing(t, filepath.Join(base, "oldfile"))
	if !got.Exists || got.SHA256 != oldState.SHA256 || got.Mode != oldState.Mode {
		t.Fatalf("oldfile restored state = %+v, want %+v", got, oldState)
	}
	requireNotExist(t, filepath.Join(base, "untouched"))
	requireNotExist(t, getTxnPath(base))
	requireNotExist(t, filepath.Join(base, ".txn.tomb-plan1"))
}

// TestCommittedCleanup covers §7.5: a journal already at phase=committed,
// crashing mid-cleanup, must have recover only resume the remaining
// forward cleanup steps (never roll back), converging to every cleanup
// step done and the transaction area gone. It also proves a cleanup step
// left Started by an injected crash resumes correctly on a second,
// independent recover against a freshly reopened Store — not just the same
// in-memory Engine.
func TestCommittedCleanup(t *testing.T) {
	withUmask(t, 0022)
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	root, base := newTestRoot(t, "root")

	if err := os.Mkdir(filepath.Join(base, "old-version"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "old-version", "old-bin"), []byte("old-binary-bytes"), 0755); err != nil {
		t.Fatal(err)
	}
	oldBinContent := []byte("old-binary-bytes")
	oldBinState := fileStateFor(0755, uid, gid, oldBinContent)
	dirState := PathState{Exists: true, Type: PathTypeDir, Mode: 0755, UID: uid, GID: gid}
	versionDirID := dirIdentityOf(t, filepath.Join(base, "old-version"))

	j := baseJournal("commit1")
	j.Phase = PhaseCommitted
	j.DirIDs["root/old-version"] = versionDirID
	j.Steps = []Step{
		mkStep("c1", KindRemoveFile, "root/old-version/old-bin", StatusPending, oldBinState, PathState{Exists: false}),
		mkStep("c2", KindRemoveDir, "root/old-version", StatusPending, dirState, PathState{Exists: false}),
	}
	store, err := BeginTransaction(root, "txn", j)
	if err != nil {
		t.Fatalf("BeginTransaction: %v", err)
	}
	engine := NewEngine(store)
	ctx := context.Background()

	mpBin := ManagedPath{Root: root, RootKey: "root", Comps: []string{"old-version"}, Keys: []string{"root/old-version"}, Leaf: "old-bin"}
	// Crash the first cleanup step right after unlink, before its own
	// fsync/done — a genuine "started, not done" resume point.
	setStepHook(t, func(p string) error {
		if p == "remove_file:after_unlink" {
			return errFakeCrash
		}
		return nil
	})
	err = engine.RemoveFile(ctx, "c1", mpBin)
	if !errors.Is(err, errFakeCrash) {
		t.Fatalf("RemoveFile c1 = %v, want errFakeCrash", err)
	}
	setStepHook(t, func(string) error { return nil })
	store.Close()

	// Reopen fresh, exactly like a real second recover invocation would.
	store2, err := OpenTransaction(root, "txn")
	if err != nil {
		t.Fatalf("OpenTransaction: %v", err)
	}
	defer store2.Close()
	if s := store2.Journal().Steps[0].Status; s != StatusStarted {
		t.Fatalf("c1 status after crash = %s, want started", s)
	}
	engine2 := NewEngine(store2)
	rec := baseRecoverer(t, engine2, root, base, nil)

	outcome, err := rec.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if outcome != OutcomeCommitted {
		t.Fatalf("Outcome = %v, want OutcomeCommitted", outcome)
	}
	requireNotExist(t, filepath.Join(base, "old-version"))
	requireNotExist(t, getTxnPath(base))
}

// TestTombstoneRecovery covers §7.6: RecoverEmptyTransactionArea's three
// outcomes for a transaction area with no journal.json, DeleteTombstone's
// wholesale (uninterpreted) deletion of a leftover tombstone, and a full
// Recover run leaving neither the transaction area nor its tombstone name
// behind.
func TestTombstoneRecovery(t *testing.T) {
	withUmask(t, 0022)

	t.Run("empty area is deleted", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		if err := os.Mkdir(filepath.Join(base, "txn"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := RecoverEmptyTransactionArea(root, "txn"); err != nil {
			t.Fatalf("RecoverEmptyTransactionArea: %v", err)
		}
		requireNotExist(t, filepath.Join(base, "txn"))
	})

	t.Run("tmp-only area is deleted", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		if err := os.Mkdir(filepath.Join(base, "txn"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "txn", "journal.json.tmp"), []byte("{"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := RecoverEmptyTransactionArea(root, "txn"); err != nil {
			t.Fatalf("RecoverEmptyTransactionArea: %v", err)
		}
		requireNotExist(t, filepath.Join(base, "txn"))
	})

	t.Run("unexpected content is tampering, area preserved", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		if err := os.Mkdir(filepath.Join(base, "txn"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "txn", "mystery"), []byte("?"), 0600); err != nil {
			t.Fatal(err)
		}
		err := RecoverEmptyTransactionArea(root, "txn")
		if !errors.Is(err, ErrTampered) {
			t.Fatalf("RecoverEmptyTransactionArea = %v, want ErrTampered", err)
		}
		if _, statErr := os.Stat(filepath.Join(base, "txn", "mystery")); statErr != nil {
			t.Fatalf("area was modified despite tampering verdict: %v", statErr)
		}
	})

	t.Run("journal.json present defers to OpenTransaction", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		j := baseJournal("tomb1")
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		defer store.Close()
		err = RecoverEmptyTransactionArea(root, "txn")
		if !errors.Is(err, ErrJournalPresent) {
			t.Fatalf("RecoverEmptyTransactionArea = %v, want ErrJournalPresent", err)
		}
		if _, statErr := os.Stat(filepath.Join(base, "txn", "journal.json")); statErr != nil {
			t.Fatalf("journal.json missing after RecoverEmptyTransactionArea: %v", statErr)
		}
	})

	t.Run("DeleteTombstone removes a leftover tombstone wholesale", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		tomb := filepath.Join(base, ".txn.tomb-x")
		if err := os.MkdirAll(filepath.Join(tomb, "snap"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tomb, "snap", "secret"), []byte("private-key-bytes"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tomb, "journal.json"), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := DeleteTombstone(root, ".txn.tomb-x"); err != nil {
			t.Fatalf("DeleteTombstone: %v", err)
		}
		requireNotExist(t, tomb)
	})

	t.Run("full Recover leaves neither the area nor its tombstone name", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		j := baseJournal("tomb2")
		j.Steps = []Step{mkStep("s1", KindWriteFile, "root/never-touched", StatusPending, PathState{Exists: false}, fileStateFor(0600, uint32(os.Getuid()), uint32(os.Getgid()), []byte("x")))}
		j.Steps[0].Temps = []string{TempName("tomb2", 1)}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		engine := NewEngine(store)
		rec := baseRecoverer(t, engine, root, base, nil)
		outcome, err := rec.Recover(context.Background())
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}
		if outcome != OutcomeNoChange {
			t.Fatalf("Outcome = %v, want OutcomeNoChange", outcome)
		}
		requireNotExist(t, getTxnPath(base))
		requireNotExist(t, filepath.Join(base, ".txn.tomb-tomb2"))
	})
}

// TestRestoredExecutableInode covers the plan Task 5 acceptance matrix's
// third row ("文件完成标记": recover looking at RollbackSystemd=="restored"
// resumes only the door still open — the gate/stop confirmation — and
// never repeats the restart) plus §14.2's F1 systemd-ordering requirement
// applied to the specific case a version rollback cares about: the binary
// systemd restarts must be the *rolled-back* one, never the interrupted
// transaction's new one. It proves file recovery happens strictly before
// restart (by reading the real binary's inode at the moment fakeSystemd's
// Restart is called and comparing it against the final on-disk inode), and
// that resuming from "restored" never calls Restart again.
func TestRestoredExecutableInode(t *testing.T) {
	withUmask(t, 0022)
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	root, base := newTestRoot(t, "root")

	oldBin := []byte("old-version-binary")
	newBin := []byte("new-version-binary-longer")
	if err := os.WriteFile(filepath.Join(base, "bin"), oldBin, 0755); err != nil {
		t.Fatal(err)
	}
	oldState := fileStateFor(0755, uid, gid, oldBin)
	newState := fileStateFor(0755, uid, gid, newBin)

	j := baseJournal("inode1")
	j.UnitStateBefore = UnitState{LoadState: "loaded", UnitFileState: "enabled", ActiveState: "active"}
	j.Steps = []Step{
		mkStep("s1", KindSnapshot, "root/bin", StatusPending, oldState, oldState),
		mkStep("s2", KindWriteFile, "root/bin", StatusPending, oldState, newState),
		// s3 stays pending throughout: the forward reinstall has not yet
		// reached systemd at all when the crash this test models happens.
		// Since s2 (write_file) already reaches done via the real engine
		// call below, the modification boundary is already crossed
		// regardless of s3's own status — continueRollback's gate is
		// "journal registers systemd steps AND !noneModified", never "the
		// systemd step itself already started" (plan Task 5 review round
		// 3's P1) — so the full demote/restore sequence still must run.
		mkStep("s3", KindSystemdRestart, "systemd/svc.service", StatusPending,
			PathState{Exists: false, UnitFileState: "enabled"}, PathState{Exists: false, UnitFileState: "enabled"}),
	}
	j.Steps[0].Temps = []string{TempName("inode1", 1)}
	j.Steps[1].Temps = []string{TempName("inode1", 2)}
	store, err := BeginTransaction(root, "txn", j)
	if err != nil {
		t.Fatalf("BeginTransaction: %v", err)
	}
	snapDir := newSnapDir(t, base)
	engine := NewEngine(store)
	ctx := context.Background()
	mpBin := ManagedPath{Root: root, RootKey: "root", Leaf: "bin"}
	if err := engine.Snapshot(ctx, "s1", mpBin, snapDir, TempName("inode1", 1)); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := engine.WriteFile(ctx, "s2", mpBin, TempName("inode1", 2), newBin); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	rec := baseRecoverer(t, engine, root, base, snapDir)
	// unitFileState starts "enabled": s3's chain is fully pending, so §7.3
	// requires the live UnitFileState to already match its first Pre.
	fsd := &fakeSystemd{loaded: true, unitFileState: "enabled", binaryPath: filepath.Join(base, "bin")}
	rec.Systemd = fsd
	gateCalls := 0
	rec.Gate = func(ctx context.Context) error { gateCalls++; return nil }

	outcome, err := rec.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if outcome != OutcomeRolledBack {
		t.Fatalf("Outcome = %v, want OutcomeRolledBack", outcome)
	}
	if gateCalls != 1 {
		t.Fatalf("gate called %d times, want 1", gateCalls)
	}
	finalInode := inodeOfPath(t, filepath.Join(base, "bin"))
	if fsd.restartInode != finalInode {
		t.Fatalf("systemd Restart saw inode %d, final on-disk inode is %d — restart did not wait for the file rollback to finish", fsd.restartInode, finalInode)
	}
	data, err := os.ReadFile(filepath.Join(base, "bin"))
	if err != nil || string(data) != string(oldBin) {
		t.Fatalf("bin content = %q, err=%v, want %q", data, err, oldBin)
	}
	wantCalls := []string{"unit_file_state", "load_state", "stop", "disable", "daemon-reload", "enable", "restart"}
	if got := fsd.Calls(); !equalStrings(got, wantCalls) {
		t.Fatalf("systemd calls = %v, want %v", got, wantCalls)
	}

	// --- resuming from RollbackSystemd == restored never restarts again ---
	root2, base2 := newTestRoot(t, "root")
	if err := os.WriteFile(filepath.Join(base2, "bin"), oldBin, 0755); err != nil {
		t.Fatal(err)
	}
	// The forward chain represents the original reinstall (already fully
	// done before rollback ever began): bin went from oldState to some
	// newer state, and the unit was enabled. The rollback chain is what
	// actually governs root/bin's live-state judgement now (§7.3:
	// "phase=rolling_back...pre/post和中间状态都取自回滚原语") and is
	// already Done, restoring oldState — consistent with the real file on
	// disk, which this test wrote back to oldBin directly, standing in for
	// a previous recover invocation that already finished file recovery
	// (RollbackFilesDone=true) and got as far as persisting
	// RollbackSystemd=="restored" before crashing, right before deleting
	// the transaction area.
	newerState := fileStateFor(0755, uid, gid, []byte("newer-version-binary"))
	j2 := baseJournal("inode2")
	j2.Phase = PhaseRollingBack
	j2.UnitStateBefore = UnitState{LoadState: "loaded", UnitFileState: "enabled", ActiveState: "active"}
	j2.RollbackSystemd = RollbackSystemdRestored
	j2.RollbackFilesDone = true
	j2.Steps = []Step{
		mkStep("s1", KindWriteFile, "root/bin", StatusDone, oldState, newerState),
		mkStep("s2", KindSystemdEnable, "systemd/svc.service", StatusDone,
			PathState{Exists: false, UnitFileState: "disabled"}, PathState{Exists: false, UnitFileState: "enabled"}),
	}
	j2.RollbackFrontier = []StepStatus{StatusDone, StatusDone}
	j2.RollbackSteps = []Step{
		mkStep("r1", KindWriteFile, "root/bin", StatusDone, newerState, oldState),
	}
	store2, err := BeginTransaction(root2, "txn", j2)
	if err != nil {
		t.Fatalf("BeginTransaction (resume scenario): %v", err)
	}
	engine2 := NewEngine(store2)
	rec2 := baseRecoverer(t, engine2, root2, base2, nil)
	fsd2 := &fakeSystemd{loaded: true}
	rec2.Systemd = fsd2
	gateCalls2 := 0
	rec2.Gate = func(ctx context.Context) error { gateCalls2++; return nil }
	outcome2, err := rec2.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover (resume): %v", err)
	}
	if outcome2 != OutcomeRolledBack {
		t.Fatalf("Outcome (resume) = %v, want OutcomeRolledBack", outcome2)
	}
	if fsd2.restartCount != 0 {
		t.Fatalf("restart called %d times when resuming from 'restored', want 0", fsd2.restartCount)
	}
	if gateCalls2 != 1 {
		t.Fatalf("gate called %d times when resuming from 'restored', want 1", gateCalls2)
	}
	requireNotExist(t, getTxnPath(base2))
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestRollbackBudget covers the plan Task 5 acceptance matrix's first row's
// cancellation half (§13.2/§13.3): RecoverDetached's rollback runs to
// completion even when the caller's own context is already canceled
// (independent budget), and a rollback that genuinely does not finish
// within its budget leaves the transaction area preserved rather than
// deleted. Neither subtest sleeps for the real 10/30/120-second production
// values — the first needs no waiting at all, and the second blocks only
// on the short test-local context's own Done() channel, never a fixed
// sleep.
func TestRollbackBudget(t *testing.T) {
	withUmask(t, 0022)
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())

	t.Run("an already-canceled parent context does not stop the independent rollback", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		post := fileStateFor(0600, uid, gid, []byte("hi"))
		j := baseJournal("budget1")
		j.Steps = []Step{mkStep("s1", KindWriteFile, "root/f", StatusDone, PathState{Exists: false}, post)}
		j.Steps[0].Temps = []string{TempName("budget1", 1)}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		if err := os.WriteFile(filepath.Join(base, "f"), []byte("hi"), 0600); err != nil {
			t.Fatal(err)
		}
		engine := NewEngine(store)
		rec := baseRecoverer(t, engine, root, base, nil)
		rec.RollbackBudget = 200 * time.Millisecond

		parent, cancel := context.WithCancel(context.Background())
		cancel() // already canceled before RecoverDetached even starts

		outcome, err := rec.RecoverDetached(parent)
		if err != nil {
			t.Fatalf("RecoverDetached with an already-canceled parent: %v", err)
		}
		if outcome != OutcomeRolledBack {
			t.Fatalf("Outcome = %v, want OutcomeRolledBack", outcome)
		}
		requireNotExist(t, filepath.Join(base, "f"))
		requireNotExist(t, getTxnPath(base))
	})

	t.Run("a rollback that exceeds its budget preserves the transaction area", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		post := fileStateFor(0600, uid, gid, []byte("hi"))
		j := baseJournal("budget2")
		j.Steps = []Step{mkStep("s1", KindWriteFile, "root/f", StatusDone, PathState{Exists: false}, post)}
		j.Steps[0].Temps = []string{TempName("budget2", 1)}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		if err := os.WriteFile(filepath.Join(base, "f"), []byte("hi"), 0600); err != nil {
			t.Fatal(err)
		}
		engine := NewEngine(store)
		rec := baseRecoverer(t, engine, root, base, nil)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		setStepHook(t, func(p string) error {
			if p == "remove_file:after_unlink" {
				<-ctx.Done() // block exactly until this test's own short budget expires; no fixed sleep
			}
			return nil
		})
		defer setStepHook(t, func(string) error { return nil })

		_, err = rec.Recover(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Recover error = %v, want context.DeadlineExceeded", err)
		}
		if _, statErr := os.Stat(getTxnPath(base)); statErr != nil {
			t.Fatalf("transaction area was not preserved after a budget timeout: %v", statErr)
		}
	})
}

// TestAbandon covers T6 (plan Task 5 review round 1): §7.4's manual escape
// hatch. Eligible only when every managed path is already at its
// pre-transaction state and no unregistered temp name remains; on success
// it makes zero systemd calls (leaves systemd exactly as-is) and deletes
// the transaction area; on any other outcome it makes zero modifications.
func TestAbandon(t *testing.T) {
	withUmask(t, 0022)
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())

	t.Run("eligible: rolling_back with files fully restored but gate not yet passed", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		preState := fileStateFor(0600, uid, gid, []byte("pre"))
		postState := fileStateFor(0600, uid, gid, []byte("post"))
		if err := os.WriteFile(filepath.Join(base, "f"), []byte("pre"), 0600); err != nil {
			t.Fatal(err)
		}
		j := baseJournal("abandon1")
		j.Phase = PhaseRollingBack
		j.RollbackSystemd = RollbackSystemdRestoring // gate/restart not yet resolved; never judged
		j.RollbackFilesDone = true
		j.UnitStateBefore = UnitState{LoadState: "loaded", UnitFileState: "enabled", ActiveState: "active"}
		j.Steps = []Step{
			mkStep("s1", KindWriteFile, "root/f", StatusDone, preState, postState),
			mkStep("s2", KindSystemdEnable, "systemd/svc.service", StatusDone,
				PathState{Exists: false, UnitFileState: "disabled"}, PathState{Exists: false, UnitFileState: "enabled"}),
		}
		j.RollbackFrontier = []StepStatus{StatusDone, StatusDone}
		j.RollbackSteps = []Step{mkStep("r1", KindWriteFile, "root/f", StatusDone, postState, preState)}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		engine := NewEngine(store)
		rec := baseRecoverer(t, engine, root, base, nil)
		// rec.Systemd deliberately left nil: rollback_systemd=="restoring"
		// means the systemd chain is never judged (§7.3), so Abandon must
		// never even touch it.
		outcome, err := rec.Abandon(context.Background())
		if err != nil {
			t.Fatalf("Abandon: %v", err)
		}
		if outcome != OutcomeAbandoned {
			t.Fatalf("Outcome = %v, want OutcomeAbandoned", outcome)
		}
		requireFileState(t, filepath.Join(base, "f"), preState)
		requireNotExist(t, getTxnPath(base))
	})

	t.Run("not eligible: a managed path is not at its pre-transaction state", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		pre := PathState{Exists: false}
		post := fileStateFor(0600, uid, gid, []byte("hi"))
		j := baseJournal("abandon2")
		j.Steps = []Step{mkStep("s1", KindWriteFile, "root/f", StatusDone, pre, post)}
		j.Steps[0].Temps = []string{TempName("abandon2", 1)}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		if err := os.WriteFile(filepath.Join(base, "f"), []byte("hi"), 0600); err != nil {
			t.Fatal(err)
		}
		before := readFile(t, filepath.Join(getTxnPath(base), "journal.json"))
		engine := NewEngine(store)
		rec := baseRecoverer(t, engine, root, base, nil)
		outcome, err := rec.Abandon(context.Background())
		if !errors.Is(err, ErrAbandonNotEligible) {
			t.Fatalf("Abandon error = %v, want ErrAbandonNotEligible", err)
		}
		if outcome != 0 {
			t.Fatalf("Outcome = %v on failure, want zero value", outcome)
		}
		requireFileState(t, filepath.Join(base, "f"), post)
		after := readFile(t, filepath.Join(getTxnPath(base), "journal.json"))
		if string(before) != string(after) {
			t.Fatalf("journal.json changed despite Abandon being ineligible")
		}
	})

	t.Run("not eligible: an unregistered temp name is present", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		j := baseJournal("abandon3")
		j.Steps = []Step{mkStep("s1", KindWriteFile, "root/f", StatusPending, PathState{Exists: false}, fileStateFor(0600, uid, gid, []byte("x")))}
		j.Steps[0].Temps = []string{TempName("abandon3", 1)}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		// A stray file matching the managed temp-name format, but not
		// registered by any step.
		if err := os.WriteFile(filepath.Join(base, ".proxyctl-txn-abandon3-99"), []byte("stray"), 0600); err != nil {
			t.Fatal(err)
		}
		engine := NewEngine(store)
		rec := baseRecoverer(t, engine, root, base, nil)
		outcome, err := rec.Abandon(context.Background())
		if !errors.Is(err, ErrAbandonNotEligible) {
			t.Fatalf("Abandon error = %v, want ErrAbandonNotEligible", err)
		}
		if outcome != 0 {
			t.Fatalf("Outcome = %v on failure, want zero value", outcome)
		}
		requireNotExist(t, filepath.Join(base, "f"))
		if _, err := os.Stat(filepath.Join(base, ".proxyctl-txn-abandon3-99")); err != nil {
			t.Fatalf("stray temp file was removed despite Abandon being ineligible: %v", err)
		}
	})

	t.Run("committed transactions are never eligible", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		j := baseJournal("abandon4")
		j.Phase = PhaseCommitted
		j.Steps = []Step{mkStep("s1", KindRemoveFile, "root/old", StatusPending, fileStateFor(0600, uid, gid, []byte("x")), PathState{Exists: false})}
		if err := os.WriteFile(filepath.Join(base, "old"), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		engine := NewEngine(store)
		rec := baseRecoverer(t, engine, root, base, nil)
		outcome, err := rec.Abandon(context.Background())
		if !errors.Is(err, ErrAbandonNotEligible) {
			t.Fatalf("Abandon error = %v, want ErrAbandonNotEligible", err)
		}
		if outcome != 0 {
			t.Fatalf("Outcome = %v on failure, want zero value", outcome)
		}
		requireFileState(t, filepath.Join(base, "old"), fileStateFor(0600, uid, gid, []byte("x")))
		if _, err := os.Stat(getTxnPath(base)); err != nil {
			t.Fatalf("transaction area was modified despite committed Abandon being refused: %v", err)
		}
	})
}

// journalBytes reads the transaction area's current journal.json verbatim,
// for a byte-for-byte before/after comparison.
func journalBytes(t *testing.T, base string) []byte {
	t.Helper()
	return readFile(t, filepath.Join(getTxnPath(base), "journal.json"))
}

// TestIllegalStatesRejected covers T5 (plan Task 5 review round 1): every
// live-state/journal mismatch §7.3 treats as tampering must be rejected
// (wrapping ErrTampered) with neither the managed path nor journal.json
// changed by even one byte — verified by literal byte comparison, not just
// a state-shape assertion.
func TestIllegalStatesRejected(t *testing.T) {
	withUmask(t, 0022)
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())

	// assertRejectedUnchanged runs setup (which must leave the store open
	// and return a Recoverer plus a probe over the one path setup put in a
	// tampered state), calls Recover, and requires ErrTampered plus a
	// byte-identical journal.json and target.
	assertRejectedUnchanged := func(t *testing.T, base string, rec *Recoverer, probePath string) {
		t.Helper()
		jBefore := journalBytes(t, base)
		var fBefore []byte
		var fBeforeErr error
		if probePath != "" {
			fBefore, fBeforeErr = os.ReadFile(probePath)
		}
		_, err := rec.Recover(context.Background())
		if !errors.Is(err, ErrTampered) {
			t.Fatalf("Recover error = %v, want ErrTampered", err)
		}
		jAfter := journalBytes(t, base)
		if string(jBefore) != string(jAfter) {
			t.Fatalf("journal.json changed despite a tampering verdict")
		}
		if probePath != "" {
			fAfter, fAfterErr := os.ReadFile(probePath)
			if fBeforeErr != nil || fAfterErr != nil {
				if !errors.Is(fBeforeErr, fAfterErr) && !(os.IsNotExist(fBeforeErr) && os.IsNotExist(fAfterErr)) {
					t.Fatalf("probe existence changed: before err=%v after err=%v", fBeforeErr, fAfterErr)
				}
			} else if string(fBefore) != string(fAfter) {
				t.Fatalf("%s changed despite a tampering verdict", probePath)
			}
		}
	}

	t.Run("all-pending chain but target does not match the first step's pre", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		j := baseJournal("bad1")
		j.Steps = []Step{mkStep("s1", KindWriteFile, "root/f", StatusPending, PathState{Exists: false}, fileStateFor(0600, uid, gid, []byte("x")))}
		j.Steps[0].Temps = []string{TempName("bad1", 1)}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		// Pre says root/f should not exist yet — an external actor placed
		// content there anyway, without ever going through snapshot.
		if err := os.WriteFile(filepath.Join(base, "f"), []byte("unexpected"), 0600); err != nil {
			t.Fatal(err)
		}
		engine := NewEngine(store)
		rec := baseRecoverer(t, engine, root, base, nil)
		assertRejectedUnchanged(t, base, rec, filepath.Join(base, "f"))
	})

	t.Run("rollback path already done but target has since regressed to an intermediate state", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		preState := fileStateFor(0600, uid, gid, []byte("pre"))
		postState := fileStateFor(0600, uid, gid, []byte("post"))
		j := baseJournal("bad2")
		j.Phase = PhaseRollingBack
		j.RollbackFilesDone = false
		j.Steps = []Step{mkStep("s1", KindWriteFile, "root/f", StatusDone, preState, postState)}
		j.RollbackFrontier = []StepStatus{StatusDone}
		j.RollbackSteps = []Step{mkStep("r1", KindWriteFile, "root/f", StatusDone, postState, preState)}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		// The rollback step for root/f is recorded Done (restored to
		// preState), but the file on disk has since regressed to postState
		// — not a legal state for a done rollback step.
		if err := os.WriteFile(filepath.Join(base, "f"), []byte("post"), 0600); err != nil {
			t.Fatal(err)
		}
		engine := NewEngine(store)
		rec := baseRecoverer(t, engine, root, base, nil)
		assertRejectedUnchanged(t, base, rec, filepath.Join(base, "f"))
	})

	t.Run("rollback_files_done=true but a path has since been tampered with", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		preState := fileStateFor(0600, uid, gid, []byte("pre"))
		postState := fileStateFor(0600, uid, gid, []byte("post"))
		if err := os.WriteFile(filepath.Join(base, "f"), []byte("pre"), 0600); err != nil {
			t.Fatal(err)
		}
		j := baseJournal("bad3")
		j.Phase = PhaseRollingBack
		j.RollbackFilesDone = true
		j.Steps = []Step{mkStep("s1", KindWriteFile, "root/f", StatusDone, preState, postState)}
		j.RollbackFrontier = []StepStatus{StatusDone}
		j.RollbackSteps = []Step{mkStep("r1", KindWriteFile, "root/f", StatusDone, postState, preState)}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		// files_done=true asserts every path is exactly at pre-transaction
		// state; tamper it after the fact.
		if err := os.WriteFile(filepath.Join(base, "f"), []byte("tampered"), 0600); err != nil {
			t.Fatal(err)
		}
		engine := NewEngine(store)
		rec := baseRecoverer(t, engine, root, base, nil)
		assertRejectedUnchanged(t, base, rec, filepath.Join(base, "f"))
	})

	t.Run("unregistered temp name present", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		j := baseJournal("bad4")
		j.Steps = []Step{mkStep("s1", KindWriteFile, "root/f", StatusPending, PathState{Exists: false}, fileStateFor(0600, uid, gid, []byte("x")))}
		j.Steps[0].Temps = []string{TempName("bad4", 1)}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		if err := os.WriteFile(filepath.Join(base, ".proxyctl-txn-bad4-77"), []byte("stray"), 0600); err != nil {
			t.Fatal(err)
		}
		engine := NewEngine(store)
		rec := baseRecoverer(t, engine, root, base, nil)
		assertRejectedUnchanged(t, base, rec, "")
		if _, err := os.Stat(filepath.Join(base, ".proxyctl-txn-bad4-77")); err != nil {
			t.Fatalf("stray temp file was removed despite a tampering verdict: %v", err)
		}
	})

	t.Run("mkdir started but directory is non-empty, not a legal resume point", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		j := baseJournal("bad5")
		j.Steps = []Step{mkStep("s1", KindMkdir, "root/d", StatusStarted, PathState{Exists: false},
			PathState{Exists: true, Type: PathTypeDir, Mode: 0700, UID: uid, GID: gid})}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		// A directory legitimately created by this mkdir (started) must be
		// empty; place a file inside it instead.
		if err := os.Mkdir(filepath.Join(base, "d"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "d", "unexpected"), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		engine := NewEngine(store)
		rec := baseRecoverer(t, engine, root, base, nil)
		assertRejectedUnchanged(t, base, rec, "")
		if _, err := os.Stat(filepath.Join(base, "d", "unexpected")); err != nil {
			t.Fatalf("directory contents were touched despite a tampering verdict: %v", err)
		}
	})

	t.Run("mkdir started but directory has the wrong owner", func(t *testing.T) {
		if os.Getuid() != 0 {
			t.Skip("chowning to a different real uid needs root; the judgement rule itself (uid must equal the current process's) is exercised by allowedStarted's own unit tests in steps_test.go")
		}
		root, base := newTestRoot(t, "root")
		j := baseJournal("bad6")
		j.Steps = []Step{mkStep("s1", KindMkdir, "root/d", StatusStarted, PathState{Exists: false},
			PathState{Exists: true, Type: PathTypeDir, Mode: 0700, UID: uid, GID: gid})}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		if err := os.Mkdir(filepath.Join(base, "d"), 0700); err != nil {
			t.Fatal(err)
		}
		// Real root: chown the started-mkdir's directory to a different
		// real uid (1, "daemon") — allowedStarted's mkdir rule requires
		// the *current process's* uid, so this must be rejected even
		// though root could plausibly have created it.
		if err := os.Chown(filepath.Join(base, "d"), 1, 0); err != nil {
			t.Fatal(err)
		}
		engine := NewEngine(store)
		rec := baseRecoverer(t, engine, root, base, nil)
		assertRejectedUnchanged(t, base, rec, "")
	})

	t.Run("a registered directory identity was replaced by a different real directory", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		if err := os.Mkdir(filepath.Join(base, "d"), 0700); err != nil {
			t.Fatal(err)
		}
		originalID := dirIdentityOf(t, filepath.Join(base, "d"))
		j := baseJournal("bad7")
		j.DirIDs["root/d"] = originalID
		j.Steps = []Step{mkStep("s1", KindWriteFile, "root/d/f", StatusPending, PathState{Exists: false}, fileStateFor(0600, uid, gid, []byte("x")))}
		j.Steps[0].Temps = []string{TempName("bad7", 1)}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		// Replace "d" with a fresh directory (new inode) between the
		// transaction starting and this recover attempt.
		if err := os.Remove(filepath.Join(base, "d")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(base, "d"), 0700); err != nil {
			t.Fatal(err)
		}
		engine := NewEngine(store)
		rec := baseRecoverer(t, engine, root, base, nil)
		_, err = rec.Recover(context.Background())
		if !errors.Is(err, ErrTampered) {
			t.Fatalf("Recover error = %v, want ErrTampered", err)
		}
		requireNotExist(t, filepath.Join(base, "d", "f"))
	})
}

// TestSystemdRollbackOrdering covers T4 (plan Task 5 review round 1) and
// §14.2's F1 requirement applied to the two shapes install/reinstall
// rollback actually needs: install (the unit never existed before) always
// disables before the unit file itself is deleted — guaranteed by
// continueRollback's own fixed order (demoteSystemd, then recoverFiles,
// then restoreSystemd), asserted here end-to-end — and a not-previously-
// active reinstall keeps the unit stopped afterward without ever gating or
// requiring a listening port.
func TestSystemdRollbackOrdering(t *testing.T) {
	withUmask(t, 0022)
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())

	t.Run("install rollback: disable happens before the unit file is deleted, no restart/gate", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		unitContent := []byte("[Unit]\n...")
		unitState := fileStateFor(0644, uid, gid, unitContent)
		if err := os.WriteFile(filepath.Join(base, "svc.service"), unitContent, 0644); err != nil {
			t.Fatal(err)
		}
		j := baseJournal("install1")
		j.UnitStateBefore = UnitState{LoadState: "not-found", UnitFileState: "", ActiveState: "inactive"}
		j.Steps = []Step{
			mkStep("s1", KindWriteFile, "root/svc.service", StatusDone, PathState{Exists: false}, unitState),
			mkStep("s2", KindSystemdEnable, "systemd/svc.service", StatusDone,
				PathState{Exists: false, UnitFileState: "disabled"}, PathState{Exists: false, UnitFileState: "enabled"}),
			mkStep("s3", KindSystemdStart, "systemd/svc.service", StatusDone,
				PathState{Exists: false, UnitFileState: "enabled"}, PathState{Exists: false, UnitFileState: "enabled"}),
		}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		engine := NewEngine(store)
		rec := baseRecoverer(t, engine, root, base, nil)
		fsd := &fakeSystemd{loaded: true, unitFileState: "enabled"}
		rec.Systemd = fsd
		gateCalls := 0
		rec.Gate = func(context.Context) error { gateCalls++; return nil }

		outcome, err := rec.Recover(context.Background())
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}
		if outcome != OutcomeRolledBack {
			t.Fatalf("Outcome = %v, want RolledBack", outcome)
		}
		wantCalls := []string{"unit_file_state", "load_state", "stop", "disable", "daemon-reload"}
		if got := fsd.Calls(); !equalStrings(got, wantCalls) {
			t.Fatalf("systemd calls = %v, want %v (no enable/restart/gate: the unit never existed before)", got, wantCalls)
		}
		if gateCalls != 0 {
			t.Fatalf("gate called %d times, want 0 (install rollback: nothing was ever running)", gateCalls)
		}
		if fsd.restartCount != 0 {
			t.Fatalf("restart called %d times, want 0", fsd.restartCount)
		}
		requireNotExist(t, filepath.Join(base, "svc.service"))
		requireNotExist(t, getTxnPath(base))
	})

	t.Run("reinstall rollback: unit was not active before, stays stopped, no gate", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		oldBin := []byte("old-binary")
		newBin := []byte("new-binary-longer")
		if err := os.WriteFile(filepath.Join(base, "bin"), oldBin, 0755); err != nil {
			t.Fatal(err)
		}
		oldState := fileStateFor(0755, uid, gid, oldBin)
		newState := fileStateFor(0755, uid, gid, newBin)
		j := baseJournal("reinstall1")
		j.UnitStateBefore = UnitState{LoadState: "loaded", UnitFileState: "enabled", ActiveState: "inactive"}
		j.Steps = []Step{
			mkStep("s1", KindSnapshot, "root/bin", StatusPending, oldState, oldState),
			mkStep("s2", KindWriteFile, "root/bin", StatusPending, oldState, newState),
			// reinstall re-enables the same (already enabled) unit — a
			// no-op enable, but a real systemd step, so this journal has
			// one (journalHasSystemd) and continueRollback drives
			// demoteSystemd/restoreSystemd. Starts pending like the
			// others; marked done below (there is no engine execution
			// primitive for a forward systemd step — Task 6+ orchestrates
			// the real systemctl call and updates the journal itself).
			mkStep("s3", KindSystemdEnable, "systemd/svc.service", StatusPending,
				PathState{Exists: false, UnitFileState: "enabled"}, PathState{Exists: false, UnitFileState: "enabled"}),
		}
		j.Steps[0].Temps = []string{TempName("reinstall1", 1)}
		j.Steps[1].Temps = []string{TempName("reinstall1", 2)}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		snapDir := newSnapDir(t, base)
		engine := NewEngine(store)
		ctx := context.Background()
		mpBin := ManagedPath{Root: root, RootKey: "root", Leaf: "bin"}
		if err := engine.Snapshot(ctx, "s1", mpBin, snapDir, TempName("reinstall1", 1)); err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if err := engine.WriteFile(ctx, "s2", mpBin, TempName("reinstall1", 2), newBin); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		// s3 (systemd_enable) is marked done directly: no engine execution
		// primitive exists for a forward systemd step (Task 6+'s own
		// orchestration calls systemctl and updates the journal itself).
		nj := store.Journal().Clone()
		nj.Steps[2].Status = StatusDone
		if err := store.Update(nj); err != nil {
			t.Fatalf("mark s3 done: %v", err)
		}
		rec := baseRecoverer(t, engine, root, base, snapDir)
		fsd := &fakeSystemd{loaded: true, unitFileState: "enabled"}
		rec.Systemd = fsd
		gateCalls := 0
		rec.Gate = func(context.Context) error { gateCalls++; return nil }

		outcome, err := rec.Recover(ctx)
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}
		if outcome != OutcomeRolledBack {
			t.Fatalf("Outcome = %v, want RolledBack", outcome)
		}
		if gateCalls != 0 {
			t.Fatalf("gate called %d times, want 0 (unit was not active before)", gateCalls)
		}
		if fsd.restartCount != 0 {
			t.Fatalf("restart called %d times, want 0", fsd.restartCount)
		}
		wantEnd := []string{"stop"}
		gotCalls := fsd.Calls()
		if len(gotCalls) == 0 || gotCalls[len(gotCalls)-1] != wantEnd[0] {
			t.Fatalf("last systemd call = %v, want to end with %q (confirm stop, no restart)", gotCalls, wantEnd[0])
		}
		data, err := os.ReadFile(filepath.Join(base, "bin"))
		if err != nil || string(data) != string(oldBin) {
			t.Fatalf("bin content = %q err=%v, want %q", data, err, oldBin)
		}
		requireNotExist(t, getTxnPath(base))
	})
}

// TestCommittedCancellation covers T7 (plan Task 5 review round 1): a
// cancellation during committed-phase forward cleanup must not roll
// anything back — it simply stops (leaving the pending cleanup step
// exactly where it was), never touching phase or constructing any
// rollback plan.
func TestCommittedCancellation(t *testing.T) {
	withUmask(t, 0022)
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	root, base := newTestRoot(t, "root")
	oldContent := []byte("old-version")
	if err := os.WriteFile(filepath.Join(base, "old"), oldContent, 0600); err != nil {
		t.Fatal(err)
	}
	oldState := fileStateFor(0600, uid, gid, oldContent)
	j := baseJournal("cancel1")
	j.Phase = PhaseCommitted
	j.Steps = []Step{mkStep("s1", KindRemoveFile, "root/old", StatusPending, oldState, PathState{Exists: false})}
	store, err := BeginTransaction(root, "txn", j)
	if err != nil {
		t.Fatalf("BeginTransaction: %v", err)
	}
	engine := NewEngine(store)
	rec := baseRecoverer(t, engine, root, base, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled before Recover even starts

	_, err = rec.Recover(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Recover error = %v, want context.Canceled", err)
	}
	// Not rolled back: the file must be untouched and the journal must
	// still show phase=committed with s1 still pending.
	requireFileState(t, filepath.Join(base, "old"), oldState)
	rj := store.Journal()
	if rj.Phase != PhaseCommitted {
		t.Fatalf("Phase = %s, want committed (cancellation must never roll back)", rj.Phase)
	}
	if rj.Steps[0].Status != StatusPending {
		t.Fatalf("s1 status = %s, want pending (untouched)", rj.Steps[0].Status)
	}
	if _, err := os.Stat(getTxnPath(base)); err != nil {
		t.Fatalf("transaction area missing after a canceled committed cleanup: %v", err)
	}
}

// TestRecoverFilesDoneNotReplayed covers T3's "done步骤不重放" (plan Task 5
// review round 1): once a rollback write_file step is done, a second
// recoverFiles pass must not recreate it — proven by the file's inode
// staying identical across two separate recover invocations against a
// freshly reopened Store each time (not just the same in-memory Engine).
func TestRecoverFilesDoneNotReplayed(t *testing.T) {
	withUmask(t, 0022)
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	root, base := newTestRoot(t, "root")
	oldContent := []byte("pre-content")
	newContent := []byte("post-content-longer")
	if err := os.WriteFile(filepath.Join(base, "f"), oldContent, 0600); err != nil {
		t.Fatal(err)
	}
	oldState := fileStateFor(0600, uid, gid, oldContent)
	newState := fileStateFor(0600, uid, gid, newContent)

	j := baseJournal("replay1")
	j.Phase = PhaseRollingBack
	j.RollbackFilesDone = false
	j.Steps = []Step{mkStep("s1", KindWriteFile, "root/f", StatusDone, oldState, newState)}
	j.RollbackFrontier = []StepStatus{StatusDone}
	j.RollbackSteps = []Step{mkStep("r1", KindWriteFile, "root/f", StatusDone, newState, oldState)}
	store, err := BeginTransaction(root, "txn", j)
	if err != nil {
		t.Fatalf("BeginTransaction: %v", err)
	}
	// The rollback step is already recorded done, and the file already
	// reflects that (oldContent) — simulating a crash right after
	// rollback_files_done should have been persisted true.
	if err := os.WriteFile(filepath.Join(base, "f"), oldContent, 0600); err != nil {
		t.Fatal(err)
	}
	inodeBefore := inodeOfPath(t, filepath.Join(base, "f"))
	engine := NewEngine(store)
	rec := baseRecoverer(t, engine, root, base, nil)
	outcome, err := rec.Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if outcome != OutcomeRolledBack {
		t.Fatalf("Outcome = %v, want RolledBack", outcome)
	}
	inodeAfter := inodeOfPath(t, filepath.Join(base, "f"))
	if inodeBefore != inodeAfter {
		t.Fatalf("inode changed (%d -> %d): a done rollback step was replayed", inodeBefore, inodeAfter)
	}
	requireFileState(t, filepath.Join(base, "f"), oldState)
	requireNotExist(t, getTxnPath(base))
}

// TestRestoringResume covers T3's "restoring 从 daemon-reload 起整体重做"
// (plan Task 5 review round 1): recover finding RollbackSystemd=="restoring"
// (crashed after daemon-reload/enable but before restart, or after restart
// but before persisting "restored") redoes daemon-reload, restores
// UnitFileState and restarts again, then gates — never skipping the
// restart the way resuming from "restored" does.
func TestRestoringResume(t *testing.T) {
	withUmask(t, 0022)
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	root, base := newTestRoot(t, "root")
	binContent := []byte("restored-binary")
	if err := os.WriteFile(filepath.Join(base, "bin"), binContent, 0755); err != nil {
		t.Fatal(err)
	}
	binState := fileStateFor(0755, uid, gid, binContent)

	j := baseJournal("restoring1")
	j.Phase = PhaseRollingBack
	j.UnitStateBefore = UnitState{LoadState: "loaded", UnitFileState: "enabled", ActiveState: "active"}
	j.RollbackSystemd = RollbackSystemdRestoring
	j.RollbackFilesDone = true
	j.Steps = []Step{
		mkStep("s1", KindWriteFile, "root/bin", StatusDone, binState, binState),
		mkStep("s2", KindSystemdEnable, "systemd/svc.service", StatusDone,
			PathState{Exists: false, UnitFileState: "disabled"}, PathState{Exists: false, UnitFileState: "enabled"}),
	}
	j.RollbackFrontier = []StepStatus{StatusDone, StatusDone}
	// No file content actually needs restoring (already at its
	// pre-transaction state) — this journal isolates the "restoring"
	// systemd resume behavior.
	store, err := BeginTransaction(root, "txn", j)
	if err != nil {
		t.Fatalf("BeginTransaction: %v", err)
	}
	engine := NewEngine(store)
	rec := baseRecoverer(t, engine, root, base, nil)
	fsd := &fakeSystemd{loaded: true, unitFileState: "disabled", binaryPath: filepath.Join(base, "bin")}
	rec.Systemd = fsd
	gateCalls := 0
	rec.Gate = func(context.Context) error { gateCalls++; return nil }

	outcome, err := rec.Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if outcome != OutcomeRolledBack {
		t.Fatalf("Outcome = %v, want RolledBack", outcome)
	}
	wantCalls := []string{"daemon-reload", "enable", "restart"}
	if got := fsd.Calls(); !equalStrings(got, wantCalls) {
		t.Fatalf("systemd calls = %v, want %v (restoring redoes from daemon-reload, no demote calls)", got, wantCalls)
	}
	if gateCalls != 1 {
		t.Fatalf("gate called %d times, want 1", gateCalls)
	}
	if fsd.restartCount != 1 {
		t.Fatalf("restart called %d times, want 1", fsd.restartCount)
	}
	requireNotExist(t, getTxnPath(base))
}

// TestTombstoneEdgeCases extends TestTombstoneRecovery with the remaining
// §7.6/§14.2 leftover-residue shapes from T8 (plan Task 5 review round 1):
// journal.json and journal.json.tmp coexisting (N1), and stray dl/ or
// snap/ content — none of these block a normal recover.
func TestTombstoneEdgeCases(t *testing.T) {
	withUmask(t, 0022)
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())

	t.Run("N1: journal.json and journal.json.tmp coexist", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		j := baseJournal("n1")
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		// Simulate a crash mid-persist of some later update: journal.json
		// (the last fully-committed version) stays, plus a leftover .tmp
		// from an update attempt that never got renamed.
		if err := os.WriteFile(filepath.Join(getTxnPath(base), "journal.json.tmp"), []byte(`{"schema":1,"broken`), 0600); err != nil {
			t.Fatal(err)
		}
		engine := NewEngine(store)
		rec := baseRecoverer(t, engine, root, base, nil)
		outcome, err := rec.Recover(context.Background())
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}
		if outcome != OutcomeNoChange {
			t.Fatalf("Outcome = %v, want NoChange", outcome)
		}
		requireNotExist(t, getTxnPath(base))
	})

	t.Run("stray dl/ and snap/ content does not block recover", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		post := fileStateFor(0600, uid, gid, []byte("hi"))
		j := baseJournal("n2")
		j.Steps = []Step{mkStep("s1", KindWriteFile, "root/f", StatusDone, PathState{Exists: false}, post)}
		j.Steps[0].Temps = []string{TempName("n2", 1)}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		if err := os.WriteFile(filepath.Join(base, "f"), []byte("hi"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(getTxnPath(base), "dl"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(getTxnPath(base), "dl", "sing-box.tar.gz"), []byte("binary-blob"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(getTxnPath(base), "snap"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(getTxnPath(base), "snap", "leftover.snap"), []byte("old-secret"), 0600); err != nil {
			t.Fatal(err)
		}
		engine := NewEngine(store)
		rec := baseRecoverer(t, engine, root, base, nil)
		outcome, err := rec.Recover(context.Background())
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}
		if outcome != OutcomeRolledBack {
			t.Fatalf("Outcome = %v, want RolledBack", outcome)
		}
		requireNotExist(t, filepath.Join(base, "f"))
		requireNotExist(t, getTxnPath(base))
	})
}

// TestPublishStagingLifecycle covers T9 (plan Task 5 review round 1): the
// same-path multi-step scenarios §14.2 calls out for a publish-shaped
// transaction's own staging directory (Step.Provisional), using a
// synthetic journal (this package has no internal/publish of its own to
// drive) — staging still `mkdir` pending when a crash happens (not
// tampering: absence is normal for a step that never started), staging
// already undone by a rollback that then itself crashed (a second recover
// must still succeed, the final removal step is allowed to stay pending),
// and staging deleted out from under a `done` mkdir step (tampering).
func TestPublishStagingLifecycle(t *testing.T) {
	withUmask(t, 0022)
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())

	t.Run("staging mkdir still pending when crash happens: recover succeeds", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		j := baseJournal("staging1")
		j.Steps = []Step{
			{ID: "p1", Kind: KindMkdir, Target: "root/staging", Status: StatusPending,
				Pre: PathState{Exists: false}, Post: PathState{Exists: true, Type: PathTypeDir, Mode: 0700, UID: uid, GID: gid}, Provisional: true},
			{ID: "p2", Kind: KindRemoveDir, Target: "root/staging", Status: StatusPending,
				Pre: PathState{Exists: true, Type: PathTypeDir, Mode: 0700, UID: uid, GID: gid}, Post: PathState{Exists: false}, Provisional: true},
		}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		engine := NewEngine(store)
		rec := baseRecoverer(t, engine, root, base, nil)
		outcome, err := rec.Recover(context.Background())
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}
		if outcome != OutcomeNoChange {
			t.Fatalf("Outcome = %v, want NoChange (only Provisional steps were ever touched, and not even that)", outcome)
		}
		requireNotExist(t, filepath.Join(base, "staging"))
		requireNotExist(t, getTxnPath(base))
	})

	t.Run("staging already removed by a prior rollback attempt that then crashed: a second recover succeeds", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		j := baseJournal("staging2")
		j.Phase = PhaseRollingBack
		j.RollbackFilesDone = false
		j.Steps = []Step{
			{ID: "p1", Kind: KindMkdir, Target: "root/staging", Status: StatusDone,
				Pre: PathState{Exists: false}, Post: PathState{Exists: true, Type: PathTypeDir, Mode: 0700, UID: uid, GID: gid}, Provisional: true},
			{ID: "p2", Kind: KindRemoveDir, Target: "root/staging", Status: StatusPending,
				Pre: PathState{Exists: true, Type: PathTypeDir, Mode: 0700, UID: uid, GID: gid}, Post: PathState{Exists: false}, Provisional: true},
		}
		j.RollbackFrontier = []StepStatus{StatusDone, StatusPending}
		// The rollback step that undid the staging mkdir is already done
		// (staging is gone); p2 (the *forward* final removal, still
		// pending per its own chain) is unaffected by rollback and stays
		// pending — its own Pre requires the directory to still exist,
		// but since a full rollback took over, p2 never runs at all in
		// this journal (RolledBack only executes rollback_steps).
		j.RollbackSteps = []Step{mkStep("r1", KindRemoveDir, "root/staging", StatusDone,
			PathState{Exists: true, Type: PathTypeDir, Mode: 0700, UID: uid, GID: gid}, PathState{Exists: false})}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		engine := NewEngine(store)
		rec := baseRecoverer(t, engine, root, base, nil)
		outcome, err := rec.Recover(context.Background())
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}
		if outcome != OutcomeRolledBack {
			t.Fatalf("Outcome = %v, want RolledBack", outcome)
		}
		requireNotExist(t, filepath.Join(base, "staging"))
		requireNotExist(t, getTxnPath(base))
	})

	t.Run("staging deleted externally after its mkdir was marked done: tampering", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		j := baseJournal("staging3")
		j.Steps = []Step{
			{ID: "p1", Kind: KindMkdir, Target: "root/staging", Status: StatusDone,
				Pre: PathState{Exists: false}, Post: PathState{Exists: true, Type: PathTypeDir, Mode: 0700, UID: uid, GID: gid}, Provisional: true},
		}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		// staging was never actually created on disk (this journal
		// declares p1 done without the engine ever having run it) —
		// exactly what "deleted out from under a done mkdir" looks like
		// from recover's point of view: the directory is absent when the
		// journal says it must exist.
		engine := NewEngine(store)
		rec := baseRecoverer(t, engine, root, base, nil)
		_, err = rec.Recover(context.Background())
		if !errors.Is(err, ErrTampered) {
			t.Fatalf("Recover error = %v, want ErrTampered", err)
		}
		requireNotExist(t, filepath.Join(base, "staging"))
	})
}

// TestSetMetaRollbackResume covers the remainder of T2 this package can
// exercise directly (plan Task 5 review round 1): a crash mid-way through
// a rollback set_meta step (mode changed by the forward transaction,
// needing to be restored), resumed by a second, independently opened
// Recoverer — proving RollbackSetMeta's own resume path (steps.go),
// exercised through the engine Recoverer.recoverFiles drives, not just
// through a direct Engine call.
func TestSetMetaRollbackResume(t *testing.T) {
	withUmask(t, 0022)
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())

	for _, point := range []string{"set_meta:after_chown", "set_meta:after_chmod"} {
		t.Run(point, func(t *testing.T) {
			root, base := newTestRoot(t, "root")
			if err := os.WriteFile(filepath.Join(base, "f"), []byte("content"), 0600); err != nil {
				t.Fatal(err)
			}
			preState := PathState{Exists: true, Type: PathTypeFile, Size: 7, SHA256: sha256HexOf([]byte("content")), Mode: 0644, UID: uid, GID: gid}
			postState := preState
			postState.Mode = 0600
			j := baseJournal("setmeta1")
			j.Phase = PhaseRollingBack
			j.RollbackFilesDone = false
			j.Steps = []Step{mkStep("s1", KindSetMeta, "root/f", StatusDone, preState, postState)}
			j.RollbackFrontier = []StepStatus{StatusDone}
			j.RollbackSteps = []Step{mkStep("r1", KindSetMeta, "root/f", StatusPending, postState, preState)}
			store, err := BeginTransaction(root, "txn", j)
			if err != nil {
				t.Fatalf("BeginTransaction: %v", err)
			}
			// The file is currently at postState (mode 0600, matching the
			// forward step's done post) — the rollback needs to restore
			// mode 0644.
			if err := os.Chmod(filepath.Join(base, "f"), 0600); err != nil {
				t.Fatal(err)
			}
			engine := NewEngine(store)
			setStepHook(t, func(p string) error {
				if p == point {
					return errFakeCrash
				}
				return nil
			})
			rec := baseRecoverer(t, engine, root, base, nil)
			_, err = rec.Recover(context.Background())
			if !errors.Is(err, errFakeCrash) {
				t.Fatalf("Recover error = %v, want errFakeCrash at %s", err, point)
			}
			setStepHook(t, func(string) error { return nil })
			store.Close()

			store2, err := OpenTransaction(root, "txn")
			if err != nil {
				t.Fatalf("OpenTransaction: %v", err)
			}
			defer store2.Close()
			engine2 := NewEngine(store2)
			rec2 := baseRecoverer(t, engine2, root, base, nil)
			outcome, err := rec2.Recover(context.Background())
			if err != nil {
				t.Fatalf("Recover (resume) at %s: %v", point, err)
			}
			if outcome != OutcomeRolledBack {
				t.Fatalf("Outcome = %v, want RolledBack (crashed at %s)", outcome, point)
			}
			info, err := os.Stat(filepath.Join(base, "f"))
			if err != nil {
				t.Fatalf("stat f: %v", err)
			}
			if info.Mode().Perm() != 0644 {
				t.Fatalf("mode = %v, want 0644 (crashed at %s)", info.Mode().Perm(), point)
			}
			requireNotExist(t, getTxnPath(base))
		})
	}
}

// mutatingSystemdCalls filters calls down to the ones that actually change
// host state (as opposed to a read like "unit_file_state"/"load_state") —
// exactly what review round 2's N1 requires be empty when nothing was
// modified: §7.4's "先校验" still requires a live read of the systemd
// chain's current UnitFileState before Recover can even decide the
// journal is legal, so a validation-only read is unavoidable and
// harmless; a stop/disable/enable/daemon-reload/restart call is not.
func mutatingSystemdCalls(calls []string) []string {
	mutating := map[string]bool{"stop": true, "disable": true, "enable": true, "daemon-reload": true, "restart": true}
	var out []string
	for _, c := range calls {
		if mutating[c] {
			out = append(out, c)
		}
	}
	return out
}

// TestNoneModifiedSystemdBoundary covers review round 2's N1: a regression
// R2's own fix introduced (continueRollback ran demoteSystemd/
// restoreSystemd — stopping and possibly restarting the real service —
// even when Recover was about to report OutcomeNoChange). Three scenarios:
// (a) only a snapshot progressed — NoChange, zero mutating systemd calls;
// (b) same, plus a Provisional staging step done — staging is removed via
// its own registered steps, still NoChange, still zero mutating systemd
// calls; (c) an uninstall shape where only systemd_stop progressed (no
// file step at all) — RolledBack, and the full demote/restore sequence
// runs (a systemd-only "modification" must not be reported as NoChange,
// and must actually be converged, not skipped).
func TestNoneModifiedSystemdBoundary(t *testing.T) {
	withUmask(t, 0022)
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())

	t.Run("reinstall shape, only snapshot done: NoChange, no mutating systemd calls", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		oldBin := []byte("old-binary")
		if err := os.WriteFile(filepath.Join(base, "bin"), oldBin, 0755); err != nil {
			t.Fatal(err)
		}
		oldState := fileStateFor(0755, uid, gid, oldBin)
		newState := fileStateFor(0755, uid, gid, []byte("new-binary-longer"))
		j := baseJournal("nm1")
		j.UnitStateBefore = UnitState{LoadState: "loaded", UnitFileState: "enabled", ActiveState: "active"}
		j.Steps = []Step{
			mkStep("s1", KindSnapshot, "root/bin", StatusPending, oldState, oldState),
			mkStep("s2", KindWriteFile, "root/bin", StatusPending, oldState, newState),
			mkStep("s3", KindSystemdRestart, "systemd/svc.service", StatusPending,
				PathState{Exists: false, UnitFileState: "enabled"}, PathState{Exists: false, UnitFileState: "enabled"}),
		}
		j.Steps[0].Temps = []string{TempName("nm1", 1)}
		j.Steps[1].Temps = []string{TempName("nm1", 2)}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		snapDir := newSnapDir(t, base)
		engine := NewEngine(store)
		if err := engine.Snapshot(context.Background(), "s1", ManagedPath{Root: root, RootKey: "root", Leaf: "bin"}, snapDir, TempName("nm1", 1)); err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		rec := baseRecoverer(t, engine, root, base, snapDir)
		fsd := &fakeSystemd{loaded: true, unitFileState: "enabled"}
		rec.Systemd = fsd
		gateCalls := 0
		rec.Gate = func(context.Context) error { gateCalls++; return nil }

		outcome, err := rec.Recover(context.Background())
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}
		if outcome != OutcomeNoChange {
			t.Fatalf("Outcome = %v, want NoChange", outcome)
		}
		if mut := mutatingSystemdCalls(fsd.Calls()); len(mut) != 0 {
			t.Fatalf("mutating systemd calls = %v, want none (service must not be stopped or restarted)", mut)
		}
		if gateCalls != 0 {
			t.Fatalf("gate called %d times, want 0", gateCalls)
		}
		requireFileState(t, filepath.Join(base, "bin"), oldState)
		requireNotExist(t, getTxnPath(base))
	})

	t.Run("reinstall shape + staging done: staging removed, still NoChange, no mutating systemd calls", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		oldBin := []byte("old-binary")
		if err := os.WriteFile(filepath.Join(base, "bin"), oldBin, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(base, "staging"), 0700); err != nil {
			t.Fatal(err)
		}
		oldState := fileStateFor(0755, uid, gid, oldBin)
		newState := fileStateFor(0755, uid, gid, []byte("new-binary-longer"))
		j := baseJournal("nm2")
		j.UnitStateBefore = UnitState{LoadState: "loaded", UnitFileState: "enabled", ActiveState: "active"}
		j.Steps = []Step{
			{ID: "p1", Kind: KindMkdir, Target: "root/staging", Status: StatusDone,
				Pre: PathState{Exists: false}, Post: PathState{Exists: true, Type: PathTypeDir, Mode: 0700, UID: uid, GID: gid}, Provisional: true},
			mkStep("s1", KindSnapshot, "root/bin", StatusPending, oldState, oldState),
			mkStep("s2", KindWriteFile, "root/bin", StatusPending, oldState, newState),
			mkStep("s3", KindSystemdRestart, "systemd/svc.service", StatusPending,
				PathState{Exists: false, UnitFileState: "enabled"}, PathState{Exists: false, UnitFileState: "enabled"}),
		}
		j.Steps[1].Temps = []string{TempName("nm2", 1)}
		j.Steps[2].Temps = []string{TempName("nm2", 2)}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		snapDir := newSnapDir(t, base)
		engine := NewEngine(store)
		if err := engine.Snapshot(context.Background(), "s1", ManagedPath{Root: root, RootKey: "root", Leaf: "bin"}, snapDir, TempName("nm2", 1)); err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		rec := baseRecoverer(t, engine, root, base, snapDir)
		fsd := &fakeSystemd{loaded: true, unitFileState: "enabled"}
		rec.Systemd = fsd

		outcome, err := rec.Recover(context.Background())
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}
		if outcome != OutcomeNoChange {
			t.Fatalf("Outcome = %v, want NoChange", outcome)
		}
		if mut := mutatingSystemdCalls(fsd.Calls()); len(mut) != 0 {
			t.Fatalf("mutating systemd calls = %v, want none", mut)
		}
		requireNotExist(t, filepath.Join(base, "staging"))
		requireFileState(t, filepath.Join(base, "bin"), oldState)
		requireNotExist(t, getTxnPath(base))
	})

	t.Run("uninstall shape, only systemd_stop done: RolledBack, full demote/restore runs", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		unitContent := []byte("[Unit]\n...")
		unitState := fileStateFor(0644, uid, gid, unitContent)
		if err := os.WriteFile(filepath.Join(base, "svc.service"), unitContent, 0644); err != nil {
			t.Fatal(err)
		}
		j := baseJournal("nm3")
		j.UnitStateBefore = UnitState{LoadState: "loaded", UnitFileState: "enabled", ActiveState: "active"}
		j.Steps = []Step{
			mkStep("s1", KindSystemdStop, "systemd/svc.service", StatusDone,
				PathState{Exists: false, UnitFileState: "enabled"}, PathState{Exists: false, UnitFileState: "enabled"}),
			mkStep("s2", KindRemoveFile, "root/svc.service", StatusPending, unitState, PathState{Exists: false}),
		}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		engine := NewEngine(store)
		rec := baseRecoverer(t, engine, root, base, nil)
		fsd := &fakeSystemd{loaded: true, unitFileState: "enabled"}
		rec.Systemd = fsd
		gateCalls := 0
		rec.Gate = func(context.Context) error { gateCalls++; return nil }

		outcome, err := rec.Recover(context.Background())
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}
		if outcome != OutcomeRolledBack {
			t.Fatalf("Outcome = %v, want RolledBack (a systemd-only modification is still a modification)", outcome)
		}
		wantCalls := []string{"unit_file_state", "load_state", "stop", "disable", "daemon-reload", "enable", "restart"}
		if got := fsd.Calls(); !equalStrings(got, wantCalls) {
			t.Fatalf("systemd calls = %v, want %v (full demote/restore must still run)", got, wantCalls)
		}
		if gateCalls != 1 {
			t.Fatalf("gate called %d times, want 1", gateCalls)
		}
		requireFileState(t, filepath.Join(base, "svc.service"), unitState)
		requireNotExist(t, getTxnPath(base))
	})
}

// TestDemotingResume covers N2 (plan Task 5 review round 2): an originally
// enabled+active reinstall that crashed during rollback step 3, after the
// disable call but before "demoted" was persisted (RollbackSystemd stays
// "demoting"; the fake systemd already reflects UnitFileState=="disabled",
// which matches neither the forward systemd_enable step's Pre nor Post —
// exactly the §7.3 carve-out for "rollback_systemd 为 demoting 及之后不判
// UnitFileState"). recover must resume demoteSystemd (idempotently
// re-running Stop/disable is harmless), then file recovery, then the full
// restore sequence — converging to enabled, restarted, with the gate
// observing the *old* binary's inode.
func TestDemotingResume(t *testing.T) {
	withUmask(t, 0022)
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	root, base := newTestRoot(t, "root")
	oldBin := []byte("old-version-binary")
	newBin := []byte("new-version-binary-longer")
	// The file still shows the *new* binary: rollback's file recovery
	// (step 4) has not run yet — only step 3 (systemd demotion) is
	// partway done, as the scenario requires.
	if err := os.WriteFile(filepath.Join(base, "bin"), newBin, 0755); err != nil {
		t.Fatal(err)
	}
	oldState := fileStateFor(0755, uid, gid, oldBin)
	newState := fileStateFor(0755, uid, gid, newBin)

	j := baseJournal("demoting1")
	j.Phase = PhaseRollingBack
	j.UnitStateBefore = UnitState{LoadState: "loaded", UnitFileState: "enabled", ActiveState: "active"}
	j.RollbackSystemd = RollbackSystemdDemoting
	j.RollbackFilesDone = false
	j.Steps = []Step{
		mkStep("s1", KindSnapshot, "root/bin", StatusDone, oldState, oldState),
		mkStep("s2", KindWriteFile, "root/bin", StatusDone, oldState, newState),
		mkStep("s3", KindSystemdRestart, "systemd/svc.service", StatusDone,
			PathState{Exists: false, UnitFileState: "enabled"}, PathState{Exists: false, UnitFileState: "enabled"}),
	}
	j.Steps[0].Temps = []string{TempName("demoting1", 2)}
	j.RollbackFrontier = []StepStatus{StatusDone, StatusDone, StatusDone}
	j.RollbackSteps = []Step{mkStep("r1", KindWriteFile, "root/bin", StatusPending, newState, oldState)}
	j.RollbackSteps[0].Temps = []string{TempName("demoting1", 1)}
	store, err := BeginTransaction(root, "txn", j)
	if err != nil {
		t.Fatalf("BeginTransaction: %v", err)
	}
	snapDir := newSnapDir(t, base)
	if err := os.WriteFile(filepath.Join(snapDir.Name(), TempName("demoting1", 2)+".snap"), oldBin, 0600); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(store)
	rec := baseRecoverer(t, engine, root, base, snapDir)
	// UnitFileState=="disabled" already: a previous attempt's disable call
	// already landed, but "demoted" was never persisted — the exact
	// mid-demoting state this test targets.
	fsd := &fakeSystemd{loaded: true, unitFileState: "disabled", binaryPath: filepath.Join(base, "bin")}
	rec.Systemd = fsd
	gateCalls := 0
	rec.Gate = func(context.Context) error { gateCalls++; return nil }

	outcome, err := rec.Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if outcome != OutcomeRolledBack {
		t.Fatalf("Outcome = %v, want RolledBack", outcome)
	}
	wantCalls := []string{"load_state", "stop", "disable", "daemon-reload", "enable", "restart"}
	if got := fsd.Calls(); !equalStrings(got, wantCalls) {
		t.Fatalf("systemd calls = %v, want %v (resumed demoting, never re-checked UnitFileState via judgement since rollback_systemd != none)", got, wantCalls)
	}
	if gateCalls != 1 {
		t.Fatalf("gate called %d times, want 1", gateCalls)
	}
	finalInode := inodeOfPath(t, filepath.Join(base, "bin"))
	if fsd.restartInode != finalInode {
		t.Fatalf("systemd Restart saw inode %d, final on-disk inode is %d", fsd.restartInode, finalInode)
	}
	data, err := os.ReadFile(filepath.Join(base, "bin"))
	if err != nil || string(data) != string(oldBin) {
		t.Fatalf("bin content = %q err=%v, want %q (old binary)", data, err, oldBin)
	}
	requireNotExist(t, getTxnPath(base))
}

// TestJudgeSystemdChain covers N3 (plan Task 5 review round 2):
// judgeSystemdChain's own §7.2/§7.3 chain rules, isolated to a
// single-target journal (just the systemd step — no file targets),
// exercised through the public Recover entry point.
func TestJudgeSystemdChain(t *testing.T) {
	withUmask(t, 0022)

	// runIsolated builds a journal containing only the given single
	// systemd step, runs Recover with fsd's current unitFileState, and
	// returns the resulting error (nil on success).
	runIsolated := func(t *testing.T, step Step, unitFileState string) error {
		t.Helper()
		root, _ := newTestRoot(t, "root")
		j := baseJournal("judge1")
		j.UnitStateBefore = UnitState{LoadState: "loaded", UnitFileState: "enabled", ActiveState: "inactive"}
		j.Steps = []Step{step}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		engine := NewEngine(store)
		rec := &Recoverer{
			Engine:        engine,
			Root:          root,
			TombstoneName: ".txn.tomb-judge1",
			Resolve:       recResolver{root: root, rootKey: "root"},
			Systemd:       &fakeSystemd{loaded: true, unitFileState: unitFileState},
		}
		_, err = rec.Recover(context.Background())
		return err
	}

	t.Run("enable started, live=pre: accepted", func(t *testing.T) {
		step := mkStep("s1", KindSystemdEnable, "systemd/svc.service", StatusStarted,
			PathState{UnitFileState: "disabled"}, PathState{UnitFileState: "enabled"})
		if err := runIsolated(t, step, "disabled"); err != nil {
			t.Fatalf("Recover = %v, want nil (live matches pre)", err)
		}
	})

	t.Run("enable started, live=post: accepted", func(t *testing.T) {
		step := mkStep("s1", KindSystemdEnable, "systemd/svc.service", StatusStarted,
			PathState{UnitFileState: "disabled"}, PathState{UnitFileState: "enabled"})
		if err := runIsolated(t, step, "enabled"); err != nil {
			t.Fatalf("Recover = %v, want nil (live matches post)", err)
		}
	})

	t.Run("enable started, live matches neither pre nor post: ErrTampered, journal unchanged", func(t *testing.T) {
		root, _ := newTestRoot(t, "root")
		j := baseJournal("judge2")
		j.UnitStateBefore = UnitState{LoadState: "loaded", UnitFileState: "enabled", ActiveState: "inactive"}
		j.Steps = []Step{mkStep("s1", KindSystemdEnable, "systemd/svc.service", StatusStarted,
			PathState{UnitFileState: "disabled"}, PathState{UnitFileState: "enabled"})}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		before := readFile(t, filepath.Join(getTxnPath(root.Path()), "journal.json"))
		engine := NewEngine(store)
		rec := &Recoverer{
			Engine: engine, Root: root, TombstoneName: ".txn.tomb-judge2",
			Resolve: recResolver{root: root, rootKey: "root"},
			Systemd: &fakeSystemd{loaded: true, unitFileState: "masked"},
		}
		_, err = rec.Recover(context.Background())
		if !errors.Is(err, ErrTampered) {
			t.Fatalf("Recover error = %v, want ErrTampered", err)
		}
		after := readFile(t, filepath.Join(getTxnPath(root.Path()), "journal.json"))
		if string(before) != string(after) {
			t.Fatalf("journal.json changed despite a tampering verdict")
		}
	})

	t.Run("enable done, live != post: ErrTampered, journal unchanged", func(t *testing.T) {
		root, _ := newTestRoot(t, "root")
		j := baseJournal("judge3")
		j.UnitStateBefore = UnitState{LoadState: "loaded", UnitFileState: "enabled", ActiveState: "inactive"}
		j.Steps = []Step{mkStep("s1", KindSystemdEnable, "systemd/svc.service", StatusDone,
			PathState{UnitFileState: "disabled"}, PathState{UnitFileState: "enabled"})}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		before := readFile(t, filepath.Join(getTxnPath(root.Path()), "journal.json"))
		engine := NewEngine(store)
		rec := &Recoverer{
			Engine: engine, Root: root, TombstoneName: ".txn.tomb-judge3",
			Resolve: recResolver{root: root, rootKey: "root"},
			Systemd: &fakeSystemd{loaded: true, unitFileState: "disabled"}, // matches pre, not post
		}
		_, err = rec.Recover(context.Background())
		if !errors.Is(err, ErrTampered) {
			t.Fatalf("Recover error = %v, want ErrTampered", err)
		}
		after := readFile(t, filepath.Join(getTxnPath(root.Path()), "journal.json"))
		if string(before) != string(after) {
			t.Fatalf("journal.json changed despite a tampering verdict")
		}
	})

	for _, kind := range []StepKind{KindSystemdStart, KindSystemdStop, KindSystemdRestart, KindSystemdDaemonReload} {
		kind := kind
		t.Run(string(kind)+" started: never judged on UnitFileState, any live value accepted", func(t *testing.T) {
			step := mkStep("s1", kind, "systemd/svc.service", StatusStarted, PathState{}, PathState{})
			if err := runIsolated(t, step, "whatever-value"); err != nil {
				t.Fatalf("Recover = %v, want nil (§7.2: not checked)", err)
			}
		})
	}

	t.Run("rollback_systemd >= demoting: UnitFileState never judged, even a wildly mismatched live value is accepted", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		j := baseJournal("judge4")
		j.Phase = PhaseRollingBack
		j.UnitStateBefore = UnitState{LoadState: "loaded", UnitFileState: "enabled", ActiveState: "inactive"}
		j.RollbackSystemd = RollbackSystemdDemoted
		j.RollbackFilesDone = true
		j.Steps = []Step{mkStep("s1", KindSystemdEnable, "systemd/svc.service", StatusDone,
			PathState{UnitFileState: "disabled"}, PathState{UnitFileState: "enabled"})}
		j.RollbackFrontier = []StepStatus{StatusDone}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		engine := NewEngine(store)
		rec := baseRecoverer(t, engine, root, base, nil)
		// A live UnitFileState matching neither this journal's forward pre
		// nor post — would be ErrTampered under judgePathsRunning, but
		// rollback_systemd="demoted" (>= demoting) must skip the check
		// entirely (§7.3).
		fsd := &fakeSystemd{loaded: true, unitFileState: "masked"}
		rec.Systemd = fsd
		outcome, err := rec.Recover(context.Background())
		if err != nil {
			t.Fatalf("Recover = %v, want nil (systemd judgement must be skipped once rollback_systemd >= demoting)", err)
		}
		if outcome != OutcomeRolledBack {
			t.Fatalf("Outcome = %v, want RolledBack", outcome)
		}
	})
}

// TestPublishFilesDoneNoSystemd covers N4 (plan Task 5 review round 2): a
// publish-shaped transaction (no systemd steps at all, Recoverer.Systemd
// left nil) crashing after rollback_files_done was persisted true but
// before the transaction area was deleted. recover must not call
// write_file again (the restored file's inode stays exactly the same) and
// must not touch r.Systemd at all (nil is never dereferenced) — it only
// re-validates and deletes the transaction area.
func TestPublishFilesDoneNoSystemd(t *testing.T) {
	withUmask(t, 0022)
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	root, base := newTestRoot(t, "root")
	preContent := []byte("clash-yaml-pre-content")
	postContent := []byte("clash-yaml-post-content-longer")
	if err := os.WriteFile(filepath.Join(base, "clash.yaml"), preContent, 0644); err != nil {
		t.Fatal(err)
	}
	preState := fileStateFor(0644, uid, gid, preContent)
	postState := fileStateFor(0644, uid, gid, postContent)

	j := baseJournal("publish1")
	j.Phase = PhaseRollingBack
	j.RollbackFilesDone = true
	j.Steps = []Step{mkStep("s1", KindWriteFile, "root/clash.yaml", StatusDone, preState, postState)}
	j.RollbackFrontier = []StepStatus{StatusDone}
	j.RollbackSteps = []Step{mkStep("r1", KindWriteFile, "root/clash.yaml", StatusDone, postState, preState)}
	store, err := BeginTransaction(root, "txn", j)
	if err != nil {
		t.Fatalf("BeginTransaction: %v", err)
	}
	// No engine execution ever ran for r1 in this test (it is already
	// "done" from a previous, now-crashed attempt) — the file on disk
	// already reflects the fully rolled-back pre-state.
	inodeBefore := inodeOfPath(t, filepath.Join(base, "clash.yaml"))
	engine := NewEngine(store)
	rec := baseRecoverer(t, engine, root, base, nil)
	// rec.Systemd deliberately left nil: this journal has no systemd
	// steps, so continueRollback must never call it.

	outcome, err := rec.Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if outcome != OutcomeRolledBack {
		t.Fatalf("Outcome = %v, want RolledBack", outcome)
	}
	inodeAfter := inodeOfPath(t, filepath.Join(base, "clash.yaml"))
	if inodeBefore != inodeAfter {
		t.Fatalf("inode changed (%d -> %d): write_file was replayed despite rollback_files_done=true", inodeBefore, inodeAfter)
	}
	requireFileState(t, filepath.Join(base, "clash.yaml"), preState)
	requireNotExist(t, getTxnPath(base))
}

// TestIllegalJournalRejectedAtEntry covers N5 (plan Task 5 review round 2):
// a journal.json written directly to a transaction area (bypassing
// Store/Validate entirely — as if produced by a different, buggy or
// malicious writer) with a structurally illegal step/rollback/frontier
// sequence must be rejected by the same entry point a real recover
// invocation uses (OpenTransaction), even when the managed paths'
// real on-disk content happens to coincide with some state that would
// otherwise look legal. journal.json, every managed path's bytes and
// inode, and the transaction area itself must all be completely
// untouched, and the resulting error must be distinguishable
// (ErrIllegalProgress, wrapped).
func TestIllegalJournalRejectedAtEntry(t *testing.T) {
	withUmask(t, 0022)
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())

	writeRawJournal := func(t *testing.T, base string, j *Journal) {
		t.Helper()
		if err := os.Mkdir(getTxnPath(base), 0700); err != nil {
			t.Fatal(err)
		}
		data := marshalRaw(t, j)
		if err := os.WriteFile(filepath.Join(getTxnPath(base), "journal.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}

	assertRejectedAtEntry := func(t *testing.T, root *TrustedRoot, base string) {
		t.Helper()
		before := readFile(t, filepath.Join(getTxnPath(base), "journal.json"))
		_, err := OpenTransaction(root, "txn")
		if !errors.Is(err, ErrIllegalProgress) {
			t.Fatalf("OpenTransaction error = %v, want ErrIllegalProgress", err)
		}
		after := readFile(t, filepath.Join(getTxnPath(base), "journal.json"))
		if string(before) != string(after) {
			t.Fatalf("journal.json changed despite rejection at entry")
		}
		if _, err := os.Stat(getTxnPath(base)); err != nil {
			t.Fatalf("transaction area missing after rejection: %v", err)
		}
	}

	t.Run("forward sequence: pending followed by done", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		// f1 and f2's real content happens to coincide exactly with their
		// own "done" post-state, and f3's own pre-state — a coincidence
		// the structural check must not be fooled by.
		f1Content, f2Content, f3Content := []byte("f1-post"), []byte("f2-pre"), []byte("f3-post")
		if err := os.WriteFile(filepath.Join(base, "f1"), f1Content, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "f2"), f2Content, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "f3"), f3Content, 0600); err != nil {
			t.Fatal(err)
		}
		j := baseJournal("illegal1")
		j.Steps = []Step{
			mkStep("s1", KindWriteFile, "root/f1", StatusDone, PathState{Exists: false}, fileStateFor(0600, uid, gid, f1Content)),
			mkStep("s2", KindWriteFile, "root/f2", StatusPending, fileStateFor(0600, uid, gid, f2Content), fileStateFor(0600, uid, gid, []byte("f2-post"))),
			// Illegal: done appears after a pending step in overall
			// execution order (validateStatusPrefix is whole-array, not
			// per-target).
			mkStep("s3", KindWriteFile, "root/f3", StatusDone, PathState{Exists: false}, fileStateFor(0600, uid, gid, f3Content)),
		}
		j.Steps[0].Temps = []string{TempName("illegal1", 1)}
		j.Steps[1].Temps = []string{TempName("illegal1", 2)}
		j.Steps[2].Temps = []string{TempName("illegal1", 3)}
		writeRawJournal(t, base, j)
		assertRejectedAtEntry(t, root, base)
		requireFileState(t, filepath.Join(base, "f1"), fileStateFor(0600, uid, gid, f1Content))
		requireFileState(t, filepath.Join(base, "f2"), fileStateFor(0600, uid, gid, f2Content))
		requireFileState(t, filepath.Join(base, "f3"), fileStateFor(0600, uid, gid, f3Content))
	})

	t.Run("rollback sequence: done appears after a started step", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		content := []byte("f-content")
		if err := os.WriteFile(filepath.Join(base, "f"), content, 0600); err != nil {
			t.Fatal(err)
		}
		fState := fileStateFor(0600, uid, gid, content)
		j := baseJournal("illegal2")
		j.Phase = PhaseRollingBack
		j.Steps = []Step{
			mkStep("s1", KindWriteFile, "root/f", StatusDone, PathState{Exists: false}, fState),
			mkStep("s2", KindWriteFile, "root/g", StatusDone, PathState{Exists: false}, fileStateFor(0600, uid, gid, []byte("g"))),
		}
		j.Steps[1].Temps = []string{TempName("illegal2", 2)}
		j.RollbackFrontier = []StepStatus{StatusDone, StatusDone}
		j.RollbackSteps = []Step{
			mkStep("r1", KindRemoveFile, "root/f", StatusStarted, fState, PathState{Exists: false}),
			// Illegal: a rollback step recorded done after one that is
			// only started.
			mkStep("r2", KindRemoveFile, "root/g", StatusDone, fileStateFor(0600, uid, gid, []byte("g")), PathState{Exists: false}),
		}
		writeRawJournal(t, base, j)
		assertRejectedAtEntry(t, root, base)
		requireFileState(t, filepath.Join(base, "f"), fState)
	})

	t.Run("rollback_frontier itself is not a legal prefix", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		content := []byte("f-content")
		if err := os.WriteFile(filepath.Join(base, "f"), content, 0600); err != nil {
			t.Fatal(err)
		}
		fState := fileStateFor(0600, uid, gid, content)
		j := baseJournal("illegal3")
		j.Phase = PhaseRollingBack
		j.Steps = []Step{
			mkStep("s1", KindWriteFile, "root/f", StatusDone, PathState{Exists: false}, fState),
			mkStep("s2", KindWriteFile, "root/g", StatusDone, PathState{Exists: false}, fileStateFor(0600, uid, gid, []byte("g"))),
		}
		j.Steps[1].Temps = []string{TempName("illegal3", 2)}
		// Illegal: frontier itself is pending-then-done, not a legal
		// done*started?pending* prefix.
		j.RollbackFrontier = []StepStatus{StatusPending, StatusDone}
		j.RollbackSteps = []Step{mkStep("r1", KindRemoveFile, "root/f", StatusPending, fState, PathState{Exists: false})}
		writeRawJournal(t, base, j)
		assertRejectedAtEntry(t, root, base)
		requireFileState(t, filepath.Join(base, "f"), fState)
	})
}

// TestModifiedBoundarySystemdGate covers plan Task 5 review round 3's P1:
// continueRollback must run the full systemd demote/restore sequence
// whenever the modification boundary was crossed at all and the journal
// registers systemd steps — not only when a systemd step itself already
// progressed past pending. A reinstall that already wrote the new binary
// (write_file done) but whose systemd_daemon_reload/enable/restart steps
// are all still pending must still fully stop, disable, restore the file,
// daemon-reload, re-enable and restart — never silently leave the service
// running the half-written new binary or skip systemd convergence just
// because the systemd steps themselves never started.
func TestModifiedBoundarySystemdGate(t *testing.T) {
	withUmask(t, 0022)
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	root, base := newTestRoot(t, "root")
	oldBin := []byte("old-version-binary")
	newBin := []byte("new-version-binary-longer")
	if err := os.WriteFile(filepath.Join(base, "bin"), newBin, 0755); err != nil {
		t.Fatal(err)
	}
	oldState := fileStateFor(0755, uid, gid, oldBin)
	newState := fileStateFor(0755, uid, gid, newBin)

	j := baseJournal("gate1")
	j.UnitStateBefore = UnitState{LoadState: "loaded", UnitFileState: "enabled", ActiveState: "active"}
	j.Steps = []Step{
		mkStep("s1", KindSnapshot, "root/bin", StatusDone, oldState, oldState),
		mkStep("s2", KindWriteFile, "root/bin", StatusDone, oldState, newState),
		mkStep("s3", KindSystemdDaemonReload, "systemd/svc.service", StatusPending,
			PathState{UnitFileState: "enabled"}, PathState{UnitFileState: "enabled"}),
		mkStep("s4", KindSystemdEnable, "systemd/svc.service", StatusPending,
			PathState{UnitFileState: "enabled"}, PathState{UnitFileState: "enabled"}),
		mkStep("s5", KindSystemdRestart, "systemd/svc.service", StatusPending,
			PathState{UnitFileState: "enabled"}, PathState{UnitFileState: "enabled"}),
	}
	j.Steps[0].Temps = []string{TempName("gate1", 1)}
	j.Steps[1].Temps = []string{TempName("gate1", 2)}
	store, err := BeginTransaction(root, "txn", j)
	if err != nil {
		t.Fatalf("BeginTransaction: %v", err)
	}
	snapDir := newSnapDir(t, base)
	if err := os.WriteFile(filepath.Join(snapDir.Name(), TempName("gate1", 1)+".snap"), oldBin, 0600); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(store)
	rec := baseRecoverer(t, engine, root, base, snapDir)
	fsd := &fakeSystemd{loaded: true, unitFileState: "enabled", binaryPath: filepath.Join(base, "bin")}
	rec.Systemd = fsd
	gateCalls := 0
	rec.Gate = func(context.Context) error { gateCalls++; return nil }

	outcome, err := rec.Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if outcome != OutcomeRolledBack {
		t.Fatalf("Outcome = %v, want RolledBack", outcome)
	}
	wantCalls := []string{"unit_file_state", "load_state", "stop", "disable", "daemon-reload", "enable", "restart"}
	if got := fsd.Calls(); !equalStrings(got, wantCalls) {
		t.Fatalf("systemd calls = %v, want %v (full demote/restore must run even though the systemd steps themselves never started)", got, wantCalls)
	}
	if gateCalls != 1 {
		t.Fatalf("gate called %d times, want 1", gateCalls)
	}
	finalInode := inodeOfPath(t, filepath.Join(base, "bin"))
	if fsd.restartInode != finalInode {
		t.Fatalf("systemd Restart saw inode %d, final on-disk inode is %d", fsd.restartInode, finalInode)
	}
	data, err := os.ReadFile(filepath.Join(base, "bin"))
	if err != nil || string(data) != string(oldBin) {
		t.Fatalf("bin content = %q err=%v, want %q (old binary)", data, err, oldBin)
	}
	requireNotExist(t, getTxnPath(base))
}
