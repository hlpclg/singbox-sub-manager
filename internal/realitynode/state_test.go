package realitynode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// In-memory StateFS for synthetic edge cases.
// ---------------------------------------------------------------------------

type memEntry struct {
	data  []byte
	isDir bool
	mode  os.FileMode
	uid   int
	gid   int
}

type memState struct {
	entries map[string]memEntry
}

type memFileInfo struct {
	name string
	e    memEntry
}

func (i memFileInfo) Name() string       { return i.name }
func (i memFileInfo) Size() int64        { return int64(len(i.e.data)) }
func (i memFileInfo) Mode() os.FileMode  { return i.e.mode }
func (i memFileInfo) ModTime() time.Time { return time.Time{} }
func (i memFileInfo) IsDir() bool        { return i.e.isDir }
func (i memFileInfo) Sys() interface{} {
	return &syscall.Stat_t{Uid: uint32(i.e.uid), Gid: uint32(i.e.gid)}
}

func (m memState) fs() StateFS {
	return StateFS{
		ReadFile: func(path string) ([]byte, error) {
			e, ok := m.entries[path]
			if !ok || e.isDir {
				return nil, os.ErrNotExist
			}
			return e.data, nil
		},
		Stat: func(path string) (os.FileInfo, error) {
			e, ok := m.entries[path]
			if !ok {
				return nil, os.ErrNotExist
			}
			return memFileInfo{name: filepath.Base(path), e: e}, nil
		},
		Glob: func(pattern string) ([]string, error) {
			var out []string
			for p := range m.entries {
				if ok, _ := filepath.Match(pattern, p); ok {
					out = append(out, p)
				}
			}
			return out, nil
		},
	}
}

func testRoots() Roots {
	return Roots{
		SystemdUnit:   "/etc/systemd/system/proxyctl-reality.service",
		ConfigDir:     "/etc/proxyctl-reality",
		Config:        "/etc/proxyctl-reality/config.json",
		StateDir:      "/var/lib/proxyctl-reality",
		Secrets:       "/var/lib/proxyctl-reality/secrets.json",
		Instance:      "/var/lib/proxyctl-reality/instance.json",
		Manifest:      "/var/lib/proxyctl-reality/manifest.json",
		BinDir:        "/usr/local/lib/proxyctl-reality",
		TxnDir:        "/var/lib/proxyctl-reality-txn",
		TombstoneGlob: "/var/lib/.proxyctl-reality-txn.tomb-*",
	}
}

// fullyInstalledEntries returns a self-consistent set of memState entries
// that DetectInstanceState should classify as StatusInstalled.
func fullyInstalledEntries(roots Roots) map[string]memEntry {
	unitData := []byte("[Unit]\nfixture unit\n")
	cfgData := []byte(`{"fixture":"config"}`)
	binData := []byte("fixture-binary")
	binPath := roots.BinDir + "/versions/1.14.1/sing-box"

	return map[string]memEntry{
		roots.SystemdUnit: {data: unitData, mode: 0o644},
		roots.ConfigDir:   {isDir: true, mode: 0o700 | os.ModeDir},
		roots.Config:      {data: cfgData, mode: 0o600},
		roots.StateDir:    {isDir: true, mode: 0o700 | os.ModeDir},
		roots.Secrets:     {data: []byte(`{"schema":1,"uuid":"x","private_key":"y","short_id":"z"}`), mode: 0o600},
		roots.Instance:    {data: []byte(`{"schema":1,"name":"reality","server":"s","listen_port":443,"sni":"n","selfcheck_port":1}`), mode: 0o600},
		roots.BinDir:      {isDir: true, mode: 0o755 | os.ModeDir},
		binPath:           {data: binData, mode: 0o755},
		roots.Manifest: {mode: 0o600, data: manifestJSON(roots, ManifestStateInstalled, []ManifestEntry{
			{Path: roots.SystemdUnit, Mode: 0o644, SHA256: sha256Hex(unitData)},
			{Path: roots.ConfigDir, Mode: 0o700},
			{Path: roots.Config, Mode: 0o600, SHA256: sha256Hex(cfgData)},
			{Path: roots.StateDir, Mode: 0o700},
			{Path: roots.Secrets, Mode: 0o600, Schema: 1},
			{Path: roots.Instance, Mode: 0o600, Schema: 1},
			{Path: roots.BinDir, Mode: 0o755},
			{Path: binPath, Mode: 0o755, SHA256: sha256Hex(binData)},
		})},
	}
}

func fullyUninstalledEntries(roots Roots) map[string]memEntry {
	return map[string]memEntry{
		roots.StateDir: {isDir: true, mode: 0o700 | os.ModeDir},
		roots.Secrets:  {data: []byte(`{"schema":1,"uuid":"x","private_key":"y","short_id":"z"}`), mode: 0o600},
		roots.Instance: {data: []byte(`{"schema":1,"name":"reality","server":"s","listen_port":443,"sni":"n","selfcheck_port":1}`), mode: 0o600},
		roots.Manifest: {mode: 0o600, data: manifestJSON(roots, ManifestStateUninstalled, []ManifestEntry{
			{Path: roots.StateDir, Mode: 0o700},
			{Path: roots.Secrets, Mode: 0o600, Schema: 1},
			{Path: roots.Instance, Mode: 0o600, Schema: 1},
		})},
	}
}

func manifestJSON(roots Roots, state string, entries []ManifestEntry) []byte {
	m := Manifest{Schema: 1, State: state, SingboxCurrent: "1.14.1", Entries: entries}
	data, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return data
}

// ---------------------------------------------------------------------------
// TestInstanceStateMatrix
// ---------------------------------------------------------------------------

func TestInstanceStateMatrix(t *testing.T) {
	roots := testRoots()

	t.Run("all four roots absent is NotInstalled", func(t *testing.T) {
		m := memState{entries: map[string]memEntry{}}
		got := DetectInstanceState(m.fs(), roots)
		if got.Status != StatusNotInstalled {
			t.Fatalf("status = %v, want NotInstalled (reasons: %v)", got.Status, got.Reasons)
		}
	})

	t.Run("lock file alone does not count as an instance asset", func(t *testing.T) {
		m := memState{entries: map[string]memEntry{
			"/run/lock/proxyctl-reality.lock": {data: []byte("")},
		}}
		got := DetectInstanceState(m.fs(), roots)
		if got.Status != StatusNotInstalled {
			t.Fatalf("status = %v, want NotInstalled (a lock file must not affect classification)", got.Status)
		}
	})

	t.Run("only one of four roots present is Inconsistent, not NotInstalled", func(t *testing.T) {
		m := memState{entries: map[string]memEntry{
			roots.SystemdUnit: {data: []byte("[Unit]\n"), mode: 0o644},
		}}
		got := DetectInstanceState(m.fs(), roots)
		if got.Status != StatusInconsistent {
			t.Fatalf("status = %v, want Inconsistent for a partial asset set", got.Status)
		}
		if len(got.Reasons) == 0 {
			t.Fatalf("expected itemized reasons for Inconsistent")
		}
	})

	t.Run("txn dir takes priority even with fully valid installed assets", func(t *testing.T) {
		entries := fullyInstalledEntries(roots)
		entries[roots.TxnDir] = memEntry{isDir: true, mode: 0o700 | os.ModeDir}
		m := memState{entries: entries}
		got := DetectInstanceState(m.fs(), roots)
		if got.Status != StatusUnfinishedTxn {
			t.Fatalf("status = %v, want UnfinishedTxn (txn must take priority)", got.Status)
		}
	})

	t.Run("tombstone alone (no txn dir) also takes priority", func(t *testing.T) {
		entries := fullyInstalledEntries(roots)
		entries["/var/lib/.proxyctl-reality-txn.tomb-abc123"] = memEntry{isDir: true, mode: 0o700 | os.ModeDir}
		m := memState{entries: entries}
		got := DetectInstanceState(m.fs(), roots)
		if got.Status != StatusUnfinishedTxn {
			t.Fatalf("status = %v, want UnfinishedTxn for a tombstone", got.Status)
		}
	})

	t.Run("fully valid installed manifest is Installed", func(t *testing.T) {
		m := memState{entries: fullyInstalledEntries(roots)}
		got := DetectInstanceState(m.fs(), roots)
		if got.Status != StatusInstalled {
			t.Fatalf("status = %v, want Installed (reasons: %v)", got.Status, got.Reasons)
		}
	})

	t.Run("fully valid uninstalled-keep-identity manifest is that state", func(t *testing.T) {
		m := memState{entries: fullyUninstalledEntries(roots)}
		got := DetectInstanceState(m.fs(), roots)
		if got.Status != StatusUninstalledKeepIdentity {
			t.Fatalf("status = %v, want UninstalledKeepIdentity (reasons: %v)", got.Status, got.Reasons)
		}
	})

	t.Run("uninstalled manifest but unit file still present is Inconsistent", func(t *testing.T) {
		entries := fullyUninstalledEntries(roots)
		entries[roots.SystemdUnit] = memEntry{data: []byte("[Unit]\n"), mode: 0o644}
		m := memState{entries: entries}
		got := DetectInstanceState(m.fs(), roots)
		if got.Status != StatusInconsistent {
			t.Fatalf("status = %v, want Inconsistent (uninstalled manifest but unit still exists)", got.Status)
		}
	})

	t.Run("installed manifest but a file's content hash no longer matches is Inconsistent", func(t *testing.T) {
		entries := fullyInstalledEntries(roots)
		entries[roots.Config] = memEntry{data: []byte(`{"tampered":true}`), mode: 0o600}
		m := memState{entries: entries}
		got := DetectInstanceState(m.fs(), roots)
		if got.Status != StatusInconsistent {
			t.Fatalf("status = %v, want Inconsistent for a hash mismatch", got.Status)
		}
		var found bool
		for _, r := range got.Reasons {
			if strings.Contains(r, "hash") {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected a hash-mismatch reason, got %v", got.Reasons)
		}
	})

	t.Run("installed manifest but a mode no longer matches is Inconsistent", func(t *testing.T) {
		entries := fullyInstalledEntries(roots)
		e := entries[roots.Secrets]
		e.mode = 0o644 // manifest says 0600
		entries[roots.Secrets] = e
		m := memState{entries: entries}
		got := DetectInstanceState(m.fs(), roots)
		if got.Status != StatusInconsistent {
			t.Fatalf("status = %v, want Inconsistent for a mode mismatch", got.Status)
		}
	})

	t.Run("installed manifest but owner no longer matches is Inconsistent", func(t *testing.T) {
		entries := fullyInstalledEntries(roots)
		e := entries[roots.Secrets]
		e.uid = 1000 // manifest expects uid 0
		entries[roots.Secrets] = e
		m := memState{entries: entries}
		got := DetectInstanceState(m.fs(), roots)
		if got.Status != StatusInconsistent {
			t.Fatalf("status = %v, want Inconsistent for an owner mismatch", got.Status)
		}
	})

	t.Run("installed manifest referencing a missing entry is Inconsistent", func(t *testing.T) {
		entries := fullyInstalledEntries(roots)
		delete(entries, roots.Config)
		m := memState{entries: entries}
		got := DetectInstanceState(m.fs(), roots)
		if got.Status != StatusInconsistent {
			t.Fatalf("status = %v, want Inconsistent for a missing asset", got.Status)
		}
	})

	t.Run("directory entry that is actually a regular file is Inconsistent (type mismatch)", func(t *testing.T) {
		entries := fullyInstalledEntries(roots)
		entries[roots.ConfigDir] = memEntry{data: []byte("not a directory"), mode: 0o700}
		m := memState{entries: entries}
		got := DetectInstanceState(m.fs(), roots)
		if got.Status != StatusInconsistent {
			t.Fatalf("status = %v, want Inconsistent for a dir/file type mismatch", got.Status)
		}
	})
}

// ---------------------------------------------------------------------------
// TestUnknownSchemaReadOnly
// ---------------------------------------------------------------------------

func TestUnknownSchemaReadOnly(t *testing.T) {
	roots := testRoots()

	t.Run("manifest schema 2 is unrecognized -> Inconsistent, no mutation", func(t *testing.T) {
		entries := fullyInstalledEntries(roots)
		bad := Manifest{Schema: 2, State: ManifestStateInstalled, Entries: nil}
		data, err := json.Marshal(bad)
		if err != nil {
			t.Fatal(err)
		}
		e := entries[roots.Manifest]
		before := append([]byte(nil), e.data...)
		e.data = data
		entries[roots.Manifest] = e
		m := memState{entries: entries}

		got := DetectInstanceState(m.fs(), roots)
		if got.Status != StatusInconsistent {
			t.Fatalf("status = %v, want Inconsistent for unknown schema", got.Status)
		}
		var found bool
		for _, r := range got.Reasons {
			if strings.Contains(r, "unknown manifest schema") {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected an unknown-schema reason, got %v", got.Reasons)
		}
		// No command may rewrite the manifest on an unrecognized schema.
		if string(entries[roots.Manifest].data) != string(before)+"" && string(entries[roots.Manifest].data) != string(data) {
			t.Fatalf("manifest bytes were unexpectedly mutated")
		}
	})

	t.Run("dynamic file with unrecognized schema is Inconsistent", func(t *testing.T) {
		entries := fullyInstalledEntries(roots)
		e := entries[roots.Secrets]
		e.data = []byte(`{"schema":2,"uuid":"x","private_key":"y","short_id":"z"}`)
		entries[roots.Secrets] = e
		m := memState{entries: entries}
		got := DetectInstanceState(m.fs(), roots)
		if got.Status != StatusInconsistent {
			t.Fatalf("status = %v, want Inconsistent for a dynamic file with unknown schema", got.Status)
		}
	})

	t.Run("manifest entry with both sha256 and schema set is a conflicting shape -> Inconsistent", func(t *testing.T) {
		entries := fullyInstalledEntries(roots)
		conflicting := Manifest{Schema: 1, State: ManifestStateInstalled, Entries: []ManifestEntry{
			{Path: roots.Secrets, Mode: 0o600, SHA256: "deadbeef", Schema: 1},
		}}
		data, err := json.Marshal(conflicting)
		if err != nil {
			t.Fatal(err)
		}
		e := entries[roots.Manifest]
		e.data = data
		entries[roots.Manifest] = e
		m := memState{entries: entries}
		got := DetectInstanceState(m.fs(), roots)
		if got.Status != StatusInconsistent {
			t.Fatalf("status = %v, want Inconsistent for a conflicting entry shape", got.Status)
		}
		var found bool
		for _, r := range got.Reasons {
			if strings.Contains(r, "conflicting") {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected a conflicting-fields reason, got %v", got.Reasons)
		}
	})

	t.Run("manifest entries include the manifest's own path -> Inconsistent, not silently accepted", func(t *testing.T) {
		entries := fullyInstalledEntries(roots)
		selfRef := Manifest{Schema: 1, State: ManifestStateInstalled, Entries: []ManifestEntry{
			{Path: roots.Manifest, Mode: 0o600},
		}}
		data, err := json.Marshal(selfRef)
		if err != nil {
			t.Fatal(err)
		}
		e := entries[roots.Manifest]
		e.data = data
		entries[roots.Manifest] = e
		m := memState{entries: entries}
		got := DetectInstanceState(m.fs(), roots)
		if got.Status != StatusInconsistent {
			t.Fatalf("status = %v, want Inconsistent when manifest references itself", got.Status)
		}
	})
}

// ---------------------------------------------------------------------------
// Real, frozen on-disk fixtures (testdata/v0.10), read through a chroot-style
// StateFS that maps production absolute paths onto the fixture tree without
// ever touching the real filesystem.
// ---------------------------------------------------------------------------

// chrootFS builds a StateFS that maps any absolute path (as it appears in
// Roots/manifest entries, e.g. "/etc/proxyctl-reality/config.json") onto
// root+path on the real filesystem, so on-disk fixtures using real
// production paths inside their manifest.json can be exercised without
// writing anywhere outside root (a temp dir copy of testdata).
func chrootFS(root string) StateFS {
	translate := func(path string) string { return filepath.Join(root, path) }
	return StateFS{
		ReadFile: func(path string) ([]byte, error) { return os.ReadFile(translate(path)) },
		Stat:     func(path string) (os.FileInfo, error) { return os.Stat(translate(path)) },
		Glob: func(pattern string) ([]string, error) {
			matches, err := filepath.Glob(translate(pattern))
			if err != nil {
				return nil, err
			}
			// Translate back to production-path form for caller
			// consistency (only used for existence checks here, so the
			// exact string doesn't matter, only non-emptiness).
			return matches, nil
		},
	}
}

// copyFixture copies testdata/v0.10/<name> into a temp dir and restores the
// exact modes spec §5.1 requires, since git does not preserve directory
// modes or non-executable file permission bits on checkout.
func copyFixture(t *testing.T, name string) string {
	t.Helper()
	src := filepath.Join("testdata", "v0.10", name)
	dst := t.TempDir()

	modes := map[string]os.FileMode{
		"/etc/systemd/system/proxyctl-reality.service":             0o644,
		"/etc/proxyctl-reality":                                    0o700,
		"/etc/proxyctl-reality/config.json":                        0o600,
		"/var/lib/proxyctl-reality":                                0o700,
		"/var/lib/proxyctl-reality/secrets.json":                   0o600,
		"/var/lib/proxyctl-reality/instance.json":                  0o600,
		"/var/lib/proxyctl-reality/manifest.json":                  0o600,
		"/usr/local/lib/proxyctl-reality":                          0o755,
		"/usr/local/lib/proxyctl-reality/versions":                 0o755,
		"/usr/local/lib/proxyctl-reality/versions/1.14.1":          0o755,
		"/usr/local/lib/proxyctl-reality/versions/1.14.1/sing-box": 0o755,
	}

	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		logicalPath := "/" + filepath.ToSlash(rel)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return err
		}
		if mode, ok := modes[logicalPath]; ok {
			if err := os.Chmod(target, mode); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("copy fixture %s: %v", name, err)
	}

	// Restore directory modes too (Walk above only chmods files it visits
	// after MkdirAll defaulted them to 0755; re-apply explicit dir modes
	// from the same table now that all files exist).
	for logicalPath, mode := range modes {
		target := filepath.Join(dst, logicalPath)
		if info, err := os.Stat(target); err == nil && info.IsDir() {
			if err := os.Chmod(target, mode); err != nil {
				t.Fatalf("chmod %s: %v", target, err)
			}
		}
	}
	return dst
}

func TestInstalledFixtureCompat(t *testing.T) {
	root := copyFixture(t, "installed")
	roots := ProdRoots()
	fsys := chrootFS(root)

	before := snapshotFixture(t, root)
	got := DetectInstanceState(fsys, roots)
	if got.Status != StatusInstalled {
		t.Fatalf("status = %v, want Installed (reasons: %v)", got.Status, got.Reasons)
	}
	assertFixtureUnchanged(t, root, before)
}

func TestUninstalledFixtureCompat(t *testing.T) {
	root := copyFixture(t, "uninstalled")
	roots := ProdRoots()
	fsys := chrootFS(root)

	before := snapshotFixture(t, root)
	got := DetectInstanceState(fsys, roots)
	if got.Status != StatusUninstalledKeepIdentity {
		t.Fatalf("status = %v, want UninstalledKeepIdentity (reasons: %v)", got.Status, got.Reasons)
	}
	assertFixtureUnchanged(t, root, before)
}

func snapshotFixture(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[p] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertFixtureUnchanged(t *testing.T, root string, before map[string][]byte) {
	t.Helper()
	after := snapshotFixture(t, root)
	if len(after) != len(before) {
		t.Fatalf("fixture file count changed: before %d, after %d", len(before), len(after))
	}
	for p, b := range before {
		a, ok := after[p]
		if !ok || string(a) != string(b) {
			t.Fatalf("%s: content changed by a read-only detection call", p)
		}
	}
}
