package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// destFile describes one file placed under a restore destination root.
type destFile struct {
	rel  string
	data string
	mode fs.FileMode
}

func newDest(t *testing.T, files []destFile) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f.rel))
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatalf("mkdir for %s: %v", f.rel, err)
		}
		if err := os.WriteFile(p, []byte(f.data), f.mode); err != nil {
			t.Fatalf("write %s: %v", f.rel, err)
		}
		if err := os.Chmod(p, f.mode); err != nil {
			t.Fatalf("chmod %s: %v", f.rel, err)
		}
	}
	return root
}

func readDestFile(t *testing.T, root, rel string) (string, fs.FileMode) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", rel, err)
	}
	return string(data), info.Mode().Perm()
}

func ownerOf(t *testing.T, path string) (uint32, uint32) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no ownership information for %s", path)
	}
	return st.Uid, st.Gid
}

func TestCaptureSnapshot_RecordsExistingAndMissingPaths(t *testing.T) {
	dest := newDest(t, []destFile{
		{"etc/caddy/Caddyfile", "old caddy\n", 0640},
		{"var/lib/singbox-sub-manager/token", "old token\n", 0600},
	})
	snapRoot := filepath.Join(t.TempDir(), "restore-snapshots")

	snap, err := CaptureSnapshot(context.Background(), CaptureOptions{
		DestRoot:     dest,
		SnapshotRoot: snapRoot,
		Paths:        []string{"etc/caddy/Caddyfile", "var/lib/singbox-sub-manager/token", "etc/sing-box/config.json"},
		Now:          fixedNow,
	})
	if err != nil {
		t.Fatalf("CaptureSnapshot: %v", err)
	}

	byPath := map[string]SnapshotEntry{}
	for _, e := range snap.Entries {
		byPath[e.Path] = e
	}
	if got := byPath["etc/caddy/Caddyfile"]; !got.Exists || got.Mode != 0640 || got.Payload == "" {
		t.Errorf("Caddyfile entry = %+v, want an existing 0640 file with a payload", got)
	}
	if got := byPath["etc/sing-box/config.json"]; got.Exists || got.Payload != "" {
		t.Errorf("absent path entry = %+v, want Exists=false and no payload", got)
	}

	payload := filepath.Join(snap.Dir, byPath["etc/caddy/Caddyfile"].Payload)
	data, err := os.ReadFile(payload)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if string(data) != "old caddy\n" {
		t.Errorf("payload = %q, want the pre-restore bytes", data)
	}

	dirInfo, err := os.Stat(snap.Dir)
	if err != nil {
		t.Fatalf("stat snapshot dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0700 {
		t.Errorf("snapshot dir mode = %o, want 0700", got)
	}
	for _, name := range []string{snapshotMetaName, byPath["etc/caddy/Caddyfile"].Payload} {
		info, err := os.Stat(filepath.Join(snap.Dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := info.Mode().Perm(); got != 0600 {
			t.Errorf("%s mode = %o, want 0600", name, got)
		}
	}

	loaded, err := LoadSnapshot(snap.Dir)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if len(loaded.Entries) != len(snap.Entries) || loaded.TransactionID != snap.TransactionID {
		t.Errorf("reloaded snapshot = %+v, want the captured one", loaded)
	}
}

func TestCaptureSnapshot_RejectsNonRegularTarget(t *testing.T) {
	dest := newDest(t, []destFile{{"etc/caddy/real", "x", 0600}})
	if err := os.Symlink(filepath.Join(dest, "etc/caddy/real"), filepath.Join(dest, "etc/caddy/Caddyfile")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	snapRoot := filepath.Join(t.TempDir(), "restore-snapshots")

	_, err := CaptureSnapshot(context.Background(), CaptureOptions{
		DestRoot:     dest,
		SnapshotRoot: snapRoot,
		Paths:        []string{"etc/caddy/Caddyfile"},
		Now:          fixedNow,
	})
	if !errors.Is(err, ErrUnsupportedSourceType) {
		t.Fatalf("err = %v, want ErrUnsupportedSourceType", err)
	}
	if names := dirNames(t, snapRoot); len(names) != 0 {
		t.Errorf("snapshot root holds %v after a rejected capture, want nothing", names)
	}
}

func TestCaptureSnapshot_Cancellation(t *testing.T) {
	dest := newDest(t, []destFile{{"etc/caddy/Caddyfile", "old", 0640}})
	snapRoot := filepath.Join(t.TempDir(), "restore-snapshots")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := CaptureSnapshot(ctx, CaptureOptions{
		DestRoot:     dest,
		SnapshotRoot: snapRoot,
		Paths:        []string{"etc/caddy/Caddyfile"},
		Now:          fixedNow,
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if names := dirNames(t, snapRoot); len(names) != 0 {
		t.Errorf("snapshot root holds %v after cancellation, want nothing", names)
	}
}

func TestRollback_RestoresBytesModesOwnershipAndDeletesCreated(t *testing.T) {
	dest := newDest(t, []destFile{
		{"etc/caddy/Caddyfile", "original caddy\n", 0640},
		{"var/lib/singbox-sub-manager/token", "original token\n", 0600},
	})
	caddyfile := filepath.Join(dest, "etc/caddy/Caddyfile")

	wantUID, wantGID := ownerOf(t, caddyfile)
	if os.Geteuid() == 0 {
		wantUID, wantGID = 4321, 4322
		if err := os.Chown(caddyfile, int(wantUID), int(wantGID)); err != nil {
			t.Fatalf("chown: %v", err)
		}
	} else {
		t.Logf("not running as root: ownership is only checked against %d:%d", wantUID, wantGID)
	}

	snapRoot := filepath.Join(t.TempDir(), "restore-snapshots")
	paths := []string{"etc/caddy/Caddyfile", "var/lib/singbox-sub-manager/token", "etc/sing-box/config.json"}
	snap, err := CaptureSnapshot(context.Background(), CaptureOptions{DestRoot: dest, SnapshotRoot: snapRoot, Paths: paths, Now: fixedNow})
	if err != nil {
		t.Fatalf("CaptureSnapshot: %v", err)
	}

	// Simulate the restore that has to be undone: overwrite two files with
	// different content and modes, and create one that did not exist.
	if err := os.WriteFile(caddyfile, []byte("new caddy\n"), 0600); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if err := os.Chmod(caddyfile, 0600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(caddyfile, 0, 0); err != nil {
			t.Fatalf("chown: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(dest, "var/lib/singbox-sub-manager/token"), []byte("new token\n"), 0600); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	created := filepath.Join(dest, "etc/sing-box/config.json")
	if err := os.MkdirAll(filepath.Dir(created), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(created, []byte("{}"), 0600); err != nil {
		t.Fatalf("create: %v", err)
	}

	res, err := Rollback(context.Background(), snap, RollbackOptions{DestRoot: dest})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("failed = %v, reasons %v", res.Failed, res.FailureReasons)
	}
	if len(res.Restored) != 2 || len(res.Deleted) != 1 {
		t.Errorf("restored = %v, deleted = %v", res.Restored, res.Deleted)
	}

	if data, mode := readDestFile(t, dest, "etc/caddy/Caddyfile"); data != "original caddy\n" || mode != 0640 {
		t.Errorf("Caddyfile = %q mode %o, want the original bytes and 0640", data, mode)
	}
	if uid, gid := ownerOf(t, caddyfile); uid != wantUID || gid != wantGID {
		t.Errorf("Caddyfile ownership = %d:%d, want %d:%d", uid, gid, wantUID, wantGID)
	}
	if data, _ := readDestFile(t, dest, "var/lib/singbox-sub-manager/token"); data != "original token\n" {
		t.Errorf("token = %q, want the original bytes", data)
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Errorf("a file created by the restore survived the rollback: %v", err)
	}
}

func TestRollback_Cancellation(t *testing.T) {
	dest := newDest(t, []destFile{{"etc/caddy/Caddyfile", "original\n", 0640}})
	snapRoot := filepath.Join(t.TempDir(), "restore-snapshots")
	snap, err := CaptureSnapshot(context.Background(), CaptureOptions{
		DestRoot: dest, SnapshotRoot: snapRoot, Paths: []string{"etc/caddy/Caddyfile"}, Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("CaptureSnapshot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "etc/caddy/Caddyfile"), []byte("new\n"), 0640); err != nil {
		t.Fatalf("overwrite: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := Rollback(ctx, snap, RollbackOptions{DestRoot: dest})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(res.Failed) != 1 || res.FailureReasons["etc/caddy/Caddyfile"] != FailCanceled {
		t.Errorf("failed = %v, reasons = %v", res.Failed, res.FailureReasons)
	}
	if data, _ := readDestFile(t, dest, "etc/caddy/Caddyfile"); data != "new\n" {
		t.Errorf("file = %q, want it untouched by a cancelled rollback", data)
	}
	if _, err := os.Stat(snap.Dir); err != nil {
		t.Errorf("the snapshot was removed after a cancelled rollback: %v", err)
	}
}

func TestRollback_FailureRetainsSnapshot(t *testing.T) {
	dest := newDest(t, []destFile{
		{"etc/caddy/Caddyfile", "original caddy\n", 0640},
		{"var/lib/singbox-sub-manager/token", "original token\n", 0600},
	})
	snapRoot := filepath.Join(t.TempDir(), "restore-snapshots")
	snap, err := CaptureSnapshot(context.Background(), CaptureOptions{
		DestRoot:     dest,
		SnapshotRoot: snapRoot,
		Paths:        []string{"etc/caddy/Caddyfile", "var/lib/singbox-sub-manager/token"},
		Now:          fixedNow,
	})
	if err != nil {
		t.Fatalf("CaptureSnapshot: %v", err)
	}

	// Lose one payload: the rollback for that path can no longer be proven
	// byte-exact and must fail loudly instead of writing something else.
	if err := os.Remove(filepath.Join(snap.Dir, snap.Entries[0].Payload)); err != nil {
		t.Fatalf("remove payload: %v", err)
	}

	res, err := Rollback(context.Background(), snap, RollbackOptions{DestRoot: dest})
	if err == nil {
		t.Fatal("Rollback succeeded with a missing payload")
	}
	if len(res.Failed) != 1 || res.FailureReasons[snap.Entries[0].Path] != FailReadSnapshot {
		t.Errorf("failed = %v, reasons = %v", res.Failed, res.FailureReasons)
	}
	if len(res.Restored) != 1 {
		t.Errorf("restored = %v, want the intact path to still be rolled back", res.Restored)
	}
	if _, err := os.Stat(snap.Dir); err != nil {
		t.Errorf("the snapshot was removed after a failed rollback: %v", err)
	}
}

func TestRollback_RejectsTamperedPayload(t *testing.T) {
	dest := newDest(t, []destFile{{"etc/caddy/Caddyfile", "original\n", 0640}})
	snapRoot := filepath.Join(t.TempDir(), "restore-snapshots")
	snap, err := CaptureSnapshot(context.Background(), CaptureOptions{
		DestRoot: dest, SnapshotRoot: snapRoot, Paths: []string{"etc/caddy/Caddyfile"}, Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("CaptureSnapshot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(snap.Dir, snap.Entries[0].Payload), []byte("tampered\n"), 0600); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	res, err := Rollback(context.Background(), snap, RollbackOptions{DestRoot: dest})
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("err = %v, want ErrChecksumMismatch", err)
	}
	if res.FailureReasons["etc/caddy/Caddyfile"] != FailReadSnapshot {
		t.Errorf("reasons = %v", res.FailureReasons)
	}
	if data, _ := readDestFile(t, dest, "etc/caddy/Caddyfile"); data != "original\n" {
		t.Errorf("file = %q, want a tampered payload never to be written", data)
	}
}

func TestCleanupSnapshot(t *testing.T) {
	dest := newDest(t, []destFile{{"etc/caddy/Caddyfile", "original\n", 0640}})
	snapRoot := filepath.Join(t.TempDir(), "restore-snapshots")
	snap, err := CaptureSnapshot(context.Background(), CaptureOptions{
		DestRoot: dest, SnapshotRoot: snapRoot, Paths: []string{"etc/caddy/Caddyfile"}, Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("CaptureSnapshot: %v", err)
	}
	if err := CleanupSnapshot(context.Background(), snap); err != nil {
		t.Fatalf("CleanupSnapshot: %v", err)
	}
	if _, err := os.Stat(snap.Dir); !os.IsNotExist(err) {
		t.Errorf("the snapshot survived cleanup: %v", err)
	}
	if err := CleanupSnapshot(context.Background(), nil); err != nil {
		t.Errorf("CleanupSnapshot(nil) = %v, want nil", err)
	}
}

func TestSnapshotPruneDoesNotDeleteActiveOrLatestFailure(t *testing.T) {
	dest := newDest(t, []destFile{{"etc/caddy/Caddyfile", "original\n", 0640}})
	snapRoot := filepath.Join(t.TempDir(), "restore-snapshots")

	capture := func(at time.Time) *RollbackSnapshot {
		t.Helper()
		snap, err := CaptureSnapshot(context.Background(), CaptureOptions{
			DestRoot:     dest,
			SnapshotRoot: snapRoot,
			Paths:        []string{"etc/caddy/Caddyfile"},
			Now:          func() time.Time { return at },
		})
		if err != nil {
			t.Fatalf("CaptureSnapshot: %v", err)
		}
		return snap
	}

	// Every image here is older than SnapshotMaxAge, so survival can only be
	// explained by the active-transaction and keep-the-newest rules.
	now := fixedClock
	active := capture(now.Add(-30 * 24 * time.Hour))
	stale := capture(now.Add(-20 * 24 * time.Hour))
	recentFailure := capture(now.Add(-8 * 24 * time.Hour))

	warnings := PruneSnapshots(snapRoot, now, SnapshotMaxAge, active.TransactionID)
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	if _, err := os.Stat(active.Dir); err != nil {
		t.Errorf("the active transaction was pruned: %v", err)
	}
	if _, err := os.Stat(recentFailure.Dir); err != nil {
		t.Errorf("the most recent snapshot was pruned: %v", err)
	}
	if _, err := os.Stat(stale.Dir); !os.IsNotExist(err) {
		t.Errorf("the stale snapshot survived pruning: %v", err)
	}
}

func TestPruneSnapshots_KeepsEverythingWithinMaxAge(t *testing.T) {
	dest := newDest(t, []destFile{{"etc/caddy/Caddyfile", "original\n", 0640}})
	snapRoot := filepath.Join(t.TempDir(), "restore-snapshots")
	for _, age := range []time.Duration{time.Hour, 48 * time.Hour} {
		if _, err := CaptureSnapshot(context.Background(), CaptureOptions{
			DestRoot:     dest,
			SnapshotRoot: snapRoot,
			Paths:        []string{"etc/caddy/Caddyfile"},
			Now:          func() time.Time { return fixedClock.Add(-age) },
		}); err != nil {
			t.Fatalf("CaptureSnapshot: %v", err)
		}
	}
	PruneSnapshots(snapRoot, fixedClock, SnapshotMaxAge, "")
	if names := dirNames(t, snapRoot); len(names) != 2 {
		t.Errorf("snapshot root holds %v, want both recent snapshots", names)
	}
}

func TestPruneSnapshots_MissingRootIsNotAnError(t *testing.T) {
	if warnings := PruneSnapshots(filepath.Join(t.TempDir(), "absent"), fixedClock, SnapshotMaxAge, ""); len(warnings) != 0 {
		t.Errorf("warnings = %v, want none for a missing snapshot root", warnings)
	}
}

func TestRollback_Timeout(t *testing.T) {
	dest := newDest(t, []destFile{{"etc/caddy/Caddyfile", "original\n", 0640}})
	snapRoot := filepath.Join(t.TempDir(), "restore-snapshots")
	snap, err := CaptureSnapshot(context.Background(), CaptureOptions{
		DestRoot: dest, SnapshotRoot: snapRoot, Paths: []string{"etc/caddy/Caddyfile"}, Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("CaptureSnapshot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "etc/caddy/Caddyfile"), []byte("new\n"), 0640); err != nil {
		t.Fatalf("overwrite: %v", err)
	}

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	res, err := Rollback(ctx, snap, RollbackOptions{DestRoot: dest})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if res.FailureReasons["etc/caddy/Caddyfile"] != FailDeadlineExceeded {
		t.Errorf("reasons = %v, want %q", res.FailureReasons, FailDeadlineExceeded)
	}
	if _, err := os.Stat(snap.Dir); err != nil {
		t.Errorf("the snapshot was removed after a timed-out rollback: %v", err)
	}
}

func TestRollback_DoesNotReleaseTheCallersLease(t *testing.T) {
	dest := newDest(t, []destFile{{"etc/caddy/Caddyfile", "original\n", 0640}})
	snapRoot := filepath.Join(t.TempDir(), "restore-snapshots")
	snap, err := CaptureSnapshot(context.Background(), CaptureOptions{
		DestRoot: dest, SnapshotRoot: snapRoot, Paths: []string{"etc/caddy/Caddyfile"}, Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("CaptureSnapshot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "etc/caddy/Caddyfile"), []byte("new\n"), 0640); err != nil {
		t.Fatalf("overwrite: %v", err)
	}

	held := &fakeLocker{}
	lease, err := AcquireWithRetry(context.Background(), held, time.Millisecond, time.Second)
	if err != nil {
		t.Fatalf("AcquireWithRetry: %v", err)
	}

	if _, err := Rollback(context.Background(), snap, RollbackOptions{DestRoot: dest, Lease: lease}); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if held.unlockCalls != 0 {
		t.Errorf("Rollback released a lease it does not own (%d Unlock calls)", held.unlockCalls)
	}
	if err := lease.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if held.unlockCalls != 1 {
		t.Errorf("owner released the lease %d times, want 1", held.unlockCalls)
	}
}

func TestPruneSnapshots_KeepsUnreadableDirectories(t *testing.T) {
	snapRoot := t.TempDir()
	foreign := filepath.Join(snapRoot, "20200101T000000Z-deadbeef")
	if err := os.Mkdir(foreign, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	warnings := PruneSnapshots(snapRoot, fixedClock, SnapshotMaxAge, "")
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want one about the unreadable directory", warnings)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("an unreadable directory was pruned: %v", err)
	}
}

// Regression: a missing intermediate parent (as opposed to a missing leaf,
// which is a normal already-deleted success) is FailDelete, matching v0.7's
// actual behavior — os.Remove(target) tolerated the leaf itself being gone,
// but the syncDir(parent) call right after it failed whenever the parent
// was gone too.
func TestRollback_DeleteWithMissingParentIsFailDeleteNotSuccess(t *testing.T) {
	dest := t.TempDir() // "etc" (and everything under it) does not exist.
	snap := &RollbackSnapshot{
		SchemaVersion: snapshotSchemaVersion,
		TransactionID: "test-txn",
		CreatedAt:     fixedClock,
		Dir:           t.TempDir(),
		Entries: []SnapshotEntry{
			{Path: caddyPath, Exists: false},
		},
	}

	res, err := Rollback(context.Background(), snap, RollbackOptions{DestRoot: dest})
	if err == nil {
		t.Fatal("Rollback succeeded with a missing intermediate parent")
	}
	if len(res.Deleted) != 0 {
		t.Errorf("deleted = %v, want none (a missing parent is a failure, not a silent success)", res.Deleted)
	}
	if res.FailureReasons[caddyPath] != FailDelete {
		t.Errorf("reason = %q, want %q", res.FailureReasons[caddyPath], FailDelete)
	}
}

// --- Task 2 acceptance matrix (design §7.1): T1-删, T2-删, P2, P3-回滚 ---
//
// Same symlink-replacement technique as the Restore-side tests in
// restore_test.go: the real directory is renamed within root, then a
// symlink to an outside location takes its original name.

// T1-删: fd-relative delete of an Exists=false target.
func TestRollback_T1Delete_SucceedsUnderTheRenamedDirectory(t *testing.T) {
	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, "etc", "caddy"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	snapRoot := filepath.Join(t.TempDir(), "restore-snapshots")
	snap, err := CaptureSnapshot(context.Background(), CaptureOptions{
		DestRoot: dest, SnapshotRoot: snapRoot, Paths: []string{caddyPath}, Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("CaptureSnapshot: %v", err)
	}
	if snap.Entries[0].Exists {
		t.Fatalf("test setup: want the target to not exist at capture time")
	}
	// The restore this rollback undoes created the file.
	if err := os.WriteFile(filepath.Join(dest, filepath.FromSlash(caddyPath)), []byte("restored\n"), 0640); err != nil {
		t.Fatalf("write: %v", err)
	}

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "Caddyfile"), []byte("attacker file\n"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	fired := false
	orig := hookBeforeMutation
	defer func() { hookBeforeMutation = orig }()
	hookBeforeMutation = func() {
		if fired {
			return
		}
		fired = true
		if err := os.Rename(filepath.Join(dest, "etc", "caddy"), filepath.Join(dest, "etc", "caddy.moved")); err != nil {
			t.Fatalf("rename: %v", err)
		}
		if err := os.Symlink(outside, filepath.Join(dest, "etc", "caddy")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	}

	res, err := Rollback(context.Background(), snap, RollbackOptions{DestRoot: dest})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if len(res.Deleted) != 1 || res.Deleted[0] != caddyPath {
		t.Errorf("deleted = %v, want %s deleted", res.Deleted, caddyPath)
	}
	if data, readErr := os.ReadFile(filepath.Join(outside, "Caddyfile")); readErr != nil || string(data) != "attacker file\n" {
		t.Errorf("the outside file was touched: data=%q err=%v", data, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(dest, "etc", "caddy.moved", "Caddyfile")); !os.IsNotExist(statErr) {
		t.Errorf("the real Caddyfile under the moved directory was not deleted: %v", statErr)
	}
}

// T2-删: the ancestor-chain re-verification catches a directory moved
// entirely out of root before the delete.
func TestRollback_T2Delete_AncestorReverificationCatchesAMoveOutOfRoot(t *testing.T) {
	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, "etc", "caddy"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	snapRoot := filepath.Join(t.TempDir(), "restore-snapshots")
	snap, err := CaptureSnapshot(context.Background(), CaptureOptions{
		DestRoot: dest, SnapshotRoot: snapRoot, Paths: []string{caddyPath}, Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("CaptureSnapshot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dest, filepath.FromSlash(caddyPath)), []byte("restored\n"), 0640); err != nil {
		t.Fatalf("write: %v", err)
	}
	outside := t.TempDir()

	fired := false
	orig := hookAfterOpen
	defer func() { hookAfterOpen = orig }()
	hookAfterOpen = func() {
		if fired {
			return
		}
		fired = true
		if err := os.Rename(filepath.Join(dest, "etc", "caddy"), filepath.Join(outside, "caddy")); err != nil {
			t.Fatalf("rename: %v", err)
		}
	}

	res, err := Rollback(context.Background(), snap, RollbackOptions{DestRoot: dest})
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath", err)
	}
	if res.FailureReasons[caddyPath] != FailUnsafePath {
		t.Errorf("reason = %q, want %q", res.FailureReasons[caddyPath], FailUnsafePath)
	}
	if names := dirNames(t, filepath.Join(outside, "caddy")); len(names) != 1 {
		t.Errorf("moved-out directory contents = %v, want only the untouched Caddyfile", names)
	}
}

// P2: reading a snapshot payload does not follow a symlink swapped in for
// it, even when the swapped-in target's bytes and recorded checksum match.
func TestRollback_P2_PayloadReadDoesNotFollowSymlink(t *testing.T) {
	dest := newDest(t, []destFile{{caddyPath, "original caddy\n", 0640}})
	snapRoot := filepath.Join(t.TempDir(), "restore-snapshots")
	snap, err := CaptureSnapshot(context.Background(), CaptureOptions{
		DestRoot: dest, SnapshotRoot: snapRoot, Paths: []string{caddyPath}, Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("CaptureSnapshot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dest, filepath.FromSlash(caddyPath)), []byte("tampered target\n"), 0640); err != nil {
		t.Fatalf("overwrite: %v", err)
	}

	outsideFile := filepath.Join(t.TempDir(), "outside-payload")
	outsideBody := "attacker-controlled bytes\n"
	if err := os.WriteFile(outsideFile, []byte(outsideBody), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	payloadPath := filepath.Join(snap.Dir, snap.Entries[0].Payload)
	if err := os.Remove(payloadPath); err != nil {
		t.Fatalf("remove payload: %v", err)
	}
	if err := os.Symlink(outsideFile, payloadPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// Recorded checksum matches the outside bytes, so only "did the read
	// follow the symlink" decides the outcome, not the checksum check.
	sum := sha256.Sum256([]byte(outsideBody))
	snap.Entries[0].SHA256 = hex.EncodeToString(sum[:])

	res, err := Rollback(context.Background(), snap, RollbackOptions{DestRoot: dest})
	if err == nil {
		t.Fatal("Rollback succeeded with a symlinked payload")
	}
	if res.FailureReasons[caddyPath] != FailReadSnapshot {
		t.Errorf("reason = %q, want %q", res.FailureReasons[caddyPath], FailReadSnapshot)
	}
	if data, _ := readDestFile(t, dest, caddyPath); data != "tampered target\n" {
		t.Errorf("target = %q, want it left untouched by the rejected rollback", data)
	}
}

// P3-回滚: the payload-name-is-a-single-component check applies only to
// Exists=true entries. A hand-built mixed snapshot value drives both halves
// in one Rollback call: an Exists=false entry with an empty payload must
// still delete its target, and an Exists=true entry with an unsafe payload
// name must be rejected without touching its target.
func TestRollback_P3_PayloadNameValidationOnlyAppliesToExistsTrue(t *testing.T) {
	dest := newDest(t, []destFile{
		{"etc/caddy/Caddyfile", "keep me\n", 0640},
		{"var/lib/singbox-sub-manager/token", "created by restore\n", 0600},
	})
	base := t.TempDir()
	snapDir := filepath.Join(base, "txn")
	if err := os.MkdirAll(snapDir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	outsideBody := "escaped bytes\n"
	if err := os.WriteFile(filepath.Join(base, "outside"), []byte(outsideBody), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	sum := sha256.Sum256([]byte(outsideBody))

	snap := &RollbackSnapshot{
		SchemaVersion: snapshotSchemaVersion,
		TransactionID: "test-txn",
		CreatedAt:     fixedClock,
		Dir:           snapDir,
		Entries: []SnapshotEntry{
			{Path: "var/lib/singbox-sub-manager/token", Exists: false, Payload: ""},
			{Path: "etc/caddy/Caddyfile", Exists: true, Mode: 0640, SHA256: hex.EncodeToString(sum[:]), Payload: "../outside"},
		},
	}

	res, err := Rollback(context.Background(), snap, RollbackOptions{DestRoot: dest})
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want it to wrap ErrUnsafePath", err)
	}
	if len(res.Deleted) != 1 || res.Deleted[0] != "var/lib/singbox-sub-manager/token" {
		t.Errorf("deleted = %v, want the Exists=false entry deleted despite its empty payload", res.Deleted)
	}
	if _, statErr := os.Stat(filepath.Join(dest, "var/lib/singbox-sub-manager/token")); !os.IsNotExist(statErr) {
		t.Errorf("the created-by-restore target survived: %v", statErr)
	}
	if res.FailureReasons["etc/caddy/Caddyfile"] != FailReadSnapshot {
		t.Errorf("reason = %q, want %q", res.FailureReasons["etc/caddy/Caddyfile"], FailReadSnapshot)
	}
	if data, _ := readDestFile(t, dest, "etc/caddy/Caddyfile"); data != "keep me\n" {
		t.Errorf("Caddyfile = %q, want it untouched", data)
	}
}

// Regression: a rejected payload (unsafe name, missing, tampered) must not
// leave newly-created parent directories behind under DestRoot. Rollback
// never cleans those up on failure (unlike Restore), so v0.7 order —
// nothing about DestRoot is touched until after the payload is read and
// checksummed — must be preserved: v0.7 created nothing for a rejected
// payload, since resolveUnderRoot never created directories at all.
func TestRollback_RejectedPayloadCreatesNothingUnderDestRoot(t *testing.T) {
	dest := t.TempDir()
	base := t.TempDir()
	snapDir := filepath.Join(base, "txn")
	if err := os.MkdirAll(snapDir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, "outside"), []byte("escaped\n"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	sum := sha256.Sum256([]byte("escaped\n"))

	snap := &RollbackSnapshot{
		SchemaVersion: snapshotSchemaVersion,
		TransactionID: "test-txn",
		CreatedAt:     fixedClock,
		Dir:           snapDir,
		Entries: []SnapshotEntry{
			{Path: caddyPath, Exists: true, Mode: 0640, SHA256: hex.EncodeToString(sum[:]), Payload: "../outside"},
		},
	}

	res, err := Rollback(context.Background(), snap, RollbackOptions{DestRoot: dest})
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want it to wrap ErrUnsafePath", err)
	}
	if res.FailureReasons[caddyPath] != FailReadSnapshot {
		t.Errorf("reason = %q, want %q", res.FailureReasons[caddyPath], FailReadSnapshot)
	}
	if _, statErr := os.Stat(filepath.Join(dest, "etc")); !os.IsNotExist(statErr) {
		t.Errorf("a rejected payload caused Rollback to create directories under DestRoot: stat err = %v", statErr)
	}
}
