package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/hlpclg/singbox-sub-manager/internal/backup"
	"github.com/hlpclg/singbox-sub-manager/internal/health"
	"github.com/hlpclg/singbox-sub-manager/internal/monitor"
)

const (
	defaultBackupDir   = "/var/lib/singbox-sub-manager/backups"
	defaultSnapshotDir = "/var/lib/singbox-sub-manager/restore-snapshots"
	recoveryLockPath   = "/run/lock/singbox-sub-manager-monitor.lock"
	defaultKeep        = 10
)

// Exit codes for backup and restore. They are part of the command contract:
// operators and install-proxy.sh branch on them.
const (
	exitOK             = 0
	exitFailure        = 1 // I/O or service failure; the system is in a known state
	exitUsage          = 2 // bad arguments
	exitArchiveInvalid = 3 // verification or extraction safety rejected the archive
	exitRolledBack     = 4 // files were restored, the service check failed, config is back
	exitRollbackFailed = 5 // the rollback itself failed; needs a human
)

// serviceForPath maps an archived configuration file to the service that has
// to be restarted when it changes.
var serviceForPath = map[string]string{
	"etc/sing-box/config.json": "sing-box",
	"etc/caddy/Caddyfile":      "caddy",
}

const (
	monitorStatePath = "var/lib/singbox-sub-manager/monitor-state.json"
	monitorPausePath = "var/lib/singbox-sub-manager/monitor-paused"
)

// monitorStatePaths are the archived files whose restore changes what the
// monitor will do next; the command spells the effect out either way.
var monitorStatePaths = []string{monitorStatePath, monitorPausePath}

// backupEnv is everything the commands touch outside their own process. Tests
// replace it wholesale; production builds it from the installed layout.
type backupEnv struct {
	sourceRoot   string
	destRoot     string
	backupDir    string
	snapshotRoot string
	version      string
	now          func() time.Time

	create       func(context.Context, backup.CreateOptions) (backup.Manifest, error)
	readManifest func(context.Context, string) (backup.Manifest, error)
	restore      func(context.Context, string, backup.RestoreOptions) (backup.Result, error)
	rollback     func(context.Context, *backup.RollbackSnapshot, backup.RollbackOptions) (backup.RollbackResult, error)
	cleanup      func(context.Context, *backup.RollbackSnapshot) error
	newLock      func() backup.Locker
	restart      func(context.Context, string) error
	recheck      func(context.Context, ...string) []health.Result

	lockRetryInterval time.Duration
	lockMaxWait       time.Duration
	restartTimeout    time.Duration
	recheckTimeout    time.Duration
}

var newBackupEnv = productionBackupEnv

func productionBackupEnv() backupEnv {
	return backupEnv{
		sourceRoot:   "/",
		destRoot:     "/",
		backupDir:    defaultBackupDir,
		snapshotRoot: defaultSnapshotDir,
		version:      Version,
		now:          time.Now,

		create:       backup.Create,
		readManifest: backup.ReadManifest,
		restore:      backup.Restore,
		rollback:     backup.Rollback,
		cleanup:      backup.CleanupSnapshot,
		newLock:      func() backup.Locker { return monitor.NewFileLock(recoveryLockPath) },
		restart:      monitor.RestartService,
		recheck:      runServiceChecks,

		lockRetryInterval: backup.DefaultLockRetryInterval,
		lockMaxWait:       backup.DefaultLockMaxWait,
		restartTimeout:    30 * time.Second,
		recheckTimeout:    30 * time.Second,
	}
}

// runServiceChecks runs the same local health checks the monitor uses, limited
// to the given check IDs.
func runServiceChecks(ctx context.Context, ids ...string) []health.Result {
	cfg := healthResolveConfig("", nil)
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	var checks []health.Check
	for _, c := range healthAllChecks() {
		if len(want) == 0 || want[c.ID()] {
			checks = append(checks, c)
		}
	}
	return health.RunAll(ctx, cfg, checks, health.ConcurrentIDs())
}

func cmdBackup(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Fprintln(stdout, "usage: proxyctl backup [--out PATH] [--keep N]")
		fmt.Fprintln(stdout, "       proxyctl backup list")
		return exitOK
	}
	if len(args) > 0 && args[0] == "list" {
		return cmdBackupList(args[1:], stdout, stderr)
	}

	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "write the archive to this path instead of the backup directory")
	keep := fs.Int("keep", defaultKeep, "how many automatic archives to keep (0 disables pruning)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "error: unexpected arguments after flags")
		return exitUsage
	}
	if *keep < 0 {
		fmt.Fprintln(stderr, "error: --keep must not be negative")
		return exitUsage
	}

	env := newBackupEnv()
	m, err := env.create(context.Background(), backup.CreateOptions{
		SourceRoot:      env.sourceRoot,
		BackupDir:       env.backupDir,
		Out:             *out,
		ProxyctlVersion: env.version,
		Now:             env.now,
	})
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return exitFailure
	}

	fmt.Fprintf(stdout, "archive %s\n", m.ArchivePath)
	fmt.Fprintf(stdout, "packed %d files", len(m.Files))
	if len(m.Skipped) > 0 {
		fmt.Fprintf(stdout, ", skipped %d", len(m.Skipped))
	}
	fmt.Fprintln(stdout)
	for _, p := range m.Skipped {
		fmt.Fprintf(stdout, "  skipped %s (%s)\n", p, m.SkippedReasons[p])
	}
	fmt.Fprintln(stdout, "the archive contains private keys and tokens: keep it as securely as the server itself")

	if *out == "" && *keep > 0 {
		for _, w := range pruneArchives(env.backupDir, *keep) {
			fmt.Fprintln(stderr, "warning:", w)
		}
	}
	return exitOK
}

// pruneArchives keeps the newest keep archives this tool created and removes
// the rest. Anything that does not match the archive naming rule belongs to
// the operator and is never touched.
func pruneArchives(dir string, keep int) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{fmt.Sprintf("cannot list %s: %v", dir, err)}
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && backup.IsArchiveName(e.Name()) {
			names = append(names, e.Name())
		}
	}
	if len(names) <= keep {
		return nil
	}
	sort.Slice(names, func(i, j int) bool { return archiveOlder(names[i], names[j]) })

	var warnings []string
	for _, name := range names[:len(names)-keep] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			warnings = append(warnings, fmt.Sprintf("cannot prune %s: %v", name, err))
		}
	}
	return warnings
}

var archiveNameParts = regexp.MustCompile(`^backup-([0-9]{8}T[0-9]{6}Z)(?:-([1-9][0-9]*))?\.tar\.gz$`)

// archiveOlder orders archives by their embedded timestamp, then by the
// collision suffix, so same-second archives keep their creation order.
func archiveOlder(a, b string) bool {
	stampA, seqA := archiveOrderKey(a)
	stampB, seqB := archiveOrderKey(b)
	if stampA != stampB {
		return stampA < stampB
	}
	return seqA < seqB
}

func archiveOrderKey(name string) (string, int) {
	m := archiveNameParts.FindStringSubmatch(name)
	if m == nil {
		return name, 0
	}
	seq := 0
	if m[2] != "" {
		seq, _ = strconv.Atoi(m[2])
	}
	return m[1], seq
}

func cmdBackupList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "error: unexpected arguments after flags")
		return exitUsage
	}

	env := newBackupEnv()
	entries, err := os.ReadDir(env.backupDir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(stdout, "no archives")
			return exitOK
		}
		fmt.Fprintln(stderr, "error:", err)
		return exitFailure
	}

	type row struct {
		name    string
		created time.Time
		size    int64
		version string
	}
	var rows []row
	ctx := context.Background()
	for _, e := range entries {
		if e.IsDir() || !backup.IsArchiveName(e.Name()) {
			continue
		}
		path := filepath.Join(env.backupDir, e.Name())
		m, err := env.readManifest(ctx, path)
		if err != nil {
			fmt.Fprintf(stderr, "warning: skipping %s: %v\n", e.Name(), err)
			continue
		}
		var size int64
		if info, statErr := e.Info(); statErr == nil {
			size = info.Size()
		}
		rows = append(rows, row{name: e.Name(), created: m.CreatedAt, size: size, version: m.ProxyctlVersion})
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "no archives")
		return exitOK
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].created.Equal(rows[j].created) {
			return rows[i].created.After(rows[j].created)
		}
		// Same-second archives are ordered by their collision suffix, newest
		// first, so the listing never depends on directory order.
		return archiveOlder(rows[j].name, rows[i].name)
	})

	for _, r := range rows {
		version := r.version
		if version == "" {
			version = "unknown"
		}
		fmt.Fprintf(stdout, "%s  %s  %s  %s\n", r.name, r.created.UTC().Format(time.RFC3339), formatSize(r.size), version)
	}
	return exitOK
}

func formatSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func cmdRestore(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Fprintln(stdout, "usage: proxyctl restore <archive-path> [--dry-run] [--no-restart]")
		return exitOK
	}
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "show what would change without writing or locking")
	noRestart := fs.Bool("no-restart", false, "restore files without restarting services")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(stderr, "usage: proxyctl restore <archive-path> [--dry-run] [--no-restart]")
		return exitUsage
	}
	archivePath := rest[0]
	// flag stops at the first positional argument, but the documented syntax
	// puts the archive path first, so parse what follows it as well.
	if err := fs.Parse(rest[1:]); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: proxyctl restore <archive-path> [--dry-run] [--no-restart]")
		return exitUsage
	}

	env := newBackupEnv()
	ctx := context.Background()

	if *dryRun {
		return runRestoreDryRun(ctx, env, archivePath, stdout, stderr)
	}
	return runRestoreTransaction(ctx, env, archivePath, *noRestart, stdout, stderr)
}

func runRestoreDryRun(ctx context.Context, env backupEnv, archivePath string, stdout, stderr io.Writer) int {
	res, err := env.restore(ctx, archivePath, backup.RestoreOptions{
		DryRun:   true,
		DestRoot: env.destRoot,
	})
	if err != nil {
		return reportRestoreError(err, stderr)
	}
	previewed := make([]string, 0, len(res.Preview))
	for _, d := range res.Preview {
		fmt.Fprintf(stdout, "%-10s %s\n", d.Action, d.Path)
		previewed = append(previewed, d.Path)
	}
	if wouldClearPauseMarker(env, previewed) {
		fmt.Fprintf(stdout, "%-10s %s\n", "remove", monitorPausePath)
	}
	fmt.Fprintln(stdout, "dry run: nothing was written and no lock was taken")
	return exitOK
}

func runRestoreTransaction(ctx context.Context, env backupEnv, archivePath string, noRestart bool, stdout, stderr io.Writer) int {
	lease, err := backup.AcquireWithRetry(ctx, env.newLock(), env.lockRetryInterval, env.lockMaxWait)
	if err != nil {
		if errors.Is(err, backup.ErrLockBusy) {
			fmt.Fprintln(stderr, "error: the monitor is holding the recovery lock; retry shortly or run `proxyctl monitor pause` first")
			return exitFailure
		}
		fmt.Fprintln(stderr, "error:", err)
		return exitFailure
	}
	// One lease covers the whole transaction: restore, restart, recheck,
	// rollback and image cleanup. Nothing inside may take the lock again.
	defer func() {
		if unlockErr := lease.Unlock(); unlockErr != nil {
			fmt.Fprintln(stderr, "warning: releasing the recovery lock failed:", unlockErr)
		}
	}()

	res, err := env.restore(ctx, archivePath, backup.RestoreOptions{
		DestRoot:        env.destRoot,
		SnapshotRoot:    env.snapshotRoot,
		Lease:           lease,
		CapturePreimage: true,
		Now:             env.now,
	})
	for _, w := range res.Warnings {
		fmt.Fprintln(stderr, "warning:", w)
	}
	if err != nil {
		reportPaths(res, stdout, stderr)
		reportMonitorEffect(res.Restored, stdout)
		code := reportRestoreError(err, stderr)
		// A rejected archive is refused before anything is written: there is
		// no image and nothing to undo.
		if code == exitArchiveInvalid || res.Preimage == nil {
			return code
		}
		fmt.Fprintln(stderr, "rolling back to the state before this restore")
		if rollbackCode := rollbackTransaction(ctx, env, res.Preimage, lease, stdout, stderr); rollbackCode != exitOK {
			return rollbackCode
		}
		return code
	}

	reportPaths(res, stdout, stderr)
	reportMonitorEffect(res.Restored, stdout)

	services := affectedServices(res.Restored)
	if noRestart || len(services) == 0 {
		if noRestart && len(services) > 0 {
			fmt.Fprintf(stdout, "not restarting %v; restart them yourself for the restored configuration to take effect\n", services)
		}
		code := exitOK
		if err := reconcilePauseMarker(env, res.Restored, stdout); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			fmt.Fprintf(stderr, "the monitor is still paused; remove %s or run `proxyctl monitor resume` yourself\n", monitorPausePath)
			code = exitFailure
		}
		cleanupPreimage(ctx, env, res.Preimage, stderr)
		return code
	}

	if err := restartAndRecheck(ctx, env, services, stdout, stderr); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		fmt.Fprintln(stderr, "rolling back to the state before this restore")
		if code := rollbackTransaction(ctx, env, res.Preimage, lease, stdout, stderr); code != exitOK {
			return code
		}
		if touchesMonitorState(res.Restored) {
			fmt.Fprintln(stdout, "monitor state was put back the way it was before this restore")
		}
		if err := restartAndRecheck(ctx, env, services, stdout, stderr); err != nil {
			fmt.Fprintln(stderr, "warning: the services did not come back after the rollback either:", err)
		}
		return exitRolledBack
	}

	code := exitOK
	if err := reconcilePauseMarker(env, res.Restored, stdout); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		fmt.Fprintf(stderr, "the monitor is still paused; remove %s or run `proxyctl monitor resume` yourself\n", monitorPausePath)
		code = exitFailure
	}
	cleanupPreimage(ctx, env, res.Preimage, stderr)
	if code == exitOK {
		fmt.Fprintln(stdout, "restore complete")
	}
	return code
}

// reconcilePauseMarker applies the archive's pause state as an existence
// question, not just as a file to write: an archive taken while the monitor
// was running has no pause marker, and restoring it must clear the one on
// disk. Everything else that is missing from an archive is left alone.
func reconcilePauseMarker(env backupEnv, restored []string, stdout io.Writer) error {
	for _, p := range restored {
		if p == monitorPausePath {
			return nil // the archive carried a marker; it was just restored
		}
	}
	target := filepath.Join(env.destRoot, filepath.FromSlash(monitorPausePath))
	if _, err := os.Lstat(target); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cannot inspect the monitor pause marker: %w", err)
	}
	if err := os.Remove(target); err != nil {
		return fmt.Errorf("cannot remove the monitor pause marker: %w", err)
	}
	fmt.Fprintln(stdout, "the archive was taken while the monitor was running, so the pause marker was removed; the monitor is active again")
	return nil
}

// wouldClearPauseMarker reports whether a real restore would remove the pause
// marker currently on disk, so a dry run shows that too.
func wouldClearPauseMarker(env backupEnv, previewed []string) bool {
	for _, p := range previewed {
		if p == monitorPausePath {
			return false
		}
	}
	_, err := os.Lstat(filepath.Join(env.destRoot, filepath.FromSlash(monitorPausePath)))
	return err == nil
}

func touchesMonitorState(paths []string) bool {
	for _, p := range paths {
		for _, m := range monitorStatePaths {
			if p == m {
				return true
			}
		}
	}
	return false
}

// rollbackTransaction undoes this restore with its own pre-restore image,
// reusing the lease the caller already holds.
func rollbackTransaction(ctx context.Context, env backupEnv, snap *backup.RollbackSnapshot, lease backup.LockLease, stdout, stderr io.Writer) int {
	res, err := env.rollback(ctx, snap, backup.RollbackOptions{DestRoot: env.destRoot, Lease: lease})
	for _, p := range res.Restored {
		fmt.Fprintf(stdout, "rolled back %s\n", p)
	}
	for _, p := range res.Deleted {
		fmt.Fprintf(stdout, "removed %s\n", p)
	}
	if err != nil {
		fmt.Fprintln(stderr, "error: the rollback did not finish; these paths are in an uncertain state:")
		for _, p := range res.Failed {
			fmt.Fprintf(stderr, "  %s (%s)\n", p, res.FailureReasons[p])
		}
		fmt.Fprintf(stderr, "the pre-restore image is kept at %s for manual recovery\n", snap.Dir)
		return exitRollbackFailed
	}
	cleanupPreimage(ctx, env, snap, stderr)
	return exitOK
}

func cleanupPreimage(ctx context.Context, env backupEnv, snap *backup.RollbackSnapshot, stderr io.Writer) {
	if snap == nil {
		return
	}
	if err := env.cleanup(ctx, snap); err != nil {
		fmt.Fprintf(stderr, "warning: the pre-restore image at %s could not be removed: %v\n", snap.Dir, err)
	}
}

// restartAndRecheck restarts each affected service and then requires the same
// health checks the monitor uses to pass.
func restartAndRecheck(ctx context.Context, env backupEnv, services []string, stdout, stderr io.Writer) error {
	for _, svc := range services {
		restartCtx, cancel := context.WithTimeout(ctx, env.restartTimeout)
		err := env.restart(restartCtx, svc)
		ctxErr := restartCtx.Err()
		cancel()
		if ctxErr != nil {
			return fmt.Errorf("restarting %s timed out", svc)
		}
		if err != nil {
			return fmt.Errorf("restarting %s failed: %w", svc, err)
		}
		fmt.Fprintf(stdout, "restarted %s\n", svc)
	}

	var triggers []string
	for _, svc := range services {
		triggers = append(triggers, monitor.ServiceTriggers(svc)...)
	}
	recheckCtx, cancel := context.WithTimeout(ctx, env.recheckTimeout)
	results := env.recheck(recheckCtx, triggers...)
	ctxErr := recheckCtx.Err()
	cancel()
	if ctxErr != nil {
		return fmt.Errorf("the health recheck timed out")
	}

	seen := make(map[string]health.Status, len(results))
	for _, r := range results {
		seen[r.ID] = r.Status
	}
	var failed []string
	for _, id := range triggers {
		status, ok := seen[id]
		if !ok {
			failed = append(failed, id+" (missing)")
			continue
		}
		if status != health.StatusPass {
			failed = append(failed, fmt.Sprintf("%s (%s)", id, status))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("the health recheck failed: %v", failed)
	}
	fmt.Fprintln(stdout, "health recheck passed")
	return nil
}

func affectedServices(restored []string) []string {
	hit := map[string]bool{}
	for _, p := range restored {
		if svc, ok := serviceForPath[p]; ok {
			hit[svc] = true
		}
	}
	var services []string
	for _, svc := range []string{"sing-box", "caddy"} {
		if hit[svc] {
			services = append(services, svc)
		}
	}
	return services
}

func reportPaths(res backup.Result, stdout, stderr io.Writer) {
	for _, p := range res.Restored {
		fmt.Fprintf(stdout, "restored %s\n", p)
	}
	if len(res.Failed) == 0 {
		return
	}
	fmt.Fprintln(stderr, "these paths were not restored:")
	for _, p := range res.Failed {
		fmt.Fprintf(stderr, "  %s (%s)\n", p, res.FailureReasons[p])
	}
}

// reportMonitorEffect spells out what restoring the monitor's own files did,
// because the effect is invisible until the next timer run.
func reportMonitorEffect(restored []string, stdout io.Writer) {
	restoredSet := map[string]bool{}
	for _, p := range restored {
		restoredSet[p] = true
	}
	touched := false
	for _, p := range monitorStatePaths {
		if restoredSet[p] {
			touched = true
		}
	}
	if !touched {
		return
	}
	if restoredSet["var/lib/singbox-sub-manager/monitor-paused"] {
		fmt.Fprintln(stdout, "the archive was taken while the monitor was paused, so it is paused again; run `proxyctl monitor resume` to re-enable it")
	} else {
		fmt.Fprintln(stdout, "monitor state was restored from the archive, including its failure counters; restoring never starts the monitor by itself")
	}
}

// reportRestoreError maps a rejected or failed restore onto the command's exit
// codes.
func reportRestoreError(err error, stderr io.Writer) int {
	fmt.Fprintln(stderr, "error:", err)
	switch {
	case errors.Is(err, backup.ErrUnsafePath),
		errors.Is(err, backup.ErrChecksumMismatch),
		errors.Is(err, backup.ErrArchiveContentMismatch),
		errors.Is(err, backup.ErrUnsupportedSchema),
		errors.Is(err, backup.ErrUnsupportedSourceType):
		return exitArchiveInvalid
	default:
		return exitFailure
	}
}
