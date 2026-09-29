// Package realitynode: export.go implements design spec §8.5's
// `uninstall --purge --export-backup PATH`: the tar.gz archive of the
// instance's state and configuration directories, written so that it can
// never replace a file the user already has and never leaves a temp file
// behind on failure or cancellation.
//
// Protocol (all of it inside the directory that will hold PATH):
//
//  1. PATH must not exist (Fstatat with AT_SYMLINK_NOFOLLOW, so a dangling
//     symlink also counts as taken).
//  2. A temp file is created with O_EXCL (mode 0600) under a random name.
//  3. The archive is written and fsynced.
//  4. The temp file is hard-linked to PATH — link(2) fails with EEXIST rather
//     than replacing an existing file, which closes the window between step 1
//     and here — and only then is the temp name unlinked.
//
// Every failure or cancellation before the link removes the temp file; the
// caller (purge) then starts no transaction. The archive is the only durable
// copy of the private key a purge leaves, and it is explicitly the user's to
// keep: SECURITY.md documents that it holds the private key, and
// docs/reality-node.md that there is no import command.
package realitynode

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// exportHook is the process-global test seam of the export, shaped like
// lifecycleHook: called before each named step; an error it returns is that
// step failing (the real operation is not performed). Points:
//
//	export:create-temp   before the O_EXCL temp file is created
//	export:write         before each archive member is written
//	export:fsync         before the archive file is fsynced
//	export:link          before the temp file is linked to PATH
//	export:unlink-temp   before the temp name is removed after a successful link
//	export:fsync-dir     before the directory is fsynced after the link
//
// Tests overriding it must not use t.Parallel.
var exportHook = func(point string) error { return nil }

const exportTempPrefix = ".proxyctl-export-"

// ValidateExportTarget is the part of --export-backup's checks that needs no
// file to be written: PATH is a non-empty absolute path, is not inside a
// directory the purge itself deletes (the archive would be deleted with it),
// its directory exists, and PATH itself does not exist yet. It is used both
// by the pre-lock precheck (fail fast, exit 1) and by ExportBackup.
func ValidateExportTarget(env *Env, dest string) error {
	if dest == "" {
		return errors.New("realitynode: --export-backup needs a non-empty path")
	}
	if !filepath.IsAbs(dest) {
		return fmt.Errorf("realitynode: --export-backup path %q must be absolute", dest)
	}
	clean := filepath.Clean(dest)
	for _, purged := range []string{env.Roots.StateDir, env.Roots.ConfigDir, env.Roots.BinDir, env.Roots.TxnDir} {
		if pathWithin(clean, purged) {
			return fmt.Errorf("realitynode: --export-backup path %s is inside %s, which the purge deletes", dest, purged)
		}
	}
	dir := filepath.Dir(clean)
	// Also refuse a directory that only reaches a purged one through a
	// symlink: compare the resolved directory with the resolved purged roots.
	if resolvedDir, err := filepath.EvalSymlinks(dir); err == nil {
		for _, purged := range []string{env.Roots.StateDir, env.Roots.ConfigDir, env.Roots.BinDir, env.Roots.TxnDir} {
			if resolved, rerr := filepath.EvalSymlinks(purged); rerr == nil && pathWithin(resolvedDir, resolved) {
				return fmt.Errorf("realitynode: --export-backup path %s resolves inside %s, which the purge deletes", dest, purged)
			}
		}
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("realitynode: --export-backup directory %s: %w", dir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("realitynode: --export-backup directory %s is not a directory", dir)
	}
	if _, err := os.Lstat(clean); err == nil {
		return fmt.Errorf("realitynode: --export-backup path %s already exists; refusing to overwrite it", dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("realitynode: --export-backup path %s: %w", dest, err)
	}
	return nil
}

func pathWithin(path, dir string) bool {
	path, dir = filepath.Clean(path), filepath.Clean(dir)
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// ExportBackup writes the tar.gz archive of Roots.StateDir and
// Roots.ConfigDir (whichever exist) to dest, mode 0600, following the
// protocol in this file's header. On any error or cancellation it removes its
// temp file and returns a non-nil error; dest is then untouched (the one
// exception is documented at the directory fsync). The caller holds L3 and
// has already rechecked the instance state, so the two directories cannot
// change underneath the read.
func ExportBackup(ctx context.Context, env *Env, dest string) (err error) {
	if err := ValidateExportTarget(env, dest); err != nil {
		return err
	}
	dest = filepath.Clean(dest)
	dir, base := filepath.Dir(dest), filepath.Base(dest)

	dfd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("realitynode: open export directory %s: %w", dir, err)
	}
	defer unix.Close(dfd)

	var st unix.Stat_t
	if serr := unix.Fstatat(dfd, base, &st, unix.AT_SYMLINK_NOFOLLOW); serr == nil {
		return fmt.Errorf("realitynode: --export-backup path %s already exists; refusing to overwrite it", dest)
	} else if !errors.Is(serr, unix.ENOENT) {
		return fmt.Errorf("realitynode: --export-backup path %s: %w", dest, serr)
	}

	if err := exportHook("export:create-temp"); err != nil {
		return err
	}
	tmpName, f, err := createExportTemp(env, dfd)
	if err != nil {
		return err
	}
	tempExists := true
	defer func() {
		if f != nil {
			f.Close()
		}
		if tempExists {
			if uerr := unix.Unlinkat(dfd, tmpName, 0); uerr != nil && !errors.Is(uerr, unix.ENOENT) {
				err = fmt.Errorf("%w (and the temporary file %s could not be removed: %v)", err, filepath.Join(dir, tmpName), uerr)
			}
		}
	}()

	if err := writeExportArchive(ctx, env, f); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := exportHook("export:fsync"); err != nil {
		return fmt.Errorf("realitynode: fsync %s: %w", tmpName, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("realitynode: fsync %s: %w", tmpName, err)
	}
	if err := f.Close(); err != nil {
		f = nil
		return fmt.Errorf("realitynode: close %s: %w", tmpName, err)
	}
	f = nil

	if err := exportHook("export:link"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// link(2) does not replace an existing name: a file that appeared at
	// PATH since the check above makes this fail with EEXIST.
	if err := unix.Linkat(dfd, tmpName, dfd, base, 0); err != nil {
		return fmt.Errorf("realitynode: link the archive to %s: %w", dest, err)
	}
	// The archive now exists at dest. From here the temp name is only a
	// second name for the same inode.
	if err := exportHook("export:unlink-temp"); err != nil {
		return fmt.Errorf("realitynode: archive written to %s, but %w", dest, err)
	}
	if uerr := unix.Unlinkat(dfd, tmpName, 0); uerr != nil && !errors.Is(uerr, unix.ENOENT) {
		return fmt.Errorf("realitynode: archive written to %s, but removing the temporary name failed: %w", dest, uerr)
	}
	tempExists = false
	if err := exportHook("export:fsync-dir"); err != nil {
		return fmt.Errorf("realitynode: archive written to %s, but making its directory entry durable failed: %w", dest, err)
	}
	if err := unix.Fsync(dfd); err != nil {
		return fmt.Errorf("realitynode: archive written to %s, but fsync of %s failed: %w", dest, dir, err)
	}
	return nil
}

// createExportTemp creates the O_EXCL temp file (mode 0600, enforced with
// fchmod so a lax umask cannot widen it) inside the export directory.
func createExportTemp(env *Env, dfd int) (string, *os.File, error) {
	var lastErr error
	for i := 0; i < 8; i++ {
		var b [8]byte
		if _, err := io.ReadFull(env.randReader(), b[:]); err != nil {
			return "", nil, fmt.Errorf("realitynode: random temp name: %w", err)
		}
		name := exportTempPrefix + hex.EncodeToString(b[:]) + ".tmp"
		fd, err := unix.Openat(dfd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if err != nil {
			if errors.Is(err, unix.EEXIST) {
				lastErr = err
				continue
			}
			return "", nil, fmt.Errorf("realitynode: create temporary archive: %w", err)
		}
		f := os.NewFile(uintptr(fd), name)
		if err := f.Chmod(0600); err != nil {
			f.Close()
			_ = unix.Unlinkat(dfd, name, 0)
			return "", nil, fmt.Errorf("realitynode: chmod temporary archive: %w", err)
		}
		return name, f, nil
	}
	return "", nil, fmt.Errorf("realitynode: could not find an unused temporary name: %w", lastErr)
}

// writeExportArchive writes the gzip'd tar stream of the two directories to
// f. Member names are the absolute paths without the leading slash (so
// extracting under a scratch directory reproduces the original layout).
func writeExportArchive(ctx context.Context, env *Env, f *os.File) error {
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, root := range []string{env.Roots.StateDir, env.Roots.ConfigDir} {
		if err := ctx.Err(); err != nil {
			return err
		}
		fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue // e.g. an uninstalled instance has no config directory
			}
			return fmt.Errorf("realitynode: open %s for export: %w", root, err)
		}
		d := os.NewFile(uintptr(fd), root)
		err = addExportDir(ctx, tw, d, strings.TrimPrefix(filepath.ToSlash(filepath.Clean(root)), "/"))
		d.Close()
		if err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("realitynode: finish archive: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("realitynode: finish archive: %w", err)
	}
	return nil
}

// headerFor builds a tar header from an already-opened (O_NOFOLLOW) file's
// own fstat, never from a path lookup.
func headerFor(name string, fi os.FileInfo, typeflag byte) *tar.Header {
	h := &tar.Header{Name: name, Typeflag: typeflag, Mode: int64(fi.Mode().Perm()), ModTime: fi.ModTime()}
	if fi.Mode()&os.ModeSetuid != 0 {
		h.Mode |= 0o4000
	}
	if fi.Mode()&os.ModeSetgid != 0 {
		h.Mode |= 0o2000
	}
	if fi.Mode()&os.ModeSticky != 0 {
		h.Mode |= 0o1000
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		h.Uid, h.Gid = int(st.Uid), int(st.Gid)
	}
	return h
}

// addExportDir adds the directory d (already opened O_NOFOLLOW) and,
// recursively, everything in it. Only regular files and directories are
// accepted: any other type inside an instance directory is not something this
// version ever wrote, and is refused rather than archived.
func addExportDir(ctx context.Context, tw *tar.Writer, d *os.File, name string) error {
	fi, err := d.Stat()
	if err != nil {
		return fmt.Errorf("realitynode: stat %s: %w", d.Name(), err)
	}
	if err := exportHook("export:write"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := tw.WriteHeader(headerFor(name+"/", fi, tar.TypeDir)); err != nil {
		return fmt.Errorf("realitynode: write archive: %w", err)
	}
	names, err := readDirNames(d)
	if err != nil {
		return fmt.Errorf("realitynode: list %s: %w", d.Name(), err)
	}
	for _, n := range names {
		var est unix.Stat_t
		if err := unix.Fstatat(int(d.Fd()), n, &est, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("realitynode: stat %s/%s: %w", d.Name(), n, err)
		}
		switch est.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			cfd, err := unix.Openat(int(d.Fd()), n, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return fmt.Errorf("realitynode: open %s/%s: %w", d.Name(), n, err)
			}
			child := os.NewFile(uintptr(cfd), filepath.Join(d.Name(), n))
			err = addExportDir(ctx, tw, child, name+"/"+n)
			child.Close()
			if err != nil {
				return err
			}
		case unix.S_IFREG:
			if err := addExportFile(ctx, tw, d, n, name+"/"+n); err != nil {
				return err
			}
		default:
			return fmt.Errorf("realitynode: %s/%s is neither a regular file nor a directory; refusing to archive it", d.Name(), n)
		}
	}
	return nil
}

func addExportFile(ctx context.Context, tw *tar.Writer, dir *os.File, leaf, name string) error {
	fd, err := unix.Openat(int(dir.Fd()), leaf, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("realitynode: open %s/%s: %w", dir.Name(), leaf, err)
	}
	f := os.NewFile(uintptr(fd), leaf)
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("realitynode: stat %s/%s: %w", dir.Name(), leaf, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("realitynode: %s/%s changed type while exporting", dir.Name(), leaf)
	}
	if err := exportHook("export:write"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	h := headerFor(name, fi, tar.TypeReg)
	h.Size = fi.Size()
	if err := tw.WriteHeader(h); err != nil {
		return fmt.Errorf("realitynode: write archive: %w", err)
	}
	if _, err := io.CopyN(tw, f, fi.Size()); err != nil {
		return fmt.Errorf("realitynode: write %s to the archive: %w", name, err)
	}
	return nil
}

func readDirNames(d *os.File) ([]string, error) {
	fd, err := unix.Openat(int(d.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), d.Name())
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}
