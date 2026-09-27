package transaction

// Safe-path primitives for managed resources (design spec §7.1, "路径类型
//与符号链接" / "祖先链复验" / "目录身份的登记"). This is an independent
// reimplementation of the v0.7.1 openat-hardening pattern documented in
// docs/superpowers/specs/2026-09-12-v0.7.1-openat-path-hardening-design.md
// §6.2-§6.5 and already shipped, for internal/backup's own narrower
// purpose, in internal/backup/dirfd.go. It is not shared code: see
// SECURITY.md and the Task 4 plan note ("是否抽取最小共享代码，由本 Task 证
// 明必要性，且不复制其业务回滚协议") for why. The two packages solve a
// related but distinct problem — internal/backup re-verifies a chain of
// fds it already holds open for the duration of one call; this package
// re-verifies a chain against the journal's *persisted* dir_ids record,
// which must survive a crash and a fresh process (recover, Task 5), so the
// held-fd comparison backup uses is not applicable here at all. Sharing
// would mean exporting backup's unexported chain machinery, widening its
// public surface for a caller it was never designed for — the plan
// explicitly forbids changing backup's public semantics.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// ErrUnsafePath reports that a trusted root, or a directory structurally
// encountered while resolving a managed path, is not safe to use: not a
// directory, a symbolic link, or (for a trusted root) not owned by the
// expected user or writable by group/other.
var ErrUnsafePath = errors.New("transaction: unsafe path")

// ErrAncestorMismatch reports that a managed path's ancestor chain
// resolved to real, non-symlink directories, but at least one level's live
// identity does not match the journal's current dir_ids record for it —
// design §7.1's "祖先链复验" detecting a replacement after the fact (v0.7.1
// §3.3's ancestor-replacement threat, applied to journal-persisted state
// instead of a held fd).
var ErrAncestorMismatch = errors.New("transaction: ancestor directory identity does not match the journal")

// ErrIllegalPathType reports that a managed file's observed type is
// neither "regular file" nor "does not exist" — the only two states §7.1
// allows a managed file's pre/post state to hold.
var ErrIllegalPathType = errors.New("transaction: managed path has an illegal type")

// ErrPreStateMismatch reports that a managed path's live state, checked
// immediately before a primitive's modifying syscall, does not equal the
// step's recorded Pre — §7.4's "先校验" principle applied per forward step:
// a primitive must never modify, delete or read-as-trustworthy a target
// whose current state is not the one the plan recorded, since Pre is what
// any later rollback assumes it is restoring from. This is
// deliberately a different sentinel from ErrAncestorMismatch (an ancestor
// *directory*, not the target itself, disagreeing with dir_ids).
var ErrPreStateMismatch = errors.New("transaction: live state does not match the step's recorded pre-state")

// TrustedRoot is design §7.1's "可信根": an opened root directory verified
// to be owned by a specific user with no group or other write permission,
// and not itself a symbolic link. Every managed path this package resolves
// starts from one.
type TrustedRoot struct {
	f    *os.File
	path string
}

// Path returns the filesystem path this root was opened from, for error
// messages and logging only.
func (r *TrustedRoot) Path() string { return r.path }

// Close releases the root's fd. It does not remove anything on disk.
func (r *TrustedRoot) Close() error { return r.f.Close() }

// OpenTrustedRoot opens path as a trusted root (§7.1: "可信根:每类受管路径
// 都有一个root所有、其他用户不可写的可信根"). It never creates path — a
// transaction's trusted roots (/etc, /var/lib, ...) always already exist —
// and never follows a symbolic link at path itself. wantOwner is the uid
// the root must be owned by; tests inject the current process's own uid,
// since a non-root test cannot chown a fixture to a different owner.
func OpenTrustedRoot(path string, wantOwner int) (*TrustedRoot, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		// Linux may report either ELOOP or ENOTDIR for a symlink here
		// (design v0.7.1 §6.4); classify by an explicit Lstat rather than
		// trusting which errno came back.
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			if info, statErr := os.Lstat(path); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("%w: %s is a symbolic link", ErrUnsafePath, path)
			}
			return nil, fmt.Errorf("%w: %s is not a directory", ErrUnsafePath, path)
		}
		return nil, fmt.Errorf("transaction: open trusted root %s: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		f.Close()
		return nil, fmt.Errorf("transaction: stat trusted root %s: %w", path, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		f.Close()
		return nil, fmt.Errorf("%w: %s is not a directory", ErrUnsafePath, path)
	}
	if int(st.Uid) != wantOwner {
		f.Close()
		return nil, fmt.Errorf("%w: %s is owned by uid %d, want %d", ErrUnsafePath, path, st.Uid, wantOwner)
	}
	if st.Mode&(unix.S_IWGRP|unix.S_IWOTH) != 0 {
		f.Close()
		return nil, fmt.Errorf("%w: %s is writable by group or other", ErrUnsafePath, path)
	}
	return &TrustedRoot{f: f, path: path}, nil
}

// openChildDir opens the directory named name directly under parent,
// without following a symbolic link. A missing name wraps os.ErrNotExist;
// every other failure is classified by classifyDirOpenError.
func openChildDir(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, fmt.Errorf("%s: %w", name, os.ErrNotExist)
		}
		return nil, classifyDirOpenError(parent, name, err)
	}
	return os.NewFile(uintptr(fd), name), nil
}

// errNotADirectory is an internal sentinel for "an ordinary file, FIFO,
// socket or device stands where a directory was expected" — distinct from
// ErrUnsafePath (a symlink, or a directory that changed identity), mirrored
// from the v0.7.1 pattern (design §6.4).
var errNotADirectory = errors.New("transaction: not a directory")

// classifyDirOpenError turns a failed, non-ENOENT attempt to open name as
// a directory under parent into design §6.4's classification (as applied
// by v0.7.1 to internal/backup, and identically applicable here): a
// symbolic link, or a directory that changed underneath the two syscalls,
// is ErrUnsafePath; an ordinary file, FIFO, socket or device is
// errNotADirectory; anything else is a plain I/O error. Linux may report
// either ELOOP or ENOTDIR for a symlink parent, so the classification does
// not depend on which errno the open itself returned.
func classifyDirOpenError(parent *os.File, name string, openErr error) error {
	if !errors.Is(openErr, unix.ELOOP) && !errors.Is(openErr, unix.ENOTDIR) {
		return fmt.Errorf("transaction: open %s: %w", name, openErr)
	}
	var st unix.Stat_t
	statErr := unix.Fstatat(int(parent.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case errors.Is(statErr, unix.ENOENT):
		return fmt.Errorf("%w: %s changed while it was being opened", ErrUnsafePath, name)
	case statErr != nil:
		return fmt.Errorf("transaction: inspect %s: %w", name, statErr)
	case st.Mode&unix.S_IFMT == unix.S_IFLNK:
		return fmt.Errorf("%w: %s is a symbolic link", ErrUnsafePath, name)
	case st.Mode&unix.S_IFMT == unix.S_IFDIR:
		return fmt.Errorf("%w: %s changed while it was being opened", ErrUnsafePath, name)
	default:
		return fmt.Errorf("%w: %s", errNotADirectory, name)
	}
}

// identityOf returns the (dev, ino) identity of an open fd, as recorded in
// DirIdentity / compared against it.
func identityOf(fd int) (DirIdentity, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return DirIdentity{}, fmt.Errorf("transaction: stat: %w", err)
	}
	return DirIdentity{Exists: true, Dev: uint64(st.Dev), Ino: uint64(st.Ino)}, nil
}

// ManagedPath names one resource this package operates on: a trusted root
// identified by RootKey (the dir_ids/Target key naming the root itself —
// e.g. "etc"), the ordered chain of directory components leading to it
// (Comps), the journal dir_ids keys naming each of those ancestor levels
// (Keys, same length as Comps — Keys[i] is the key for the directory
// reached after opening Comps[i]), and the final path component this
// operation actually creates, writes, or removes (Leaf). Leaf is
// deliberately not part of Comps/Keys: §7.1's dir_ids table only ever
// records *directory* identities, and Leaf may name a file.
//
// Keys and the Step.Target an engine method operates against are never
// taken on faith: validate (called by every Engine method that accepts a
// ManagedPath, via requireStep) recomputes both AncestorKey and TargetKey
// from RootKey+Comps+Leaf and rejects a ManagedPath whose caller-supplied
// Keys, or whose caller-supplied step's Target, disagree with that
// computation — so "the path being safety-checked" and "the
// path actually mutated" can never diverge by a caller's mistake, and a
// journal's Target string is meaningful independent of whichever
// in-memory ManagedPath a particular call happens to build.
type ManagedPath struct {
	Root    *TrustedRoot
	RootKey string
	Comps   []string
	Keys    []string
	Leaf    string
}

// AncestorKey returns the dir_ids key for the directory reached after
// opening mp.Comps[:i+1] — the same deterministic format mp.Keys[i] must
// equal.
func (mp ManagedPath) AncestorKey(i int) string {
	parts := append([]string{mp.RootKey}, mp.Comps[:i+1]...)
	return strings.Join(parts, "/")
}

// TargetKey returns the deterministic key for mp's own leaf — the value a
// Step.Target naming this ManagedPath must equal.
func (mp ManagedPath) TargetKey() string {
	parts := append(append([]string{mp.RootKey}, mp.Comps...), mp.Leaf)
	return strings.Join(parts, "/")
}

// validComponent rejects an empty component, "." or "..", and one
// containing a "/" or a NUL byte — the components openat(2) would
// otherwise treat specially (openat(fd, "..", ...) escapes fd's own
// directory entirely, openat(fd, "a/b", ...) resolves through more than
// one directory) rather than as a single, safety-checked path element.
func validComponent(c string) error {
	if c == "" || c == "." || c == ".." || strings.ContainsRune(c, '/') || strings.ContainsRune(c, 0) {
		return fmt.Errorf("%w: illegal path component %q", ErrUnsafePath, c)
	}
	return nil
}

func (mp ManagedPath) validate() error {
	if mp.Root == nil {
		return fmt.Errorf("transaction: ManagedPath has no Root")
	}
	if mp.RootKey == "" {
		return fmt.Errorf("transaction: ManagedPath has no RootKey")
	}
	if len(mp.Comps) != len(mp.Keys) {
		return fmt.Errorf("transaction: ManagedPath has %d path components but %d dir_ids keys", len(mp.Comps), len(mp.Keys))
	}
	for i, c := range mp.Comps {
		if err := validComponent(c); err != nil {
			return err
		}
		if want := mp.AncestorKey(i); mp.Keys[i] != want {
			return fmt.Errorf("transaction: ManagedPath.Keys[%d] = %q, want %q (computed from RootKey/Comps)", i, mp.Keys[i], want)
		}
	}
	if err := validComponent(mp.Leaf); err != nil {
		return err
	}
	return nil
}

// resolveVerifiedParent re-opens, fresh from mp.Root, every ancestor
// directory in mp.Comps in order, and compares each level's live identity
// against dirIDs's current record for the corresponding mp.Keys entry —
// design §7.1's "每次修改前...从可信根重新逐级打开并复验" performed anew for
// this single call, exactly as the spec requires for every one of
// "创建临时文件、rename、unlink、mkdir、rmdir、fchmod、fchown". It returns
// the fully resolved parent directory (mp.Root's own fd when mp.Comps is
// empty) that the caller performs its own single mutating syscall against
// immediately afterward, plus every fd this call itself opened (for the
// caller to close — mp.Root is never included, since resolveVerifiedParent
// never owns it).
//
// A recorded identity of "exists" requires the live directory to exist and
// match; a recorded identity of "does not exist", or no recorded entry at
// all, requires the live level to not exist. There is no exception for
// "the level this call is about to create": a caller creating a new
// directory always passes only that directory's *ancestors* in mp.Comps,
// never the directory being created itself (§7.1's parenthetical is this
// same point, stated from the step-sequencing side rather than the
// verification side — see steps.go's Mkdir).
func resolveVerifiedParent(mp ManagedPath, dirIDs map[string]DirIdentity, afterOpen func(level int)) (parent *os.File, opened []*os.File, err error) {
	if err := mp.validate(); err != nil {
		return nil, nil, err
	}
	if afterOpen == nil {
		afterOpen = func(int) {}
	}
	parent = mp.Root.f
	for i, name := range mp.Comps {
		child, openErr := openChildDir(parent, name)
		if openErr == nil {
			afterOpen(i)
		}
		recorded, ok := dirIDs[mp.Keys[i]]
		if openErr != nil {
			if errors.Is(openErr, os.ErrNotExist) {
				if ok && recorded.Exists {
					closeAll(opened)
					return nil, nil, fmt.Errorf("%w: %s is recorded as existing but is missing", ErrAncestorMismatch, mp.Keys[i])
				}
				closeAll(opened)
				return nil, nil, fmt.Errorf("%s: %w", name, os.ErrNotExist)
			}
			closeAll(opened)
			return nil, nil, openErr
		}
		live, idErr := identityOf(int(child.Fd()))
		if idErr != nil {
			child.Close()
			closeAll(opened)
			return nil, nil, idErr
		}
		if !ok || !recorded.Exists {
			child.Close()
			closeAll(opened)
			return nil, nil, fmt.Errorf("%w: %s is recorded as not existing but a directory is present", ErrAncestorMismatch, mp.Keys[i])
		}
		if recorded.Dev != live.Dev || recorded.Ino != live.Ino {
			child.Close()
			closeAll(opened)
			return nil, nil, fmt.Errorf("%w: %s no longer matches the journal's recorded identity", ErrAncestorMismatch, mp.Keys[i])
		}
		opened = append(opened, child)
		parent = child
	}
	return parent, opened, nil
}

func closeAll(files []*os.File) {
	for i := len(files) - 1; i >= 0; i-- {
		files[i].Close()
	}
}

// checkCancel reports ctx's error, if any — called immediately before every
// modifying syscall (plan G7: "在每个修改性系统调用之前检查取消").
func checkCancel(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// openLeafNoFollow opens name directly under parent (O_NOFOLLOW, so a
// symlink at name fails with ELOOP rather than being followed) with
// O_NONBLOCK, so a FIFO at name — which a plain blocking open would wait
// forever for a writer on, hanging an operation that holds a lock and
// cannot be canceled mid-syscall — returns immediately
// instead. It never assumes the entry's type from a separate, by-name
// stat: the fd it returns is fstat'd by the caller, which is the only
// stat result ever trusted for a decision, since it names the exact
// object this call opened rather than whatever a second, independent
// name resolution might find.
func openLeafNoFollow(parent *os.File, name string) (*os.File, unix.Stat_t, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, unix.Stat_t{}, err
	}
	f := os.NewFile(uintptr(fd), name)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		f.Close()
		return nil, unix.Stat_t{}, fmt.Errorf("transaction: stat %s: %w", name, err)
	}
	return f, st, nil
}

// openLeafFile opens name directly under parent and, when it exists,
// returns both its fd (still open, positioned at the start, for the
// caller to read exactly once — for a content hash, a dynamic-state
// schema detection, or a snapshot copy) and its PathState with every
// field statting alone can supply (Exists, Type, Size, Mode, UID, GID)
// already filled in; SHA256 and Schema are left zero for the caller to
// fill from that same fd's content, since which one (if either) applies
// depends on what the caller is comparing against (want.SHA256 vs
// want.Schema — see steps.go's computeContentField). A missing name
// reports (nil, PathState{Exists: false}, nil) — not an error — so a
// caller can tell "does not exist" apart from a real failure without a
// type assertion. Any type other than a regular file is ErrIllegalPathType
// (§7.1: "受管文件的前状态只允许两种:普通文件,或不存在").
func openLeafFile(parent *os.File, name string) (*os.File, PathState, error) {
	f, st, err := openLeafNoFollow(parent, name)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, PathState{Exists: false}, nil
		}
		if errors.Is(err, unix.ELOOP) {
			return nil, PathState{}, fmt.Errorf("%w: %s is a symbolic link", ErrIllegalPathType, name)
		}
		return nil, PathState{}, fmt.Errorf("transaction: open %s: %w", name, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		f.Close()
		return nil, PathState{}, fmt.Errorf("%w: %s is not a regular file", ErrIllegalPathType, name)
	}
	return f, PathState{
		Exists: true,
		Type:   PathTypeFile,
		Size:   st.Size,
		Mode:   uint32(st.Mode) & 07777,
		UID:    st.Uid,
		GID:    st.Gid,
	}, nil
}

// statLeaf inspects name directly under parent, returning the PathState
// §7.1 allows a managed file to hold: "does not exist", or a regular
// file's existence/size/mode/ownership/content-hash. Any other type
// (symlink, directory, FIFO, socket, device, ...) is ErrIllegalPathType.
// withHash controls whether the file's SHA256 is computed (a caller
// comparing against a Pre/Post that only records a dynamic-state Schema
// name, not a hash, has no use for it — and has no generic way to fill it
// in here anyway, since schema detection needs an Engine's injected
// detector function; see steps.go's (*Engine).verifyPreState instead).
// Callers that need a directory's own PathState (mkdir/remove_dir/
// set_meta acting on a directory) use statLeafDir instead.
func statLeaf(parent *os.File, name string, withHash bool) (PathState, error) {
	f, ps, err := openLeafFile(parent, name)
	if err != nil {
		return PathState{}, err
	}
	if f == nil {
		return ps, nil
	}
	defer f.Close()
	if withHash {
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return PathState{}, fmt.Errorf("transaction: hash %s: %w", name, err)
		}
		ps.SHA256 = hex.EncodeToString(h.Sum(nil))
	}
	return ps, nil
}

// statLeafDir inspects a directory named name directly under parent
// (O_NOFOLLOW|O_DIRECTORY, so anything other than a real directory fails
// the open itself rather than being classified from a second, by-name
// stat), returning its PathState (Type=dir) or Exists=false; any other
// type is ErrIllegalPathType.
func statLeafDir(parent *os.File, name string) (PathState, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return PathState{Exists: false}, nil
		}
		return PathState{}, fmt.Errorf("%w: %s could not be opened as a directory: %v", ErrIllegalPathType, name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return PathState{}, fmt.Errorf("transaction: stat %s: %w", name, err)
	}
	return PathState{
		Exists: true,
		Type:   PathTypeDir,
		Mode:   uint32(st.Mode) & 07777,
		UID:    st.Uid,
		GID:    st.Gid,
	}, nil
}

// mkdirManaged creates a directory named dirName directly under root
// (§7.1's transaction-area mkdir, and steps.go's Mkdir primitive when
// Comps is empty): design's fixed sequence for "本次新建" — Mkdirat, then
// Openat(O_NOFOLLOW|O_DIRECTORY) to obtain its own fd, then Fchmod that fd
// (never by name — see the v0.7.1 rationale this package's header
// references). It does not accept an already-existing entry: the caller
// (transaction start, or a mkdir step whose pre-state is "does not exist")
// always expects to be the one creating it.
//
// Mkdirat's own mode argument is not umask-adjusted here:
// the explicit Fchmod on the opened fd right below already makes the
// result independent of the ambient umask, and unix.Umask is process-wide
// mutable state that would otherwise race every other goroutine's
// concurrent file/directory creation for no benefit. This assumes the
// process's umask never strips the owner bits our own next Openat needs
// (true of every umask this project's deployments or tests use — 002,
// 022, 077 all leave owner rwx alone); a pathological umask stripping
// owner permissions is out of scope.
func mkdirManaged(root *TrustedRoot, dirName string, mode fs.FileMode) (*os.File, error) {
	if mkErr := unix.Mkdirat(int(root.f.Fd()), dirName, uint32(mode.Perm())); mkErr != nil {
		return nil, fmt.Errorf("transaction: mkdir %s: %w", dirName, mkErr)
	}
	fd, err := openChildDir(root.f, dirName)
	if err != nil {
		return nil, fmt.Errorf("transaction: %s needs manual confirmation after creation: %w", dirName, err)
	}
	if err := fd.Chmod(mode); err != nil {
		fd.Close()
		return nil, fmt.Errorf("transaction: set mode on %s: %w", dirName, err)
	}
	return fd, nil
}

// scanUnregisteredTemps lists every entry directly under dir whose name
// matches the managed temp-name format ".proxyctl-txn-*" (§7.3) but is not
// present in registered. It never follows symbolic links and never
// descends into subdirectories — the format is only ever used for a flat
// temp name in the same directory as its eventual target.
func scanUnregisteredTemps(dir *os.File, registered map[string]bool) ([]string, error) {
	// os.File.ReadDir seeks from the file's current position; a dedicated
	// fd is used so a caller's own use of dir's position (if any) is never
	// disturbed.
	fd, err := unix.Openat(int(dir.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("transaction: reopen directory for scan: %w", err)
	}
	f := os.NewFile(uintptr(fd), dir.Name())
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("transaction: list directory: %w", err)
	}
	var out []string
	for _, name := range names {
		if !strings.HasPrefix(name, ".proxyctl-txn-") {
			continue
		}
		if registered[name] {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

// TempName returns the fixed format §7.3 requires for every temp name a
// transaction creates in a managed path's own directory:
// ".proxyctl-txn-<txn_id>-<seq>".
func TempName(txnID string, seq int) string {
	return fmt.Sprintf(".proxyctl-txn-%s-%d", txnID, seq)
}
