package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var fixedClock = time.Date(2026, 8, 17, 4, 5, 6, 0, time.UTC)

func fixedNow() time.Time { return fixedClock }

type sourceFile struct {
	rel  string
	data string
	mode fs.FileMode
}

var defaultSource = []sourceFile{
	{"etc/singbox-sub-manager/nodes.conf", "[hk]\nSERVER=1.2.3.4\n", 0600},
	{"etc/singbox-sub-manager/templates/clash.yaml", "proxies: []\n", 0644},
	{"etc/singbox-sub-manager/certs/server.key", "PRIVATE KEY\n", 0600},
	{"etc/sing-box/config.json", `{"log":{"level":"info"}}`, 0600},
	{"etc/caddy/Caddyfile", "sub.example.com {\n}\n", 0640},
	{"var/lib/singbox-sub-manager/token", "deadbeef\n", 0600},
	{"var/lib/singbox-sub-manager/monitor-state.json", `{"schema_version":1}`, 0600},
	{"var/lib/singbox-sub-manager/monitor-paused", "", 0600},
}

func writeSourceFile(t *testing.T, root string, f sourceFile) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(f.rel))
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatalf("mkdir for %s: %v", f.rel, err)
	}
	if err := os.WriteFile(p, []byte(f.data), f.mode); err != nil {
		t.Fatalf("write %s: %v", f.rel, err)
	}
	if err := os.Chmod(p, f.mode); err != nil {
		t.Fatalf("chmod %s: %v", f.rel, err)
	}
	return p
}

// newSource materializes the given files under a fresh source root.
func newSource(t *testing.T, files []sourceFile) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		writeSourceFile(t, root, f)
	}
	return root
}

type archiveEntry struct {
	header tar.Header
	data   []byte
}

func readArchiveEntries(t *testing.T, path string) []archiveEntry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var entries []archiveEntry
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read entry %s: %v", hdr.Name, err)
		}
		entries = append(entries, archiveEntry{header: *hdr, data: data})
	}
	return entries
}

func entryByName(t *testing.T, entries []archiveEntry, name string) archiveEntry {
	t.Helper()
	for _, e := range entries {
		if e.header.Name == name {
			return e
		}
	}
	t.Fatalf("archive entry %q not found", name)
	return archiveEntry{}
}

func manifestPaths(m Manifest) map[string]FileEntry {
	out := make(map[string]FileEntry, len(m.Files))
	for _, f := range m.Files {
		out[f.Path] = f
	}
	return out
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read dir %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestCreate_AllPathsPresent(t *testing.T) {
	src := newSource(t, defaultSource)
	backupDir := filepath.Join(t.TempDir(), "backups")

	m, err := Create(context.Background(), CreateOptions{
		SourceRoot:      src,
		BackupDir:       backupDir,
		ProxyctlVersion: "v0.6.0",
		Now:             fixedNow,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if m.SchemaVersion != SchemaVersion {
		t.Errorf("schema version = %d, want %d", m.SchemaVersion, SchemaVersion)
	}
	if !m.CreatedAt.Equal(fixedClock) {
		t.Errorf("created_at = %s, want %s", m.CreatedAt, fixedClock)
	}
	if m.ProxyctlVersion != "v0.6.0" {
		t.Errorf("proxyctl_version = %q", m.ProxyctlVersion)
	}
	if len(m.Skipped) != 0 {
		t.Errorf("skipped = %v, want none", m.Skipped)
	}
	if len(m.Files) != len(defaultSource) {
		t.Fatalf("files = %d, want %d", len(m.Files), len(defaultSource))
	}
	packed := manifestPaths(m)
	for _, f := range defaultSource {
		if _, ok := packed[f.rel]; !ok {
			t.Errorf("missing manifest entry for %s", f.rel)
		}
	}
	if got := filepath.Base(m.ArchivePath); !IsArchiveName(got) {
		t.Errorf("archive name %q does not match the naming rule", got)
	}
	if want := filepath.Join(backupDir, "backup-20260817T040506Z.tar.gz"); m.ArchivePath != want {
		t.Errorf("archive path = %q, want %q", m.ArchivePath, want)
	}
}

func TestCreate_ChecksumMatches(t *testing.T) {
	src := newSource(t, defaultSource)
	backupDir := filepath.Join(t.TempDir(), "backups")

	m, err := Create(context.Background(), CreateOptions{SourceRoot: src, BackupDir: backupDir, Now: fixedNow})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	entries := readArchiveEntries(t, m.ArchivePath)
	for _, f := range m.Files {
		archived := entryByName(t, entries, f.Path)
		sum := sha256.Sum256(archived.data)
		if got := hex.EncodeToString(sum[:]); got != f.SHA256 {
			t.Errorf("%s: archived checksum %s, manifest %s", f.Path, got, f.SHA256)
		}
		onDisk, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(f.Path)))
		if err != nil {
			t.Fatalf("read source %s: %v", f.Path, err)
		}
		srcSum := sha256.Sum256(onDisk)
		if hex.EncodeToString(srcSum[:]) != f.SHA256 {
			t.Errorf("%s: manifest checksum does not match the source file", f.Path)
		}
	}

	var decoded Manifest
	if err := json.Unmarshal(entryByName(t, entries, ManifestName).data, &decoded); err != nil {
		t.Fatalf("decode archived manifest: %v", err)
	}
	if err := decoded.Validate(); err != nil {
		t.Errorf("archived manifest invalid: %v", err)
	}
	if len(decoded.Files) != len(m.Files) {
		t.Errorf("archived manifest has %d files, returned manifest has %d", len(decoded.Files), len(m.Files))
	}
}

func TestCreate_SkipsMissingPaths(t *testing.T) {
	var present []sourceFile
	for _, f := range defaultSource {
		if f.rel == "var/lib/singbox-sub-manager/token" || f.rel == "var/lib/singbox-sub-manager/monitor-paused" {
			continue
		}
		present = append(present, f)
	}
	src := newSource(t, present)
	backupDir := filepath.Join(t.TempDir(), "backups")

	m, err := Create(context.Background(), CreateOptions{SourceRoot: src, BackupDir: backupDir, Now: fixedNow})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	want := []string{"var/lib/singbox-sub-manager/monitor-paused", "var/lib/singbox-sub-manager/token"}
	if len(m.Skipped) != len(want) {
		t.Fatalf("skipped = %v, want %v", m.Skipped, want)
	}
	for i, p := range want {
		if m.Skipped[i] != p {
			t.Errorf("skipped[%d] = %q, want %q", i, m.Skipped[i], p)
		}
		if m.SkippedReasons[p] != SkipNotFound {
			t.Errorf("reason for %s = %q, want %q", p, m.SkippedReasons[p], SkipNotFound)
		}
	}
	packed := manifestPaths(m)
	for _, p := range want {
		if _, ok := packed[p]; ok {
			t.Errorf("%s must not be packed", p)
		}
	}
	for _, e := range readArchiveEntries(t, m.ArchivePath) {
		for _, p := range want {
			if e.header.Name == p {
				t.Errorf("archive contains skipped path %s", p)
			}
		}
	}
}

func TestCreate_Cancellation(t *testing.T) {
	src := newSource(t, defaultSource)
	backupDir := filepath.Join(t.TempDir(), "backups")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Create(ctx, CreateOptions{SourceRoot: src, BackupDir: backupDir, Now: fixedNow})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if names := dirNames(t, backupDir); len(names) != 0 {
		t.Errorf("backup dir holds %v after cancellation, want nothing", names)
	}
}

func TestCreate_DeadlineExceeded(t *testing.T) {
	src := newSource(t, defaultSource)
	backupDir := filepath.Join(t.TempDir(), "backups")

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	_, err := Create(ctx, CreateOptions{SourceRoot: src, BackupDir: backupDir, Now: fixedNow})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if names := dirNames(t, backupDir); len(names) != 0 {
		t.Errorf("backup dir holds %v after deadline, want nothing", names)
	}
}

func TestCreate_DirAndArchivePermissions(t *testing.T) {
	src := newSource(t, defaultSource)
	backupDir := filepath.Join(t.TempDir(), "backups")

	// A hostile umask must not be able to loosen or tighten the archive: the
	// package sets both modes explicitly. umask is process-wide, so no test
	// in this package may call t.Parallel while this one runs.
	old := syscall.Umask(0777)
	defer syscall.Umask(old)

	m, err := Create(context.Background(), CreateOptions{SourceRoot: src, BackupDir: backupDir, Now: fixedNow})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	dirInfo, err := os.Stat(backupDir)
	if err != nil {
		t.Fatalf("stat backup dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0700 {
		t.Errorf("backup dir mode = %o, want 0700", got)
	}
	fileInfo, err := os.Stat(m.ArchivePath)
	if err != nil {
		t.Fatalf("stat archive: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0600 {
		t.Errorf("archive mode = %o, want 0600", got)
	}
}

func TestCreate_RecursesTreeAndSkipsNonRegularEntries(t *testing.T) {
	src := newSource(t, defaultSource)
	tree := filepath.Join(src, "etc", "singbox-sub-manager")

	if err := os.Symlink(filepath.Join(tree, "nodes.conf"), filepath.Join(tree, "nodes.link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.Link(filepath.Join(tree, "nodes.conf"), filepath.Join(tree, "nodes.hard")); err != nil {
		t.Fatalf("hard link: %v", err)
	}
	if err := syscall.Mkfifo(filepath.Join(tree, "pipe"), 0600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	if err := os.Mkdir(filepath.Join(tree, "empty"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	backupDir := filepath.Join(t.TempDir(), "backups")
	m, err := Create(context.Background(), CreateOptions{SourceRoot: src, BackupDir: backupDir, Now: fixedNow})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	packed := manifestPaths(m)
	for _, rel := range []string{
		"etc/singbox-sub-manager/templates/clash.yaml",
		"etc/singbox-sub-manager/certs/server.key",
	} {
		if _, ok := packed[rel]; !ok {
			t.Errorf("nested regular file %s was not packed", rel)
		}
	}

	wantSkips := map[string]string{
		"etc/singbox-sub-manager/nodes.link": SkipUnsupportedSourceType,
		"etc/singbox-sub-manager/nodes.hard": SkipUnsupportedSourceType,
		"etc/singbox-sub-manager/nodes.conf": SkipUnsupportedSourceType, // now hard-linked too
		"etc/singbox-sub-manager/pipe":       SkipUnsupportedSourceType,
		"etc/singbox-sub-manager/empty":      SkipEmptyDirectory,
	}
	for rel, reason := range wantSkips {
		if got := m.SkippedReasons[rel]; got != reason {
			t.Errorf("reason for %s = %q, want %q", rel, got, reason)
		}
		if _, ok := packed[rel]; ok {
			t.Errorf("%s must not be packed", rel)
		}
	}
	for _, e := range readArchiveEntries(t, m.ArchivePath) {
		if e.header.Typeflag != tar.TypeReg {
			t.Errorf("archive entry %s has type %q, want a regular file", e.header.Name, string(e.header.Typeflag))
		}
	}
}

func TestCreate_RejectsExplicitSourceSymlink(t *testing.T) {
	var present []sourceFile
	for _, f := range defaultSource {
		if f.rel == "etc/sing-box/config.json" {
			continue
		}
		present = append(present, f)
	}
	src := newSource(t, present)
	target := writeSourceFile(t, src, sourceFile{"etc/sing-box/real.json", "{}", 0600})
	if err := os.Symlink(target, filepath.Join(src, "etc", "sing-box", "config.json")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	backupDir := filepath.Join(t.TempDir(), "backups")
	_, err := Create(context.Background(), CreateOptions{SourceRoot: src, BackupDir: backupDir, Now: fixedNow})
	if !errors.Is(err, ErrUnsupportedSourceType) {
		t.Fatalf("err = %v, want ErrUnsupportedSourceType", err)
	}
	if names := dirNames(t, backupDir); len(names) != 0 {
		t.Errorf("backup dir holds %v after a rejected source, want nothing", names)
	}
}

func TestCreate_PreservesOwnershipAndMode(t *testing.T) {
	src := newSource(t, defaultSource)
	caddyfile := filepath.Join(src, "etc", "caddy", "Caddyfile")

	wantUID, wantGID := uint32(os.Getuid()), uint32(os.Getgid())
	if os.Geteuid() != 0 {
		t.Logf("not running as root: ownership is only checked against the current user %d:%d", wantUID, wantGID)
	}
	if os.Geteuid() == 0 {
		wantUID, wantGID = 12345, 12346
		if err := os.Chown(caddyfile, int(wantUID), int(wantGID)); err != nil {
			t.Fatalf("chown: %v", err)
		}
	}

	backupDir := filepath.Join(t.TempDir(), "backups")
	m, err := Create(context.Background(), CreateOptions{SourceRoot: src, BackupDir: backupDir, Now: fixedNow})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	entry, ok := manifestPaths(m)["etc/caddy/Caddyfile"]
	if !ok {
		t.Fatal("Caddyfile missing from the manifest")
	}
	if entry.Mode != 0640 {
		t.Errorf("manifest mode = %o, want 0640", entry.Mode)
	}
	if entry.UID != wantUID || entry.GID != wantGID {
		t.Errorf("manifest ownership = %d:%d, want %d:%d", entry.UID, entry.GID, wantUID, wantGID)
	}

	hdr := entryByName(t, readArchiveEntries(t, m.ArchivePath), "etc/caddy/Caddyfile").header
	if fs.FileMode(hdr.Mode).Perm() != 0640 {
		t.Errorf("tar mode = %o, want 0640", hdr.Mode)
	}
	if uint32(hdr.Uid) != wantUID || uint32(hdr.Gid) != wantGID {
		t.Errorf("tar ownership = %d:%d, want %d:%d", hdr.Uid, hdr.Gid, wantUID, wantGID)
	}

	// The archive itself stays private regardless of the source modes.
	info, err := os.Stat(m.ArchivePath)
	if err != nil {
		t.Fatalf("stat archive: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Errorf("archive mode = %o, want 0600", got)
	}
}

func TestCreate_TarShapeExact(t *testing.T) {
	src := newSource(t, defaultSource)
	backupDir := filepath.Join(t.TempDir(), "backups")

	m, err := Create(context.Background(), CreateOptions{SourceRoot: src, BackupDir: backupDir, Now: fixedNow})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	entries := readArchiveEntries(t, m.ArchivePath)
	if len(entries) != len(m.Files)+1 {
		t.Fatalf("archive has %d entries, want %d (manifest + files)", len(entries), len(m.Files)+1)
	}
	seen := map[string]int{}
	for _, e := range entries {
		if e.header.Typeflag != tar.TypeReg {
			t.Errorf("entry %s is not a regular file entry", e.header.Name)
		}
		seen[e.header.Name]++
	}
	if seen[ManifestName] != 1 {
		t.Errorf("archive holds %d manifest entries, want exactly 1", seen[ManifestName])
	}
	for _, f := range m.Files {
		if seen[f.Path] != 1 {
			t.Errorf("archive holds %d entries for %s, want exactly 1", seen[f.Path], f.Path)
		}
	}
	for name := range seen {
		if name == ManifestName {
			continue
		}
		if _, ok := manifestPaths(m)[name]; !ok {
			t.Errorf("archive holds undeclared entry %s", name)
		}
	}
}

func TestCreate_NameCollisionDoesNotOverwrite(t *testing.T) {
	src := newSource(t, defaultSource)
	backupDir := filepath.Join(t.TempDir(), "backups")
	opts := CreateOptions{SourceRoot: src, BackupDir: backupDir, Now: fixedNow}

	first, err := Create(context.Background(), opts)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	firstBytes, err := os.ReadFile(first.ArchivePath)
	if err != nil {
		t.Fatalf("read first archive: %v", err)
	}

	second, err := Create(context.Background(), opts)
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}

	if first.ArchivePath == second.ArchivePath {
		t.Fatalf("both runs used %s", first.ArchivePath)
	}
	if want := filepath.Join(backupDir, "backup-20260817T040506Z-1.tar.gz"); second.ArchivePath != want {
		t.Errorf("second archive = %q, want %q", second.ArchivePath, want)
	}
	afterBytes, err := os.ReadFile(first.ArchivePath)
	if err != nil {
		t.Fatalf("re-read first archive: %v", err)
	}
	if string(firstBytes) != string(afterBytes) {
		t.Error("the first archive was modified by the second run")
	}
	if names := dirNames(t, backupDir); len(names) != 2 {
		t.Errorf("backup dir holds %v, want exactly the two archives", names)
	}
}

func TestCreate_UnwritableDestinationLeavesNothingBehind(t *testing.T) {
	src := newSource(t, defaultSource)
	base := t.TempDir()
	blocker := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	_, err := Create(context.Background(), CreateOptions{
		SourceRoot: src,
		BackupDir:  filepath.Join(blocker, "backups"),
		Now:        fixedNow,
	})
	if err == nil {
		t.Fatal("Create succeeded with an unusable backup directory")
	}
	names := dirNames(t, base)
	if len(names) != 1 || names[0] != "not-a-dir" {
		t.Errorf("destination base holds %v, want only the blocking file", names)
	}
}

func TestCreate_OutOverridesNamingAndRetentionPath(t *testing.T) {
	src := newSource(t, defaultSource)
	out := filepath.Join(t.TempDir(), "custom-name.tar.gz")

	m, err := Create(context.Background(), CreateOptions{SourceRoot: src, Out: out, Now: fixedNow})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if m.ArchivePath != out {
		t.Errorf("archive path = %q, want %q", m.ArchivePath, out)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat archive: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Errorf("archive mode = %o, want 0600", got)
	}
	if IsArchiveName(filepath.Base(out)) {
		t.Error("a --out path must not be treated as a system-created archive name")
	}
	if len(readArchiveEntries(t, out)) != len(m.Files)+1 {
		t.Error("--out archive does not have the expected shape")
	}
}

// 独立终审 N1 回归: a trailing separator on --out is rejected outright,
// rather than silently writing beside the path the caller almost certainly
// meant (filepath.Dir/Base would treat the last real component as the file
// name, e.g. "/var/backups/" would write to "/var/backups", one level up
// from what it looks like).
func TestCreate_RejectsOutWithATrailingSeparator(t *testing.T) {
	src := newSource(t, defaultSource)
	// The intended destination directory must already exist for this to
	// exercise the actual bug: filepath.Dir/Base on a path ending in "/"
	// resolve to (out, base(out)) — not (parent(out), base(out)) — so an
	// unguarded reserveDestination would silently write to out/base(out)
	// (e.g. ".../backups/backups") instead of failing.
	out := filepath.Join(t.TempDir(), "backups")
	if err := os.MkdirAll(out, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if _, err := Create(context.Background(), CreateOptions{SourceRoot: src, Out: out + "/", Now: fixedNow}); err == nil {
		t.Fatal("Create succeeded with a trailing separator on --out")
	}
	if names := dirNames(t, out); len(names) != 0 {
		t.Errorf("%s holds %v, want nothing written into it", out, names)
	}
}

func TestCreate_RequiresDestination(t *testing.T) {
	src := newSource(t, defaultSource)
	if _, err := Create(context.Background(), CreateOptions{SourceRoot: src, Now: fixedNow}); err == nil {
		t.Fatal("Create succeeded without BackupDir or Out")
	}
}

func TestIsArchiveName(t *testing.T) {
	valid := []string{
		"backup-20260817T040506Z.tar.gz",
		"backup-20260817T040506Z-1.tar.gz",
		"backup-20260817T040506Z-42.tar.gz",
	}
	for _, name := range valid {
		if !IsArchiveName(name) {
			t.Errorf("IsArchiveName(%q) = false, want true", name)
		}
	}
	invalid := []string{
		"backup-20260817T040506Z-0.tar.gz",
		"backup-20260817T040506Z-01.tar.gz",
		"backup-20260817T040506.tar.gz",
		"backup-20260817T040506Z.tar",
		"user-notes.tar.gz",
		"backup-20260817T040506Z.tar.gz.bak",
	}
	for _, name := range invalid {
		if IsArchiveName(name) {
			t.Errorf("IsArchiveName(%q) = true, want false", name)
		}
	}
}

// sourceWithout materializes the default source minus one path, so the caller
// can put something other than a regular file there.
func sourceWithout(t *testing.T, rel string) string {
	t.Helper()
	var present []sourceFile
	for _, f := range defaultSource {
		if f.rel == rel {
			continue
		}
		present = append(present, f)
	}
	return newSource(t, present)
}

func TestCreate_RejectsExplicitSourceNonRegularVariants(t *testing.T) {
	const target = "etc/sing-box/config.json"

	cases := []struct {
		name  string
		place func(t *testing.T, src, path string)
	}{
		{"hard link", func(t *testing.T, src, path string) {
			real := writeSourceFile(t, src, sourceFile{"etc/sing-box/real.json", "{}", 0600})
			if err := os.Link(real, path); err != nil {
				t.Fatalf("hard link: %v", err)
			}
		}},
		{"fifo", func(t *testing.T, src, path string) {
			if err := syscall.Mkfifo(path, 0600); err != nil {
				t.Fatalf("mkfifo: %v", err)
			}
		}},
		{"directory", func(t *testing.T, src, path string) {
			if err := os.Mkdir(path, 0755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := sourceWithout(t, target)
			path := filepath.Join(src, filepath.FromSlash(target))
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			tc.place(t, src, path)

			backupDir := filepath.Join(t.TempDir(), "backups")
			_, err := Create(context.Background(), CreateOptions{SourceRoot: src, BackupDir: backupDir, Now: fixedNow})
			if !errors.Is(err, ErrUnsupportedSourceType) {
				t.Fatalf("err = %v, want ErrUnsupportedSourceType", err)
			}
			if names := dirNames(t, backupDir); len(names) != 0 {
				t.Errorf("backup dir holds %v after a rejected source, want nothing", names)
			}
		})
	}
}

func TestCreate_PacksLegalButUnusualFileNames(t *testing.T) {
	src := newSource(t, defaultSource)
	unusual := []string{
		`etc/singbox-sub-manager/we\ird.conf`,
		"etc/singbox-sub-manager/with space.conf",
		"etc/singbox-sub-manager/名前.conf",
	}
	for _, rel := range unusual {
		writeSourceFile(t, src, sourceFile{rel, "value\n", 0600})
	}

	backupDir := filepath.Join(t.TempDir(), "backups")
	m, err := Create(context.Background(), CreateOptions{SourceRoot: src, BackupDir: backupDir, Now: fixedNow})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	packed := manifestPaths(m)
	entries := readArchiveEntries(t, m.ArchivePath)
	for _, rel := range unusual {
		if _, ok := packed[rel]; !ok {
			t.Errorf("%s was not packed", rel)
			continue
		}
		if got := entryByName(t, entries, rel); string(got.data) != "value\n" {
			t.Errorf("%s: archived %q", rel, got.data)
		}
	}
}

func TestCreate_FailedArchiveWriteRemovesReservedName(t *testing.T) {
	src := newSource(t, defaultSource)
	backupDir := filepath.Join(t.TempDir(), "backups")

	wantErr := errors.New("archive write failed")
	original := writeArchiveFn
	writeArchiveFn = func(ctx context.Context, dirFd *os.File, leaf string, m Manifest, payloads []payload) (bool, error) {
		if _, err := os.Stat(filepath.Join(backupDir, leaf)); err != nil {
			t.Errorf("archive name was not reserved before writing: %v", err)
		}
		return false, wantErr
	}
	defer func() { writeArchiveFn = original }()

	_, err := Create(context.Background(), CreateOptions{SourceRoot: src, BackupDir: backupDir, Now: fixedNow})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if names := dirNames(t, backupDir); len(names) != 0 {
		t.Errorf("backup dir holds %v after a failed write, want no reserved name", names)
	}
}

func TestWriteArchive_CancellationRemovesStagingFileAndKeepsReservation(t *testing.T) {
	src := newSource(t, defaultSource)
	backupDir := filepath.Join(t.TempDir(), "backups")
	opts := CreateOptions{SourceRoot: src, BackupDir: backupDir, Now: fixedNow}

	sourceRootFd, err := openTrustedRoot(src, false, 0)
	if err != nil {
		t.Fatalf("open source root: %v", err)
	}
	defer sourceRootFd.Close()
	payloads, skipped, reasons, err := collect(context.Background(), sourceRootFd)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	m := Manifest{SchemaVersion: SchemaVersion, CreatedAt: fixedClock, Skipped: skipped, SkippedReasons: reasons}
	for _, p := range payloads {
		m.Files = append(m.Files, p.entry)
	}

	dirFd, leaf, _, self, release, err := reserveDestination(opts, fixedClock)
	if err != nil {
		t.Fatalf("reserveDestination: %v", err)
	}
	defer dirFd.Close()
	defer self.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	published, err := writeArchive(ctx, dirFd, leaf, m, payloads)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("writeArchive err = %v, want context.Canceled", err)
	}
	if published {
		t.Errorf("published = true, want false: the run was cancelled before the rename")
	}

	names := dirNames(t, backupDir)
	if len(names) != 1 || names[0] != leaf {
		t.Fatalf("backup dir holds %v, want only the reserved name %s", names, leaf)
	}

	if w := release(); w != "" {
		t.Errorf("release() reported a warning: %s", w)
	}
	if names := dirNames(t, backupDir); len(names) != 0 {
		t.Errorf("backup dir holds %v after release, want nothing", names)
	}
}

func TestCreate_PublishFailureLeavesNoStagingFile(t *testing.T) {
	src := newSource(t, defaultSource)
	base := t.TempDir()
	out := filepath.Join(base, "occupied")
	if err := os.Mkdir(out, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	_, err := Create(context.Background(), CreateOptions{SourceRoot: src, Out: out, Now: fixedNow})
	if err == nil {
		t.Fatal("Create succeeded with a directory in the destination path")
	}
	names := dirNames(t, base)
	if len(names) != 1 || names[0] != "occupied" {
		t.Errorf("destination dir holds %v, want only the pre-existing directory", names)
	}
	info, statErr := os.Stat(out)
	if statErr != nil || !info.IsDir() {
		t.Errorf("the pre-existing directory was replaced or removed: %v", statErr)
	}
}

func TestCreate_OutFailureKeepsAPreExistingFile(t *testing.T) {
	src := newSource(t, defaultSource)
	base := t.TempDir()
	out := filepath.Join(base, "existing.tar.gz")
	if err := os.WriteFile(out, []byte("original"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	wantErr := errors.New("archive write failed")
	original := writeArchiveFn
	writeArchiveFn = func(ctx context.Context, dirFd *os.File, leaf string, m Manifest, payloads []payload) (bool, error) {
		return false, wantErr
	}
	defer func() { writeArchiveFn = original }()

	if _, err := Create(context.Background(), CreateOptions{SourceRoot: src, Out: out, Now: fixedNow}); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the caller's file was removed: %v", err)
	}
	if string(data) != "original" {
		t.Errorf("the caller's file = %q, want it untouched", data)
	}
}

func TestCreate_OutFailureRemovesAFileThisRunCreated(t *testing.T) {
	src := newSource(t, defaultSource)
	base := t.TempDir()
	out := filepath.Join(base, "fresh.tar.gz")

	wantErr := errors.New("archive write failed")
	original := writeArchiveFn
	writeArchiveFn = func(ctx context.Context, dirFd *os.File, leaf string, m Manifest, payloads []payload) (bool, error) {
		if err := os.WriteFile(filepath.Join(base, leaf), []byte("half"), 0600); err != nil {
			t.Fatalf("simulate a published file: %v", err)
		}
		return false, wantErr
	}
	defer func() { writeArchiveFn = original }()

	if _, err := Create(context.Background(), CreateOptions{SourceRoot: src, Out: out, Now: fixedNow}); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if names := dirNames(t, base); len(names) != 0 {
		t.Errorf("destination dir holds %v after a failed run, want nothing", names)
	}
}

// T4 (design §6.4): a symlinked parent under SourceRoot is ErrUnsafePath,
// driving Create's source-side traversal exactly like the existing
// TestRestore_/TestCaptureSnapshot_/TestRollback_RejectsSymlinkedParentDirectory
// drive the other three operations. The shared classification logic itself
// is exhaustively tested once, at the primitive level, in
// TestClassifyDirOpenError (dirfd_test.go); this confirms Create's own
// wiring surfaces it correctly.
func TestCreate_RejectsSymlinkedParentDirectory(t *testing.T) {
	src := sourceWithout(t, caddyPath)
	outside := filepath.Join(filepath.Dir(src), "outside")
	if err := os.MkdirAll(outside, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "Caddyfile"), []byte("secret\n"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(src, "etc", "caddy")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	backupDir := filepath.Join(t.TempDir(), "backups")
	_, err := Create(context.Background(), CreateOptions{SourceRoot: src, BackupDir: backupDir, Now: fixedNow})
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath", err)
	}
	if names := dirNames(t, backupDir); len(names) != 0 {
		t.Errorf("backup dir holds %v after a rejected source, want nothing", names)
	}
}

func TestCreate_SkipsMissingRecursiveTreeRoot(t *testing.T) {
	var present []sourceFile
	for _, f := range defaultSource {
		if strings.HasPrefix(f.rel, "etc/singbox-sub-manager/") {
			continue
		}
		present = append(present, f)
	}
	src := newSource(t, present)
	backupDir := filepath.Join(t.TempDir(), "backups")

	m, err := Create(context.Background(), CreateOptions{SourceRoot: src, BackupDir: backupDir, Now: fixedNow})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	const tree = "etc/singbox-sub-manager"
	if got := m.SkippedReasons[tree]; got != SkipNotFound {
		t.Errorf("reason for %s = %q, want %q", tree, got, SkipNotFound)
	}
	for _, f := range m.Files {
		if strings.HasPrefix(f.Path, tree) {
			t.Errorf("%s must not be packed", f.Path)
		}
	}
}

// 独立终审 B2 回归: a recursive tree ROOT that turned out not to be a
// directory (here, a symlink) is skipped exactly like v0.7's
// filepath.WalkDir treated it — a non-directory entry found during the walk
// — not a hard failure of the whole backup. Only a replaced ANCESTOR of the
// tree root (etc/caddy in the sibling T4 test, not the tree root itself)
// is new, intentional hardening.
func TestCreate_SkipsARecursiveTreeRootThatIsASymlink(t *testing.T) {
	var present []sourceFile
	for _, f := range defaultSource {
		if strings.HasPrefix(f.rel, "etc/singbox-sub-manager/") {
			continue
		}
		present = append(present, f)
	}
	src := newSource(t, present)
	outside := filepath.Join(filepath.Dir(src), "outside-tree-root")
	if err := os.MkdirAll(outside, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "OUTSIDE"), []byte("x"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(src, "etc", "singbox-sub-manager")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	backupDir := filepath.Join(t.TempDir(), "backups")

	m, err := Create(context.Background(), CreateOptions{SourceRoot: src, BackupDir: backupDir, Now: fixedNow})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	const tree = "etc/singbox-sub-manager"
	if got := m.SkippedReasons[tree]; got != SkipUnsupportedSourceType {
		t.Errorf("reason for %s = %q, want %q", tree, got, SkipUnsupportedSourceType)
	}
	for _, f := range m.Files {
		if f.Path == "OUTSIDE" || strings.Contains(f.Path, "OUTSIDE") {
			t.Errorf("archive contains bytes read through the symlinked tree root: %s", f.Path)
		}
	}
}

// 独立终审 B1 回归 (a): the reserved archive name's §6.7 identity comparison
// must not be skipped — if the reservation is replaced by a different
// object before a later failure triggers cleanup, the returned error says
// so, and the replacement is left in place rather than silently deleted.
func TestCreate_FailureCleanupReportsAnIdentityMismatchOnAReplacedReservedName(t *testing.T) {
	src := newSource(t, defaultSource)
	backupDir := filepath.Join(t.TempDir(), "backups")
	leaf := "backup-" + fixedClock.Format(archiveTimeLayout) + ".tar.gz"

	wantErr := errors.New("archive write failed")
	original := writeArchiveFn
	writeArchiveFn = func(ctx context.Context, dirFd *os.File, leaf string, m Manifest, payloads []payload) (bool, error) {
		full := filepath.Join(backupDir, leaf)
		if err := os.Remove(full); err != nil {
			t.Fatalf("remove reserved name: %v", err)
		}
		if err := os.WriteFile(full, []byte("intruder"), 0600); err != nil {
			t.Fatalf("replace reserved name: %v", err)
		}
		return false, wantErr
	}
	defer func() { writeArchiveFn = original }()

	_, err := Create(context.Background(), CreateOptions{SourceRoot: src, BackupDir: backupDir, Now: fixedNow})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want it to wrap %v", err, wantErr)
	}
	if !strings.Contains(err.Error(), "needs manual confirmation") {
		t.Errorf("err = %v, want it to mention the replaced name needs manual confirmation", err)
	}
	data, readErr := os.ReadFile(filepath.Join(backupDir, leaf))
	if readErr != nil || string(data) != "intruder" {
		t.Errorf("the replaced reserved name was touched: data=%q err=%v", data, readErr)
	}
}

// 独立终审 NB-1 回归: once writeArchiveFn's rename has actually succeeded
// (published=true), a later failure in the same call must not run the
// reservation cleanup — it would run against the archive this call itself
// just finished writing (§6.7 applies to what a run is still trying to
// create, not to what it already published). Create still reports the
// error, but the archive it just wrote is left exactly as published.
func TestCreate_DoesNotCleanUpAReservationAfterAPublishSucceeded(t *testing.T) {
	src := newSource(t, defaultSource)
	backupDir := filepath.Join(t.TempDir(), "backups")
	leaf := "backup-" + fixedClock.Format(archiveTimeLayout) + ".tar.gz"

	wantErr := errors.New("sync failed after publish")
	original := writeArchiveFn
	writeArchiveFn = func(ctx context.Context, dirFd *os.File, leaf string, m Manifest, payloads []payload) (bool, error) {
		published, err := writeArchive(ctx, dirFd, leaf, m, payloads)
		if !published || err != nil {
			t.Fatalf("writeArchive: published=%v err=%v, want a clean publish to build on", published, err)
		}
		// Simulate a failure that strikes only after the rename succeeded
		// (e.g. the output directory's own Fsync).
		return true, wantErr
	}
	defer func() { writeArchiveFn = original }()

	_, err := Create(context.Background(), CreateOptions{SourceRoot: src, BackupDir: backupDir, Now: fixedNow})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want it to wrap %v", err, wantErr)
	}
	if strings.Contains(err.Error(), "needs manual confirmation") {
		t.Errorf("err = %v, want no cleanup warning: nothing should have tried to clean up the published archive", err)
	}
	data, readErr := os.ReadFile(filepath.Join(backupDir, leaf))
	if readErr != nil {
		t.Fatalf("the published archive was removed: %v", readErr)
	}
	if len(readArchiveEntries(t, filepath.Join(backupDir, leaf))) == 0 {
		t.Errorf("the surviving file at %s is not a valid archive: %q", leaf, data)
	}
}
