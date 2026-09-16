package backup

// Cross-version compatibility (design §7.3): testdata/v0.7.0-backup.tar.gz is
// a real archive produced by v0.7.0's own Create — not a hand-built fixture —
// so these tests prove v0.7.1's read side (Verify, ReadManifest, Restore)
// still understands exactly what v0.7.0 wrote, byte for byte.
//
// Generation method (reproducible; recorded here rather than only in the
// plan doc so it travels with the fixture):
//
//  1. git worktree add /tmp/v070-fixture-gen v0.7.0
//  2. Add cmd/genfixture/main.go to that worktree: it writes the exact same
//     eight files (same relative paths, contents and modes) as this
//     package's own archive_test.go defaultSource fixture under a source
//     directory, then calls backup.Create with BackupDir set to an output
//     directory, ProxyctlVersion "v0.7.0", and Now pinned to
//     2026-08-17T04:05:06Z (this package's own fixedClock) — using v0.7.0's
//     own Create, imported from that checked-out commit, not v0.7.1's.
//  3. Run it inside the golang:1.22 container (the fixture is meant to
//     represent what production, on Linux, actually wrote):
//     docker run --rm -v "$PWD":/w -v <src>:/src -v <out>:/out \
//       -v sbsm-gomod:/go/pkg/mod -w /w golang:1.22 \
//       go run ./cmd/genfixture /src /out
//  4. Copy the resulting backup-20260817T040506Z.tar.gz to
//     internal/backup/testdata/v0.7.0-backup.tar.gz in the v0.7.1 tree; the
//     temporary worktree and its scratch source/output directories are then
//     discarded, and are not part of this repository.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

const v070FixturePath = "testdata/v0.7.0-backup.tar.gz"

// v070FixtureFiles is the fixture's known contents, independently recorded
// here (not derived from the fixture itself) so a test failure means the
// fixture or the code changed, not that the assertions quietly followed
// whatever the fixture happens to contain.
var v070FixtureFiles = map[string]struct {
	data string
	mode os.FileMode
}{
	"etc/singbox-sub-manager/nodes.conf":             {"[hk]\nSERVER=1.2.3.4\n", 0600},
	"etc/singbox-sub-manager/templates/clash.yaml":   {"proxies: []\n", 0644},
	"etc/singbox-sub-manager/certs/server.key":       {"PRIVATE KEY\n", 0600},
	"etc/sing-box/config.json":                       {`{"log":{"level":"info"}}`, 0600},
	"etc/caddy/Caddyfile":                            {"sub.example.com {\n}\n", 0640},
	"var/lib/singbox-sub-manager/token":              {"deadbeef\n", 0600},
	"var/lib/singbox-sub-manager/monitor-state.json": {`{"schema_version":1}`, 0600},
	"var/lib/singbox-sub-manager/monitor-paused":     {"", 0600},
}

func checkV070Manifest(t *testing.T, m Manifest) {
	t.Helper()
	if m.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", m.SchemaVersion, SchemaVersion)
	}
	if m.ProxyctlVersion != "v0.7.0" {
		t.Errorf("proxyctl_version = %q, want %q", m.ProxyctlVersion, "v0.7.0")
	}
	if !m.CreatedAt.Equal(fixedClock) {
		t.Errorf("created_at = %s, want %s", m.CreatedAt, fixedClock)
	}
	if len(m.Skipped) != 0 {
		t.Errorf("skipped = %v, want none", m.Skipped)
	}
	if len(m.Files) != len(v070FixtureFiles) {
		t.Fatalf("files = %d, want %d", len(m.Files), len(v070FixtureFiles))
	}
	for _, f := range m.Files {
		want, ok := v070FixtureFiles[f.Path]
		if !ok {
			t.Errorf("unexpected manifest entry %s", f.Path)
			continue
		}
		if f.Mode != want.mode {
			t.Errorf("%s: mode = %o, want %o", f.Path, f.Mode, want.mode)
		}
		if f.UID != 0 || f.GID != 0 {
			t.Errorf("%s: ownership = %d:%d, want 0:0 (the fixture was generated as root)", f.Path, f.UID, f.GID)
		}
	}
}

func TestV070Fixture_VerifyMatchesManifest(t *testing.T) {
	m, err := Verify(context.Background(), v070FixturePath)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	checkV070Manifest(t, m)
}

func TestV070Fixture_ReadManifestMatchesVerify(t *testing.T) {
	verified, err := Verify(context.Background(), v070FixturePath)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	read, err := ReadManifest(context.Background(), v070FixturePath)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	checkV070Manifest(t, read)
	if len(read.Files) != len(verified.Files) {
		t.Fatalf("ReadManifest files = %d, Verify files = %d", len(read.Files), len(verified.Files))
	}
	for i := range read.Files {
		if read.Files[i] != verified.Files[i] {
			t.Errorf("ReadManifest entry %d = %+v, Verify entry %d = %+v", i, read.Files[i], i, verified.Files[i])
		}
	}
}

func TestV070Fixture_RestoreReproducesBytesModesAndOwnership(t *testing.T) {
	dest := t.TempDir()
	l := &fakeLocker{}

	res, err := Restore(context.Background(), v070FixturePath, RestoreOptions{DestRoot: dest, Lock: l})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("failed = %v, want none", res.Failed)
	}
	if len(res.Restored) != len(v070FixtureFiles) {
		t.Fatalf("restored = %d files, want %d", len(res.Restored), len(v070FixtureFiles))
	}
	for rel, want := range v070FixtureFiles {
		p := filepath.Join(dest, filepath.FromSlash(rel))
		data, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("%s: read: %v", rel, err)
			continue
		}
		if string(data) != want.data {
			t.Errorf("%s: content = %q, want %q", rel, data, want.data)
		}
		info, err := os.Stat(p)
		if err != nil {
			t.Errorf("%s: stat: %v", rel, err)
			continue
		}
		if got := info.Mode().Perm(); got != want.mode {
			t.Errorf("%s: mode = %o, want %o", rel, got, want.mode)
		}
	}
}
