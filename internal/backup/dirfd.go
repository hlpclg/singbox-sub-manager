package backup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// Test seams for the TOCTOU-sensitive windows described in design §6.5 and
// §6.7 of
// docs/superpowers/specs/2026-09-12-v0.7.1-openat-path-hardening-design.md.
// Production builds leave every hook as a no-op; a test replaces exactly one
// at a time, in a deferred-restored assignment, to run a filesystem mutation
// at a precise point in the sequence below. This package's tests must not
// use t.Parallel: the hooks, like the v0.7 seams they follow
// (publishFileFn, writeArchiveFn), are process-global state.
//
//	open the full parent directory chain
//	  -> hookAfterOpen            (about to re-verify the ancestor chain)
//	  -> ancestor-chain re-verification
//	  -> hookBeforeMutation       (re-verification passed; about to mutate/read)
//	  -> the mutating or read syscall
//
// Creating a directory runs the sequence above twice (design §6.5): once for
// Mkdirat itself, against the chain already open so far, then again —
// against that same chain extended with the newly opened directory — right
// before Fchmod:
//
//	[the sequence above] -> Mkdirat -> hookAfterMkdirat -> Openat(O_NOFOLLOW)
//	  -> [the sequence above again, chain now including the new directory]
//	  -> Fchmod
var (
	hookAfterOpen      = func() {}
	hookBeforeMutation = func() {}
	hookAfterMkdirat   = func() {}
)

// dirPolicy controls how resolveOneComponent treats a directory component
// that already exists, per design §6.3's table.
type dirPolicy int

const (
	// dirAllowExisting keeps using an existing directory as-is: it is opened
	// but never Fchmod'd, and is not reported as created.
	dirAllowExisting dirPolicy = iota
	// dirMustCreate requires this call to be the one that creates the
	// directory. An entry already there — whether it predates this call or
	// races into existence right before this call's own Mkdirat — is left
	// completely untouched and reported as an error.
	dirMustCreate
)

// openTrustedRoot opens path as a trusted root (design §6.2): resolved by
// following symbolic links, exactly like v0.7. When create is true, the path
// is created first with os.MkdirAll (also following symlinks). When mode is
// non-zero, the opened root's fd — not its name — is Fchmod'd to mode.
func openTrustedRoot(path string, create bool, mode fs.FileMode) (*os.File, error) {
	if create {
		if err := os.MkdirAll(path, mode); err != nil {
			return nil, fmt.Errorf("backup: create %s: %w", path, err)
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("backup: open %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("backup: stat %s: %w", path, err)
	}
	if !info.IsDir() {
		f.Close()
		return nil, fmt.Errorf("backup: %s is not a directory", path)
	}
	if mode != 0 {
		if err := f.Chmod(mode); err != nil {
			f.Close()
			return nil, fmt.Errorf("backup: set mode on %s: %w", path, err)
		}
	}
	return f, nil
}

// openChildDir opens the directory named name directly under parent, without
// following a symbolic link. A missing name is reported as an error wrapping
// os.ErrNotExist; every other failure is classified per design §6.4.
func openChildDir(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, fmt.Errorf("backup: %s: %w", name, os.ErrNotExist)
		}
		return nil, classifyDirOpenError(parent, name, err)
	}
	return os.NewFile(uintptr(fd), name), nil
}

// classifyDirOpenError turns a failed, non-ENOENT attempt to open name as a
// directory under parent into the error design §6.4 requires: a symbolic
// link, or a directory that changed underneath the two syscalls, is
// ErrUnsafePath; an ordinary file, FIFO, socket or device is the v0.7 "not a
// directory" class of error; anything else is a plain I/O error. The
// classification does not depend on which errno the open itself returned for
// a symlink parent (Linux may report either ELOOP or ENOTDIR).
func classifyDirOpenError(parent *os.File, name string, openErr error) error {
	if !errors.Is(openErr, unix.ELOOP) && !errors.Is(openErr, unix.ENOTDIR) {
		return fmt.Errorf("backup: open %s: %w", name, openErr)
	}
	var st unix.Stat_t
	statErr := unix.Fstatat(int(parent.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case errors.Is(statErr, unix.ENOENT):
		return fmt.Errorf("%w: %s changed while it was being opened", ErrUnsafePath, name)
	case statErr != nil:
		return fmt.Errorf("backup: inspect %s: %w", name, statErr)
	case st.Mode&unix.S_IFMT == unix.S_IFLNK:
		return fmt.Errorf("%w: %s is a symbolic link", ErrUnsafePath, name)
	case st.Mode&unix.S_IFMT == unix.S_IFDIR:
		return fmt.Errorf("%w: %s changed while it was being opened", ErrUnsafePath, name)
	default:
		return fmt.Errorf("backup: %s is not a directory", name)
	}
}

// sameDirEntry reports whether a and b are the same inode on the same
// device — the (st_dev, st_ino) identity comparison design §6.5 requires.
func sameDirEntry(a, b *os.File) (bool, error) {
	var sa, sb unix.Stat_t
	if err := unix.Fstat(int(a.Fd()), &sa); err != nil {
		return false, fmt.Errorf("backup: stat: %w", err)
	}
	if err := unix.Fstat(int(b.Fd()), &sb); err != nil {
		return false, fmt.Errorf("backup: stat: %w", err)
	}
	return sa.Dev == sb.Dev && sa.Ino == sb.Ino, nil
}

// verifyChain re-verifies, per design §6.5, that every directory fd held in
// chain[1:] still matches what a fresh walk from chain[0] (the trusted root)
// finds under the same names. This is a detection, not a guarantee: it can
// only see a change that happened before it runs and is still observable
// when it runs (design §3.3).
func verifyChain(chain []*os.File, names []string) error {
	parent := chain[0]
	for i, name := range names {
		// Every component here was already proven to be a directory when
		// its fd was first opened (chain[i+1]). Any problem re-opening or
		// re-stat'ing it now — missing, turned into a symlink or a plain
		// file, or any other failure — means it no longer matches the
		// directory this call is holding open, which design §6.5 treats as
		// a single case: ErrUnsafePath. This is deliberately coarser than
		// design §6.4's classification, which applies only to a path's
		// first resolution, not to re-verifying an already-open chain.
		fresh, err := openChildDir(parent, name)
		if err != nil {
			return fmt.Errorf("%w: %s could not be re-verified: %v", ErrUnsafePath, name, err)
		}
		match, statErr := sameDirEntry(fresh, chain[i+1])
		fresh.Close()
		if statErr != nil {
			return fmt.Errorf("%w: %s could not be re-verified: %v", ErrUnsafePath, name, statErr)
		}
		if !match {
			return fmt.Errorf("%w: %s no longer matches the previously opened directory", ErrUnsafePath, name)
		}
		parent = chain[i+1]
	}
	return nil
}

// prepareMutation is design §6.5's per-operation sequence, minus the actual
// mutating or read syscall: hookAfterOpen, an ancestor-chain re-verification
// of chain against its root, then hookBeforeMutation. Call it immediately
// before performing exactly one Openat(O_CREAT), Renameat, Unlinkat,
// Mkdirat, Fchmod, or read-oriented Openat, with nothing else between this
// call returning and that syscall.
func prepareMutation(chain []*os.File, names []string) error {
	hookAfterOpen()
	if err := verifyChain(chain, names); err != nil {
		return err
	}
	hookBeforeMutation()
	return nil
}

// dirChain is the open directory fd chain built while resolving a logical
// path's parent components under a trusted root. fds[0] is the root, owned
// by the caller of resolveParentDirs; fds[i] for i>=1 is the directory
// opened or created for comps[i-1].
type dirChain struct {
	fds     []*os.File
	comps   []string
	created []bool // len(created) == len(comps); created[i] describes fds[i+1].
}

// leaf returns the fd of the last resolved directory: the parent the caller
// operates the leaf name against.
func (c *dirChain) leaf() *os.File { return c.fds[len(c.fds)-1] }

// closeOpened closes every fd this chain opened, excluding the root, which
// resolveParentDirs's caller still owns.
func (c *dirChain) closeOpened() {
	for i := len(c.fds) - 1; i >= 1; i-- {
		c.fds[i].Close()
	}
}

// resolveOneComponent opens or creates the single directory name, the
// (i+1)th component of comps (comps[:i] having already been resolved into
// chain), following design §6.3.
//
// When create is false, this never writes anything: it only opens name,
// reporting a missing entry as an error wrapping os.ErrNotExist. When create
// is true, Mkdirat is always the first attempt — "本次新建" is determined
// solely by whether that call itself reports success (design §6.3): an
// EEXIST result means some directory entry was already there (predating this
// call, or racing its Mkdirat), which dirAllowExisting opens and uses as-is
// and dirMustCreate instead reports as an error, touching nothing.
func resolveOneComponent(chain []*os.File, comps []string, i int, create bool, mode fs.FileMode, policy dirPolicy) (fd *os.File, created bool, err error) {
	parent := chain[len(chain)-1]
	name := comps[i]

	if !create {
		fd, err := openChildDir(parent, name)
		return fd, false, err
	}

	// Design §6.5 lists Mkdirat itself among the mutating syscalls that must
	// be preceded by hookAfterOpen -> re-verify -> hookBeforeMutation,
	// checked against the chain already open so far (comps[:i]): without
	// this, a directory this call itself creates under an ancestor that was
	// silently moved out of the trusted root just before would land outside
	// it, undetected.
	if err := prepareMutation(chain, comps[:i]); err != nil {
		return nil, false, err
	}
	if mkErr := unix.Mkdirat(int(parent.Fd()), name, uint32(mode.Perm())); mkErr != nil {
		if !errors.Is(mkErr, unix.EEXIST) {
			return nil, false, fmt.Errorf("backup: create %s: %w", name, mkErr)
		}
		// Design §6.3: "Mkdirat 返回 EEXIST 一律视为既有目录项"; open and
		// classify it like any other existing entry, without Fchmod or
		// recording it for cleanup.
		if policy == dirMustCreate {
			return nil, false, fmt.Errorf("backup: %s already exists", name)
		}
		existing, openErr := openChildDir(parent, name)
		return existing, false, openErr
	}
	hookAfterMkdirat()

	newFd, openErr := openChildDir(parent, name)
	if openErr != nil {
		// Design §6.3: the name may no longer be the directory this call
		// just created. Do not clean it up by name; the caller must be told
		// it needs manual confirmation.
		return nil, false, fmt.Errorf("backup: %s needs manual confirmation after creation: %w", name, openErr)
	}
	extChain := append(append([]*os.File{}, chain...), newFd)
	if err := prepareMutation(extChain, comps[:i+1]); err != nil {
		newFd.Close()
		return nil, false, err
	}
	if err := newFd.Chmod(mode); err != nil {
		newFd.Close()
		return nil, false, fmt.Errorf("backup: set mode on %s: %w", name, err)
	}
	return newFd, true, nil
}

// resolveParentDirs opens, and when create is true creates as needed, the
// directory chain under root for every component in comps, in order. Each
// directory this call itself creates gets its own inline verify-then-Fchmod
// cycle (design §6.3, via resolveOneComponent); mode and policy apply only
// to a directory this call creates — an existing directory's permissions and
// ownership are left untouched. When create is false, a missing component is
// reported as an error wrapping os.ErrNotExist and nothing is created.
//
// The returned chain's own directory-creation steps are already verified,
// but the assembled chain as a whole is not re-verified again here: a
// caller about to perform its own leaf-level mutation or read must call
// prepareMutation(chain.fds, chain.comps) once more, immediately before that
// syscall, exactly as design §6.5 describes for a single operation.
//
// On error, the returned chain is never nil once at least the root has been
// opened by the caller — it is the (possibly partial) chain built so far,
// still open, with chain.created recording exactly which of its directories
// this call itself created before the failure. The caller owns it either
// way: it must always chain.closeOpened() it, and — because whether a
// newly-created directory should be deleted on failure is a decision that
// belongs to the caller, not this shared primitive (design §6.3's table:
// Restore cleans them up, Rollback does not) — it is the caller's
// responsibility to remove any directories in chain.created it decides not
// to keep, before closing.
func resolveParentDirs(root *os.File, comps []string, create bool, mode fs.FileMode, policy dirPolicy) (*dirChain, error) {
	c := &dirChain{fds: []*os.File{root}}
	for i := range comps {
		fd, created, err := resolveOneComponent(c.fds, comps, i, create, mode, policy)
		if err != nil {
			return c, err
		}
		c.fds = append(c.fds, fd)
		c.comps = append(c.comps, comps[i])
		c.created = append(c.created, created)
	}
	return c, nil
}

// resolveUnderTrustedRoot is the fd-based replacement for the v0.7
// path-based resolveUnderRoot (design §6.6: that function and its
// path-based helpers are deleted once every caller has migrated). It
// validates logical exactly as ValidateLogicalPath always has, then walks
// every directory component of logical except the last under root via
// resolveParentDirs, creating missing parents when create is true. It
// returns the open parent chain and the logical path's final component —
// callers perform the actual create/replace/delete/read syscall themselves,
// relative to chain.leaf(), immediately after their own prepareMutation
// call.
//
// Same ownership contract as resolveParentDirs: a nil chain means nothing
// was opened at all (logical was rejected before any filesystem access); a
// non-nil chain, error or not, is the caller's to close and, on error, to
// decide what to do with chain.created.
func resolveUnderTrustedRoot(root *os.File, logical string, create bool, mode fs.FileMode, policy dirPolicy) (chain *dirChain, leaf string, err error) {
	if err := ValidateLogicalPath(logical); err != nil {
		return nil, "", err
	}
	comps := strings.Split(logical, "/")
	parents, leaf := comps[:len(comps)-1], comps[len(comps)-1]
	chain, err = resolveParentDirs(root, parents, create, mode, policy)
	if err != nil {
		return chain, "", err
	}
	return chain, leaf, nil
}
