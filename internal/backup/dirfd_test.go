package backup

// These tests drive the new fd-relative resolution capabilities directly:
// design §7.1's Task 1 rows (T3a, T3b, T4, T6, T8, the "allow existing"
// policy and the private-level ancestor-chain re-verification) plus R1-R3,
// the replacement for the deleted TestResolveUnderRoot_KeepsPathsInside
// (Global Constraints #9). Nothing here is wired into a public entry point
// yet — that happens in later Tasks.
//
// t.Parallel is never used in this file: hookAfterOpen, hookBeforeMutation
// and hookAfterMkdirat are process-global test seams.

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func openDirForTest(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	return f
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func permOf(t *testing.T, path string) fs.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

// withUmask installs a restrictive umask (0277: strips every group/other bit
// and the owner write bit, deliberately keeping owner read+execute so the
// directory this test just created remains one it can still open itself)
// for the rest of the test (restored via t.Cleanup), so that "the created
// directory ended up at exactly the requested mode" is not satisfied merely
// by Mkdirat's own mode argument already surviving the ambient umask
// unchanged. Fchmod, unlike Mkdirat, is never affected by umask, so this
// makes a mode assertion after creation actually exercise design §6.3's
// Fchmod-on-the-held-fd step — 0777 would strip owner execute too, and this
// package's own subsequent O_NOFOLLOW|O_DIRECTORY open of that directory
// (not just the assertion) would then fail EACCES for a non-root test
// runner, before Fchmod ever got a chance to correct the mode; 0277 sidesteps
// that without weakening what the assertion actually proves. Call it only
// after any setup the test itself needs (pre-existing directories, etc.) is
// already done — it affects every subsequent directory- and file-creation
// syscall in the process, and this package's tests never run with
// t.Parallel.
func withUmask(t *testing.T, mask int) {
	t.Helper()
	old := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(old) })
}

// --- openTrustedRoot (design §6.2) ---

func TestOpenTrustedRoot(t *testing.T) {
	t.Run("creates a missing path and Fchmods the fd, independent of umask", func(t *testing.T) {
		base := t.TempDir()
		path := filepath.Join(base, "root")
		withUmask(t, 0277)

		f, err := openTrustedRoot(path, true, 0700)
		if err != nil {
			t.Fatalf("openTrustedRoot: %v", err)
		}
		defer f.Close()
		if mode := permOf(t, path); mode != 0700 {
			t.Fatalf("root mode = %v, want 0700", mode)
		}
	})

	t.Run("follows a symlinked root, per design §3.1/§6.2", func(t *testing.T) {
		base := t.TempDir()
		real := filepath.Join(base, "real")
		mustMkdir(t, real)
		link := filepath.Join(base, "link")
		if err := os.Symlink(real, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		f, err := openTrustedRoot(link, false, 0)
		if err != nil {
			t.Fatalf("openTrustedRoot: %v", err)
		}
		defer f.Close()

		var gotSt, wantSt unix.Stat_t
		if err := unix.Fstat(int(f.Fd()), &gotSt); err != nil {
			t.Fatalf("fstat: %v", err)
		}
		if err := unix.Stat(real, &wantSt); err != nil {
			t.Fatalf("stat: %v", err)
		}
		if gotSt.Dev != wantSt.Dev || gotSt.Ino != wantSt.Ino {
			t.Fatalf("openTrustedRoot did not open the symlink's target")
		}
	})

	t.Run("rejects a non-directory path", func(t *testing.T) {
		base := t.TempDir()
		plain := filepath.Join(base, "plain")
		if err := os.WriteFile(plain, []byte("x"), 0600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := openTrustedRoot(plain, false, 0); err == nil {
			t.Fatalf("err = nil, want an error")
		}
	})
}

// --- T4: error classification (design §6.4) ---

func TestClassifyDirOpenError(t *testing.T) {
	root := t.TempDir()
	parent := openDirForTest(t, root)
	defer parent.Close()

	t.Run("a symlink parent is ErrUnsafePath (Linux observed errno recorded below)", func(t *testing.T) {
		outside := filepath.Join(root, "outside")
		mustMkdir(t, outside)
		link := filepath.Join(root, "link")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		_, err := openChildDir(parent, "link")
		if !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("err = %v, want ErrUnsafePath", err)
		}
		t.Logf("observed classification for a real symlink parent on this kernel: %v", err)
	})

	t.Run("a regular file parent is a non-safe, non-ErrUnsafePath error", func(t *testing.T) {
		f := filepath.Join(root, "plainfile")
		if err := os.WriteFile(f, []byte("x"), 0600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
		_, err := openChildDir(parent, "plainfile")
		if err == nil || errors.Is(err, ErrUnsafePath) {
			t.Fatalf("err = %v, want a non-ErrUnsafePath error", err)
		}
	})

	t.Run("ELOOP is classified the same as ENOTDIR for a symlink", func(t *testing.T) {
		outside := filepath.Join(root, "outside2")
		mustMkdir(t, outside)
		link := filepath.Join(root, "eloop-link")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		// Some kernels never return ELOOP for O_DIRECTORY|O_NOFOLLOW on a
		// symlink (this environment's did not); the errno is driven
		// directly here so both reported errnos are covered regardless of
		// what the current kernel happens to produce (design §7.1).
		err := classifyDirOpenError(parent, "eloop-link", unix.ELOOP)
		if !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("err = %v, want ErrUnsafePath", err)
		}
	})

	t.Run("Openat/Fstatat race: the entry became a directory", func(t *testing.T) {
		became := filepath.Join(root, "became-dir")
		mustMkdir(t, became)
		err := classifyDirOpenError(parent, "became-dir", unix.ENOTDIR)
		if !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("err = %v, want ErrUnsafePath", err)
		}
	})

	t.Run("Openat/Fstatat race: the entry disappeared", func(t *testing.T) {
		err := classifyDirOpenError(parent, "never-existed", unix.ENOTDIR)
		if !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("err = %v, want ErrUnsafePath", err)
		}
	})

	t.Run("any other errno is a plain I/O error, not ErrUnsafePath", func(t *testing.T) {
		closed := openDirForTest(t, root)
		closed.Close()
		_, err := openChildDir(closed, "plainfile")
		if err == nil || errors.Is(err, ErrUnsafePath) {
			t.Fatalf("err = %v, want a plain I/O error", err)
		}
	})
}

// --- T3a: setting permissions on a newly created directory opens it with
// O_NOFOLLOW first ---

func TestResolveOneComponent_T3a_RejectsSymlinkSwappedInBeforeOpen(t *testing.T) {
	root := t.TempDir()
	rootFd := openDirForTest(t, root)
	defer rootFd.Close()

	// t.TempDir() itself may already hand back 0755; chmod explicitly to a
	// baseline distinct from the 0755 this test asks resolveOneComponent to
	// set, so a leak through the symlink is actually observable below.
	outside := t.TempDir()
	if err := os.Chmod(outside, 0700); err != nil {
		t.Fatalf("chmod outside: %v", err)
	}
	before := permOf(t, outside)

	orig := hookAfterMkdirat
	defer func() { hookAfterMkdirat = orig }()
	hookAfterMkdirat = func() {
		if err := os.RemoveAll(filepath.Join(root, "newdir")); err != nil {
			t.Fatalf("remove new dir: %v", err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "newdir")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	}

	_, _, err := resolveOneComponent([]*os.File{rootFd}, []string{"newdir"}, 0, true, 0755, dirAllowExisting)
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath", err)
	}
	if after := permOf(t, outside); after != before {
		t.Fatalf("outside directory mode changed: before=%v after=%v", before, after)
	}
}

// --- T3b: permissions are set only on the fd already held, never by name ---

func TestResolveOneComponent_T3b_ChmodActsOnlyOnHeldFd(t *testing.T) {
	root := t.TempDir()
	rootFd := openDirForTest(t, root)
	defer rootFd.Close()

	// Explicit baseline, distinct from the 0700 this test asks
	// resolveOneComponent to set, so a leak through the symlink is
	// observable regardless of what t.TempDir() itself hands back.
	outside := t.TempDir()
	if err := os.Chmod(outside, 0755); err != nil {
		t.Fatalf("chmod outside: %v", err)
	}
	before := permOf(t, outside)
	// 0700 has no group/other bits for a default umask to strip, so a
	// maximally restrictive umask is needed to make the "newdir.moved mode
	// = 0700" assertion below mean anything: Fchmod, unlike Mkdirat's own
	// mode argument, ignores umask entirely.
	withUmask(t, 0277)

	// hookBeforeMutation now fires twice for a directory this call creates
	// (design §6.5): once before Mkdirat, against the not-yet-existing
	// name, and once more before Fchmod. Only the second occurrence is
	// this test's target; the first is a no-op since "newdir" does not
	// exist yet at that point.
	orig := hookBeforeMutation
	defer func() { hookBeforeMutation = orig }()
	hookBeforeMutation = func() {
		if _, err := os.Lstat(filepath.Join(root, "newdir")); err != nil {
			return
		}
		if err := os.Rename(filepath.Join(root, "newdir"), filepath.Join(root, "newdir.moved")); err != nil {
			t.Fatalf("rename: %v", err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "newdir")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	}

	fd, created, err := resolveOneComponent([]*os.File{rootFd}, []string{"newdir"}, 0, true, 0700, dirAllowExisting)
	if err != nil {
		t.Fatalf("resolveOneComponent: %v", err)
	}
	if !created {
		t.Fatalf("created = false, want true")
	}

	if after := permOf(t, outside); after != before {
		t.Fatalf("outside directory mode changed: before=%v after=%v", before, after)
	}
	if moved := permOf(t, filepath.Join(root, "newdir.moved")); moved != fs.FileMode(0700) {
		t.Fatalf("moved directory mode = %v, want 0700 (Fchmod acted on the held fd)", moved)
	}

	// A later re-verification against the recorded chain now sees the
	// replacement.
	verr := verifyChain([]*os.File{rootFd, fd}, []string{"newdir"})
	fd.Close()
	if !errors.Is(verr, ErrUnsafePath) {
		t.Fatalf("verifyChain after the swap = %v, want ErrUnsafePath", verr)
	}
}

// --- Mkdirat itself is preceded by a re-verification of the chain already
// open so far (design §6.5 lists Mkdirat among the mutating syscalls) ---

func TestResolveParentDirs_PreMkdiratVerificationCatchesAMovedAncestor(t *testing.T) {
	root := t.TempDir()
	rootFd := openDirForTest(t, root)
	defer rootFd.Close()
	mustMkdir(t, filepath.Join(root, "etc"))
	elsewhere := t.TempDir()

	// hookAfterOpen fires once per resolveOneComponent call. The first
	// (for "etc", which already exists) must be left alone; only the
	// second (for "caddy", about to be created under "etc") should move
	// "etc" out of root, right before the Mkdirat this call is about to
	// perform for "caddy".
	calls := 0
	orig := hookAfterOpen
	defer func() { hookAfterOpen = orig }()
	hookAfterOpen = func() {
		calls++
		if calls != 2 {
			return
		}
		if err := os.Rename(filepath.Join(root, "etc"), filepath.Join(elsewhere, "etc")); err != nil {
			t.Fatalf("rename: %v", err)
		}
	}

	// On error, resolveParentDirs returns the partial chain still open (the
	// caller owns closing it and deciding what to do with chain.created) —
	// close it here even though this test does not need chain.created.
	chain, err := resolveParentDirs(rootFd, []string{"etc", "caddy"}, true, 0755, dirAllowExisting)
	if chain != nil {
		defer chain.closeOpened()
	}
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath", err)
	}
	if calls < 2 {
		t.Fatalf("hookAfterOpen fired %d times, want at least 2 (the injection never ran)", calls)
	}
	if _, statErr := os.Lstat(filepath.Join(elsewhere, "etc", "caddy")); !os.IsNotExist(statErr) {
		t.Fatalf("caddy was created outside the trusted root: stat err = %v", statErr)
	}
}

// --- T8: a transaction-style directory must be created by this call, never
// reused ---

func TestResolveOneComponent_T8_MustCreatePolicy(t *testing.T) {
	root := t.TempDir()
	rootFd := openDirForTest(t, root)
	defer rootFd.Close()

	t.Run("an existing directory is rejected and left untouched", func(t *testing.T) {
		existing := filepath.Join(root, "txn")
		if err := os.Mkdir(existing, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		keep := filepath.Join(existing, "keep")
		if err := os.WriteFile(keep, []byte("x"), 0600); err != nil {
			t.Fatalf("write: %v", err)
		}

		_, created, err := resolveOneComponent([]*os.File{rootFd}, []string{"txn"}, 0, true, 0700, dirMustCreate)
		if err == nil {
			t.Fatalf("err = nil, want an error")
		}
		if created {
			t.Fatalf("created = true, want false")
		}
		if mode := permOf(t, existing); mode != 0755 {
			t.Fatalf("existing directory mode = %v, want unchanged 0755", mode)
		}
		if _, statErr := os.Stat(keep); statErr != nil {
			t.Fatalf("existing file was removed: %v", statErr)
		}
	})

	t.Run("a missing directory is created at 0700 and reported as created", func(t *testing.T) {
		// 0700 has no group/other bits for a default umask to strip, so a
		// maximally restrictive umask is needed to make this mode
		// assertion actually exercise Fchmod (which ignores umask).
		withUmask(t, 0277)
		fd, created, err := resolveOneComponent([]*os.File{rootFd}, []string{"txn-new"}, 0, true, 0700, dirMustCreate)
		if err != nil {
			t.Fatalf("resolveOneComponent: %v", err)
		}
		defer fd.Close()
		if !created {
			t.Fatalf("created = false, want true")
		}
		if mode := permOf(t, filepath.Join(root, "txn-new")); mode != 0700 {
			t.Fatalf("new directory mode = %v, want 0700", mode)
		}
	})
}

// --- "allow existing" does not widen an existing directory's permissions ---

func TestResolveOneComponent_AllowExistingDoesNotWidenPermissions(t *testing.T) {
	root := t.TempDir()
	rootFd := openDirForTest(t, root)
	defer rootFd.Close()

	existing := filepath.Join(root, "caddy")
	if err := os.Mkdir(existing, 0750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	fd, created, err := resolveOneComponent([]*os.File{rootFd}, []string{"caddy"}, 0, true, 0755, dirAllowExisting)
	if err != nil {
		t.Fatalf("resolveOneComponent: %v", err)
	}
	defer fd.Close()
	if created {
		t.Fatalf("created = true, want false (directory pre-existed)")
	}
	if mode := permOf(t, existing); mode != 0750 {
		t.Fatalf("existing directory mode = %v, want unchanged 0750", mode)
	}

	// A maximally restrictive umask makes the mode assertion below actually
	// exercise Fchmod (which ignores umask), rather than the mode
	// coincidentally surviving Mkdirat's own default-umask-masked creation.
	withUmask(t, 0277)
	fd2, created2, err := resolveOneComponent([]*os.File{rootFd}, []string{"sing-box"}, 0, true, 0755, dirAllowExisting)
	if err != nil {
		t.Fatalf("resolveOneComponent: %v", err)
	}
	defer fd2.Close()
	if !created2 {
		t.Fatalf("created = false, want true (directory was missing)")
	}
	if mode := permOf(t, filepath.Join(root, "sing-box")); mode != 0755 {
		t.Fatalf("new directory mode = %v, want 0755", mode)
	}
}

// --- ancestor-chain re-verification (design §6.5), private-level ---

func TestVerifyChain_UnchangedChainPasses(t *testing.T) {
	root := t.TempDir()
	rootFd := openDirForTest(t, root)
	defer rootFd.Close()
	mustMkdir(t, filepath.Join(root, "etc", "caddy"))

	chain, err := resolveParentDirs(rootFd, []string{"etc", "caddy"}, false, 0, dirAllowExisting)
	if err != nil {
		t.Fatalf("resolveParentDirs: %v", err)
	}
	defer chain.closeOpened()

	if err := verifyChain(chain.fds, chain.comps); err != nil {
		t.Fatalf("verifyChain: %v", err)
	}
}

func TestVerifyChain_CatchesADirectoryMovedOutAndReplaced(t *testing.T) {
	root := t.TempDir()
	rootFd := openDirForTest(t, root)
	defer rootFd.Close()
	mustMkdir(t, filepath.Join(root, "etc", "caddy"))

	chain, err := resolveParentDirs(rootFd, []string{"etc", "caddy"}, false, 0, dirAllowExisting)
	if err != nil {
		t.Fatalf("resolveParentDirs: %v", err)
	}
	defer chain.closeOpened()

	elsewhere := t.TempDir()
	if err := os.Rename(filepath.Join(root, "etc", "caddy"), filepath.Join(elsewhere, "caddy")); err != nil {
		t.Fatalf("rename out: %v", err)
	}
	// A different, real directory takes the same name so the fresh walk
	// still succeeds and the mismatch is what verifyChain must catch.
	mustMkdir(t, filepath.Join(root, "etc", "caddy"))

	if err := verifyChain(chain.fds, chain.comps); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath", err)
	}
}

func TestVerifyChain_CatchesAMissingDirectory(t *testing.T) {
	root := t.TempDir()
	rootFd := openDirForTest(t, root)
	defer rootFd.Close()
	mustMkdir(t, filepath.Join(root, "etc", "caddy"))

	chain, err := resolveParentDirs(rootFd, []string{"etc", "caddy"}, false, 0, dirAllowExisting)
	if err != nil {
		t.Fatalf("resolveParentDirs: %v", err)
	}
	defer chain.closeOpened()

	if err := os.RemoveAll(filepath.Join(root, "etc", "caddy")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if err := verifyChain(chain.fds, chain.comps); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath", err)
	}
}

// A component that was proven to be a directory when its fd was first
// opened, but now resolves to a plain file, is a replacement caught by
// re-verification — not design §6.4's "ordinary file as parent" case, which
// applies only to a path's first resolution.
func TestVerifyChain_CatchesADirectoryReplacedByARegularFile(t *testing.T) {
	root := t.TempDir()
	rootFd := openDirForTest(t, root)
	defer rootFd.Close()
	mustMkdir(t, filepath.Join(root, "etc", "caddy"))

	chain, err := resolveParentDirs(rootFd, []string{"etc", "caddy"}, false, 0, dirAllowExisting)
	if err != nil {
		t.Fatalf("resolveParentDirs: %v", err)
	}
	defer chain.closeOpened()

	if err := os.RemoveAll(filepath.Join(root, "etc", "caddy")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc", "caddy"), []byte("x"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := verifyChain(chain.fds, chain.comps); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath", err)
	}
}

// --- T6 regression: a symlink ancestor under root is still rejected once
// resolution goes through the fd-based entry point ---

func TestResolveUnderTrustedRoot_T6_RejectsSymlinkAncestor(t *testing.T) {
	root := t.TempDir()
	rootFd := openDirForTest(t, root)
	defer rootFd.Close()

	outside := t.TempDir()
	mustMkdir(t, filepath.Join(root, "etc"))
	if err := os.Symlink(outside, filepath.Join(root, "etc", "caddy")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	chain, _, err := resolveUnderTrustedRoot(rootFd, "etc/caddy/Caddyfile", false, 0, dirAllowExisting)
	if chain != nil {
		defer chain.closeOpened()
	}
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath", err)
	}
}

// --- R1-R3: replacement for the deleted TestResolveUnderRoot_KeepsPathsInside
// (Global Constraints #9's "test replacement"; deleted in Task 4) ---

// R1 (assertion ①: "../escape" is rejected).
func TestResolveUnderTrustedRoot_R1_RejectsRelativeTraversal(t *testing.T) {
	root := t.TempDir()
	rootFd := openDirForTest(t, root)
	defer rootFd.Close()

	// Errorf, not Fatalf: both assertions below must run even if the first
	// one fails, or a mutation that breaks only the second could hide
	// behind a t.Fatalf on the first and go unnoticed.
	if _, _, err := resolveUnderTrustedRoot(rootFd, "../escape", true, 0755, dirAllowExisting); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("../escape: err = %v, want ErrUnsafePath", err)
	}

	if _, _, err := resolveUnderTrustedRoot(rootFd, "../escape/file", true, 0755, dirAllowExisting); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("../escape/file: err = %v, want ErrUnsafePath", err)
	}
	if _, statErr := os.Lstat(filepath.Join(root, "..", "escape")); !os.IsNotExist(statErr) {
		t.Errorf("an \"escape\" directory was created next to root: stat err = %v", statErr)
	}
}

// R2 (assertion ②: "/etc/passwd" is rejected).
func TestResolveUnderTrustedRoot_R2_RejectsAbsolutePath(t *testing.T) {
	root := t.TempDir()
	rootFd := openDirForTest(t, root)
	defer rootFd.Close()

	chain, leaf, err := resolveUnderTrustedRoot(rootFd, "/etc/passwd", false, 0, dirAllowExisting)
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath", err)
	}
	if chain != nil || leaf != "" {
		t.Fatalf("resolveUnderTrustedRoot returned results alongside an error")
	}
}

// R3 (assertion ③: a legal logical path resolves to the same object under
// root that a plain filepath.Join would name).
func TestResolveUnderTrustedRoot_R3_ResolvesToTheSameObjectUnderRoot(t *testing.T) {
	root := t.TempDir()
	rootFd := openDirForTest(t, root)
	defer rootFd.Close()

	mustMkdir(t, filepath.Join(root, "etc", "caddy"))
	target := filepath.Join(root, "etc", "caddy", "Caddyfile")
	if err := os.WriteFile(target, []byte("content"), 0640); err != nil {
		t.Fatalf("write: %v", err)
	}

	chain, leaf, err := resolveUnderTrustedRoot(rootFd, "etc/caddy/Caddyfile", false, 0, dirAllowExisting)
	if err != nil {
		t.Fatalf("resolveUnderTrustedRoot: %v", err)
	}
	defer chain.closeOpened()
	if leaf != "Caddyfile" {
		t.Fatalf("leaf = %q, want Caddyfile", leaf)
	}

	fd, err := unix.Openat(int(chain.leaf().Fd()), leaf, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open resolved leaf: %v", err)
	}
	f := os.NewFile(uintptr(fd), leaf)
	defer f.Close()

	var gotSt unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &gotSt); err != nil {
		t.Fatalf("fstat: %v", err)
	}
	wantInfo, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	wantSt, ok := wantInfo.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("could not read %s's ownership", target)
	}
	if uint64(gotSt.Dev) != uint64(wantSt.Dev) || uint64(gotSt.Ino) != uint64(wantSt.Ino) {
		t.Fatalf("resolved object (dev=%d ino=%d) != filepath.Join(root, ...) (dev=%d ino=%d)",
			gotSt.Dev, gotSt.Ino, wantSt.Dev, wantSt.Ino)
	}
}
