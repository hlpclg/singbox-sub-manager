package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hlpclg/singbox-sub-manager/internal/backup"
	"github.com/hlpclg/singbox-sub-manager/internal/health"
	"github.com/hlpclg/singbox-sub-manager/internal/monitor"
)

var testClock = time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)

// countingLocker is the recovery lock as the command sees it.
type countingLocker struct {
	tryCalls    int
	unlockCalls int
	tryErr      error
	unlockErr   error
	held        bool
}

func (l *countingLocker) TryLock() error {
	l.tryCalls++
	if l.tryErr != nil {
		return l.tryErr
	}
	l.held = true
	return nil
}

func (l *countingLocker) Unlock() error {
	l.unlockCalls++
	l.held = false
	return l.unlockErr
}

// harness wires a backupEnv of stubs and records what the command did.
type harness struct {
	env  backupEnv
	lock *countingLocker

	restoreOpts   []backup.RestoreOptions
	restoreResult backup.Result
	restoreErr    error

	restarted     []string
	restartErr    error
	recheckStatus []health.Status // one entry per recheck call; missing means pass
	recheckCalls  int
	recheckIDs    [][]string
	onRecheck     func()

	rollbackCalls  int
	rollbackOpts   []backup.RollbackOptions
	rollbackResult backup.RollbackResult
	rollbackErr    error

	cleanupCalls int
	cleanupErr   error

	preimage *backup.RollbackSnapshot
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{lock: &countingLocker{}}
	h.preimage = &backup.RollbackSnapshot{TransactionID: "tx", Dir: filepath.Join(t.TempDir(), "tx")}
	h.restoreResult = backup.Result{
		Restored:       []string{"etc/caddy/Caddyfile"},
		FailureReasons: map[string]string{},
	}

	h.env = backupEnv{
		sourceRoot:   t.TempDir(),
		destRoot:     t.TempDir(),
		backupDir:    t.TempDir(),
		snapshotRoot: t.TempDir(),
		version:      "v0.7.0",
		now:          func() time.Time { return testClock },

		create: func(ctx context.Context, opts backup.CreateOptions) (backup.Manifest, error) {
			return backup.Manifest{}, nil
		},
		readManifest: backup.ReadManifest,
		restore: func(ctx context.Context, archive string, opts backup.RestoreOptions) (backup.Result, error) {
			h.restoreOpts = append(h.restoreOpts, opts)
			res := h.restoreResult
			if opts.CapturePreimage && res.Preimage == nil {
				res.Preimage = h.preimage
			}
			return res, h.restoreErr
		},
		rollback: func(ctx context.Context, snap *backup.RollbackSnapshot, opts backup.RollbackOptions) (backup.RollbackResult, error) {
			h.rollbackCalls++
			h.rollbackOpts = append(h.rollbackOpts, opts)
			return h.rollbackResult, h.rollbackErr
		},
		cleanup: func(ctx context.Context, snap *backup.RollbackSnapshot) error {
			h.cleanupCalls++
			return h.cleanupErr
		},
		newLock: func() backup.Locker { return h.lock },
		restart: func(ctx context.Context, svc string) error {
			h.restarted = append(h.restarted, svc)
			return h.restartErr
		},
		recheck: func(ctx context.Context, ids ...string) []health.Result {
			h.recheckIDs = append(h.recheckIDs, ids)
			if h.onRecheck != nil {
				h.onRecheck()
			}
			status := health.StatusPass
			if h.recheckCalls < len(h.recheckStatus) {
				status = h.recheckStatus[h.recheckCalls]
			}
			h.recheckCalls++
			var out []health.Result
			for _, id := range ids {
				out = append(out, health.Result{ID: id, Status: status})
			}
			return out
		},

		lockRetryInterval: time.Millisecond,
		lockMaxWait:       10 * time.Millisecond,
		restartTimeout:    time.Second,
		recheckTimeout:    time.Second,
	}
	return h
}

func (h *harness) install(t *testing.T) {
	t.Helper()
	original := newBackupEnv
	newBackupEnv = func() backupEnv { return h.env }
	t.Cleanup(func() { newBackupEnv = original })
}

func runCmd(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// --- backup ---------------------------------------------------------------

func writeArchiveFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(name), 0600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func TestBackupCmd_RetentionPrunesOldest(t *testing.T) {
	h := newHarness(t)
	dir := h.env.backupDir
	older := []string{
		"backup-20260901T000000Z.tar.gz",
		"backup-20260902T000000Z.tar.gz",
		"backup-20260903T000000Z.tar.gz",
		"backup-20260903T000000Z-1.tar.gz",
	}
	for _, name := range older {
		writeArchiveFile(t, dir, name)
	}
	// Files the operator put there must survive whatever the policy does.
	writeArchiveFile(t, dir, "keep-me.tar.gz")
	writeArchiveFile(t, dir, "notes.txt")

	h.env.create = func(ctx context.Context, opts backup.CreateOptions) (backup.Manifest, error) {
		name := "backup-20260904T000000Z.tar.gz"
		writeArchiveFile(t, dir, name)
		return backup.Manifest{ArchivePath: filepath.Join(dir, name)}, nil
	}
	h.install(t)

	code, stdout, stderr := runCmd(t, "backup", "--keep", "2")
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "backup-20260904T000000Z.tar.gz") {
		t.Errorf("stdout = %q, want the new archive named", stdout)
	}

	got := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		got[e.Name()] = true
	}
	for _, name := range []string{"backup-20260904T000000Z.tar.gz", "backup-20260903T000000Z-1.tar.gz", "keep-me.tar.gz", "notes.txt"} {
		if !got[name] {
			t.Errorf("%s was removed, want it kept", name)
		}
	}
	for _, name := range []string{"backup-20260901T000000Z.tar.gz", "backup-20260902T000000Z.tar.gz", "backup-20260903T000000Z.tar.gz"} {
		if got[name] {
			t.Errorf("%s survived pruning", name)
		}
	}
}

func TestBackupCmd_KeepZeroDisablesPrune(t *testing.T) {
	h := newHarness(t)
	dir := h.env.backupDir
	for i := 1; i <= 3; i++ {
		writeArchiveFile(t, dir, fmt.Sprintf("backup-2026090%dT000000Z.tar.gz", i))
	}
	h.env.create = func(ctx context.Context, opts backup.CreateOptions) (backup.Manifest, error) {
		return backup.Manifest{ArchivePath: filepath.Join(dir, "backup-20260904T000000Z.tar.gz")}, nil
	}
	h.install(t)

	if code, _, stderr := runCmd(t, "backup", "--keep", "0"); code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if names, _ := os.ReadDir(dir); len(names) != 3 {
		t.Errorf("backup dir holds %d files, want all 3 kept", len(names))
	}
}

func TestBackupCmd_OutSkipsRetention(t *testing.T) {
	h := newHarness(t)
	dir := h.env.backupDir
	for i := 1; i <= 3; i++ {
		writeArchiveFile(t, dir, fmt.Sprintf("backup-2026090%dT000000Z.tar.gz", i))
	}
	var gotOpts backup.CreateOptions
	h.env.create = func(ctx context.Context, opts backup.CreateOptions) (backup.Manifest, error) {
		gotOpts = opts
		return backup.Manifest{ArchivePath: opts.Out}, nil
	}
	h.install(t)

	out := filepath.Join(t.TempDir(), "manual.tar.gz")
	if code, _, stderr := runCmd(t, "backup", "--out", out, "--keep", "1"); code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if gotOpts.Out != out {
		t.Errorf("Out = %q, want %q", gotOpts.Out, out)
	}
	if names, _ := os.ReadDir(dir); len(names) != 3 {
		t.Errorf("--out still pruned the backup directory: %d files left", len(names))
	}
}

func TestBackupCmd_RejectsBadArguments(t *testing.T) {
	h := newHarness(t)
	h.install(t)
	for _, args := range [][]string{
		{"backup", "--keep", "-1"},
		{"backup", "extra"},
		{"backup", "--nope"},
	} {
		if code, _, _ := runCmd(t, args...); code != exitUsage {
			t.Errorf("%v: exit = %d, want %d", args, code, exitUsage)
		}
	}
}

func TestBackupCmd_WarnsAboutSensitiveContent(t *testing.T) {
	h := newHarness(t)
	h.env.create = func(ctx context.Context, opts backup.CreateOptions) (backup.Manifest, error) {
		return backup.Manifest{
			ArchivePath:    filepath.Join(opts.BackupDir, "backup-20260904T000000Z.tar.gz"),
			Files:          []backup.FileEntry{{Path: "etc/caddy/Caddyfile"}},
			Skipped:        []string{"var/lib/singbox-sub-manager/monitor-paused"},
			SkippedReasons: map[string]string{"var/lib/singbox-sub-manager/monitor-paused": backup.SkipNotFound},
		}, nil
	}
	h.install(t)

	code, stdout, _ := runCmd(t, "backup")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "private keys") {
		t.Errorf("stdout = %q, want the sensitive-content warning", stdout)
	}
	if !strings.Contains(stdout, "monitor-paused") || !strings.Contains(stdout, backup.SkipNotFound) {
		t.Errorf("stdout = %q, want skipped paths and reasons", stdout)
	}
}

// --- backup list ----------------------------------------------------------

func TestBackupListCmd_OrdersByTime(t *testing.T) {
	h := newHarness(t)
	dir := h.env.backupDir
	times := map[string]time.Time{
		"backup-20260901T000000Z.tar.gz": time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		"backup-20260903T000000Z.tar.gz": time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC),
		"backup-20260902T000000Z.tar.gz": time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
	}
	for name := range times {
		writeArchiveFile(t, dir, name)
	}
	h.env.readManifest = func(ctx context.Context, path string) (backup.Manifest, error) {
		return backup.Manifest{CreatedAt: times[filepath.Base(path)], ProxyctlVersion: "v0.6.0"}, nil
	}
	h.install(t)

	code, stdout, stderr := runCmd(t, "backup", "list")
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	want := []string{"backup-20260903T000000Z.tar.gz", "backup-20260902T000000Z.tar.gz", "backup-20260901T000000Z.tar.gz"}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != len(want) {
		t.Fatalf("stdout = %q, want %d rows", stdout, len(want))
	}
	for i, name := range want {
		if !strings.HasPrefix(lines[i], name) {
			t.Errorf("row %d = %q, want it to start with %s", i, lines[i], name)
		}
		if !strings.Contains(lines[i], "v0.6.0") {
			t.Errorf("row %d = %q, want the recorded version", i, lines[i])
		}
	}
}

func TestBackupListCmd_UsesReadManifestAndSkipsForeignFiles(t *testing.T) {
	h := newHarness(t)
	dir := h.env.backupDir
	writeArchiveFile(t, dir, "backup-20260901T000000Z.tar.gz")
	writeArchiveFile(t, dir, "backup-20260902T000000Z.tar.gz")
	writeArchiveFile(t, dir, "operator-notes.tar.gz")
	if err := os.Mkdir(filepath.Join(dir, "backup-20260905T000000Z.tar.gz.d"), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	var asked []string
	h.env.readManifest = func(ctx context.Context, path string) (backup.Manifest, error) {
		asked = append(asked, filepath.Base(path))
		if strings.Contains(path, "20260902") {
			return backup.Manifest{}, errors.New("manifest unreadable")
		}
		return backup.Manifest{CreatedAt: testClock, ProxyctlVersion: "v0.7.0"}, nil
	}
	h.install(t)

	code, stdout, stderr := runCmd(t, "backup", "list")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if len(asked) != 2 {
		t.Errorf("ReadManifest was asked for %v, want only the two archive-named files", asked)
	}
	if strings.Contains(stdout, "operator-notes") {
		t.Errorf("stdout = %q, want foreign files skipped", stdout)
	}
	if !strings.Contains(stdout, "backup-20260901T000000Z.tar.gz") {
		t.Errorf("stdout = %q, want the readable archive listed", stdout)
	}
	if strings.Contains(stdout, "backup-20260902T000000Z.tar.gz") {
		t.Errorf("stdout = %q, want the unreadable archive left out of the listing", stdout)
	}
	if !strings.Contains(stderr, "backup-20260902T000000Z.tar.gz") {
		t.Errorf("stderr = %q, want a warning about the unreadable archive", stderr)
	}
}

// --- restore --------------------------------------------------------------

func TestRestoreCmd_RestartRecheckSuccess(t *testing.T) {
	h := newHarness(t)
	h.restoreResult.Restored = []string{"etc/caddy/Caddyfile", "etc/sing-box/config.json"}
	h.install(t)

	code, stdout, stderr := runCmd(t, "restore", "/tmp/archive.tar.gz")
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	want := []string{"sing-box", "caddy"}
	if len(h.restarted) != 2 || h.restarted[0] != want[0] || h.restarted[1] != want[1] {
		t.Errorf("restarted = %v, want %v", h.restarted, want)
	}
	if h.recheckCalls != 1 {
		t.Errorf("recheck ran %d times, want 1", h.recheckCalls)
	}
	if h.rollbackCalls != 0 {
		t.Errorf("rollback ran %d times on a successful restore", h.rollbackCalls)
	}
	if h.cleanupCalls != 1 {
		t.Errorf("the pre-restore image was cleaned %d times, want 1", h.cleanupCalls)
	}
	if !strings.Contains(stdout, "restore complete") {
		t.Errorf("stdout = %q", stdout)
	}
	if h.lock.tryCalls != 1 || h.lock.unlockCalls != 1 {
		t.Errorf("lock taken %d times, released %d", h.lock.tryCalls, h.lock.unlockCalls)
	}

	opts := h.restoreOpts[0]
	if opts.DryRun || !opts.CapturePreimage || opts.Lease == nil {
		t.Errorf("restore options = %+v, want a leased, image-capturing restore", opts)
	}
	if opts.DestRoot != h.env.destRoot || opts.SnapshotRoot != h.env.snapshotRoot {
		t.Errorf("restore roots = %q/%q", opts.DestRoot, opts.SnapshotRoot)
	}
}

func TestRestoreCmd_RecheckFailureRollsBack(t *testing.T) {
	h := newHarness(t)
	h.recheckStatus = []health.Status{health.StatusFail, health.StatusPass}
	h.install(t)

	code, stdout, stderr := runCmd(t, "restore", "/tmp/archive.tar.gz")
	if code != exitRolledBack {
		t.Fatalf("exit = %d, want %d; stderr = %s", code, exitRolledBack, stderr)
	}
	if h.rollbackCalls != 1 {
		t.Fatalf("rollback ran %d times, want 1", h.rollbackCalls)
	}
	if h.rollbackOpts[0].Lease == nil {
		t.Error("rollback was called without the transaction's lease")
	}
	if h.rollbackOpts[0].DestRoot != h.env.destRoot {
		t.Errorf("rollback DestRoot = %q, want %q", h.rollbackOpts[0].DestRoot, h.env.destRoot)
	}
	if h.recheckCalls != 2 {
		t.Errorf("recheck ran %d times, want a second one after the rollback", h.recheckCalls)
	}
	if h.cleanupCalls != 1 {
		t.Errorf("the image was cleaned %d times, want 1 after a successful rollback", h.cleanupCalls)
	}
	if !strings.Contains(stderr, "rolling back") {
		t.Errorf("stderr = %q, want the rollback announced", stderr)
	}
	_ = stdout
	if h.lock.unlockCalls != 1 {
		t.Errorf("lock released %d times", h.lock.unlockCalls)
	}
}

func TestRestoreCmd_RestartFailureRollsBack(t *testing.T) {
	h := newHarness(t)
	h.restartErr = errors.New("unit failed")
	h.install(t)

	code, _, stderr := runCmd(t, "restore", "/tmp/archive.tar.gz")
	if code != exitRolledBack {
		t.Fatalf("exit = %d, want %d", code, exitRolledBack)
	}
	if h.rollbackCalls != 1 {
		t.Errorf("rollback ran %d times, want 1", h.rollbackCalls)
	}
	if !strings.Contains(stderr, "unit failed") {
		t.Errorf("stderr = %q, want the restart failure reported", stderr)
	}
}

func TestRestoreCmd_RollbackFailureReported(t *testing.T) {
	h := newHarness(t)
	h.recheckStatus = []health.Status{health.StatusFail}
	h.rollbackErr = errors.New("write failed")
	h.rollbackResult = backup.RollbackResult{
		Restored:       []string{"etc/caddy/Caddyfile"},
		Failed:         []string{"etc/sing-box/config.json"},
		FailureReasons: map[string]string{"etc/sing-box/config.json": backup.FailWrite},
	}
	h.install(t)

	code, _, stderr := runCmd(t, "restore", "/tmp/archive.tar.gz")
	if code != exitRollbackFailed {
		t.Fatalf("exit = %d, want %d", code, exitRollbackFailed)
	}
	if !strings.Contains(stderr, "etc/sing-box/config.json") || !strings.Contains(stderr, backup.FailWrite) {
		t.Errorf("stderr = %q, want the uncertain path and its reason", stderr)
	}
	if !strings.Contains(stderr, h.preimage.Dir) {
		t.Errorf("stderr = %q, want the retained image path", stderr)
	}
	if h.cleanupCalls != 0 {
		t.Errorf("the image was cleaned %d times after a failed rollback, want 0", h.cleanupCalls)
	}
	if h.lock.unlockCalls != 1 {
		t.Errorf("lock released %d times", h.lock.unlockCalls)
	}
}

func TestRestoreCmd_RollbackFailureRetainsSnapshot(t *testing.T) {
	h := newHarness(t)
	h.recheckStatus = []health.Status{health.StatusFail}
	h.rollbackErr = errors.New("write failed")
	h.install(t)

	if code, _, _ := runCmd(t, "restore", "/tmp/archive.tar.gz"); code != exitRollbackFailed {
		t.Fatalf("exit = %d, want %d", code, exitRollbackFailed)
	}
	if h.cleanupCalls != 0 {
		t.Errorf("CleanupSnapshot ran %d times, want the image kept for manual recovery", h.cleanupCalls)
	}
}

func TestRestoreCmd_NoRestartSkipsServiceControl(t *testing.T) {
	h := newHarness(t)
	h.install(t)

	code, stdout, stderr := runCmd(t, "restore", "/tmp/archive.tar.gz", "--no-restart")
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if len(h.restarted) != 0 || h.recheckCalls != 0 {
		t.Errorf("restarted = %v, rechecks = %d, want neither", h.restarted, h.recheckCalls)
	}
	if !strings.Contains(stdout, "not restarting") {
		t.Errorf("stdout = %q, want the skipped restart spelled out", stdout)
	}
}

func TestRestoreCmd_NoRestartCleansPreimage(t *testing.T) {
	h := newHarness(t)
	h.install(t)

	if code, _, _ := runCmd(t, "restore", "/tmp/archive.tar.gz", "--no-restart"); code != exitOK {
		t.Fatal("expected success")
	}
	if h.cleanupCalls != 1 {
		t.Errorf("CleanupSnapshot ran %d times, want 1", h.cleanupCalls)
	}

	h2 := newHarness(t)
	h2.cleanupErr = errors.New("busy")
	h2.install(t)
	code, _, stderr := runCmd(t, "restore", "/tmp/archive.tar.gz", "--no-restart")
	if code != exitOK {
		t.Errorf("exit = %d, want cleanup failure to stay a warning", code)
	}
	if !strings.Contains(stderr, "warning") || !strings.Contains(stderr, h2.preimage.Dir) {
		t.Errorf("stderr = %q, want a warning naming the retained image", stderr)
	}
}

func TestRestoreCmd_PreimageCleanupLifecycle(t *testing.T) {
	cases := []struct {
		name         string
		args         []string
		prepare      func(h *harness)
		wantCleanups int
		wantCode     int
	}{
		{"success cleans", []string{"restore", "/a.tar.gz"}, nil, 1, exitOK},
		{"no-restart cleans", []string{"restore", "/a.tar.gz", "--no-restart"}, nil, 1, exitOK},
		{"rollback cleans after it succeeds", []string{"restore", "/a.tar.gz"}, func(h *harness) {
			h.recheckStatus = []health.Status{health.StatusFail, health.StatusPass}
		}, 1, exitRolledBack},
		{"failed rollback keeps", []string{"restore", "/a.tar.gz"}, func(h *harness) {
			h.recheckStatus = []health.Status{health.StatusFail}
			h.rollbackErr = errors.New("boom")
		}, 0, exitRollbackFailed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if tc.prepare != nil {
				tc.prepare(h)
			}
			h.install(t)
			code, _, _ := runCmd(t, tc.args...)
			if code != tc.wantCode {
				t.Errorf("exit = %d, want %d", code, tc.wantCode)
			}
			if h.cleanupCalls != tc.wantCleanups {
				t.Errorf("CleanupSnapshot ran %d times, want %d", h.cleanupCalls, tc.wantCleanups)
			}
		})
	}
}

func TestRestoreCmd_DryRunSkipsLockAndPreimage(t *testing.T) {
	h := newHarness(t)
	h.restoreResult = backup.Result{
		Preview: []backup.FileDiff{
			{Path: "etc/caddy/Caddyfile", Action: backup.ActionOverwrite},
			{Path: "var/lib/singbox-sub-manager/token", Action: backup.ActionCreate},
		},
	}
	h.install(t)

	code, stdout, stderr := runCmd(t, "restore", "/tmp/archive.tar.gz", "--dry-run")
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if h.lock.tryCalls != 0 || h.lock.unlockCalls != 0 {
		t.Errorf("dry-run touched the lock: %d TryLock, %d Unlock", h.lock.tryCalls, h.lock.unlockCalls)
	}
	opts := h.restoreOpts[0]
	if !opts.DryRun || opts.CapturePreimage || opts.Lease != nil || opts.SnapshotRoot != "" {
		t.Errorf("dry-run options = %+v, want no image, no lease and no snapshot root", opts)
	}
	if len(h.restarted) != 0 || h.rollbackCalls != 0 || h.cleanupCalls != 0 {
		t.Error("dry-run touched services or images")
	}
	for _, want := range []string{backup.ActionOverwrite, "etc/caddy/Caddyfile", "nothing was written"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to mention %q", stdout, want)
		}
	}
}

func TestRestoreCmd_LockBusyReturnsActionableError(t *testing.T) {
	h := newHarness(t)
	h.lock.tryErr = errors.New("held")
	h.install(t)

	code, _, stderr := runCmd(t, "restore", "/tmp/archive.tar.gz")
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "monitor pause") {
		t.Errorf("stderr = %q, want an actionable hint", stderr)
	}
	if len(h.restoreOpts) != 0 {
		t.Error("the restore ran without the lock")
	}
	if h.lock.unlockCalls != 0 {
		t.Errorf("Unlock ran %d times without holding the lock", h.lock.unlockCalls)
	}
}

func TestRestoreCmd_ArchiveRejectionExitCode(t *testing.T) {
	cases := []struct {
		name         string
		err          error
		want         int
		wantRollback int
	}{
		{"unsafe path", fmt.Errorf("wrapped: %w", backup.ErrUnsafePath), exitArchiveInvalid, 0},
		{"checksum", fmt.Errorf("wrapped: %w", backup.ErrChecksumMismatch), exitArchiveInvalid, 0},
		{"content mismatch", fmt.Errorf("wrapped: %w", backup.ErrArchiveContentMismatch), exitArchiveInvalid, 0},
		// Regression for the on-host finding: a corrupted gzip/tar container
		// or manifest must classify the same as the other archive rejections
		// above, not fall through to the generic I/O exit code below.
		{"archive format", fmt.Errorf("wrapped: %w", backup.ErrArchiveFormat), exitArchiveInvalid, 0},
		{"schema", fmt.Errorf("wrapped: %w", backup.ErrUnsupportedSchema), exitArchiveInvalid, 0},
		// An I/O failure can leave half a restore behind, so that one is
		// undone with the image.
		{"io", errors.New("disk on fire"), exitFailure, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.restoreErr = tc.err
			h.restoreResult = backup.Result{FailureReasons: map[string]string{}}
			h.install(t)

			code, _, stderr := runCmd(t, "restore", "/tmp/archive.tar.gz")
			if code != tc.want {
				t.Errorf("exit = %d, want %d (stderr %q)", code, tc.want, stderr)
			}
			if h.rollbackCalls != tc.wantRollback {
				t.Errorf("rollback ran %d times, want %d", h.rollbackCalls, tc.wantRollback)
			}
			if tc.want == exitArchiveInvalid && len(h.restarted) != 0 {
				t.Errorf("a rejected archive restarted %v", h.restarted)
			}
			if h.lock.tryCalls != h.lock.unlockCalls {
				t.Errorf("lock taken %d times, released %d", h.lock.tryCalls, h.lock.unlockCalls)
			}
		})
	}
}

// TestRestoreCmd_CorruptGzipArchiveExitsArchiveInvalid is the end-to-end
// regression for the on-host finding: it runs the real internal/backup.Restore
// against an actual corrupted archive file, rather than a mocked error, so a
// classification gap in that package (not just in reportRestoreError's
// switch) would still be caught here.
func TestRestoreCmd_CorruptGzipArchiveExitsArchiveInvalid(t *testing.T) {
	h := newHarness(t)
	h.env.restore = backup.Restore
	h.install(t)

	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	if err := os.WriteFile(archive, []byte("not a valid gzip archive"), 0600); err != nil {
		t.Fatalf("write corrupt archive: %v", err)
	}

	code, stdout, stderr := runCmd(t, "restore", archive)
	if code != exitArchiveInvalid {
		t.Fatalf("exit = %d, want %d (stdout %q, stderr %q)", code, exitArchiveInvalid, stdout, stderr)
	}
	entries, err := os.ReadDir(h.env.destRoot)
	if err != nil {
		t.Fatalf("read destRoot: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("destRoot has %v, want a rejected archive to write nothing", entries)
	}
	// newHarness's snapshotRoot is itself a t.TempDir(), which testing
	// creates up front, so its absence is not the signal; an untouched
	// snapshot root has nothing inside it.
	snapEntries, err := os.ReadDir(h.env.snapshotRoot)
	if err != nil {
		t.Fatalf("read snapshotRoot: %v", err)
	}
	if len(snapEntries) != 0 {
		t.Errorf("snapshotRoot has %v, want a rejected archive to capture no pre-restore image", snapEntries)
	}
	if len(h.restarted) != 0 {
		t.Errorf("a rejected archive restarted %v", h.restarted)
	}
}

// TestRestoreCmd_CorruptGzipArchiveDryRunExitsArchiveInvalid covers the same
// real corrupted archive through --dry-run, which shares readArchive with the
// real restore but takes a different command path to the same exit code.
func TestRestoreCmd_CorruptGzipArchiveDryRunExitsArchiveInvalid(t *testing.T) {
	h := newHarness(t)
	h.env.restore = backup.Restore
	h.install(t)

	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	if err := os.WriteFile(archive, []byte("not a valid gzip archive"), 0600); err != nil {
		t.Fatalf("write corrupt archive: %v", err)
	}

	code, stdout, stderr := runCmd(t, "restore", "--dry-run", archive)
	if code != exitArchiveInvalid {
		t.Fatalf("exit = %d, want %d (stdout %q, stderr %q)", code, exitArchiveInvalid, stdout, stderr)
	}
	if h.lock.tryCalls != 0 {
		t.Errorf("a rejected dry-run took the recovery lock %d times", h.lock.tryCalls)
	}
	entries, err := os.ReadDir(h.env.destRoot)
	if err != nil {
		t.Fatalf("read destRoot: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("destRoot has %v, want a rejected dry-run to write nothing", entries)
	}
}

// TestRestoreCmd_CorruptDeflateBodyExitsArchiveInvalid is the regression for
// the review finding on a corrupted gzip header (7606138): a valid gzip
// header wrapping corrupt DEFLATE data fails later, inside decompression
// (flate.CorruptInputError), not at gzip.NewReader, and must classify the
// same way.
func TestRestoreCmd_CorruptDeflateBodyExitsArchiveInvalid(t *testing.T) {
	h := newHarness(t)
	h.env.restore = backup.Restore
	h.install(t)

	// A minimal valid 10-byte gzip header (no name/comment/extra flags)
	// followed by one byte that is not a legal DEFLATE block type.
	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	body := []byte{0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xff, 0xff}
	if err := os.WriteFile(archive, body, 0600); err != nil {
		t.Fatalf("write corrupt archive: %v", err)
	}

	code, stdout, stderr := runCmd(t, "restore", archive)
	if code != exitArchiveInvalid {
		t.Fatalf("exit = %d, want %d (stdout %q, stderr %q)", code, exitArchiveInvalid, stdout, stderr)
	}
	entries, err := os.ReadDir(h.env.destRoot)
	if err != nil {
		t.Fatalf("read destRoot: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("destRoot has %v, want a rejected archive to write nothing", entries)
	}
}

func TestRestoreCmd_PartialRestoreRollsBackAndReportsPaths(t *testing.T) {
	h := newHarness(t)
	h.restoreErr = errors.New("publish failed")
	h.restoreResult = backup.Result{
		Restored:       []string{"etc/caddy/Caddyfile"},
		Failed:         []string{"etc/sing-box/config.json"},
		FailureReasons: map[string]string{"etc/sing-box/config.json": backup.FailPublish},
	}
	h.install(t)

	code, _, stderr := runCmd(t, "restore", "/tmp/archive.tar.gz")
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if h.rollbackCalls != 1 {
		t.Errorf("rollback ran %d times, want the partial restore undone", h.rollbackCalls)
	}
	if !strings.Contains(stderr, "etc/sing-box/config.json") || !strings.Contains(stderr, backup.FailPublish) {
		t.Errorf("stderr = %q, want the failed path and its stable reason", stderr)
	}
	if len(h.restarted) != 0 {
		t.Errorf("services were restarted after a failed restore: %v", h.restarted)
	}
}

func TestRestoreCmd_ReportsMonitorStateEffect(t *testing.T) {
	paused := newHarness(t)
	paused.restoreResult.Restored = []string{"var/lib/singbox-sub-manager/monitor-paused", "var/lib/singbox-sub-manager/monitor-state.json"}
	paused.install(t)
	if code, stdout, _ := runCmd(t, "restore", "/a.tar.gz"); code != exitOK || !strings.Contains(stdout, "monitor resume") {
		t.Errorf("exit = %d, stdout = %q, want the pause effect and the resume hint", code, stdout)
	}

	state := newHarness(t)
	state.restoreResult.Restored = []string{"var/lib/singbox-sub-manager/monitor-state.json"}
	state.install(t)
	if code, stdout, _ := runCmd(t, "restore", "/a.tar.gz"); code != exitOK || !strings.Contains(stdout, "never starts the monitor") {
		t.Errorf("exit = %d, stdout = %q, want the state-restore effect", code, stdout)
	}
}

func TestRestoreCmd_RejectsBadArguments(t *testing.T) {
	h := newHarness(t)
	h.install(t)
	for _, args := range [][]string{
		{"restore"},
		{"restore", "a.tar.gz", "b.tar.gz"},
		{"restore", "--nope", "a.tar.gz"},
	} {
		if code, _, _ := runCmd(t, args...); code != exitUsage {
			t.Errorf("%v: exit = %d, want %d", args, code, exitUsage)
		}
	}
}

// --- lock coverage over the whole transaction -----------------------------

func TestRestoreCmd_LockHeldThroughRecheckAndRollback(t *testing.T) {
	h := newHarness(t)
	h.recheckStatus = []health.Status{health.StatusFail, health.StatusPass}

	var heldDuringRecheck, heldDuringRollback bool
	h.onRecheck = func() { heldDuringRecheck = h.lock.held }
	inner := h.env.rollback
	h.env.rollback = func(ctx context.Context, snap *backup.RollbackSnapshot, opts backup.RollbackOptions) (backup.RollbackResult, error) {
		heldDuringRollback = h.lock.held
		return inner(ctx, snap, opts)
	}
	h.install(t)

	if code, _, _ := runCmd(t, "restore", "/a.tar.gz"); code != exitRolledBack {
		t.Fatalf("exit = %d", code)
	}
	if !heldDuringRecheck || !heldDuringRollback {
		t.Errorf("lock held during recheck = %v, during rollback = %v; want both true", heldDuringRecheck, heldDuringRollback)
	}
	if h.lock.tryCalls != 1 {
		t.Errorf("the lock was acquired %d times, want exactly one for the whole transaction", h.lock.tryCalls)
	}
	if h.lock.held {
		t.Error("the lock is still held after the command returned")
	}
}

func TestRestoreCmd_MonitorBlockedUntilTransactionEnd(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "recovery.lock")
	h := newHarness(t)
	h.env.newLock = func() backup.Locker { return monitor.NewFileLock(lockPath) }

	var competitorErr error
	h.onRecheck = func() {
		competitor := monitor.NewFileLock(lockPath)
		competitorErr = competitor.TryLock()
		if competitorErr == nil {
			_ = competitor.Unlock()
		}
	}
	h.install(t)

	if code, _, stderr := runCmd(t, "restore", "/a.tar.gz"); code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if competitorErr == nil {
		t.Error("a competing monitor acquired the recovery lock while the restore was rechecking")
	}

	after := monitor.NewFileLock(lockPath)
	if err := after.TryLock(); err != nil {
		t.Errorf("the lock is still held after the transaction: %v", err)
	} else {
		_ = after.Unlock()
	}
}

// --- end to end against the real internal/backup --------------------------

// liveEnv wires the command to the real package against temporary roots.
func liveEnv(t *testing.T, destRoot string) *harness {
	t.Helper()
	h := newHarness(t)
	h.env.destRoot = destRoot
	h.env.restore = backup.Restore
	h.env.rollback = backup.Rollback
	h.env.cleanup = backup.CleanupSnapshot
	h.env.create = backup.Create
	return h
}

func makeSourceTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := []struct {
		rel  string
		body string
		mode fs.FileMode
	}{
		{"etc/caddy/Caddyfile", "archived caddy\n", 0640},
		{"etc/sing-box/config.json", `{"log":{"level":"info"}}`, 0600},
		{"var/lib/singbox-sub-manager/token", "archived token\n", 0600},
	}
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f.rel))
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(f.body), f.mode); err != nil {
			t.Fatalf("write %s: %v", f.rel, err)
		}
		if err := os.Chmod(p, f.mode); err != nil {
			t.Fatalf("chmod %s: %v", f.rel, err)
		}
	}
	return root
}

func makeArchive(t *testing.T, sourceRoot string) string {
	t.Helper()
	m, err := backup.Create(context.Background(), backup.CreateOptions{
		SourceRoot:      sourceRoot,
		BackupDir:       t.TempDir(),
		ProxyctlVersion: "v0.7.0",
		Now:             func() time.Time { return testClock },
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return m.ArchivePath
}

func TestRestoreCmd_RollbackRestoresBytesModesAndDeletesCreated(t *testing.T) {
	source := makeSourceTree(t)
	archive := makeArchive(t, source)

	dest := t.TempDir()
	// The destination differs from the archive: one file has other content
	// and mode, and the token does not exist at all.
	caddyfile := filepath.Join(dest, "etc/caddy/Caddyfile")
	if err := os.MkdirAll(filepath.Dir(caddyfile), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(caddyfile, []byte("live caddy\n"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(caddyfile, 0600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	wantUID, wantGID := uint32(os.Geteuid()), uint32(os.Getegid())
	if os.Geteuid() == 0 {
		wantUID, wantGID = 5551, 5552
		if err := os.Chown(caddyfile, int(wantUID), int(wantGID)); err != nil {
			t.Fatalf("chown: %v", err)
		}
	}

	h := liveEnv(t, dest)
	h.recheckStatus = []health.Status{health.StatusFail, health.StatusPass}
	h.install(t)

	code, _, stderr := runCmd(t, "restore", archive)
	if code != exitRolledBack {
		t.Fatalf("exit = %d, want %d; stderr = %s", code, exitRolledBack, stderr)
	}

	data, err := os.ReadFile(caddyfile)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "live caddy\n" {
		t.Errorf("Caddyfile = %q, want the pre-restore bytes back", data)
	}
	info, err := os.Stat(caddyfile)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("Caddyfile mode = %o, want the pre-restore 0600", info.Mode().Perm())
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("no ownership information")
	}
	if st.Uid != wantUID || st.Gid != wantGID {
		t.Errorf("Caddyfile ownership = %d:%d, want %d:%d", st.Uid, st.Gid, wantUID, wantGID)
	}
	for _, rel := range []string{"etc/sing-box/config.json", "var/lib/singbox-sub-manager/token"} {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("%s was created by the restore and survived the rollback: %v", rel, err)
		}
	}
	if entries, _ := os.ReadDir(h.env.snapshotRoot); len(entries) != 0 {
		t.Errorf("snapshot root holds %d images after a successful rollback, want none", len(entries))
	}
}

func TestRestoreCmd_LiveSuccessWritesArchiveContent(t *testing.T) {
	source := makeSourceTree(t)
	archive := makeArchive(t, source)
	dest := t.TempDir()

	h := liveEnv(t, dest)
	h.install(t)

	code, stdout, stderr := runCmd(t, "restore", archive)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	for _, rel := range []string{"etc/caddy/Caddyfile", "etc/sing-box/config.json", "var/lib/singbox-sub-manager/token"} {
		want, err := os.ReadFile(filepath.Join(source, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read source: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read restored %s: %v", rel, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
	if !strings.Contains(stdout, "health recheck passed") {
		t.Errorf("stdout = %q", stdout)
	}
	if entries, _ := os.ReadDir(h.env.snapshotRoot); len(entries) != 0 {
		t.Errorf("snapshot root holds %d images after success, want none", len(entries))
	}
}

func TestRestoreCmd_LiveDryRunLeavesDestinationUntouched(t *testing.T) {
	source := makeSourceTree(t)
	archive := makeArchive(t, source)
	dest := t.TempDir()

	h := liveEnv(t, dest)
	h.install(t)

	code, stdout, stderr := runCmd(t, "restore", archive, "--dry-run")
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if entries, _ := os.ReadDir(dest); len(entries) != 0 {
		t.Errorf("dry-run created %d entries in the destination", len(entries))
	}
	if _, err := os.Stat(h.env.snapshotRoot); err == nil {
		if entries, _ := os.ReadDir(h.env.snapshotRoot); len(entries) != 0 {
			t.Errorf("dry-run created %d images", len(entries))
		}
	}
	if !strings.Contains(stdout, backup.ActionCreate) {
		t.Errorf("stdout = %q, want create actions", stdout)
	}
}

// --- monitor pause reconciliation -----------------------------------------

func writePauseMarker(t *testing.T, destRoot string) string {
	t.Helper()
	p := filepath.Join(destRoot, filepath.FromSlash(monitorPausePath))
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, nil, 0600); err != nil {
		t.Fatalf("write pause marker: %v", err)
	}
	return p
}

func TestRestoreCmd_ClearsPauseMarkerMissingFromArchive(t *testing.T) {
	h := newHarness(t)
	marker := writePauseMarker(t, h.env.destRoot)
	// The archive was taken while the monitor was running, so the marker is
	// not among the restored paths.
	h.restoreResult.Restored = []string{"etc/caddy/Caddyfile"}
	h.install(t)

	code, stdout, stderr := runCmd(t, "restore", "/a.tar.gz")
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("the pause marker survived a restore of an archive without one: %v", err)
	}
	if !strings.Contains(stdout, "pause marker was removed") {
		t.Errorf("stdout = %q, want the cleared pause marker reported", stdout)
	}
}

func TestRestoreCmd_KeepsPauseMarkerCarriedByArchive(t *testing.T) {
	h := newHarness(t)
	marker := writePauseMarker(t, h.env.destRoot)
	h.restoreResult.Restored = []string{"etc/caddy/Caddyfile", monitorPausePath}
	h.install(t)

	code, stdout, _ := runCmd(t, "restore", "/a.tar.gz")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("a pause marker the archive carries was removed: %v", err)
	}
	if !strings.Contains(stdout, "monitor resume") {
		t.Errorf("stdout = %q, want the still-paused state reported", stdout)
	}
}

func TestRestoreCmd_ClearsPauseMarkerWithNoRestart(t *testing.T) {
	h := newHarness(t)
	marker := writePauseMarker(t, h.env.destRoot)
	h.install(t)

	if code, _, stderr := runCmd(t, "restore", "/a.tar.gz", "--no-restart"); code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("--no-restart left a stale pause marker: %v", err)
	}
}

func TestRestoreCmd_PauseMarkerRemovalFailureIsReported(t *testing.T) {
	h := newHarness(t)
	// A directory in the marker's place cannot be removed with os.Remove
	// once it has content, so the reconciliation fails deterministically.
	markerDir := filepath.Join(h.env.destRoot, filepath.FromSlash(monitorPausePath))
	if err := os.MkdirAll(markerDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(markerDir, "blocker"), nil, 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	h.install(t)

	code, _, stderr := runCmd(t, "restore", "/a.tar.gz")
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "monitor resume") {
		t.Errorf("stderr = %q, want an actionable hint", stderr)
	}
	if h.cleanupCalls != 1 {
		t.Errorf("the image was cleaned %d times, want the successful restore's image removed", h.cleanupCalls)
	}
}

func TestRestoreCmd_DryRunShowsPauseMarkerRemoval(t *testing.T) {
	h := newHarness(t)
	writePauseMarker(t, h.env.destRoot)
	h.restoreResult = backup.Result{Preview: []backup.FileDiff{{Path: "etc/caddy/Caddyfile", Action: backup.ActionOverwrite}}}
	h.install(t)

	code, stdout, _ := runCmd(t, "restore", "/a.tar.gz", "--dry-run")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "remove") || !strings.Contains(stdout, monitorPausePath) {
		t.Errorf("stdout = %q, want the pause marker removal previewed", stdout)
	}
	if _, err := os.Stat(filepath.Join(h.env.destRoot, filepath.FromSlash(monitorPausePath))); err != nil {
		t.Errorf("dry-run removed the marker for real: %v", err)
	}
}

func TestRestoreCmd_ReportsMonitorEffectOnFailurePaths(t *testing.T) {
	h := newHarness(t)
	h.restoreErr = errors.New("publish failed")
	h.restoreResult = backup.Result{
		Restored:       []string{monitorStatePath},
		Failed:         []string{"etc/caddy/Caddyfile"},
		FailureReasons: map[string]string{"etc/caddy/Caddyfile": backup.FailPublish},
	}
	h.install(t)

	code, stdout, _ := runCmd(t, "restore", "/a.tar.gz")
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stdout, "monitor state was restored") {
		t.Errorf("stdout = %q, want the monitor effect reported on the failure path too", stdout)
	}
}

func TestRestoreCmd_RollbackReportsMonitorStateRestored(t *testing.T) {
	h := newHarness(t)
	h.restoreResult.Restored = []string{"etc/caddy/Caddyfile", monitorStatePath}
	h.recheckStatus = []health.Status{health.StatusFail, health.StatusPass}
	h.install(t)

	code, stdout, _ := runCmd(t, "restore", "/a.tar.gz")
	if code != exitRolledBack {
		t.Fatalf("exit = %d, want %d", code, exitRolledBack)
	}
	if !strings.Contains(stdout, "put back the way it was") {
		t.Errorf("stdout = %q, want the rollback's effect on monitor state stated", stdout)
	}
}

// --- restart and recheck timeouts -----------------------------------------

func TestRestoreCmd_RestartTimeoutRollsBack(t *testing.T) {
	h := newHarness(t)
	h.env.restartTimeout = 20 * time.Millisecond
	h.env.restart = func(ctx context.Context, svc string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	h.install(t)

	code, _, stderr := runCmd(t, "restore", "/a.tar.gz")
	if code != exitRolledBack {
		t.Fatalf("exit = %d, want %d", code, exitRolledBack)
	}
	if !strings.Contains(stderr, "timed out") {
		t.Errorf("stderr = %q, want the timeout named", stderr)
	}
	if h.rollbackCalls != 1 {
		t.Errorf("rollback ran %d times after a restart timeout, want 1", h.rollbackCalls)
	}
}

func TestRestoreCmd_RecheckTimeoutRollsBack(t *testing.T) {
	h := newHarness(t)
	h.env.recheckTimeout = 20 * time.Millisecond
	h.env.recheck = func(ctx context.Context, ids ...string) []health.Result {
		<-ctx.Done()
		return nil
	}
	h.install(t)

	code, _, stderr := runCmd(t, "restore", "/a.tar.gz")
	if code != exitRolledBack {
		t.Fatalf("exit = %d, want %d", code, exitRolledBack)
	}
	if !strings.Contains(stderr, "timed out") {
		t.Errorf("stderr = %q, want the recheck timeout named", stderr)
	}
	if h.rollbackCalls != 1 {
		t.Errorf("rollback ran %d times after a recheck timeout, want 1", h.rollbackCalls)
	}
}

func TestRestoreCmd_MissingRecheckResultRollsBack(t *testing.T) {
	h := newHarness(t)
	// A recheck that answers about nothing must not be read as success.
	h.env.recheck = func(ctx context.Context, ids ...string) []health.Result { return nil }
	h.install(t)

	code, _, stderr := runCmd(t, "restore", "/a.tar.gz")
	if code != exitRolledBack {
		t.Fatalf("exit = %d, want %d", code, exitRolledBack)
	}
	if !strings.Contains(stderr, "missing") {
		t.Errorf("stderr = %q, want the missing check reported", stderr)
	}
}

// --- recheck identity with the monitor ------------------------------------

func TestRestoreCmd_RechecksExactlyTheMonitorTriggers(t *testing.T) {
	h := newHarness(t)
	h.restoreResult.Restored = []string{"etc/caddy/Caddyfile", "etc/sing-box/config.json"}
	h.install(t)

	if code, _, stderr := runCmd(t, "restore", "/a.tar.gz"); code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	want := append(monitor.ServiceTriggers("sing-box"), monitor.ServiceTriggers("caddy")...)
	if len(h.recheckIDs) != 1 {
		t.Fatalf("recheck ran %d times, want 1", len(h.recheckIDs))
	}
	got := h.recheckIDs[0]
	if len(got) != len(want) {
		t.Fatalf("recheck ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("recheck ids = %v, want %v", got, want)
		}
	}
}

func TestArchiveOrderKeyMatchesTheArchiveNamingRule(t *testing.T) {
	// Ordering must keep working if the naming rule in internal/backup
	// changes: an unparsed name silently degrades to whole-string ordering.
	for _, name := range []string{
		"backup-20260909T100000Z.tar.gz",
		"backup-20260909T100000Z-1.tar.gz",
		"backup-20260909T100000Z-12.tar.gz",
	} {
		if !backup.IsArchiveName(name) {
			t.Fatalf("%s is not accepted by the archive naming rule any more", name)
		}
		if stamp, _ := archiveOrderKey(name); stamp != "20260909T100000Z" {
			t.Errorf("archiveOrderKey(%q) = %q, want the embedded timestamp", name, stamp)
		}
	}
	if !archiveOlder("backup-20260909T100000Z.tar.gz", "backup-20260909T100000Z-1.tar.gz") {
		t.Error("the unsuffixed archive must sort before its same-second successor")
	}
}

func TestBackupListCmd_OrdersSameSecondArchivesBySuffix(t *testing.T) {
	h := newHarness(t)
	dir := h.env.backupDir
	names := []string{
		"backup-20260909T100000Z.tar.gz",
		"backup-20260909T100000Z-1.tar.gz",
		"backup-20260909T100000Z-2.tar.gz",
	}
	for _, n := range names {
		writeArchiveFile(t, dir, n)
	}
	h.env.readManifest = func(ctx context.Context, path string) (backup.Manifest, error) {
		return backup.Manifest{CreatedAt: testClock, ProxyctlVersion: "v0.7.0"}, nil
	}
	h.install(t)

	code, stdout, _ := runCmd(t, "backup", "list")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	want := []string{names[2], names[1], names[0]}
	for i, name := range want {
		if !strings.HasPrefix(lines[i], name) {
			t.Errorf("row %d = %q, want %s", i, lines[i], name)
		}
	}
}

func TestBackupAndRestoreHelpSucceed(t *testing.T) {
	// install-proxy.sh probes these before an upgrade to find out whether the
	// installed binary can snapshot configuration at all, so they must exit 0
	// the way `monitor --help` does.
	h := newHarness(t)
	h.install(t)

	for _, args := range [][]string{
		{"backup", "--help"},
		{"backup", "-h"},
		{"restore", "--help"},
		{"restore", "-h"},
	} {
		code, stdout, stderr := runCmd(t, args...)
		if code != exitOK {
			t.Errorf("%v: exit = %d, want 0 (stderr %q)", args, code, stderr)
		}
		if !strings.Contains(stdout, "usage:") {
			t.Errorf("%v: stdout = %q, want usage text", args, stdout)
		}
	}
	if len(h.restoreOpts) != 0 || h.lock.tryCalls != 0 {
		t.Error("--help did any real work")
	}
}
