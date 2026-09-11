package backup

import (
	"context"
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
