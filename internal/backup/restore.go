package backup

import (
	"archive/tar"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/rand"
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

	"golang.org/x/sys/unix"
)

var (
	// ErrChecksumMismatch reports that archived bytes do not hash to what the
	// manifest declares.
	ErrChecksumMismatch = errors.New("backup: archive checksum mismatch")
	// ErrArchiveContentMismatch reports that the archive holds entries the
	// manifest does not declare, or is missing entries it does declare.
	ErrArchiveContentMismatch = errors.New("backup: archive content does not match its manifest")
	// ErrArchiveFormat reports that the archive container itself is not a
	// valid gzip/tar package (corrupt or truncated gzip framing, an
	// unparseable tar header, or a manifest.json entry that is not valid
	// JSON). It is distinct from ErrArchiveContentMismatch, which covers a
	// well-formed archive holding the wrong entries.
	ErrArchiveFormat = errors.New("backup: archive is not a valid package")
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
// phase deterministically. It performs design §6.5's per-operation sequence
// (prepareMutation, i.e. hookAfterOpen -> re-verify -> hookBeforeMutation)
// against chain, then design §6.6's publish: Renameat(parent, tmp, parent,
// dest), Fsync(parent). Restore and Rollback share it.
var publishFileFn = publishStagedFile

func publishStagedFile(chain *dirChain, tmp, dest string) error {
	if err := prepareMutation(chain.fds, chain.comps); err != nil {
		return err
	}
	parentFd := int(chain.leaf().Fd())
	if err := unix.Renameat(parentFd, tmp, parentFd, dest); err != nil {
		return fmt.Errorf("backup: publish %s: %w", dest, err)
	}
	return chain.leaf().Sync()
}

// publishNewFile stages data under chain.leaf() with a random temporary
// name, then publishes it as leaf through publishFileFn — a one-shot
// stage-then-publish for callers (Rollback; CaptureSnapshot from Task 3
// onward) that write one file at a time, unlike Restore's stage-everything-
// then-publish-everything transaction. It always closes chain. warning is
// non-empty only when a publish failure's own cleanup had to leave the
// staging file in place because design §6.7's identity comparison found it
// no longer matches what this call created — the caller decides how to
// surface that alongside the returned error, since RollbackResult (unlike
// Result) has no field for it.
func publishNewFile(chain *dirChain, leaf string, data []byte, mode fs.FileMode, uid, gid uint32) (warning string, err error) {
	defer chain.closeOpened()
	// design §6.5 lists Openat(...O_CREAT...) itself among the mutating
	// syscalls needing a fresh pre-verify, same as the Renameat inside
	// publishFileFn below.
	if err := prepareMutation(chain.fds, chain.comps); err != nil {
		return "", err
	}
	tmpName, tmpFile, err := createStagingFile(chain.leaf(), mode, uid, gid, data)
	if err != nil {
		return "", err
	}
	if pubErr := publishFileFn(chain, tmpName, leaf); pubErr != nil {
		warning = removeStagedFileFd(chain.leaf(), tmpName, tmpFile)
		tmpFile.Close()
		return warning, pubErr
	}
	tmpFile.Close()
	return "", nil
}

// createStagingFile creates, under dir, a new file with a random name
// (O_CREAT|O_EXCL, so it is provably this call's own inode), writes data to
// it with the given mode and ownership and fsyncs it, and returns both its
// name — ready for publishFileFn to rename into place — and the still-open
// fd. The caller keeps that fd open until either the file is published (then
// just closes it) or a failure needs the fd for design §6.7's pre-delete
// identity comparison before cleaning it up.
//
// The caller's own prepareMutation call covers the first Openat(O_CREAT)
// attempt. A retry after EEXIST (a random-name collision, vanishingly
// unlikely — see randomTmpName) does not get its own fresh re-verify:
// createStagingFile only holds dir, not the chain prepareMutation needs.
func createStagingFile(dir *os.File, mode fs.FileMode, uid, gid uint32, data []byte) (name string, f *os.File, err error) {
	for attempt := 0; attempt < 10; attempt++ {
		candidate, genErr := randomTmpName()
		if genErr != nil {
			return "", nil, genErr
		}
		fd, openErr := unix.Openat(int(dir.Fd()), candidate, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode.Perm()))
		if openErr != nil {
			if errors.Is(openErr, unix.EEXIST) {
				continue
			}
			return "", nil, fmt.Errorf("backup: stage %s: %w", candidate, openErr)
		}
		newFile := os.NewFile(uintptr(fd), candidate)
		if writeErr := writeStagingFile(newFile, mode, uid, gid, data); writeErr != nil {
			// This is itself a design §6.7 failure-cleanup delete (of the
			// O_CREAT|O_EXCL file just created), so it gets the same
			// pre-delete identity comparison as every other one, via the
			// same helper — not a bare Unlinkat.
			w := removeStagedFileFd(dir, candidate, newFile)
			newFile.Close()
			if w != "" {
				writeErr = fmt.Errorf("%w (%s)", writeErr, w)
			}
			return "", nil, writeErr
		}
		return candidate, newFile, nil
	}
	return "", nil, fmt.Errorf("backup: could not reserve a staging name after 10 attempts")
}

// writeStagingFile sets mode and ownership on the fd it was just created
// with (never by name), writes data and fsyncs it. It does not close f: the
// caller keeps it open (see createStagingFile).
func writeStagingFile(f *os.File, mode fs.FileMode, uid, gid uint32, data []byte) error {
	if err := f.Chmod(mode); err != nil {
		return fmt.Errorf("backup: set mode on %s: %w", f.Name(), err)
	}
	if uid != uint32(os.Geteuid()) || gid != uint32(os.Getegid()) {
		if err := f.Chown(int(uid), int(gid)); err != nil {
			return fmt.Errorf("backup: set ownership on %s: %w", f.Name(), err)
		}
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("backup: write %s: %w", f.Name(), err)
	}
	return f.Sync()
}

// randomTmpName returns a name that is, for practical purposes, guaranteed
// unique: createStagingFile's O_EXCL is what actually proves it, this just
// makes a collision vanishingly unlikely.
func randomTmpName() (string, error) {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("backup: generate staging name: %w", err)
	}
	return ".restore-" + hex.EncodeToString(buf[:]) + ".tmp", nil
}

// identityCheck is design §6.7's pre-delete identity comparison: a
// best-effort check, not a guarantee (the object can still be swapped
// between this call returning and the Unlinkat that follows it), via
// (st_dev, st_ino), without following a symbolic link at name. It reports
// match — whether the directory entry parent/name is
// still the same filesystem object as self — and gone, whether name no
// longer exists at all. gone is a distinct outcome from a mismatch: there
// being nothing left to delete (e.g. a publish whose Renameat already
// succeeded before an unrelated later step failed) is not itself something
// design §6.7 asks to be reported as needing manual confirmation; only an
// object that was swapped for a different one is.
func identityCheck(parent *os.File, name string, self *os.File) (match, gone bool) {
	var entry, held unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &entry, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false, errors.Is(err, unix.ENOENT)
	}
	if err := unix.Fstat(int(self.Fd()), &held); err != nil {
		return false, false
	}
	return entry.Dev == held.Dev && entry.Ino == held.Ino, false
}

// removeStagedFileFd deletes a staging file this run created, per design
// §6.7: no ancestor-chain re-verification (that is what triggered the
// cleanup in the first place — see design §6.5's exception for failure
// cleanup), but the identity comparison above first. A mismatch means name
// now refers to a different object than self; it is left alone and a
// non-empty warning is returned instead of deleting it. A name that is
// simply gone already (nothing left to delete — e.g. this file was in fact
// published, and only an unrelated later step failed) is not a mismatch and
// produces no warning.
func removeStagedFileFd(parent *os.File, name string, self *os.File) string {
	match, gone := identityCheck(parent, name, self)
	if gone {
		return ""
	}
	if !match {
		return fmt.Sprintf("backup: %s no longer matches the staging file this run created; left in place, needs manual confirmation", name)
	}
	_ = unix.Unlinkat(int(parent.Fd()), name, 0)
	return ""
}

// createdDirRef names one directory this package itself created: the fd of
// its parent and its name — enough to Unlinkat(parent, name, AT_REMOVEDIR)
// it later without re-resolving anything by path — plus self, the
// directory's own fd, kept open for design §6.7's pre-delete identity
// comparison.
type createdDirRef struct {
	parent *os.File
	name   string
	self   *os.File
}

// removeCreatedDirsFd undoes, in reverse creation order (deepest first),
// only the directories a failed operation itself created (design §6.7): no
// re-verification, no recursion — AT_REMOVEDIR only ever removes an already
// -empty directory, so anything holding content this run did not put there
// is left alone regardless. Each removal is additionally gated on the
// identity comparison above; a mismatch (not a name that is simply already
// gone — see removeStagedFileFd) produces a warning instead of a delete
// attempt.
func removeCreatedDirsFd(dirs []createdDirRef) []string {
	var warnings []string
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		match, gone := identityCheck(d.parent, d.name, d.self)
		if gone {
			continue
		}
		if !match {
			warnings = append(warnings, fmt.Sprintf("backup: %s no longer matches the directory this run created; left in place, needs manual confirmation", d.name))
			continue
		}
		_ = unix.Unlinkat(int(d.parent.Fd()), d.name, unix.AT_REMOVEDIR)
	}
	return warnings
}

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
			if isArchiveFormatError(err) {
				return Manifest{}, fmt.Errorf("%w: %v", ErrArchiveFormat, err)
			}
			return Manifest{}, fmt.Errorf("backup: read archive: %w", err)
		}
		if hdr.Name != ManifestName {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			if isArchiveFormatError(err) {
				return Manifest{}, fmt.Errorf("%w: %v", ErrArchiveFormat, err)
			}
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
		path    string
		chain   *dirChain
		leaf    string
		tmp     string
		tmpFile *os.File
	}

	fail := func(path, reason string) {
		res.Failed = append(res.Failed, path)
		res.FailureReasons[path] = reason
	}

	destRootFd, rootErr := openTrustedRoot(opts.DestRoot, false, 0)
	if rootErr != nil {
		// v0.7 parity: the first path carries the real failure, the rest
		// are reported as never attempted, not all as equally staging-failed.
		if len(m.Files) > 0 {
			fail(m.Files[0].Path, FailStaging)
			for _, rest := range m.Files[1:] {
				fail(rest.Path, FailNotAttempted)
			}
		}
		return res, fmt.Errorf("backup: open destination root: %w", rootErr)
	}
	defer destRootFd.Close()

	var createdDirs []createdDirRef
	// abandonStaging gives the destination back exactly as the restore found
	// it: staged files disappear, directories this run created are removed,
	// and every path is reported so no caller can read the result as a
	// partial success. createdDirs entries reference fds owned by items'
	// chains AND by the chain the caller is in the middle of handling (its
	// own newly-created directories were already recorded into createdDirs
	// before the caller ever calls this): every caller MUST call this
	// before closing that chain, not after, or removeCreatedDirsFd below
	// runs against an already-closed fd and silently does nothing.
	abandonStaging := func(items []staged, remaining []FileEntry, reason string) {
		for _, s := range items {
			if w := removeStagedFileFd(s.chain.leaf(), s.tmp, s.tmpFile); w != "" {
				res.Warnings = append(res.Warnings, w)
			}
			s.tmpFile.Close()
			fail(s.path, FailStagedDiscarded)
		}
		for _, f := range remaining {
			fail(f.Path, reason)
		}
		res.Warnings = append(res.Warnings, removeCreatedDirsFd(createdDirs)...)
		for _, s := range items {
			s.chain.closeOpened()
		}
	}

	items := make([]staged, 0, len(m.Files))
	for i, f := range m.Files {
		if ctxErr := ctx.Err(); ctxErr != nil {
			abandonStaging(items, m.Files[i:], cancelReason(ctxErr))
			return res, ctxErr
		}
		chain, leaf, pathErr := resolveUnderTrustedRoot(destRootFd, f.Path, true, restoreDirMode, dirAllowExisting)
		if chain != nil {
			for idx, wasCreated := range chain.created {
				if wasCreated {
					createdDirs = append(createdDirs, createdDirRef{parent: chain.fds[idx], name: chain.comps[idx], self: chain.fds[idx+1]})
				}
			}
		}
		if pathErr != nil {
			// abandonStaging's directory cleanup needs chain's own fds
			// (recorded into createdDirs just above) still open, so it
			// must run before chain itself is closed.
			abandonStaging(items, nil, "")
			if chain != nil {
				chain.closeOpened()
			}
			reason := FailStaging
			if errors.Is(pathErr, ErrUnsafePath) {
				reason = FailUnsafePath
			}
			fail(f.Path, reason)
			for _, rest := range m.Files[i+1:] {
				fail(rest.Path, FailNotAttempted)
			}
			return res, pathErr
		}
		// design §6.5 lists Openat(...O_CREAT...) itself among the mutating
		// syscalls needing a fresh pre-verify.
		if verifyErr := prepareMutation(chain.fds, chain.comps); verifyErr != nil {
			abandonStaging(items, nil, "")
			chain.closeOpened()
			fail(f.Path, FailUnsafePath)
			for _, rest := range m.Files[i+1:] {
				fail(rest.Path, FailNotAttempted)
			}
			return res, verifyErr
		}
		tmp, tmpFile, stageErr := createStagingFile(chain.leaf(), f.Mode, f.UID, f.GID, contents[f.Path])
		if stageErr != nil {
			abandonStaging(items, nil, "")
			chain.closeOpened()
			fail(f.Path, FailStaging)
			for _, rest := range m.Files[i+1:] {
				fail(rest.Path, FailNotAttempted)
			}
			return res, stageErr
		}
		items = append(items, staged{path: f.Path, chain: chain, leaf: leaf, tmp: tmp, tmpFile: tmpFile})
	}

	for i, s := range items {
		if ctxErr := ctx.Err(); ctxErr != nil {
			for _, remaining := range items[i:] {
				if w := removeStagedFileFd(remaining.chain.leaf(), remaining.tmp, remaining.tmpFile); w != "" {
					res.Warnings = append(res.Warnings, w)
				}
				remaining.tmpFile.Close()
				remaining.chain.closeOpened()
				fail(remaining.path, cancelReason(ctxErr))
			}
			return res, ctxErr
		}
		if pubErr := publishFileFn(s.chain, s.tmp, s.leaf); pubErr != nil {
			if w := removeStagedFileFd(s.chain.leaf(), s.tmp, s.tmpFile); w != "" {
				res.Warnings = append(res.Warnings, w)
			}
			s.tmpFile.Close()
			s.chain.closeOpened()
			reason := FailPublish
			if errors.Is(pubErr, ErrUnsafePath) {
				reason = FailUnsafePath
			}
			fail(s.path, reason)
			if err == nil {
				err = pubErr
			}
			continue
		}
		s.tmpFile.Close()
		s.chain.closeOpened()
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

// previewRestore computes a dry-run diff without writing anything and
// without acquiring a lock (design §3.1): parent directories are only
// resolved, never created; a missing parent, like a missing leaf, means
// ActionCreate. It deliberately does not run design §6.5's ancestor-chain
// re-verification — there is nothing here that mutates, so there is nothing
// for that re-verification to protect.
func previewRestore(ctx context.Context, destRoot string, m Manifest, contents map[string][]byte) ([]FileDiff, error) {
	rootFd, err := openTrustedRoot(destRoot, false, 0)
	if err != nil {
		return nil, fmt.Errorf("backup: open destination root: %w", err)
	}
	defer rootFd.Close()

	preview := make([]FileDiff, 0, len(m.Files))
	for _, f := range m.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		diff, err := previewOneFile(rootFd, f)
		if err != nil {
			return nil, err
		}
		preview = append(preview, diff)
	}
	return preview, nil
}

func previewOneFile(rootFd *os.File, f FileEntry) (FileDiff, error) {
	diff := FileDiff{Path: f.Path, NewSHA256: f.SHA256, Action: ActionCreate}

	chain, leaf, resErr := resolveUnderTrustedRoot(rootFd, f.Path, false, 0, dirAllowExisting)
	if resErr != nil {
		if chain != nil {
			chain.closeOpened()
		}
		if errors.Is(resErr, os.ErrNotExist) {
			// Nothing below a missing parent can exist either.
			return diff, nil
		}
		return FileDiff{}, resErr
	}
	defer chain.closeOpened()
	parentFd := int(chain.leaf().Fd())

	var st unix.Stat_t
	statErr := unix.Fstatat(parentFd, leaf, &st, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case errors.Is(statErr, os.ErrNotExist):
		// Leaf missing: stays ActionCreate.
	case statErr != nil:
		return FileDiff{}, fmt.Errorf("backup: inspect %s: %w", f.Path, statErr)
	case st.Mode&unix.S_IFMT != unix.S_IFREG:
		// The restore replaces whatever is there without following it, so
		// no meaningful "old" digest exists.
		diff.Action = ActionOverwrite
	default:
		hookBeforeMutation() // design §7.1 P1b's injection point; no re-verify around it (design §3.1).
		fd, openErr := unix.Openat(parentFd, leaf, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			// No longer a regular file by the time it was opened: same as
			// the non-regular case above.
			diff.Action = ActionOverwrite
			break
		}
		lf := os.NewFile(uintptr(fd), leaf)
		current, readErr := io.ReadAll(lf)
		lf.Close()
		if readErr != nil {
			return FileDiff{}, fmt.Errorf("backup: read %s: %w", f.Path, readErr)
		}
		sum := sha256.Sum256(current)
		diff.OldSHA256 = hex.EncodeToString(sum[:])
		diff.Action = ActionOverwrite
		if diff.OldSHA256 == diff.NewSHA256 {
			diff.Action = ActionUnchanged
		}
	}
	return diff, nil
}

func openArchive(archivePath string) (*os.File, *gzip.Reader, *tar.Reader, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("backup: open archive: %w", err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		if isArchiveFormatError(err) {
			return nil, nil, nil, fmt.Errorf("%w: %v", ErrArchiveFormat, err)
		}
		return nil, nil, nil, fmt.Errorf("backup: read archive: %w", err)
	}
	return f, gz, tar.NewReader(gz), nil
}

// isArchiveFormatError reports whether err is one of the specific, stable
// errors gzip and tar return for malformed framing (a bad magic number or
// checksum, corrupt DEFLATE data past a valid gzip header, an unparseable
// tar header, or a stream that ends before its declared content does) — as
// opposed to a filesystem, permission, or cancellation failure that merely
// surfaced while reading through them. Those stdlib errors are propagated
// unwrapped on a genuine I/O failure from the underlying reader, so this
// check does not need to special-case them.
func isArchiveFormatError(err error) bool {
	if errors.Is(err, gzip.ErrHeader) ||
		errors.Is(err, gzip.ErrChecksum) ||
		errors.Is(err, tar.ErrHeader) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.EOF) {
		return true
	}
	var flateErr flate.CorruptInputError
	return errors.As(err, &flateErr)
}

func decodeManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("%w: decode %s: %v", ErrArchiveFormat, ManifestName, err)
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
			if isArchiveFormatError(err) {
				return Manifest{}, nil, fmt.Errorf("%w: %v", ErrArchiveFormat, err)
			}
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
				if isArchiveFormatError(err) {
					return Manifest{}, nil, fmt.Errorf("%w: %v", ErrArchiveFormat, err)
				}
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
			if isArchiveFormatError(err) {
				return Manifest{}, nil, fmt.Errorf("%w: %v", ErrArchiveFormat, err)
			}
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
