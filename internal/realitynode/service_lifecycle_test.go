// service_lifecycle_test.go: committed-path and cancellation coverage for
// Install/Reinstall (plan Task 7 checklist P1-3 a/c/d, review items
// P1-1, P1-2, P2-6, P2-7, P2-8, P3-9). Unlike service_test.go's rollback
// scenarios, the fake "sing-box" used here is a shell wrapper that answers
// `version`/`check` itself and delegates `run` to this package's existing
// run_proxy helper process (helper_process_test.go), a genuine HTTP forward
// proxy — so the §9.3 nonce request really travels client -> E2 -> E3 and
// the whole transaction, including the manifest write, phase=committed and
// forward cleanup, runs to completion against real temporary trusted roots.
package realitynode

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hlpclg/singbox-sub-manager/internal/transaction"
)

// --- fake sing-box that can actually serve the selfcheck ---

type workingSingbox struct {
	path   string // the wrapper script (also the bytes the tarball carries)
	runLog string // every `run` appends its own $0 here
}

func buildWorkingFakeSingbox(t *testing.T, version string, checkFails bool) workingSingbox {
	t.Helper()
	dir := t.TempDir()
	runLog := filepath.Join(dir, "run.log")
	proxy := buildFakeBinary(t, "run_proxy")
	code := 0
	if checkFails {
		code = 1
	}
	script := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\n  version) echo 'sing-box version %s'; exit 0 ;;\n  check) exit %d ;;\n  run) echo \"$0\" >> '%s'; exec '%s' \"$@\" ;;\n  *) exit 1 ;;\nesac\n",
		version, code, runLog, proxy)
	p := filepath.Join(dir, "sing-box")
	if err := os.WriteFile(p, []byte(script), 0755); err != nil {
		t.Fatalf("write working fake sing-box: %v", err)
	}
	return workingSingbox{path: p, runLog: runLog}
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// useRelease points env at a release (tarball + pinned hash + expected
// running executable) of version built from script.
func useRelease(t *testing.T, env *Env, roots Roots, version, script string) (sha string) {
	t.Helper()
	dl, sha := fakeDownloaderFor(t, version, "amd64", script)
	env.Downloader = dl
	env.PinnedVersion = version
	env.PinnedSHA256ForArch = func(string) (string, bool) { return sha, true }
	env.Proc.(*fakeProc).exePath = versionBinaryPath(roots, version)
	return sha
}

func setHook(t *testing.T, fn func(point string) error) {
	t.Helper()
	orig := lifecycleHook
	lifecycleHook = fn
	t.Cleanup(func() { lifecycleHook = orig })
}

// --- tree snapshots (bytes, mode, owner, existence) ---

func treeSnapshot(t *testing.T, roots Roots) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, base := range []string{filepath.Dir(roots.ConfigDir), filepath.Dir(roots.StateDir), filepath.Dir(roots.BinDir), filepath.Dir(roots.SystemdUnit)} {
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			fi, err := os.Lstat(p)
			if err != nil {
				return err
			}
			st := fi.Sys().(*syscall.Stat_t)
			desc := fmt.Sprintf("%v %d:%d", fi.Mode(), st.Uid, st.Gid)
			if fi.Mode().IsRegular() {
				b, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				sum := sha256.Sum256(b)
				desc += " " + hex.EncodeToString(sum[:])
			}
			out[p] = desc
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", base, err)
		}
	}
	return out
}

func assertTreeEqual(t *testing.T, want, got map[string]string) {
	t.Helper()
	for p, d := range want {
		g, ok := got[p]
		if !ok {
			t.Errorf("%s existed before but is gone", p)
		} else if g != d {
			t.Errorf("%s changed: before %q, after %q", p, d, g)
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("%s did not exist before but exists now", p)
		}
	}
}

func assertOwnerMode(t *testing.T, path string, wantMode os.FileMode) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	if fi.Mode().Perm() != wantMode {
		t.Errorf("%s mode = %v, want %v", path, fi.Mode().Perm(), wantMode)
	}
	st := fi.Sys().(*syscall.Stat_t)
	if int(st.Uid) != os.Getuid() || int(st.Gid) != os.Getgid() {
		t.Errorf("%s owner = %d:%d, want %d:%d", path, st.Uid, st.Gid, os.Getuid(), os.Getgid())
	}
}

func assertNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s should not exist (err=%v)", path, err)
	}
}

// --- paste-output literals, independent of output.go ---

func wantAddLine(server string, port int, uuid, pub, sid, sni, name string) string {
	return fmt.Sprintf("sudo proxyctl node add --publish --min-version v0.10.0 --type vless-reality --server '%s' --port '%d' --uuid '%s' --public-key '%s' --short-id '%s' --sni '%s' --name '%s'",
		server, port, uuid, pub, sid, sni, name)
}

func wantNameLine(verb, name string) string {
	return fmt.Sprintf("sudo proxyctl node %s --publish --min-version v0.10.0 '%s'", verb, name)
}

// assertBlock checks spec §10.1's shape: exactly one executable line
// (equal to wantExec), every other line a "# " comment, and the commented
// alternatives (lines "# sudo proxyctl ...") equal to wantAlts in order.
func assertBlock(t *testing.T, out, wantExec string, wantAlts ...string) {
	t.Helper()
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("output must end with a newline: %q", out)
	}
	var exec, alts []string
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "# sudo proxyctl "):
			alts = append(alts, strings.TrimPrefix(l, "# "))
		case strings.HasPrefix(l, "# "):
		default:
			exec = append(exec, l)
		}
	}
	if len(exec) != 1 || exec[0] != wantExec {
		t.Fatalf("executable lines = %q, want exactly %q\nfull output:\n%s", exec, wantExec, out)
	}
	if strings.Join(alts, "\n") != strings.Join(wantAlts, "\n") {
		t.Fatalf("commented alternatives = %q, want %q\nfull output:\n%s", alts, wantAlts, out)
	}
	if !strings.HasPrefix(out, "# ") {
		t.Fatalf("block must start with a comment line:\n%s", out)
	}
}

type serverCfgDoc struct {
	Inbounds []struct {
		ListenPort int `json:"listen_port"`
		Users      []struct {
			UUID string `json:"uuid"`
		} `json:"users"`
		TLS struct {
			Reality struct {
				PrivateKey string   `json:"private_key"`
				ShortID    []string `json:"short_id"`
			} `json:"reality"`
		} `json:"tls"`
	} `json:"inbounds"`
	Route struct {
		Rules []map[string]interface{} `json:"rules"`
	} `json:"route"`
}

func readServerConfig(t *testing.T, path string) (serverCfgDoc, string) {
	t.Helper()
	raw := readBytes(t, path)
	var d serverCfgDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	return d, string(raw)
}

func readSecrets(t *testing.T, roots Roots) (SecretsFile, InstanceFile) {
	t.Helper()
	var s SecretsFile
	var i InstanceFile
	if err := json.Unmarshal(readBytes(t, roots.Secrets), &s); err != nil {
		t.Fatalf("parse secrets: %v", err)
	}
	if err := json.Unmarshal(readBytes(t, roots.Instance), &i); err != nil {
		t.Fatalf("parse instance: %v", err)
	}
	return s, i
}

func readManifestT(t *testing.T, roots Roots) Manifest {
	t.Helper()
	var m Manifest
	if err := json.Unmarshal(readBytes(t, roots.Manifest), &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	return m
}

func manifestPaths(m Manifest) map[string]ManifestEntry {
	out := map[string]ManifestEntry{}
	for _, e := range m.Entries {
		out[e.Path] = e
	}
	return out
}

func assertCommitted(t *testing.T, res Result) {
	t.Helper()
	if res.Err != nil || res.PreTransaction || !res.HasOutcome || res.Outcome != transaction.OutcomeCommitted {
		t.Fatalf("want committed, got %+v (cause=%v)", res, res.Cause)
	}
}

func assertInstalledState(t *testing.T, env *Env) {
	t.Helper()
	if st := DetectInstanceState(env.FS, env.Roots); st.Status != StatusInstalled {
		t.Fatalf("state = %v (reasons=%v), want StatusInstalled", st.Status, st.Reasons)
	}
}

func freeLoopbackListener(t *testing.T) (net.Listener, int) {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l, l.Addr().(*net.TCPAddr).Port
}

// --- install: committed success path (P1-3a, P3-9) ---

func TestInstallCommitPath(t *testing.T) {
	const version = "1.14.1"
	ws := buildWorkingFakeSingbox(t, version, false)
	env, sysd, roots := testEnv(t, ws.path, version)
	env.DataPath.ClientStartGrace = 5 * time.Second

	params := InstallParams{Name: "jp1", Server: "203.0.113.10", Port: freeTCPPort(t), SNI: "example.com"}
	res := Install(context.Background(), env, params)
	assertCommitted(t, res)
	assertNoTxnArea(t, roots)
	assertInstalledState(t, env)

	installedBin := versionBinaryPath(roots, version)
	secrets, inst := readSecrets(t, roots)

	// modes and owners, spec §5.1
	assertOwnerMode(t, roots.SystemdUnit, 0644)
	assertOwnerMode(t, roots.ConfigDir, 0700)
	assertOwnerMode(t, roots.Config, 0600)
	assertOwnerMode(t, roots.StateDir, 0700)
	assertOwnerMode(t, roots.Secrets, 0600)
	assertOwnerMode(t, roots.Instance, 0600)
	assertOwnerMode(t, roots.Manifest, 0600)
	assertOwnerMode(t, roots.BinDir, 0755)
	assertOwnerMode(t, filepath.Join(roots.BinDir, "versions"), 0755)
	assertOwnerMode(t, filepath.Join(roots.BinDir, "versions", version), 0755)
	assertOwnerMode(t, installedBin, 0755)

	// unit and binary
	if string(readBytes(t, roots.SystemdUnit)) != string(RenderUnit(version)) {
		t.Errorf("unit differs from the fixed template")
	}
	if string(readBytes(t, installedBin)) != string(readBytes(t, ws.path)) {
		t.Errorf("installed binary differs from the verified download")
	}

	// identity
	if !ValidUUIDv4(secrets.UUID) || !ValidGeneratedShortID(secrets.ShortID) {
		t.Errorf("identity malformed: %+v", secrets)
	}
	pub, err := DerivePublicKey(secrets.PrivateKey)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	cfg, raw := readServerConfig(t, roots.Config)
	if len(cfg.Inbounds) != 1 || cfg.Inbounds[0].Users[0].UUID != secrets.UUID ||
		cfg.Inbounds[0].TLS.Reality.PrivateKey != secrets.PrivateKey ||
		cfg.Inbounds[0].TLS.Reality.ShortID[0] != secrets.ShortID || cfg.Inbounds[0].ListenPort != params.Port {
		t.Errorf("config does not match stored identity/params: %s", raw)
	}
	if inst.Name != "jp1" || inst.Server != params.Server || inst.ListenPort != params.Port || inst.SNI != "example.com" ||
		inst.SelfcheckPort < 10000 || inst.SelfcheckPort > 19999 {
		t.Errorf("instance.json wrong: %+v", inst)
	}

	// manifest
	m := readManifestT(t, roots)
	if m.State != ManifestStateInstalled || m.SingboxCurrent != version || m.SingboxPrevious != "" {
		t.Errorf("manifest header wrong: %+v", m)
	}
	e, ok := manifestPaths(m)[installedBin]
	if !ok || e.SHA256 != hashHex(readBytes(t, installedBin)) {
		t.Errorf("manifest does not register the private binary hash: %+v", e)
	}

	// stdout equals the §10.1 add template exactly, one executable line
	assertBlock(t, res.Stdout, wantAddLine(params.Server, params.Port, secrets.UUID, pub, secrets.ShortID, "example.com", "jp1"))

	// no shared or unrelated asset was created
	want := map[string]bool{}
	for _, p := range []string{roots.SystemdUnit, roots.ConfigDir, roots.Config, roots.StateDir, roots.Secrets, roots.Instance, roots.Manifest,
		roots.BinDir, filepath.Join(roots.BinDir, "versions"), filepath.Join(roots.BinDir, "versions", version), installedBin} {
		want[p] = true
	}
	for p := range treeSnapshot(t, roots) {
		isRoot := p == filepath.Dir(roots.ConfigDir) || p == filepath.Dir(roots.StateDir) || p == filepath.Dir(roots.BinDir) || p == filepath.Dir(roots.SystemdUnit)
		if !isRoot && !want[p] {
			t.Errorf("unexpected path created: %s", p)
		}
	}

	// systemd order and the selfcheck client is the installed binary (§9.3)
	var seq []string
	for _, c := range sysd.Calls() {
		if c == "daemon-reload" || c == "enable" || c == "start" {
			seq = append(seq, c)
		}
	}
	if strings.Join(seq, ",") != "daemon-reload,enable,start" {
		t.Errorf("systemd call order = %v", seq)
	}
	runs := strings.Fields(string(readBytes(t, ws.runLog)))
	if len(runs) == 0 {
		t.Fatalf("temporary client never ran")
	}
	for _, r := range runs {
		if r != installedBin {
			t.Errorf("selfcheck client ran %s, want the installed binary %s", r, installedBin)
		}
	}
}

// --- reinstall, source "installed" (P1-3a, P1-3d) ---

func TestReinstallFromInstalledCommit(t *testing.T) {
	const version = "1.14.1"
	ws := buildWorkingFakeSingbox(t, version, false)
	env, sysd, roots := testEnv(t, ws.path, version)
	env.DataPath.ClientStartGrace = 5 * time.Second
	env.InterfaceLister = func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("192.0.2.7"), Mask: net.CIDRMask(24, 32)}}, nil
	}
	occupied, occupiedPort := freeLoopbackListener(t) // makes the recorded selfcheck_port unusable
	_ = occupied
	fx := writeInstalledFixtureOpts(t, roots, fixtureOpts{binContent: readBytes(t, ws.path), listenPort: freeTCPPort(t), selfcheckPort: occupiedPort})
	sysd.loaded, sysd.unitFileState, sysd.activeState = true, "enabled", "active"

	beforeSecrets := readBytes(t, roots.Secrets)
	res := Reinstall(context.Background(), env, ReinstallParams{})
	assertCommitted(t, res)
	assertNoTxnArea(t, roots)
	assertInstalledState(t, env)

	if string(readBytes(t, roots.Secrets)) != string(beforeSecrets) {
		t.Errorf("identity must not change on reinstall")
	}
	_, inst := readSecrets(t, roots)
	if inst.SelfcheckPort == occupiedPort || inst.SelfcheckPort < 10000 || inst.SelfcheckPort > 19999 {
		t.Errorf("selfcheck_port not reselected: %d (occupied %d)", inst.SelfcheckPort, occupiedPort)
	}
	if inst.Name != fx.name || inst.ListenPort != fx.listenPort || inst.Server != fx.server {
		t.Errorf("instance parameters must be preserved: %+v", inst)
	}
	cfg, raw := readServerConfig(t, roots.Config)
	if !strings.Contains(raw, "192.0.2.7/32") {
		t.Errorf("refreshed local address missing from config: %s", raw)
	}
	if got := cfg.Route.Rules[0]["port"]; fmt.Sprint(got) != fmt.Sprint([]interface{}{float64(inst.SelfcheckPort)}) {
		t.Errorf("config selfcheck allow rule = %v, want port %d", got, inst.SelfcheckPort)
	}
	m := readManifestT(t, roots)
	if m.State != ManifestStateInstalled || m.SingboxCurrent != version || m.SingboxPrevious != "" {
		t.Errorf("manifest = %+v", m)
	}
	assertOwnerMode(t, roots.Config, 0600)
	assertOwnerMode(t, roots.SystemdUnit, 0644)
	assertOwnerMode(t, roots.Manifest, 0600)

	pub, _ := DerivePublicKey(fx.privateKey)
	assertBlock(t, res.Stdout, wantAddLine(fx.server, fx.listenPort, fx.uuid, pub, fx.shortID, fx.sni, fx.name))

	var seq []string
	for _, c := range sysd.Calls() {
		if c == "daemon-reload" || c == "enable" || c == "restart" || c == "start" {
			seq = append(seq, c)
		}
	}
	if strings.Join(seq, ",") != "daemon-reload,enable,restart" {
		t.Errorf("systemd call order = %v", seq)
	}
}

// --- reinstall, source "uninstalled but identity retained" (P1-3d) ---

func TestReinstallFromUninstalledCommit(t *testing.T) {
	const version = "1.14.1"
	ws := buildWorkingFakeSingbox(t, version, false)
	env, sysd, roots := testEnv(t, ws.path, version)
	env.DataPath.ClientStartGrace = 5 * time.Second
	env.InterfaceLister = func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("192.0.2.9"), Mask: net.CIDRMask(24, 32)}}, nil
	}
	_, occupiedPort := freeLoopbackListener(t)
	fx := writeUninstalledFixtureOpts(t, roots, fixtureOpts{listenPort: freeTCPPort(t), selfcheckPort: occupiedPort})
	assertNotExist(t, roots.ConfigDir)
	assertNotExist(t, roots.BinDir)
	assertNotExist(t, roots.SystemdUnit)
	beforeSecrets := readBytes(t, roots.Secrets)

	res := Reinstall(context.Background(), env, ReinstallParams{})
	assertCommitted(t, res)
	assertNoTxnArea(t, roots)
	assertInstalledState(t, env)

	// directory chain rebuilt with the right modes/owners
	assertOwnerMode(t, roots.ConfigDir, 0700)
	assertOwnerMode(t, roots.Config, 0600)
	assertOwnerMode(t, roots.SystemdUnit, 0644)
	assertOwnerMode(t, roots.BinDir, 0755)
	assertOwnerMode(t, filepath.Join(roots.BinDir, "versions"), 0755)
	assertOwnerMode(t, filepath.Join(roots.BinDir, "versions", version), 0755)
	assertOwnerMode(t, versionBinaryPath(roots, version), 0755)
	assertOwnerMode(t, roots.Manifest, 0600)

	if string(readBytes(t, roots.Secrets)) != string(beforeSecrets) {
		t.Errorf("identity must not be regenerated")
	}
	_, inst := readSecrets(t, roots)
	if inst.SelfcheckPort == occupiedPort {
		t.Errorf("selfcheck_port was not reselected")
	}
	_, raw := readServerConfig(t, roots.Config)
	if !strings.Contains(raw, "192.0.2.9/32") {
		t.Errorf("refreshed local address missing from config")
	}
	m := readManifestT(t, roots)
	if m.State != ManifestStateInstalled || m.SingboxCurrent != version || m.SingboxPrevious != "" {
		t.Errorf("manifest = %+v", m)
	}
	pub, _ := DerivePublicKey(fx.privateKey)
	assertBlock(t, res.Stdout,
		wantNameLine("enable", fx.name),
		wantAddLine(fx.server, fx.listenPort, fx.uuid, pub, fx.shortID, fx.sni, fx.name))

	var seq []string
	for _, c := range sysd.Calls() {
		if c == "daemon-reload" || c == "enable" || c == "restart" || c == "start" {
			seq = append(seq, c)
		}
	}
	if strings.Join(seq, ",") != "daemon-reload,enable,restart" {
		t.Errorf("systemd call order = %v", seq)
	}
}

// --- two-version window (P1-2) ---

func installV1(t *testing.T) (*Env, *fakeServiceSystemd, Roots, workingSingbox, string) {
	t.Helper()
	ws := buildWorkingFakeSingbox(t, "1.14.1", false)
	env, sysd, roots := testEnv(t, ws.path, "1.14.1")
	env.DataPath.ClientStartGrace = 5 * time.Second
	sha1, _ := env.PinnedSHA256ForArch("amd64")
	res := Install(context.Background(), env, InstallParams{Name: "jp1", Server: "203.0.113.10", Port: freeTCPPort(t), SNI: "example.com"})
	assertCommitted(t, res)
	return env, sysd, roots, ws, sha1
}

func reinstallTo(t *testing.T, ctx context.Context, env *Env, roots Roots, version string) Result {
	t.Helper()
	ws := buildWorkingFakeSingbox(t, version, false)
	sha := useRelease(t, env, roots, version, ws.path)
	return Reinstall(ctx, env, ReinstallParams{SingboxVersion: version, SingboxSHA256: sha})
}

func assertVersionEntries(t *testing.T, env *Env, roots Roots, cur, prev string, gone ...string) {
	t.Helper()
	m := readManifestT(t, roots)
	if m.SingboxCurrent != cur || m.SingboxPrevious != prev {
		t.Fatalf("manifest current/previous = %s/%s, want %s/%s", m.SingboxCurrent, m.SingboxPrevious, cur, prev)
	}
	entries := manifestPaths(m)
	for _, v := range []string{cur, prev} {
		if v == "" {
			continue
		}
		dir := filepath.Join(roots.BinDir, "versions", v)
		bin := filepath.Join(dir, "sing-box")
		if _, ok := entries[dir]; !ok {
			t.Errorf("manifest does not register version dir %s", dir)
		}
		e, ok := entries[bin]
		if !ok || e.SHA256 != hashHex(readBytes(t, bin)) {
			t.Errorf("manifest does not register binary %s with its hash (%+v)", bin, e)
		}
	}
	for _, v := range gone {
		dir := filepath.Join(roots.BinDir, "versions", v)
		assertNotExist(t, dir)
		if _, ok := entries[dir]; ok {
			t.Errorf("manifest still registers removed version %s", v)
		}
	}
	assertInstalledState(t, env)
}

func TestReinstallVersionWindow(t *testing.T) {
	env, _, roots, _, _ := installV1(t)
	ctx := context.Background()

	res := reinstallTo(t, ctx, env, roots, "1.14.2")
	assertCommitted(t, res)
	assertVersionEntries(t, env, roots, "1.14.2", "1.14.1")
	if string(readBytes(t, roots.SystemdUnit)) != string(RenderUnit("1.14.2")) {
		t.Errorf("unit must point at the new version")
	}

	res = reinstallTo(t, ctx, env, roots, "1.14.3")
	assertCommitted(t, res)
	assertVersionEntries(t, env, roots, "1.14.3", "1.14.2", "1.14.1")
	assertNoTxnArea(t, roots)
}

// TestReinstallSameVersionKeepsPrevious: a plain reinstall whose target is
// the current version leaves the retained previous version registered AND
// on disk (review R1: the cleanup decision must follow the same newPrev the
// manifest is built from).
func TestReinstallSameVersionKeepsPrevious(t *testing.T) {
	env, _, roots, _, _ := installV1(t)
	ctx := context.Background()
	assertCommitted(t, reinstallTo(t, ctx, env, roots, "1.14.2")) // current v2, previous v1

	ws2 := buildWorkingFakeSingbox(t, "1.14.2", false)
	useRelease(t, env, roots, "1.14.2", ws2.path) // pinned target = v2 = current
	res := Reinstall(ctx, env, ReinstallParams{})
	assertCommitted(t, res)

	for _, p := range []string{filepath.Join(roots.BinDir, "versions", "1.14.1"), versionBinaryPath(roots, "1.14.1"),
		filepath.Join(roots.BinDir, "versions", "1.14.2"), versionBinaryPath(roots, "1.14.2")} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s must still exist: %v", p, err)
		}
	}
	assertVersionEntries(t, env, roots, "1.14.2", "1.14.1")
	assertInstalledState(t, env)
	assertNoTxnArea(t, roots)
}

func TestReinstallTamperedPreviousVersion(t *testing.T) {
	t.Run("tampered before the run is refused with no change", func(t *testing.T) {
		env, _, roots, _, _ := installV1(t)
		ctx := context.Background()
		assertCommitted(t, reinstallTo(t, ctx, env, roots, "1.14.2"))
		v1bin := versionBinaryPath(roots, "1.14.1")
		if err := os.WriteFile(v1bin, []byte("tampered"), 0755); err != nil {
			t.Fatal(err)
		}
		before := treeSnapshot(t, roots)
		res := reinstallTo(t, ctx, env, roots, "1.14.3")
		if !res.PreTransaction || res.Err == nil {
			t.Fatalf("want a pre-transaction refusal, got %+v", res)
		}
		assertTreeEqual(t, before, treeSnapshot(t, roots))
	})

	t.Run("tampered during the run rolls back and is never deleted", func(t *testing.T) {
		env, sysd, roots, _, _ := installV1(t)
		ctx := context.Background()
		assertCommitted(t, reinstallTo(t, ctx, env, roots, "1.14.2"))
		v1bin := versionBinaryPath(roots, "1.14.1")
		oldManifest := readBytes(t, roots.Manifest)
		_ = sysd
		setHook(t, func(point string) error {
			switch point {
			case "done:manifest-write":
				if err := os.WriteFile(v1bin, []byte("tampered"), 0755); err != nil {
					t.Error(err)
				}
			case "wrapup":
				// rollback re-gates the pre-transaction (v2) binary
				env.Proc.(*fakeProc).exePath = versionBinaryPath(roots, "1.14.2")
			}
			return nil
		})
		res := reinstallTo(t, ctx, env, roots, "1.14.3")
		if !res.HasOutcome || res.Outcome != transaction.OutcomeRolledBack || res.Err != nil {
			t.Fatalf("want rolled back (4), got %+v", res)
		}
		if got := readBytes(t, v1bin); string(got) != "tampered" {
			t.Errorf("the mismatching previous binary must be left alone, got %q", got)
		}
		if string(readBytes(t, roots.Manifest)) != string(oldManifest) {
			t.Errorf("manifest not restored")
		}
		assertNotExist(t, filepath.Join(roots.BinDir, "versions", "1.14.3"))
	})

	t.Run("tampered after registration is refused by the engine at cleanup", func(t *testing.T) {
		env, _, roots, _, _ := installV1(t)
		ctx := context.Background()
		assertCommitted(t, reinstallTo(t, ctx, env, roots, "1.14.2"))
		v1bin := versionBinaryPath(roots, "1.14.1")
		setHook(t, func(point string) error {
			if point == "after-commit-flip" {
				return os.WriteFile(v1bin, []byte("tampered"), 0755)
			}
			return nil
		})
		res := reinstallTo(t, ctx, env, roots, "1.14.3")
		if res.Err == nil || res.PreTransaction || res.TxnDir != roots.TxnDir {
			t.Fatalf("want a needs-intervention result naming the txn area, got %+v", res)
		}
		if got := readBytes(t, v1bin); string(got) != "tampered" {
			t.Errorf("tampered previous binary was deleted: %q", got)
		}
	})
}

// --- explicit version / hash boundaries (P2-8) ---

func TestReinstallExplicitVersionBoundaries(t *testing.T) {
	ctx := context.Background()

	t.Run("same version with a matching hash reuses the installed binary", func(t *testing.T) {
		env, _, roots, _, sha1 := installV1(t)
		before := readBytes(t, versionBinaryPath(roots, "1.14.1"))
		res := Reinstall(ctx, env, ReinstallParams{SingboxVersion: "1.14.1", SingboxSHA256: sha1})
		assertCommitted(t, res)
		assertVersionEntries(t, env, roots, "1.14.1", "")
		if string(readBytes(t, versionBinaryPath(roots, "1.14.1"))) != string(before) {
			t.Errorf("installed binary changed")
		}
	})

	t.Run("same version with different bytes is refused before any change", func(t *testing.T) {
		env, _, roots, _, _ := installV1(t)
		other := buildWorkingFakeSingbox(t, "1.14.1", false) // same version string, different bytes
		sha := useRelease(t, env, roots, "1.14.1", other.path)
		before := treeSnapshot(t, roots)
		res := Reinstall(ctx, env, ReinstallParams{SingboxVersion: "1.14.1", SingboxSHA256: sha})
		if !res.PreTransaction || res.Err == nil {
			t.Fatalf("want exit-1 refusal, got %+v", res)
		}
		assertTreeEqual(t, before, treeSnapshot(t, roots))
		assertNoTxnArea(t, roots)
	})

	t.Run("switching back to the retained previous version", func(t *testing.T) {
		env, _, roots, ws1, sha1 := installV1(t)
		assertCommitted(t, reinstallTo(t, ctx, env, roots, "1.14.2"))
		useRelease(t, env, roots, "1.14.1", ws1.path) // the original v1 tarball again
		res := Reinstall(ctx, env, ReinstallParams{SingboxVersion: "1.14.1", SingboxSHA256: sha1})
		assertCommitted(t, res)
		assertVersionEntries(t, env, roots, "1.14.1", "1.14.2")
		for _, v := range []string{"1.14.1", "1.14.2"} { // current/previous swapped, nothing removed
			for _, p := range []string{filepath.Join(roots.BinDir, "versions", v), versionBinaryPath(roots, v)} {
				if _, err := os.Lstat(p); err != nil {
					t.Errorf("%s must be kept after switching back: %v", p, err)
				}
			}
		}
		if string(readBytes(t, roots.SystemdUnit)) != string(RenderUnit("1.14.1")) {
			t.Errorf("unit must point back at v1")
		}
	})

	t.Run("an unregistered existing version directory is refused up front", func(t *testing.T) {
		env, _, roots, _, _ := installV1(t)
		stray := filepath.Join(roots.BinDir, "versions", "1.14.9")
		if err := os.Mkdir(stray, 0755); err != nil {
			t.Fatal(err)
		}
		before := treeSnapshot(t, roots)
		res := reinstallTo(t, ctx, env, roots, "1.14.9")
		if !res.PreTransaction || res.Err == nil {
			t.Fatalf("want exit-1 refusal, got %+v", res)
		}
		assertTreeEqual(t, before, treeSnapshot(t, roots))
		assertNoTxnArea(t, roots)
	})
}

// --- reused binary is checked against the NEW config before any change (P2-7) ---

func TestReinstallReusedBinaryCheckFails(t *testing.T) {
	ws := buildWorkingFakeSingbox(t, "1.14.1", true) // `check` always fails
	env, sysd, roots := testEnv(t, ws.path, "1.14.1")
	writeInstalledFixtureOpts(t, roots, fixtureOpts{binContent: readBytes(t, ws.path), listenPort: freeTCPPort(t)})
	sysd.loaded, sysd.unitFileState, sysd.activeState = true, "enabled", "active"
	before := treeSnapshot(t, roots)

	res := Reinstall(context.Background(), env, ReinstallParams{})
	if !res.PreTransaction || res.Err == nil {
		t.Fatalf("want exit 1 with no change, got %+v", res)
	}
	assertTreeEqual(t, before, treeSnapshot(t, roots))
	assertNoTxnArea(t, roots)
	for _, c := range sysd.Calls() {
		if c == "daemon-reload" || c == "enable" || c == "restart" || c == "stop" || c == "disable" {
			t.Errorf("systemd must not be touched, saw %q", c)
		}
	}
}

// --- rollback is not interrupted by a signal (P1-1) ---

func TestRollbackNotInterruptedByCancel(t *testing.T) {
	script := buildFakeSingboxScript(t, "1.14.1") // `run` exits 1: selfcheck fails, forcing a rollback
	env, sysd, roots := testEnv(t, script, "1.14.1")
	fx := writeInstalledFixtureOpts(t, roots, fixtureOpts{binContent: readBytes(t, script), listenPort: freeTCPPort(t)})
	sysd.loaded, sysd.unitFileState, sysd.activeState = true, "enabled", "active"
	before := treeSnapshot(t, roots)
	_ = fx

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sysd.onRestart = func(n int) {
		if n == 2 { // the rollback's restart (n==1 is the forward one)
			cancel()
		}
	}
	res := Reinstall(ctx, env, ReinstallParams{})
	if ctx.Err() == nil {
		t.Fatalf("the test never cancelled the main context")
	}
	if res.Err != nil || !res.HasOutcome || res.Outcome != transaction.OutcomeRolledBack {
		t.Fatalf("want rolled back (exit 4) despite the cancel, got %+v", res)
	}
	assertTreeEqual(t, before, treeSnapshot(t, roots))
	assertNoTxnArea(t, roots)
	assertInstalledState(t, env)
}

// --- cancellation matrix (P1-3c): before the boundary, mid-transaction,
// committed cleanup. (Mid-rollback: TestRollbackNotInterruptedByCancel; lock
// wait: cmd/proxyctl's TestNodeServiceLockWaitCancel.)

func TestCancellationMatrix(t *testing.T) {
	t.Run("before the modification boundary is exit 1 and leaves nothing", func(t *testing.T) {
		ws := buildWorkingFakeSingbox(t, "1.14.1", false)
		env, _, roots := testEnv(t, ws.path, "1.14.1")
		before := treeSnapshot(t, roots)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		setHook(t, func(point string) error {
			if point == "downloaded" {
				cancel()
			}
			return nil
		})
		res := Install(ctx, env, InstallParams{Name: "jp1", Server: "203.0.113.10", Port: freeTCPPort(t), SNI: "example.com"})
		if !res.PreTransaction || res.Err == nil || !res.HasOutcome || res.Outcome != transaction.OutcomeNoChange {
			t.Fatalf("want no-change (exit 1), got %+v", res)
		}
		assertTreeEqual(t, before, treeSnapshot(t, roots))
		assertNoTxnArea(t, roots)
	})

	t.Run("mid-transaction is rolled back to nothing, exit 4", func(t *testing.T) {
		ws := buildWorkingFakeSingbox(t, "1.14.1", false)
		env, _, roots := testEnv(t, ws.path, "1.14.1")
		before := treeSnapshot(t, roots)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		setHook(t, func(point string) error {
			if point == "done:write-config" {
				cancel()
			}
			return nil
		})
		res := Install(ctx, env, InstallParams{Name: "jp1", Server: "203.0.113.10", Port: freeTCPPort(t), SNI: "example.com"})
		if res.Err != nil || res.PreTransaction || !res.HasOutcome || res.Outcome != transaction.OutcomeRolledBack {
			t.Fatalf("want rolled back (exit 4), got %+v", res)
		}
		assertTreeEqual(t, before, treeSnapshot(t, roots))
		assertNoTxnArea(t, roots)
	})

	t.Run("committed cleanup only moves forward", func(t *testing.T) {
		env, _, roots, _, _ := installV1(t)
		ctx0 := context.Background()
		assertCommitted(t, reinstallTo(t, ctx0, env, roots, "1.14.2")) // window: v2 + v1

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		setHook(t, func(point string) error {
			if point == "after-commit-flip" {
				cancel()
			}
			return nil
		})
		res := reinstallTo(t, ctx, env, roots, "1.14.3") // v1 falls out of the window
		if ctx.Err() == nil {
			t.Fatalf("the test never cancelled")
		}
		assertCommitted(t, res)
		assertVersionEntries(t, env, roots, "1.14.3", "1.14.2", "1.14.1")
		assertNoTxnArea(t, roots)
	})
}

// --- exit-code / cleanup boundaries after BeginTransaction (P2-6) ---

func TestFailureAfterTransactionBegan(t *testing.T) {
	t.Run("an unconfirmable recovery is needs-intervention, never exit 1", func(t *testing.T) {
		script := buildFakeSingboxScript(t, "1.14.1") // selfcheck fails -> rollback
		env, sysd, roots := testEnv(t, script, "1.14.1")
		sysd.stopErr = errors.New("simulated: systemctl stop failed")
		res := Install(context.Background(), env, validInstallParams())
		if res.PreTransaction || res.Err == nil {
			t.Fatalf("want a non-pre-transaction error, got %+v", res)
		}
		if errors.Is(res.Err, transaction.ErrTampered) || errors.Is(res.Err, transaction.ErrGateFailed) || errors.Is(res.Err, transaction.ErrRollbackTimeout) {
			t.Fatalf("this case must exercise the unclassified-error path, got %v", res.Err)
		}
		if res.TxnDir != roots.TxnDir {
			t.Errorf("TxnDir = %q, want %q", res.TxnDir, roots.TxnDir)
		}
		if _, err := os.Stat(roots.TxnDir); err != nil {
			t.Errorf("the transaction area must be preserved: %v", err)
		}
	})

	t.Run("a failed commit flip goes through recovery and rolls back", func(t *testing.T) {
		ws := buildWorkingFakeSingbox(t, "1.14.1", false)
		env, _, roots := testEnv(t, ws.path, "1.14.1")
		env.DataPath.ClientStartGrace = 5 * time.Second
		before := treeSnapshot(t, roots)
		blocker := filepath.Join(roots.TxnDir, "journal.json.tmp")
		setHook(t, func(point string) error {
			switch point {
			case "before-commit":
				// A directory where the journal's temp file must go makes
				// the next journal update fail.
				return os.Mkdir(blocker, 0700)
			case "wrapup":
				_ = os.Remove(blocker) // let the recovery's own updates through
			}
			return nil
		})
		res := Install(context.Background(), env, InstallParams{Name: "jp1", Server: "203.0.113.10", Port: freeTCPPort(t), SNI: "example.com"})
		if res.Err != nil || !res.HasOutcome || res.Outcome != transaction.OutcomeRolledBack {
			t.Fatalf("want rolled back (exit 4), got %+v", res)
		}
		assertTreeEqual(t, before, treeSnapshot(t, roots))
		assertNoTxnArea(t, roots)
	})

	t.Run("snap directory failure removes the transaction area", func(t *testing.T) {
		ws := buildWorkingFakeSingbox(t, "1.14.1", false)
		env, sysd, roots := testEnv(t, ws.path, "1.14.1")
		writeInstalledFixtureOpts(t, roots, fixtureOpts{binContent: readBytes(t, ws.path), listenPort: freeTCPPort(t)})
		sysd.loaded, sysd.unitFileState, sysd.activeState = true, "enabled", "active"
		before := treeSnapshot(t, roots)
		setHook(t, func(point string) error {
			if point == "mkdir-snap" {
				return errors.New("simulated: cannot create snap/")
			}
			return nil
		})
		res := Reinstall(context.Background(), env, ReinstallParams{})
		if !res.PreTransaction || res.Err == nil || !res.HasOutcome || res.Outcome != transaction.OutcomeNoChange {
			t.Fatalf("want no-change (exit 1), got %+v", res)
		}
		assertTreeEqual(t, before, treeSnapshot(t, roots))
		assertNoTxnArea(t, roots)
	})
}

// --- pre-lock precheck vs post-lock recheck split (P1-4) ---

func TestPrecheckThenRecheck(t *testing.T) {
	ws := buildWorkingFakeSingbox(t, "1.14.1", false)
	env, _, roots := testEnv(t, ws.path, "1.14.1")
	env.DataPath.ClientStartGrace = 5 * time.Second
	destCalls := 0
	inner := env.TLSDialer
	env.TLSDialer = func(ctx context.Context, n, a string, cfg *tls.Config) (*tls.Conn, error) {
		destCalls++
		return inner(ctx, n, a, cfg)
	}
	params := InstallParams{Name: "jp1", Server: "203.0.113.10", Port: freeTCPPort(t), SNI: "example.com"}

	plan, err := PrecheckInstall(context.Background(), env, params)
	if err != nil {
		t.Fatalf("precheck: %v", err)
	}
	if destCalls != 1 {
		t.Fatalf("PrecheckInstall must run the dest probe once, ran %d", destCalls)
	}
	assertNotExist(t, roots.ConfigDir) // the precheck modifies nothing
	assertNoTxnArea(t, roots)

	t.Run("recheck rejects a path conflict that appeared after the precheck", func(t *testing.T) {
		if err := os.Mkdir(roots.ConfigDir, 0700); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(roots.ConfigDir)
		res := RunInstall(context.Background(), env, plan)
		if !res.PreTransaction || res.Err == nil {
			t.Fatalf("want a pre-transaction refusal, got %+v", res)
		}
	})

	t.Run("recheck rejects a selfcheck_port taken after the precheck", func(t *testing.T) {
		l, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", plan.SelfcheckPort))
		if err != nil {
			t.Skipf("cannot occupy the port: %v", err)
		}
		defer l.Close()
		res := RunInstall(context.Background(), env, plan)
		if !res.PreTransaction || res.Err == nil {
			t.Fatalf("want a pre-transaction refusal, got %+v", res)
		}
	})

	t.Run("recheck rejects a listen_port taken after the precheck", func(t *testing.T) {
		l, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", params.Port))
		if err != nil {
			t.Skipf("cannot occupy the port: %v", err)
		}
		defer l.Close()
		res := RunInstall(context.Background(), env, plan)
		if !res.PreTransaction || res.Err == nil {
			t.Fatalf("want a pre-transaction refusal, got %+v", res)
		}
	})

	t.Run("a clean recheck proceeds without a second dest probe", func(t *testing.T) {
		res := RunInstall(context.Background(), env, plan)
		assertCommitted(t, res)
		if destCalls != 1 {
			t.Errorf("RunInstall must not re-run the dest probe (calls=%d)", destCalls)
		}
	})
}
