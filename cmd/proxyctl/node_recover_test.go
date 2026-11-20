package main

// node_recover_test.go covers plan Task 8's CLI layer: `proxyctl node service
// uninstall [--purge [--export-backup PATH]]` and `recover [--abandon]`
// (design spec §4.5, §8.4-§8.6, §13.1). Like node_service_test.go, the
// process-level effects (root, locks, role, the Env) are replaced through
// newServiceRuntime, so nothing needs root, /run/lock or a real systemd; the
// filesystem, though, is real (temporary trusted roots owned by the current
// uid), because the assertions that matter — exit codes, what stays on disk —
// come from the real transaction engine behind the command.
import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hlpclg/singbox-sub-manager/internal/hostlock"
	"github.com/hlpclg/singbox-sub-manager/internal/realitynode"
	"github.com/hlpclg/singbox-sub-manager/internal/role"
	"github.com/hlpclg/singbox-sub-manager/internal/transaction"
)

// --- strict parsing: exit 2 before root, role, lock, Env, IO (spec §4.5) ---

func TestNodeServiceUninstallStrictParsing(t *testing.T) {
	with := func(extra ...string) []string { return append([]string{"node", "service", "uninstall"}, extra...) }
	cases := map[string][]string{
		"unknown flag":                    with("--bogus"),
		"positional argument":             with("extra"),
		"duplicate purge":                 with("--purge", "--purge"),
		"purge value must be true/false":  with("--purge=maybe"),
		"purge with a separate value":     with("--purge", "false"), // "false" is a positional argument
		"export-backup without purge":     with("--export-backup", "/tmp/b.tar.gz"),
		"export-backup with purge=false":  with("--purge=false", "--export-backup", "/tmp/b.tar.gz"),
		"export-backup missing its value": with("--purge", "--export-backup"),
		"export-backup empty value":       with("--purge", "--export-backup="),
		"duplicate export-backup":         with("--purge", "--export-backup", "/tmp/a", "--export-backup", "/tmp/b"),
		"abandon does not belong here":    with("--abandon"),
		"purge and abandon":               with("--purge", "--abandon"),
		"help mixed with other arguments": with("--help", "--purge"),
		"single-dash publish":             with("-publish"),
	}
	for name, args := range cases {
		t.Run(strings.ReplaceAll(name, " ", "_"), func(t *testing.T) {
			log := installFakeRuntime(t, fakeRuntimeOpts{})
			var out, errb bytes.Buffer
			if code := run(args, &out, &errb); code != 2 {
				t.Fatalf("exit = %d, want 2 (stderr: %s)", code, errb.String())
			}
			if got := log.list(); len(got) != 0 {
				t.Fatalf("a usage error must touch nothing (no root check, role, lock, env); saw %v", got)
			}
			if out.String() != "" {
				t.Fatalf("stdout must be empty, got %q", out.String())
			}
		})
	}
}

func TestNodeServiceRecoverStrictParsing(t *testing.T) {
	with := func(extra ...string) []string { return append([]string{"node", "service", "recover"}, extra...) }
	cases := map[string][]string{
		"unknown flag":                    with("--bogus"),
		"positional argument":             with("extra"),
		"duplicate abandon":               with("--abandon", "--abandon"),
		"abandon value must be true":      with("--abandon=maybe"),
		"abandon with a separate value":   with("--abandon", "true"), // "true" is a positional argument
		"purge does not belong here":      with("--purge"),
		"export-backup does not belong":   with("--export-backup", "/tmp/b"),
		"help mixed with other arguments": with("--help", "--abandon"),
	}
	for name, args := range cases {
		t.Run(strings.ReplaceAll(name, " ", "_"), func(t *testing.T) {
			log := installFakeRuntime(t, fakeRuntimeOpts{})
			var out, errb bytes.Buffer
			if code := run(args, &out, &errb); code != 2 {
				t.Fatalf("exit = %d, want 2 (stderr: %s)", code, errb.String())
			}
			if got := log.list(); len(got) != 0 {
				t.Fatalf("a usage error must touch nothing; saw %v", got)
			}
			if out.String() != "" {
				t.Fatalf("stdout must be empty, got %q", out.String())
			}
		})
	}
}

// --abandon belongs only to `node service recover` (and, in Task 9,
// `node publish --recover`): everywhere else CheckAbandonPlacement rejects it
// with its own message (spec §4.5), still exit 2 and still before any effect.
func TestNodeServiceAbandonPlacement(t *testing.T) {
	cases := map[string][]string{
		"install":   append(goodInstallArgs(), "--abandon"),
		"reinstall": {"node", "service", "reinstall", "--abandon"},
		"show":      {"node", "service", "show", "--abandon"},
		"uninstall": {"node", "service", "uninstall", "--abandon"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			log := installFakeRuntime(t, fakeRuntimeOpts{})
			var out, errb bytes.Buffer
			if code := run(args, &out, &errb); code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
			if !strings.Contains(errb.String(), "--abandon is only valid") {
				t.Errorf("stderr should explain the placement rule, got %q", errb.String())
			}
			if got := log.list(); len(got) != 0 {
				t.Fatalf("saw %v", got)
			}
		})
	}
}

func TestNodeServiceHelpListsUninstallAndRecover(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"node", "service", "--help"}, &out, &errb); code != 0 {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{"uninstall", "--purge", "--export-backup", "recover", "--abandon"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("service usage must mention %q:\n%s", want, out.String())
		}
	}
	for _, sub := range []string{"uninstall", "recover"} {
		out.Reset()
		errb.Reset()
		if code := run([]string{"node", "service", sub, "--help"}, &out, &errb); code != 0 || !strings.Contains(out.String(), sub) {
			t.Fatalf("%s --help: code=%d out=%q err=%q", sub, code, out.String(), errb.String())
		}
	}
}

// --- a host with real temporary trusted roots ---

type stubSystemd struct {
	mu       sync.Mutex
	calls    []string
	loaded   bool
	ufs      string
	active   string
	unitPath string
	// failStops makes the next failStops Stop calls fail.
	failStops int
	// alwaysFailStop makes every Stop fail.
	alwaysFailStop bool
}

func (s *stubSystemd) rec(c string) {
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.mu.Unlock()
}
func (s *stubSystemd) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}
func (s *stubSystemd) LoadState(context.Context, string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loaded, nil
}
func (s *stubSystemd) UnitFileState(context.Context, string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ufs, nil
}
func (s *stubSystemd) Stop(context.Context, string) error {
	s.rec("stop")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.alwaysFailStop {
		return errors.New("stub: systemctl stop failed")
	}
	if s.failStops > 0 {
		s.failStops--
		return errors.New("stub: systemctl stop failed")
	}
	s.active = "inactive"
	return nil
}
func (s *stubSystemd) SetUnitFileState(_ context.Context, _, want string) error {
	if want == "enabled" {
		s.rec("enable")
	} else {
		s.rec("disable")
	}
	s.mu.Lock()
	s.ufs = want
	s.mu.Unlock()
	return nil
}
func (s *stubSystemd) DaemonReload(context.Context) error {
	s.rec("daemon-reload")
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Lstat(s.unitPath); err != nil {
		s.loaded, s.ufs, s.active = false, "", "inactive"
	} else {
		s.loaded = true
		if s.ufs == "" {
			s.ufs = "disabled"
		}
	}
	return nil
}
func (s *stubSystemd) Restart(context.Context, string) error {
	s.rec("restart")
	s.mu.Lock()
	s.active = "active"
	s.mu.Unlock()
	return nil
}
func (s *stubSystemd) Start(context.Context, string) error {
	s.rec("start")
	return nil
}
func (s *stubSystemd) ActiveState(context.Context, string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active, nil
}
func (s *stubSystemd) MainPID(context.Context, string) (int, error) { return 0, nil }

var _ realitynode.SystemdFull = (*stubSystemd)(nil)

type hostOpts struct {
	euid         int
	acquireErr   error
	acquireBlock chan struct{}
	ctx          context.Context
	cancel       context.CancelFunc
	roles        []role.Status
}

type host struct {
	roots realitynode.Roots
	log   *callLog
	sysd  *stubSystemd
	base  string
}

func mkHostRoots(t *testing.T) (realitynode.Roots, string) {
	t.Helper()
	base := t.TempDir()
	for _, d := range []string{"etc", "var-lib", "usr-local-lib", "systemd"} {
		p := filepath.Join(base, d)
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0755); err != nil {
			t.Fatal(err)
		}
	}
	etc, varLib, usrLib, sd := filepath.Join(base, "etc"), filepath.Join(base, "var-lib"), filepath.Join(base, "usr-local-lib"), filepath.Join(base, "systemd")
	return realitynode.Roots{
		SystemdUnit:   filepath.Join(sd, "proxyctl-reality.service"),
		ConfigDir:     filepath.Join(etc, "proxyctl-reality"),
		Config:        filepath.Join(etc, "proxyctl-reality", "config.json"),
		StateDir:      filepath.Join(varLib, "proxyctl-reality"),
		Secrets:       filepath.Join(varLib, "proxyctl-reality", "secrets.json"),
		Instance:      filepath.Join(varLib, "proxyctl-reality", "instance.json"),
		Manifest:      filepath.Join(varLib, "proxyctl-reality", "manifest.json"),
		BinDir:        filepath.Join(usrLib, "proxyctl-reality"),
		TxnDir:        filepath.Join(varLib, "proxyctl-reality-txn"),
		TombstoneGlob: filepath.Join(varLib, ".proxyctl-reality-txn.tomb-*"),
	}, base
}

func installHost(t *testing.T, roots realitynode.Roots, base string, o hostOpts) *host {
	t.Helper()
	log := &callLog{}
	if len(o.roles) == 0 {
		o.roles = []role.Status{role.StatusNo}
	}
	sysd := &stubSystemd{unitPath: roots.SystemdUnit}
	h := &host{roots: roots, log: log, sysd: sysd, base: base}
	orig := newServiceRuntime
	t.Cleanup(func() { newServiceRuntime = orig })
	newServiceRuntime = func() serviceRuntime {
		return serviceRuntime{
			geteuid: func() int { log.add("geteuid"); return o.euid },
			detectRole: func() role.Result {
				log.add("role")
				return role.Result{Status: o.roles[0]}
			},
			acquire: func(ctx context.Context, levels ...hostlock.Level) (*hostlock.Lease, error) {
				var names []string
				for _, l := range levels {
					names = append(names, l.String())
				}
				log.add("lock:" + strings.Join(names, "+"))
				if o.acquireBlock != nil {
					close(o.acquireBlock)
					<-ctx.Done()
					return nil, ctx.Err()
				}
				if o.acquireErr != nil {
					return nil, o.acquireErr
				}
				return &hostlock.Lease{}, nil
			},
			newEnv: func() *realitynode.Env {
				log.add("newEnv")
				return &realitynode.Env{
					Roots: roots, FS: realitynode.OSStateFS(),
					TrustedOwnerUID: os.Getuid(), TrustedOwnerGID: os.Getgid(),
					GOOS: "linux", GOARCH: "amd64",
					CheckDiskSpace: func(string, uint64) error { return nil },
					Systemd:        sysd,
					GatePollBudget: 50 * time.Millisecond, GatePollInterval: 5 * time.Millisecond,
				}
			},
			newContext: func() (context.Context, context.CancelFunc) {
				if o.ctx != nil {
					return o.ctx, o.cancel
				}
				return context.WithCancel(context.Background())
			},
		}
	}
	return h
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func writeAt(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// writeInstalledHost writes a self-consistent installed instance and returns
// the node name and private key (for "no key left behind" assertions).
func writeInstalledHost(t *testing.T, roots realitynode.Roots) (name, privateKey string) {
	t.Helper()
	uid, gid := os.Getuid(), os.Getgid()
	id, err := realitynode.GenerateIdentity(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const version = "1.14.1"
	unit := realitynode.RenderUnit(version)
	cfg, err := realitynode.GenerateServerConfig(realitynode.ServerConfigParams{
		ListenPort: 443, SelfcheckPort: 21000, SNI: "www.microsoft.com", UUID: id.UUID, PrivateKey: id.PrivateKey, ShortID: id.ShortID,
	})
	if err != nil {
		t.Fatal(err)
	}
	secrets, _ := json.Marshal(realitynode.SecretsFile{Schema: 1, UUID: id.UUID, PrivateKey: id.PrivateKey, ShortID: id.ShortID})
	inst, _ := json.Marshal(realitynode.InstanceFile{Schema: 1, Name: "jp1", Server: "203.0.113.10", ListenPort: 443, SNI: "www.microsoft.com", SelfcheckPort: 21000})
	bin := []byte("#!/bin/sh\nexit 0\n")
	binPath := filepath.Join(roots.BinDir, "versions", version, "sing-box")

	writeAt(t, roots.SystemdUnit, unit, 0644)
	writeAt(t, roots.Config, cfg, 0600)
	writeAt(t, roots.Secrets, secrets, 0600)
	writeAt(t, roots.Instance, inst, 0600)
	writeAt(t, binPath, bin, 0755)
	for _, d := range []struct {
		p string
		m os.FileMode
	}{
		{roots.ConfigDir, 0700}, {roots.StateDir, 0700}, {roots.BinDir, 0755},
		{filepath.Join(roots.BinDir, "versions"), 0755}, {filepath.Join(roots.BinDir, "versions", version), 0755},
	} {
		if err := os.Chmod(d.p, d.m); err != nil {
			t.Fatal(err)
		}
	}
	dir := func(p string, m uint32) realitynode.ManifestEntry {
		return realitynode.ManifestEntry{Path: p, Mode: m, UID: uid, GID: gid}
	}
	file := func(p string, m uint32, b []byte) realitynode.ManifestEntry {
		return realitynode.ManifestEntry{Path: p, Mode: m, UID: uid, GID: gid, SHA256: sha(b)}
	}
	dyn := func(p string) realitynode.ManifestEntry {
		return realitynode.ManifestEntry{Path: p, Mode: 0600, UID: uid, GID: gid, Schema: 1}
	}
	m := realitynode.Manifest{Schema: 1, State: realitynode.ManifestStateInstalled, SingboxCurrent: version, Entries: []realitynode.ManifestEntry{
		file(roots.SystemdUnit, 0644, unit), dir(roots.ConfigDir, 0700), file(roots.Config, 0600, cfg),
		dir(roots.StateDir, 0700), dyn(roots.Secrets), dyn(roots.Instance),
		dir(roots.BinDir, 0755), dir(filepath.Join(roots.BinDir, "versions"), 0755),
		dir(filepath.Join(roots.BinDir, "versions", version), 0755), file(binPath, 0755, bin),
	}}
	data, _ := json.MarshalIndent(m, "", "  ")
	writeAt(t, roots.Manifest, data, 0600)
	return "jp1", id.PrivateKey
}

func hostSysdActive(h *host) {
	h.sysd.loaded, h.sysd.ufs, h.sysd.active = true, "enabled", "active"
}

func lockEvents(l *callLog) []string {
	var out []string
	for _, e := range l.list() {
		if strings.HasPrefix(e, "lock:") {
			out = append(out, e)
		}
	}
	return out
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// --- uninstall through the CLI ---

func TestNodeServiceUninstallOrdering(t *testing.T) {
	uninstall := []string{"node", "service", "uninstall"}

	t.Run("non-root stops at the root check", func(t *testing.T) {
		roots, base := mkHostRoots(t)
		writeInstalledHost(t, roots)
		h := installHost(t, roots, base, hostOpts{euid: 1000})
		var out, errb bytes.Buffer
		if code := run(uninstall, &out, &errb); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if got := h.log.String(); got != "geteuid" {
			t.Fatalf("events = %s, want only the root check", got)
		}
	})

	t.Run("an ineligible state stops at the precheck, before the lock", func(t *testing.T) {
		roots, base := mkHostRoots(t) // nothing installed
		h := installHost(t, roots, base, hostOpts{})
		var out, errb bytes.Buffer
		if code := run(uninstall, &out, &errb); code != 1 {
			t.Fatalf("exit = %d, want 1 (stderr: %s)", code, errb.String())
		}
		if got := lockEvents(h.log); len(got) != 0 {
			t.Fatalf("the lock must not be taken for an ineligible state: %v", h.log.list())
		}
		if out.String() != "" {
			t.Errorf("stdout must be empty: %q", out.String())
		}
	})

	t.Run("L1 then L3, no role detection (spec §8 table: uninstall rechecks only the state)", func(t *testing.T) {
		roots, base := mkHostRoots(t)
		h := installHost(t, roots, base, hostOpts{acquireErr: errors.New("simulated lock timeout")})
		writeInstalledHost(t, roots)
		hostSysdActive(h)
		var out, errb bytes.Buffer
		if code := run(uninstall, &out, &errb); code != 1 {
			t.Fatalf("exit = %d, want 1 (stderr: %s)", code, errb.String())
		}
		if got := lockEvents(h.log); len(got) != 1 || got[0] != "lock:L1+L3" {
			t.Fatalf("uninstall must take exactly L1+L3, took %v", got)
		}
		for _, e := range h.log.list() {
			if e == "role" {
				t.Errorf("uninstall must not require role detection: %v", h.log.list())
			}
		}
		if mut := h.sysd.Calls(); len(mut) != 0 {
			t.Errorf("a lock timeout must leave systemd untouched: %v", mut)
		}
	})

	t.Run("a signal while waiting for the lock ends the command with 1", func(t *testing.T) {
		roots, base := mkHostRoots(t)
		writeInstalledHost(t, roots)
		ctx, cancel := context.WithCancel(context.Background())
		entered := make(chan struct{})
		h := installHost(t, roots, base, hostOpts{ctx: ctx, cancel: cancel, acquireBlock: entered})
		hostSysdActive(h)
		done := make(chan int, 1)
		var out, errb bytes.Buffer
		go func() { done <- run(append(uninstall, "--purge"), &out, &errb) }()
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("never reached the lock")
		}
		cancel()
		select {
		case code := <-done:
			if code != 1 {
				t.Fatalf("exit = %d, want 1", code)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("cancelling during the lock wait did not return promptly")
		}
		if !exists(roots.Secrets) || len(h.sysd.Calls()) != 0 {
			t.Fatalf("nothing may be modified")
		}
	})
}

func TestNodeServiceUninstallRuns(t *testing.T) {
	uninstall := []string{"node", "service", "uninstall"}

	t.Run("uninstall: exit 0, one disable line, identity kept, notices on stderr", func(t *testing.T) {
		roots, base := mkHostRoots(t)
		h := installHost(t, roots, base, hostOpts{})
		name, _ := writeInstalledHost(t, roots)
		hostSysdActive(h)
		var out, errb bytes.Buffer
		if code := run(uninstall, &out, &errb); code != 0 {
			t.Fatalf("exit = %d (stderr: %s)", code, errb.String())
		}
		lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
		var exec []string
		for _, l := range lines {
			if !strings.HasPrefix(l, "#") {
				exec = append(exec, l)
			}
		}
		if len(exec) != 1 || !strings.Contains(exec[0], "node disable --publish --min-version v0.10.0 '"+name+"'") {
			t.Fatalf("stdout must hold exactly one executable disable line, got %q", out.String())
		}
		if !strings.Contains(out.String(), "# sudo proxyctl node remove") {
			t.Errorf("the remove alternative must be commented out:\n%s", out.String())
		}
		if !strings.Contains(errb.String(), "proxyctl") || !strings.Contains(errb.String(), "bootstrap") {
			t.Errorf("stderr must say proxyctl and bootstrap's previous copy were kept: %q", errb.String())
		}
		if got := lockEvents(h.log); len(got) != 1 || got[0] != "lock:L1+L3" {
			t.Errorf("locks = %v", got)
		}
		if exists(roots.SystemdUnit) || exists(roots.ConfigDir) || exists(roots.BinDir) || exists(roots.TxnDir) {
			t.Errorf("service assets must be gone")
		}
		if !exists(roots.Secrets) || !exists(roots.Instance) || !exists(roots.Manifest) {
			t.Errorf("identity files must be kept")
		}
		if got := strings.Join(h.sysd.Calls(), ","); got != "stop,disable,daemon-reload" {
			t.Errorf("systemd calls = %s", got)
		}
	})

	t.Run("purge: exit 0, one remove line, everything gone, no archive", func(t *testing.T) {
		roots, base := mkHostRoots(t)
		h := installHost(t, roots, base, hostOpts{})
		name, key := writeInstalledHost(t, roots)
		hostSysdActive(h)
		var out, errb bytes.Buffer
		if code := run(append(uninstall, "--purge"), &out, &errb); code != 0 {
			t.Fatalf("exit = %d (stderr: %s)", code, errb.String())
		}
		if !strings.Contains(out.String(), "node remove --publish --min-version v0.10.0 '"+name+"'") || !strings.Contains(out.String(), "# sudo proxyctl node disable") {
			t.Fatalf("stdout = %q", out.String())
		}
		for _, p := range []string{roots.SystemdUnit, roots.ConfigDir, roots.StateDir, roots.BinDir, roots.TxnDir} {
			if exists(p) {
				t.Errorf("%s must be gone", p)
			}
		}
		_ = filepath.Walk(base, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.Mode().IsRegular() {
				if b, _ := os.ReadFile(p); strings.Contains(string(b), key) {
					t.Errorf("%s still holds the private key", p)
				}
			}
			return nil
		})
		if strings.Contains(errb.String(), "private key") {
			t.Errorf("no archive was requested: %q", errb.String())
		}
	})

	t.Run("purge --export-backup writes the 0600 archive before the transaction", func(t *testing.T) {
		roots, base := mkHostRoots(t)
		h := installHost(t, roots, base, hostOpts{})
		writeInstalledHost(t, roots)
		hostSysdActive(h)
		dest := filepath.Join(t.TempDir(), "backup.tar.gz")
		var out, errb bytes.Buffer
		if code := run(append(uninstall, "--purge", "--export-backup", dest), &out, &errb); code != 0 {
			t.Fatalf("exit = %d (stderr: %s)", code, errb.String())
		}
		fi, err := os.Stat(dest)
		if err != nil || fi.Mode().Perm() != 0600 {
			t.Fatalf("archive missing or wrong mode: %v %v", err, fi)
		}
		if !strings.Contains(errb.String(), "private key") || !strings.Contains(errb.String(), dest) {
			t.Errorf("stderr must warn that the archive contains the private key: %q", errb.String())
		}
		if exists(roots.StateDir) {
			t.Errorf("state dir must be purged")
		}
	})

	t.Run("purge --export-backup to an existing path: exit 1, nothing changes", func(t *testing.T) {
		roots, base := mkHostRoots(t)
		h := installHost(t, roots, base, hostOpts{})
		writeInstalledHost(t, roots)
		hostSysdActive(h)
		dest := filepath.Join(t.TempDir(), "backup.tar.gz")
		if err := os.WriteFile(dest, []byte("mine"), 0600); err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		if code := run(append(uninstall, "--purge", "--export-backup", dest), &out, &errb); code != 1 {
			t.Fatalf("exit = %d, want 1 (stderr: %s)", code, errb.String())
		}
		if b, _ := os.ReadFile(dest); string(b) != "mine" {
			t.Fatalf("the existing file was overwritten")
		}
		if !exists(roots.SystemdUnit) || !exists(roots.Secrets) || exists(roots.TxnDir) || len(h.sysd.Calls()) != 0 {
			t.Fatalf("nothing may be modified")
		}
	})

	t.Run("a relative --export-backup path is resolved against the working directory", func(t *testing.T) {
		roots, base := mkHostRoots(t)
		h := installHost(t, roots, base, hostOpts{})
		writeInstalledHost(t, roots)
		hostSysdActive(h)
		dir := t.TempDir()
		old, _ := os.Getwd()
		if err := os.Chdir(dir); err != nil {
			t.Fatal(err)
		}
		defer os.Chdir(old)
		var out, errb bytes.Buffer
		if code := run(append(uninstall, "--purge", "--export-backup", "rel.tar.gz"), &out, &errb); code != 0 {
			t.Fatalf("exit = %d (stderr: %s)", code, errb.String())
		}
		if !exists(filepath.Join(dir, "rel.tar.gz")) {
			t.Fatalf("archive not written next to the working directory")
		}
	})

	t.Run("an unrecoverable failure after the transaction began is exit 5 naming the area", func(t *testing.T) {
		roots, base := mkHostRoots(t)
		h := installHost(t, roots, base, hostOpts{})
		writeInstalledHost(t, roots)
		h.sysd.loaded, h.sysd.ufs, h.sysd.active = true, "enabled", "inactive" // not running: a rollback needs no restart and no gate
		h.sysd.alwaysFailStop = true                                           // the forward stop fails, and so does the rollback's stop
		var out, errb bytes.Buffer
		if code := run(uninstall, &out, &errb); code != 5 {
			t.Fatalf("exit = %d, want 5 (stderr: %s)", code, errb.String())
		}
		if !strings.Contains(errb.String(), roots.TxnDir) {
			t.Errorf("stderr must name the preserved transaction area: %q", errb.String())
		}
		if out.String() != "" {
			t.Errorf("stdout must stay empty on failure: %q", out.String())
		}
		if !exists(roots.TxnDir) {
			t.Fatal("the area must be preserved")
		}

		// ... and `recover` finishes the job once systemd behaves
		h.sysd.alwaysFailStop = false
		out.Reset()
		errb.Reset()
		if code := run([]string{"node", "service", "recover"}, &out, &errb); code != 0 {
			t.Fatalf("recover: exit = %d (stderr: %s)", code, errb.String())
		}
		if exists(roots.TxnDir) || !exists(roots.SystemdUnit) || !exists(roots.Config) {
			t.Errorf("recover must roll the failed uninstall back")
		}
	})

	t.Run("a failure that rolls back cleanly is exit 4", func(t *testing.T) {
		roots, base := mkHostRoots(t)
		h := installHost(t, roots, base, hostOpts{})
		writeInstalledHost(t, roots)
		h.sysd.loaded, h.sysd.ufs, h.sysd.active = true, "enabled", "inactive" // not running: no restart, no gate on rollback
		h.sysd.failStops = 1
		var out, errb bytes.Buffer
		if code := run(uninstall, &out, &errb); code != 4 {
			t.Fatalf("exit = %d, want 4 (stderr: %s)", code, errb.String())
		}
		if exists(roots.TxnDir) || !exists(roots.SystemdUnit) || !exists(roots.Config) || !exists(roots.BinDir) {
			t.Errorf("the pre-transaction state must be back")
		}
		if !strings.Contains(errb.String(), "rolled back") {
			t.Errorf("stderr should say the operation was rolled back: %q", errb.String())
		}
	})
}

// --- recover through the CLI ---

func writeLeftoverJournal(t *testing.T, roots realitynode.Roots, mutate func(j *transaction.Journal)) {
	t.Helper()
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	j := &transaction.Journal{
		Schema: transaction.CurrentSchema, TxnID: "abcd0001", Op: "uninstall", ProxyctlVersion: "v0.10.0",
		CreatedAt: time.Unix(1700000000, 0).UTC(), Phase: transaction.PhaseRunning,
		DirIDs: map[string]transaction.DirIdentity{}, RollbackSystemd: transaction.RollbackSystemdNone,
		Steps: []transaction.Step{{
			ID: "write-config", Kind: transaction.KindWriteFile, Target: "etc/proxyctl-reality/config.json",
			Pre:    transaction.PathState{Exists: false},
			Post:   transaction.PathState{Exists: true, Type: transaction.PathTypeFile, Size: 3, SHA256: sha([]byte("abc")), Mode: 0600, UID: uid, GID: gid},
			Status: transaction.StatusPending, Temps: []string{transaction.TempName("abcd0001", 1)},
		}},
	}
	j.DirIDs["etc/proxyctl-reality"] = transaction.DirIdentity{Exists: false}
	if mutate != nil {
		mutate(j)
	}
	root, err := transaction.OpenTrustedRoot(filepath.Dir(roots.StateDir), os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	store, err := transaction.BeginTransaction(root, filepath.Base(roots.TxnDir), j)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
}

func TestNodeServiceRecover(t *testing.T) {
	recoverCmd := []string{"node", "service", "recover"}

	t.Run("non-root stops at the root check", func(t *testing.T) {
		roots, base := mkHostRoots(t)
		writeLeftoverJournal(t, roots, nil)
		h := installHost(t, roots, base, hostOpts{euid: 1000})
		var out, errb bytes.Buffer
		if code := run(recoverCmd, &out, &errb); code != 1 {
			t.Fatalf("exit = %d", code)
		}
		if got := h.log.String(); got != "geteuid" {
			t.Fatalf("events = %s", got)
		}
	})

	t.Run("without an unfinished transaction: exit 1 before the lock", func(t *testing.T) {
		roots, base := mkHostRoots(t)
		writeInstalledHost(t, roots)
		h := installHost(t, roots, base, hostOpts{})
		for _, args := range [][]string{recoverCmd, append(recoverCmd, "--abandon")} {
			var out, errb bytes.Buffer
			if code := run(args, &out, &errb); code != 1 {
				t.Fatalf("%v: exit = %d, want 1", args, code)
			}
		}
		if got := lockEvents(h.log); len(got) != 0 {
			t.Fatalf("the lock must not be taken: %v", got)
		}
		if !exists(roots.Secrets) || !exists(roots.Config) {
			t.Fatalf("nothing may change")
		}
	})

	t.Run("takes L1+L3 and no role detection; lock failure is exit 1", func(t *testing.T) {
		roots, base := mkHostRoots(t)
		writeLeftoverJournal(t, roots, nil)
		h := installHost(t, roots, base, hostOpts{acquireErr: errors.New("simulated lock timeout")})
		var out, errb bytes.Buffer
		if code := run(recoverCmd, &out, &errb); code != 1 {
			t.Fatalf("exit = %d", code)
		}
		if got := lockEvents(h.log); len(got) != 1 || got[0] != "lock:L1+L3" {
			t.Fatalf("locks = %v", got)
		}
		for _, e := range h.log.list() {
			if e == "role" {
				t.Errorf("recover must not require role detection")
			}
		}
		if !exists(roots.TxnDir) {
			t.Fatalf("the area must be left alone")
		}
	})

	t.Run("a leftover with nothing modified is discarded: exit 0", func(t *testing.T) {
		roots, base := mkHostRoots(t)
		writeLeftoverJournal(t, roots, nil)
		installHost(t, roots, base, hostOpts{})
		var out, errb bytes.Buffer
		if code := run(recoverCmd, &out, &errb); code != 0 {
			t.Fatalf("exit = %d (stderr: %s)", code, errb.String())
		}
		if exists(roots.TxnDir) {
			t.Fatal("the area must be gone")
		}
		if out.String() != "" {
			t.Errorf("recover prints nothing on stdout: %q", out.String())
		}
	})

	t.Run("no journal: an empty area is discarded (0); an unknown one is exit 5, untouched", func(t *testing.T) {
		roots, base := mkHostRoots(t)
		if err := os.Mkdir(roots.TxnDir, 0700); err != nil {
			t.Fatal(err)
		}
		installHost(t, roots, base, hostOpts{})
		var out, errb bytes.Buffer
		if code := run(recoverCmd, &out, &errb); code != 0 || exists(roots.TxnDir) {
			t.Fatalf("empty area: exit = %d (stderr: %s), exists=%v", code, errb.String(), exists(roots.TxnDir))
		}

		if err := os.MkdirAll(filepath.Join(roots.TxnDir, "dl"), 0700); err != nil {
			t.Fatal(err)
		}
		out.Reset()
		errb.Reset()
		if code := run(recoverCmd, &out, &errb); code != 5 {
			t.Fatalf("unknown content: exit = %d, want 5 (stderr: %s)", code, errb.String())
		}
		if !strings.Contains(errb.String(), roots.TxnDir) || !exists(filepath.Join(roots.TxnDir, "dl")) {
			t.Fatalf("stderr must name the area, and the area must be untouched: %q", errb.String())
		}
	})

	t.Run("a tombstone is deleted: exit 0", func(t *testing.T) {
		roots, base := mkHostRoots(t)
		tomb := filepath.Join(filepath.Dir(roots.StateDir), ".proxyctl-reality-txn.tomb-cafe0001")
		writeAt(t, filepath.Join(tomb, "snap", "x.snap"), []byte("key-like content"), 0600)
		installHost(t, roots, base, hostOpts{})
		var out, errb bytes.Buffer
		if code := run(recoverCmd, &out, &errb); code != 0 || exists(tomb) {
			t.Fatalf("exit = %d (stderr: %s), tombstone exists=%v", code, errb.String(), exists(tomb))
		}
	})

	t.Run("tampering is exit 5 and prints the area", func(t *testing.T) {
		roots, base := mkHostRoots(t)
		writeLeftoverJournal(t, roots, func(j *transaction.Journal) { j.Steps[0].Status = transaction.StatusDone })
		installHost(t, roots, base, hostOpts{})
		before, _ := os.ReadFile(filepath.Join(roots.TxnDir, "journal.json"))
		var out, errb bytes.Buffer
		if code := run(recoverCmd, &out, &errb); code != 5 {
			t.Fatalf("exit = %d, want 5 (stderr: %s)", code, errb.String())
		}
		if !strings.Contains(errb.String(), roots.TxnDir) {
			t.Errorf("stderr must print the transaction area path: %q", errb.String())
		}
		after, _ := os.ReadFile(filepath.Join(roots.TxnDir, "journal.json"))
		if !bytes.Equal(before, after) {
			t.Errorf("the journal must not be modified on tampering")
		}
	})

	t.Run("committed: exit 0; committed with --abandon: exit 1", func(t *testing.T) {
		roots, base := mkHostRoots(t)
		writeLeftoverJournal(t, roots, func(j *transaction.Journal) {
			j.Phase = transaction.PhaseCommitted
			j.Steps = nil
			j.DirIDs = map[string]transaction.DirIdentity{}
		})
		installHost(t, roots, base, hostOpts{})
		var out, errb bytes.Buffer
		if code := run(append(recoverCmd, "--abandon"), &out, &errb); code != 1 {
			t.Fatalf("abandon of a committed transaction: exit = %d, want 1 (stderr: %s)", code, errb.String())
		}
		if !exists(roots.TxnDir) {
			t.Fatal("a refused abandon must not modify anything")
		}
		errb.Reset()
		if code := run(recoverCmd, &out, &errb); code != 0 || exists(roots.TxnDir) {
			t.Fatalf("recover: exit = %d (stderr: %s)", code, errb.String())
		}
	})

	t.Run("--abandon when every path is at its pre-transaction state: exit 0 with a warning", func(t *testing.T) {
		roots, base := mkHostRoots(t)
		writeLeftoverJournal(t, roots, nil)
		h := installHost(t, roots, base, hostOpts{})
		var out, errb bytes.Buffer
		if code := run(append(recoverCmd, "--abandon"), &out, &errb); code != 0 {
			t.Fatalf("exit = %d (stderr: %s)", code, errb.String())
		}
		if exists(roots.TxnDir) {
			t.Fatal("the area must be gone")
		}
		if !strings.Contains(errb.String(), "may not be running") {
			t.Errorf("stderr must warn that the service may not be running: %q", errb.String())
		}
		if got := h.sysd.Calls(); len(got) != 0 {
			t.Errorf("abandon must not touch systemd: %v", got)
		}
		if out.String() != "" {
			t.Errorf("stdout must stay empty: %q", out.String())
		}
	})

	t.Run("a signal while waiting for the lock ends recover with 1, area untouched", func(t *testing.T) {
		roots, base := mkHostRoots(t)
		writeLeftoverJournal(t, roots, nil)
		ctx, cancel := context.WithCancel(context.Background())
		entered := make(chan struct{})
		installHost(t, roots, base, hostOpts{ctx: ctx, cancel: cancel, acquireBlock: entered})
		done := make(chan int, 1)
		var out, errb bytes.Buffer
		go func() { done <- run(recoverCmd, &out, &errb) }()
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("never reached the lock")
		}
		cancel()
		select {
		case code := <-done:
			if code != 1 {
				t.Fatalf("exit = %d, want 1", code)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("no prompt return")
		}
		if !exists(roots.TxnDir) {
			t.Fatal("the area must be untouched")
		}
	})
}
