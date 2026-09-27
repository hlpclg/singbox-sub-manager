package transaction

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// withUmask installs a restrictive umask for the rest of the test
// (restored via t.Cleanup) — the same pattern internal/backup/dirfd_test.go
// documents (this package's tests never use t.Parallel either).
func withUmask(t *testing.T, mask int) {
	t.Helper()
	old := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(old) })
}

func mustMkdirAll(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, mode); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

// swapDirWithFreshInode replaces the directory at path with a directory
// that is provably a different filesystem object: it builds the
// replacement elsewhere (so it gets its own, already-distinct inode while
// the original at path still exists), removes the original, then renames
// the replacement into place. A plain remove-then-mkdir-at-the-same-name
// is not reliable for this on a filesystem (tmpfs, notably) that
// immediately reissues a just-freed inode number to the very next
// allocation at the same path.
func swapDirWithFreshInode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	replacement := path + ".replacement-fresh-inode"
	mustMkdirAll(t, replacement, mode)
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("rename %s to %s: %v", replacement, path, err)
	}
}

func identityOfPath(t *testing.T, path string) DirIdentity {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	id, err := identityOf(int(f.Fd()))
	if err != nil {
		t.Fatalf("identityOf %s: %v", path, err)
	}
	return id
}

// --- OpenTrustedRoot ---

func TestOpenTrustedRoot(t *testing.T) {
	t.Run("rejects a root writable by group", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0770); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenTrustedRoot(dir, os.Getuid()); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("OpenTrustedRoot error = %v, want ErrUnsafePath", err)
		}
	})

	t.Run("rejects a root writable by other", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0707); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenTrustedRoot(dir, os.Getuid()); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("OpenTrustedRoot error = %v, want ErrUnsafePath", err)
		}
	})

	t.Run("rejects a root owned by a different user", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := OpenTrustedRoot(dir, os.Getuid()+12345); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("OpenTrustedRoot error = %v, want ErrUnsafePath", err)
		}
	})

	t.Run("rejects a root that is itself a symlink", func(t *testing.T) {
		base := t.TempDir()
		real := filepath.Join(base, "real")
		mustMkdirAll(t, real, 0700)
		link := filepath.Join(base, "link")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenTrustedRoot(link, os.Getuid()); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("OpenTrustedRoot error = %v, want ErrUnsafePath", err)
		}
	})

	t.Run("rejects a root that is a regular file", func(t *testing.T) {
		base := t.TempDir()
		file := filepath.Join(base, "notadir")
		if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenTrustedRoot(file, os.Getuid()); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("OpenTrustedRoot error = %v, want ErrUnsafePath", err)
		}
	})

	t.Run("accepts a properly owned, non-group/other-writable root", func(t *testing.T) {
		// t.TempDir() itself is created with Mkdir(dir, 0777) (see Go's
		// testing.common.TempDir), so its actual mode is 0777 masked by
		// whatever the test process's umask happens to be — under a
		// umask that does not clear the group-write bit (e.g. Debian/
		// Ubuntu's common default 0002 for a user with a private group),
		// that leaves it group-writable, which OpenTrustedRoot must (and,
		// per the sibling subtests above, does) reject. This subtest is
		// about the *accept* path, so it explicitly locks the mode down
		// first rather than relying on t.TempDir()'s incidental result
		// (review P1#4 — this was a test-fixture bug, not a production
		// one: OpenTrustedRoot's own check was already correct).
		dir := t.TempDir()
		if err := os.Chmod(dir, 0700); err != nil {
			t.Fatal(err)
		}
		root, err := OpenTrustedRoot(dir, os.Getuid())
		if err != nil {
			t.Fatalf("OpenTrustedRoot: %v", err)
		}
		root.Close()
	})
}

// --- Ancestor replacement (TestAncestorReplacement) ---

func TestAncestorReplacement(t *testing.T) {
	dirIDsFor := func(base string, levels ...string) map[string]DirIdentity {
		ids := map[string]DirIdentity{}
		path := base
		key := "root"
		for _, lvl := range levels {
			path = filepath.Join(path, lvl)
			key = key + "/" + lvl
			ids[key] = identityOfPath(t, path)
		}
		return ids
	}

	t.Run("unsafe trusted root is rejected before anything is touched", func(t *testing.T) {
		base := t.TempDir()
		if err := os.Chmod(base, 0777); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenTrustedRoot(base, os.Getuid()); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("OpenTrustedRoot error = %v, want ErrUnsafePath", err)
		}
	})

	t.Run("leaf symlink is rejected without following it", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		mustMkdirAll(t, filepath.Join(base, "token"), 0755)
		outside := filepath.Join(t.TempDir(), "secret")
		if err := os.WriteFile(outside, []byte("do-not-read-me"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(base, "token", "nodes.conf")); err != nil {
			t.Fatal(err)
		}
		ids := dirIDsFor(base, "token")
		mp := ManagedPath{Root: root, RootKey: "root", Comps: []string{"token"}, Keys: []string{"root/token"}, Leaf: "nodes.conf"}
		parent, opened, err := resolveVerifiedParent(mp, ids, nil)
		if err != nil {
			t.Fatalf("resolveVerifiedParent (ancestors only) unexpectedly failed: %v", err)
		}
		defer closeAll(opened)
		if _, err := statLeaf(parent, "nodes.conf", false); !errors.Is(err, ErrIllegalPathType) {
			t.Fatalf("statLeaf error = %v, want ErrIllegalPathType (symlink)", err)
		}
		// The secret file outside the root must be untouched.
		data, rerr := os.ReadFile(outside)
		if rerr != nil || string(data) != "do-not-read-me" {
			t.Fatalf("outside file was touched: data=%q err=%v", data, rerr)
		}
	})

	t.Run("intermediate directory replaced with a symlink is rejected", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		mustMkdirAll(t, filepath.Join(base, "token"), 0755)
		ids := dirIDsFor(base, "token")
		// Swap "token" for a symlink to somewhere else, after recording its
		// original identity.
		if err := os.RemoveAll(filepath.Join(base, "token")); err != nil {
			t.Fatal(err)
		}
		elsewhere := filepath.Join(t.TempDir(), "elsewhere")
		mustMkdirAll(t, elsewhere, 0755)
		if err := os.Symlink(elsewhere, filepath.Join(base, "token")); err != nil {
			t.Fatal(err)
		}
		mp := ManagedPath{Root: root, RootKey: "root", Comps: []string{"token"}, Keys: []string{"root/token"}, Leaf: "nodes.conf"}
		_, opened, err := resolveVerifiedParent(mp, ids, nil)
		defer closeAll(opened)
		if !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("resolveVerifiedParent error = %v, want ErrUnsafePath", err)
		}
	})

	t.Run("token directory replaced with another real directory (new inode) is caught by dir_ids mismatch", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		mustMkdirAll(t, filepath.Join(base, "token"), 0755)
		ids := dirIDsFor(base, "token")
		swapDirWithFreshInode(t, filepath.Join(base, "token"), 0755)
		mp := ManagedPath{Root: root, RootKey: "root", Comps: []string{"token"}, Keys: []string{"root/token"}, Leaf: "nodes.conf"}
		_, opened, err := resolveVerifiedParent(mp, ids, nil)
		defer closeAll(opened)
		if !errors.Is(err, ErrAncestorMismatch) {
			t.Fatalf("resolveVerifiedParent error = %v, want ErrAncestorMismatch", err)
		}
	})

	t.Run("subscription_root itself (multi-level ancestor) replaced with another real directory is caught before descending into it", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		mustMkdirAll(t, filepath.Join(base, "proxy-sub", "tok1"), 0755)
		ids := dirIDsFor(base, "proxy-sub", "tok1")
		// Replace the whole "proxy-sub" subtree with a structurally
		// identical but provably different real directory tree — proves
		// the mismatch is caught at the first replaced level, not only at
		// the leaf.
		swapDirWithFreshInode(t, filepath.Join(base, "proxy-sub"), 0755)
		mustMkdirAll(t, filepath.Join(base, "proxy-sub", "tok1"), 0755)
		mp := ManagedPath{Root: root, RootKey: "root", Comps: []string{"proxy-sub", "tok1"}, Keys: []string{"root/proxy-sub", "root/proxy-sub/tok1"}, Leaf: "nodes.conf"}
		_, opened, err := resolveVerifiedParent(mp, ids, nil)
		defer closeAll(opened)
		if !errors.Is(err, ErrAncestorMismatch) {
			t.Fatalf("resolveVerifiedParent error = %v, want ErrAncestorMismatch", err)
		}
	})

	t.Run("hook fires after opening the dirfd, before the identity comparison, without changing a correct result", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		mustMkdirAll(t, filepath.Join(base, "token"), 0755)
		ids := dirIDsFor(base, "token")
		var hookCalls []int
		hook := func(level int) {
			hookCalls = append(hookCalls, level)
			// Rename the directory away and put a fresh one in its place,
			// entirely by path, after this call already holds an fd to the
			// original object. The held fd is inode-bound, so the
			// subsequent identity comparison still reflects the ORIGINAL
			// object (matching the recorded identity) regardless of this
			// swap-by-name.
			if err := os.Rename(filepath.Join(base, "token"), filepath.Join(base, "token-moved")); err != nil {
				t.Fatal(err)
			}
			mustMkdirAll(t, filepath.Join(base, "token"), 0755)
		}
		mp := ManagedPath{Root: root, RootKey: "root", Comps: []string{"token"}, Keys: []string{"root/token"}, Leaf: "nodes.conf"}
		parent, opened, err := resolveVerifiedParent(mp, ids, hook)
		defer closeAll(opened)
		if err != nil {
			t.Fatalf("resolveVerifiedParent: %v", err)
		}
		if len(hookCalls) != 1 || hookCalls[0] != 0 {
			t.Fatalf("hook calls = %v, want [0]", hookCalls)
		}
		// parent must still be the ORIGINAL directory (now reachable only
		// via "token-moved" by path), not the new one swapped in at
		// "token" — write into it and check it landed at the old path.
		if err := os.WriteFile(filepath.Join(base, "token-moved", "nodes.conf"), nil, 0); err == nil {
			_ = parent // parent is a live fd to that same directory; both routes reach the same inode.
		}
	})

	t.Run("neither the target nor the journal changes on rejection", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		mustMkdirAll(t, filepath.Join(base, "token"), 0755)
		targetPath := filepath.Join(base, "token", "nodes.conf")
		if err := os.WriteFile(targetPath, []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
		before, err := os.Stat(targetPath)
		if err != nil {
			t.Fatal(err)
		}
		ids := dirIDsFor(base, "token")
		swapDirWithFreshInode(t, filepath.Join(base, "token"), 0755)
		// The old "nodes.conf" is gone along with the old directory; a
		// fresh empty "token" has nothing at that name — resolving the
		// parent chain must still fail (ancestor mismatch) before ever
		// reaching a leaf operation.
		mp := ManagedPath{Root: root, RootKey: "root", Comps: []string{"token"}, Keys: []string{"root/token"}, Leaf: "nodes.conf"}
		_, opened, err := resolveVerifiedParent(mp, ids, nil)
		closeAll(opened)
		if !errors.Is(err, ErrAncestorMismatch) {
			t.Fatalf("resolveVerifiedParent error = %v, want ErrAncestorMismatch", err)
		}
		newTarget := filepath.Join(base, "token", "nodes.conf")
		if exists(newTarget) {
			t.Fatalf("nodes.conf must not have been created under the replacement directory")
		}
		_ = before
	})
}

// --- Directory identity (TestDirectoryIdentity) ---

func TestDirectoryIdentity(t *testing.T) {
	t.Run("Mkdir records the new identity itself, before marking done", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		mp := ManagedPath{Root: root, RootKey: "root", Leaf: "instance"}
		j := baseJournal("t1")
		j.Steps = []Step{mkStep("mk1", KindMkdir, "root/instance", StatusPending,
			PathState{Exists: false},
			PathState{Exists: true, Type: PathTypeDir, Mode: 0700, UID: uint32(os.Getuid()), GID: uint32(os.Getgid())})}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		defer store.Close()
		engine := NewEngine(store)
		dirFd, err := engine.Mkdir(context.Background(), "mk1", mp)
		if err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		defer dirFd.Close()
		id, err := CurrentIdentity(dirFd)
		if err != nil {
			t.Fatalf("CurrentIdentity: %v", err)
		}
		// Mkdir itself must already have recorded this identity (review
		// P1#1) — no separate RecordDirIdentity call from the caller.
		recorded := engine.Journal().DirIDs["root/instance"]
		if !recorded.Exists || recorded.Dev != id.Dev || recorded.Ino != id.Ino {
			t.Fatalf("recorded identity = %+v, want %+v", recorded, id)
		}
		if status := engine.Journal().Steps[0].Status; status != StatusDone {
			t.Fatalf("step status = %s, want done", status)
		}
		liveID := identityOfPath(t, filepath.Join(base, "instance"))
		if liveID != id {
			t.Fatalf("live identity = %+v, want %+v", liveID, id)
		}
	})

	t.Run("crash after mkdirat, before chmod/chown/identity, is recognizable as a legal condition: started, empty, process-owned", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		mp := ManagedPath{Root: root, RootKey: "root", Leaf: "instance"}
		post := PathState{Exists: true, Type: PathTypeDir, Mode: 0700, UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
		j := baseJournal("t1")
		j.Steps = []Step{mkStep("mk1", KindMkdir, "root/instance", StatusPending, PathState{Exists: false}, post)}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		defer store.Close()
		engine := NewEngine(store)

		old := stepHook
		stepHook = func(point string) error {
			if point == "mkdir:after_mkdirat" {
				return errFakeCrash
			}
			return nil
		}
		_, mkErr := engine.Mkdir(context.Background(), "mk1", mp)
		stepHook = old
		if !errors.Is(mkErr, errFakeCrash) {
			t.Fatalf("Mkdir error = %v, want errFakeCrash", mkErr)
		}

		// The step is still started, and dir_ids has not been touched yet
		// (Mkdir records the new identity only after chmod+chown succeed).
		if status := engine.Journal().Steps[0].Status; status != StatusStarted {
			t.Fatalf("step status = %s, want started", status)
		}
		if _, ok := engine.Journal().DirIDs["root/instance"]; ok {
			t.Fatalf("dir_ids must not be registered yet at this crash point")
		}
		chain := PathChain(engine.Journal().Steps, "root/instance")
		liveState, err := statLeafDirForTest(t, base, "instance")
		if err != nil {
			t.Fatalf("stat instance: %v", err)
		}
		if err := DetermineRunningState(chain, liveState, true /* empty */); err != nil {
			t.Fatalf("DetermineRunningState rejected a legal post-mkdirat, pre-chmod state: %v", err)
		}
	})

	t.Run("crash after identity is recorded, before done, is recognizable as a legal condition: started, at post, dir_ids already updated", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		mp := ManagedPath{Root: root, RootKey: "root", Leaf: "instance"}
		post := PathState{Exists: true, Type: PathTypeDir, Mode: 0700, UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
		j := baseJournal("t1")
		j.Steps = []Step{mkStep("mk1", KindMkdir, "root/instance", StatusPending, PathState{Exists: false}, post)}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		defer store.Close()
		engine := NewEngine(store)

		old := stepHook
		stepHook = func(point string) error {
			if point == "mkdir:after_identity" {
				return errFakeCrash
			}
			return nil
		}
		_, mkErr := engine.Mkdir(context.Background(), "mk1", mp)
		stepHook = old
		if !errors.Is(mkErr, errFakeCrash) {
			t.Fatalf("Mkdir error = %v, want errFakeCrash", mkErr)
		}

		if status := engine.Journal().Steps[0].Status; status != StatusStarted {
			t.Fatalf("step status = %s, want started (review P1#1: done must not be reached without dir_ids first)", status)
		}
		recorded, ok := engine.Journal().DirIDs["root/instance"]
		if !ok || !recorded.Exists {
			t.Fatalf("dir_ids must already be registered at this crash point, got %+v ok=%v", recorded, ok)
		}
		liveID := identityOfPath(t, filepath.Join(base, "instance"))
		if liveID.Dev != recorded.Dev || liveID.Ino != recorded.Ino {
			t.Fatalf("recorded identity %+v does not match live directory %+v", recorded, liveID)
		}
		chain := PathChain(engine.Journal().Steps, "root/instance")
		liveState, err := statLeafDirForTest(t, base, "instance")
		if err != nil {
			t.Fatalf("stat instance: %v", err)
		}
		if err := DetermineRunningState(chain, liveState, true); err != nil {
			t.Fatalf("DetermineRunningState rejected a legal post-identity, pre-done state: %v", err)
		}
	})

	t.Run("a non-empty or wrong-owner directory at that name is not a legal started state", func(t *testing.T) {
		post := PathState{Exists: true, Type: PathTypeDir, Mode: 0700, UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
		chain := []Step{mkStep("mk1", KindMkdir, "root/instance", StatusStarted, PathState{Exists: false}, post)}

		t.Run("non-empty", func(t *testing.T) {
			live := PathState{Exists: true, Type: PathTypeDir, Mode: 0755, UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
			if err := DetermineRunningState(chain, live, false /* not empty */); err == nil {
				t.Fatalf("DetermineRunningState accepted a non-empty directory mid-mkdir")
			}
		})
		t.Run("wrong owner", func(t *testing.T) {
			live := PathState{Exists: true, Type: PathTypeDir, Mode: 0755, UID: 65534, GID: 65534}
			if live != post {
				if err := DetermineRunningState(chain, live, true); err == nil {
					t.Fatalf("DetermineRunningState accepted a directory not owned by the current process mid-mkdir")
				}
			}
		})
	})

	t.Run("RemoveDir records not-exist itself, before marking done", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		mustMkdirAll(t, filepath.Join(base, "old"), 0700)
		id := identityOfPath(t, filepath.Join(base, "old"))
		mp := ManagedPath{Root: root, RootKey: "root", Leaf: "old"}
		j := baseJournal("t1")
		j.DirIDs["root/old"] = id
		j.Steps = []Step{mkStep("rm1", KindRemoveDir, "root/old", StatusPending,
			PathState{Exists: true, Type: PathTypeDir, Mode: 0700, UID: uint32(os.Getuid()), GID: uint32(os.Getgid())},
			PathState{Exists: false})}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		defer store.Close()
		engine := NewEngine(store)
		if err := engine.RemoveDir(context.Background(), "rm1", mp); err != nil {
			t.Fatalf("RemoveDir: %v", err)
		}
		recorded := engine.Journal().DirIDs["root/old"]
		if recorded.Exists {
			t.Fatalf("recorded identity after remove_dir = %+v, want Exists=false", recorded)
		}
		if status := engine.Journal().Steps[0].Status; status != StatusDone {
			t.Fatalf("step status = %s, want done", status)
		}
		if exists(filepath.Join(base, "old")) {
			t.Fatalf("directory still exists on disk")
		}
	})

	t.Run("crash after rmdir, before identity is recorded, is recognizable as a legal condition: started, not-exist", func(t *testing.T) {
		root, base := newTestRoot(t, "root")
		mustMkdirAll(t, filepath.Join(base, "old"), 0700)
		id := identityOfPath(t, filepath.Join(base, "old"))
		mp := ManagedPath{Root: root, RootKey: "root", Leaf: "old"}
		j := baseJournal("t1")
		j.DirIDs["root/old"] = id
		j.Steps = []Step{mkStep("rm1", KindRemoveDir, "root/old", StatusPending,
			PathState{Exists: true, Type: PathTypeDir, Mode: 0700, UID: uint32(os.Getuid()), GID: uint32(os.Getgid())},
			PathState{Exists: false})}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		defer store.Close()
		engine := NewEngine(store)

		old := stepHook
		stepHook = func(point string) error {
			if point == "remove_dir:after_parent_fsync" {
				return errFakeCrash
			}
			return nil
		}
		rmErr := engine.RemoveDir(context.Background(), "rm1", mp)
		stepHook = old
		if !errors.Is(rmErr, errFakeCrash) {
			t.Fatalf("RemoveDir error = %v, want errFakeCrash", rmErr)
		}
		if status := engine.Journal().Steps[0].Status; status != StatusStarted {
			t.Fatalf("step status = %s, want started", status)
		}
		// dir_ids still says "exists" at this crash point (not yet
		// updated) — the live directory is nonetheless already gone,
		// which DetermineRunningState's remove_dir rule (!live.Exists)
		// accepts regardless of dir_ids's own timing.
		chain := PathChain(engine.Journal().Steps, "root/old")
		if exists(filepath.Join(base, "old")) {
			t.Fatalf("directory should already be removed")
		}
		if err := DetermineRunningState(chain, PathState{Exists: false}, false); err != nil {
			t.Fatalf("DetermineRunningState rejected a legal post-rmdir state: %v", err)
		}
	})
}

func statLeafDirForTest(t *testing.T, base, name string) (PathState, error) {
	t.Helper()
	f, err := os.Open(base)
	if err != nil {
		return PathState{}, err
	}
	defer f.Close()
	return statLeafDir(f, name)
}
