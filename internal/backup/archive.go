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
	"regexp"
	"sort"
	"syscall"
	"time"
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
// file, fsynced and atomically renamed, so a failed run never leaves a file
// that could be mistaken for a complete archive.
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

	payloads, skipped, reasons, err := collect(ctx, opts.SourceRoot)
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

	dest, release, err := reserveDestination(opts, createdAt)
	if err != nil {
		return Manifest{}, err
	}
	if err := writeArchiveFn(ctx, dest, m, payloads); err != nil {
		release()
		return Manifest{}, err
	}
	m.ArchivePath = dest
	return m, nil
}

// collect reads every source file exactly once, using the same bytes for the
// manifest checksum and the archive payload so the two can never disagree.
func collect(ctx context.Context, sourceRoot string) ([]payload, []string, map[string]string, error) {
	var payloads []payload
	skipped := []string{}
	reasons := map[string]string{}

	skip := func(rel, reason string) {
		skipped = append(skipped, rel)
		reasons[rel] = reason
	}

	for _, tree := range recursiveTrees {
		root := filepath.Join(sourceRoot, filepath.FromSlash(tree))
		if _, err := os.Lstat(root); err != nil {
			if os.IsNotExist(err) {
				// Record the whole tree so a restore can tell "there was
				// no configuration tree" from "there was one and it was
				// packed" (design document, section 4).
				skip(tree, SkipNotFound)
				continue
			}
			return nil, nil, nil, fmt.Errorf("backup: stat %s: %w", tree, err)
		}
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			rel, relErr := relLogicalPath(sourceRoot, p)
			if relErr != nil {
				return relErr
			}
			if d.IsDir() {
				entries, readErr := os.ReadDir(p)
				if readErr != nil {
					return readErr
				}
				if len(entries) == 0 {
					skip(rel, SkipEmptyDirectory)
				}
				return nil
			}
			info, infoErr := d.Info()
			if infoErr != nil {
				return infoErr
			}
			if !isPlainRegular(info) {
				skip(rel, SkipUnsupportedSourceType)
				return nil
			}
			pl, readErr := readPayload(p, rel)
			if readErr != nil {
				if errors.Is(readErr, ErrUnsupportedSourceType) {
					skip(rel, SkipUnsupportedSourceType)
					return nil
				}
				return readErr
			}
			payloads = append(payloads, pl)
			return nil
		})
		if err != nil {
			return nil, nil, nil, err
		}
	}

	for _, name := range explicitFiles {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		p := filepath.Join(sourceRoot, filepath.FromSlash(name))
		info, err := os.Lstat(p)
		if err != nil {
			if os.IsNotExist(err) {
				skip(name, SkipNotFound)
				continue
			}
			return nil, nil, nil, fmt.Errorf("backup: stat %s: %w", name, err)
		}
		if !isPlainRegular(info) {
			return nil, nil, nil, fmt.Errorf("%w: %s", ErrUnsupportedSourceType, name)
		}
		pl, err := readPayload(p, name)
		if err != nil {
			return nil, nil, nil, err
		}
		payloads = append(payloads, pl)
	}

	sort.Slice(payloads, func(i, j int) bool { return payloads[i].entry.Path < payloads[j].entry.Path })
	sort.Strings(skipped)
	return payloads, skipped, reasons, nil
}

// isPlainRegular reports whether info describes a regular file with exactly one
// link. Hard links are detected through Stat_t.Nlink because the Lstat mode
// bits alone cannot distinguish them from ordinary files.
func isPlainRegular(info fs.FileInfo) bool {
	if !info.Mode().IsRegular() {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return uint64(st.Nlink) == 1
}

// readPayload opens the file without following symlinks and re-checks its type
// on the opened descriptor, so a link swapped in after the directory scan can
// neither be followed nor packed. O_NONBLOCK keeps a FIFO swapped in the same
// way from blocking the open. Mode and ownership come from the same descriptor
// the bytes are read from, and the bytes are read exactly once so the manifest
// checksum and the archive payload can never disagree.
func readPayload(p, rel string) (payload, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK) {
			return payload{}, fmt.Errorf("%w: %s", ErrUnsupportedSourceType, rel)
		}
		return payload{}, fmt.Errorf("backup: open %s: %w", rel, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return payload{}, fmt.Errorf("backup: stat %s: %w", rel, err)
	}
	if !isPlainRegular(info) {
		return payload{}, fmt.Errorf("%w: %s", ErrUnsupportedSourceType, rel)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return payload{}, fmt.Errorf("backup: cannot read ownership of %s", rel)
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
			Mode:   info.Mode().Perm(),
			UID:    st.Uid,
			GID:    st.Gid,
		},
		data: data,
	}, nil
}

func relLogicalPath(sourceRoot, p string) (string, error) {
	rel, err := filepath.Rel(sourceRoot, p)
	if err != nil {
		return "", fmt.Errorf("backup: resolve %s: %w", p, err)
	}
	rel = filepath.ToSlash(rel)
	if err := ValidateLogicalPath(rel); err != nil {
		return "", err
	}
	return rel, nil
}

// reserveDestination fixes the final archive path before any bytes are
// written. Automatic names are reserved with O_EXCL so two runs in the same
// second cannot overwrite each other; the returned release removes the
// reservation when the run fails.
func reserveDestination(opts CreateOptions, createdAt time.Time) (string, func(), error) {
	if opts.Out != "" {
		if _, err := os.Lstat(opts.Out); err == nil {
			// The run replaces a file the caller already had; removing it
			// on failure would destroy data this package never created.
			return opts.Out, func() {}, nil
		} else if !os.IsNotExist(err) {
			return "", nil, fmt.Errorf("backup: stat destination: %w", err)
		}
		return opts.Out, func() { _ = os.Remove(opts.Out) }, nil
	}
	if err := os.MkdirAll(opts.BackupDir, backupDirMode); err != nil {
		return "", nil, fmt.Errorf("backup: create backup directory: %w", err)
	}
	if err := os.Chmod(opts.BackupDir, backupDirMode); err != nil {
		return "", nil, fmt.Errorf("backup: tighten backup directory: %w", err)
	}
	stamp := createdAt.Format(archiveTimeLayout)
	for i := 0; i <= maxNameCollisions; i++ {
		name := fmt.Sprintf("backup-%s.tar.gz", stamp)
		if i > 0 {
			name = fmt.Sprintf("backup-%s-%d.tar.gz", stamp, i)
		}
		dest := filepath.Join(opts.BackupDir, name)
		f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, archiveFileMode)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", nil, fmt.Errorf("backup: reserve archive name: %w", err)
		}
		if err := f.Close(); err != nil {
			return "", nil, fmt.Errorf("backup: reserve archive name: %w", err)
		}
		return dest, func() { _ = os.Remove(dest) }, nil
	}
	return "", nil, fmt.Errorf("backup: too many archives created at %s", stamp)
}

// writeArchive stages the archive next to its destination and renames it into
// place. The archive holds exactly one manifest.json plus one regular entry per
// manifest file: no directory, symlink or hard link entries.
func writeArchive(ctx context.Context, dest string, m Manifest, payloads []payload) (err error) {
	manifestData, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("backup: encode manifest: %w", err)
	}

	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, ".backup-*.tar.gz.tmp")
	if err != nil {
		return fmt.Errorf("backup: create staging file: %w", err)
	}
	tmpName := tmp.Name()
	closed := false
	defer func() {
		if !closed {
			_ = tmp.Close()
		}
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()

	if err = tmp.Chmod(archiveFileMode); err != nil {
		return fmt.Errorf("backup: tighten staging file: %w", err)
	}

	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)

	if err = writeTarEntry(tw, ManifestName, manifestData, archiveFileMode, 0, 0, m.CreatedAt); err != nil {
		return err
	}
	for _, p := range payloads {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = writeTarEntry(tw, p.entry.Path, p.data, p.entry.Mode, p.entry.UID, p.entry.GID, m.CreatedAt); err != nil {
			return err
		}
	}
	if err = tw.Close(); err != nil {
		return fmt.Errorf("backup: finish tar: %w", err)
	}
	if err = gz.Close(); err != nil {
		return fmt.Errorf("backup: finish gzip: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("backup: sync archive: %w", err)
	}
	closed = true
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("backup: close archive: %w", err)
	}
	if err = os.Rename(tmpName, dest); err != nil {
		return fmt.Errorf("backup: publish archive: %w", err)
	}
	return syncDir(dir)
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

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("backup: open %s: %w", dir, err)
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return fmt.Errorf("backup: sync %s: %w", dir, err)
	}
	return d.Close()
}
