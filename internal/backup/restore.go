package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	// ErrChecksumMismatch reports that archived bytes do not hash to what the
	// manifest declares.
	ErrChecksumMismatch = errors.New("backup: archive checksum mismatch")
	// ErrArchiveContentMismatch reports that the archive holds entries the
	// manifest does not declare, or is missing entries it does declare.
	ErrArchiveContentMismatch = errors.New("backup: archive content does not match its manifest")
)

// Stable restore failure reasons, reported per path in FailureReasons.
const (
	FailStaging          = "staging_failed"
	FailPublish          = "publish_failed"
	FailCanceled         = "canceled"
	FailDeadlineExceeded = "deadline_exceeded"
	FailNotAttempted     = "not_attempted"
	FailStagedDiscarded  = "staged_discarded"
	FailUnsafePath       = "unsafe_path"
)

// Restore actions reported in a dry-run preview.
const (
	ActionCreate    = "create"
	ActionOverwrite = "overwrite"
	ActionUnchanged = "unchanged"
)

// restoreDirMode is used for parent directories a restore has to create. The
// archive records no directory entries, and Caddy needs to traverse /etc/caddy
// as its own user, so recreated parents are world-traversable while the files
// inside keep their recorded modes.
const restoreDirMode = fs.FileMode(0755)

// publishFileFn is replaced in tests to observe and interrupt the publish
// phase deterministically.
var publishFileFn = publishFile

// FileDiff is one line of a dry-run preview.
type FileDiff struct {
	Path      string
	Action    string
	OldSHA256 string
	NewSHA256 string
}

// Result reports what a restore did, or would do in dry-run mode.
type Result struct {
	Restored       []string
	Failed         []string
	FailureReasons map[string]string
	Preview        []FileDiff
	Preimage       *RollbackSnapshot
	// Warnings carries non-fatal problems, currently stale-snapshot pruning
	// failures, for the CLI to surface without failing the restore.
	Warnings []string
}

// RestoreOptions configures one restore.
type RestoreOptions struct {
	// DryRun computes a preview without acquiring the lock or writing.
	DryRun bool
	// DestRoot is the root archive paths are resolved against ("/" in
	// production, a temporary directory in tests).
	DestRoot string
	// SnapshotRoot holds pre-restore images. It must not live inside any
	// backed-up tree.
	SnapshotRoot string
	// Lock is used only when Lease is nil. A nil Lock means the caller
	// deliberately waives lock coordination; production must pass one.
	Lock Locker
	// Lease is an already-held lease owned by the caller. When set, Restore
	// neither acquires nor releases the lock.
	Lease LockLease
	// CapturePreimage records the byte-exact state of every target before
	// the first write, so the caller can roll the transaction back.
	CapturePreimage   bool
	LockRetryInterval time.Duration
	LockMaxWait       time.Duration
	// Now injects the clock used for snapshot naming and stale pruning.
	Now func() time.Time
}

// ReadManifest returns only the archive's manifest, without rehashing the
// payload. It is the cheap path behind `backup list`.
func ReadManifest(ctx context.Context, archivePath string) (Manifest, error) {
	f, gz, tr, err := openArchive(archivePath)
	if err != nil {
		return Manifest{}, err
	}
	defer f.Close()
	defer gz.Close()

	for {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return Manifest{}, fmt.Errorf("%w: no %s", ErrArchiveContentMismatch, ManifestName)
		}
		if err != nil {
			return Manifest{}, fmt.Errorf("backup: read archive: %w", err)
		}
		if hdr.Name != ManifestName {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return Manifest{}, fmt.Errorf("backup: read %s: %w", ManifestName, err)
		}
		return decodeManifest(data)
	}
}

// Verify rehashes every archived file and checks it against the manifest, and
// enforces the extraction safety boundary. It writes nothing, so it is safe to
// run before deciding whether to restore.
func Verify(ctx context.Context, archivePath string) (Manifest, error) {
	m, _, err := readArchive(ctx, archivePath)
	return m, err
}

// Restore writes an archive's files back under DestRoot. Every check that can
// reject the archive runs first, before any lock is taken or any byte is
// written. Files are staged next to their targets and only published once all
// of them are on disk, so a failure during staging leaves no visible change.
func Restore(ctx context.Context, archivePath string, opts RestoreOptions) (res Result, err error) {
	res.FailureReasons = map[string]string{}
	if opts.DestRoot == "" {
		return res, fmt.Errorf("backup: DestRoot is required")
	}

	m, contents, err := readArchive(ctx, archivePath)
	if err != nil {
		return res, err
	}

	if opts.DryRun {
		res.Preview, err = previewRestore(ctx, opts.DestRoot, m, contents)
		return res, err
	}

	if opts.Lease == nil && opts.Lock != nil {
		lease, lockErr := AcquireWithRetry(ctx, opts.Lock, opts.LockRetryInterval, lockMaxWait(opts))
		if lockErr != nil {
			return res, lockErr
		}
		defer func() {
			if unlockErr := lease.Unlock(); unlockErr != nil && err == nil {
				err = fmt.Errorf("backup: release recovery lock: %w", unlockErr)
			}
		}()
	}

	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	if opts.CapturePreimage {
		if opts.SnapshotRoot == "" {
			return res, fmt.Errorf("backup: SnapshotRoot is required to capture a pre-restore image")
		}
		res.Warnings = append(res.Warnings, PruneSnapshots(opts.SnapshotRoot, nowFn().UTC(), SnapshotMaxAge, "")...)

		paths := make([]string, 0, len(m.Files))
		for _, f := range m.Files {
			paths = append(paths, f.Path)
		}
		snap, snapErr := CaptureSnapshot(ctx, CaptureOptions{
			DestRoot:     opts.DestRoot,
			SnapshotRoot: opts.SnapshotRoot,
			Paths:        paths,
			Now:          nowFn,
		})
		if snapErr != nil {
			return res, snapErr
		}
		res.Preimage = snap
	}

	type staged struct {
		path string
		tmp  string
		dest string
	}

	var createdDirs []string
	fail := func(path, reason string) {
		res.Failed = append(res.Failed, path)
		res.FailureReasons[path] = reason
	}
	// abandonStaging gives the destination back exactly as the restore found
	// it: staged files disappear, directories this run created are removed,
	// and every path is reported so no caller can read the result as a
	// partial success.
	abandonStaging := func(items []staged, remaining []FileEntry, reason string) {
		for _, s := range items {
			_ = os.Remove(s.tmp)
			fail(s.path, FailStagedDiscarded)
		}
		for _, f := range remaining {
			fail(f.Path, reason)
		}
		removeCreatedDirs(createdDirs)
	}

	items := make([]staged, 0, len(m.Files))
	for i, f := range m.Files {
		if ctxErr := ctx.Err(); ctxErr != nil {
			abandonStaging(items, m.Files[i:], cancelReason(ctxErr))
			return res, ctxErr
		}
		dest, pathErr := resolveUnderRoot(opts.DestRoot, f.Path)
		if pathErr != nil {
			abandonStaging(items, nil, "")
			fail(f.Path, FailUnsafePath)
			for _, rest := range m.Files[i+1:] {
				fail(rest.Path, FailNotAttempted)
			}
			return res, pathErr
		}
		tmp, created, stageErr := stageFile(opts.DestRoot, f.Path, contents[f.Path], f.Mode, f.UID, f.GID)
		createdDirs = append(createdDirs, created...)
		if stageErr != nil {
			abandonStaging(items, nil, "")
			fail(f.Path, FailStaging)
			for _, rest := range m.Files[i+1:] {
				fail(rest.Path, FailNotAttempted)
			}
			return res, stageErr
		}
		items = append(items, staged{path: f.Path, tmp: tmp, dest: dest})
	}

	for i, s := range items {
		if ctxErr := ctx.Err(); ctxErr != nil {
			for _, remaining := range items[i:] {
				_ = os.Remove(remaining.tmp)
				fail(remaining.path, cancelReason(ctxErr))
			}
			return res, ctxErr
		}
		if pubErr := publishFileFn(s.tmp, s.dest); pubErr != nil {
			_ = os.Remove(s.tmp)
			fail(s.path, FailPublish)
			if err == nil {
				err = pubErr
			}
			continue
		}
		res.Restored = append(res.Restored, s.path)
	}

	return res, err
}

func lockMaxWait(opts RestoreOptions) time.Duration {
	if opts.LockMaxWait == 0 {
		return DefaultLockMaxWait
	}
	return opts.LockMaxWait
}

func cancelReason(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return FailDeadlineExceeded
	}
	return FailCanceled
}

// previewRestore compares the archive against what is currently on disk
// without touching the file system.
func previewRestore(ctx context.Context, destRoot string, m Manifest, contents map[string][]byte) ([]FileDiff, error) {
	preview := make([]FileDiff, 0, len(m.Files))
	for _, f := range m.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dest, err := resolveUnderRoot(destRoot, f.Path)
		if err != nil {
			return nil, err
		}
		diff := FileDiff{Path: f.Path, NewSHA256: f.SHA256, Action: ActionCreate}
		if info, statErr := os.Lstat(dest); statErr == nil && !info.Mode().IsRegular() {
			// The restore replaces whatever is there without following it,
			// so no meaningful "old" digest exists.
			diff.Action = ActionOverwrite
			preview = append(preview, diff)
			continue
		}
		if current, readErr := os.ReadFile(dest); readErr == nil {
			sum := sha256.Sum256(current)
			diff.OldSHA256 = hex.EncodeToString(sum[:])
			diff.Action = ActionOverwrite
			if diff.OldSHA256 == diff.NewSHA256 {
				diff.Action = ActionUnchanged
			}
		} else if !os.IsNotExist(readErr) {
			return nil, fmt.Errorf("backup: inspect %s: %w", f.Path, readErr)
		}
		preview = append(preview, diff)
	}
	return preview, nil
}

func openArchive(archivePath string) (*os.File, *gzip.Reader, *tar.Reader, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("backup: open archive: %w", err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, nil, nil, fmt.Errorf("backup: read archive: %w", err)
	}
	return f, gz, tar.NewReader(gz), nil
}

func decodeManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("backup: decode %s: %w", ManifestName, err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// readArchive enforces the extraction safety boundary and returns the manifest
// together with the verified payload. Nothing here writes to the file system,
// so a rejected archive can never leave a trace.
func readArchive(ctx context.Context, archivePath string) (Manifest, map[string][]byte, error) {
	f, gz, tr, err := openArchive(archivePath)
	if err != nil {
		return Manifest{}, nil, err
	}
	defer f.Close()
	defer gz.Close()

	var (
		manifestData []byte
		haveManifest bool
	)
	contents := map[string][]byte{}
	sums := map[string]string{}

	for {
		if err := ctx.Err(); err != nil {
			return Manifest{}, nil, err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Manifest{}, nil, fmt.Errorf("backup: read archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			return Manifest{}, nil, fmt.Errorf("%w: entry %q is not a regular file", ErrUnsafePath, hdr.Name)
		}
		if hdr.Name == ManifestName {
			if haveManifest {
				return Manifest{}, nil, fmt.Errorf("%w: more than one %s", ErrArchiveContentMismatch, ManifestName)
			}
			manifestData, err = io.ReadAll(tr)
			if err != nil {
				return Manifest{}, nil, fmt.Errorf("backup: read %s: %w", ManifestName, err)
			}
			haveManifest = true
			continue
		}
		if err := ValidateLogicalPath(hdr.Name); err != nil {
			return Manifest{}, nil, err
		}
		if _, dup := contents[hdr.Name]; dup {
			return Manifest{}, nil, fmt.Errorf("%w: duplicate entry %q", ErrUnsafePath, hdr.Name)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return Manifest{}, nil, fmt.Errorf("backup: read %s: %w", hdr.Name, err)
		}
		sum := sha256.Sum256(data)
		contents[hdr.Name] = data
		sums[hdr.Name] = hex.EncodeToString(sum[:])
	}

	if !haveManifest {
		return Manifest{}, nil, fmt.Errorf("%w: no %s", ErrArchiveContentMismatch, ManifestName)
	}
	m, err := decodeManifest(manifestData)
	if err != nil {
		return Manifest{}, nil, err
	}

	declared := make(map[string]struct{}, len(m.Files))
	for _, entry := range m.Files {
		declared[entry.Path] = struct{}{}
		got, ok := sums[entry.Path]
		if !ok {
			return Manifest{}, nil, fmt.Errorf("%w: %s is declared but missing", ErrArchiveContentMismatch, entry.Path)
		}
		if got != entry.SHA256 {
			return Manifest{}, nil, fmt.Errorf("%w: %s", ErrChecksumMismatch, entry.Path)
		}
	}
	for name := range contents {
		if _, ok := declared[name]; !ok {
			return Manifest{}, nil, fmt.Errorf("%w: %s is not declared", ErrArchiveContentMismatch, name)
		}
	}
	return m, contents, nil
}

// resolveUnderRoot maps an archive path to a destination path and re-checks
// that the result stays inside root, even though the manifest was validated.
// The string checks alone are not enough: a symbolic link among the parent
// directories would send an ordinary-looking path outside root, so every
// existing parent component is inspected with Lstat.
func resolveUnderRoot(root, logical string) (string, error) {
	if err := ValidateLogicalPath(logical); err != nil {
		return "", err
	}
	if err := checkParentChain(root, logical); err != nil {
		return "", err
	}
	return filepath.Join(root, filepath.FromSlash(logical)), nil
}

// checkParentChain walks the parents of logical under root and rejects any
// component that is a symbolic link. A missing component ends the walk: nothing
// below it can exist yet.
func checkParentChain(root, logical string) error {
	current := root
	parts := strings.Split(logical, "/")
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			// The component is missing, or the chain is already broken (a
			// file where a directory belongs). Nothing below it can exist,
			// and the caller reports the real problem with better context.
			return nil
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: %q crosses the symbolic link %s", ErrUnsafePath, logical, part)
		}
	}
	return nil
}

// ensureParentDir creates the parents of logical one component at a time,
// refusing to walk through a symbolic link and setting each new directory's
// mode explicitly so the result does not depend on the caller's umask. It
// returns the directories it created, deepest last, so a failed restore can
// give the destination back exactly as it found it.
func ensureParentDir(root, logical string) ([]string, error) {
	var created []string
	current := root
	parts := strings.Split(logical, "/")
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		switch {
		case err == nil:
			if info.Mode()&fs.ModeSymlink != 0 {
				return created, fmt.Errorf("%w: %q crosses the symbolic link %s", ErrUnsafePath, logical, part)
			}
			if !info.IsDir() {
				return created, fmt.Errorf("backup: %s is not a directory", current)
			}
		case os.IsNotExist(err):
			if err := os.Mkdir(current, restoreDirMode); err != nil {
				return created, fmt.Errorf("backup: create %s: %w", current, err)
			}
			if err := os.Chmod(current, restoreDirMode); err != nil {
				return created, fmt.Errorf("backup: set mode on %s: %w", current, err)
			}
			created = append(created, current)
		default:
			return created, fmt.Errorf("backup: inspect %s: %w", current, err)
		}
	}
	return created, nil
}

// removeCreatedDirs gives back directories a failed restore created, deepest
// first. A directory that meanwhile holds anything is left alone.
func removeCreatedDirs(dirs []string) {
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i])
	}
}

// stageFile writes data next to dest as a temporary file carrying its final
// mode and ownership, fsynced but not yet visible under the target name.
func stageFile(root, logical string, data []byte, mode fs.FileMode, uid, gid uint32) (string, []string, error) {
	created, err := ensureParentDir(root, logical)
	if err != nil {
		return "", created, err
	}
	dest := filepath.Join(root, filepath.FromSlash(logical))
	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, ".restore-*.tmp")
	if err != nil {
		return "", created, fmt.Errorf("backup: stage %s: %w", dest, err)
	}
	tmpName := tmp.Name()

	fail := func(err error) (string, []string, error) {
		tmp.Close()
		_ = os.Remove(tmpName)
		return "", created, err
	}
	if err := tmp.Chmod(mode); err != nil {
		return fail(fmt.Errorf("backup: set mode on %s: %w", dest, err))
	}
	if uid != uint32(os.Geteuid()) || gid != uint32(os.Getegid()) {
		if err := tmp.Chown(int(uid), int(gid)); err != nil {
			return fail(fmt.Errorf("backup: set ownership on %s: %w", dest, err))
		}
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(fmt.Errorf("backup: write %s: %w", dest, err))
	}
	if err := tmp.Sync(); err != nil {
		return fail(fmt.Errorf("backup: sync %s: %w", dest, err))
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", created, fmt.Errorf("backup: close %s: %w", dest, err)
	}
	return tmpName, created, nil
}

func publishFile(tmp, dest string) error {
	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("backup: publish %s: %w", dest, err)
	}
	return syncDir(filepath.Dir(dest))
}

// writeFileAtomic publishes data at root/logical in one step. It is used where
// each file stands on its own (snapshot payloads and rollback), unlike a
// restore, which stages every file before publishing any of them.
func writeFileAtomic(root, logical string, data []byte, mode fs.FileMode, uid, gid uint32, withOwnership bool) error {
	if !withOwnership {
		uid, gid = uint32(os.Geteuid()), uint32(os.Getegid())
	}
	tmp, _, err := stageFile(root, logical, data, mode, uid, gid)
	if err != nil {
		return err
	}
	dest := filepath.Join(root, filepath.FromSlash(logical))
	if err := publishFile(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
