//go:build reality_integration

// Package realitynode holds the v0.10 Reality node implementation.
//
// This file is Task 1 of the v0.10 plan
// (docs/superpowers/plans/2026-09-18-v0.10-reality-node-install.md): a
// real-runtime compatibility gate for the pinned sing-box release, the
// fixed systemd unit template, and the private-network rejection rules
// from docs/superpowers/specs/2026-09-18-v0.10-reality-node-install-design.md
// (§3, §6, §8.1, §9, §14.1). It does not implement any production
// lifecycle code; later Tasks own that.
//
// Every test here either exercises the real pinned sing-box binary and a
// real network round trip, or explicitly skips with a reason when the
// current environment cannot provide that (no real systemd, no second
// architecture, no genuinely-owned private-network host). See
// docs/reality-node-validation.md for the full record of what has and has
// not been exercised, and why.
package realitynode

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hlpclg/singbox-sub-manager/internal/health/remote"
	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
)

// downloadCacheDir holds tarballs for the lifetime of one test binary
// run, shared across every test and subtest in this package. Each of
// the four exported tests independently needs the amd64 asset (and
// TestPinnedReleaseProbe needs arm64 too); without this cache a full run
// re-downloads the ~30 MB amd64 tarball five times, which both wastes
// time and, observed directly during this Task's validation, can trip a
// transient 502 from the release CDN under repeated load. t.TempDir()
// cannot serve this purpose because a subtest's TempDir is removed when
// that subtest returns, before later sibling tests run.
var (
	downloadCacheOnce sync.Once
	downloadCacheDir  string
	downloadCacheMu   sync.Mutex
	downloadCachePath = map[string]string{}
)

func cacheDir(t *testing.T) string {
	t.Helper()
	downloadCacheOnce.Do(func() {
		dir, err := os.MkdirTemp("", "reality-probe-cache-")
		if err != nil {
			t.Fatalf("create download cache dir: %v", err)
		}
		downloadCacheDir = dir
	})
	return downloadCacheDir
}

func TestMain(m *testing.M) {
	code := m.Run()
	if downloadCacheDir != "" {
		os.RemoveAll(downloadCacheDir)
	}
	os.Exit(code)
}

// pinnedVersion is the sing-box release selected by the v0.10 plan
// (§ "固定上游依赖"). It must be compared as an exact token, never a
// substring (spec §6).
const pinnedVersion = "1.14.1"

const downloadBase = "https://github.com/SagerNet/sing-box/releases/download/v" + pinnedVersion + "/"

type pinnedAsset struct {
	arch   string
	name   string
	sha256 string
}

// pinnedAssets are the two tarballs and hashes recorded in the plan,
// verified against the official release on 2026-09-25 (see
// docs/reality-node-validation.md for the verification command and
// output).
var pinnedAssets = []pinnedAsset{
	{arch: "amd64", name: "sing-box-" + pinnedVersion + "-linux-amd64.tar.gz", sha256: "12cb2816b52febb356f6a885b740cc8758c3f30b8ae0ca8edba80f0d2d35343f"},
	{arch: "arm64", name: "sing-box-" + pinnedVersion + "-linux-arm64.tar.gz", sha256: "6060b42fa84c5dcaeae1799af7f61b0f1ae4855d9d5ddc9e02baba17154b3ae2"},
}

// downloadTarball fetches a's tarball into the shared cache (see
// downloadCacheDir), verifying its SHA256 against the plan's pinned
// value every time it is served, including from cache, so a corrupted
// or tampered cache entry cannot silently pass. Transient 5xx responses
// from the release CDN are retried a few times with a short backoff.
func downloadTarball(t *testing.T, a pinnedAsset) string {
	t.Helper()
	downloadCacheMu.Lock()
	defer downloadCacheMu.Unlock()

	dst := filepath.Join(cacheDir(t), a.name)
	if data, err := os.ReadFile(dst); err == nil {
		if verifyTarballHash(t, a, data) {
			downloadCachePath[a.name] = dst
			return dst
		}
		t.Fatalf("cached %s failed SHA256 re-verification: cache corruption or tampering", a.name)
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		data, err := fetchTarball(t, a)
		if err != nil {
			lastErr = err
			continue
		}
		if !verifyTarballHash(t, a, data) {
			got := sha256.Sum256(data)
			t.Fatalf("SHA256 mismatch for %s: got %s, want %s (STOP: pinned release no longer matches plan; report before proceeding)", a.name, hex.EncodeToString(got[:]), a.sha256)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", dst, err)
		}
		downloadCachePath[a.name] = dst
		return dst
	}
	t.Skipf("blocked: could not download %s after retries: %v", a.name, lastErr)
	return ""
}

func verifyTarballHash(t *testing.T, a pinnedAsset, data []byte) bool {
	t.Helper()
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) == a.sha256
}

func fetchTarball(t *testing.T, a pinnedAsset) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadBase+a.name, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// extractSingBoxBinary implements spec §6 step 2: extract exactly the
// "sing-box-<v>-linux-<arch>/sing-box" member, reject anything that is
// not a regular file (in particular symlinks), and enforce the 200 MB
// cap. It ignores other archive members (this release also ships
// LICENSE and libcronet.so, confirmed not to conflict with the
// extraction rule: see docs/reality-node-validation.md).
func extractSingBoxBinary(t *testing.T, tarPath string, a pinnedAsset) string {
	t.Helper()
	wantMember := fmt.Sprintf("sing-box-%s-linux-%s/sing-box", pinnedVersion, a.arch)

	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatalf("open %s: %v", tarPath, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip %s: %v", tarPath, err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var found bool
	dst := filepath.Join(t.TempDir(), "sing-box-"+a.arch)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar %s: %v", tarPath, err)
		}
		if hdr.Name != wantMember {
			continue
		}
		found = true
		if hdr.Typeflag != tar.TypeReg {
			t.Fatalf("member %s is not a regular file (typeflag=%v): reject per spec §6", wantMember, hdr.Typeflag)
		}
		if hdr.Size > 200*1024*1024 {
			t.Fatalf("member %s exceeds 200 MB cap: %d bytes", wantMember, hdr.Size)
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			t.Fatalf("create %s: %v", dst, err)
		}
		n, err := io.Copy(out, tr)
		out.Close()
		if err != nil {
			t.Fatalf("extract %s: %v", wantMember, err)
		}
		if n != hdr.Size {
			t.Fatalf("extract %s: short write %d != %d", wantMember, n, hdr.Size)
		}
	}
	if !found {
		t.Fatalf("archive %s does not contain expected member %s", tarPath, wantMember)
	}
	return dst
}

// TestPinnedReleaseProbe verifies spec §6 / §"固定上游依赖" against the
// real upstream release: SHA256 of both tarballs, and single-member
// extraction with type/size enforcement for both architectures. Version
// token match and `sing-box check` are additionally run for the host
// architecture (amd64 in this environment); arm64 execution is not
// available here (see docs/reality-node-validation.md) and is not
// claimed.
func TestPinnedReleaseProbe(t *testing.T) {
	for _, a := range pinnedAssets {
		a := a
		t.Run(a.arch, func(t *testing.T) {
			tarPath := downloadTarball(t, a)
			bin := extractSingBoxBinary(t, tarPath, a)
			if a.arch != runtime.GOARCH {
				t.Logf("blocked: cannot execute %s binary on a %s host in this environment; hash and extraction were verified, version/check execution was not", a.arch, runtime.GOARCH)
				return
			}
			out, err := exec.Command(bin, "version").CombinedOutput()
			if err != nil {
				t.Fatalf("%s version: %v\n%s", a.arch, err, out)
			}
			firstLine := strings.SplitN(string(out), "\n", 2)[0]
			tokens := strings.Fields(firstLine)
			var matched bool
			for _, tok := range tokens {
				if tok == pinnedVersion {
					matched = true
					break
				}
			}
			if !matched {
				t.Fatalf("sing-box version output %q does not contain exact token %q", firstLine, pinnedVersion)
			}
		})
	}
	if runtime.GOARCH != "amd64" || runtime.GOOS != "linux" {
		t.Skip("blocked: requires a real linux/amd64 target to run sing-box check")
	}

	// Extracted directly in the parent test (not a subtest) so the
	// binary survives past this point: a subtest's t.TempDir() is
	// removed when that subtest returns.
	amd64Bin := extractSingBoxBinary(t, downloadTarball(t, pinnedAssets[0]), pinnedAssets[0])

	t.Run("check_spec_config", func(t *testing.T) {
		id := newTestIdentity(t)
		cfg := renderServerConfig(serverConfigParams{
			ListenPort:    28443,
			UUID:          id.uuid,
			PrivateKey:    id.privateKeyB64,
			ShortID:       id.shortIDHex,
			SNI:           "www.microsoft.com",
			SelfcheckPort: 39999,
			LocalCIDRs:    nil,
		})
		cfgPath := writeJSON(t, cfg)
		out, err := exec.Command(amd64Bin, "check", "-c", cfgPath).CombinedOutput()
		if err != nil {
			t.Fatalf("sing-box check rejected the spec §8.1 config: %v\n%s", err, out)
		}
	})
}

// systemdAnalyzeAvailable reports whether `systemd-analyze verify` can
// run in this environment. It requires the systemd-analyze binary but
// not a running systemd instance (verify is a static check).
func systemdAnalyzeAvailable(t *testing.T) bool {
	t.Helper()
	_, err := exec.LookPath("systemd-analyze")
	return err == nil
}

// TestRealityUnitProbe statically verifies the fixed unit template from
// spec §8.1 with `systemd-analyze verify`, which parses and validates
// unit syntax and directive names without a running systemd instance.
//
// This is NOT a substitute for the plan's required real-VM systemd
// gate (live enable/start/ActiveState on Debian 12 and Ubuntu 22.04/24.04,
// amd64 and arm64): this environment is not booted with systemd as PID 1
// ("System has not been booted with systemd as init system (PID 1).
// Can't operate."), so no live unit can be started here. See
// docs/reality-node-validation.md.
func TestRealityUnitProbe(t *testing.T) {
	if !systemdAnalyzeAvailable(t) {
		t.Skip("blocked: systemd-analyze not available in this environment")
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "probe", "reality.service"))
	if err != nil {
		t.Fatalf("read unit fixture: %v", err)
	}
	rendered := strings.ReplaceAll(string(raw), "<v>", pinnedVersion)

	dir := t.TempDir()
	unitPath := filepath.Join(dir, "proxyctl-reality.service")
	if err := os.WriteFile(unitPath, []byte(rendered), 0o644); err != nil {
		t.Fatalf("write rendered unit: %v", err)
	}

	// The one diagnostic this static check cannot avoid: verify resolves
	// ExecStart's binary against the real filesystem root, and the fixed
	// absolute path only exists after a real install. Confirmed
	// separately (docs/reality-node-validation.md) that an actual syntax
	// problem (e.g. an unknown directive) produces a distinct, additional
	// diagnostic line rather than replacing this one, so filtering only
	// this exact expected line still catches real regressions.
	wantBenign := fmt.Sprintf("Command /usr/local/lib/proxyctl-reality/versions/%s/sing-box is not executable: No such file or directory", pinnedVersion)

	out, err := exec.Command("systemd-analyze", "verify", unitPath).CombinedOutput()
	if err != nil {
		for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if strings.HasSuffix(line, wantBenign) {
				continue
			}
			t.Fatalf("systemd-analyze verify rejected the fixed unit template: %v\nunexpected diagnostic: %s\nfull output:\n%s", err, line, out)
		}
	}
}

type testIdentity struct {
	uuid          string
	privateKeyB64 string
	publicKeyB64  string
	shortIDHex    string
}

// newTestIdentity generates a fresh identity the same way spec §8.1 step 2
// describes: UUIDv4 via crypto/rand, an X25519 keypair via crypto/ecdh
// with the public key derived from the private key (never stored
// separately, matching spec §8.3's "公钥实时推导"), and an 8-byte
// short_id. This does not reuse production identity code because Task 1
// has no production package to depend on yet (internal/role/identity.go
// is Task 2); later Tasks are expected to consume the same encoding
// verified here.
func newTestIdentity(t *testing.T) testIdentity {
	t.Helper()
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("generate uuid: %v", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	uuidStr := fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])

	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate x25519 key: %v", err)
	}

	var sid [8]byte
	if _, err := rand.Read(sid[:]); err != nil {
		t.Fatalf("generate short_id: %v", err)
	}

	id := testIdentity{
		uuid:          uuidStr,
		privateKeyB64: base64.RawURLEncoding.EncodeToString(priv.Bytes()),
		publicKeyB64:  base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()),
		shortIDHex:    hex.EncodeToString(sid[:]),
	}
	if err := nodes.Validate(nodes.Node{
		Name: "probe", Type: nodes.TypeVlessReality, Server: "127.0.0.1", Port: 1, SNI: "example.com",
		UUID: id.uuid, PublicKey: id.publicKeyB64, ShortID: id.shortIDHex,
	}); err != nil {
		t.Fatalf("generated identity fails nodes.Validate: %v", err)
	}
	return id
}

type serverConfigParams struct {
	ListenPort    int
	UUID          string
	PrivateKey    string
	ShortID       string
	SNI           string
	SelfcheckPort int
	// LocalCIDRs is the "<本机地址列表>" rejection entry from spec §8.1;
	// nil omits that rule line (used where no additional local address is
	// under test).
	LocalCIDRs []string
}

// renderServerConfig builds the exact JSON structure from spec §8.1,
// field for field, including the fixed private-network rejection list
// (§8.1 "私网拒绝规则"). It is deliberately not shared with any future
// production package: Task 1 verifies the structure sing-box actually
// accepts, and later Tasks that implement config.go are reviewed against
// this record, not against a shared helper.
func renderServerConfig(p serverConfigParams) map[string]interface{} {
	rejectCIDRs := []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
		"169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16",
		"224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "::ffff:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48",
		"fc00::/7", "fe80::/10", "ff00::/8",
	}
	rules := []interface{}{
		map[string]interface{}{
			"ip_cidr":  []string{"127.0.0.1/32"},
			"port":     []int{p.SelfcheckPort},
			"outbound": "direct",
		},
		map[string]interface{}{
			"ip_cidr": rejectCIDRs,
			"action":  "reject",
		},
	}
	if len(p.LocalCIDRs) > 0 {
		rules = append(rules, map[string]interface{}{
			"ip_cidr": p.LocalCIDRs,
			"action":  "reject",
		})
	}
	rules = append(rules, map[string]interface{}{
		"ip_is_private": true,
		"action":        "reject",
	})

	return map[string]interface{}{
		"log": map[string]interface{}{"level": "warn", "timestamp": true},
		"inbounds": []interface{}{
			map[string]interface{}{
				"type": "vless", "tag": "reality-in",
				"listen": "0.0.0.0", "listen_port": p.ListenPort,
				"users": []interface{}{
					map[string]interface{}{"uuid": p.UUID, "flow": nodes.RealityFlow},
				},
				"tls": map[string]interface{}{
					"enabled":     true,
					"server_name": p.SNI,
					"reality": map[string]interface{}{
						"enabled":     true,
						"handshake":   map[string]interface{}{"server": p.SNI, "server_port": 443},
						"private_key": p.PrivateKey,
						"short_id":    []string{p.ShortID},
					},
				},
			},
		},
		"outbounds": []interface{}{
			map[string]interface{}{"type": "direct", "tag": "direct"},
		},
		"route": map[string]interface{}{
			"rules": rules,
			"final": "direct",
		},
	}
}

func writeJSON(t *testing.T, v interface{}) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("get free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// runSingBox starts the given sing-box binary against cfgPath and
// returns a cleanup func that terminates it and waits for exit, matching
// the "kill then Wait" requirement the plan calls out for exec lifecycle
// (plan review-focus table, "exec 超时后子进程仍存活...").
func runSingBox(t *testing.T, ctx context.Context, bin, cfgPath, logLabel string) func() {
	t.Helper()
	cmd := exec.CommandContext(ctx, bin, "run", "-c", cfgPath)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s sing-box: %v", logLabel, err)
	}
	return func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("%s sing-box output:\n%s", logLabel, out.String())
		}
	}
}

func waitPortOpen(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp4", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s did not open within %s", addr, timeout)
}

// realityPair starts a real pinned-binary server and client pair wired
// per spec §8.1 (server) and §9.3 (client, via remote.GenerateConfig).
// It returns the client's local mixed-proxy URL and a cleanup func.
func realityPair(t *testing.T, ctx context.Context, bin string, listenPort, selfcheckPort int, localCIDRs []string) (*url.URL, func()) {
	t.Helper()
	id := newTestIdentity(t)

	serverCfg := renderServerConfig(serverConfigParams{
		ListenPort: listenPort, UUID: id.uuid, PrivateKey: id.privateKeyB64,
		ShortID: id.shortIDHex, SNI: "www.microsoft.com", SelfcheckPort: selfcheckPort,
		LocalCIDRs: localCIDRs,
	})
	serverCfgPath := writeJSON(t, serverCfg)
	stopServer := runSingBox(t, ctx, bin, serverCfgPath, "server")
	waitPortOpen(t, fmt.Sprintf("127.0.0.1:%d", listenPort), 10*time.Second)

	clientPort := freePort(t)
	n := nodes.Node{
		Name: "probe", Type: nodes.TypeVlessReality,
		Server: "127.0.0.1", Port: listenPort, SNI: "www.microsoft.com", Enabled: true,
		UUID: id.uuid, PublicKey: id.publicKeyB64, ShortID: id.shortIDHex,
	}
	clientData, err := remote.GenerateConfig(n, clientPort)
	if err != nil {
		stopServer()
		t.Fatalf("remote.GenerateConfig: %v", err)
	}
	clientCfgPath := filepath.Join(t.TempDir(), "client.json")
	if err := os.WriteFile(clientCfgPath, clientData, 0o600); err != nil {
		stopServer()
		t.Fatalf("write client config: %v", err)
	}
	stopClient := runSingBox(t, ctx, bin, clientCfgPath, "client")
	waitPortOpen(t, fmt.Sprintf("127.0.0.1:%d", clientPort), 10*time.Second)

	proxyURL := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", clientPort)}
	return proxyURL, func() {
		stopClient()
		stopServer()
	}
}

func proxiedGet(t *testing.T, ctx context.Context, proxyURL *url.URL, target string) (int, []byte, error) {
	t.Helper()
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
		Timeout:   10 * time.Second,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// nonceServer implements spec §9.3's E3: a listener that returns a
// random nonce for any path, held for the duration of the test.
func nonceServer(t *testing.T, port int) (nonce string, close func()) {
	t.Helper()
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("generate nonce: %v", err)
	}
	nonce = hex.EncodeToString(raw[:])
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(nonce))
	})
	l, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("bind selfcheck port %d: %v", port, err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	return nonce, func() {
		_ = srv.Close()
	}
}

// TestRealityDataPathProbe is the real E1->E2->E3 round trip required by
// spec §9.3: a genuine pinned-binary server (E1) and client (E2, wired
// through internal/health/remote.GenerateConfig exactly as the production
// self-check will use it) carry an HTTP request over a real Reality
// handshake to a local nonce responder (E3), and the response body must
// equal the nonce.
//
// This exercises the real protocol end to end but not the systemd unit
// wrapper (blocked in this environment, see TestRealityUnitProbe) and
// runs on amd64 only (see TestPinnedReleaseProbe).
func TestRealityDataPathProbe(t *testing.T) {
	if runtime.GOARCH != "amd64" || runtime.GOOS != "linux" {
		t.Skip("blocked: requires a real linux/amd64 target to execute the pinned binary")
	}
	tarPath := downloadTarball(t, pinnedAssets[0])
	bin := extractSingBoxBinary(t, tarPath, pinnedAssets[0])

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	listenPort := freePort(t)
	selfcheckPort := freePort(t)
	nonce, closeE3 := nonceServer(t, selfcheckPort)
	defer closeE3()

	proxyURL, cleanup := realityPair(t, ctx, bin, listenPort, selfcheckPort, nil)
	defer cleanup()

	status, body, err := proxiedGet(t, ctx, proxyURL, fmt.Sprintf("http://127.0.0.1:%d/probe-path", selfcheckPort))
	if err != nil {
		t.Fatalf("proxied request over real Reality tunnel failed: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if string(body) != nonce {
		t.Fatalf("nonce mismatch: got %q, want %q", body, nonce)
	}
}

// TestRealityRoutingProbe verifies spec §8.1's private-network rejection
// rule set. Two sub-cases are proven dynamically, with a real reachable
// control target for each (per the plan's requirement not to substitute
// "unreachable" for "rejected"):
//
//   - a second local HTTP server on 127.0.0.1 at a port other than
//     selfcheck_port: directly reachable (asserted before the tunnel is
//     even involved), but rejected when requested through the tunnel;
//   - a local HTTP server bound to 0.0.0.0: same pattern.
//
// The RFC1918 / 100.64.0.0/10 / 169.254.0.0/16 / IPv6 ULA / link-local /
// AWS IMDS members of the reject list are NOT dynamically exercised
// here: this container's own network stack answers TCP connects to
// addresses it does not own (confirmed independently: raw connect() to
// 169.254.169.254, 10.0.0.1, 192.168.1.1, and 100.64.0.1 all "succeed"
// in well under 1ms, yet binding those addresses locally fails with
// EADDRNOTADDR, proving they are not real, controllable endpoints of
// this host). Using such an address as a "reachable control target"
// would not distinguish a real rule rejection from an environment
// artifact, which is exactly the failure mode the plan warns against.
// Instead, those entries are asserted present in the rendered rule set
// and the whole config is confirmed accepted by `sing-box check` with
// the real pinned binary. See docs/reality-node-validation.md.
func TestRealityRoutingProbe(t *testing.T) {
	if runtime.GOARCH != "amd64" || runtime.GOOS != "linux" {
		t.Skip("blocked: requires a real linux/amd64 target to execute the pinned binary")
	}
	tarPath := downloadTarball(t, pinnedAssets[0])
	bin := extractSingBoxBinary(t, tarPath, pinnedAssets[0])

	t.Run("static_reject_list_and_check", func(t *testing.T) {
		id := newTestIdentity(t)
		cfg := renderServerConfig(serverConfigParams{
			ListenPort: freePort(t), UUID: id.uuid, PrivateKey: id.privateKeyB64,
			ShortID: id.shortIDHex, SNI: "www.microsoft.com", SelfcheckPort: freePort(t),
			LocalCIDRs: []string{"192.0.2.2/32"},
		})
		route := cfg["route"].(map[string]interface{})
		rules := route["rules"].([]interface{})
		rejectRule := rules[1].(map[string]interface{})
		cidrs := rejectRule["ip_cidr"].([]string)
		required := []string{
			"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", // RFC1918
			"100.64.0.0/10",  // CGNAT (also covers the shared-address-space test range)
			"169.254.0.0/16", // link-local (covers 169.254.169.254 IMDS)
			"fc00::/7",       // IPv6 ULA
			"fe80::/10",      // IPv6 link-local
		}
		for _, want := range required {
			var found bool
			for _, got := range cidrs {
				if got == want {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("reject rule missing required CIDR %s", want)
			}
		}
		cfgPath := writeJSON(t, cfg)
		out, err := exec.Command(bin, "check", "-c", cfgPath).CombinedOutput()
		if err != nil {
			t.Fatalf("sing-box check rejected the production route rule set: %v\n%s", err, out)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	listenPort := freePort(t)
	selfcheckPort := freePort(t)
	otherPort := freePort(t)
	zeroBindPort := freePort(t)

	nonce, closeE3 := nonceServer(t, selfcheckPort)
	defer closeE3()

	otherNonce, closeOther := nonceServer(t, otherPort)
	defer closeOther()

	// A listener bound to 0.0.0.0 at zeroBindPort, matching the "0.0.0.0/8"
	// reject-list entry's real-world purpose (a service bound to all
	// interfaces on the node host).
	zeroNonce := func() string {
		var raw [32]byte
		if _, err := rand.Read(raw[:]); err != nil {
			t.Fatalf("generate nonce: %v", err)
		}
		return hex.EncodeToString(raw[:])
	}()
	zeroMux := http.NewServeMux()
	zeroMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(zeroNonce)) })
	zeroListener, err := net.Listen("tcp4", "0.0.0.0:"+strconv.Itoa(zeroBindPort))
	if err != nil {
		t.Fatalf("bind 0.0.0.0:%d: %v", zeroBindPort, err)
	}
	zeroSrv := &http.Server{Handler: zeroMux}
	go zeroSrv.Serve(zeroListener)
	defer zeroSrv.Close()

	// Control: both targets are genuinely reachable outside the tunnel
	// before we ever involve sing-box.
	directClient := &http.Client{Timeout: 5 * time.Second}
	if resp, err := directClient.Get(fmt.Sprintf("http://127.0.0.1:%d/", otherPort)); err != nil {
		t.Fatalf("control: direct request to loopback other-port target failed: %v", err)
	} else {
		resp.Body.Close()
	}
	if resp, err := directClient.Get(fmt.Sprintf("http://0.0.0.0:%d/", zeroBindPort)); err != nil {
		t.Fatalf("control: direct request to 0.0.0.0 target failed: %v", err)
	} else {
		resp.Body.Close()
	}

	proxyURL, cleanup := realityPair(t, ctx, bin, listenPort, selfcheckPort, nil)
	defer cleanup()

	t.Run("selfcheck_port_allowed", func(t *testing.T) {
		status, body, err := proxiedGet(t, ctx, proxyURL, fmt.Sprintf("http://127.0.0.1:%d/", selfcheckPort))
		if err != nil {
			t.Fatalf("allowed selfcheck target was rejected: %v", err)
		}
		if status != http.StatusOK || string(body) != nonce {
			t.Fatalf("allowed selfcheck target returned unexpected result: status=%d body=%q", status, body)
		}
	})

	t.Run("loopback_other_port_rejected", func(t *testing.T) {
		status, body, err := proxiedGet(t, ctx, proxyURL, fmt.Sprintf("http://127.0.0.1:%d/", otherPort))
		if err == nil && status == http.StatusOK && string(body) == otherNonce {
			t.Fatalf("private-network reject rule did not block 127.0.0.1:%d (other port): got the target's real content through the tunnel", otherPort)
		}
	})

	t.Run("zero_bind_rejected", func(t *testing.T) {
		status, body, err := proxiedGet(t, ctx, proxyURL, fmt.Sprintf("http://0.0.0.0:%d/", zeroBindPort))
		if err == nil && status == http.StatusOK && string(body) == zeroNonce {
			t.Fatalf("private-network reject rule did not block 0.0.0.0:%d: got the target's real content through the tunnel", zeroBindPort)
		}
	})
}
