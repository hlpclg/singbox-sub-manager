package backup

import (
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
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	snapshotSchemaVersion = 1
	snapshotMetaName      = "snapshot.json"
	snapshotDirMode       = fs.FileMode(0700)
	snapshotFileMode      = fs.FileMode(0600)
	// SnapshotMaxAge is how long an abandoned pre-restore image is kept
	// before a later restore prunes it.
	SnapshotMaxAge = 7 * 24 * time.Hour
)

// Stable rollback failure reasons.
const (
	FailReadSnapshot = "snapshot_unreadable"
	FailWrite        = "write_failed"
	FailDelete       = "delete_failed"
)

// SnapshotEntry records what one target path looked like before a restore.
// Exists=false means the path was absent, so rolling back means deleting
// whatever the restore created there.
type SnapshotEntry struct {
	Path    string
	Exists  bool
	Mode    fs.FileMode
	UID     uint32
	GID     uint32
	SHA256  string
	Payload string
}

type snapshotEntryJSON struct {
	Path    string `json:"path"`
	Exists  bool   `json:"exists"`
	Mode    string `json:"mode"`
	UID     uint32 `json:"uid"`
	GID     uint32 `json:"gid"`
	SHA256  string `json:"sha256"`
	Payload string `json:"payload"`
}

func (e SnapshotEntry) MarshalJSON() ([]byte, error) {
	return json.Marshal(snapshotEntryJSON{
		Path:    e.Path,
		Exists:  e.Exists,
		Mode:    formatMode(e.Mode),
		UID:     e.UID,
		GID:     e.GID,
		SHA256:  e.SHA256,
		Payload: e.Payload,
	})
}

func (e *SnapshotEntry) UnmarshalJSON(data []byte) error {
	var raw snapshotEntryJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	mode, err := parseMode(raw.Mode)
	if err != nil {
		return err
	}
	e.Path = raw.Path
	e.Exists = raw.Exists
	e.Mode = mode
	e.UID = raw.UID
	e.GID = raw.GID
	e.SHA256 = raw.SHA256
	e.Payload = raw.Payload
	return nil
}

// RollbackSnapshot is the byte-exact pre-restore image of every path a restore
// is about to touch. Dir is where it lives and is not serialized.
type RollbackSnapshot struct {
	SchemaVersion int             `json:"schema_version"`
	TransactionID string          `json:"transaction_id"`
	CreatedAt     time.Time       `json:"created_at"`
	Entries       []SnapshotEntry `json:"entries"`
	Dir           string          `json:"-"`
}

// CaptureOptions configures one pre-restore image.
type CaptureOptions struct {
	DestRoot     string
	SnapshotRoot string
	Paths        []string
	Now          func() time.Time
}

// RollbackOptions configures a rollback. DestRoot must match the root the
// image was captured against; Lease must already be held by the caller, which
// keeps ownership of it.
type RollbackOptions struct {
	DestRoot string
	Lease    LockLease
}

// RollbackResult reports the outcome per path.
type RollbackResult struct {
	Restored       []string
	Deleted        []string
	Failed         []string
	FailureReasons map[string]string
}

// capturedFile names one file CaptureSnapshot itself has already published
// into the transaction directory (a payload or snapshot.json), with the fd
// obtained from publishFileInChain — still open, for design §6.7's
// pre-delete identity comparison if a later step forces this call to clean
// up what it already wrote.
type capturedFile struct {
	name string
	self *os.File
}

// syncTxnDirFn is a no-op-swappable seam (same pattern as publishFileFn) so
// a test can inject an fsync failure on the transaction directory or the
// snapshot root without actually closing or otherwise invalidating the fd —
// the fd must stay usable afterward for abandonCapture's own §6.7 cleanup.
var syncTxnDirFn = func(f *os.File) error { return f.Sync() }

// CaptureSnapshot copies every listed path byte for byte, together with its
// mode and ownership, into a fresh transaction directory. It must complete
// before a restore writes anything, so a failed restore can always be undone.
func CaptureSnapshot(ctx context.Context, opts CaptureOptions) (*RollbackSnapshot, error) {
	if opts.DestRoot == "" {
		return nil, fmt.Errorf("backup: DestRoot is required")
	}
	if opts.SnapshotRoot == "" {
		return nil, fmt.Errorf("backup: SnapshotRoot is required")
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	createdAt := nowFn().UTC()

	destRootFd, err := openTrustedRoot(opts.DestRoot, false, 0)
	if err != nil {
		return nil, fmt.Errorf("backup: open destination root: %w", err)
	}
	defer destRootFd.Close()

	snapRootFd, err := openTrustedRoot(opts.SnapshotRoot, true, snapshotDirMode)
	if err != nil {
		return nil, fmt.Errorf("backup: open snapshot root: %w", err)
	}
	defer snapRootFd.Close()

	id, err := newTransactionID(createdAt)
	if err != nil {
		return nil, err
	}
	// Design §6.3's "must create" policy: a transaction ID collision is an
	// error, never a silent reuse of whatever is already there.
	txnChain, err := resolveParentDirs(snapRootFd, []string{id}, true, snapshotDirMode, dirMustCreate)
	if err != nil {
		if txnChain != nil {
			txnChain.closeOpened()
		}
		return nil, fmt.Errorf("backup: create snapshot directory: %w", err)
	}

	snap := &RollbackSnapshot{
		SchemaVersion: snapshotSchemaVersion,
		TransactionID: id,
		CreatedAt:     createdAt,
		Dir:           filepath.Join(opts.SnapshotRoot, id),
	}

	var written []capturedFile
	// abandonCapture undoes exactly what this call itself wrote (design
	// §6.7): each recorded payload/metadata file by name, identity-checked
	// against the fd held since it was published, then the transaction
	// directory itself (also identity-checked, against its parent,
	// snapRootFd) — which AT_REMOVEDIR leaves in place, unremoved, if
	// anything this call did not write is present in it. Any "needs manual
	// confirmation" text from a failed identity check (design §6.7) is
	// returned, not discarded — CaptureSnapshot has nowhere else to put it
	// (no Warnings field, unlike Result), so every caller folds it into the
	// error it returns, exactly as publishFileInChain already does for a
	// single file.
	abandonCapture := func() string {
		var warnings []string
		for _, w := range written {
			if msg := removeStagedFileFd(txnChain.leaf(), w.name, w.self); msg != "" {
				warnings = append(warnings, msg)
			}
			w.self.Close()
		}
		warnings = append(warnings, removeCreatedDirsFd([]createdDirRef{{parent: txnChain.fds[0], name: txnChain.comps[0], self: txnChain.leaf()}})...)
		txnChain.closeOpened()
		return strings.Join(warnings, "; ")
	}
	fail := func(err error) (*RollbackSnapshot, error) {
		if w := abandonCapture(); w != "" {
			err = fmt.Errorf("%w (%s)", err, w)
		}
		return nil, err
	}

	paths := append([]string(nil), opts.Paths...)
	sort.Strings(paths)
	for i, logical := range paths {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fail(ctxErr)
		}
		entry, data, err := captureOneTarget(destRootFd, logical, i)
		if err != nil {
			return fail(err)
		}
		if entry.Exists {
			self, pubErr := publishFileInChain(txnChain, entry.Payload, data, snapshotFileMode, 0, 0)
			if pubErr != nil {
				return fail(pubErr)
			}
			written = append(written, capturedFile{name: entry.Payload, self: self})
		}
		snap.Entries = append(snap.Entries, entry)
	}

	meta, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fail(fmt.Errorf("backup: encode snapshot metadata: %w", err))
	}
	metaSelf, err := publishFileInChain(txnChain, snapshotMetaName, meta, snapshotFileMode, 0, 0)
	if err != nil {
		return fail(err)
	}
	written = append(written, capturedFile{name: snapshotMetaName, self: metaSelf})

	// The written fds stay open across both syncs (not closed until they
	// succeed): a failed fsync is itself a failure this function must clean
	// up after (design §6.7), and abandonCapture's identity comparison
	// needs them open to do that, exactly like every other failure path
	// above.
	if err := syncTxnDirFn(txnChain.leaf()); err != nil {
		return fail(fmt.Errorf("backup: sync snapshot directory: %w", err))
	}
	if err := syncTxnDirFn(snapRootFd); err != nil {
		return fail(fmt.Errorf("backup: sync snapshot root: %w", err))
	}
	for _, w := range written {
		w.self.Close()
	}
	txnChain.closeOpened()
	return snap, nil
}

// captureOneTarget resolves logical under destRootFd's trusted root,
// re-verifies the chain immediately before touching the target (design
// §6.5's "CaptureSnapshot 读取目标之前"), and if the target exists, reads it
// — CaptureSnapshot's own fd-relative version of archive.go's readPayload
// (design §6.6): opened without following a symbolic link, then the opened
// descriptor's own type and link count are re-checked, exactly as v0.7's
// path-based version did. A missing parent or missing leaf both mean
// Exists=false, matching v0.7's Lstat-based check.
func captureOneTarget(destRootFd *os.File, logical string, index int) (SnapshotEntry, []byte, error) {
	entry := SnapshotEntry{Path: logical}

	chain, leaf, resErr := resolveUnderTrustedRoot(destRootFd, logical, false, 0, dirAllowExisting)
	if resErr != nil {
		if chain != nil {
			chain.closeOpened()
		}
		if errors.Is(resErr, os.ErrNotExist) {
			return entry, nil, nil
		}
		return SnapshotEntry{}, nil, resErr
	}
	defer chain.closeOpened()
	if err := prepareMutation(chain.fds, chain.comps); err != nil {
		return SnapshotEntry{}, nil, err
	}
	parentFd := int(chain.leaf().Fd())

	// The classification Fstatat below and the read Openat further down are
	// both fd-relative against this same just-reverified parentFd — design
	// §6.5's "nothing else between prepareMutation and the syscall" is about
	// not re-resolving anything by path in between, not about issuing
	// exactly one syscall; two reads against an already-verified fd don't
	// reopen any window prepareMutation just closed.
	var st unix.Stat_t
	statErr := unix.Fstatat(parentFd, leaf, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(statErr, os.ErrNotExist) {
		return entry, nil, nil
	}
	if statErr != nil {
		return SnapshotEntry{}, nil, fmt.Errorf("backup: stat %s: %w", logical, statErr)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return SnapshotEntry{}, nil, fmt.Errorf("%w: %s", ErrUnsupportedSourceType, logical)
	}

	fd, openErr := unix.Openat(parentFd, leaf, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if openErr != nil {
		if openErr == unix.ELOOP {
			return SnapshotEntry{}, nil, fmt.Errorf("%w: %s", ErrUnsupportedSourceType, logical)
		}
		return SnapshotEntry{}, nil, fmt.Errorf("backup: open %s: %w", logical, openErr)
	}
	f := os.NewFile(uintptr(fd), leaf)
	defer f.Close()
	var fst unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &fst); err != nil {
		return SnapshotEntry{}, nil, fmt.Errorf("backup: stat %s: %w", logical, err)
	}
	if fst.Mode&unix.S_IFMT != unix.S_IFREG || fst.Nlink != 1 {
		return SnapshotEntry{}, nil, fmt.Errorf("%w: %s", ErrUnsupportedSourceType, logical)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return SnapshotEntry{}, nil, fmt.Errorf("backup: read %s: %w", logical, err)
	}
	sum := sha256.Sum256(data)
	entry.Exists = true
	entry.Mode = fs.FileMode(fst.Mode & 0777)
	entry.UID = fst.Uid
	entry.GID = fst.Gid
	entry.SHA256 = hex.EncodeToString(sum[:])
	entry.Payload = fmt.Sprintf("payload-%04d.bin", index)
	return entry, data, nil
}

// LoadSnapshot reads an image previously written by CaptureSnapshot. dir is
// a trusted root (design §3.1's supplementary table), resolved by following
// symbolic links exactly like v0.7; snapshot.json itself is opened relative
// to dir's own fd, without following a symbolic link there.
func LoadSnapshot(dir string) (*RollbackSnapshot, error) {
	// openTrustedRoot already wraps its own error with "backup: open
	// <dir>: ...", so it is returned as-is rather than wrapped again.
	dirFd, err := openTrustedRoot(dir, false, 0)
	if err != nil {
		return nil, err
	}
	defer dirFd.Close()
	snap, err := readSnapshotMetadata(dirFd)
	if err != nil {
		return nil, err
	}
	snap.Dir = dir
	return snap, nil
}

// readSnapshotMetadata reads and parses snapshot.json relative to dirFd
// (already open and, design §6.5, re-verified by the caller if that
// applies to how dirFd was obtained), applying design §3.1's payload-name
// check to Exists=true entries only — CaptureSnapshot never produces a
// Payload this rejects, so a rejection here means the metadata was tampered
// with or corrupted, not that a normal snapshot became unloadable.
func readSnapshotMetadata(dirFd *os.File) (*RollbackSnapshot, error) {
	fd, err := unix.Openat(int(dirFd.Fd()), snapshotMetaName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("backup: open snapshot metadata: %w", err)
	}
	f := os.NewFile(uintptr(fd), snapshotMetaName)
	defer f.Close()
	info, statErr := f.Stat()
	if statErr != nil {
		return nil, fmt.Errorf("backup: stat snapshot metadata: %w", statErr)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("backup: snapshot metadata is not a regular file")
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("backup: read snapshot metadata: %w", err)
	}
	var snap RollbackSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("backup: decode snapshot metadata: %w", err)
	}
	if snap.SchemaVersion != snapshotSchemaVersion {
		return nil, fmt.Errorf("%w: snapshot %d", ErrUnsupportedSchema, snap.SchemaVersion)
	}
	for _, e := range snap.Entries {
		if err := ValidateLogicalPath(e.Path); err != nil {
			return nil, err
		}
		if e.Exists {
			if err := validatePayloadName(e.Payload); err != nil {
				return nil, err
			}
		}
	}
	return &snap, nil
}

// Rollback puts every captured path back exactly as it was: original bytes,
// mode and ownership for files that existed, and removal for paths the restore
// created. It never acquires or releases the caller's lease.
func Rollback(ctx context.Context, snapshot *RollbackSnapshot, opts RollbackOptions) (RollbackResult, error) {
	res := RollbackResult{FailureReasons: map[string]string{}}
	if snapshot == nil {
		return res, fmt.Errorf("backup: no snapshot to roll back")
	}
	if opts.DestRoot == "" {
		return res, fmt.Errorf("backup: DestRoot is required")
	}

	var firstErr error
	fail := func(path, reason string, err error) {
		res.Failed = append(res.Failed, path)
		res.FailureReasons[path] = reason
		if firstErr == nil {
			firstErr = err
		}
	}

	destRootFd, rootErr := openTrustedRoot(opts.DestRoot, false, 0)
	if rootErr != nil {
		for _, entry := range snapshot.Entries {
			fail(entry.Path, FailWrite, rootErr)
		}
		return res, rootErr
	}
	defer destRootFd.Close()

	// The payload root (snapshot.Dir) is only needed for Exists=true
	// entries; opened lazily and reused so a snapshot holding only
	// Exists=false entries never has to touch it at all.
	var payloadRootFd *os.File
	defer func() {
		if payloadRootFd != nil {
			payloadRootFd.Close()
		}
	}()
	openPayloadRoot := func() (*os.File, error) {
		if payloadRootFd == nil {
			fd, err := openTrustedRoot(snapshot.Dir, false, 0)
			if err != nil {
				return nil, err
			}
			payloadRootFd = fd
		}
		return payloadRootFd, nil
	}

	for i, entry := range snapshot.Entries {
		if err := ctx.Err(); err != nil {
			reason := FailCanceled
			if err == context.DeadlineExceeded {
				reason = FailDeadlineExceeded
			}
			for _, remaining := range snapshot.Entries[i:] {
				res.Failed = append(res.Failed, remaining.Path)
				res.FailureReasons[remaining.Path] = reason
			}
			if firstErr == nil {
				firstErr = err
			}
			return res, firstErr
		}

		if !entry.Exists {
			// Intentional delete of a target the original restore created
			// (design §6.6): re-verify the parent chain, Unlinkat, Fsync.
			chain, leaf, resErr := resolveUnderTrustedRoot(destRootFd, entry.Path, false, 0, dirAllowExisting)
			if resErr != nil {
				if chain != nil {
					chain.closeOpened()
				}
				if errors.Is(resErr, ErrUnsafePath) {
					fail(entry.Path, FailUnsafePath, resErr)
				} else {
					// Covers a missing intermediate parent too (v0.7
					// parity, not a success): os.Remove(target) tolerated
					// the target itself already being gone, but the
					// syncDir(parent) right after it failed whenever the
					// parent itself was also missing — so that case was
					// always FailDelete, never a silent "already deleted".
					fail(entry.Path, FailDelete, resErr)
				}
				continue
			}
			if delErr := deleteRollbackTarget(chain, leaf); delErr != nil {
				reason := FailDelete
				if errors.Is(delErr, ErrUnsafePath) {
					reason = FailUnsafePath
				}
				fail(entry.Path, reason, delErr)
				continue
			}
			res.Deleted = append(res.Deleted, entry.Path)
			continue
		}

		// Read and validate the payload before touching DestRoot at all
		// (v0.7 order: resolve happened, but nothing was created, until
		// after the payload was read and checksummed). An entry this call
		// is going to reject must not leave newly created parent
		// directories behind under DestRoot — Rollback never cleans those
		// up (design §6.3's table), unlike Restore.
		dirFd, dirErr := openPayloadRoot()
		if dirErr != nil {
			fail(entry.Path, FailReadSnapshot, fmt.Errorf("backup: open snapshot directory: %w", dirErr))
			continue
		}
		data, readErr := readRollbackPayload(dirFd, entry.Payload)
		if readErr != nil {
			fail(entry.Path, FailReadSnapshot, fmt.Errorf("backup: read snapshot payload for %s: %w", entry.Path, readErr))
			continue
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != entry.SHA256 {
			fail(entry.Path, FailReadSnapshot, fmt.Errorf("%w: snapshot payload for %s", ErrChecksumMismatch, entry.Path))
			continue
		}

		chain, leaf, resErr := resolveUnderTrustedRoot(destRootFd, entry.Path, true, restoreDirMode, dirAllowExisting)
		if resErr != nil {
			if chain != nil {
				chain.closeOpened()
			}
			reason := FailWrite
			if errors.Is(resErr, ErrUnsafePath) {
				reason = FailUnsafePath
			}
			fail(entry.Path, reason, resErr)
			continue
		}
		if writeErr := publishNewFile(chain, leaf, data, entry.Mode, entry.UID, entry.GID); writeErr != nil {
			// RollbackResult has no Warnings field (契约冻结); design §6.7's
			// "注明该路径需人工确认", when applicable, is already folded
			// into writeErr by publishFileInChain.
			reason := FailWrite
			if errors.Is(writeErr, ErrUnsafePath) {
				reason = FailUnsafePath
			}
			fail(entry.Path, reason, writeErr)
			continue
		}
		res.Restored = append(res.Restored, entry.Path)
	}

	return res, firstErr
}

// deleteRollbackTarget performs Rollback's intentional delete of a target
// (design §6.6): re-verify the chain, Unlinkat the leaf, Fsync the parent.
// Always closes chain. An already-missing leaf is not an error, matching
// v0.7's os.IsNotExist-tolerant os.Remove.
func deleteRollbackTarget(chain *dirChain, leaf string) error {
	defer chain.closeOpened()
	if err := prepareMutation(chain.fds, chain.comps); err != nil {
		return err
	}
	parentFd := int(chain.leaf().Fd())
	if err := unix.Unlinkat(parentFd, leaf, 0); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("backup: remove %s: %w", leaf, err)
	}
	return chain.leaf().Sync()
}

// validatePayloadName rejects any SnapshotEntry.Payload that is not a
// single path component (design §3.1's supplementary table): the
// fd-relative Openat readRollbackPayload uses resolves ".." and multi-level
// names, and O_NOFOLLOW does not stop "..", so this check — callers apply it
// only to Exists=true entries — is what keeps a payload read inside the
// transaction directory. CaptureSnapshot never produces a Payload this
// rejects.
func validatePayloadName(payload string) error {
	if payload == "" || payload == "." || payload == ".." || strings.Contains(payload, "/") {
		return fmt.Errorf("%w: invalid snapshot payload name %q", ErrUnsafePath, payload)
	}
	return nil
}

// readRollbackPayload reads an Exists=true entry's payload relative to
// dirRootFd (the trusted root at snapshot.Dir), without following a
// symbolic link, and re-checks the opened descriptor is a regular file.
func readRollbackPayload(dirRootFd *os.File, payload string) ([]byte, error) {
	if err := validatePayloadName(payload); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(dirRootFd.Fd()), payload, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("backup: open payload %s: %w", payload, err)
	}
	f := os.NewFile(uintptr(fd), payload)
	defer f.Close()
	info, statErr := f.Stat()
	if statErr != nil {
		return nil, fmt.Errorf("backup: stat payload %s: %w", payload, statErr)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("backup: payload %s is not a regular file", payload)
	}
	return io.ReadAll(f)
}

// CleanupSnapshot removes an image. Call it only once the transaction is over:
// either the restore succeeded and was verified, or the rollback finished.
// Per design §3.1's supplementary table, the trusted root is snapshot.Dir's
// *parent* — snapshot.Dir itself is not trusted as a root, since removing
// the transaction directory needs its parent's fd, and following a symbolic
// link at snapshot.Dir would send the recursive delete below outside the
// snapshot root entirely.
func CleanupSnapshot(ctx context.Context, snapshot *RollbackSnapshot) error {
	if snapshot == nil || snapshot.Dir == "" {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	parentFd, err := openTrustedRoot(filepath.Dir(snapshot.Dir), false, 0)
	if err != nil {
		return fmt.Errorf("backup: open %s: %w", filepath.Dir(snapshot.Dir), err)
	}
	defer parentFd.Close()
	name := filepath.Base(snapshot.Dir)
	// Design §6.5: this intentional delete is re-verified first — trivially
	// here, since the chain is just the root itself with nothing between it
	// and name, but the hooks and the (no-op) walk still run, consistent
	// with every other intentional delete in this package.
	if err := prepareMutation([]*os.File{parentFd}, nil); err != nil {
		return err
	}
	if err := removeTreeFd(parentFd, name); err != nil {
		return fmt.Errorf("backup: remove snapshot %s: %w", snapshot.TransactionID, err)
	}
	return nil
}

// removeTreeFd recursively removes the directory parent/name, exactly like
// os.RemoveAll but never following a symbolic link at any level (design
// §6.6): each subdirectory is entered via Openat(O_NOFOLLOW|O_DIRECTORY);
// anything else — a file, or a symbolic link regardless of what it points
// to — is removed by a plain Unlinkat, never traversed into.
func removeTreeFd(parent *os.File, name string) error {
	dirFd, err := openChildDir(parent, name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	entries, readErr := dirFd.Readdirnames(-1)
	if readErr != nil {
		dirFd.Close()
		return fmt.Errorf("backup: read %s: %w", name, readErr)
	}
	for _, entry := range entries {
		var st unix.Stat_t
		if statErr := unix.Fstatat(int(dirFd.Fd()), entry, &st, unix.AT_SYMLINK_NOFOLLOW); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			dirFd.Close()
			return fmt.Errorf("backup: inspect %s/%s: %w", name, entry, statErr)
		}
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			if err := removeTreeFd(dirFd, entry); err != nil {
				dirFd.Close()
				return err
			}
			continue
		}
		if err := unix.Unlinkat(int(dirFd.Fd()), entry, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
			dirFd.Close()
			return fmt.Errorf("backup: remove %s/%s: %w", name, entry, err)
		}
	}
	dirFd.Close()
	if err := unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("backup: remove %s: %w", name, err)
	}
	return nil
}

// PruneSnapshots removes abandoned images older than maxAge. It never touches
// the active transaction and never removes the most recent image, which is the
// one an operator would still need to investigate a failed restore. Removal
// problems are returned as warnings, not errors: pruning must not fail a
// restore. Per design §6.6, this does not go through the path-based
// LoadSnapshot: snapshotRoot is the trusted root, each transaction directory
// is opened relative to it without following a symbolic link, and its
// metadata is read relative to that fd.
func PruneSnapshots(snapshotRoot string, now time.Time, maxAge time.Duration, activeID string) []string {
	rootFd, err := openTrustedRoot(snapshotRoot, false, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return []string{fmt.Sprintf("cannot list snapshots in %s: %v", snapshotRoot, err)}
	}
	defer rootFd.Close()

	names, readErr := rootFd.Readdirnames(-1)
	if readErr != nil {
		return []string{fmt.Sprintf("cannot list snapshots in %s: %v", snapshotRoot, readErr)}
	}

	type candidate struct {
		name string
		age  time.Time
	}
	var candidates []candidate
	var warnings []string
	for _, name := range names {
		if name == activeID {
			continue
		}
		var st unix.Stat_t
		if statErr := unix.Fstatat(int(rootFd.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); statErr != nil {
			continue // vanished between Readdirnames and here.
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			continue // not a directory: v0.7's e.IsDir() check skipped these too, silently.
		}
		dirFd, openErr := openChildDir(rootFd, name)
		if openErr != nil {
			// Keep anything this package cannot recognise: it is either a
			// damaged image worth investigating or not ours to delete.
			warnings = append(warnings, fmt.Sprintf("keeping unreadable snapshot %s: %v", name, openErr))
			continue
		}
		snap, loadErr := readSnapshotMetadata(dirFd)
		dirFd.Close()
		if loadErr != nil {
			warnings = append(warnings, fmt.Sprintf("keeping unreadable snapshot %s: %v", name, loadErr))
			continue
		}
		candidates = append(candidates, candidate{name: name, age: snap.CreatedAt})
	}
	if len(candidates) <= 1 {
		return warnings
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].age.After(candidates[j].age) })

	// candidates[0] is the newest and is always kept for investigation.
	for _, c := range candidates[1:] {
		if now.Sub(c.age) <= maxAge {
			continue
		}
		if err := prepareMutation([]*os.File{rootFd}, nil); err != nil {
			warnings = append(warnings, fmt.Sprintf("cannot prune stale snapshot %s: %v", c.name, err))
			continue
		}
		if err := removeTreeFd(rootFd, c.name); err != nil {
			warnings = append(warnings, fmt.Sprintf("cannot prune stale snapshot %s: %v", c.name, err))
		}
	}
	return warnings
}

func newTransactionID(createdAt time.Time) (string, error) {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("backup: generate transaction id: %w", err)
	}
	return fmt.Sprintf("%s-%s", createdAt.Format(archiveTimeLayout), hex.EncodeToString(buf[:])), nil
}
