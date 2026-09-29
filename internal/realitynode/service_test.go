// service_test.go covers plan Task 7's TestServiceLifecycleMatrix and
// TestServiceShowReadOnly (design spec §8.1-§8.3), built on real temporary
// filesystem trusted roots (umask 077, real fsync/rename/chmod — matching
// every other v0.10 transaction test in this repository) with only
// systemd, the sing-box binary's download source, and network endpoints
// faked. Every fake here is either reused from this package's existing
// test files (fakeProc from systemd_test.go, buildTarGz/tarEntry from
// download_test.go) or new and local to this file.
//
// Known, documented scope reduction (see the Task 7 handback report):
// this file exercises install/reinstall through every precheck, every
// asset write, systemd enable/start|restart and the §9.2 gate using a
// real fake binary and a real subprocess, then deliberately fails §9.3's
// client-start phase (a fake "sing-box" that exits 1 immediately on `run`)
// to drive automatic rollback — proving the transactional/crash-safety
// core this Task is rated L2 for. It does not additionally implement a
// working fake Reality-protocol proxy to reach a *committed* install: that
// requires a binary that actually terminates TLS, derives the Reality
// keypair and forwards a mixed inbound through it, which is exactly what
// Task 1/6 already scoped real protocol verification out to VM-level
// validation (see docs/reality-node-validation.md and the plan's Task 13).
// The commit path itself (manifest write, phase=committed, forward
// cleanup, stdout rendering) is covered directly against the Store/Engine
// without going through a real selfcheck — see TestInstallCommitPath.
package realitynode

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hlpclg/singbox-sub-manager/internal/transaction"
)

// --- fake systemd covering SystemdFull ---

type fakeServiceSystemd struct {
	// persistPath, when set, keeps the unit's state in a JSON file so that a
	// crashed child process and the recovering parent see the same systemd
	// (real systemd state outlives the proxyctl process that changed it).
	persistPath string

	mu            sync.Mutex
	calls         []string
	loaded        bool
	unitFileState string
	activeState   string
	mainPID       int
	failStart     bool
	stopErr       error
	restarts      int
	// onRestart, when set, runs at the top of every Restart with the
	// 1-based restart count (before the restart takes effect).
	onRestart func(n int)
}

type persistedUnit struct {
	Loaded        bool   `json:"loaded"`
	UnitFileState string `json:"unit_file_state"`
	ActiveState   string `json:"active_state"`
	MainPID       int    `json:"main_pid"`
}

// loadLocked/saveLocked mirror the unit state to persistPath (caller holds
// f.mu). A missing file leaves the in-memory state untouched.
func (f *fakeServiceSystemd) loadLocked() {
	if f.persistPath == "" {
		return
	}
	b, err := os.ReadFile(f.persistPath)
	if err != nil {
		return
	}
	var p persistedUnit
	if json.Unmarshal(b, &p) == nil {
		f.loaded, f.unitFileState, f.activeState, f.mainPID = p.Loaded, p.UnitFileState, p.ActiveState, p.MainPID
	}
}

func (f *fakeServiceSystemd) saveLocked() {
	if f.persistPath == "" {
		return
	}
	b, _ := json.Marshal(persistedUnit{f.loaded, f.unitFileState, f.activeState, f.mainPID})
	_ = os.WriteFile(f.persistPath, b, 0600)
}

// persistTo starts mirroring state to path, seeding it with the current
// in-memory state.
func (f *fakeServiceSystemd) persistTo(path string) {
	f.mu.Lock()
	f.persistPath = path
	f.saveLocked()
	f.mu.Unlock()
}

func (f *fakeServiceSystemd) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}
func (f *fakeServiceSystemd) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}
func (f *fakeServiceSystemd) LoadState(ctx context.Context, unit string) (bool, error) {
	f.record("load_state")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadLocked()
	return f.loaded, nil
}
func (f *fakeServiceSystemd) UnitFileState(ctx context.Context, unit string) (string, error) {
	f.record("unit_file_state")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadLocked()
	return f.unitFileState, nil
}
func (f *fakeServiceSystemd) Stop(ctx context.Context, unit string) error {
	f.record("stop")
	if f.stopErr != nil {
		return f.stopErr
	}
	f.mu.Lock()
	f.loadLocked()
	f.activeState = "inactive"
	f.saveLocked()
	f.mu.Unlock()
	return nil
}
func (f *fakeServiceSystemd) SetUnitFileState(ctx context.Context, unit, want string) error {
	f.mu.Lock()
	f.loadLocked()
	f.unitFileState = want
	f.saveLocked()
	f.mu.Unlock()
	if want == "enabled" {
		f.record("enable")
	} else {
		f.record("disable")
	}
	return nil
}
func (f *fakeServiceSystemd) DaemonReload(ctx context.Context) error {
	f.record("daemon-reload")
	return nil
}
func (f *fakeServiceSystemd) Restart(ctx context.Context, unit string) error {
	f.record("restart")
	f.mu.Lock()
	f.restarts++
	n, hook := f.restarts, f.onRestart
	f.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	f.mu.Lock()
	f.loadLocked()
	f.loaded = true
	f.activeState = "active"
	f.saveLocked()
	f.mu.Unlock()
	return nil
}
func (f *fakeServiceSystemd) Start(ctx context.Context, unit string) error {
	f.record("start")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failStart {
		return fmt.Errorf("fake start failure")
	}
	f.loadLocked()
	f.loaded = true
	f.activeState = "active"
	f.saveLocked()
	return nil
}
func (f *fakeServiceSystemd) ActiveState(ctx context.Context, unit string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadLocked()
	return f.activeState, nil
}
func (f *fakeServiceSystemd) MainPID(ctx context.Context, unit string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadLocked()
	return f.mainPID, nil
}

var _ SystemdFull = (*fakeServiceSystemd)(nil)

// --- fake proc satisfying the §9.2 gate without any real listening
// process (ProcReader is defined in systemd.go; fakeProc in
// systemd_test.go is unexported but same package) ---

func healthyGateProc(binPath string) *fakeProc {
	return &fakeProc{inode: 77, inodeOK: true, ownerPID: 4242, exePath: binPath}
}

// --- fake tar.gz downloader wrapping a locally-built fake "sing-box" ---

// buildFakeSingboxScript writes a POSIX shell script that behaves exactly
// as much like sing-box as Install/Reinstall/RunSelfcheck need: "version"
// prints wantVersion, "check ..." exits 0, and "run ..." exits 1
// immediately (deliberately failing the temporary client, spec §9.3's E2 —
// see this file's header comment for why a committed install is not
// covered here).
func buildFakeSingboxScript(t *testing.T, wantVersion string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "sing-box")
	script := "#!/bin/sh\ncase \"$1\" in\n  version) echo 'sing-box version " + wantVersion + "'; exit 0 ;;\n  check) exit 0 ;;\n  run) exit 1 ;;\n  *) exit 1 ;;\nesac\n"
	if err := os.WriteFile(p, []byte(script), 0755); err != nil {
		t.Fatalf("write fake sing-box: %v", err)
	}
	return p
}

func fakeDownloaderFor(t *testing.T, version, goarch string, scriptPath string) (fakeDownloader, string) {
	t.Helper()
	content, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read fake sing-box: %v", err)
	}
	member := singboxMemberName(version, goarch)
	tgz := buildTarGz(t, []tarEntry{{name: member, content: content}})
	sum := hashHex(tgz)
	return fakeDownloader{body: tgz}, sum
}

// --- test Roots under a temp dir, mirroring ProdRoots' leaf-name shape ---

func testTempRoots(t *testing.T) Roots {
	t.Helper()
	base := t.TempDir()
	etc := filepath.Join(base, "etc")
	varLib := filepath.Join(base, "var-lib")
	usrLib := filepath.Join(base, "usr-local-lib")
	systemdDir := filepath.Join(base, "systemd")
	for _, d := range []string{etc, varLib, usrLib, systemdDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	return Roots{
		SystemdUnit:   filepath.Join(systemdDir, "proxyctl-reality.service"),
		ConfigDir:     filepath.Join(etc, "proxyctl-reality"),
		Config:        filepath.Join(etc, "proxyctl-reality", "config.json"),
		StateDir:      filepath.Join(varLib, "proxyctl-reality"),
		Secrets:       filepath.Join(varLib, "proxyctl-reality", "secrets.json"),
		Instance:      filepath.Join(varLib, "proxyctl-reality", "instance.json"),
		Manifest:      filepath.Join(varLib, "proxyctl-reality", "manifest.json"),
		BinDir:        filepath.Join(usrLib, "proxyctl-reality"),
		TxnDir:        filepath.Join(varLib, "proxyctl-reality-txn"),
		TombstoneGlob: filepath.Join(varLib, ".proxyctl-reality-txn.tomb-*"),
	}
}

// testEnv builds a fully wired *Env against real temporary trusted roots
// (owned by the current uid, matching plan Task 7's "非root测试用临时目录
// 作可信根,属主取当前euid"), a fake downloader/systemd/dest-probe, and a
// deterministic clock/port selection. sysd is returned separately so
// tests can assert on its recorded call order. version is the version the
// fake "sing-box" binary reports for its own `version` invocation, and the
// version this Env's gate/proc wiring assumes will end up running.
func testEnv(t *testing.T, script string, version string) (*Env, *fakeServiceSystemd, Roots) {
	t.Helper()
	roots := testTempRoots(t)
	env, sysd := testEnvAt(t, roots, script, version)
	return env, sysd, roots
}

// testEnvAt is testEnv against caller-supplied roots (the crash tests'
// child process shares its roots with the parent through the filesystem).
func testEnvAt(t *testing.T, roots Roots, script string, version string) (*Env, *fakeServiceSystemd) {
	t.Helper()
	// mainPID/activeState make the §9.2 gate's systemd-side checks pass
	// immediately once Start/Restart runs (fakeServiceSystemd's own
	// Start/Restart already set activeState=active); the gate's PID and
	// exe checks are separately satisfied by healthyGateProc below.
	sysd := &fakeServiceSystemd{unitFileState: "disabled", mainPID: 4242}

	dlBody, dlSHA := fakeDownloaderFor(t, version, "amd64", script)

	// §9.1 dest probe: redirect to a real local TLS server instead of the
	// network (matching TestProbeDest's own established pattern).
	destSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(destSrv.Close)
	destAddr := destSrv.Listener.Addr().String()
	pool := x509.NewCertPool()
	pool.AddCert(destSrv.Certificate())
	tlsDialer := func(ctx context.Context, network, _ string, cfg *tls.Config) (*tls.Conn, error) {
		cfg.RootCAs = pool
		d := &tls.Dialer{Config: cfg}
		conn, err := d.DialContext(ctx, network, destAddr)
		if err != nil {
			return nil, err
		}
		return conn.(*tls.Conn), nil
	}

	// The §9.2 gate never touches a real /proc: it just needs to agree
	// that *some* PID owns the listening socket and that PID's exe equals
	// the target binary path this operation will end up recording.
	binPath := filepath.Join(roots.BinDir, "versions", version, "sing-box")
	proc := healthyGateProc(binPath)

	egressSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(egressSrv.Close)

	env := &Env{
		Roots:            roots,
		FS:               OSStateFS(),
		TrustedOwnerUID:  os.Getuid(),
		TrustedOwnerGID:  os.Getgid(),
		Now:              func() time.Time { return time.Unix(1700000000, 0).UTC() },
		NewTxnID:         txnIDCounter(),
		GOOS:             "linux",
		GOARCH:           "amd64",
		SystemdAvailable: func() bool { return true },
		CheckDiskSpace:   func(string, uint64) error { return nil },
		CheckPortBindable: func(port int) (bool, string) {
			return defaultCheckPortBindable(port)
		},
		Downloader:       dlBody,
		Runner:           ExecRunner{},
		Systemd:          sysd,
		Proc:             proc,
		TLSDialer:        tlsDialer,
		InterfaceLister:  func() ([]net.Addr, error) { return nil, nil },
		PortRange:        func() (int, int, error) { return 20000, 30000, nil },
		PortBind:         defaultPortBinder,
		GatePollBudget:   300 * time.Millisecond,
		GatePollInterval: 10 * time.Millisecond,
		DataPath:         DataPathConfig{ClientStartGrace: 100 * time.Millisecond, EgressURL: egressSrv.URL},
		ProxyctlVersion:  "v0.10.0",
		PinnedVersion:    version,
		PinnedSHA256ForArch: func(goarch string) (string, bool) {
			if goarch == "amd64" {
				return dlSHA, true
			}
			return "", false
		},
	}
	return env, sysd
}

func txnIDCounter() func() string {
	n := 0
	return func() string {
		n++
		return fmt.Sprintf("test%04d", n)
	}
}

var _ = transaction.CurrentSchema

// --- fixture builders (real files on disk) ---

func writeFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil { // WriteFile's mode is subject to umask
		t.Fatalf("chmod %s: %v", path, err)
	}
}

// installedFixture writes a self-consistent "installed" instance directly
// to disk (real files, current uid:gid — matching plan Task 7's non-root
// test convention) and returns the identity/instance values used, so a
// test can assert Show's output against them.
type installedFixture struct {
	uuid, privateKey, shortID string
	name, server, sni         string
	listenPort, selfcheckPort int
	version                   string
}

// writeInstalledFixture writes a self-consistent "installed" instance.
// binContent is the bytes stored as the private sing-box binary; pass a
// real executable fake (buildFakeSingboxScript) when the test will exec
// it (e.g. via reinstall's binary-reuse path or the §9.2 gate's check),
// or nil for an arbitrary placeholder when the test never executes it.
func writeInstalledFixture(t *testing.T, roots Roots, binContent []byte) installedFixture {
	t.Helper()
	return writeInstalledFixtureOpts(t, roots, fixtureOpts{binContent: binContent})
}

// fixtureOpts customizes a fixture; zero values keep writeInstalledFixture's
// original defaults.
type fixtureOpts struct {
	binContent    []byte
	version       string
	listenPort    int
	selfcheckPort int
	// prevVersion, when set, additionally installs (and registers) a
	// retained previous version directory with the same binary bytes,
	// as left behind by an earlier version switch.
	prevVersion string
}

func writeInstalledFixtureOpts(t *testing.T, roots Roots, o fixtureOpts) installedFixture {
	t.Helper()
	binContent := o.binContent
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	fx := installedFixture{
		uuid: "11111111-1111-4111-8111-111111111111", privateKey: mustGenPrivateKey(t), shortID: "0123456789abcdef",
		name: "jp1", server: "203.0.113.10", sni: "www.microsoft.com",
		listenPort: 443, selfcheckPort: 21000, version: "1.14.1",
	}
	if o.version != "" {
		fx.version = o.version
	}
	if o.listenPort != 0 {
		fx.listenPort = o.listenPort
	}
	if o.selfcheckPort != 0 {
		fx.selfcheckPort = o.selfcheckPort
	}
	unitData := RenderUnit(fx.version)
	cfgData, err := GenerateServerConfig(ServerConfigParams{ListenPort: fx.listenPort, SelfcheckPort: fx.selfcheckPort, SNI: fx.sni, UUID: fx.uuid, PrivateKey: fx.privateKey, ShortID: fx.shortID})
	if err != nil {
		t.Fatalf("GenerateServerConfig: %v", err)
	}
	binData := binContent
	if binData == nil {
		binData = []byte("fixture-sing-box-binary")
	}
	binPath := filepath.Join(roots.BinDir, "versions", fx.version, "sing-box")

	secretsData, _ := json.Marshal(SecretsFile{Schema: dynamicFileSchema, UUID: fx.uuid, PrivateKey: fx.privateKey, ShortID: fx.shortID})
	instanceData, _ := json.Marshal(InstanceFile{Schema: dynamicFileSchema, Name: fx.name, Server: fx.server, ListenPort: fx.listenPort, SNI: fx.sni, SelfcheckPort: fx.selfcheckPort})

	writeFile(t, roots.SystemdUnit, unitData, 0644)
	writeFile(t, roots.Config, cfgData, 0600)
	if err := os.Chmod(roots.ConfigDir, 0700); err != nil {
		t.Fatalf("chmod configdir: %v", err)
	}
	writeFile(t, roots.Secrets, secretsData, 0600)
	writeFile(t, roots.Instance, instanceData, 0600)
	if err := os.Chmod(roots.StateDir, 0700); err != nil {
		t.Fatalf("chmod statedir: %v", err)
	}
	writeFile(t, binPath, binData, 0755)
	if err := os.Chmod(roots.BinDir, 0755); err != nil {
		t.Fatalf("chmod bindir: %v", err)
	}
	if err := os.Chmod(filepath.Join(roots.BinDir, "versions"), 0755); err != nil {
		t.Fatalf("chmod versions dir: %v", err)
	}
	if err := os.Chmod(filepath.Join(roots.BinDir, "versions", fx.version), 0755); err != nil {
		t.Fatalf("chmod version dir: %v", err)
	}

	manifest := Manifest{
		Schema: manifestSchema, State: ManifestStateInstalled, SingboxCurrent: fx.version,
		Entries: []ManifestEntry{
			fileEntry(roots.SystemdUnit, 0644, uid, gid, hashHex(unitData)),
			dirEntry(roots.ConfigDir, 0700, uid, gid),
			fileEntry(roots.Config, 0600, uid, gid, hashHex(cfgData)),
			dirEntry(roots.StateDir, 0700, uid, gid),
			dynamicEntry(roots.Secrets, 0600, uid, gid),
			dynamicEntry(roots.Instance, 0600, uid, gid),
			dirEntry(roots.BinDir, 0755, uid, gid),
			dirEntry(filepath.Join(roots.BinDir, "versions"), 0755, uid, gid),
			dirEntry(filepath.Join(roots.BinDir, "versions", fx.version), 0755, uid, gid),
			fileEntry(binPath, 0755, uid, gid, hashHex(binData)),
		},
	}
	if o.prevVersion != "" {
		prevDir := filepath.Join(roots.BinDir, "versions", o.prevVersion)
		prevBin := filepath.Join(prevDir, "sing-box")
		writeFile(t, prevBin, binData, 0755)
		if err := os.Chmod(prevDir, 0755); err != nil {
			t.Fatalf("chmod prev version dir: %v", err)
		}
		manifest.SingboxPrevious = o.prevVersion
		manifest.Entries = append(manifest.Entries,
			dirEntry(prevDir, 0755, uid, gid), fileEntry(prevBin, 0755, uid, gid, hashHex(binData)))
	}
	manifestData, _ := json.MarshalIndent(manifest, "", "  ")
	writeFile(t, roots.Manifest, manifestData, 0600)
	return fx
}

// writeUninstalledFixture writes a self-consistent "uninstalled, identity
// retained" instance: only the state directory (secrets/instance/
// manifest) exists.
func writeUninstalledFixture(t *testing.T, roots Roots) installedFixture {
	t.Helper()
	return writeUninstalledFixtureOpts(t, roots, fixtureOpts{})
}

func writeUninstalledFixtureOpts(t *testing.T, roots Roots, o fixtureOpts) installedFixture {
	t.Helper()
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	fx := installedFixture{
		uuid: "22222222-2222-4222-8222-222222222222", privateKey: mustGenPrivateKey(t), shortID: "fedcba9876543210",
		name: "jp2", server: "203.0.113.20", sni: "www.microsoft.com",
		listenPort: 443, selfcheckPort: 21001, version: "1.14.1",
	}
	if o.listenPort != 0 {
		fx.listenPort = o.listenPort
	}
	if o.selfcheckPort != 0 {
		fx.selfcheckPort = o.selfcheckPort
	}
	secretsData, _ := json.Marshal(SecretsFile{Schema: dynamicFileSchema, UUID: fx.uuid, PrivateKey: fx.privateKey, ShortID: fx.shortID})
	instanceData, _ := json.Marshal(InstanceFile{Schema: dynamicFileSchema, Name: fx.name, Server: fx.server, ListenPort: fx.listenPort, SNI: fx.sni, SelfcheckPort: fx.selfcheckPort})
	writeFile(t, roots.Secrets, secretsData, 0600)
	writeFile(t, roots.Instance, instanceData, 0600)
	if err := os.Chmod(roots.StateDir, 0700); err != nil {
		t.Fatalf("chmod statedir: %v", err)
	}
	manifest := Manifest{
		Schema: manifestSchema, State: ManifestStateUninstalled, SingboxCurrent: fx.version,
		Entries: []ManifestEntry{
			dirEntry(roots.StateDir, 0700, uid, gid),
			dynamicEntry(roots.Secrets, 0600, uid, gid),
			dynamicEntry(roots.Instance, 0600, uid, gid),
		},
	}
	manifestData, _ := json.MarshalIndent(manifest, "", "  ")
	writeFile(t, roots.Manifest, manifestData, 0600)
	return fx
}

// --- TestServiceShowReadOnly (plan Task 7) ---

func snapshotFileStates(t *testing.T, paths []string) map[string]os.FileInfo {
	t.Helper()
	out := map[string]os.FileInfo{}
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatalf("lstat %s: %v", p, err)
		}
		out[p] = fi
	}
	return out
}

func assertUnchanged(t *testing.T, before map[string]os.FileInfo) {
	t.Helper()
	for p, fi := range before {
		after, err := os.Lstat(p)
		if err != nil {
			t.Fatalf("lstat %s after Show: %v", p, err)
		}
		if after.Size() != fi.Size() || after.Mode() != fi.Mode() || !after.ModTime().Equal(fi.ModTime()) {
			t.Fatalf("%s changed: before={size=%d mode=%v mtime=%v} after={size=%d mode=%v mtime=%v}",
				p, fi.Size(), fi.Mode(), fi.ModTime(), after.Size(), after.Mode(), after.ModTime())
		}
	}
}

func TestServiceShowReadOnly(t *testing.T) {
	t.Run("installed: prints add + commented alt enable, no writes", func(t *testing.T) {
		roots := testTempRoots(t)
		fx := writeInstalledFixture(t, roots, nil)
		env := &Env{Roots: roots, FS: OSStateFS(), ProxyctlVersion: "v0.10.0"}

		tracked := []string{roots.SystemdUnit, roots.Config, roots.Secrets, roots.Instance, roots.Manifest}
		before := snapshotFileStates(t, tracked)

		result := Show(context.Background(), env)
		assertUnchanged(t, before)

		if result.Err != nil || result.PreTransaction {
			t.Fatalf("Show failed: %+v", result)
		}
		pub, _ := DerivePublicKey(fx.privateKey)
		if !strings.Contains(result.Stdout, "node add") {
			t.Fatalf("stdout missing add command:\n%s", result.Stdout)
		}
		if !strings.Contains(result.Stdout, pub) {
			t.Fatalf("stdout missing live-derived public key %q:\n%s", pub, result.Stdout)
		}
		if !strings.Contains(result.Stdout, "# ") || !strings.Contains(result.Stdout, "node enable") {
			t.Fatalf("stdout missing commented alt enable command:\n%s", result.Stdout)
		}
		execLines := 0
		for _, l := range strings.Split(strings.TrimRight(result.Stdout, "\n"), "\n") {
			if !strings.HasPrefix(l, "#") {
				execLines++
			}
		}
		if execLines != 1 {
			t.Fatalf("want exactly 1 executable line, got %d:\n%s", execLines, result.Stdout)
		}
	})

	t.Run("uninstalled, identity retained: leading hint + disable + commented alt remove", func(t *testing.T) {
		roots := testTempRoots(t)
		writeUninstalledFixture(t, roots)
		env := &Env{Roots: roots, FS: OSStateFS(), ProxyctlVersion: "v0.10.0"}

		tracked := []string{roots.Secrets, roots.Instance, roots.Manifest}
		before := snapshotFileStates(t, tracked)

		result := Show(context.Background(), env)
		assertUnchanged(t, before)

		if result.Err != nil {
			t.Fatalf("Show failed: %+v", result)
		}
		if !strings.Contains(result.Stdout, "not running") {
			t.Fatalf("stdout missing lead hint:\n%s", result.Stdout)
		}
		if !strings.Contains(result.Stdout, "node disable") {
			t.Fatalf("stdout missing disable command:\n%s", result.Stdout)
		}
		if !strings.Contains(result.Stdout, "node remove") {
			t.Fatalf("stdout missing commented alt remove command:\n%s", result.Stdout)
		}
	})

	t.Run("not installed: exit 1, no output", func(t *testing.T) {
		roots := testTempRoots(t)
		env := &Env{Roots: roots, FS: OSStateFS()}
		result := Show(context.Background(), env)
		if result.Err == nil || !result.PreTransaction {
			t.Fatalf("want a PreTransaction error, got %+v", result)
		}
		if result.Stdout != "" {
			t.Fatalf("want no stdout, got %q", result.Stdout)
		}
	})

	t.Run("version mismatch: stderr hint only, stdout unaffected", func(t *testing.T) {
		roots := testTempRoots(t)
		writeInstalledFixture(t, roots, nil)
		// Rewrite the on-disk manifest's singbox_current to something
		// other than the compiled-in pinned SingboxVersion, simulating a
		// bootstrap-node.sh --upgrade that changed the pinned constant
		// without a reinstall having run yet (spec §8.3). Show only reads
		// this field for the warning; DetectInstanceState's own asset
		// checks never compare it against anything on disk.
		manifestData, err := os.ReadFile(roots.Manifest)
		if err != nil {
			t.Fatalf("read manifest: %v", err)
		}
		var m Manifest
		if err := json.Unmarshal(manifestData, &m); err != nil {
			t.Fatalf("unmarshal manifest: %v", err)
		}
		m.SingboxCurrent = SingboxVersion + "-mismatch"
		newData, _ := json.MarshalIndent(m, "", "  ")
		writeFile(t, roots.Manifest, newData, 0600)

		env := &Env{Roots: roots, FS: OSStateFS()}
		result := Show(context.Background(), env)
		if result.Err != nil {
			t.Fatalf("Show failed: %+v", result)
		}
		if !strings.Contains(result.Stdout, "node add") {
			t.Fatalf("stdout must be unaffected by the mismatch:\n%s", result.Stdout)
		}
		found := false
		for _, w := range result.Warnings {
			if strings.Contains(w, "reinstall") {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected a reinstall warning, got warnings=%v", result.Warnings)
		}
	})
}

// --- TestServiceLifecycleMatrix (plan Task 7) ---

// validInstallParams uses "example.com" as SNI (not the production
// default "www.microsoft.com") because the dest probe in tests dials a
// local httptest.NewTLSServer whose auto-generated certificate covers
// "example.com" (matching this package's own TestProbeDest convention);
// the SNI value itself is never otherwise semantically significant to
// these tests, which redirect every "dest" dial to the same local server
// regardless of hostname.
func validInstallParams() InstallParams {
	return InstallParams{Name: "jp1", Server: "203.0.113.10", Port: 443, SNI: "example.com"}
}

// assertNoTxnArea fails the test if a transaction area or tombstone
// exists under roots.
func assertNoTxnArea(t *testing.T, roots Roots) {
	t.Helper()
	if _, err := os.Stat(roots.TxnDir); err == nil {
		t.Fatalf("transaction area %s still exists", roots.TxnDir)
	}
	matches, _ := filepath.Glob(roots.TombstoneGlob)
	if len(matches) != 0 {
		t.Fatalf("tombstone(s) still exist: %v", matches)
	}
}

// assertNoInstanceAssets fails the test if any of the five asset roots
// (spec §5.1) exist.
func assertNoInstanceAssets(t *testing.T, roots Roots) {
	t.Helper()
	for _, p := range []string{roots.SystemdUnit, roots.ConfigDir, roots.StateDir, roots.BinDir} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s should not exist after rollback", p)
		}
	}
}

func TestInstallPrechecks(t *testing.T) {
	t.Run("invalid params: PreTransaction, no transaction area", func(t *testing.T) {
		script := buildFakeSingboxScript(t, "1.14.1")
		env, _, roots := testEnv(t, script, "1.14.1")
		bad := validInstallParams()
		bad.Name = "-leading-dash" // ValidateInstallParams rejects this
		result := Install(context.Background(), env, bad)
		if !result.PreTransaction || result.Err == nil {
			t.Fatalf("want PreTransaction error, got %+v", result)
		}
		assertNoTxnArea(t, roots)
		assertNoInstanceAssets(t, roots)
	})

	t.Run("already installed: PreTransaction, refuses without touching assets", func(t *testing.T) {
		script := buildFakeSingboxScript(t, "1.14.1")
		env, _, roots := testEnv(t, script, "1.14.1")
		writeInstalledFixture(t, roots, nil)
		before := snapshotFileStates(t, []string{roots.Manifest, roots.Config, roots.SystemdUnit})

		result := Install(context.Background(), env, validInstallParams())
		if !result.PreTransaction || result.Err == nil {
			t.Fatalf("want PreTransaction error, got %+v", result)
		}
		assertUnchanged(t, before)
		assertNoTxnArea(t, roots)
	})

	t.Run("listen_port not bindable: PreTransaction", func(t *testing.T) {
		script := buildFakeSingboxScript(t, "1.14.1")
		env, _, roots := testEnv(t, script, "1.14.1")
		l, err := net.Listen("tcp4", "0.0.0.0:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer l.Close()
		occupiedPort := l.Addr().(*net.TCPAddr).Port

		params := validInstallParams()
		params.Port = occupiedPort
		result := Install(context.Background(), env, params)
		if !result.PreTransaction || result.Err == nil {
			t.Fatalf("want PreTransaction error, got %+v", result)
		}
		assertNoTxnArea(t, roots)
	})

	t.Run("dest unreachable: PreTransaction", func(t *testing.T) {
		script := buildFakeSingboxScript(t, "1.14.1")
		env, _, roots := testEnv(t, script, "1.14.1")
		// Break the dest dialer deliberately.
		env.TLSDialer = func(ctx context.Context, network, addr string, cfg *tls.Config) (*tls.Conn, error) {
			return nil, fmt.Errorf("simulated: dest unreachable")
		}
		result := Install(context.Background(), env, validInstallParams())
		if !result.PreTransaction || result.Err == nil {
			t.Fatalf("want PreTransaction error, got %+v", result)
		}
		assertNoTxnArea(t, roots)
	})
}

// TestInstallRollbackOnSelfcheckFailure drives Install() through every
// precheck, the full download/identity/config/check sequence, every asset
// write, systemd daemon-reload/enable/start and the §9.2 gate (all real,
// against real temporary trusted roots) — then lets §9.3's temporary
// client deliberately fail to start (the fake "sing-box" exits 1 on `run`,
// deterministically, no network). This is the highest-value scenario for
// this L2-rated Task: it proves several real managed-path writes plus
// systemd state changes are correctly, automatically rolled back to
// nothing (spec §7.4, exit code 4) using this package's own journal
// planning glued to the already-reviewed transaction engine. See this
// file's header comment for why a *committed* install is not additionally
// covered here.
func TestInstallRollbackOnSelfcheckFailure(t *testing.T) {
	script := buildFakeSingboxScript(t, "1.14.1")
	env, sysd, roots := testEnv(t, script, "1.14.1")

	result := Install(context.Background(), env, validInstallParams())
	if result.Err != nil {
		t.Fatalf("unexpected hard error: %v (result=%+v)", result.Err, result)
	}
	if !result.HasOutcome || result.Outcome != transaction.OutcomeRolledBack {
		t.Fatalf("want OutcomeRolledBack, got %+v", result)
	}
	t.Logf("warnings: %v", result.Warnings)

	assertNoTxnArea(t, roots)
	assertNoInstanceAssets(t, roots)
	if _, err := os.Stat(roots.Manifest); err == nil {
		t.Fatalf("manifest.json must not exist after rollback")
	}

	calls := sysd.Calls()
	wantContains := []string{"daemon-reload", "enable", "start"}
	for _, w := range wantContains {
		found := false
		for _, c := range calls {
			if c == w {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected systemd call %q among %v", w, calls)
		}
	}
	// Rollback must disable and stop the unit it just enabled/started
	// (spec §7.4 step 3: "确保unit不再运行、也不再被enable").
	lastEnableLike := ""
	for _, c := range calls {
		if c == "enable" || c == "disable" {
			lastEnableLike = c
		}
	}
	if lastEnableLike != "disable" {
		t.Fatalf("expected the unit to end up disabled after rollback, calls=%v", calls)
	}

	// Final state must be judged StatusNotInstalled again — this is also
	// what a subsequent install/reinstall precheck relies on.
	state := DetectInstanceState(env.FS, roots)
	if state.Status != StatusNotInstalled {
		t.Fatalf("state after rollback = %v (reasons=%v), want StatusNotInstalled", state.Status, state.Reasons)
	}
}

func TestReinstallParamValidation(t *testing.T) {
	script := buildFakeSingboxScript(t, "1.14.1")
	env, _, _ := testEnv(t, script, "1.14.1")

	cases := []ReinstallParams{
		{SingboxVersion: "1.14.2"},               // sha missing
		{SingboxSHA256: strings.Repeat("a", 64)}, // version missing
	}
	for _, p := range cases {
		result := Reinstall(context.Background(), env, p)
		if !result.PreTransaction || result.Err == nil {
			t.Fatalf("params=%+v: want PreTransaction error, got %+v", p, result)
		}
	}
}

func TestReinstallNotEligible(t *testing.T) {
	script := buildFakeSingboxScript(t, "1.14.1")
	env, _, _ := testEnv(t, script, "1.14.1")
	// Not installed at all: reinstall must refuse.
	result := Reinstall(context.Background(), env, ReinstallParams{})
	if !result.PreTransaction || result.Err == nil {
		t.Fatalf("want PreTransaction error, got %+v", result)
	}
}

// TestReinstallFromInstalledRollback proves reinstall's snapshot-then-
// overwrite path for config.json/unit/manifest converges back to the
// EXACT pre-transaction bytes (not just "some old content") when
// selfcheck fails after the new files have already been written — the
// scenario spec §8.2 singles out ("失败时按§7.4回滚到事务前状态,包括旧的
// config、unit").
func TestReinstallFromInstalledRollback(t *testing.T) {
	script := buildFakeSingboxScript(t, "1.14.1")
	env, sysd, roots := testEnv(t, script, "1.14.1")
	scriptContent, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("read fake sing-box script: %v", err)
	}
	writeInstalledFixture(t, roots, scriptContent)
	sysd.loaded = true
	sysd.unitFileState = "enabled"
	sysd.activeState = "active"

	oldConfig, err := os.ReadFile(roots.Config)
	if err != nil {
		t.Fatalf("read old config: %v", err)
	}
	oldUnit, err := os.ReadFile(roots.SystemdUnit)
	if err != nil {
		t.Fatalf("read old unit: %v", err)
	}
	oldManifest, err := os.ReadFile(roots.Manifest)
	if err != nil {
		t.Fatalf("read old manifest: %v", err)
	}

	result := Reinstall(context.Background(), env, ReinstallParams{})
	if result.Err != nil {
		t.Fatalf("unexpected hard error: %v (result=%+v)", result.Err, result)
	}
	if !result.HasOutcome || result.Outcome != transaction.OutcomeRolledBack {
		t.Fatalf("want OutcomeRolledBack, got %+v", result)
	}

	assertNoTxnArea(t, roots)
	newConfig, err := os.ReadFile(roots.Config)
	if err != nil {
		t.Fatalf("read config after rollback: %v", err)
	}
	if string(newConfig) != string(oldConfig) {
		t.Fatalf("config.json not restored to pre-transaction bytes")
	}
	newUnit, err := os.ReadFile(roots.SystemdUnit)
	if err != nil {
		t.Fatalf("read unit after rollback: %v", err)
	}
	if string(newUnit) != string(oldUnit) {
		t.Fatalf("unit file not restored to pre-transaction bytes")
	}
	newManifest, err := os.ReadFile(roots.Manifest)
	if err != nil {
		t.Fatalf("read manifest after rollback: %v", err)
	}
	if string(newManifest) != string(oldManifest) {
		t.Fatalf("manifest.json not restored to pre-transaction bytes")
	}

	state := DetectInstanceState(env.FS, roots)
	if state.Status != StatusInstalled {
		t.Fatalf("state after rollback = %v (reasons=%v), want StatusInstalled (unchanged)", state.Status, state.Reasons)
	}
}
