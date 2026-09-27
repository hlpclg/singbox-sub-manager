package transaction

// TestRecoveryCrashMatrix (plan Task 5, design spec §14.2's crash-injection
// requirement): the test process re-execs itself as a real, independent OS
// process that runs a real forward or recovery operation against a real
// temp filesystem and os.Exit(1)s at a named hook point (stepHook/
// persistHook — the same process-global seams Task 4's own crash tests
// use), simulating a hard crash. The parent then opens the same
// transaction area fresh and drives recovery to completion (possibly
// through more than one further helper subprocess, to prove recover itself
// surviving a second crash converges too), and verifies every managed
// path's bytes, mode, ownership and existence against the pre-transaction
// state, plus that the transaction area and any tombstone are gone.
//
// This file covers a representative subset of §14.2's full cross-product
// (every primitive's every internal hook point, times every operation
// shape) rather than the exhaustive matrix — see the Task 5 handback
// report for exactly which combinations are deferred to Task 6-9's real
// operation-level tests, and why (this package has no concrete instance or
// publish path layout of its own to run them against).

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	envCrashCase  = "TXN_CRASH_CASE"  // "start" | "fwd_create" | "fwd_delete" | "recover"
	envCrashRoot  = "TXN_CRASH_ROOT"  // the trusted root's real directory path
	envCrashPoint = "TXN_CRASH_POINT" // stepHook/persistHook point name to crash at
	envCrashTxnID = "TXN_CRASH_TXNID"
	// envCrashDirMode overrides "fwd_delete"'s managed directory mode
	// (octal, e.g. "755"); defaults to 0700 when unset.
	envCrashDirMode = "TXN_CRASH_DIR_MODE"
)

func init() {
	if c := os.Getenv(envCrashCase); c != "" {
		os.Exit(runCrashHelper(c))
	}
}

// runCrashHelper is invoked by init() (before any test runs, and before
// TestMain would matter — package tests here don't register their own
// TestMain, so a plain init is simplest and unambiguous) when this process
// was re-exec'd specifically to play the crashing side of a scenario.
func runCrashHelper(caseName string) int {
	rootPath := os.Getenv(envCrashRoot)
	point := os.Getenv(envCrashPoint)
	txnID := os.Getenv(envCrashTxnID)

	root, err := OpenTrustedRoot(rootPath, os.Getuid())
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper: OpenTrustedRoot:", err)
		return 9
	}
	defer root.Close()

	crash := func(p string) error {
		if point != "" && p == point {
			os.Exit(1)
		}
		return nil
	}

	switch caseName {
	case "start":
		persistHook = crash
		j := buildCreateJournal(txnID)
		if _, err := BeginTransaction(root, "txn", j); err != nil {
			fmt.Fprintln(os.Stderr, "helper: BeginTransaction:", err)
			return 1
		}
		return 0

	case "fwd_create":
		j := buildCreateJournal(txnID)
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			fmt.Fprintln(os.Stderr, "helper: BeginTransaction:", err)
			return 1
		}
		defer store.Close()
		engine := NewEngine(store)
		stepHook = crash
		ctx := context.Background()
		mpInstance := ManagedPath{Root: root, RootKey: "root", Leaf: "instance"}
		if _, err := engine.Mkdir(ctx, "s1", mpInstance); err != nil {
			fmt.Fprintln(os.Stderr, "helper: Mkdir:", err)
			return 1
		}
		mpConfig := ManagedPath{Root: root, RootKey: "root", Comps: []string{"instance"}, Keys: []string{"root/instance"}, Leaf: "config"}
		if err := engine.WriteFile(ctx, "s2", mpConfig, TempName(txnID, 1), []byte("hello")); err != nil {
			fmt.Fprintln(os.Stderr, "helper: WriteFile:", err)
			return 1
		}
		return 0

	case "fwd_delete":
		dirID := dirIdentityRaw(root, "instance")
		dirMode := os.FileMode(0700)
		if m := os.Getenv(envCrashDirMode); m != "" {
			var parsed uint32
			if _, err := fmt.Sscanf(m, "%o", &parsed); err == nil {
				dirMode = os.FileMode(parsed)
			}
		}
		j := buildDeleteJournal(txnID, dirID, dirMode)
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			fmt.Fprintln(os.Stderr, "helper: BeginTransaction:", err)
			return 1
		}
		defer store.Close()
		if err := os.Mkdir(filepath.Join(rootPath, "txn", "snap"), 0700); err != nil {
			fmt.Fprintln(os.Stderr, "helper: mkdir snap:", err)
			return 1
		}
		snapDir := openDirRaw(filepath.Join(rootPath, "txn", "snap"))
		engine := NewEngine(store)
		stepHook = crash
		ctx := context.Background()
		mpConfig := ManagedPath{Root: root, RootKey: "root", Comps: []string{"instance"}, Keys: []string{"root/instance"}, Leaf: "config"}
		if err := engine.Snapshot(ctx, "s1", mpConfig, snapDir, TempName(txnID, 1)); err != nil {
			fmt.Fprintln(os.Stderr, "helper: Snapshot:", err)
			return 1
		}
		if err := engine.RemoveFile(ctx, "s2", mpConfig); err != nil {
			fmt.Fprintln(os.Stderr, "helper: RemoveFile:", err)
			return 1
		}
		mpInstance := ManagedPath{Root: root, RootKey: "root", Leaf: "instance"}
		if err := engine.RemoveDir(ctx, "s3", mpInstance); err != nil {
			fmt.Fprintln(os.Stderr, "helper: RemoveDir:", err)
			return 1
		}
		return 0

	case "recover":
		store, err := OpenTransaction(root, "txn")
		if err != nil {
			fmt.Fprintln(os.Stderr, "helper: OpenTransaction:", err)
			return 1
		}
		defer store.Close()
		engine := NewEngine(store)
		stepHook = crash
		persistHook = crash
		rec := crashRecoverer(engine, root, rootPath)
		outcome, err := rec.Recover(context.Background())
		if err != nil {
			fmt.Fprintln(os.Stderr, "helper: Recover:", err)
			return 1
		}
		fmt.Fprintln(os.Stdout, "outcome:", outcome)
		return 0

	default:
		fmt.Fprintln(os.Stderr, "helper: unknown case", caseName)
		return 9
	}
}

// buildCreateJournal returns the (not yet begun) journal for the "create"
// scenario: a fresh directory and a file inside it, both brand new.
func buildCreateJournal(txnID string) *Journal {
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	j := baseJournal(txnID)
	j.Steps = []Step{
		mkStep("s1", KindMkdir, "root/instance", StatusPending, PathState{Exists: false},
			PathState{Exists: true, Type: PathTypeDir, Mode: 0700, UID: uid, GID: gid}),
		mkStep("s2", KindWriteFile, "root/instance/config", StatusPending, PathState{Exists: false},
			fileStateFor(0600, uid, gid, []byte("hello"))),
	}
	j.Steps[1].Temps = []string{TempName(txnID, 1)}
	return j
}

// buildDeleteJournal returns the (not yet begun) journal for the "delete"
// scenario: a pre-existing directory (mode dirMode) and file, both removed
// by this transaction — dirID is root/instance's real, already-created
// identity (the caller creates root/instance and root/instance/config on
// disk itself, before BeginTransaction).
func buildDeleteJournal(txnID string, dirID DirIdentity, dirMode os.FileMode) *Journal {
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	oldContent := []byte("original-content")
	oldState := fileStateFor(0600, uid, gid, oldContent)
	j := baseJournal(txnID)
	j.DirIDs["root/instance"] = dirID
	j.Steps = []Step{
		mkStep("s1", KindSnapshot, "root/instance/config", StatusPending, oldState, oldState),
		mkStep("s2", KindRemoveFile, "root/instance/config", StatusPending, oldState, PathState{Exists: false}),
		mkStep("s3", KindRemoveDir, "root/instance", StatusPending,
			PathState{Exists: true, Type: PathTypeDir, Mode: uint32(dirMode.Perm()), UID: uid, GID: gid}, PathState{Exists: false}),
	}
	j.Steps[0].Temps = []string{TempName(txnID, 1)}
	return j
}

func dirIdentityRaw(root *TrustedRoot, name string) DirIdentity {
	f, err := os.Open(filepath.Join(root.Path(), name))
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper: open", name, ":", err)
		os.Exit(9)
	}
	defer f.Close()
	id, err := CurrentIdentity(f)
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper: CurrentIdentity:", err)
		os.Exit(9)
	}
	return id
}

func openDirRaw(path string) *os.File {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper: open", path, ":", err)
		os.Exit(9)
	}
	return f
}

// crashRecoverer builds a Recoverer over the given already-open engine, for
// either the parent test process or the "recover" helper case — both need
// exactly the same wiring (no systemd in either crash scenario). Scan
// directories are derived by Recoverer itself from the journal (R4 fix);
// this only needs to supply SnapDir when the transaction area's snap/
// directory exists.
func crashRecoverer(engine *Engine, root *TrustedRoot, rootPath string) *Recoverer {
	var snapDir *os.File
	if f, err := os.Open(filepath.Join(rootPath, "txn", "snap")); err == nil {
		snapDir = f
	}
	return &Recoverer{
		Engine:        engine,
		Root:          root,
		TombstoneName: ".txn.tomb-" + engine.Journal().TxnID,
		Resolve:       recResolver{root: root, rootKey: "root"},
		SnapDir:       snapDir,
	}
}

// runHelper re-execs this test binary with env set (plus the crash env
// vars), waiting for it to exit; it returns the exit code and captured
// stderr for a failing assertion's diagnostics. A crash point causes the
// helper to os.Exit(1) from inside the hook — indistinguishable, from the
// parent's point of view, from any other hard process termination at that
// exact point, which is the property this test needs.
func runHelper(t *testing.T, env map[string]string) (exitCode int, stderr string) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err == nil {
		return 0, errBuf.String()
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode(), errBuf.String()
	}
	t.Fatalf("run helper: %v (stderr: %s)", err, errBuf.String())
	return -1, errBuf.String()
}

func requireFileState(t *testing.T, path string, want PathState) {
	t.Helper()
	got := statPathOrMissing(t, path)
	if got != want {
		t.Fatalf("%s state = %+v, want %+v", path, got, want)
	}
}

// TestRecoveryCrashMatrix is described in this file's header comment.
// §14.2 requires umask 077 for these real-filesystem crash tests (proving
// the mkdir/write_file primitives' explicit Fchmod, not the ambient umask,
// is what determines final mode) — every subtest below runs under it.
func TestRecoveryCrashMatrix(t *testing.T) {
	withUmask(t, 0077)
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())

	t.Run("crash before journal.json ever exists (transaction-start)", func(t *testing.T) {
		for _, point := range []string{"before_create", "after_tmp_fsync"} {
			t.Run(point, func(t *testing.T) {
				base := t.TempDir()
				rootPath := filepath.Join(base, "root")
				if err := os.Mkdir(rootPath, 0700); err != nil {
					t.Fatal(err)
				}
				code, stderr := runHelper(t, map[string]string{
					envCrashCase: "start", envCrashRoot: rootPath, envCrashPoint: point, envCrashTxnID: "c1",
				})
				if code != 1 {
					t.Fatalf("helper exit code = %d, want 1 (crash at %s); stderr=%s", code, point, stderr)
				}
				root, err := OpenTrustedRoot(rootPath, int(uid))
				if err != nil {
					t.Fatalf("OpenTrustedRoot: %v", err)
				}
				defer root.Close()
				if err := RecoverEmptyTransactionArea(root, "txn"); err != nil {
					t.Fatalf("RecoverEmptyTransactionArea after crash at %s: %v", point, err)
				}
				requireNotExist(t, filepath.Join(rootPath, "txn"))
				requireNotExist(t, filepath.Join(rootPath, "instance"))
			})
		}
	})

	t.Run("crash mid forward primitive, one recover call completes it", func(t *testing.T) {
		// mkdir has already been called (its very first syscall already
		// ran) at every one of these points, so root/instance is always a
		// real managed path that got modified: every point must converge
		// to OutcomeRolledBack, never NoChange.
		points := []string{
			"mkdir:after_mkdirat", "mkdir:after_chmod", "mkdir:after_chown", "mkdir:after_identity",
			"write_file:after_create", "write_file:after_write", "write_file:after_rename", "write_file:after_dir_fsync",
		}
		for _, point := range points {
			t.Run(point, func(t *testing.T) {
				base := t.TempDir()
				rootPath := filepath.Join(base, "root")
				if err := os.Mkdir(rootPath, 0700); err != nil {
					t.Fatal(err)
				}
				code, stderr := runHelper(t, map[string]string{
					envCrashCase: "fwd_create", envCrashRoot: rootPath, envCrashPoint: point, envCrashTxnID: "c1",
				})
				if code != 1 {
					t.Fatalf("helper exit code = %d, want 1 (crash at %s); stderr=%s", code, point, stderr)
				}

				root, err := OpenTrustedRoot(rootPath, int(uid))
				if err != nil {
					t.Fatalf("OpenTrustedRoot: %v", err)
				}
				defer root.Close()
				store, err := OpenTransaction(root, "txn")
				if err != nil {
					t.Fatalf("OpenTransaction after crash at %s: %v", point, err)
				}
				engine := NewEngine(store)
				rec := crashRecoverer(engine, root, rootPath)
				outcome, err := rec.Recover(context.Background())
				if err != nil {
					t.Fatalf("Recover after crash at %s: %v", point, err)
				}
				if outcome != OutcomeRolledBack {
					t.Fatalf("Outcome after crash at %s = %v, want RolledBack (mkdir already ran)", point, outcome)
				}
				requireNotExist(t, filepath.Join(rootPath, "instance"))
				requireNotExist(t, filepath.Join(rootPath, "txn"))
				requireNotExist(t, filepath.Join(rootPath, ".txn.tomb-c1"))
			})
		}
	})

	t.Run("crash mid forward deletion, restores from snapshot", func(t *testing.T) {
		// snapshot never touches the real target (§7.2), so a crash still
		// inside the snapshot step must report NoChange even though the
		// step itself reached done — only once remove_file has actually
		// started has a real managed path been modified (RolledBack).
		wantOutcome := map[string]Outcome{
			"snapshot:after_verified_open":  OutcomeNoChange,
			"snapshot:after_create":         OutcomeNoChange,
			"snapshot:after_copy":           OutcomeNoChange,
			"snapshot:after_fsync":          OutcomeNoChange,
			"snapshot:after_rename":         OutcomeNoChange,
			"snapshot:after_dir_fsync":      OutcomeNoChange,
			"remove_file:after_unlink":      OutcomeRolledBack,
			"remove_file:after_fsync":       OutcomeRolledBack,
			"remove_dir:after_rmdir":        OutcomeRolledBack,
			"remove_dir:after_parent_fsync": OutcomeRolledBack,
			"remove_dir:after_identity":     OutcomeRolledBack,
		}
		for point, want := range wantOutcome {
			t.Run(point, func(t *testing.T) {
				base := t.TempDir()
				rootPath := filepath.Join(base, "root")
				if err := os.Mkdir(rootPath, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(rootPath, "instance"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(rootPath, "instance", "config"), []byte("original-content"), 0600); err != nil {
					t.Fatal(err)
				}
				code, stderr := runHelper(t, map[string]string{
					envCrashCase: "fwd_delete", envCrashRoot: rootPath, envCrashPoint: point, envCrashTxnID: "c1",
				})
				if code != 1 {
					t.Fatalf("helper exit code = %d, want 1 (crash at %s); stderr=%s", code, point, stderr)
				}

				root, err := OpenTrustedRoot(rootPath, int(uid))
				if err != nil {
					t.Fatalf("OpenTrustedRoot: %v", err)
				}
				defer root.Close()
				store, err := OpenTransaction(root, "txn")
				if err != nil {
					t.Fatalf("OpenTransaction after crash at %s: %v", point, err)
				}
				engine := NewEngine(store)
				rec := crashRecoverer(engine, root, rootPath)
				outcome, err := rec.Recover(context.Background())
				if err != nil {
					t.Fatalf("Recover after crash at %s: %v", point, err)
				}
				if outcome != want {
					t.Fatalf("Outcome after crash at %s = %v, want %v", point, outcome, want)
				}
				requireFileState(t, filepath.Join(rootPath, "instance", "config"), fileStateFor(0600, uid, gid, []byte("original-content")))
				requireNotExist(t, filepath.Join(rootPath, "txn"))
				requireNotExist(t, filepath.Join(rootPath, ".txn.tomb-c1"))
			})
		}
	})

	t.Run("recover itself crashes mid-rollback, a second recover converges", func(t *testing.T) {
		base := t.TempDir()
		rootPath := filepath.Join(base, "root")
		if err := os.Mkdir(rootPath, 0700); err != nil {
			t.Fatal(err)
		}

		// First: a forward crash, well past both steps completing (a
		// realistic "committed content, not yet marked done" state).
		code, stderr := runHelper(t, map[string]string{
			envCrashCase: "fwd_create", envCrashRoot: rootPath, envCrashPoint: "write_file:after_rename", envCrashTxnID: "c1",
		})
		if code != 1 {
			t.Fatalf("first helper exit code = %d; stderr=%s", code, stderr)
		}

		// Second: recover itself crashes mid-rollback (during the
		// rollback's own remove_file step, undoing s2's write_file).
		code, stderr = runHelper(t, map[string]string{
			envCrashCase: "recover", envCrashRoot: rootPath, envCrashPoint: "remove_file:after_unlink", envCrashTxnID: "c1",
		})
		if code != 1 {
			t.Fatalf("second helper (recover) exit code = %d, want 1; stderr=%s", code, stderr)
		}

		// Third: a clean recover must converge — proving recover's own
		// idempotence across a second crash (§14.2: "让recover自身在第k步
		// 崩溃,再执行一次recover,最终结果必须一致").
		code, stderr = runHelper(t, map[string]string{
			envCrashCase: "recover", envCrashRoot: rootPath, envCrashTxnID: "c1",
		})
		if code != 0 {
			t.Fatalf("third helper (recover) exit code = %d, want 0; stderr=%s", code, stderr)
		}

		requireNotExist(t, filepath.Join(rootPath, "instance"))
		requireNotExist(t, filepath.Join(rootPath, "txn"))
		requireNotExist(t, filepath.Join(rootPath, ".txn.tomb-c1"))
	})

	// T2: remove_dir's rollback (a reverse mkdir recreating the original
	// 0755 directory) crashed at each of its own internal points, under
	// umask 077 — the directory must still end up exactly 0755 (the
	// primitive's own Fchmod, not the ambient umask, must be what decides
	// the final mode), with dir_ids correctly registering its new inode
	// (R5) and the restored file's content/mode/owner intact.
	t.Run("rollback's reverse mkdir under umask 077 recreates the original 0755 directory", func(t *testing.T) {
		points := []string{"mkdir:after_mkdirat", "mkdir:after_chmod", "mkdir:after_chown", "mkdir:after_identity"}
		for _, point := range points {
			t.Run(point, func(t *testing.T) {
				base := t.TempDir()
				rootPath := filepath.Join(base, "root")
				if err := os.Mkdir(rootPath, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(rootPath, "instance"), 0755); err != nil {
					t.Fatal(err)
				}
				// The ambient umask 077 this whole test runs under would
				// otherwise silently reduce the raw os.Mkdir above to
				// 0700; force the fixture's own starting mode explicitly
				// rather than relying on Mkdir's mode argument surviving
				// umask (only the production Mkdir/RollbackMkdir
				// primitive's explicit Fchmod is required to be
				// umask-independent — TestUmaskIndependence — this test
				// fixture setup is not).
				if err := os.Chmod(filepath.Join(rootPath, "instance"), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(rootPath, "instance", "config"), []byte("original-content"), 0600); err != nil {
					t.Fatal(err)
				}
				// Forward runs to completion, uninterrupted: instance and
				// its file are both fully removed, phase stays running
				// (nothing commits it) — a legitimate state recover must
				// roll back.
				code, stderr := runHelper(t, map[string]string{
					envCrashCase: "fwd_delete", envCrashRoot: rootPath, envCrashTxnID: "c1", envCrashDirMode: "755",
				})
				if code != 0 {
					t.Fatalf("forward helper exit code = %d, want 0; stderr=%s", code, stderr)
				}

				// recover crashes inside its own rollback reconstruction.
				code, stderr = runHelper(t, map[string]string{
					envCrashCase: "recover", envCrashRoot: rootPath, envCrashPoint: point, envCrashTxnID: "c1",
				})
				if code != 1 {
					t.Fatalf("recover (crashing at %s) exit code = %d, want 1; stderr=%s", point, code, stderr)
				}

				// A clean recover must then converge.
				code, stderr = runHelper(t, map[string]string{
					envCrashCase: "recover", envCrashRoot: rootPath, envCrashTxnID: "c1",
				})
				if code != 0 {
					t.Fatalf("final recover exit code = %d, want 0 (crashed at %s); stderr=%s", code, point, stderr)
				}

				info, err := os.Stat(filepath.Join(rootPath, "instance"))
				if err != nil {
					t.Fatalf("instance directory missing after recover (crashed at %s): %v", point, err)
				}
				if info.Mode().Perm() != 0755 {
					t.Fatalf("instance mode = %v, want 0755 (crashed at %s; umask must not have weakened it)", info.Mode().Perm(), point)
				}
				requireFileState(t, filepath.Join(rootPath, "instance", "config"), fileStateFor(0600, uid, gid, []byte("original-content")))
				requireNotExist(t, filepath.Join(rootPath, "txn"))
				requireNotExist(t, filepath.Join(rootPath, ".txn.tomb-c1"))
			})
		}
	})
}
