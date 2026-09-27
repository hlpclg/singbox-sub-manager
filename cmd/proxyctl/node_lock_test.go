package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hlpclg/singbox-sub-manager/internal/backup"
	"github.com/hlpclg/singbox-sub-manager/internal/hostlock"
	"github.com/hlpclg/singbox-sub-manager/internal/realitynode"
)

// withOverriddenDefaultNodesPath points the package-level default-path
// identity check at a temp file for the duration of one test, and restores
// it afterward. Tests in this file are not run with t.Parallel, matching
// the rest of this package's convention.
func withOverriddenDefaultNodesPath(t *testing.T, path string) {
	t.Helper()
	orig := defaultNodesPath
	defaultNodesPath = path
	t.Cleanup(func() { defaultNodesPath = orig })
}

func withOverriddenNodeLockEnv(t *testing.T, env nodeLockEnv) {
	t.Helper()
	orig := newNodeLockEnv
	newNodeLockEnv = func() nodeLockEnv { return env }
	t.Cleanup(func() { newNodeLockEnv = orig })
}

// clearNodeLockEnv is a nodeLockEnv that never rejects and never touches
// the real filesystem: an always-empty in-memory Reality state, and
// publish-guard paths that never exist.
func clearNodeLockEnv(t *testing.T, acquire func(ctx context.Context, paths hostlock.Paths, opts hostlock.AcquireOptions, levels ...hostlock.Level) (*hostlock.Lease, error)) nodeLockEnv {
	t.Helper()
	return nodeLockEnv{
		lockPaths: hostlock.Paths{L1: filepath.Join(t.TempDir(), "l1.lock")},
		acquire:   acquire,
		realityRoots: realitynode.Roots{
			SystemdUnit: "/nonexistent/unit", ConfigDir: "/nonexistent/cfgdir", StateDir: "/nonexistent/state",
			BinDir: "/nonexistent/bin", TxnDir: "/nonexistent/txn", TombstoneGlob: "/nonexistent/tomb-*",
		},
		realityFS: realitynode.StateFS{
			ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
			Stat:     func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
			Glob:     func(string) ([]string, error) { return nil, nil },
		},
		publishTxnDir:   filepath.Join(t.TempDir(), "publish-does-not-exist"),
		publishTombGlob: filepath.Join(t.TempDir(), "tomb-does-not-exist-*"),
	}
}

func alwaysAcquire(counted *int) func(context.Context, hostlock.Paths, hostlock.AcquireOptions, ...hostlock.Level) (*hostlock.Lease, error) {
	return func(ctx context.Context, paths hostlock.Paths, opts hostlock.AcquireOptions, levels ...hostlock.Level) (*hostlock.Lease, error) {
		*counted++
		return hostlock.Acquire(ctx, paths, hostlock.AcquireOptions{
			NewLocker: func(string) backup.Locker { return noopLocker{} },
		}, levels...)
	}
}

// noopLocker always succeeds; used where a test wants Acquire's bookkeeping
// (ordering, release tracking) without any real filesystem lock.
type noopLocker struct{}

func (noopLocker) TryLock() error { return nil }
func (noopLocker) Unlock() error  { return nil }

func TestIsDefaultNodesPath(t *testing.T) {
	dir := t.TempDir()
	def := filepath.Join(dir, "nodes.conf")
	withOverriddenDefaultNodesPath(t, def)

	t.Run("identical string is default", func(t *testing.T) {
		got, err := isDefaultNodesPath(def)
		if err != nil || !got {
			t.Fatalf("got %v, %v; want true, nil", got, err)
		}
	})

	t.Run("different path, neither exists, is not default", func(t *testing.T) {
		got, err := isDefaultNodesPath(filepath.Join(dir, "other.conf"))
		if err != nil || got {
			t.Fatalf("got %v, %v; want false, nil", got, err)
		}
	})

	t.Run("both exist and are the same file via a different path spelling", func(t *testing.T) {
		if err := os.WriteFile(def, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(dir, ".", "nodes.conf")
		got, err := isDefaultNodesPath(alias)
		if err != nil || !got {
			t.Fatalf("got %v, %v; want true, nil", got, err)
		}
	})

	t.Run("both exist but are different files", func(t *testing.T) {
		other := filepath.Join(dir, "other.conf")
		if err := os.WriteFile(other, []byte("y"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := isDefaultNodesPath(other)
		if err != nil || got {
			t.Fatalf("got %v, %v; want false, nil", got, err)
		}
	})

	t.Run("symlink to the default file counts as the default via SameFile", func(t *testing.T) {
		link := filepath.Join(dir, "link.conf")
		if err := os.Symlink(def, link); err != nil {
			t.Skipf("symlink not supported here: %v", err)
		}
		got, err := isDefaultNodesPath(link)
		if err != nil || !got {
			t.Fatalf("got %v, %v; want true, nil", got, err)
		}
	})
}

func TestWithDefaultNodesLock_NonDefaultPathBypassesLockAndGuard(t *testing.T) {
	dir := t.TempDir()
	withOverriddenDefaultNodesPath(t, filepath.Join(dir, "nodes.conf"))

	acquireCalls := 0
	env := nodeLockEnv{
		acquire: func(ctx context.Context, paths hostlock.Paths, opts hostlock.AcquireOptions, levels ...hostlock.Level) (*hostlock.Lease, error) {
			acquireCalls++
			t.Fatalf("acquire must not be called for a non-default --nodes path")
			return nil, nil
		},
	}
	withOverriddenNodeLockEnv(t, env)

	fnCalled := false
	code := withDefaultNodesLock(filepath.Join(dir, "custom.conf"), &bytes.Buffer{}, func() int {
		fnCalled = true
		return 0
	})
	if code != 0 || !fnCalled {
		t.Fatalf("code=%d fnCalled=%v; want 0, true", code, fnCalled)
	}
	if acquireCalls != 0 {
		t.Fatalf("acquire called %d times for a non-default path, want 0", acquireCalls)
	}
}

func TestWithDefaultNodesLock_DefaultPathAcquiresAndReleases(t *testing.T) {
	dir := t.TempDir()
	def := filepath.Join(dir, "nodes.conf")
	withOverriddenDefaultNodesPath(t, def)

	var acquireCalls int
	env := clearNodeLockEnv(t, alwaysAcquire(&acquireCalls))
	withOverriddenNodeLockEnv(t, env)

	fnCalled := false
	code := withDefaultNodesLock(def, &bytes.Buffer{}, func() int {
		fnCalled = true
		return 0
	})
	if code != 0 || !fnCalled {
		t.Fatalf("code=%d fnCalled=%v; want 0, true", code, fnCalled)
	}
	if acquireCalls != 1 {
		t.Fatalf("acquire called %d times, want 1", acquireCalls)
	}
}

func TestWithDefaultNodesLock_GuardRejectsWithoutCallingFn(t *testing.T) {
	dir := t.TempDir()
	def := filepath.Join(dir, "nodes.conf")
	withOverriddenDefaultNodesPath(t, def)

	var acquireCalls int
	env := clearNodeLockEnv(t, alwaysAcquire(&acquireCalls))
	// Give DetectInstanceState one present asset (all four roots are
	// otherwise absent by default in clearNodeLockEnv, which reads as
	// "not installed" and would not trigger the guard): a state dir that
	// exists but nothing else is enough to land on "inconsistent", which
	// like every non-"not installed" status must reject.
	env.realityRoots.StateDir = "/present-state-dir"
	env.realityFS.Stat = func(p string) (os.FileInfo, error) {
		if p == "/present-state-dir" {
			return realDirInfo{}, nil
		}
		return nil, os.ErrNotExist
	}
	withOverriddenNodeLockEnv(t, env)

	var stderr bytes.Buffer
	fnCalled := false
	code := withDefaultNodesLock(def, &stderr, func() int {
		fnCalled = true
		return 0
	})
	if fnCalled {
		t.Fatalf("fn must not run when the legacy guard rejects")
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "Reality node instance") {
		t.Fatalf("stderr = %q, want a Reality-instance guard message", stderr.String())
	}
	if acquireCalls != 1 {
		t.Fatalf("acquire called %d times, want 1 (lock must still be taken before the recheck)", acquireCalls)
	}
}

func TestWithDefaultNodesLock_PublishTxnGuardRejects(t *testing.T) {
	dir := t.TempDir()
	def := filepath.Join(dir, "nodes.conf")
	withOverriddenDefaultNodesPath(t, def)

	var acquireCalls int
	env := clearNodeLockEnv(t, alwaysAcquire(&acquireCalls))
	txnDir := filepath.Join(dir, "publish-txn")
	if err := os.Mkdir(txnDir, 0o700); err != nil {
		t.Fatal(err)
	}
	env.publishTxnDir = txnDir
	withOverriddenNodeLockEnv(t, env)

	var stderr bytes.Buffer
	fnCalled := false
	code := withDefaultNodesLock(def, &stderr, func() int {
		fnCalled = true
		return 0
	})
	if fnCalled {
		t.Fatalf("fn must not run when a leftover publish transaction exists")
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "publish --recover") {
		t.Fatalf("stderr = %q, want a publish --recover hint", stderr.String())
	}
}

func TestWithDefaultNodesLock_AcquireFailureRejectsWithoutCallingFn(t *testing.T) {
	dir := t.TempDir()
	def := filepath.Join(dir, "nodes.conf")
	withOverriddenDefaultNodesPath(t, def)

	env := clearNodeLockEnv(t, nil)
	env.acquire = func(ctx context.Context, paths hostlock.Paths, opts hostlock.AcquireOptions, levels ...hostlock.Level) (*hostlock.Lease, error) {
		return nil, errors.New("simulated: lock busy")
	}
	withOverriddenNodeLockEnv(t, env)

	var stderr bytes.Buffer
	fnCalled := false
	code := withDefaultNodesLock(def, &stderr, func() int {
		fnCalled = true
		return 0
	})
	if fnCalled {
		t.Fatalf("fn must not run when the lock cannot be acquired")
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "another operation") {
		t.Fatalf("stderr = %q, want an 'another operation' message", stderr.String())
	}
}

// realDirInfo is a minimal os.FileInfo standing in for "a directory exists
// here", used only to make DetectInstanceState see a present asset.
type realDirInfo struct{}

func (realDirInfo) Name() string       { return "state" }
func (realDirInfo) Size() int64        { return 0 }
func (realDirInfo) Mode() os.FileMode  { return os.ModeDir | 0o700 }
func (realDirInfo) ModTime() time.Time { return time.Time{} }
func (realDirInfo) IsDir() bool        { return true }
func (realDirInfo) Sys() interface{}   { return nil }

// ---------------------------------------------------------------------------
// Real cross-process/goroutine flock behavior at the cmd/proxyctl layer:
// L1 blocks a default-path CRUD write; L2 does not.
// ---------------------------------------------------------------------------

func realNodeLockEnvForTest(t *testing.T, lockDir string, fastWait time.Duration) nodeLockEnv {
	t.Helper()
	env := clearNodeLockEnv(t, nil)
	env.lockPaths = hostlock.Paths{
		L1: filepath.Join(lockDir, "l1.lock"),
		L2: filepath.Join(lockDir, "l2.lock"),
		L3: filepath.Join(lockDir, "l3.lock"),
	}
	env.acquire = func(ctx context.Context, paths hostlock.Paths, opts hostlock.AcquireOptions, levels ...hostlock.Level) (*hostlock.Lease, error) {
		return hostlock.Acquire(ctx, paths, hostlock.AcquireOptions{
			MaxWait: func(hostlock.Level) time.Duration { return fastWait },
		}, levels...)
	}
	return env
}

func TestDefaultNodesWrite_RealL1Blocks(t *testing.T) {
	dir := t.TempDir()
	def := filepath.Join(dir, "nodes.conf")
	withOverriddenDefaultNodesPath(t, def)
	withOverriddenNodeLockEnv(t, realNodeLockEnvForTest(t, dir, 150*time.Millisecond))

	external := openRawFlock(t, filepath.Join(dir, "l1.lock"))
	defer external.release()

	var stderr bytes.Buffer
	fnCalled := false
	code := withDefaultNodesLock(def, &stderr, func() int { fnCalled = true; return 0 })
	if fnCalled {
		t.Fatalf("fn must not run while L1 is held by another process/holder")
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1 while L1 is externally held", code)
	}

	external.release()
	fnCalled = false
	code = withDefaultNodesLock(def, &bytes.Buffer{}, func() int { fnCalled = true; return 0 })
	if code != 0 || !fnCalled {
		t.Fatalf("after releasing L1: code=%d fnCalled=%v, want 0 true", code, fnCalled)
	}
}

func TestDefaultNodesWrite_RealL2DoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	def := filepath.Join(dir, "nodes.conf")
	withOverriddenDefaultNodesPath(t, def)
	withOverriddenNodeLockEnv(t, realNodeLockEnvForTest(t, dir, 150*time.Millisecond))

	external := openRawFlock(t, filepath.Join(dir, "l2.lock"))
	defer external.release()

	fnCalled := false
	code := withDefaultNodesLock(def, &bytes.Buffer{}, func() int { fnCalled = true; return 0 })
	if code != 0 || !fnCalled {
		t.Fatalf("default-path CRUD only takes L1; holding L2 externally must not block it. code=%d fnCalled=%v", code, fnCalled)
	}
}

// ---------------------------------------------------------------------------
// rawFlock: an independent, real flock on a path, bypassing hostlock
// entirely, used to prove genuine cross-holder mutual exclusion rather than
// this package's own bookkeeping.
// ---------------------------------------------------------------------------

type rawFlock struct {
	t        *testing.T
	f        *os.File
	released bool
}

func openRawFlock(t *testing.T, path string) *rawFlock {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		t.Fatalf("flock %s: %v", path, err)
	}
	return &rawFlock{t: t, f: f}
}

func (r *rawFlock) release() {
	if r.released {
		return
	}
	r.released = true
	if err := syscall.Flock(int(r.f.Fd()), syscall.LOCK_UN); err != nil {
		r.t.Errorf("unlock %s: %v", r.f.Name(), err)
	}
	r.f.Close()
}
