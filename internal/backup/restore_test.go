package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// rawEntry is one tar entry a test writes by hand, so archives that a correct
// packer would never produce can still be fed to the restore path.
type rawEntry struct {
	name     string
	data     string
	typeflag byte
	linkname string
	mode     int64
	uid      int
	gid      int
}

func writeRawArchive(t *testing.T, path string, entries []rawEntry) string {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typeflag
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0600
		}
		hdr := &tar.Header{
			Typeflag: typ,
			Name:     e.name,
			Linkname: e.linkname,
			Mode:     mode,
			Uid:      e.uid,
			Gid:      e.gid,
			ModTime:  fixedClock,
			Format:   tar.FormatPAX,
		}
		if typ == tar.TypeReg {
			hdr.Size = int64(len(e.data))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %s: %v", e.name, err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.data)); err != nil {
				t.Fatalf("write %s: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return path
}

func sha256Of(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func manifestRawEntry(t *testing.T, m Manifest) rawEntry {
	t.Helper()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	return rawEntry{name: ManifestName, data: string(data)}
}

const (
	caddyPath = "etc/caddy/Caddyfile"
	tokenPath = "var/lib/singbox-sub-manager/token"
	caddyBody = "archived caddy\n"
	tokenBody = "archived token\n"
)

// archivedOwner is the ownership recorded in test manifests. As root it is
// deliberately not the running process's, so restoring has to apply the
// recorded UID/GID rather than whatever the staging file happens to inherit.
func archivedOwner() (uint32, uint32) {
	if os.Geteuid() == 0 {
		return 4321, 4322
	}
	return uint32(os.Geteuid()), uint32(os.Getegid())
}

// validParts returns a manifest and entries that a correct packer would write.
func validParts(t *testing.T) (Manifest, []rawEntry) {
	t.Helper()
	uid, gid := archivedOwner()
	m := Manifest{
		SchemaVersion:   SchemaVersion,
		CreatedAt:       fixedClock,
		ProxyctlVersion: "v0.6.0",
		Files: []FileEntry{
			{Path: caddyPath, SHA256: sha256Of(caddyBody), Mode: 0640, UID: uid, GID: gid},
			{Path: tokenPath, SHA256: sha256Of(tokenBody), Mode: 0600, UID: uid, GID: gid},
		},
		Skipped:        []string{},
		SkippedReasons: map[string]string{},
	}
	entries := []rawEntry{
		manifestRawEntry(t, m),
		{name: caddyPath, data: caddyBody, mode: 0640},
		{name: tokenPath, data: tokenBody, mode: 0600},
	}
	return m, entries
}

func validArchive(t *testing.T) string {
	t.Helper()
	_, entries := validParts(t)
	return writeRawArchive(t, filepath.Join(t.TempDir(), "backup.tar.gz"), entries)
}

// fingerprint records every file under root so a test can prove that nothing
// changed at all.
func fingerprint(t *testing.T, root string) map[string]string {
	t.Helper()
	return walkFingerprint(t, root, true)
}

// fileFingerprint ignores directories. Staging a file has to create its parent
// directory first, so only the files themselves prove that nothing became
// visible.
func fileFingerprint(t *testing.T, root string) map[string]string {
	t.Helper()
	return walkFingerprint(t, root, false)
}

func walkFingerprint(t *testing.T, root string, includeDirs bool) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			if !includeDirs {
				return nil
			}
			out[rel+"/"] = info.Mode().String()
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = info.Mode().String() + ":" + sha256Of(string(data))
		return nil
	})
	if err != nil {
		t.Fatalf("fingerprint %s: %v", root, err)
	}
	return out
}

func sameFingerprint(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func tempFilesUnder(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".tmp") {
			found = append(found, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", root, err)
	}
	return found
}

func TestRestore_RoundTripWritesFilesModesAndParents(t *testing.T) {
	archive := validArchive(t)
	dest := t.TempDir()

	res, err := Restore(context.Background(), archive, RestoreOptions{DestRoot: dest})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	sort.Strings(res.Restored)
	if len(res.Restored) != 2 || len(res.Failed) != 0 {
		t.Fatalf("restored = %v, failed = %v", res.Restored, res.Failed)
	}
	if data, mode := readDestFile(t, dest, caddyPath); data != caddyBody || mode != 0640 {
		t.Errorf("Caddyfile = %q mode %o, want the archived bytes at 0640", data, mode)
	}
	if data, mode := readDestFile(t, dest, tokenPath); data != tokenBody || mode != 0600 {
		t.Errorf("token = %q mode %o", data, mode)
	}
	if files := tempFilesUnder(t, dest); len(files) != 0 {
		t.Errorf("staging files left behind: %v", files)
	}
	if len(res.Preview) != 0 {
		t.Errorf("preview = %v, want it empty outside dry-run", res.Preview)
	}

	wantUID, wantGID := archivedOwner()
	if os.Geteuid() != 0 {
		t.Logf("not running as root: ownership is only checked against %d:%d", wantUID, wantGID)
	}
	for _, rel := range []string{caddyPath, tokenPath} {
		if uid, gid := ownerOf(t, filepath.Join(dest, filepath.FromSlash(rel))); uid != wantUID || gid != wantGID {
			t.Errorf("%s ownership = %d:%d, want the archived %d:%d", rel, uid, gid, wantUID, wantGID)
		}
	}
}

func TestVerify_AcceptsArchiveWrittenByCreate(t *testing.T) {
	src := newSource(t, defaultSource)
	backupDir := filepath.Join(t.TempDir(), "backups")
	created, err := Create(context.Background(), CreateOptions{SourceRoot: src, BackupDir: backupDir, Now: fixedNow})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	verified, err := Verify(context.Background(), created.ArchivePath)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(verified.Files) != len(created.Files) {
		t.Errorf("verified %d files, packed %d", len(verified.Files), len(created.Files))
	}

	dest := t.TempDir()
	res, err := Restore(context.Background(), created.ArchivePath, RestoreOptions{DestRoot: dest})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(res.Restored) != len(created.Files) {
		t.Fatalf("restored %d files, want %d", len(res.Restored), len(created.Files))
	}
	for _, f := range created.Files {
		data, mode := readDestFile(t, dest, f.Path)
		if sha256Of(data) != f.SHA256 {
			t.Errorf("%s: restored bytes differ from the archive", f.Path)
		}
		if mode != f.Mode {
			t.Errorf("%s: mode = %o, want %o", f.Path, mode, f.Mode)
		}
	}
}

func TestRestore_ChecksumMismatchRejected(t *testing.T) {
	m, entries := validParts(t)
	entries[1].data = "tampered\n"
	archive := writeRawArchive(t, filepath.Join(t.TempDir(), "backup.tar.gz"), entries)
	_ = m

	dest := t.TempDir()
	before := fingerprint(t, dest)
	l := &fakeLocker{}

	_, err := Restore(context.Background(), archive, RestoreOptions{DestRoot: dest, Lock: l})
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("err = %v, want ErrChecksumMismatch", err)
	}
	if !sameFingerprint(before, fingerprint(t, dest)) {
		t.Error("a rejected archive changed the destination")
	}
	if l.tryCalls != 0 {
		t.Errorf("a rejected archive still reached for the lock (%d TryLock calls)", l.tryCalls)
	}
}

func TestVerify_Cancellation(t *testing.T) {
	archive := validArchive(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Verify(ctx, archive); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestVerify_Timeout(t *testing.T) {
	archive := validArchive(t)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := Verify(ctx, archive); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestRestore_RejectsPathTraversal(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T) string
	}{
		{"manifest declares a traversing path", func(t *testing.T) string {
			m, entries := validParts(t)
			m.Files[0].Path = "../escape"
			entries[0] = manifestRawEntry(t, m)
			entries[1].name = "../escape"
			return writeRawArchive(t, filepath.Join(t.TempDir(), "a.tar.gz"), entries)
		}},
		{"manifest declares an absolute path", func(t *testing.T) string {
			m, entries := validParts(t)
			m.Files[0].Path = "/etc/passwd"
			entries[0] = manifestRawEntry(t, m)
			entries[1].name = "/etc/passwd"
			return writeRawArchive(t, filepath.Join(t.TempDir(), "a.tar.gz"), entries)
		}},
		{"tar entry traverses while the manifest looks clean", func(t *testing.T) string {
			_, entries := validParts(t)
			entries[1].name = "../escape"
			return writeRawArchive(t, filepath.Join(t.TempDir(), "a.tar.gz"), entries)
		}},
		{"duplicate entries", func(t *testing.T) string {
			_, entries := validParts(t)
			return writeRawArchive(t, filepath.Join(t.TempDir(), "a.tar.gz"), append(entries, entries[1]))
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			archive := tc.build(t)
			dest := t.TempDir()
			before := fingerprint(t, dest)
			l := &fakeLocker{}

			_, err := Restore(context.Background(), archive, RestoreOptions{DestRoot: dest, Lock: l})
			if !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("err = %v, want ErrUnsafePath", err)
			}
			if !sameFingerprint(before, fingerprint(t, dest)) {
				t.Error("a rejected archive changed the destination")
			}
			if l.tryCalls != 0 {
				t.Errorf("a rejected archive still reached for the lock (%d TryLock calls)", l.tryCalls)
			}
		})
	}
}

func TestRestore_RejectsSymlinkEntries(t *testing.T) {
	cases := []struct {
		name  string
		entry rawEntry
	}{
		{"symlink", rawEntry{name: "etc/evil", typeflag: tar.TypeSymlink, linkname: "/etc/shadow"}},
		{"hard link", rawEntry{name: "etc/evil", typeflag: tar.TypeLink, linkname: caddyPath}},
		{"directory", rawEntry{name: "etc/evil", typeflag: tar.TypeDir}},
		{"fifo", rawEntry{name: "etc/evil", typeflag: tar.TypeFifo}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, entries := validParts(t)
			archive := writeRawArchive(t, filepath.Join(t.TempDir(), "a.tar.gz"), append(entries, tc.entry))
			dest := t.TempDir()
			before := fingerprint(t, dest)
			l := &fakeLocker{}

			_, err := Restore(context.Background(), archive, RestoreOptions{DestRoot: dest, Lock: l})
			if !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("err = %v, want ErrUnsafePath", err)
			}
			if !sameFingerprint(before, fingerprint(t, dest)) {
				t.Error("a rejected archive changed the destination")
			}
			if l.tryCalls != 0 {
				t.Errorf("a rejected archive still reached for the lock (%d TryLock calls)", l.tryCalls)
			}
		})
	}
}

func TestRestore_RejectsManifestContentMismatch(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T) string
	}{
		{"undeclared entry", func(t *testing.T) string {
			_, entries := validParts(t)
			extra := rawEntry{name: "etc/extra.conf", data: "surprise\n"}
			return writeRawArchive(t, filepath.Join(t.TempDir(), "a.tar.gz"), append(entries, extra))
		}},
		{"declared entry missing", func(t *testing.T) string {
			_, entries := validParts(t)
			return writeRawArchive(t, filepath.Join(t.TempDir(), "a.tar.gz"), entries[:2])
		}},
		{"no manifest", func(t *testing.T) string {
			_, entries := validParts(t)
			return writeRawArchive(t, filepath.Join(t.TempDir(), "a.tar.gz"), entries[1:])
		}},
		{"two manifests", func(t *testing.T) string {
			m, entries := validParts(t)
			return writeRawArchive(t, filepath.Join(t.TempDir(), "a.tar.gz"), append(entries, manifestRawEntry(t, m)))
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			archive := tc.build(t)
			dest := t.TempDir()
			before := fingerprint(t, dest)
			l := &fakeLocker{}

			_, err := Restore(context.Background(), archive, RestoreOptions{DestRoot: dest, Lock: l})
			if !errors.Is(err, ErrArchiveContentMismatch) {
				t.Fatalf("err = %v, want ErrArchiveContentMismatch", err)
			}
			if !sameFingerprint(before, fingerprint(t, dest)) {
				t.Error("a rejected archive changed the destination")
			}
			if l.tryCalls != 0 {
				t.Errorf("a rejected archive still reached for the lock (%d TryLock calls)", l.tryCalls)
			}
		})
	}
}

// corruptGzipHeader flips the gzip magic number so gzip.NewReader rejects the
// file outright, the failure a real machine hit with a corrupted download.
func corruptGzipHeader(t *testing.T, archive string) string {
	t.Helper()
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	data[0] ^= 0xff
	out := filepath.Join(t.TempDir(), "bad-header.tar.gz")
	if err := os.WriteFile(out, data, 0600); err != nil {
		t.Fatalf("write corrupted archive: %v", err)
	}
	return out
}

// truncatedGzipHeader cuts the file off inside the fixed 10-byte gzip header,
// short of a full header but past empty.
func truncatedGzipHeader(t *testing.T, archive string) string {
	t.Helper()
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	out := filepath.Join(t.TempDir(), "truncated-header.tar.gz")
	if err := os.WriteFile(out, data[:4], 0600); err != nil {
		t.Fatalf("write truncated archive: %v", err)
	}
	return out
}

// truncatedGzipBody keeps a valid gzip header but cuts the compressed stream
// short, so the header parses fine and the failure only surfaces once tar
// starts reading decompressed bytes.
func truncatedGzipBody(t *testing.T, archive string) string {
	t.Helper()
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if len(data) < 40 {
		t.Fatalf("archive too small to truncate meaningfully: %d bytes", len(data))
	}
	out := filepath.Join(t.TempDir(), "truncated-body.tar.gz")
	if err := os.WriteFile(out, data[:len(data)-20], 0600); err != nil {
		t.Fatalf("write truncated archive: %v", err)
	}
	return out
}

// corruptDeflateBody keeps a valid 10-byte gzip header but replaces the first
// byte of the compressed stream with an invalid DEFLATE block type, so gzip
// accepts the header and decompression itself fails with
// flate.CorruptInputError instead of a truncation or header error.
func corruptDeflateBody(t *testing.T, archive string) string {
	t.Helper()
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	const gzipHeaderLen = 10
	if len(data) <= gzipHeaderLen {
		t.Fatalf("archive too small to hold a compressed body: %d bytes", len(data))
	}
	out := make([]byte, len(data))
	copy(out, data)
	out[gzipHeaderLen] = 0xff
	path := filepath.Join(t.TempDir(), "bad-deflate.tar.gz")
	if err := os.WriteFile(path, out, 0600); err != nil {
		t.Fatalf("write corrupted archive: %v", err)
	}
	return path
}

// corruptTarStructure wraps garbage bytes in a well-formed gzip stream, so
// gzip accepts the file but the first tar header is unparseable.
func corruptTarStructure(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bad-tar.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	if _, err := gz.Write([]byte("this is not a tar payload, just enough garbage bytes to fill a header block")); err != nil {
		t.Fatalf("write garbage payload: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return path
}

// corruptManifestJSON produces an otherwise well-formed archive whose
// manifest.json entry is not valid JSON.
func corruptManifestJSON(t *testing.T) string {
	t.Helper()
	_, entries := validParts(t)
	entries[0] = rawEntry{name: ManifestName, data: "{not valid json"}
	return writeRawArchive(t, filepath.Join(t.TempDir(), "bad-manifest.tar.gz"), entries)
}

// truncatedManifestBody cuts an archive containing only the manifest entry
// short, so the truncation lands inside the one entry ReadManifest actually
// reads (unlike truncatedGzipBody on a multi-file archive, which only cuts
// the last few bytes and can land entirely inside tar's trailing zero blocks,
// staying invisible to the cheap listing path by design). Compressed bytes
// don't map linearly onto the plaintext they decode to, so this cuts a
// fraction of the compressed stream rather than a fixed byte count, landing
// inside the manifest's own compressed content across archive sizes.
func truncatedManifestBody(t *testing.T) string {
	t.Helper()
	_, entries := validParts(t)
	full := writeRawArchive(t, filepath.Join(t.TempDir(), "manifest-only.tar.gz"), entries[:1])
	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	out := filepath.Join(t.TempDir(), "truncated-manifest.tar.gz")
	if err := os.WriteFile(out, data[:len(data)*9/10], 0600); err != nil {
		t.Fatalf("write truncated archive: %v", err)
	}
	return out
}

type archiveFormatCase struct {
	name  string
	build func(t *testing.T) string
}

// archiveFormatCases enumerates the ways an archive's gzip/tar container or
// embedded manifest can be malformed, independent of its declared content.
func archiveFormatCases(t *testing.T) []archiveFormatCase {
	t.Helper()
	valid := validArchive(t)
	return []archiveFormatCase{
		{"corrupt gzip header", func(t *testing.T) string { return corruptGzipHeader(t, valid) }},
		{"truncated gzip header", func(t *testing.T) string { return truncatedGzipHeader(t, valid) }},
		{"truncated gzip body", func(t *testing.T) string { return truncatedGzipBody(t, valid) }},
		{"corrupt deflate body", func(t *testing.T) string { return corruptDeflateBody(t, valid) }},
		{"corrupt tar structure", corruptTarStructure},
		{"corrupt manifest json", corruptManifestJSON},
	}
}

// readManifestFormatCases mirrors archiveFormatCases but swaps in a
// truncation that actually lands inside the manifest entry, since ReadManifest
// only reads that one entry and stops.
func readManifestFormatCases(t *testing.T) []archiveFormatCase {
	t.Helper()
	valid := validArchive(t)
	return []archiveFormatCase{
		{"corrupt gzip header", func(t *testing.T) string { return corruptGzipHeader(t, valid) }},
		{"truncated gzip header", func(t *testing.T) string { return truncatedGzipHeader(t, valid) }},
		{"truncated manifest body", truncatedManifestBody},
		{"corrupt deflate body", func(t *testing.T) string { return corruptDeflateBody(t, valid) }},
		{"corrupt tar structure", corruptTarStructure},
		{"corrupt manifest json", corruptManifestJSON},
	}
}

// TestRestore_RejectsCorruptArchiveContainer is the regression test for the
// on-host finding: a corrupted gzip header (and its adjacent tar/manifest
// parsing failures) must be classified the same way ErrChecksumMismatch and
// friends are, not fall through as a generic I/O failure. Every case must be
// rejected before the lock is taken and before anything is written.
func TestRestore_RejectsCorruptArchiveContainer(t *testing.T) {
	for _, tc := range archiveFormatCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			archive := tc.build(t)
			dest := t.TempDir()
			before := fingerprint(t, dest)
			l := &fakeLocker{}

			_, err := Restore(context.Background(), archive, RestoreOptions{DestRoot: dest, Lock: l})
			if !errors.Is(err, ErrArchiveFormat) {
				t.Fatalf("err = %v, want ErrArchiveFormat", err)
			}
			if !sameFingerprint(before, fingerprint(t, dest)) {
				t.Error("a rejected archive changed the destination")
			}
			if l.tryCalls != 0 {
				t.Errorf("a rejected archive still reached for the lock (%d TryLock calls)", l.tryCalls)
			}
		})
	}
}

// TestRestore_DryRun_RejectsCorruptArchiveContainer covers the dry-run path
// separately: it shares readArchive with the real restore, but a regression
// that only checked the writing path would miss it going unrejected here.
func TestRestore_DryRun_RejectsCorruptArchiveContainer(t *testing.T) {
	for _, tc := range archiveFormatCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			archive := tc.build(t)
			dest := t.TempDir()
			before := fingerprint(t, dest)

			res, err := Restore(context.Background(), archive, RestoreOptions{DryRun: true, DestRoot: dest})
			if !errors.Is(err, ErrArchiveFormat) {
				t.Fatalf("err = %v, want ErrArchiveFormat", err)
			}
			if len(res.Preview) != 0 {
				t.Errorf("Preview = %v, want none for a rejected archive", res.Preview)
			}
			if !sameFingerprint(before, fingerprint(t, dest)) {
				t.Error("a rejected dry-run changed the destination")
			}
		})
	}
}

// TestVerify_RejectsCorruptArchiveContainer confirms Verify, which shares
// readArchive with Restore, gets the same classification.
func TestVerify_RejectsCorruptArchiveContainer(t *testing.T) {
	archive := corruptGzipHeader(t, validArchive(t))
	if _, err := Verify(context.Background(), archive); !errors.Is(err, ErrArchiveFormat) {
		t.Fatalf("err = %v, want ErrArchiveFormat", err)
	}
}

// TestReadManifest_RejectsCorruptArchiveContainer confirms the cheap listing
// path (`backup list`), which opens the archive independently of Restore and
// Verify, classifies the same container failures the same way.
func TestReadManifest_RejectsCorruptArchiveContainer(t *testing.T) {
	for _, tc := range readManifestFormatCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			archive := tc.build(t)
			if _, err := ReadManifest(context.Background(), archive); !errors.Is(err, ErrArchiveFormat) {
				t.Fatalf("err = %v, want ErrArchiveFormat", err)
			}
		})
	}
}

// TestRestore_MissingOrUnreadableArchiveIsNotArchiveFormat guards the other
// side of the fix: a missing file or a permission failure opening the
// archive is a plain I/O problem, not a verdict on the archive's own bytes,
// and must not be swept into the same classification as a corrupt container.
func TestRestore_MissingOrUnreadableArchiveIsNotArchiveFormat(t *testing.T) {
	dest := t.TempDir()

	t.Run("missing file", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "does-not-exist.tar.gz")
		_, err := Restore(context.Background(), missing, RestoreOptions{DestRoot: dest})
		if err == nil {
			t.Fatal("Restore: want an error for a missing archive")
		}
		if errors.Is(err, ErrArchiveFormat) {
			t.Errorf("err = %v, misclassified a missing file as an invalid archive", err)
		}
		if !os.IsNotExist(errors.Unwrap(err)) {
			t.Errorf("err = %v, want it to unwrap to a not-exist error", err)
		}
	})

	t.Run("permission denied", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses file permissions")
		}
		archive := validArchive(t)
		if err := os.Chmod(archive, 0000); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		defer os.Chmod(archive, 0600)

		_, err := Restore(context.Background(), archive, RestoreOptions{DestRoot: dest})
		if err == nil {
			t.Fatal("Restore: want an error for an unreadable archive")
		}
		if errors.Is(err, ErrArchiveFormat) {
			t.Errorf("err = %v, misclassified a permission failure as an invalid archive", err)
		}
	})
}

func TestRestore_DryRunNoWrites(t *testing.T) {
	archive := validArchive(t)
	dest := newDest(t, []destFile{
		{caddyPath, "current caddy\n", 0640},
		{tokenPath, tokenBody, 0600},
	})
	snapRoot := filepath.Join(t.TempDir(), "restore-snapshots")
	before := fingerprint(t, dest)

	res, err := Restore(context.Background(), archive, RestoreOptions{
		DryRun:          true,
		DestRoot:        dest,
		SnapshotRoot:    snapRoot,
		CapturePreimage: true,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !sameFingerprint(before, fingerprint(t, dest)) {
		t.Error("dry-run modified the destination")
	}
	if _, err := os.Stat(snapRoot); !os.IsNotExist(err) {
		t.Errorf("dry-run created a snapshot root: %v", err)
	}
	if res.Preimage != nil {
		t.Error("dry-run captured a pre-restore image")
	}
	if len(res.Restored) != 0 || len(res.Failed) != 0 {
		t.Errorf("restored = %v, failed = %v, want both empty in dry-run", res.Restored, res.Failed)
	}

	actions := map[string]FileDiff{}
	for _, d := range res.Preview {
		actions[d.Path] = d
	}
	if got := actions[caddyPath]; got.Action != ActionOverwrite || got.OldSHA256 != sha256Of("current caddy\n") || got.NewSHA256 != sha256Of(caddyBody) {
		t.Errorf("Caddyfile diff = %+v, want an overwrite with both digests", got)
	}
	if got := actions[tokenPath]; got.Action != ActionUnchanged {
		t.Errorf("token diff = %+v, want unchanged", got)
	}

	dest2 := t.TempDir()
	res2, err := Restore(context.Background(), archive, RestoreOptions{DryRun: true, DestRoot: dest2})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for _, d := range res2.Preview {
		if d.Action != ActionCreate || d.OldSHA256 != "" {
			t.Errorf("diff on an empty destination = %+v, want create", d)
		}
	}
}

func TestRestore_DryRunNoLock(t *testing.T) {
	archive := validArchive(t)
	dest := t.TempDir()
	l := &fakeLocker{}

	if _, err := Restore(context.Background(), archive, RestoreOptions{
		DryRun:   true,
		DestRoot: dest,
		Lock:     l,
	}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if l.tryCalls != 0 || l.unlockCalls != 0 {
		t.Errorf("dry-run touched the lock: %d TryLock, %d Unlock", l.tryCalls, l.unlockCalls)
	}
}

func TestRestore_StagingFailureHasZeroVisibleWrites(t *testing.T) {
	archive := validArchive(t)
	dest := t.TempDir()
	// A regular file where a parent directory must be: staging the second
	// target fails after the first one is already staged.
	blocker := filepath.Join(dest, "var", "lib")
	if err := os.MkdirAll(filepath.Dir(blocker), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(blocker, []byte("not a directory"), 0600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	before := fileFingerprint(t, dest)

	res, err := Restore(context.Background(), archive, RestoreOptions{DestRoot: dest})
	if err == nil {
		t.Fatal("Restore succeeded with an unusable parent path")
	}
	if len(res.Restored) != 0 {
		t.Errorf("restored = %v, want nothing published", res.Restored)
	}
	if res.FailureReasons[tokenPath] != FailStaging {
		t.Errorf("reason for %s = %q, want %q", tokenPath, res.FailureReasons[tokenPath], FailStaging)
	}
	if res.FailureReasons[caddyPath] != FailStagedDiscarded {
		t.Errorf("reason for the already staged %s = %q, want %q", caddyPath, res.FailureReasons[caddyPath], FailStagedDiscarded)
	}
	if len(res.Failed) != 2 {
		t.Errorf("failed = %v, want every path accounted for", res.Failed)
	}
	if !sameFingerprint(before, fileFingerprint(t, dest)) {
		t.Error("a staging failure left visible writes behind")
	}
	if files := tempFilesUnder(t, dest); len(files) != 0 {
		t.Errorf("staging files left behind: %v", files)
	}
}

func TestRestore_RenameFailureReporting(t *testing.T) {
	archive := validArchive(t)
	dest := t.TempDir()
	// A directory where a target file must go: staging succeeds, the rename
	// cannot.
	if err := os.MkdirAll(filepath.Join(dest, filepath.FromSlash(caddyPath)), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	res, err := Restore(context.Background(), archive, RestoreOptions{DestRoot: dest})
	if err == nil {
		t.Fatal("Restore succeeded although one rename could not work")
	}
	if len(res.Restored) != 1 || res.Restored[0] != tokenPath {
		t.Errorf("restored = %v, want the publishable path listed", res.Restored)
	}
	if len(res.Failed) != 1 || res.FailureReasons[caddyPath] != FailPublish {
		t.Errorf("failed = %v, reasons = %v", res.Failed, res.FailureReasons)
	}
	if data, _ := readDestFile(t, dest, tokenPath); data != tokenBody {
		t.Errorf("token = %q, want the published path to hold archived bytes", data)
	}
	if files := tempFilesUnder(t, dest); len(files) != 0 {
		t.Errorf("staging files left behind: %v", files)
	}
}

func TestRestore_Cancellation(t *testing.T) {
	t.Run("before any work", func(t *testing.T) {
		archive := validArchive(t)
		dest := t.TempDir()
		before := fingerprint(t, dest)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := Restore(ctx, archive, RestoreOptions{DestRoot: dest}); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if !sameFingerprint(before, fingerprint(t, dest)) {
			t.Error("a cancelled restore changed the destination")
		}
	})

	t.Run("between publishes", func(t *testing.T) {
		archive := validArchive(t)
		dest := t.TempDir()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		original := publishFileFn
		publishFileFn = func(chain *dirChain, tmp, dst string) error {
			err := publishStagedFile(chain, tmp, dst)
			cancel()
			return err
		}
		defer func() { publishFileFn = original }()

		res, err := Restore(ctx, archive, RestoreOptions{DestRoot: dest})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if len(res.Restored) != 1 || res.Restored[0] != caddyPath {
			t.Errorf("restored = %v, want the already published path", res.Restored)
		}
		if res.FailureReasons[tokenPath] != FailCanceled {
			t.Errorf("reason for %s = %q, want %q", tokenPath, res.FailureReasons[tokenPath], FailCanceled)
		}
		if files := tempFilesUnder(t, dest); len(files) != 0 {
			t.Errorf("staging files left behind after cancellation: %v", files)
		}
	})
}

func TestRestore_FailureReasons(t *testing.T) {
	valid := map[string]bool{
		FailStaging:          true,
		FailPublish:          true,
		FailCanceled:         true,
		FailDeadlineExceeded: true,
		FailNotAttempted:     true,
		FailUnsafePath:       true,
	}

	archive := validArchive(t)
	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, filepath.FromSlash(caddyPath)), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	res, err := Restore(context.Background(), archive, RestoreOptions{DestRoot: dest})
	if err == nil {
		t.Fatal("expected a failing restore")
	}
	if len(res.Failed) == 0 {
		t.Fatal("no failed paths reported")
	}
	for _, p := range res.Failed {
		reason, ok := res.FailureReasons[p]
		if !ok {
			t.Errorf("%s has no reason", p)
			continue
		}
		if !valid[reason] {
			t.Errorf("%s has unstable reason %q", p, reason)
		}
		for _, done := range res.Restored {
			if done == p {
				t.Errorf("%s is reported as both restored and failed", p)
			}
		}
	}
}

func TestRestore_LockReleasedOnAllExitPaths(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, dest string)
		ctx     func() (context.Context, context.CancelFunc)
		wantErr bool
	}{
		{"success", func(t *testing.T, dest string) {}, nil, false},
		{"staging failure", func(t *testing.T, dest string) {
			if err := os.MkdirAll(filepath.Join(dest, "var"), 0755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dest, "var", "lib"), []byte("x"), 0600); err != nil {
				t.Fatalf("write: %v", err)
			}
		}, nil, true},
		{"publish failure", func(t *testing.T, dest string) {
			if err := os.MkdirAll(filepath.Join(dest, filepath.FromSlash(caddyPath)), 0755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
		}, nil, true},
		{"cancelled", func(t *testing.T, dest string) {}, func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			archive := validArchive(t)
			dest := t.TempDir()
			tc.prepare(t, dest)

			ctx := context.Background()
			if tc.ctx != nil {
				c, cancel := tc.ctx()
				defer cancel()
				ctx = c
			}

			l := &fakeLocker{}
			_, err := Restore(ctx, archive, RestoreOptions{DestRoot: dest, Lock: l, LockRetryInterval: time.Millisecond, LockMaxWait: 20 * time.Millisecond})
			if tc.wantErr && err == nil {
				t.Fatal("expected an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Restore: %v", err)
			}
			if l.tryCalls != l.unlockCalls {
				t.Errorf("lock taken %d times, released %d times", l.tryCalls, l.unlockCalls)
			}
		})
	}
}

func TestRestore_ExternalLeaseIsNeitherTakenNorReleased(t *testing.T) {
	archive := validArchive(t)
	dest := t.TempDir()

	held := &fakeLocker{}
	lease, err := AcquireWithRetry(context.Background(), held, time.Millisecond, time.Second)
	if err != nil {
		t.Fatalf("AcquireWithRetry: %v", err)
	}
	own := &fakeLocker{}

	if _, err := Restore(context.Background(), archive, RestoreOptions{DestRoot: dest, Lease: lease, Lock: own}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if own.tryCalls != 0 {
		t.Errorf("Restore acquired its own lock despite being handed a lease")
	}
	if held.unlockCalls != 0 {
		t.Errorf("Restore released a lease it does not own")
	}
	if err := lease.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if held.unlockCalls != 1 {
		t.Errorf("owner released the lease %d times, want 1", held.unlockCalls)
	}
}

func TestRestore_LockBusyRejectsWithoutWriting(t *testing.T) {
	archive := validArchive(t)
	dest := t.TempDir()
	before := fingerprint(t, dest)

	l := &fakeLocker{tryErr: errors.New("held by monitor")}
	_, err := Restore(context.Background(), archive, RestoreOptions{
		DestRoot:          dest,
		Lock:              l,
		LockRetryInterval: time.Millisecond,
		LockMaxWait:       10 * time.Millisecond,
	})
	if !errors.Is(err, ErrLockBusy) {
		t.Fatalf("err = %v, want ErrLockBusy", err)
	}
	if !sameFingerprint(before, fingerprint(t, dest)) {
		t.Error("a busy lock still allowed writes")
	}
}

func TestRestore_UnlockFailureReported(t *testing.T) {
	archive := validArchive(t)
	dest := t.TempDir()

	unlockErr := errors.New("flock release failed")
	l := &fakeLocker{unlockErr: unlockErr}

	res, err := Restore(context.Background(), archive, RestoreOptions{DestRoot: dest, Lock: l})
	if !errors.Is(err, unlockErr) {
		t.Fatalf("err = %v, want the unlock failure", err)
	}
	if len(res.Restored) != 2 {
		t.Errorf("restored = %v, want the restore itself to have succeeded", res.Restored)
	}
}

func TestRestore_CapturesPreimageBeforeWriting(t *testing.T) {
	archive := validArchive(t)
	dest := newDest(t, []destFile{{caddyPath, "current caddy\n", 0640}})
	snapRoot := filepath.Join(t.TempDir(), "restore-snapshots")

	res, err := Restore(context.Background(), archive, RestoreOptions{
		DestRoot:        dest,
		SnapshotRoot:    snapRoot,
		CapturePreimage: true,
		Now:             fixedNow,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.Preimage == nil {
		t.Fatal("no pre-restore image was returned")
	}
	if _, err := os.Stat(res.Preimage.Dir); err != nil {
		t.Fatalf("the image is not on disk: %v", err)
	}

	byPath := map[string]SnapshotEntry{}
	for _, e := range res.Preimage.Entries {
		byPath[e.Path] = e
	}
	if got := byPath[caddyPath]; !got.Exists || got.SHA256 != sha256Of("current caddy\n") {
		t.Errorf("image entry for %s = %+v, want the pre-restore bytes", caddyPath, got)
	}
	if got := byPath[tokenPath]; got.Exists {
		t.Errorf("image entry for %s = %+v, want Exists=false", tokenPath, got)
	}

	// The image is exactly what a rollback needs to undo this restore.
	if _, err := Rollback(context.Background(), res.Preimage, RollbackOptions{DestRoot: dest}); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if data, _ := readDestFile(t, dest, caddyPath); data != "current caddy\n" {
		t.Errorf("Caddyfile = %q, want the pre-restore content", data)
	}
	if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(tokenPath))); !os.IsNotExist(err) {
		t.Errorf("the file the restore created survived the rollback: %v", err)
	}
}

func TestRestore_RequiresSnapshotRootForPreimage(t *testing.T) {
	archive := validArchive(t)
	if _, err := Restore(context.Background(), archive, RestoreOptions{DestRoot: t.TempDir(), CapturePreimage: true}); err == nil {
		t.Fatal("Restore captured an image without a snapshot root")
	}
}

func TestRestore_RequiresDestRoot(t *testing.T) {
	archive := validArchive(t)
	if _, err := Restore(context.Background(), archive, RestoreOptions{}); err == nil {
		t.Fatal("Restore accepted an empty DestRoot")
	}
}

func TestReadManifest_ReadsWithoutRehashing(t *testing.T) {
	m, entries := validParts(t)
	// Corrupt a payload: ReadManifest must still answer, because it is the
	// cheap listing path and does not verify content.
	entries[1].data = "corrupted\n"
	archive := writeRawArchive(t, filepath.Join(t.TempDir(), "a.tar.gz"), entries)

	got, err := ReadManifest(context.Background(), archive)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if got.ProxyctlVersion != m.ProxyctlVersion || !got.CreatedAt.Equal(m.CreatedAt) || len(got.Files) != len(m.Files) {
		t.Errorf("manifest = %+v, want the archived one", got)
	}
	if _, err := Verify(context.Background(), archive); !errors.Is(err, ErrChecksumMismatch) {
		t.Errorf("Verify err = %v, want the corruption to be caught there", err)
	}
}

func TestReadManifest_RejectsUnsupportedSchema(t *testing.T) {
	m, entries := validParts(t)
	m.SchemaVersion = 99
	raw, err := json.Marshal(map[string]any{
		"schema_version": 99,
		"created_at":     m.CreatedAt,
		"files":          []any{},
		"skipped":        []string{},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	entries[0] = rawEntry{name: ManifestName, data: string(raw)}
	archive := writeRawArchive(t, filepath.Join(t.TempDir(), "a.tar.gz"), entries)

	if _, err := ReadManifest(context.Background(), archive); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("err = %v, want ErrUnsupportedSchema", err)
	}
}

func TestReadManifest_MissingManifest(t *testing.T) {
	_, entries := validParts(t)
	archive := writeRawArchive(t, filepath.Join(t.TempDir(), "a.tar.gz"), entries[1:])
	if _, err := ReadManifest(context.Background(), archive); !errors.Is(err, ErrArchiveContentMismatch) {
		t.Fatalf("err = %v, want ErrArchiveContentMismatch", err)
	}
}

func TestReadManifest_Cancellation(t *testing.T) {
	archive := validArchive(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadManifest(ctx, archive); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestResolveUnderRoot_KeepsPathsInside(t *testing.T) {
	root := t.TempDir()
	if _, err := resolveUnderRoot(root, "../escape"); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("traversal was accepted")
	}
	if _, err := resolveUnderRoot(root, "/etc/passwd"); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("absolute path was accepted")
	}
	got, err := resolveUnderRoot(root, "etc/caddy/Caddyfile")
	if err != nil {
		t.Fatalf("resolveUnderRoot: %v", err)
	}
	if want := filepath.Join(root, "etc/caddy/Caddyfile"); got != want {
		t.Errorf("resolved = %q, want %q", got, want)
	}
}

func TestRestore_RejectsSymlinkedParentDirectory(t *testing.T) {
	archive := validArchive(t)
	base := t.TempDir()
	dest := filepath.Join(base, "dest")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{dest, outside, filepath.Join(dest, "etc")} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	// A parent directory inside the destination points outside it. String
	// checks alone would let an ordinary-looking archive path write there.
	if err := os.Symlink(outside, filepath.Join(dest, "etc", "caddy")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	l := &fakeLocker{}

	res, err := Restore(context.Background(), archive, RestoreOptions{DestRoot: dest, Lock: l})
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath", err)
	}
	if len(res.Restored) != 0 {
		t.Errorf("restored = %v, want nothing written", res.Restored)
	}
	if names := dirNames(t, outside); len(names) != 0 {
		t.Errorf("the restore wrote %v outside the destination root", names)
	}
	if l.tryCalls != l.unlockCalls {
		t.Errorf("lock taken %d times, released %d", l.tryCalls, l.unlockCalls)
	}
}

func TestCaptureSnapshot_RejectsSymlinkedParentDirectory(t *testing.T) {
	base := t.TempDir()
	dest := filepath.Join(base, "dest")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(filepath.Join(dest, "etc"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(outside, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "Caddyfile"), []byte("secret\n"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dest, "etc", "caddy")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := CaptureSnapshot(context.Background(), CaptureOptions{
		DestRoot:     dest,
		SnapshotRoot: filepath.Join(base, "snapshots"),
		Paths:        []string{caddyPath},
		Now:          fixedNow,
	})
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath", err)
	}
}

func TestRollback_RejectsSymlinkedParentDirectory(t *testing.T) {
	base := t.TempDir()
	dest := filepath.Join(base, "dest")
	if err := os.MkdirAll(filepath.Join(dest, "etc", "caddy"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dest, filepath.FromSlash(caddyPath)), []byte("original\n"), 0640); err != nil {
		t.Fatalf("write: %v", err)
	}
	snap, err := CaptureSnapshot(context.Background(), CaptureOptions{
		DestRoot:     dest,
		SnapshotRoot: filepath.Join(base, "snapshots"),
		Paths:        []string{caddyPath},
		Now:          fixedNow,
	})
	if err != nil {
		t.Fatalf("CaptureSnapshot: %v", err)
	}

	// Between capture and rollback the parent becomes a link out of the tree.
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(dest, "etc", "caddy")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dest, "etc", "caddy")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	res, err := Rollback(context.Background(), snap, RollbackOptions{DestRoot: dest})
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath", err)
	}
	if res.FailureReasons[caddyPath] != FailUnsafePath {
		t.Errorf("reasons = %v", res.FailureReasons)
	}
	if names := dirNames(t, outside); len(names) != 0 {
		t.Errorf("the rollback wrote %v outside the destination root", names)
	}
}

func TestRestore_StagingFailureRemovesDirectoriesItCreated(t *testing.T) {
	archive := validArchive(t)
	dest := t.TempDir()
	blocker := filepath.Join(dest, "var", "lib")
	if err := os.MkdirAll(filepath.Dir(blocker), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(blocker, []byte("not a directory"), 0600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	if _, err := Restore(context.Background(), archive, RestoreOptions{DestRoot: dest}); err == nil {
		t.Fatal("expected a staging failure")
	}
	if _, err := os.Stat(filepath.Join(dest, "etc")); !os.IsNotExist(err) {
		t.Errorf("a directory created by the failed restore survived: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(blocker)); err != nil {
		t.Errorf("a directory the restore did not create was removed: %v", err)
	}
}

func TestRestore_CreatesParentDirectoriesIndependentlyOfUmask(t *testing.T) {
	archive := validArchive(t)
	dest := t.TempDir()

	// umask is process-wide: no test in this package may run in parallel.
	old := syscall.Umask(0777)
	defer syscall.Umask(old)

	if _, err := Restore(context.Background(), archive, RestoreOptions{DestRoot: dest}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for _, dir := range []string{"etc", "etc/caddy", "var/lib/singbox-sub-manager"} {
		info, err := os.Stat(filepath.Join(dest, filepath.FromSlash(dir)))
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if got := info.Mode().Perm(); got != restoreDirMode {
			t.Errorf("%s mode = %o, want %o", dir, got, restoreDirMode)
		}
	}
}

func TestRestore_Timeout(t *testing.T) {
	archive := validArchive(t)
	dest := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	original := publishFileFn
	publishFileFn = func(chain *dirChain, tmp, dst string) error {
		err := publishStagedFile(chain, tmp, dst)
		// Let the deadline pass before the next path is published.
		<-ctx.Done()
		return err
	}
	defer func() { publishFileFn = original }()

	res, err := Restore(ctx, archive, RestoreOptions{DestRoot: dest})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if len(res.Restored) != 1 || res.Restored[0] != caddyPath {
		t.Errorf("restored = %v, want the path published before the deadline", res.Restored)
	}
	if res.FailureReasons[tokenPath] != FailDeadlineExceeded {
		t.Errorf("reason for %s = %q, want %q", tokenPath, res.FailureReasons[tokenPath], FailDeadlineExceeded)
	}
	if files := tempFilesUnder(t, dest); len(files) != 0 {
		t.Errorf("staging files left behind after the deadline: %v", files)
	}
}

func TestRestore_LockReleasedOnPanic(t *testing.T) {
	archive := validArchive(t)
	dest := t.TempDir()
	l := &fakeLocker{}

	original := publishFileFn
	publishFileFn = func(chain *dirChain, tmp, dst string) error { panic("publish exploded") }
	defer func() { publishFileFn = original }()

	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected the injected panic to propagate")
			}
		}()
		_, _ = Restore(context.Background(), archive, RestoreOptions{DestRoot: dest, Lock: l})
	}()

	if l.tryCalls != 1 || l.unlockCalls != 1 {
		t.Errorf("lock taken %d times, released %d times across a panic", l.tryCalls, l.unlockCalls)
	}
}

func TestRestore_PrunesStaleSnapshotsBeforeCapturing(t *testing.T) {
	archive := validArchive(t)
	dest := t.TempDir()
	snapRoot := filepath.Join(t.TempDir(), "restore-snapshots")

	stale, err := CaptureSnapshot(context.Background(), CaptureOptions{
		DestRoot:     dest,
		SnapshotRoot: snapRoot,
		Paths:        []string{caddyPath},
		Now:          func() time.Time { return fixedClock.Add(-30 * 24 * time.Hour) },
	})
	if err != nil {
		t.Fatalf("CaptureSnapshot: %v", err)
	}
	keep, err := CaptureSnapshot(context.Background(), CaptureOptions{
		DestRoot:     dest,
		SnapshotRoot: snapRoot,
		Paths:        []string{caddyPath},
		Now:          func() time.Time { return fixedClock.Add(-20 * 24 * time.Hour) },
	})
	if err != nil {
		t.Fatalf("CaptureSnapshot: %v", err)
	}

	res, err := Restore(context.Background(), archive, RestoreOptions{
		DestRoot:        dest,
		SnapshotRoot:    snapRoot,
		CapturePreimage: true,
		Now:             fixedNow,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := os.Stat(stale.Dir); !os.IsNotExist(err) {
		t.Errorf("the stale snapshot survived the restore: %v", err)
	}
	if _, err := os.Stat(keep.Dir); err != nil {
		t.Errorf("the most recent old snapshot was pruned: %v", err)
	}
	if _, err := os.Stat(res.Preimage.Dir); err != nil {
		t.Errorf("this restore's image is missing: %v", err)
	}
}

// --- Task 2 acceptance matrix (design §7.1): T1-写, T2-写, P1, T7, T9, T10 ---
//
// The symlink-replacement tests below all use the technique design §7.1
// specifies: the real directory is renamed within root, then a symlink to
// an outside location takes its original name. Whatever fd this package
// already holds for that directory still refers to the real (now renamed)
// inode; only a re-resolution by name would follow the symlink.

// T1-写: fd-relative staging and publish.
func TestRestore_T1Write_SucceedsUnderTheRenamedDirectory(t *testing.T) {
	archive := validArchive(t)
	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, "etc", "caddy"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	outside := t.TempDir()

	// Both "etc" and "caddy" pre-exist, so resolveUnderTrustedRoot's own
	// walk takes the EEXIST branch for each (one hookBeforeMutation call
	// per component) before returning the fully-resolved chain to Restore;
	// its own pre-staging re-verify is the 3rd call — the one positioned,
	// per design §7.1's T1-写 row, right before the staging Openat(O_CREAT).
	calls := 0
	orig := hookBeforeMutation
	defer func() { hookBeforeMutation = orig }()
	hookBeforeMutation = func() {
		calls++
		if calls != 3 {
			return
		}
		if err := os.Rename(filepath.Join(dest, "etc", "caddy"), filepath.Join(dest, "etc", "caddy.moved")); err != nil {
			t.Fatalf("rename: %v", err)
		}
		if err := os.Symlink(outside, filepath.Join(dest, "etc", "caddy")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	}

	res, err := Restore(context.Background(), archive, RestoreOptions{DestRoot: dest})
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath", err)
	}
	if calls < 3 {
		t.Fatalf("hookBeforeMutation fired %d times, want at least 3 (the injection never ran)", calls)
	}
	if res.FailureReasons[caddyPath] != FailUnsafePath {
		t.Errorf("reason for %s = %q, want %q", caddyPath, res.FailureReasons[caddyPath], FailUnsafePath)
	}
	if names := dirNames(t, outside); len(names) != 0 {
		t.Errorf("the restore wrote %v outside the destination root", names)
	}
}

// T2-写: the ancestor-chain re-verification catches a directory moved
// entirely out of root, in the window between opening it and re-verifying.
func TestRestore_T2Write_AncestorReverificationCatchesAMoveOutOfRoot(t *testing.T) {
	archive := validArchive(t)
	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, "etc", "caddy"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	outside := t.TempDir()

	// Both "etc" and "caddy" pre-exist, so resolveUnderTrustedRoot's own
	// walk takes the EEXIST branch for each (one hookAfterOpen call per
	// component, neither doing anything more than a trivial pre-Mkdirat
	// check) before returning the fully-resolved chain to Restore, whose
	// own pre-staging re-verify is the 3rd call — that is where "caddy"
	// (already open, held from the walk above) needs to move for this test
	// to isolate the re-verification Restore's own staging step performs,
	// rather than the one already covered at the private level (Task 1).
	calls := 0
	orig := hookAfterOpen
	defer func() { hookAfterOpen = orig }()
	hookAfterOpen = func() {
		calls++
		if calls != 3 {
			return
		}
		if err := os.Rename(filepath.Join(dest, "etc", "caddy"), filepath.Join(outside, "caddy")); err != nil {
			t.Fatalf("rename: %v", err)
		}
	}

	res, err := Restore(context.Background(), archive, RestoreOptions{DestRoot: dest})
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath", err)
	}
	if calls < 3 {
		t.Fatalf("hookAfterOpen fired %d times, want at least 3 (the injection never ran)", calls)
	}
	if res.FailureReasons[caddyPath] != FailUnsafePath {
		t.Errorf("reason for %s = %q, want %q", caddyPath, res.FailureReasons[caddyPath], FailUnsafePath)
	}
	if names := dirNames(t, filepath.Join(outside, "caddy")); len(names) != 0 {
		t.Errorf("the restore wrote into the moved-out directory: %v", names)
	}
}

// Regression: failure cleanup must remove a directory the currently-failing
// chain itself created, not just directories created by an earlier,
// already-staged file. createdDirs entries record the fd of the directory's
// *parent*; if that fd belongs to the chain that is about to fail and gets
// closed before cleanup runs, the cleanup call silently no-ops (Unlinkat on
// a closed fd) and the directory survives.
func TestRestore_FailureCleanupRemovesADirectoryCreatedByTheSameFailingChain(t *testing.T) {
	archive := validArchive(t)
	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, "etc"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// "caddy" does not exist yet: this restore itself must create it
	// (recorded into createdDirs, parent fd = "etc"'s) before its own
	// pre-staging re-verify fails — by renaming "etc" itself (an ancestor,
	// not "caddy"), so "caddy" is left behind fully intact, as a plain
	// empty directory, now under "etc.moved". Cleanup must still remove it:
	// the parent fd it needs (etc's) was opened long before the rename and
	// is unaffected by renaming the directory it refers to — unless that fd
	// was already (incorrectly) closed before cleanup ran.
	calls := 0
	orig := hookAfterOpen
	defer func() { hookAfterOpen = orig }()
	hookAfterOpen = func() {
		calls++
		if calls != 4 {
			return
		}
		if err := os.Rename(filepath.Join(dest, "etc"), filepath.Join(dest, "etc.moved")); err != nil {
			t.Fatalf("rename: %v", err)
		}
	}

	res, err := Restore(context.Background(), archive, RestoreOptions{DestRoot: dest})
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath", err)
	}
	if calls < 4 {
		t.Fatalf("hookAfterOpen fired %d times, want at least 4 (the injection never ran)", calls)
	}
	if res.FailureReasons[caddyPath] != FailUnsafePath {
		t.Errorf("reason for %s = %q, want %q", caddyPath, res.FailureReasons[caddyPath], FailUnsafePath)
	}
	if _, statErr := os.Stat(filepath.Join(dest, "etc.moved", "caddy")); !os.IsNotExist(statErr) {
		t.Errorf("the directory this restore itself created, under the renamed ancestor, was not cleaned up: stat err = %v", statErr)
	}
}

// Design §6.7's pre-delete identity comparison: a directory replaced by a
// different, still-empty real directory would pass AT_REMOVEDIR's own
// emptiness check regardless — it is not ENOTEMPTY, since T10 already
// covers that case with a non-empty replacement — so only the identity
// comparison against the fd held since creation can tell them apart.
func TestRestore_FailureCleanupSkipsADirectoryReplacedByADifferentEmptyOne(t *testing.T) {
	archive := validArchive(t)
	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, "etc"), 0755); err != nil {
		t.Fatalf("mkdir etc: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dest, "var"), 0755); err != nil {
		t.Fatalf("mkdir var: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "var", "lib"), []byte("not a directory"), 0600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	// "caddy" does not exist yet: this restore creates it, stages Caddyfile
	// under it, and moves on to "token" — whose "lib" component is blocked,
	// triggering abandonStaging. Somewhere in token's own resolution (call
	// #6: "var" is call #1 via resolveUnderTrustedRoot's own trivial
	// pre-Mkdirat check plus this test's earlier caddy creation accounting
	// for #2-4, "var"'s EEXIST branch is #5, "lib"'s pre-Mkdirat check is
	// #6), replace "caddy" — well after it was fully created and verified —
	// with a different, still-empty real directory.
	calls := 0
	orig := hookAfterOpen
	defer func() { hookAfterOpen = orig }()
	hookAfterOpen = func() {
		calls++
		if calls != 6 {
			return
		}
		// caddy already holds this run's own staged Caddyfile tmp file by
		// this point (staging happened at hookAfterOpen call #4, above) —
		// RemoveAll clears it before the empty replacement takes its place.
		caddy := filepath.Join(dest, "etc", "caddy")
		if err := os.RemoveAll(caddy); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if err := os.Mkdir(caddy, 0700); err != nil {
			t.Fatalf("mkdir replacement: %v", err)
		}
	}

	res, err := Restore(context.Background(), archive, RestoreOptions{DestRoot: dest})
	if err == nil {
		t.Fatal("Restore succeeded although the token path is blocked")
	}
	if calls < 6 {
		t.Fatalf("hookAfterOpen fired %d times, want at least 6 (the injection never ran)", calls)
	}
	if res.FailureReasons[caddyPath] != FailStagedDiscarded {
		t.Errorf("reason for %s = %q, want %q", caddyPath, res.FailureReasons[caddyPath], FailStagedDiscarded)
	}
	if _, statErr := os.Stat(filepath.Join(dest, "etc", "caddy")); statErr != nil {
		t.Errorf("the replacement directory (not the one this run created) was deleted: %v", statErr)
	}
	if len(res.Warnings) == 0 {
		t.Errorf("want a warning that the replaced directory needs manual confirmation")
	}
}

// Design §6.7's pre-delete identity comparison, staged-file half (the
// directory half is covered above): a staging file replaced — same name,
// different inode — before this run's own failure cleanup gets to it must
// be left alone, not deleted by name.
func TestRestore_FailureCleanupSkipsAStagedFileReplacedByADifferentFile(t *testing.T) {
	archive := validArchive(t)
	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, "etc"), 0755); err != nil {
		t.Fatalf("mkdir etc: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dest, "var"), 0755); err != nil {
		t.Fatalf("mkdir var: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "var", "lib"), []byte("not a directory"), 0600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	// Same call-count reasoning as the directory-replacement test above:
	// call #6 is "lib"'s pre-Mkdirat check, well after caddy's own staging
	// file (created at call #4's step) already exists.
	calls := 0
	orig := hookAfterOpen
	defer func() { hookAfterOpen = orig }()
	hookAfterOpen = func() {
		calls++
		if calls != 6 {
			return
		}
		entries, err := os.ReadDir(filepath.Join(dest, "etc", "caddy"))
		if err != nil {
			t.Fatalf("read caddy dir: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("caddy dir has %d entries, want 1 (the staging file)", len(entries))
		}
		tmp := filepath.Join(dest, "etc", "caddy", entries[0].Name())
		if err := os.Remove(tmp); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if err := os.WriteFile(tmp, []byte("not this run's staging file"), 0600); err != nil {
			t.Fatalf("write replacement: %v", err)
		}
	}

	res, err := Restore(context.Background(), archive, RestoreOptions{DestRoot: dest})
	if err == nil {
		t.Fatal("Restore succeeded although the token path is blocked")
	}
	if calls < 6 {
		t.Fatalf("hookAfterOpen fired %d times, want at least 6 (the injection never ran)", calls)
	}
	entries, readErr := os.ReadDir(filepath.Join(dest, "etc", "caddy"))
	if readErr != nil {
		t.Fatalf("read caddy dir: %v", readErr)
	}
	if len(entries) != 1 {
		t.Errorf("caddy dir entries = %v, want the replacement file left untouched", entries)
	} else if data, _ := os.ReadFile(filepath.Join(dest, "etc", "caddy", entries[0].Name())); string(data) != "not this run's staging file" {
		t.Errorf("the replacement file was deleted or modified: %q", data)
	}
	if len(res.Warnings) == 0 {
		t.Errorf("want a warning that the replaced staging file needs manual confirmation")
	}
}

// P1a: static-layout regression for the preview path (two independent
// defenses — Fstatat's own type check and the O_NOFOLLOW read — so no
// single mutation isolates it; see design §7.1's P1 row).
func TestRestore_P1a_PreviewSymlinkRegression(t *testing.T) {
	archive := validArchive(t)

	t.Run("a symlinked leaf is an overwrite with no old digest", func(t *testing.T) {
		dest := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dest, "etc", "caddy"), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(dest, "var", "lib", "singbox-sub-manager"), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		outsideFile := filepath.Join(t.TempDir(), "secret")
		if err := os.WriteFile(outsideFile, []byte("secret\n"), 0600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := os.Symlink(outsideFile, filepath.Join(dest, filepath.FromSlash(caddyPath))); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		res, err := Restore(context.Background(), archive, RestoreOptions{DryRun: true, DestRoot: dest})
		if err != nil {
			t.Fatalf("Restore: %v", err)
		}
		got := diffFor(res.Preview, caddyPath)
		if got.Action != ActionOverwrite || got.OldSHA256 != "" {
			t.Errorf("diff = %+v, want ActionOverwrite with no old digest", got)
		}
	})

	t.Run("a symlinked parent is rejected", func(t *testing.T) {
		dest := t.TempDir()
		outside := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dest, "etc"), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.Symlink(outside, filepath.Join(dest, "etc", "caddy")); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		_, err := Restore(context.Background(), archive, RestoreOptions{DryRun: true, DestRoot: dest})
		if !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("err = %v, want ErrUnsafePath", err)
		}
	})
}

// P1b: the preview read of a leaf already proven regular by Fstatat does not
// follow a symlink swapped in right before the read.
func TestRestore_P1b_PreviewDoesNotFollowSymlinkSwappedInBeforeRead(t *testing.T) {
	archive := validArchive(t)
	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, "etc", "caddy"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dest, filepath.FromSlash(caddyPath)), []byte(caddyBody), 0640); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dest, "var", "lib", "singbox-sub-manager"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Same bytes as the archive: a bug that follows the symlink would read
	// this and misreport the diff as ActionUnchanged instead of Overwrite.
	outsideFile := filepath.Join(t.TempDir(), "outside-caddy")
	if err := os.WriteFile(outsideFile, []byte(caddyBody), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	orig := hookBeforeMutation
	defer func() { hookBeforeMutation = orig }()
	hookBeforeMutation = func() {
		target := filepath.Join(dest, filepath.FromSlash(caddyPath))
		if err := os.Remove(target); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if err := os.Symlink(outsideFile, target); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	}

	res, err := Restore(context.Background(), archive, RestoreOptions{DryRun: true, DestRoot: dest})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got := diffFor(res.Preview, caddyPath)
	if got.Action != ActionOverwrite || got.OldSHA256 != "" {
		t.Errorf("diff = %+v, want ActionOverwrite with no old digest (must not follow the swapped-in symlink)", got)
	}
}

func diffFor(preview []FileDiff, path string) FileDiff {
	for _, d := range preview {
		if d.Path == path {
			return d
		}
	}
	return FileDiff{}
}

// T7: an existing parent directory's permissions and ownership are left
// untouched, end to end.
func TestRestore_T7_ExistingDirectoryPermissionsNotWidened(t *testing.T) {
	archive := validArchive(t)
	dest := t.TempDir()
	existing := filepath.Join(dest, "etc", "caddy")
	if err := os.MkdirAll(existing, 0750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	wantUID, wantGID := uint32(os.Geteuid()), uint32(os.Getegid())
	if os.Geteuid() == 0 {
		wantUID, wantGID = 4321, 4322
		if err := os.Chown(existing, int(wantUID), int(wantGID)); err != nil {
			t.Fatalf("chown: %v", err)
		}
	} else {
		t.Logf("not running as root: ownership is only checked against %d:%d", wantUID, wantGID)
	}

	res, err := Restore(context.Background(), archive, RestoreOptions{DestRoot: dest})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("failed = %v, reasons %v", res.Failed, res.FailureReasons)
	}
	if mode := permOf(t, existing); mode != 0750 {
		t.Errorf("existing directory mode = %v, want unchanged 0750", mode)
	}
	if uid, gid := ownerOf(t, existing); uid != wantUID || gid != wantGID {
		t.Errorf("existing directory ownership = %d:%d, want unchanged %d:%d", uid, gid, wantUID, wantGID)
	}
	// A directory this restore itself had to create still gets the default
	// mode, regardless of the existing sibling above.
	created := filepath.Join(dest, "var", "lib", "singbox-sub-manager")
	if mode := permOf(t, created); mode != restoreDirMode {
		t.Errorf("newly created directory mode = %v, want %v", mode, restoreDirMode)
	}
}

// T9: failure cleanup does not itself re-verify (design §6.7's deliberate
// exception) — otherwise the very replacement that triggered the cleanup
// would also block it, leaving the staging file behind.
func TestRestore_T9_FailureCleanupSkipsReverification(t *testing.T) {
	archive := validArchive(t)
	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, "etc", "caddy"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	outside := t.TempDir()

	// Same 3rd-call targeting as T1-write above: the swap needs to happen at
	// Restore's own pre-staging re-verify so that staging actually succeeds
	// under the moved directory first, giving the later publish-time
	// re-verify failure something to clean up.
	calls := 0
	orig := hookBeforeMutation
	defer func() { hookBeforeMutation = orig }()
	hookBeforeMutation = func() {
		calls++
		if calls != 3 {
			return
		}
		if err := os.Rename(filepath.Join(dest, "etc", "caddy"), filepath.Join(dest, "etc", "caddy.moved")); err != nil {
			t.Fatalf("rename: %v", err)
		}
		if err := os.Symlink(outside, filepath.Join(dest, "etc", "caddy")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	}

	res, err := Restore(context.Background(), archive, RestoreOptions{DestRoot: dest})
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath", err)
	}
	if calls < 3 {
		t.Fatalf("hookBeforeMutation fired %d times, want at least 3 (the injection never ran)", calls)
	}
	if res.FailureReasons[caddyPath] != FailUnsafePath {
		t.Errorf("reason for %s = %q, want %q", caddyPath, res.FailureReasons[caddyPath], FailUnsafePath)
	}
	if files := tempFilesUnder(t, filepath.Join(dest, "etc", "caddy.moved")); len(files) != 0 {
		t.Errorf("staging files left behind under the moved directory: %v (cleanup must not skip on a re-verify failure)", files)
	}
}

// T10: the structural guarantee that survives even the one race this
// package cannot close (design §3.3's safety premise B) — Mkdirat succeeding
// does not prove the fd opened right after it is the directory this call
// just created, so failure cleanup must never recurse or it could delete
// content a swapped-in real directory already held.
func TestRestore_T10_RealDirectoryReplacementStructuralGuarantee(t *testing.T) {
	archive := validArchive(t)
	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, "etc"), 0755); err != nil {
		t.Fatalf("mkdir etc: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dest, "var"), 0755); err != nil {
		t.Fatalf("mkdir var: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "var", "lib"), []byte("not a directory"), 0600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	replacement := filepath.Join(t.TempDir(), "replacement")
	if err := os.MkdirAll(replacement, 0750); err != nil {
		t.Fatalf("mkdir replacement: %v", err)
	}
	if err := os.WriteFile(filepath.Join(replacement, "keep"), []byte("keep me\n"), 0600); err != nil {
		t.Fatalf("write keep: %v", err)
	}

	fired := false
	orig := hookAfterMkdirat
	defer func() { hookAfterMkdirat = orig }()
	hookAfterMkdirat = func() {
		if fired {
			return
		}
		fired = true
		caddy := filepath.Join(dest, "etc", "caddy")
		if err := os.Rename(caddy, filepath.Join(dest, "etc", "caddy.orig")); err != nil {
			t.Fatalf("rename away: %v", err)
		}
		if err := os.Rename(replacement, caddy); err != nil {
			t.Fatalf("swap in: %v", err)
		}
	}

	res, err := Restore(context.Background(), archive, RestoreOptions{DestRoot: dest})
	if err == nil {
		t.Fatal("Restore succeeded, want a staging failure once the swapped-in directory is discovered non-empty")
	}
	if res.FailureReasons[tokenPath] != FailStaging {
		t.Errorf("reason for %s = %q, want %q", tokenPath, res.FailureReasons[tokenPath], FailStaging)
	}
	if res.FailureReasons[caddyPath] != FailStagedDiscarded {
		t.Errorf("reason for %s = %q, want %q", caddyPath, res.FailureReasons[caddyPath], FailStagedDiscarded)
	}

	// The structural guarantee (design §3.3 guarantee 4, §6.7): failure
	// cleanup never recurses and never deletes a non-empty directory. It
	// does remove, by fd, the staging file this run itself put into the
	// swapped-in directory.
	entries, readErr := os.ReadDir(filepath.Join(dest, "etc", "caddy"))
	if readErr != nil {
		t.Fatalf("read swapped-in directory: %v", readErr)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "keep" {
		t.Errorf("swapped-in directory contents = %v, want only \"keep\" (the staging file must be gone, keep untouched)", names)
	}
	if _, statErr := os.Stat(filepath.Join(dest, "etc", "caddy.orig")); statErr != nil {
		t.Errorf("the original, empty directory was removed: %v", statErr)
	}
	// Deliberately not asserted: the swapped-in directory's own mode.
	// Design §6.3/§7.1 T10 flags Fchmod landing on it as a known limitation
	// (safety premise B), not a guarantee this test locks in.
}
