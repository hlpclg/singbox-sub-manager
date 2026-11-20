package realitynode

// export_test.go: plan Task 8's `uninstall --purge --export-backup PATH`
// (design spec §8.5): the archive writer (ExportBackup) on its own, and its
// place in the purge flow — after the L3 recheck, before the transaction, and
// never overwriting a user file. TestPurgeExportMatrix is the plan's named
// test.
//
// Failure injection uses exportHook (export.go), a process-global seam with
// the same shape as lifecycleHook: it is called before each of the export's
// named steps; an error it returns is that step failing. Tests overriding it
// must not use t.Parallel.
import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func setExportHook(t *testing.T, fn func(point string) error) {
	t.Helper()
	orig := exportHook
	exportHook = fn
	t.Cleanup(func() { exportHook = orig })
}

// readArchive returns every member of a tar.gz as name -> content ("" for
// directories), and the member names in order.
func readArchive(t *testing.T, path string) (map[string]string, []string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	members := map[string]string{}
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		var b []byte
		if h.Typeflag == tar.TypeReg {
			b, err = io.ReadAll(tr)
			if err != nil {
				t.Fatalf("tar read: %v", err)
			}
		}
		members[h.Name] = string(b)
		names = append(names, h.Name)
	}
	return members, names
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func archiveName(p string) string { return strings.TrimPrefix(filepath.ToSlash(p), "/") }

// --- ExportBackup on its own ---

func TestExportBackupContents(t *testing.T) {
	setUmask077(t)
	for _, kind := range []tdKind{tdInstalled, tdUninstalled} {
		kind := kind
		t.Run(string(kind), func(t *testing.T) {
			r := newTdRun(t, kind)
			dest := filepath.Join(t.TempDir(), "backup.tar.gz")

			if err := ExportBackup(context.Background(), r.env, dest); err != nil {
				t.Fatalf("ExportBackup: %v", err)
			}
			assertOwnerMode(t, dest, 0600)
			if names := listDir(t, filepath.Dir(dest)); len(names) != 1 || names[0] != "backup.tar.gz" {
				t.Fatalf("the export directory must hold only the archive, got %v", names)
			}

			members, names := readArchive(t, dest)
			stateDir, configDir := archiveName(r.roots.StateDir), archiveName(r.roots.ConfigDir)
			var sawConfig bool
			for _, n := range names {
				n = strings.TrimSuffix(n, "/")
				switch {
				case n == stateDir || strings.HasPrefix(n, stateDir+"/"):
				case n == configDir || strings.HasPrefix(n, configDir+"/"):
					sawConfig = true
				default:
					t.Errorf("archive member %q is outside the two exported directories", n)
				}
			}
			if members[archiveName(r.roots.Secrets)] != string(readBytes(t, r.roots.Secrets)) {
				t.Errorf("secrets.json missing or altered in the archive")
			}
			if members[archiveName(r.roots.Instance)] != string(readBytes(t, r.roots.Instance)) {
				t.Errorf("instance.json missing or altered in the archive")
			}
			if members[archiveName(r.roots.Manifest)] != string(readBytes(t, r.roots.Manifest)) {
				t.Errorf("manifest.json missing or altered in the archive")
			}
			if kind == tdInstalled {
				if !sawConfig || members[archiveName(r.roots.Config)] != string(readBytes(t, r.roots.Config)) {
					t.Errorf("config.json missing or altered in the archive")
				}
			} else if sawConfig {
				t.Errorf("an uninstalled instance has no config directory to export")
			}
			// nothing else: no binaries, no unit, no transaction area
			for n := range members {
				if strings.Contains(n, "versions") || strings.Contains(n, "proxyctl-reality.service") || strings.Contains(n, "txn") {
					t.Errorf("unexpected member %q", n)
				}
			}
			assertTreeEqual(t, r.before, treeSnapshot(t, r.roots)) // the export reads only
		})
	}
}

func TestExportBackupRefusals(t *testing.T) {
	setUmask077(t)
	r := newTdRun(t, tdInstalled)
	dir := t.TempDir()

	t.Run("an existing file is never overwritten", func(t *testing.T) {
		dest := filepath.Join(dir, "exists.tar.gz")
		if err := os.WriteFile(dest, []byte("precious"), 0644); err != nil {
			t.Fatal(err)
		}
		fi0, _ := os.Stat(dest)
		err := ExportBackup(context.Background(), r.env, dest)
		if err == nil {
			t.Fatal("want an error")
		}
		if b := readBytes(t, dest); string(b) != "precious" {
			t.Fatalf("existing file overwritten: %q", b)
		}
		fi1, _ := os.Stat(dest)
		if !fi1.ModTime().Equal(fi0.ModTime()) {
			t.Errorf("existing file touched")
		}
		if names := listDir(t, dir); len(names) != 1 {
			t.Errorf("leftover entries: %v", names)
		}
	})

	t.Run("an existing directory or dangling symlink at PATH is refused", func(t *testing.T) {
		d := filepath.Join(dir, "isdir")
		if err := os.Mkdir(d, 0700); err != nil {
			t.Fatal(err)
		}
		if err := ExportBackup(context.Background(), r.env, d); err == nil {
			t.Fatal("want an error for a directory")
		}
		link := filepath.Join(dir, "dangling")
		if err := os.Symlink(filepath.Join(dir, "nowhere"), link); err != nil {
			t.Fatal(err)
		}
		if err := ExportBackup(context.Background(), r.env, link); err == nil {
			t.Fatal("want an error for a dangling symlink (link() would follow nothing but the name is taken)")
		}
		assertNotExist(t, filepath.Join(dir, "nowhere"))
	})

	t.Run("missing parent directory, relative path, empty path", func(t *testing.T) {
		for _, dest := range []string{filepath.Join(dir, "no-such-dir", "b.tar.gz"), "relative.tar.gz", ""} {
			if err := ExportBackup(context.Background(), r.env, dest); err == nil {
				t.Errorf("dest %q: want an error", dest)
			}
		}
	})

	t.Run("inside a directory purge deletes", func(t *testing.T) {
		for _, dir := range []string{r.roots.StateDir, r.roots.ConfigDir, r.roots.BinDir, r.roots.TxnDir} {
			if err := ExportBackup(context.Background(), r.env, filepath.Join(dir, "backup.tar.gz")); err == nil {
				t.Errorf("an archive placed in %s would be deleted by the purge itself and must be refused", dir)
			}
		}
		assertTreeEqual(t, r.before, treeSnapshot(t, r.roots))
	})
}

// --- TestPurgeExportMatrix: failures around the temp file / link protocol ---

func TestPurgeExportMatrix(t *testing.T) {
	setUmask077(t)

	type failure struct {
		name string
		// arm installs the fault for dest (and may cancel ctx).
		arm func(t *testing.T, dest string, cancel context.CancelFunc)
	}
	failures := []failure{
		{"temp name creation fails", func(t *testing.T, dest string, cancel context.CancelFunc) {
			setExportHook(t, func(p string) error {
				if p == "export:create-temp" {
					return errors.New("injected: cannot create the temp file")
				}
				return nil
			})
		}},
		{"write failure mid-archive", func(t *testing.T, dest string, cancel context.CancelFunc) {
			n := 0
			setExportHook(t, func(p string) error {
				if p == "export:write" {
					n++
					if n == 2 {
						return errors.New("injected: write failed (disk full)")
					}
				}
				return nil
			})
		}},
		{"fsync failure", func(t *testing.T, dest string, cancel context.CancelFunc) {
			setExportHook(t, func(p string) error {
				if p == "export:fsync" {
					return errors.New("injected: fsync failed")
				}
				return nil
			})
		}},
		{"link failure", func(t *testing.T, dest string, cancel context.CancelFunc) {
			setExportHook(t, func(p string) error {
				if p == "export:link" {
					return errors.New("injected: link not supported")
				}
				return nil
			})
		}},
		{"target name taken between the check and the link", func(t *testing.T, dest string, cancel context.CancelFunc) {
			setExportHook(t, func(p string) error {
				if p == "export:link" {
					if err := os.WriteFile(dest, []byte("a user file that appeared meanwhile"), 0644); err != nil {
						t.Error(err)
					}
				}
				return nil
			})
		}},
		{"cancelled while writing", func(t *testing.T, dest string, cancel context.CancelFunc) {
			n := 0
			setExportHook(t, func(p string) error {
				if p == "export:write" {
					n++
					if n == 2 {
						cancel()
					}
				}
				return nil
			})
		}},
		{"cancelled right before the link", func(t *testing.T, dest string, cancel context.CancelFunc) {
			setExportHook(t, func(p string) error {
				if p == "export:link" {
					cancel()
				}
				return nil
			})
		}},
	}

	for _, kind := range []tdKind{tdInstalled, tdUninstalled} {
		for _, f := range failures {
			kind, f := kind, f
			t.Run(string(kind)+"/"+strings.ReplaceAll(f.name, " ", "_"), func(t *testing.T) {
				r := newTdRun(t, kind)
				dir := t.TempDir()
				dest := filepath.Join(dir, "backup.tar.gz")
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				f.arm(t, dest, cancel)

				res := Uninstall(ctx, r.env, UninstallParams{Purge: true, ExportPath: dest})
				// the export failed: exit 1, and no transaction ever began
				if res.ExitCode() != 1 || !res.PreTransaction || res.Err == nil {
					t.Fatalf("want exit 1 (export failed, nothing modified), got %+v", res)
				}
				assertNoTxnArea(t, r.roots)
				assertTreeEqual(t, r.before, treeSnapshot(t, r.roots))
				if got := mutatingSystemdCalls(r.sysd.Calls()); len(got) != 0 {
					t.Errorf("systemd must not be touched when the export fails: %v", got)
				}
				// a user file that appeared at PATH is intact; nothing else,
				// in particular no temp name, is left in the directory
				names := listDir(t, dir)
				if strings.Contains(f.name, "taken") {
					if len(names) != 1 || names[0] != "backup.tar.gz" || string(readBytes(t, dest)) != "a user file that appeared meanwhile" {
						t.Fatalf("user file lost or temp left behind: %v", names)
					}
				} else if len(names) != 0 {
					t.Fatalf("the export directory must be empty after a failed export, got %v", names)
				}
			})
		}
	}

	t.Run("PATH already exists: refused before anything happens", func(t *testing.T) {
		r := newTdRun(t, tdInstalled)
		dir := t.TempDir()
		dest := filepath.Join(dir, "backup.tar.gz")
		if err := os.WriteFile(dest, []byte("mine"), 0600); err != nil {
			t.Fatal(err)
		}
		res := Uninstall(context.Background(), r.env, UninstallParams{Purge: true, ExportPath: dest})
		if res.ExitCode() != 1 || !res.PreTransaction {
			t.Fatalf("want exit 1, got %+v", res)
		}
		if string(readBytes(t, dest)) != "mine" {
			t.Errorf("the existing file was overwritten")
		}
		assertTreeEqual(t, r.before, treeSnapshot(t, r.roots))
		assertNoTxnArea(t, r.roots)
	})

	for _, kind := range []tdKind{tdInstalled, tdUninstalled} {
		kind := kind
		t.Run("success/"+string(kind), func(t *testing.T) {
			r := newTdRun(t, kind)
			dir := t.TempDir()
			dest := filepath.Join(dir, "backup.tar.gz")
			secrets := readBytes(t, r.roots.Secrets)

			// The export runs after the L3 recheck and before the
			// transaction: every export step precedes the journal's first
			// lifecycle point ("began").
			var events []string
			orig := exportHook
			exportHook = func(p string) error { events = append(events, p); return orig(p) }
			defer func() { exportHook = orig }()
			setHook(t, func(p string) error { events = append(events, "life:"+p); return nil })

			res := Uninstall(context.Background(), r.env, UninstallParams{Purge: true, ExportPath: dest})
			assertCommitted(t, res)
			assertPurged(t, r)

			began := -1
			lastExport := -1
			for i, e := range events {
				if e == "life:began" {
					began = i
				}
				if strings.HasPrefix(e, "export:") {
					lastExport = i
				}
			}
			if began < 0 || lastExport < 0 || lastExport > began {
				t.Fatalf("the export must complete before the transaction begins: %v", events)
			}

			// the only permanent copy of the identity is the archive
			assertOwnerMode(t, dest, 0600)
			if names := listDir(t, dir); len(names) != 1 {
				t.Fatalf("export dir = %v, want only the archive", names)
			}
			members, _ := readArchive(t, dest)
			if members[archiveName(r.roots.Secrets)] != string(secrets) {
				t.Errorf("the archive must hold the retained identity")
			}
			for _, p := range []string{filepath.Dir(r.roots.ConfigDir), filepath.Dir(r.roots.StateDir), filepath.Dir(r.roots.BinDir), filepath.Dir(r.roots.SystemdUnit)} {
				assertNoFileContains(t, p, r.fx.privateKey)
			}
			// stderr: the archive contains the private key
			found := false
			for _, w := range res.Warnings {
				if strings.Contains(w, "private key") && strings.Contains(w, dest) {
					found = true
				}
			}
			if !found {
				t.Errorf("warnings must say the archive contains the private key and where it is: %v", res.Warnings)
			}
			notice := strings.Join(res.Notices, "\n")
			if !strings.Contains(notice, "import") {
				t.Errorf("notices should say the archive cannot be imported or extracted back: %q", notice)
			}
		})
	}

	t.Run("without --export-backup no archive and no key copy remains", func(t *testing.T) {
		r := newTdRun(t, tdInstalled)
		res := Uninstall(context.Background(), r.env, UninstallParams{Purge: true})
		assertCommitted(t, res)
		assertPurged(t, r)
		for _, w := range res.Warnings {
			if strings.Contains(w, "private key") {
				t.Errorf("no archive was requested, yet warned: %q", w)
			}
		}
	})
}

// A failure after the link (the archive already exists at PATH) is reported,
// so the purge does not start, but the complete archive is kept and no temp
// name is left behind: the one deliberate exception to "dest is untouched".
func TestExportBackupFailureAfterLink(t *testing.T) {
	setUmask077(t)
	for _, point := range []string{"export:unlink-temp", "export:fsync-dir"} {
		point := point
		t.Run(point, func(t *testing.T) {
			r := newTdRun(t, tdInstalled)
			dir := t.TempDir()
			dest := filepath.Join(dir, "backup.tar.gz")
			setExportHook(t, func(p string) error {
				if p == point {
					return errors.New("injected")
				}
				return nil
			})
			res := Uninstall(context.Background(), r.env, UninstallParams{Purge: true, ExportPath: dest})
			if res.ExitCode() != 1 || !res.PreTransaction {
				t.Fatalf("want exit 1, got %+v", res)
			}
			assertNoTxnArea(t, r.roots)
			assertTreeEqual(t, r.before, treeSnapshot(t, r.roots))
			members, _ := readArchive(t, dest) // complete and readable
			if members[archiveName(r.roots.Secrets)] == "" {
				t.Errorf("the kept archive must be complete")
			}
			for _, n := range listDir(t, dir) {
				if strings.HasPrefix(n, exportTempPrefix) && point == "export:fsync-dir" {
					t.Errorf("temp name %s left behind", n)
				}
			}
		})
	}
}
