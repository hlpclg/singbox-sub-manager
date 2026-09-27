package transaction

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func mustOpenDir(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func statManagedFile(t *testing.T, dir, name string) PathState {
	t.Helper()
	parent := mustOpenDir(t, dir)
	ps, err := statLeaf(parent, name, true)
	if err != nil {
		t.Fatalf("statLeaf %s/%s: %v", dir, name, err)
	}
	return ps
}

func statManagedDir(t *testing.T, dir, name string) (ps PathState, empty bool) {
	t.Helper()
	parent := mustOpenDir(t, dir)
	ps, err := statLeafDir(parent, name)
	if err != nil {
		t.Fatalf("statLeafDir %s/%s: %v", dir, name, err)
	}
	if ps.Exists {
		entries, rerr := os.ReadDir(filepath.Join(dir, name))
		if rerr != nil {
			t.Fatalf("readdir %s/%s: %v", dir, name, rerr)
		}
		empty = len(entries) == 0
	}
	return ps, empty
}

// setStepHook installs h for the duration of the calling (sub)test.
func setStepHook(t *testing.T, h func(point string) error) {
	t.Helper()
	old := stepHook
	stepHook = h
	t.Cleanup(func() { stepHook = old })
}

func TestPrimitiveCrashStates(t *testing.T) {
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())

	t.Run("write_file", func(t *testing.T) {
		points := []string{
			"write_file:after_create",
			"write_file:after_write",
			"write_file:after_chmod",
			"write_file:after_chown",
			"write_file:after_fsync",
			"write_file:after_rename",
			"write_file:after_dir_fsync",
		}
		for _, point := range points {
			t.Run(point, func(t *testing.T) {
				root, base := newTestRoot(t, "root")
				pre := PathState{Exists: false}
				post := fileStateFor(0600, uid, gid, []byte("hello"))
				j := baseJournal("t1")
				j.Steps = []Step{mkStep("s1", KindWriteFile, "root/config", StatusPending, pre, post)}
				j.Steps[0].Temps = []string{".proxyctl-txn-t1-1"}
				store, err := BeginTransaction(root, "txn", j)
				if err != nil {
					t.Fatalf("BeginTransaction: %v", err)
				}
				defer store.Close()
				engine := NewEngine(store)

				setStepHook(t, func(p string) error {
					if p == point {
						return errFakeCrash
					}
					return nil
				})
				mp := ManagedPath{Root: root, RootKey: "root", Leaf: "config"}
				err = engine.WriteFile(context.Background(), "s1", mp, ".proxyctl-txn-t1-1", []byte("hello"))
				if !errors.Is(err, errFakeCrash) {
					t.Fatalf("WriteFile error = %v, want errFakeCrash at %s", err, point)
				}

				// Target must be in {pre, post}: since content is only
				// written to the temp name before rename, the target
				// itself (before rename lands) stays "does not exist".
				live := statOrMissing(t, base, "config")
				chain := PathChain(store.Journal().Steps, "root/config")
				if err := DetermineRunningState(chain, live, false); err != nil {
					t.Fatalf("DetermineRunningState rejected the state left by a crash at %s: %v (live=%+v)", point, err, live)
				}
			})
		}
	})

	t.Run("mkdir", func(t *testing.T) {
		points := []string{"mkdir:after_mkdirat", "mkdir:after_parent_fsync", "mkdir:after_chmod", "mkdir:after_chown", "mkdir:after_identity"}
		for _, point := range points {
			t.Run(point, func(t *testing.T) {
				root, base := newTestRoot(t, "root")
				pre := PathState{Exists: false}
				post := PathState{Exists: true, Type: PathTypeDir, Mode: 0700, UID: uid, GID: gid}
				j := baseJournal("t1")
				j.Steps = []Step{mkStep("s1", KindMkdir, "root/instance", StatusPending, pre, post)}
				store, err := BeginTransaction(root, "txn", j)
				if err != nil {
					t.Fatalf("BeginTransaction: %v", err)
				}
				defer store.Close()
				engine := NewEngine(store)

				setStepHook(t, func(p string) error {
					if p == point {
						return errFakeCrash
					}
					return nil
				})
				mp := ManagedPath{Root: root, RootKey: "root", Leaf: "instance"}
				_, err = engine.Mkdir(context.Background(), "s1", mp)
				if !errors.Is(err, errFakeCrash) {
					t.Fatalf("Mkdir error = %v, want errFakeCrash at %s", err, point)
				}

				live, empty := statManagedDir(t, base, "instance")
				chain := PathChain(store.Journal().Steps, "root/instance")
				if err := DetermineRunningState(chain, live, empty); err != nil {
					t.Fatalf("DetermineRunningState rejected the state left by a crash at %s: %v (live=%+v empty=%v)", point, err, live, empty)
				}
				if status := store.Journal().Steps[0].Status; status != StatusStarted {
					t.Fatalf("step status = %s, want started at %s (review P1#1: done must never be reached before this crash)", status, point)
				}
				// dir_ids must be registered starting at (and including)
				// mkdir:after_identity, and not before (review P1#1).
				_, hasIdentity := store.Journal().DirIDs["root/instance"]
				wantIdentity := point == "mkdir:after_identity"
				if hasIdentity != wantIdentity {
					t.Fatalf("dir_ids[root/instance] registered=%v at %s, want %v", hasIdentity, point, wantIdentity)
				}
			})
		}
	})

	t.Run("set_meta", func(t *testing.T) {
		points := []string{"set_meta:after_chown", "set_meta:after_chmod"}
		for _, point := range points {
			t.Run(point, func(t *testing.T) {
				root, base := newTestRoot(t, "root")
				if err := os.WriteFile(filepath.Join(base, "config"), []byte("hello"), 0644); err != nil {
					t.Fatal(err)
				}
				pre := fileStateFor(0644, uid, gid, []byte("hello"))
				post := pre
				post.Mode = 0600
				j := baseJournal("t1")
				j.Steps = []Step{mkStep("s1", KindSetMeta, "root/config", StatusPending, pre, post)}
				store, err := BeginTransaction(root, "txn", j)
				if err != nil {
					t.Fatalf("BeginTransaction: %v", err)
				}
				defer store.Close()
				engine := NewEngine(store)

				setStepHook(t, func(p string) error {
					if p == point {
						return errFakeCrash
					}
					return nil
				})
				mp := ManagedPath{Root: root, RootKey: "root", Leaf: "config"}
				err = engine.SetMeta(context.Background(), "s1", mp)
				if !errors.Is(err, errFakeCrash) {
					t.Fatalf("SetMeta error = %v, want errFakeCrash at %s", err, point)
				}
				live := statManagedFile(t, base, "config")
				chain := PathChain(store.Journal().Steps, "root/config")
				if err := DetermineRunningState(chain, live, false); err != nil {
					t.Fatalf("DetermineRunningState rejected the state left by a crash at %s: %v (live=%+v)", point, err, live)
				}
			})
		}
	})

	t.Run("set_meta chmod does not follow a leaf swapped to a symlink between chown and chmod", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		if err := os.WriteFile(filepath.Join(base, "config"), []byte("hello"), 0644); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "victim")
		if err := os.WriteFile(outside, []byte("victim"), 0644); err != nil {
			t.Fatal(err)
		}
		pre := fileStateFor(0644, uid, gid, []byte("hello"))
		post := pre
		post.Mode = 0600
		j := baseJournal("t1")
		j.Steps = []Step{mkStep("s1", KindSetMeta, "root/config", StatusPending, pre, post)}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		defer store.Close()
		engine := NewEngine(store)

		setStepHook(t, func(p string) error {
			if p == "set_meta:after_chown" {
				if err := os.Remove(filepath.Join(base, "config")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(base, "config")); err != nil {
					t.Fatal(err)
				}
			}
			return nil
		})
		mp := ManagedPath{Root: root, RootKey: "root", Leaf: "config"}
		err = engine.SetMeta(context.Background(), "s1", mp)
		if err == nil {
			t.Fatalf("SetMeta must reject a leaf that became a symlink before chmod")
		}
		victimInfo, statErr := os.Stat(outside)
		if statErr != nil {
			t.Fatalf("stat victim: %v", statErr)
		}
		if victimInfo.Mode().Perm() != 0644 {
			t.Fatalf("victim file outside the root was chmod'd to %v; chmod must not follow the swapped-in symlink", victimInfo.Mode().Perm())
		}
	})

	t.Run("remove_file", func(t *testing.T) {
		points := []string{"remove_file:after_unlink", "remove_file:after_fsync"}
		for _, point := range points {
			t.Run(point, func(t *testing.T) {
				root, base := newTestRoot(t, "root")
				if err := os.WriteFile(filepath.Join(base, "config"), []byte("hello"), 0600); err != nil {
					t.Fatal(err)
				}
				pre := fileStateFor(0600, uid, gid, []byte("hello"))
				post := PathState{Exists: false}
				j := baseJournal("t1")
				j.Steps = []Step{mkStep("s1", KindRemoveFile, "root/config", StatusPending, pre, post)}
				store, err := BeginTransaction(root, "txn", j)
				if err != nil {
					t.Fatalf("BeginTransaction: %v", err)
				}
				defer store.Close()
				engine := NewEngine(store)

				setStepHook(t, func(p string) error {
					if p == point {
						return errFakeCrash
					}
					return nil
				})
				mp := ManagedPath{Root: root, RootKey: "root", Leaf: "config"}
				err = engine.RemoveFile(context.Background(), "s1", mp)
				if !errors.Is(err, errFakeCrash) {
					t.Fatalf("RemoveFile error = %v, want errFakeCrash at %s", err, point)
				}
				live := statOrMissing(t, base, "config")
				chain := PathChain(store.Journal().Steps, "root/config")
				if err := DetermineRunningState(chain, live, false); err != nil {
					t.Fatalf("DetermineRunningState rejected the state left by a crash at %s: %v (live=%+v)", point, err, live)
				}
			})
		}
	})

	t.Run("remove_dir", func(t *testing.T) {
		points := []string{"remove_dir:after_rmdir", "remove_dir:after_parent_fsync", "remove_dir:after_identity"}
		for _, point := range points {
			t.Run(point, func(t *testing.T) {
				root, base := newTestRoot(t, "root")
				if err := os.Mkdir(filepath.Join(base, "old"), 0700); err != nil {
					t.Fatal(err)
				}
				pre := PathState{Exists: true, Type: PathTypeDir, Mode: 0700, UID: uid, GID: gid}
				post := PathState{Exists: false}
				j := baseJournal("t1")
				j.DirIDs["root/old"] = identityOfPath(t, filepath.Join(base, "old"))
				j.Steps = []Step{mkStep("s1", KindRemoveDir, "root/old", StatusPending, pre, post)}
				store, err := BeginTransaction(root, "txn", j)
				if err != nil {
					t.Fatalf("BeginTransaction: %v", err)
				}
				defer store.Close()
				engine := NewEngine(store)

				setStepHook(t, func(p string) error {
					if p == point {
						return errFakeCrash
					}
					return nil
				})
				mp := ManagedPath{Root: root, RootKey: "root", Leaf: "old"}
				err = engine.RemoveDir(context.Background(), "s1", mp)
				if !errors.Is(err, errFakeCrash) {
					t.Fatalf("RemoveDir error = %v, want errFakeCrash at %s", err, point)
				}
				if exists(filepath.Join(base, "old")) {
					t.Fatalf("rmdir did not actually remove the directory")
				}
				if status := store.Journal().Steps[0].Status; status != StatusStarted {
					t.Fatalf("step status = %s, want started at %s (review P1#1)", status, point)
				}
				recorded := store.Journal().DirIDs["root/old"]
				wantUpdated := point == "remove_dir:after_identity"
				if recorded.Exists == wantUpdated {
					t.Fatalf("dir_ids[root/old] = %+v at %s, want Exists=%v (updated only at/after remove_dir:after_identity)", recorded, point, !wantUpdated)
				}
				chain := PathChain(store.Journal().Steps, "root/old")
				if err := DetermineRunningState(chain, PathState{Exists: false}, false); err != nil {
					t.Fatalf("DetermineRunningState rejected the state left by a crash at %s: %v", point, err)
				}
			})
		}
	})

	t.Run("snapshot", func(t *testing.T) {
		points := []string{
			"snapshot:after_verified_open",
			"snapshot:after_create",
			"snapshot:after_copy",
			"snapshot:after_fsync",
			"snapshot:after_rename",
			"snapshot:after_dir_fsync",
		}
		newSnapshotFixture := func(t *testing.T) (*Engine, *Store, ManagedPath, *os.File, string) {
			t.Helper()
			root, base := newTestRoot(t, "root")
			content := []byte("secret-data")
			if err := os.WriteFile(filepath.Join(base, "config"), content, 0600); err != nil {
				t.Fatal(err)
			}
			srcState := fileStateFor(0600, uid, gid, content)
			j := baseJournal("t1")
			// snapshot never modifies its target (review P2#6): Pre and
			// Post are both the source's own unchanged state.
			j.Steps = []Step{mkStep("s1", KindSnapshot, "root/config", StatusPending, srcState, srcState)}
			j.Steps[0].Temps = []string{".proxyctl-txn-t1-1"}
			store, err := BeginTransaction(root, "txn", j)
			if err != nil {
				t.Fatalf("BeginTransaction: %v", err)
			}
			t.Cleanup(func() { store.Close() })
			if err := os.Mkdir(filepath.Join(getTxnPath(base), "snap"), 0700); err != nil {
				t.Fatalf("mkdir snap: %v", err)
			}
			snapDir := mustOpenDir(t, filepath.Join(getTxnPath(base), "snap"))
			mp := ManagedPath{Root: root, RootKey: "root", Leaf: "config"}
			return NewEngine(store), store, mp, snapDir, base
		}

		for _, point := range points {
			t.Run(point, func(t *testing.T) {
				engine, store, mp, snapDir, base := newSnapshotFixture(t)
				setStepHook(t, func(p string) error {
					if p == point {
						return errFakeCrash
					}
					return nil
				})
				err := engine.Snapshot(context.Background(), "s1", mp, snapDir, ".proxyctl-txn-t1-1")
				if !errors.Is(err, errFakeCrash) {
					t.Fatalf("Snapshot error = %v, want errFakeCrash at %s", err, point)
				}
				// The source file itself must never be modified by a
				// snapshot, at any interruption point (review P1#3).
				data, rerr := os.ReadFile(filepath.Join(base, "config"))
				if rerr != nil || string(data) != "secret-data" {
					t.Fatalf("snapshot source was modified: data=%q err=%v", data, rerr)
				}
				if status := store.Journal().Steps[0].Status; status != StatusStarted {
					t.Fatalf("step status = %s, want started at %s", status, point)
				}
			})
		}

		t.Run("succeeds and publishes the snapshot file when uninterrupted", func(t *testing.T) {
			engine, store, mp, snapDir, _ := newSnapshotFixture(t)
			if err := engine.Snapshot(context.Background(), "s1", mp, snapDir, ".proxyctl-txn-t1-1"); err != nil {
				t.Fatalf("Snapshot: %v", err)
			}
			if status := store.Journal().Steps[0].Status; status != StatusDone {
				t.Fatalf("step status = %s, want done", status)
			}
			snapContent, err := os.ReadFile(filepath.Join(snapDir.Name(), ".proxyctl-txn-t1-1.snap"))
			if err != nil {
				t.Fatalf("read published snapshot: %v", err)
			}
			if string(snapContent) != "secret-data" {
				t.Fatalf("snapshot content = %q, want secret-data", snapContent)
			}
		})

		t.Run("rejects a source whose live content does not match Pre, without copying anything (review P1#3c)", func(t *testing.T) {
			engine, store, mp, snapDir, base := newSnapshotFixture(t)
			// Tamper the source after the journal already recorded Pre
			// for the untampered content — the plan's Pre is now stale.
			if err := os.WriteFile(filepath.Join(base, "config"), []byte("tampered!!"), 0600); err != nil {
				t.Fatal(err)
			}
			err := engine.Snapshot(context.Background(), "s1", mp, snapDir, ".proxyctl-txn-t1-1")
			if !errors.Is(err, ErrPreStateMismatch) {
				t.Fatalf("Snapshot error = %v, want ErrPreStateMismatch", err)
			}
			if status := store.Journal().Steps[0].Status; status != StatusStarted {
				t.Fatalf("step status = %s, want started (rejected before any copy)", status)
			}
			entries, err := os.ReadDir(snapDir.Name())
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("snap/ directory has %d entries, want 0 — a rejected pre-check must not have created anything", len(entries))
			}
		})

		t.Run("a leaf that is a FIFO is rejected immediately, not hung (review P2#9)", func(t *testing.T) {
			root, base := newTestRoot(t, "root")
			fifoPath := filepath.Join(base, "config")
			if err := unix.Mkfifo(fifoPath, 0600); err != nil {
				t.Fatalf("mkfifo: %v", err)
			}
			srcState := PathState{Exists: true, Type: PathTypeFile, Mode: 0600, UID: uid, GID: gid}
			j := baseJournal("t1")
			j.Steps = []Step{mkStep("s1", KindSnapshot, "root/config", StatusPending, srcState, srcState)}
			j.Steps[0].Temps = []string{".proxyctl-txn-t1-1"}
			store, err := BeginTransaction(root, "txn", j)
			if err != nil {
				t.Fatalf("BeginTransaction: %v", err)
			}
			defer store.Close()
			if err := os.Mkdir(filepath.Join(getTxnPath(base), "snap"), 0700); err != nil {
				t.Fatalf("mkdir snap: %v", err)
			}
			snapDir := mustOpenDir(t, filepath.Join(getTxnPath(base), "snap"))
			mp := ManagedPath{Root: root, RootKey: "root", Leaf: "config"}
			engine := NewEngine(store)

			done := make(chan error, 1)
			go func() { done <- engine.Snapshot(context.Background(), "s1", mp, snapDir, ".proxyctl-txn-t1-1") }()
			select {
			case err := <-done:
				if !errors.Is(err, ErrIllegalPathType) {
					t.Fatalf("Snapshot error = %v, want ErrIllegalPathType", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("Snapshot on a FIFO did not return within 5s — it hung waiting for a writer")
			}
		})
	})

	t.Run("disallowed states are rejected by DetermineRunningState", func(t *testing.T) {
		pre := PathState{Exists: false}
		post := fileStateFor(0600, uid, gid, []byte("hello"))
		chain := []Step{mkStep("s1", KindWriteFile, "config", StatusStarted, pre, post)}

		t.Run("all pending but state is not pre", func(t *testing.T) {
			pendingChain := []Step{mkStep("s1", KindWriteFile, "config", StatusPending, pre, post)}
			wrong := PathState{Exists: true, Type: PathTypeFile, Mode: 0644, UID: uid, GID: gid, SHA256: "deadbeef"}
			if err := DetermineRunningState(pendingChain, wrong, false); err == nil {
				t.Fatalf("accepted a state that does not match pre while all pending")
			}
		})

		t.Run("done but state does not match post", func(t *testing.T) {
			doneChain := []Step{mkStep("s1", KindWriteFile, "config", StatusDone, pre, post)}
			wrong := PathState{Exists: true, Type: PathTypeFile, Mode: 0644, UID: uid, GID: gid, SHA256: "deadbeef"}
			if err := DetermineRunningState(doneChain, wrong, false); err == nil {
				t.Fatalf("accepted a state that does not match post for a done step")
			}
		})

		t.Run("write_file started but state is neither pre nor post", func(t *testing.T) {
			wrong := PathState{Exists: true, Type: PathTypeFile, Mode: 0644, UID: uid, GID: gid, SHA256: "deadbeef"}
			if err := DetermineRunningState(chain, wrong, false); err == nil {
				t.Fatalf("accepted a state outside write_file's allowed set")
			}
		})

		t.Run("write_file started but target became a symlink", func(t *testing.T) {
			// Represented as an illegal type at the caller (statLeaf
			// itself returns ErrIllegalPathType before DetermineRunningState
			// would even be reached) — covered by TestAncestorReplacement's
			// leaf-symlink case; DetermineRunningState itself only ever
			// receives PathState values statLeaf/statLeafDir already
			// restricted to legal shapes, by design.
		})

		t.Run("unregistered temp name present is not in the allowed set", func(t *testing.T) {
			root, base := newTestRoot(t, "root")
			if err := os.WriteFile(filepath.Join(base, ".proxyctl-txn-other-1"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			_ = root
			registered := map[string]bool{".proxyctl-txn-t1-1": true}
			found, err := scanUnregisteredTemps(mustOpenDir(t, base), registered)
			if err != nil {
				t.Fatalf("scanUnregisteredTemps: %v", err)
			}
			if len(found) != 1 || found[0] != ".proxyctl-txn-other-1" {
				t.Fatalf("scanUnregisteredTemps = %v, want [.proxyctl-txn-other-1]", found)
			}
		})
	})
}

func sha256HexOf(data []byte) string { return sha256Hex(nil, data) }

// fileStateFor builds the PathState a regular file with this exact content
// and metadata would produce from statLeaf — Size must track the content's
// length or PathState equality (used throughout §7.3's judgement) never
// matches an on-disk file, whatever its content hash says.
func fileStateFor(mode os.FileMode, uid, gid uint32, data []byte) PathState {
	return PathState{
		Exists: true,
		Type:   PathTypeFile,
		Size:   int64(len(data)),
		Mode:   uint32(mode.Perm()),
		UID:    uid,
		GID:    gid,
		SHA256: sha256HexOf(data),
	}
}

func statOrMissing(t *testing.T, dir, name string) PathState {
	t.Helper()
	parent := mustOpenDir(t, dir)
	ps, err := statLeaf(parent, name, true)
	if err != nil {
		t.Fatalf("statLeaf %s/%s: %v", dir, name, err)
	}
	return ps
}

// getTxnPath is a test-only convenience: the transaction area created by
// BeginTransaction(root, "txn", ...) in these tests always lives directly
// under root's own path, named "txn".
func getTxnPath(rootPath string) string { return filepath.Join(rootPath, "txn") }

// TestUmaskIndependence covers review P1#4/P2#8: under a restrictive
// umask, mkdir's and write_file's final mode must equal Post exactly —
// proving the explicit Fchmod each primitive performs on its own opened
// fd is what fixes the mode up, not any unix.Umask(0) suppression around
// the creating syscall (which review P2#8 asks removed, since it is
// process-global state that would otherwise race concurrent goroutines'
// own file/directory creation for no benefit once that Fchmod exists).
func TestUmaskIndependence(t *testing.T) {
	withUmask(t, 0077)
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())

	t.Run("mkdir mode 0755 survives umask 077", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		pre := PathState{Exists: false}
		post := PathState{Exists: true, Type: PathTypeDir, Mode: 0755, UID: uid, GID: gid}
		j := baseJournal("t1")
		j.Steps = []Step{mkStep("s1", KindMkdir, "root/instance", StatusPending, pre, post)}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		defer store.Close()
		engine := NewEngine(store)
		mp := ManagedPath{Root: root, RootKey: "root", Leaf: "instance"}
		dirFd, err := engine.Mkdir(context.Background(), "s1", mp)
		if err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		defer dirFd.Close()
		info, err := os.Stat(filepath.Join(base, "instance"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0755 {
			t.Fatalf("directory mode = %v, want 0755 (umask must not have weakened it)", info.Mode().Perm())
		}
	})

	t.Run("write_file mode 0644 survives umask 077", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		pre := PathState{Exists: false}
		post := fileStateFor(0644, uid, gid, []byte("hello"))
		j := baseJournal("t1")
		j.Steps = []Step{mkStep("s1", KindWriteFile, "root/config", StatusPending, pre, post)}
		j.Steps[0].Temps = []string{".proxyctl-txn-t1-1"}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		defer store.Close()
		engine := NewEngine(store)
		mp := ManagedPath{Root: root, RootKey: "root", Leaf: "config"}
		if err := engine.WriteFile(context.Background(), "s1", mp, ".proxyctl-txn-t1-1", []byte("hello")); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		info, err := os.Stat(filepath.Join(base, "config"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0644 {
			t.Fatalf("file mode = %v, want 0644 (umask must not have weakened it)", info.Mode().Perm())
		}
	})

	t.Run("transaction area itself (mkdirManaged, mode 0700) survives umask 077", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		j := baseJournal("t1")
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		defer store.Close()
		info, err := os.Stat(filepath.Join(base, "txn"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0700 {
			t.Fatalf("transaction area mode = %v, want 0700", info.Mode().Perm())
		}
	})
}

// TestPrimitiveCancellation covers plan G7: every primitive checks ctx
// immediately before its next modifying syscall and performs none of them
// once canceled.
func TestPrimitiveCancellation(t *testing.T) {
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	root, base := newTestRoot(t, "root")
	pre := PathState{Exists: false}
	post := fileStateFor(0600, uid, gid, []byte("hello"))
	j := baseJournal("t1")
	j.Steps = []Step{mkStep("s1", KindWriteFile, "root/config", StatusPending, pre, post)}
	j.Steps[0].Temps = []string{".proxyctl-txn-t1-1"}
	store, err := BeginTransaction(root, "txn", j)
	if err != nil {
		t.Fatalf("BeginTransaction: %v", err)
	}
	defer store.Close()
	engine := NewEngine(store)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mp := ManagedPath{Root: root, RootKey: "root", Leaf: "config"}
	err = engine.WriteFile(ctx, "s1", mp, ".proxyctl-txn-t1-1", []byte("hello"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteFile error = %v, want context.Canceled", err)
	}
	if exists(filepath.Join(base, "config")) {
		t.Fatalf("a canceled WriteFile must not have created the target")
	}
	// The step was already marked started (registering intent) before the
	// first cancellation check inside the primitive body — canceling
	// before even that first mark is not distinguishable from canceling a
	// microsecond later during lock acquisition upstream of this package,
	// so it is not this package's job to special-case it; what matters is
	// that no modifying syscall ran afterward, which the file's absence
	// above already proves.
}

func TestPathChainStates(t *testing.T) {
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())

	t.Run("snapshot then write_file on the same path: chain determination uses only the write_file step", func(t *testing.T) {
		pre := fileStateFor(0600, uid, gid, []byte("old"))
		post := fileStateFor(0600, uid, gid, []byte("new"))
		j := baseJournal("t1")
		j.Steps = []Step{
			// snapshot never modifies its target (review P2#6): Pre==Post,
			// both equal to the pre-overwrite content it captured.
			mkStep("snap1", KindSnapshot, "config", StatusDone, pre, pre),
			mkStep("wf1", KindWriteFile, "config", StatusStarted, pre, post),
		}
		if err := j.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		chain := PathChain(j.Steps, "config")
		if err := DetermineRunningState(chain, pre, false); err != nil {
			t.Fatalf("DetermineRunningState: %v", err)
		}
		if err := DetermineRunningState(chain, post, false); err != nil {
			t.Fatalf("DetermineRunningState: %v", err)
		}
	})

	t.Run("mkdir then remove_dir on the same path: S(k+1).pre must equal Sk.post", func(t *testing.T) {
		dirPre := PathState{Exists: false}
		dirMid := PathState{Exists: true, Type: PathTypeDir, Mode: 0700, UID: uid, GID: gid}
		j := baseJournal("t1")
		j.Steps = []Step{
			mkStep("mk1", KindMkdir, "staging", StatusDone, dirPre, dirMid),
			mkStep("rm1", KindRemoveDir, "staging", StatusStarted, dirMid, PathState{Exists: false}),
		}
		if err := j.Validate(); err != nil {
			t.Fatalf("legal chain rejected: %v", err)
		}

		broken := baseJournal("t1")
		broken.Steps = []Step{
			mkStep("mk1", KindMkdir, "staging", StatusDone, dirPre, dirMid),
			mkStep("rm1", KindRemoveDir, "staging", StatusStarted, dirPre /* wrong: should be dirMid */, PathState{Exists: false}),
		}
		if err := broken.Validate(); !errors.Is(err, ErrIllegalProgress) {
			t.Fatalf("Validate error = %v, want ErrIllegalProgress for a broken pre/post chain", err)
		}

		chain := PathChain(j.Steps, "staging")
		if err := DetermineRunningState(chain, dirMid, true); err != nil {
			t.Fatalf("DetermineRunningState rejected the legal in-between state: %v", err)
		}
	})

	t.Run("multiple distinct targets do not interfere with each other's chains", func(t *testing.T) {
		j := baseJournal("t1")
		fileA := fileStateFor(0600, uid, gid, []byte("a"))
		fileB := fileStateFor(0600, uid, gid, []byte("b"))
		j.Steps = []Step{
			mkStep("s1", KindWriteFile, "a", StatusDone, PathState{Exists: false}, fileA),
			mkStep("s2", KindWriteFile, "b", StatusStarted, PathState{Exists: false}, fileB),
		}
		if err := j.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		chainA := PathChain(j.Steps, "a")
		chainB := PathChain(j.Steps, "b")
		if len(chainA) != 1 || len(chainB) != 1 {
			t.Fatalf("PathChain lengths = %d, %d, want 1, 1", len(chainA), len(chainB))
		}
		if err := DetermineRunningState(chainA, fileA, false); err != nil {
			t.Fatalf("chain a: %v", err)
		}
		if err := DetermineRunningState(chainB, PathState{Exists: false}, false); err != nil {
			t.Fatalf("chain b (all pending... wait started): %v", err)
		}
	})
}

// TestPreStateEnforcement covers review P1#2's exact regression scenarios:
// a primitive must never modify a target whose live state disagrees with
// the step's own Pre, because that live state was never snapshotted (or
// verified) as what the plan assumes it is restoring from.
func TestPreStateEnforcement(t *testing.T) {
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())

	t.Run("write_file refuses to overwrite an unexpected existing file instead of silently destroying its content", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		// Pre claims the target does not exist yet, but a real file with
		// real content is already there and was never snapshotted.
		if err := os.WriteFile(filepath.Join(base, "config"), []byte("unexpected-real-data"), 0644); err != nil {
			t.Fatal(err)
		}
		pre := PathState{Exists: false}
		post := fileStateFor(0600, uid, gid, []byte("new-data"))
		j := baseJournal("t1")
		j.Steps = []Step{mkStep("s1", KindWriteFile, "root/config", StatusPending, pre, post)}
		j.Steps[0].Temps = []string{".proxyctl-txn-t1-1"}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		defer store.Close()
		engine := NewEngine(store)
		mp := ManagedPath{Root: root, RootKey: "root", Leaf: "config"}
		err = engine.WriteFile(context.Background(), "s1", mp, ".proxyctl-txn-t1-1", []byte("new-data"))
		if !errors.Is(err, ErrPreStateMismatch) {
			t.Fatalf("WriteFile error = %v, want ErrPreStateMismatch", err)
		}
		data, rerr := os.ReadFile(filepath.Join(base, "config"))
		if rerr != nil || string(data) != "unexpected-real-data" {
			t.Fatalf("original content was destroyed: data=%q err=%v", data, rerr)
		}
		if status := store.Journal().Steps[0].Status; status != StatusStarted {
			t.Fatalf("step status = %s, want started (rejected before rename, not rolled back)", status)
		}
	})

	t.Run("write_file rejects when the target changed between the first check and the rename", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		pre := PathState{Exists: false}
		post := fileStateFor(0600, uid, gid, []byte("new-data"))
		j := baseJournal("t1")
		j.Steps = []Step{mkStep("s1", KindWriteFile, "root/config", StatusPending, pre, post)}
		j.Steps[0].Temps = []string{".proxyctl-txn-t1-1"}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		defer store.Close()
		engine := NewEngine(store)
		setStepHook(t, func(p string) error {
			if p == "write_file:after_fsync" {
				// An external actor creates the target for the first
				// time right before the rename that was about to.
				if err := os.WriteFile(filepath.Join(base, "config"), []byte("raced-in"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			return nil
		})
		mp := ManagedPath{Root: root, RootKey: "root", Leaf: "config"}
		err = engine.WriteFile(context.Background(), "s1", mp, ".proxyctl-txn-t1-1", []byte("new-data"))
		if !errors.Is(err, ErrPreStateMismatch) {
			t.Fatalf("WriteFile error = %v, want ErrPreStateMismatch", err)
		}
		data, rerr := os.ReadFile(filepath.Join(base, "config"))
		if rerr != nil || string(data) != "raced-in" {
			t.Fatalf("raced-in content was destroyed: data=%q err=%v", data, rerr)
		}
	})

	t.Run("remove_file refuses to delete a file whose content does not match what the plan snapshotted", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		if err := os.WriteFile(filepath.Join(base, "config"), []byte("different-from-snapshot"), 0600); err != nil {
			t.Fatal(err)
		}
		// Pre claims the snapshotted content was "original", but the live
		// file now holds something else entirely.
		pre := fileStateFor(0600, uid, gid, []byte("original"))
		post := PathState{Exists: false}
		j := baseJournal("t1")
		j.Steps = []Step{mkStep("s1", KindRemoveFile, "root/config", StatusPending, pre, post)}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		defer store.Close()
		engine := NewEngine(store)
		mp := ManagedPath{Root: root, RootKey: "root", Leaf: "config"}
		err = engine.RemoveFile(context.Background(), "s1", mp)
		if !errors.Is(err, ErrPreStateMismatch) {
			t.Fatalf("RemoveFile error = %v, want ErrPreStateMismatch", err)
		}
		if !exists(filepath.Join(base, "config")) {
			t.Fatalf("remove_file must not have deleted a file that did not match Pre")
		}
	})

	t.Run("mkdir refuses to proceed when the target already exists", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		if err := os.Mkdir(filepath.Join(base, "instance"), 0755); err != nil {
			t.Fatal(err)
		}
		pre := PathState{Exists: false}
		post := PathState{Exists: true, Type: PathTypeDir, Mode: 0700, UID: uid, GID: gid}
		j := baseJournal("t1")
		j.Steps = []Step{mkStep("s1", KindMkdir, "root/instance", StatusPending, pre, post)}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		defer store.Close()
		engine := NewEngine(store)
		mp := ManagedPath{Root: root, RootKey: "root", Leaf: "instance"}
		_, err = engine.Mkdir(context.Background(), "s1", mp)
		if !errors.Is(err, ErrPreStateMismatch) {
			t.Fatalf("Mkdir error = %v, want ErrPreStateMismatch", err)
		}
		info, statErr := os.Stat(filepath.Join(base, "instance"))
		if statErr != nil || info.Mode().Perm() != 0755 {
			t.Fatalf("pre-existing directory must be untouched: %v %v", info, statErr)
		}
	})
}

// TestManagedPathValidation covers review P1#11: ManagedPath's components
// and Keys must be internally consistent and never let an operation
// escape its intended root or diverge from the journal step it claims to
// execute.
func TestManagedPathValidation(t *testing.T) {
	root, _ := newTestRoot(t, "root")
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())

	illegalComponents := []string{"", ".", "..", "a/b"}
	for _, c := range illegalComponents {
		t.Run("rejects illegal Leaf component "+c, func(t *testing.T) {
			mp := ManagedPath{Root: root, RootKey: "root", Leaf: c}
			if err := mp.validate(); !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("validate() error = %v, want ErrUnsafePath for Leaf %q", err, c)
			}
		})
		t.Run("rejects illegal Comps component "+c, func(t *testing.T) {
			mp := ManagedPath{Root: root, RootKey: "root", Comps: []string{c}, Keys: []string{"root/" + c}, Leaf: "config"}
			if err := mp.validate(); !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("validate() error = %v, want ErrUnsafePath for component %q", err, c)
			}
		})
	}

	t.Run("rejects a Keys entry that does not match RootKey/Comps", func(t *testing.T) {
		mp := ManagedPath{Root: root, RootKey: "root", Comps: []string{"token"}, Keys: []string{"wrong-key"}, Leaf: "nodes.conf"}
		if err := mp.validate(); err == nil {
			t.Fatalf("validate() accepted a Keys entry that does not match RootKey/Comps")
		}
	})

	t.Run("rejects an empty RootKey", func(t *testing.T) {
		mp := ManagedPath{Root: root, Leaf: "config"}
		if err := mp.validate(); err == nil {
			t.Fatalf("validate() accepted an empty RootKey")
		}
	})

	t.Run("Engine rejects a ManagedPath whose computed TargetKey does not match the step's own Target", func(t *testing.T) {
		pre := PathState{Exists: false}
		post := fileStateFor(0600, uid, gid, []byte("hello"))
		j := baseJournal("t1")
		j.Steps = []Step{mkStep("s1", KindWriteFile, "root/config", StatusPending, pre, post)}
		j.Steps[0].Temps = []string{".proxyctl-txn-t1-1"}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		defer store.Close()
		engine := NewEngine(store)
		// mp resolves to "root/other-name", not "root/config" as the step
		// claims — this must be rejected before anything is touched,
		// rather than silently operating on "other-name" while the
		// journal still says it planned "config".
		mp := ManagedPath{Root: root, RootKey: "root", Leaf: "other-name"}
		err = engine.WriteFile(context.Background(), "s1", mp, ".proxyctl-txn-t1-1", []byte("hello"))
		if err == nil {
			t.Fatalf("WriteFile accepted a ManagedPath that does not match the step's Target")
		}
		if status := store.Journal().Steps[0].Status; status != StatusPending {
			t.Fatalf("step status = %s, want pending (rejected before any transition)", status)
		}
	})
}

// fakeSchemaDetector is a trivial SchemaDetector for tests: content of the
// form "SCHEMA:<name>\n..." reports schema name <name>; anything else is
// an error. Real schema parsing (secrets.json, instance.json) belongs to
// Task 6+'s internal/realitynode — this package only needs to prove the
// injected-function seam itself works.
func fakeSchemaDetector(_ string, data []byte) (string, error) {
	const prefix = "SCHEMA:"
	s := string(data)
	if !strings.HasPrefix(s, prefix) {
		return "", fmt.Errorf("fakeSchemaDetector: content does not start with %q", prefix)
	}
	rest := s[len(prefix):]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[:nl]
	}
	return rest, nil
}

// TestSchemaDetection covers review round 2, item 1: a dynamic-state
// PathState (Schema set instead of SHA256) must go through the same
// pre-state enforcement as a content-hashed one, via an injected
// SchemaDetector — not be silently skipped.
func TestSchemaDetection(t *testing.T) {
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	// schemaState mirrors fileStateFor for dynamic-state PathStates: Size
	// must track the live file's real content length (statLeaf/openLeafFile
	// always populates it from a real stat), or PathState equality never
	// matches, whatever the schema name says.
	schemaState := func(mode os.FileMode, schema string, size int) PathState {
		return PathState{Exists: true, Type: PathTypeFile, Size: int64(size), Mode: uint32(mode.Perm()), UID: uid, GID: gid, Schema: schema}
	}

	t.Run("write_file succeeds against a schema-type Pre when a detector is installed", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		oldContent := []byte("SCHEMA:instance-v1\nold-content")
		newContent := []byte("SCHEMA:instance-v1\nnew-content")
		if err := os.WriteFile(filepath.Join(base, "instance.json"), oldContent, 0600); err != nil {
			t.Fatal(err)
		}
		pre := schemaState(0600, "instance-v1", len(oldContent))
		post := schemaState(0600, "instance-v1", len(newContent))
		j := baseJournal("t1")
		j.Steps = []Step{mkStep("s1", KindWriteFile, "root/instance.json", StatusPending, pre, post)}
		j.Steps[0].Temps = []string{".proxyctl-txn-t1-1"}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		defer store.Close()
		engine := NewEngine(store)
		engine.SetSchemaDetector(fakeSchemaDetector)
		mp := ManagedPath{Root: root, RootKey: "root", Leaf: "instance.json"}
		if err := engine.WriteFile(context.Background(), "s1", mp, ".proxyctl-txn-t1-1", newContent); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(base, "instance.json"))
		if err != nil || string(data) != string(newContent) {
			t.Fatalf("content = %q, err=%v", data, err)
		}
	})

	t.Run("remove_file succeeds against a schema-type Pre when a detector is installed", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		content := []byte("SCHEMA:instance-v1\ncontent")
		if err := os.WriteFile(filepath.Join(base, "instance.json"), content, 0600); err != nil {
			t.Fatal(err)
		}
		pre := schemaState(0600, "instance-v1", len(content))
		post := PathState{Exists: false}
		j := baseJournal("t1")
		j.Steps = []Step{mkStep("s1", KindRemoveFile, "root/instance.json", StatusPending, pre, post)}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		defer store.Close()
		engine := NewEngine(store)
		engine.SetSchemaDetector(fakeSchemaDetector)
		mp := ManagedPath{Root: root, RootKey: "root", Leaf: "instance.json"}
		if err := engine.RemoveFile(context.Background(), "s1", mp); err != nil {
			t.Fatalf("RemoveFile: %v", err)
		}
		if exists(filepath.Join(base, "instance.json")) {
			t.Fatalf("instance.json should have been removed")
		}
	})

	t.Run("snapshot succeeds against a schema-type Pre when a detector is installed", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		content := []byte("SCHEMA:instance-v1\nsecret-content")
		if err := os.WriteFile(filepath.Join(base, "instance.json"), content, 0600); err != nil {
			t.Fatal(err)
		}
		pre := schemaState(0600, "instance-v1", len(content))
		j := baseJournal("t1")
		j.Steps = []Step{mkStep("s1", KindSnapshot, "root/instance.json", StatusPending, pre, pre)}
		j.Steps[0].Temps = []string{".proxyctl-txn-t1-1"}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		defer store.Close()
		if err := os.Mkdir(filepath.Join(getTxnPath(base), "snap"), 0700); err != nil {
			t.Fatalf("mkdir snap: %v", err)
		}
		snapDir := mustOpenDir(t, filepath.Join(getTxnPath(base), "snap"))
		mp := ManagedPath{Root: root, RootKey: "root", Leaf: "instance.json"}
		engine := NewEngine(store)
		engine.SetSchemaDetector(fakeSchemaDetector)
		if err := engine.Snapshot(context.Background(), "s1", mp, snapDir, ".proxyctl-txn-t1-1"); err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		snapContent, err := os.ReadFile(filepath.Join(snapDir.Name(), ".proxyctl-txn-t1-1.snap"))
		if err != nil || string(snapContent) != string(content) {
			t.Fatalf("snapshot content = %q, err=%v, want %q", snapContent, err, content)
		}
	})

	t.Run("without an installed detector, a schema-type Pre is rejected, not skipped", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		fixtureContent := []byte("SCHEMA:instance-v1\ncontent")
		if err := os.WriteFile(filepath.Join(base, "instance.json"), fixtureContent, 0600); err != nil {
			t.Fatal(err)
		}
		pre := schemaState(0600, "instance-v1", len(fixtureContent))
		post := PathState{Exists: false}
		j := baseJournal("t1")
		j.Steps = []Step{mkStep("s1", KindRemoveFile, "root/instance.json", StatusPending, pre, post)}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		defer store.Close()
		engine := NewEngine(store) // no SetSchemaDetector call
		mp := ManagedPath{Root: root, RootKey: "root", Leaf: "instance.json"}
		err = engine.RemoveFile(context.Background(), "s1", mp)
		if !errors.Is(err, ErrSchemaDetectorRequired) {
			t.Fatalf("RemoveFile error = %v, want ErrSchemaDetectorRequired", err)
		}
		if !exists(filepath.Join(base, "instance.json")) {
			t.Fatalf("instance.json must not have been removed without a schema check")
		}
	})

	t.Run("a schema mismatch is rejected without modifying anything", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		// Live content's schema (per fakeSchemaDetector) is "instance-v2",
		// but the plan's Pre claims "instance-v1" — the file was not the
		// one the plan snapshotted from.
		mismatchContent := []byte("SCHEMA:instance-v2\ncontent")
		if err := os.WriteFile(filepath.Join(base, "instance.json"), mismatchContent, 0600); err != nil {
			t.Fatal(err)
		}
		pre := schemaState(0600, "instance-v1", len(mismatchContent))
		post := PathState{Exists: false}
		j := baseJournal("t1")
		j.Steps = []Step{mkStep("s1", KindRemoveFile, "root/instance.json", StatusPending, pre, post)}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		defer store.Close()
		engine := NewEngine(store)
		engine.SetSchemaDetector(fakeSchemaDetector)
		mp := ManagedPath{Root: root, RootKey: "root", Leaf: "instance.json"}
		err = engine.RemoveFile(context.Background(), "s1", mp)
		if !errors.Is(err, ErrPreStateMismatch) {
			t.Fatalf("RemoveFile error = %v, want ErrPreStateMismatch", err)
		}
		data, rerr := os.ReadFile(filepath.Join(base, "instance.json"))
		if rerr != nil || string(data) != "SCHEMA:instance-v2\ncontent" {
			t.Fatalf("file was modified: data=%q err=%v", data, rerr)
		}
		if status := store.Journal().Steps[0].Status; status != StatusStarted {
			t.Fatalf("step status = %s, want started", status)
		}
	})
}

// TestSystemdAllowedStarted covers review round 2, item 2: §7.2's
// systemd_enable/systemd_disable judgement (UnitFileState ∈ {pre, post})
// and the "not judged" rows (daemon_reload, start, restart, stop).
func TestSystemdAllowedStarted(t *testing.T) {
	pre := PathState{UnitFileState: "disabled"}
	post := PathState{UnitFileState: "enabled"}

	for _, kind := range []StepKind{KindSystemdEnable, KindSystemdDisable} {
		kind := kind
		t.Run(string(kind)+"/live matches pre", func(t *testing.T) {
			chain := []Step{mkStep("s1", kind, "systemd/proxyctl-reality.service", StatusStarted, pre, post)}
			if err := DetermineRunningState(chain, PathState{UnitFileState: "disabled"}, false); err != nil {
				t.Fatalf("DetermineRunningState: %v", err)
			}
		})
		t.Run(string(kind)+"/live matches post", func(t *testing.T) {
			chain := []Step{mkStep("s1", kind, "systemd/proxyctl-reality.service", StatusStarted, pre, post)}
			if err := DetermineRunningState(chain, PathState{UnitFileState: "enabled"}, false); err != nil {
				t.Fatalf("DetermineRunningState: %v", err)
			}
		})
		t.Run(string(kind)+"/live matches neither pre nor post is rejected", func(t *testing.T) {
			chain := []Step{mkStep("s1", kind, "systemd/proxyctl-reality.service", StatusStarted, pre, post)}
			if err := DetermineRunningState(chain, PathState{UnitFileState: "static"}, false); err == nil {
				t.Fatalf("DetermineRunningState accepted a UnitFileState outside {pre, post}")
			}
		})
	}

	for _, kind := range []StepKind{KindSystemdDaemonReload, KindSystemdStart, KindSystemdRestart, KindSystemdStop} {
		kind := kind
		t.Run(string(kind)+"/not judged, any live state accepted", func(t *testing.T) {
			chain := []Step{mkStep("s1", kind, "systemd/proxyctl-reality.service", StatusStarted, pre, post)}
			if err := DetermineRunningState(chain, PathState{UnitFileState: "anything-at-all"}, false); err != nil {
				t.Fatalf("DetermineRunningState: %v (this kind's ActiveState/progress is never a tamper basis)", err)
			}
		})
	}
}
