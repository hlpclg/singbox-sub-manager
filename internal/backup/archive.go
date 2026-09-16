package backup

import (
	"archive/tar"
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
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// The fixed backup path set from the design document, section 4. All paths are
// relative to CreateOptions.SourceRoot. recursiveTrees are walked in full;
// explicitFiles are single-file rules.
var (
	recursiveTrees = []string{
		"etc/singbox-sub-manager",
	}
	explicitFiles = []string{
		"etc/sing-box/config.json",
		"etc/caddy/Caddyfile",
		"var/lib/singbox-sub-manager/token",
		"var/lib/singbox-sub-manager/monitor-state.json",
		"var/lib/singbox-sub-manager/monitor-paused",
	}
)

const (
	backupDirMode     fs.FileMode = 0700
	archiveFileMode   fs.FileMode = 0600
	archiveTimeLayout             = "20060102T150405Z"
	maxNameCollisions             = 1000
)

var archiveNamePattern = regexp.MustCompile(`^backup-[0-9]{8}T[0-9]{6}Z(?:-[1-9][0-9]*)?\.tar\.gz$`)

// IsArchiveName reports whether name follows the automatic archive naming rule.
// Only such files are system-created archives; retention and listing must not
// touch anything else a user placed in the backup directory.
func IsArchiveName(name string) bool {
	return archiveNamePattern.MatchString(name)
}

// CreateOptions configures one backup run. SourceRoot and BackupDir are
// injectable so tests can run against a temporary tree; production passes
// SourceRoot "/" and the default backup directory.
type CreateOptions struct {
	// SourceRoot is the root the fixed path set is resolved against. Empty
	// means "/".
	SourceRoot string
	// BackupDir receives automatically named archives. Required unless Out
	// is set.
	BackupDir string
	// Out overrides the destination path. The automatic naming and
	// retention rules do not apply to it.
	Out string
	// ProxyctlVersion is recorded in the manifest.
	ProxyctlVersion string
	// Now injects the clock used for the archive name and created_at.
	Now func() time.Time
}

// writeArchiveFn is replaced in tests to exercise the failure cleanup that
// removes a reserved archive name.
var writeArchiveFn = writeArchive

type payload struct {
	entry FileEntry
	data  []byte
}

// Create packs the fixed path set into a gzip-compressed tar archive and
// returns its manifest. The archive is staged in a same-directory temporary
// file, fsynced and atomically renamed. A run that fails before the rename
// leaves nothing that could be mistaken for a complete archive; one whose
// only failure is after a successful rename (closing the fd, or fsyncing
// the output directory) leaves the archive itself in place — its content is
// already complete and correctly named, only the directory entry's
// durability across a crash is unconfirmed — and does not run the
// reservation cleanup against it (see writeArchive's published return).
func Create(ctx context.Context, opts CreateOptions) (Manifest, error) {
	if opts.SourceRoot == "" {
		opts.SourceRoot = "/"
	}
	if opts.Out == "" && opts.BackupDir == "" {
		return Manifest{}, fmt.Errorf("backup: BackupDir or Out is required")
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	createdAt := nowFn().UTC()

	sourceRootFd, err := openTrustedRoot(opts.SourceRoot, false, 0)
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: open source root: %w", err)
	}
	defer sourceRootFd.Close()

	payloads, skipped, reasons, err := collect(ctx, sourceRootFd)
	if err != nil {
		return Manifest{}, err
	}

	m := Manifest{
		SchemaVersion:   SchemaVersion,
		CreatedAt:       createdAt,
		ProxyctlVersion: opts.ProxyctlVersion,
		Files:           make([]FileEntry, 0, len(payloads)),
		Skipped:         skipped,
		SkippedReasons:  reasons,
	}
	for _, p := range payloads {
		m.Files = append(m.Files, p.entry)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}

	outDirFd, leaf, fullPath, reservedSelf, release, err := reserveDestination(opts, createdAt)
	if err != nil {
		return Manifest{}, err
	}
	defer outDirFd.Close()
	published, err := writeArchiveFn(ctx, outDirFd, leaf, m, payloads)
	if err != nil {
		// published: the rename already succeeded — release() must not run
		// against the archive this call itself just finished writing (it
		// would either refuse via a §6.7 identity mismatch, since the
		// reservation's fd no longer matches what the name now points to,
		// or — for a hypothetical bare-Unlinkat release — delete it
		// outright). release() needs reservedSelf's fd still open for that
		// identity check, so close it only after release() has run.
		if !published {
			if w := release(); w != "" {
				err = fmt.Errorf("%w (%s)", err, w)
			}
		}
		if reservedSelf != nil {
			reservedSelf.Close()
		}
		return Manifest{}, err
	}
	if reservedSelf != nil {
		reservedSelf.Close()
	}
	m.ArchivePath = fullPath
	return m, nil
}

// collect reads every source file exactly once, using the same bytes for the
// manifest checksum and the archive payload so the two can never disagree.
// sourceRootFd is SourceRoot's trusted-root fd (design §6.2); every name
// under it is resolved fd-relative, O_NOFOLLOW at each component, exactly
// like every other operation in this package.
func collect(ctx context.Context, sourceRootFd *os.File) ([]payload, []string, map[string]string, error) {
	var payloads []payload
	skipped := []string{}
	reasons := map[string]string{}

	skip := func(rel, reason string) {
		skipped = append(skipped, rel)
		reasons[rel] = reason
	}

	for _, tree := range recursiveTrees {
		comps := splitLogicalPath(tree)
		chain, err := resolveParentDirs(sourceRootFd, comps, false, 0, dirAllowExisting)
		closeErr := func() { chain.closeOpened() }
		if err != nil {
			closeErr()
			if errors.Is(err, os.ErrNotExist) {
				// Record the whole tree so a restore can tell "there was
				// no configuration tree" from "there was one and it was
				// packed" (design document, section 4).
				skip(tree, SkipNotFound)
				continue
			}
			// v0.7's filepath.WalkDir called its callback once for the tree
			// root itself before ever recursing, so a root that turned out
			// not to be a directory (a symlink, a plain file, a FIFO) was
			// classified exactly like any other non-directory entry found
			// during the walk — skipped, not a hard failure (design §6.6:
			// "跳过规则与稳定原因不变"). Only when the failure happened at
			// an INTERMEDIATE ancestor of the tree root (not the root's own
			// final component) does this remain a hard failure: that is a
			// genuinely new defense this version adds (an ancestor replaced
			// out from under the walk), not a v0.7 skip case.
			if len(chain.comps) == len(comps)-1 &&
				(errors.Is(err, ErrUnsafePath) || errors.Is(err, errNotADirectory)) {
				skip(tree, SkipUnsupportedSourceType)
				continue
			}
			return nil, nil, nil, err
		}
		if err := walkSourceTree(ctx, chain.leaf(), tree, &payloads, skip); err != nil {
			closeErr()
			return nil, nil, nil, err
		}
		closeErr()
	}

	for _, name := range explicitFiles {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		chain, leaf, resErr := resolveUnderTrustedRoot(sourceRootFd, name, false, 0, dirAllowExisting)
		if resErr != nil {
			if chain != nil {
				chain.closeOpened()
			}
			if errors.Is(resErr, os.ErrNotExist) {
				skip(name, SkipNotFound)
				continue
			}
			return nil, nil, nil, resErr
		}
		pl, readErr := readSourceFile(chain.leaf(), leaf, name)
		chain.closeOpened()
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				skip(name, SkipNotFound)
				continue
			}
			// Explicit files are hard requirements, not best-effort: an
			// unsupported type here is reported, never silently skipped
			// (design document, section 4; v0.7 parity).
			return nil, nil, nil, readErr
		}
		payloads = append(payloads, pl)
	}

	sort.Slice(payloads, func(i, j int) bool { return payloads[i].entry.Path < payloads[j].entry.Path })
	sort.Strings(skipped)
	return payloads, skipped, reasons, nil
}

// walkSourceTree recurses dirFd (already open, O_NOFOLLOW'd) exactly like
// v0.7's filepath.WalkDir did by path, opening each subdirectory fd-relative
// (design §6.6) instead of re-resolving by path. rel is dirFd's own logical
// path, used to build each entry's.
func walkSourceTree(ctx context.Context, dirFd *os.File, rel string, payloads *[]payload, skip func(rel, reason string)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	names, err := dirFd.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("backup: read %s: %w", rel, err)
	}
	if len(names) == 0 {
		skip(rel, SkipEmptyDirectory)
		return nil
	}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		childRel := rel + "/" + name
		var st unix.Stat_t
		if statErr := unix.Fstatat(int(dirFd.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); statErr != nil {
			if errors.Is(statErr, unix.ENOENT) {
				continue // vanished between Readdirnames and here.
			}
			return fmt.Errorf("backup: inspect %s: %w", childRel, statErr)
		}
		switch {
		case st.Mode&unix.S_IFMT == unix.S_IFDIR:
			child, openErr := openChildDir(dirFd, name)
			if openErr != nil {
				if errors.Is(openErr, os.ErrNotExist) {
					continue // vanished between Readdirnames and here.
				}
				return openErr
			}
			walkErr := walkSourceTree(ctx, child, childRel, payloads, skip)
			child.Close()
			if walkErr != nil {
				return walkErr
			}
		case st.Mode&unix.S_IFMT == unix.S_IFREG && st.Nlink == 1:
			pl, readErr := readSourceFile(dirFd, name, childRel)
			if readErr != nil {
				if errors.Is(readErr, ErrUnsupportedSourceType) {
					skip(childRel, SkipUnsupportedSourceType)
					continue
				}
				if errors.Is(readErr, os.ErrNotExist) {
					continue // vanished between Readdirnames and here.
				}
				return readErr
			}
			*payloads = append(*payloads, pl)
		default:
			// A symlink, hard link (Nlink != 1), FIFO, socket or device —
			// the entry-level Fstatat above already caught it, so this
			// never reaches readSourceFile's own re-check.
			skip(childRel, SkipUnsupportedSourceType)
		}
	}
	return nil
}

// splitLogicalPath splits an already-known-safe logical path (one of this
// package's own recursiveTrees/explicitFiles constants, never external
// input) into its slash-separated components.
func splitLogicalPath(logical string) []string {
	var comps []string
	start := 0
	for i := 0; i < len(logical); i++ {
		if logical[i] == '/' {
			comps = append(comps, logical[start:i])
			start = i + 1
		}
	}
	return append(comps, logical[start:])
}

// readSourceFile opens name under dirFd without following a symlink and
// re-checks its type and link count on the opened descriptor (design §6.6),
// so a link swapped in after the directory scan can neither be followed nor
// packed. O_NONBLOCK keeps a FIFO swapped in the same way from blocking the
// open. Mode and ownership come from the same descriptor the bytes are read
// from, and the bytes are read exactly once so the manifest checksum and the
// archive payload can never disagree.
func readSourceFile(dirFd *os.File, name, rel string) (payload, error) {
	fd, err := unix.Openat(int(dirFd.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return payload{}, fmt.Errorf("%s: %w", rel, os.ErrNotExist)
		}
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EMLINK) {
			return payload{}, fmt.Errorf("%w: %s", ErrUnsupportedSourceType, rel)
		}
		return payload{}, fmt.Errorf("backup: open %s: %w", rel, err)
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()

	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return payload{}, fmt.Errorf("backup: stat %s: %w", rel, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		return payload{}, fmt.Errorf("%w: %s", ErrUnsupportedSourceType, rel)
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return payload{}, fmt.Errorf("backup: read %s: %w", rel, err)
	}
	sum := sha256.Sum256(data)
	return payload{
		entry: FileEntry{
			Path:   rel,
			SHA256: hex.EncodeToString(sum[:]),
			Mode:   fs.FileMode(st.Mode & 0777),
			UID:    st.Uid,
			GID:    st.Gid,
		},
		data: data,
	}, nil
}

// reserveDestination fixes the final archive path before any bytes are
// written and returns the open output-directory fd (design §6.2's trusted
// root: BackupDir, or --out's parent), the leaf name under it, the string
// path for Manifest.ArchivePath, the reserved name's own fd (design §6.7's
// pre-delete identity comparison; nil when nothing was reserved — see
// below), and a release func that undoes the reservation when the run
// fails. Automatic names are reserved with O_EXCL so two runs in the same
// second cannot overwrite each other; --out never reserves anything ahead
// of time (the eventual Renameat in writeArchive overwrites it atomically
// either way, so there is no O_EXCL-created object for §6.7 to apply to
// here) — release only removes a file this call itself is about to create,
// never one the caller already had.
func reserveDestination(opts CreateOptions, createdAt time.Time) (dirFd *os.File, leaf, fullPath string, self *os.File, release func() string, err error) {
	if opts.Out != "" {
		// A trailing separator makes Out ambiguous between "a file named
		// after the last real component" and "a directory to write into" —
		// filepath.Dir/Base would silently resolve it as the former
		// (writing beside where the caller probably meant), where v0.7's
		// path-based os.Rename instead failed outright on such a
		// destination. Reject it the same way, rather than silently
		// picking a guess.
		if strings.HasSuffix(opts.Out, "/") {
			return nil, "", "", nil, nil, fmt.Errorf("backup: --out %q must not end with a path separator", opts.Out)
		}
		dir := filepath.Dir(opts.Out)
		base := filepath.Base(opts.Out)
		outDirFd, openErr := openTrustedRoot(dir, false, 0)
		if openErr != nil {
			return nil, "", "", nil, nil, openErr
		}
		var st unix.Stat_t
		statErr := unix.Fstatat(int(outDirFd.Fd()), base, &st, unix.AT_SYMLINK_NOFOLLOW)
		if statErr == nil {
			// The run replaces a file the caller already had; removing it
			// on failure would destroy data this package never created.
			return outDirFd, base, opts.Out, nil, func() string { return "" }, nil
		}
		if !errors.Is(statErr, unix.ENOENT) {
			outDirFd.Close()
			return nil, "", "", nil, nil, fmt.Errorf("backup: stat destination: %w", statErr)
		}
		return outDirFd, base, opts.Out, nil, func() string {
			_ = unix.Unlinkat(int(outDirFd.Fd()), base, 0)
			return ""
		}, nil
	}

	backupDirFd, openErr := openTrustedRoot(opts.BackupDir, true, backupDirMode)
	if openErr != nil {
		return nil, "", "", nil, nil, openErr
	}
	stamp := createdAt.Format(archiveTimeLayout)
	for i := 0; i <= maxNameCollisions; i++ {
		name := fmt.Sprintf("backup-%s.tar.gz", stamp)
		if i > 0 {
			name = fmt.Sprintf("backup-%s-%d.tar.gz", stamp, i)
		}
		fd, openErr := unix.Openat(int(backupDirFd.Fd()), name, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(archiveFileMode.Perm()))
		if openErr != nil {
			if errors.Is(openErr, unix.EEXIST) {
				continue
			}
			backupDirFd.Close()
			return nil, "", "", nil, nil, fmt.Errorf("backup: reserve archive name: %w", openErr)
		}
		reserved := os.NewFile(uintptr(fd), name)
		full := filepath.Join(opts.BackupDir, name)
		return backupDirFd, name, full, reserved, func() string {
			return removeStagedFileFd(backupDirFd, name, reserved)
		}, nil
	}
	backupDirFd.Close()
	return nil, "", "", nil, nil, fmt.Errorf("backup: too many archives created at %s", stamp)
}

// randomArchiveTmpName returns a name that is, for practical purposes,
// guaranteed unique: writeArchive's O_EXCL is what actually proves it, this
// just makes a collision vanishingly unlikely.
func randomArchiveTmpName() (string, error) {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("backup: generate staging name: %w", err)
	}
	return ".backup-" + hex.EncodeToString(buf[:]) + ".tar.gz.tmp", nil
}

// writeArchive stages the archive under dirFd and renames it into place as
// leaf (design §6.6). The archive holds exactly one manifest.json plus one
// regular entry per manifest file: no directory, symlink or hard link
// entries.
func writeArchive(ctx context.Context, dirFd *os.File, leaf string, m Manifest, payloads []payload) (published bool, err error) {
	manifestData, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return false, fmt.Errorf("backup: encode manifest: %w", err)
	}

	tmpName, tmp, err := createArchiveStagingFile(dirFd)
	if err != nil {
		return false, err
	}
	closed := false
	defer func() {
		// The design §6.7 identity comparison inside removeStagedFileFd
		// needs tmp's own fd for Fstat, so it must run before tmp is
		// closed, not after.
		if err != nil {
			if w := removeStagedFileFd(dirFd, tmpName, tmp); w != "" {
				err = fmt.Errorf("%w (%s)", err, w)
			}
		}
		if !closed {
			_ = tmp.Close()
		}
	}()

	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)

	if err = writeTarEntry(tw, ManifestName, manifestData, archiveFileMode, 0, 0, m.CreatedAt); err != nil {
		return false, err
	}
	for _, p := range payloads {
		if err = ctx.Err(); err != nil {
			return false, err
		}
		if err = writeTarEntry(tw, p.entry.Path, p.data, p.entry.Mode, p.entry.UID, p.entry.GID, m.CreatedAt); err != nil {
			return false, err
		}
	}
	if err = tw.Close(); err != nil {
		return false, fmt.Errorf("backup: finish tar: %w", err)
	}
	if err = gz.Close(); err != nil {
		return false, fmt.Errorf("backup: finish gzip: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return false, fmt.Errorf("backup: sync archive: %w", err)
	}
	// tmp stays open across the rename (not closed until it succeeds): a
	// failed Renameat is itself a failure the deferred cleanup above must
	// clean up after, and that cleanup's §6.7 identity comparison needs
	// tmp's fd open to do it.
	if err = unix.Renameat(int(dirFd.Fd()), tmpName, int(dirFd.Fd()), leaf); err != nil {
		return false, fmt.Errorf("backup: publish archive: %w", err)
	}
	// Renameat succeeded: dirFd's entry named leaf is now this run's
	// archive, complete and correctly named. Everything from here on is
	// reported as an error if it fails, but published stays true — the
	// caller must not run its reservation cleanup against what is now the
	// real archive (design §6.7 applies to what this run is still trying
	// to create, not to what it already finished publishing).
	published = true
	closed = true
	if err = tmp.Close(); err != nil {
		return true, fmt.Errorf("backup: close archive: %w", err)
	}
	if err = dirFd.Sync(); err != nil {
		return true, fmt.Errorf("backup: sync output directory: %w", err)
	}
	return true, nil
}

// createArchiveStagingFile creates, under dirFd, a new file with a random
// name (O_CREAT|O_EXCL, so it is provably this call's own inode), tightens
// it to archiveFileMode on the fd (never by name), and returns both the name
// — ready for the caller's Renameat — and the still-open fd.
func createArchiveStagingFile(dirFd *os.File) (name string, f *os.File, err error) {
	for attempt := 0; attempt < 10; attempt++ {
		candidate, genErr := randomArchiveTmpName()
		if genErr != nil {
			return "", nil, genErr
		}
		fd, openErr := unix.Openat(int(dirFd.Fd()), candidate, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(archiveFileMode.Perm()))
		if openErr != nil {
			if errors.Is(openErr, unix.EEXIST) {
				continue
			}
			return "", nil, fmt.Errorf("backup: create staging file: %w", openErr)
		}
		newFile := os.NewFile(uintptr(fd), candidate)
		if chmodErr := newFile.Chmod(archiveFileMode); chmodErr != nil {
			// This is itself a design §6.7 failure-cleanup delete (of the
			// O_CREAT|O_EXCL file just created), so it gets the same
			// pre-delete identity comparison as every other one, via the
			// same helper — not a bare Unlinkat.
			w := removeStagedFileFd(dirFd, candidate, newFile)
			newFile.Close()
			err := fmt.Errorf("backup: tighten staging file: %w", chmodErr)
			if w != "" {
				err = fmt.Errorf("%w (%s)", err, w)
			}
			return "", nil, err
		}
		return candidate, newFile, nil
	}
	return "", nil, fmt.Errorf("backup: could not reserve a staging name after 10 attempts")
}

func writeTarEntry(tw *tar.Writer, name string, data []byte, mode fs.FileMode, uid, gid uint32, modTime time.Time) error {
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Mode:     int64(mode.Perm()),
		Uid:      int(uid),
		Gid:      int(gid),
		Size:     int64(len(data)),
		ModTime:  modTime,
		Format:   tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("backup: write header %s: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("backup: write %s: %w", name, err)
	}
	return nil
}
