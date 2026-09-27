package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hlpclg/singbox-sub-manager/internal/backup"
	"github.com/hlpclg/singbox-sub-manager/internal/realitynode"
)

// TestBackupCreate_AcquiresAndReleasesL2 proves spec §11.1/§11.2's new
// requirement that `backup` (create) takes L2 for the duration of the
// archive creation, exactly once, and always releases it.
func TestBackupCreate_AcquiresAndReleasesL2(t *testing.T) {
	h := newHarness(t)
	h.install(t)

	code, _, stderr := runCmd(t, "backup")
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if h.lock.tryCalls != 1 || h.lock.unlockCalls != 1 {
		t.Fatalf("L2 lock taken %d times, released %d times; want 1, 1", h.lock.tryCalls, h.lock.unlockCalls)
	}
	if h.lock.held {
		t.Fatalf("L2 lock still held after cmdBackup returned")
	}
}

// TestBackupCreate_LockBusyRefusesWithoutCreating proves that when L2 is
// busy, the archive is never created and the error is the same actionable
// message restore already gives.
func TestBackupCreate_LockBusyRefusesWithoutCreating(t *testing.T) {
	h := newHarness(t)
	h.lock.tryErr = errors.New("held")
	createCalled := false
	h.env.create = func(ctx context.Context, opts backup.CreateOptions) (backup.Manifest, error) {
		createCalled = true
		return backup.Manifest{}, nil
	}
	h.install(t)

	code, _, stderr := runCmd(t, "backup")
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if createCalled {
		t.Fatalf("create must not run while L2 is busy")
	}
	if !strings.Contains(stderr, "monitor is holding the recovery lock") {
		t.Fatalf("stderr = %q, want the monitor-holding-lock message", stderr)
	}
	if h.lock.unlockCalls != 0 {
		t.Fatalf("a never-acquired lock must not be unlocked, got %d unlock calls", h.lock.unlockCalls)
	}
}

// ---------------------------------------------------------------------------
// Restore: L2 -> L3 ordering, reverse release, and the reverse guard.
// ---------------------------------------------------------------------------

// TestRestore_L2ThenL3OrderAndReverseRelease proves restore acquires L2
// before L3 and releases L3 before L2 (spec §11.1/§11.5), using dedicated
// order-recording fakes rather than the shared harness countingLocker
// (which does not track cross-lock ordering).
func TestRestore_L2ThenL3OrderAndReverseRelease(t *testing.T) {
	h := newHarness(t)
	var order []string
	h.env.newLock = func() backup.Locker { return &orderedLocker{name: "L2", order: &order} }
	h.env.newL3Lock = func() backup.Locker { return &orderedLocker{name: "L3", order: &order} }
	h.install(t)

	code, _, stderr := runCmd(t, "restore", "/tmp/archive.tar.gz")
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}

	want := []string{"lock:L2", "lock:L3", "unlock:L3", "unlock:L2"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

type orderedLocker struct {
	name  string
	order *[]string
}

func (l *orderedLocker) TryLock() error {
	*l.order = append(*l.order, "lock:"+l.name)
	return nil
}
func (l *orderedLocker) Unlock() error {
	*l.order = append(*l.order, "unlock:"+l.name)
	return nil
}

// TestRestore_L3BusyReleasesL2 proves that when L3 cannot be acquired, L2
// (already held) is released before returning — no lock leak.
func TestRestore_L3BusyReleasesL2(t *testing.T) {
	h := newHarness(t)
	h.l3lock.tryErr = errors.New("held")
	h.install(t)

	code, _, stderr := runCmd(t, "restore", "/tmp/archive.tar.gz")
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d, stderr = %s", code, exitFailure, stderr)
	}
	if h.lock.tryCalls != 1 || h.lock.unlockCalls != 1 {
		t.Fatalf("L2 taken %d times, released %d times; want 1, 1 (must not leak when L3 fails)", h.lock.tryCalls, h.lock.unlockCalls)
	}
	if len(h.restoreOpts) != 0 {
		t.Fatalf("restore must not run when L3 cannot be acquired")
	}
}

// TestRestore_LegacyGuardRejectsWhenRealityInstancePresent proves restore
// refuses (spec §11.5) when this host shows any trace of a Reality node
// instance, without ever calling the restore function.
func TestRestore_LegacyGuardRejectsWhenRealityInstancePresent(t *testing.T) {
	h := newHarness(t)
	presentDir := filepath.Join(t.TempDir(), "present-state-dir")
	h.env.realityRoots.StateDir = presentDir
	h.env.realityFS = realitynode.StateFS{
		ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Stat: func(p string) (os.FileInfo, error) {
			if p == presentDir {
				return fakeDirInfo{}, nil
			}
			return nil, os.ErrNotExist
		},
		Glob: func(string) ([]string, error) { return nil, nil },
	}
	h.install(t)

	code, _, stderr := runCmd(t, "restore", "/tmp/archive.tar.gz")
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d, stderr = %s", code, exitFailure, stderr)
	}
	if len(h.restoreOpts) != 0 {
		t.Fatalf("restore must not run when the legacy guard rejects")
	}
	if !strings.Contains(stderr, "Reality node instance") {
		t.Fatalf("stderr = %q, want a Reality-instance guard message", stderr)
	}
	// L2 must still have been released even though the guard rejected.
	if h.lock.unlockCalls != 1 {
		t.Fatalf("L2 unlocked %d times, want 1", h.lock.unlockCalls)
	}
}

// TestRestore_LegacyGuardRejectsWhenPublishTxnLeftover proves restore
// refuses when a leftover publish transaction directory exists.
func TestRestore_LegacyGuardRejectsWhenPublishTxnLeftover(t *testing.T) {
	h := newHarness(t)
	txnDir := filepath.Join(t.TempDir(), "publish-txn")
	if err := os.Mkdir(txnDir, 0o700); err != nil {
		t.Fatal(err)
	}
	h.env.publishTxnDir = txnDir
	h.install(t)

	code, _, stderr := runCmd(t, "restore", "/tmp/archive.tar.gz")
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d, stderr = %s", code, exitFailure, stderr)
	}
	if len(h.restoreOpts) != 0 {
		t.Fatalf("restore must not run when a leftover publish transaction exists")
	}
	if !strings.Contains(stderr, "publish --recover") {
		t.Fatalf("stderr = %q, want a publish --recover hint", stderr)
	}
}

// fakeDirInfo is a minimal os.FileInfo standing in for "a directory exists
// here", used only to make DetectInstanceState see a present asset.
type fakeDirInfo struct{}

func (fakeDirInfo) Name() string       { return "state" }
func (fakeDirInfo) Size() int64        { return 0 }
func (fakeDirInfo) Mode() os.FileMode  { return os.ModeDir | 0o700 }
func (fakeDirInfo) ModTime() time.Time { return time.Time{} }
func (fakeDirInfo) IsDir() bool        { return true }
func (fakeDirInfo) Sys() interface{}   { return nil }
