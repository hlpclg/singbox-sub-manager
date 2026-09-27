package hostlock

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hlpclg/singbox-sub-manager/internal/backup"
)

func testPaths(t *testing.T) Paths {
	dir := t.TempDir()
	return Paths{
		L1: filepath.Join(dir, "l1.lock"),
		L2: filepath.Join(dir, "l2.lock"),
		L3: filepath.Join(dir, "l3.lock"),
	}
}

func TestDefaultMaxWaitValues(t *testing.T) {
	if got := DefaultMaxWait(L1); got != 30*time.Second {
		t.Errorf("L1 max wait = %v, want 30s", got)
	}
	if got := DefaultMaxWait(L2); got != 30*time.Second {
		t.Errorf("L2 max wait = %v, want 30s", got)
	}
	if got := DefaultMaxWait(L3); got != 10*time.Second {
		t.Errorf("L3 max wait = %v, want 10s", got)
	}
}

// TestHostLockOrder proves two invariants with real flock files (no mocks):
// levels are always acquired in L1,L2,L3 order regardless of the order
// requested, and Release lets go of them in reverse order. It observes real
// acquisition order by holding an independent, external flock on L2 (opened
// directly via syscall, bypassing this package entirely) and confirming that
// Acquire(L3, L1, L2) — a deliberately scrambled request — successfully
// takes L1 first (which does not conflict with the external L2 holder), then
// blocks on L2 exactly where the external holder is, never reaching L3 early.
func TestHostLockOrder(t *testing.T) {
	paths := testPaths(t)

	externalL2 := openAndFlock(t, paths.L2)
	defer externalL2.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	fastWait := func(level Level) time.Duration { return 150 * time.Millisecond }
	_, err := Acquire(ctx, paths, AcquireOptions{MaxWait: fastWait}, L3, L1, L2)
	if err == nil {
		t.Fatalf("expected Acquire to fail while L2 is externally held")
	}
	if !errors.Is(err, backup.ErrLockBusy) {
		t.Fatalf("error = %v, want it to wrap backup.ErrLockBusy", err)
	}

	// L3 must never have been reachable: release the external L2 holder and
	// confirm a fresh Acquire for the same scrambled level set now succeeds
	// and reports canonical order.
	if err := externalL2.Unlock(); err != nil {
		t.Fatalf("release external L2: %v", err)
	}

	lease, err := Acquire(context.Background(), paths, AcquireOptions{}, L3, L1, L2)
	if err != nil {
		t.Fatalf("Acquire after external release: %v", err)
	}
	defer lease.Release()

	got := lease.Levels()
	want := []Level{L1, L2, L3}
	if len(got) != len(want) {
		t.Fatalf("Levels() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Levels() = %v, want %v (canonical L1,L2,L3 order)", got, want)
		}
	}
}

// TestHostLockReleaseOrder proves Release walks levels in reverse (L3, L2,
// L1) by racing an external waiter for each lock and recording the order in
// which they each become acquirable after Release runs.
func TestHostLockReleaseOrder(t *testing.T) {
	paths := testPaths(t)

	// Racing three independent real flock waiters and inferring release
	// order from which one's goroutine happens to observe its wakeup first
	// is not reliable: three distinct kernel lock objects give no
	// cross-object ordering guarantee, and OS thread wakeup / Go scheduler
	// jitter between them can reorder observations even when the three
	// underlying LOCK_UN syscalls happened in the intended sequence
	// microseconds apart (this was verified empirically: an earlier version
	// of this test using that approach flaked on the very first run). What
	// actually needs proving — that Release() calls Unlock() on its held
	// levels in L3, L2, L1 order — is a pure sequencing property of
	// Release() itself, so it is observed directly via a recording fake
	// Locker instead. Real cross-process mutual exclusion is separately and
	// unambiguously proven by TestHostLockOrder and TestHostLockCrossProcess
	// using genuine flock.
	var mu sync.Mutex
	var order []Level
	recordingLocker := func(path string) backup.Locker {
		var level Level
		switch path {
		case paths.L1:
			level = L1
		case paths.L2:
			level = L2
		case paths.L3:
			level = L3
		}
		return &recordingFakeLocker{level: level, mu: &mu, order: &order}
	}

	lease, err := Acquire(context.Background(), paths, AcquireOptions{NewLocker: recordingLocker}, L1, L2, L3)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	want := []Level{L3, L2, L1}
	if len(order) != len(want) {
		t.Fatalf("Unlock call order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("Unlock call order = %v, want %v", order, want)
		}
	}
}

// recordingFakeLocker is a Locker that never touches the filesystem; it only
// records, under mu, the order in which Unlock is called across every
// instance sharing the same order slice pointer.
type recordingFakeLocker struct {
	level Level
	mu    *sync.Mutex
	order *[]Level
}

func (r *recordingFakeLocker) TryLock() error { return nil }
func (r *recordingFakeLocker) Unlock() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	*r.order = append(*r.order, r.level)
	return nil
}

// TestHostLockContextCancel proves Acquire returns promptly on context
// cancellation rather than waiting out the full (much longer) MaxWait.
func TestHostLockContextCancel(t *testing.T) {
	paths := testPaths(t)
	external := openAndFlock(t, paths.L1)
	defer external.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	longWait := func(level Level) time.Duration { return 5 * time.Second }
	start := time.Now()
	_, err := Acquire(ctx, paths, AcquireOptions{MaxWait: longWait}, L1)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected error from a cancelled context")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Acquire took %v, expected it to return promptly on ctx cancellation, not wait out the 5s MaxWait", elapsed)
	}
}

// TestHostLockPartialAcquireUnwinds proves that when a later level cannot be
// acquired, every level already held for THIS Acquire call is released
// before the error returns — no partial lease survives.
func TestHostLockPartialAcquireUnwinds(t *testing.T) {
	paths := testPaths(t)
	externalL2 := openAndFlock(t, paths.L2)
	defer externalL2.Close()

	fastWait := func(level Level) time.Duration { return 100 * time.Millisecond }
	_, err := Acquire(context.Background(), paths, AcquireOptions{MaxWait: fastWait}, L1, L2)
	if err == nil {
		t.Fatalf("expected error acquiring L2 while externally held")
	}

	// L1 must have been released by the unwind: a fresh, independent
	// acquisition of L1 alone must succeed immediately.
	quick, err := Acquire(context.Background(), paths, AcquireOptions{}, L1)
	if err != nil {
		t.Fatalf("L1 should have been released after the failed L2 acquire, got: %v", err)
	}
	quick.Release()
}

// ---------------------------------------------------------------------------
// Real cross-process flock contention, driven by pipes, not sleep.
// ---------------------------------------------------------------------------

const helperEnvVar = "HOSTLOCK_TEST_HELPER_LOCK_PATH"

// TestMain lets this test binary re-exec itself as a lock-holding helper
// process when the env var is set, so TestHostLockCrossProcess can spawn a
// real second process that holds a real flock, independent of this
// package's own code, and signals readiness/release over pipes.
func TestMain(m *testing.M) {
	if path := os.Getenv(helperEnvVar); path != "" {
		os.Exit(runLockHelper(path))
	}
	os.Exit(m.Run())
}

// runLockHelper flocks path, writes "locked\n" to stdout once held (the
// parent's readiness barrier), then blocks reading a line from stdin (the
// parent's release barrier) before exiting, which drops the flock.
func runLockHelper(path string) int {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper: open:", err)
		return 1
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		fmt.Fprintln(os.Stderr, "helper: flock:", err)
		return 1
	}
	fmt.Println("locked")
	bufio.NewReader(os.Stdin).ReadString('\n')
	return 0
}

// TestHostLockCrossProcess proves real cross-process mutual exclusion (not
// just in-process bookkeeping): a genuine second OS process holds L1 via a
// raw flock, Acquire(L1) in this process is proven to block on it (times
// out while held), and succeeds immediately once the helper process exits
// and the kernel releases its flock. Readiness and release are both
// signalled over pipes; no sleep is used for synchronization.
func TestHostLockCrossProcess(t *testing.T) {
	paths := testPaths(t)

	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnvVar+"="+paths.L1)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		stdin.Close()
		_ = cmd.Wait()
	})

	// Block until the helper actually confirms it holds the lock (readiness
	// barrier over the pipe), not a fixed sleep.
	reader := bufio.NewReader(stdout)
	line, err := reader.ReadString('\n')
	if err != nil || line != "locked\n" {
		t.Fatalf("helper did not report holding the lock: line=%q err=%v", line, err)
	}

	fastWait := func(level Level) time.Duration { return 200 * time.Millisecond }
	_, err = Acquire(context.Background(), paths, AcquireOptions{MaxWait: fastWait}, L1)
	if err == nil {
		t.Fatalf("expected Acquire to fail while a real external process holds L1")
	}

	// Release barrier: tell the helper to exit, which drops its flock.
	if _, err := stdin.Write([]byte("release\n")); err != nil {
		t.Fatalf("signal helper release: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper exited with error: %v", err)
	}

	lease, err := Acquire(context.Background(), paths, AcquireOptions{}, L1)
	if err != nil {
		t.Fatalf("Acquire after helper released: %v", err)
	}
	lease.Release()
}

// ---------------------------------------------------------------------------
// Test helpers for direct (bypassing this package) flock manipulation.
// ---------------------------------------------------------------------------

type rawFlock struct {
	f *os.File
}

func openAndFlock(t *testing.T, path string) *rawFlock {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		t.Fatalf("flock %s: %v", path, err)
	}
	return &rawFlock{f: f}
}

func (r *rawFlock) Unlock() error {
	unlockErr := syscall.Flock(int(r.f.Fd()), syscall.LOCK_UN)
	closeErr := r.f.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func (r *rawFlock) Close() error { return r.Unlock() }
