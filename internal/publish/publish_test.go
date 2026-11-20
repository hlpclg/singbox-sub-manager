package publish

// Tests for publish.go (design spec §10, plan Task 9). Like the other
// transaction-backed packages these tests never use t.Parallel: publishHook
// is a process-global test seam.

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
	"github.com/hlpclg/singbox-sub-manager/internal/render"
	"github.com/hlpclg/singbox-sub-manager/internal/subscriptionvalidate"
	"github.com/hlpclg/singbox-sub-manager/internal/transaction"
)

// --- fixture ---

const testToken = "tok0123456789abcdefABCDEF_-xyz"
const testDomain = "sub.example.test"

func hy2Node(name string) nodes.Node {
	return nodes.Node{Name: name, Type: nodes.TypeHysteria2, Server: "203.0.113.5", Port: 443,
		Password: "pw-" + name, ObfsPassword: "ob-" + name, SNI: "www.example.com", Enabled: true}
}

func realityNode(name string) nodes.Node {
	return nodes.Node{Name: name, Type: nodes.TypeVlessReality, Server: "203.0.113.9", Port: 443,
		UUID: "11111111-2222-4333-8444-555555555555", PublicKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8",
		ShortID: "ab12", SNI: "www.microsoft.com", Enabled: true}
}

func writeFileMode(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func mkdirMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

type fakeFetcher struct {
	mu      sync.Mutex
	dir     string
	calls   []string
	status  map[string]int
	errs    map[string]error
	body    map[string][]byte
	block   bool
	domains []string
}

func (f *fakeFetcher) Fetch(ctx context.Context, domain, urlPath string) (FetchResult, error) {
	f.mu.Lock()
	name := strings.TrimPrefix(urlPath, "/"+testToken+"/")
	f.calls = append(f.calls, name)
	f.domains = append(f.domains, domain)
	block, status, ferr, body := f.block, f.status[name], f.errs[name], f.body[name]
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return FetchResult{}, ctx.Err()
	}
	if ferr != nil {
		return FetchResult{}, ferr
	}
	if body == nil {
		var err error
		body, err = os.ReadFile(filepath.Join(f.dir, name))
		if err != nil {
			return FetchResult{Status: 404}, nil
		}
	}
	if status == 0 {
		status = 200
	}
	sum := sha256.Sum256(body)
	return FetchResult{Status: status, SHA256: hex.EncodeToString(sum[:])}, nil
}

func (f *fakeFetcher) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

type fixture struct {
	t         *testing.T
	base      string
	env       *Env
	fetch     *fakeFetcher
	etcRoot   string // Dir(Dir(nodes.conf))
	nodesPath string
	installJS string
	wwwRoot   string // dirname(subscription_root)
	subRoot   string
	tokenDir  string
	stateDir  string
	caddyUID  uint32
	caddyGID  uint32
	pre       map[string]entryState // set by crashAt: the tree before the crash child ran
}

func (f *fixture) clashPath() string { return filepath.Join(f.tokenDir, "clash.yaml") }
func (f *fixture) srPath() string    { return filepath.Join(f.tokenDir, "sr.txt") }

// newFixture builds a standard subscription center under a temp dir: nodes.conf
// (two enabled hysteria2 nodes) with the matching, already-published clash.yaml
// and sr.txt, install.json, the token file and the state directory.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureAt(t, t.TempDir(), true)
}

// newFixtureAt lays out the fixture's paths and Env under base. With populate
// it also writes the initial host state; without it (a crash child re-opening
// the parent's directory) it only builds the handles.
func newFixtureAt(t *testing.T, base string, populate bool) *fixture {
	t.Helper()
	f := &fixture{t: t, base: base}
	f.etcRoot = filepath.Join(base, "etc")
	etcDir := filepath.Join(f.etcRoot, "singbox-sub-manager")
	f.nodesPath = filepath.Join(etcDir, "nodes.conf")
	f.installJS = filepath.Join(etcDir, "install.json")
	f.stateDir = filepath.Join(base, "var-lib", "singbox-sub-manager")
	f.wwwRoot = filepath.Join(base, "www")
	f.subRoot = filepath.Join(f.wwwRoot, "proxy-sub")
	f.tokenDir = filepath.Join(f.subRoot, testToken)
	if populate {
		for _, d := range []string{f.etcRoot, etcDir, filepath.Dir(f.stateDir), f.stateDir, f.wwwRoot, f.subRoot, f.tokenDir} {
			mkdirMode(t, d, 0755)
		}
		tokenFile := filepath.Join(f.stateDir, "token")
		writeFileMode(t, tokenFile, []byte(testToken+"\n"), 0600)
		js, _ := json.Marshal(map[string]string{"domain": testDomain, "subscription_root": f.subRoot, "token_file": tokenFile})
		writeFileMode(t, f.installJS, js, 0600)

		ns := []nodes.Node{hy2Node("A"), hy2Node("B")}
		writeFileMode(t, f.nodesPath, []byte(nodes.Serialize(ns)), 0600)
		f.publishOld(ns)
	}

	f.caddyUID, f.caddyGID = uint32(os.Getuid()), uint32(os.Getgid())
	if os.Geteuid() == 0 {
		f.caddyUID, f.caddyGID = 4242, 4343
	}
	f.fetch = &fakeFetcher{dir: f.tokenDir, status: map[string]int{}, errs: map[string]error{}, body: map[string][]byte{}}
	f.env = &Env{
		InstallJSON: f.installJS, NodesConf: f.nodesPath, StateDir: f.stateDir,
		TrustedOwnerUID: os.Getuid(), TrustedOwnerGID: os.Getgid(),
		LookupFileOwner: func() (uint32, uint32, error) { return f.caddyUID, f.caddyGID, nil },
		Fetcher:         f.fetch,
		ProxyctlVersion: "v0.10.0",
	}
	t.Cleanup(func() { publishHook = func(string) error { return nil } })
	return f
}

// publishOld writes the "previous publication" for ns (clash.yaml + sr.txt,
// 0644, owned by the caddy stand-in or, as root, 4242:4343).
func (f *fixture) publishOld(ns []nodes.Node) {
	f.t.Helper()
	en := nodes.Enabled(ns)
	writeFileMode(f.t, f.clashPath(), []byte(render.Clash(en)), 0644)
	writeFileMode(f.t, f.srPath(), []byte(render.Shadowrocket(en)), 0644)
	if os.Geteuid() == 0 {
		for _, p := range []string{f.clashPath(), f.srPath()} {
			if err := os.Chown(p, 4242, 4343); err != nil {
				f.t.Fatal(err)
			}
		}
	}
}

func (f *fixture) setNodes(ns ...nodes.Node) {
	f.t.Helper()
	writeFileMode(f.t, f.nodesPath, []byte(nodes.Serialize(ns)), 0600)
}

func (f *fixture) txnDir() string { return filepath.Join(f.stateDir, TxnDirName) }

func (f *fixture) assertNoResidue() {
	f.t.Helper()
	if _, err := os.Lstat(f.txnDir()); !os.IsNotExist(err) {
		f.t.Errorf("transaction area still exists (err=%v)", err)
	}
	for _, dir := range []string{f.stateDir, f.wwwRoot, f.tokenDir, filepath.Dir(f.nodesPath)} {
		ents, err := os.ReadDir(dir)
		if err != nil {
			f.t.Fatal(err)
		}
		for _, e := range ents {
			n := e.Name()
			if strings.HasPrefix(n, ".proxyctl-") || strings.HasPrefix(n, ".publish.tomb-") || strings.HasSuffix(n, ".bak") {
				f.t.Errorf("residue %s in %s", n, dir)
			}
		}
	}
}

// --- tree snapshots ---

type entryState struct {
	Kind  string
	Mode  os.FileMode
	UID   uint32
	GID   uint32
	SHA   string
	Ino   uint64
	MtNs  int64
	Link  string
	Count int
}

func treeState(t *testing.T, base string, identity bool) map[string]entryState {
	t.Helper()
	out := map[string]entryState{}
	err := filepath.Walk(base, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(base, p)
		st := info.Sys().(*syscall.Stat_t)
		e := entryState{Mode: info.Mode().Perm(), UID: st.Uid, GID: st.Gid}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			e.Kind = "link"
			e.Link, _ = os.Readlink(p)
		case info.IsDir():
			e.Kind = "dir"
		default:
			e.Kind = "file"
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			e.SHA = hex.EncodeToString(sum[:])
		}
		if identity {
			e.Ino = st.Ino
			e.MtNs = info.ModTime().UnixNano()
		}
		out[rel] = e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func diffTrees(a, b map[string]entryState) string {
	var msgs []string
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	var sorted []string
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		x, okA := a[k]
		y, okB := b[k]
		switch {
		case !okA:
			msgs = append(msgs, "added: "+k)
		case !okB:
			msgs = append(msgs, "removed: "+k)
		case x != y:
			msgs = append(msgs, fmt.Sprintf("changed: %s\n  before %+v\n  after  %+v", k, x, y))
		}
	}
	return strings.Join(msgs, "\n")
}

func (f *fixture) assertTreeUnchanged(before map[string]entryState, identity bool) {
	f.t.Helper()
	if d := diffTrees(before, treeState(f.t, f.base, identity)); d != "" {
		f.t.Errorf("tree changed:\n%s", d)
	}
}

func (f *fixture) assertPublished(enabled []nodes.Node) {
	f.t.Helper()
	wantClash := render.Clash(enabled)
	wantSR := render.Shadowrocket(enabled)
	gotClash, err := os.ReadFile(f.clashPath())
	if err != nil {
		f.t.Fatal(err)
	}
	gotSR, err := os.ReadFile(f.srPath())
	if err != nil {
		f.t.Fatal(err)
	}
	if string(gotClash) != wantClash {
		f.t.Errorf("clash.yaml is not the rendering of %d enabled node(s)", len(enabled))
	}
	if string(gotSR) != wantSR {
		f.t.Errorf("sr.txt is not the rendering of %d enabled node(s)", len(enabled))
	}
}

func (f *fixture) loadNodes() []nodes.Node {
	f.t.Helper()
	ns, _, err := nodes.Load(f.nodesPath)
	if err != nil {
		f.t.Fatal(err)
	}
	return ns
}

func ctxBG() context.Context { return context.Background() }

func setHook(h func(point string) error) { publishHook = h }

func errAt(point string, err error) func(string) error {
	return func(p string) error {
		if p == point {
			return err
		}
		return nil
	}
}

// --- version rule (spec §3, §10.3 step 1) ---

func TestVersionRule(t *testing.T) {
	cases := []struct {
		have, min string
		ok        bool
	}{
		{"v0.10.0", "v0.10.0", true},
		{"v0.10.1", "v0.10.0", true},
		{"v0.10.10", "v0.10.9", true}, // numeric, not lexical, segments
		{"v0.10.9", "v0.10.10", false},
		{"v0.9.9", "v0.10.0", false},
		{"v1.0.0", "v0.10.0", true},
		{"v0.10.0", "v0.10.1", false},
		{"dev", "v0.10.0", false},
		{"", "v0.10.0", false},
		{"v0.10", "v0.10.0", false},
		{"0.10.0", "v0.10.0", false},
		{"v0.10.0-rc1", "v0.10.0", false},
		{"v0.10.x", "v0.10.0", false},
		{"v99999999999999999999.0.0", "v0.10.0", false},
	}
	for _, tc := range cases {
		err := CheckMinVersion(tc.have, tc.min)
		if (err == nil) != tc.ok {
			t.Errorf("CheckMinVersion(%q, %q) = %v, want ok=%v", tc.have, tc.min, err, tc.ok)
		}
	}
	for _, bad := range []string{"", "dev", "v1", "v1.2", "1.2.3", "v1.2.3.4", "v1.2.3-x", "V1.2.3"} {
		if ValidMinVersion(bad) == nil {
			t.Errorf("ValidMinVersion(%q) accepted a malformed requirement", bad)
		}
	}
	if err := ValidMinVersion("v0.10.0"); err != nil {
		t.Errorf("ValidMinVersion(v0.10.0) = %v", err)
	}
}

// --- TestPublishMutationMatrix (spec §10.3 steps 1-3, §14.1 "发布幂等与拒绝") ---

func TestPublishMutationMatrix(t *testing.T) {
	A, B := hy2Node("A"), hy2Node("B")
	disabledA := A
	disabledA.Enabled = false
	changedA := A
	changedA.Port = 8443
	newX := hy2Node("X")
	str := func(s string) *string { return &s }
	port := func(p int) *int { return &p }

	type row struct {
		name       string
		initial    []nodes.Node // nil = fixture default (A, B enabled)
		prep       func(f *fixture)
		m          Mutation
		minVersion string
		version    string // proxyctl Version override
		wantExit   int
		wantErr    string
		nodesTouch bool // nodes.conf is rewritten (new inode)
		wantNodes  []string
		wantEn     []string // enabled names rendered into the subscription
	}
	rows := []row{
		{name: "add new node", m: Mutation{Kind: KindAdd, Node: newX}, wantNodes: []string{"A", "B", "X"}, wantEn: []string{"A", "B", "X"}, nodesTouch: true},
		{name: "add identical enabled node is an idempotent republish", m: Mutation{Kind: KindAdd, Node: A}, wantNodes: []string{"A", "B"}, wantEn: []string{"A", "B"}},
		{name: "add same-name same-fields but disabled is rejected (use enable)", initial: []nodes.Node{disabledA, B}, m: Mutation{Kind: KindAdd, Node: A}, wantExit: 1, wantErr: "enable"},
		{name: "add same name with different fields is rejected (use edit)", m: Mutation{Kind: KindAdd, Node: changedA}, wantExit: 1, wantErr: "edit"},
		{name: "enable already enabled is an idempotent republish", m: Mutation{Kind: KindEnable, Name: "A"}, wantNodes: []string{"A", "B"}, wantEn: []string{"A", "B"}},
		{name: "enable disabled node", initial: []nodes.Node{disabledA, B}, m: Mutation{Kind: KindEnable, Name: "A"}, wantNodes: []string{"A", "B"}, wantEn: []string{"A", "B"}, nodesTouch: true},
		{name: "disable already disabled is an idempotent republish", initial: []nodes.Node{disabledA, B}, m: Mutation{Kind: KindDisable, Name: "A"}, wantNodes: []string{"A", "B"}, wantEn: []string{"B"}},
		{name: "disable one of two", m: Mutation{Kind: KindDisable, Name: "A"}, wantNodes: []string{"A", "B"}, wantEn: []string{"B"}, nodesTouch: true},
		{name: "remove missing node is an idempotent republish", m: Mutation{Kind: KindRemove, Name: "ghost"}, wantNodes: []string{"A", "B"}, wantEn: []string{"A", "B"}},
		{name: "remove one of two", m: Mutation{Kind: KindRemove, Name: "A"}, wantNodes: []string{"B"}, wantEn: []string{"B"}, nodesTouch: true},
		{name: "edit missing node", m: Mutation{Kind: KindEdit, Name: "ghost", Edit: Edit{Port: port(1)}}, wantExit: 1, wantErr: "not found"},
		{name: "enable missing node", m: Mutation{Kind: KindEnable, Name: "ghost"}, wantExit: 1, wantErr: "not found"},
		{name: "disable missing node", m: Mutation{Kind: KindDisable, Name: "ghost"}, wantExit: 1, wantErr: "not found"},
		{name: "disable the last enabled node", initial: []nodes.Node{A, func() nodes.Node { b := B; b.Enabled = false; return b }()}, m: Mutation{Kind: KindDisable, Name: "A"}, wantExit: 1, wantErr: "no enabled"},
		{name: "remove the last enabled node", initial: []nodes.Node{A, func() nodes.Node { b := B; b.Enabled = false; return b }()}, m: Mutation{Kind: KindRemove, Name: "A"}, wantExit: 1, wantErr: "no enabled"},
		{name: "publish with an empty enabled set", initial: []nodes.Node{disabledA}, m: Mutation{Kind: KindPublish}, wantExit: 1, wantErr: "no enabled"},
		{name: "publish only republishes", m: Mutation{Kind: KindPublish}, wantNodes: []string{"A", "B"}, wantEn: []string{"A", "B"}},
		{name: "edit changes a field", m: Mutation{Kind: KindEdit, Name: "A", Edit: Edit{Port: port(8443)}}, wantNodes: []string{"A", "B"}, wantEn: []string{"A", "B"}, nodesTouch: true},
		{name: "edit that changes nothing leaves nodes.conf alone", m: Mutation{Kind: KindEdit, Name: "A", Edit: Edit{Port: port(443)}}, wantNodes: []string{"A", "B"}, wantEn: []string{"A", "B"}},
		{name: "edit rename onto an existing name", m: Mutation{Kind: KindEdit, Name: "A", Edit: Edit{Name: str("B")}}, wantExit: 1, wantErr: "already exists"},
		{name: "edit a hysteria2 node with a reality field", m: Mutation{Kind: KindEdit, Name: "A", Edit: Edit{UUID: str("11111111-2222-4333-8444-555555555555")}}, wantExit: 1, wantErr: "not allowed"},
		{name: "edit to an invalid value", m: Mutation{Kind: KindEdit, Name: "A", Edit: Edit{Port: port(70000)}}, wantExit: 1, wantErr: "invalid port"},
		{name: "reality node round trip", initial: []nodes.Node{A, realityNode("R")}, m: Mutation{Kind: KindPublish}, wantNodes: []string{"A", "R"}, wantEn: []string{"A", "R"}},

		// --- preconditions (spec §10.3 step 1) ---
		{name: "legacy nodes.conf", prep: func(f *fixture) {
			writeFileMode(f.t, f.nodesPath, []byte("A|203.0.113.5|443|pw|ob|www.example.com\n"), 0600)
		}, m: Mutation{Kind: KindPublish}, wantExit: 1, wantErr: "migrate"},
		{name: "empty nodes.conf", prep: func(f *fixture) { writeFileMode(f.t, f.nodesPath, []byte("# nothing\n"), 0600) }, m: Mutation{Kind: KindPublish}, wantExit: 1, wantErr: "no nodes"},
		{name: "missing token directory", prep: func(f *fixture) {
			os.Remove(f.clashPath())
			os.Remove(f.srPath())
			os.Remove(f.tokenDir)
		}, m: Mutation{Kind: KindPublish}, wantExit: 1, wantErr: "installer"},
		{name: "missing subscription root", prep: func(f *fixture) {
			os.Remove(f.clashPath())
			os.Remove(f.srPath())
			os.Remove(f.tokenDir)
			os.Remove(f.subRoot)
		}, m: Mutation{Kind: KindPublish}, wantExit: 1, wantErr: "installer"},
		{name: "missing install.json", prep: func(f *fixture) { os.Remove(f.installJS) }, m: Mutation{Kind: KindPublish}, wantExit: 1, wantErr: "install.json"},
		{name: "invalid token file", prep: func(f *fixture) {
			writeFileMode(f.t, filepath.Join(f.stateDir, "token"), []byte("short"), 0600)
		}, m: Mutation{Kind: KindPublish}, wantExit: 1, wantErr: "token"},
		{name: "min-version satisfied", minVersion: "v0.10.0", m: Mutation{Kind: KindPublish}, wantNodes: []string{"A", "B"}, wantEn: []string{"A", "B"}},
		{name: "v0.10.10 satisfies v0.10.9", version: "v0.10.10", minVersion: "v0.10.9", m: Mutation{Kind: KindPublish}, wantNodes: []string{"A", "B"}, wantEn: []string{"A", "B"}},
		{name: "v0.10.9 does not satisfy v0.10.10", version: "v0.10.9", minVersion: "v0.10.10", m: Mutation{Kind: KindPublish}, wantExit: 1, wantErr: "v0.10.10"},
		{name: "older version", version: "v0.9.9", minVersion: "v0.10.0", m: Mutation{Kind: KindPublish}, wantExit: 1, wantErr: "v0.10.0"},
		{name: "dev build cannot prove compatibility", version: "dev", minVersion: "v0.10.0", m: Mutation{Kind: KindPublish}, wantExit: 1, wantErr: "v0.10.0"},
		{name: "malformed build version", version: "v0.10", minVersion: "v0.10.0", m: Mutation{Kind: KindPublish}, wantExit: 1, wantErr: "v0.10.0"},
		{name: "dev build without --min-version is allowed", version: "dev", m: Mutation{Kind: KindPublish}, wantNodes: []string{"A", "B"}, wantEn: []string{"A", "B"}},
		{name: "leftover transaction area", prep: func(f *fixture) { mkdirMode(f.t, f.txnDir(), 0700) }, m: Mutation{Kind: KindPublish}, wantExit: 1, wantErr: "--recover"},
		{name: "leftover tombstone", prep: func(f *fixture) { mkdirMode(f.t, filepath.Join(f.stateDir, TombstonePrefix+"abc"), 0700) }, m: Mutation{Kind: KindPublish}, wantExit: 1, wantErr: "--recover"},
		{name: "nodes.conf is a symlink", prep: func(f *fixture) {
			real := filepath.Join(f.base, "real-nodes.conf")
			b, _ := os.ReadFile(f.nodesPath)
			writeFileMode(f.t, real, b, 0600)
			os.Remove(f.nodesPath)
			os.Symlink(real, f.nodesPath)
		}, m: Mutation{Kind: KindPublish}, wantExit: 1, wantErr: "symbolic link"},
		{name: "clash.yaml is a symlink", prep: func(f *fixture) {
			outside := filepath.Join(f.base, "outside.txt")
			writeFileMode(f.t, outside, []byte("secret"), 0644)
			os.Remove(f.clashPath())
			os.Symlink(outside, f.clashPath())
		}, m: Mutation{Kind: KindPublish}, wantExit: 1, wantErr: "symbolic link"},
		{name: "token directory is a symlink", prep: func(f *fixture) {
			other := filepath.Join(f.base, "elsewhere")
			mkdirMode(f.t, other, 0755)
			os.Remove(f.clashPath())
			os.Remove(f.srPath())
			os.Remove(f.tokenDir)
			os.Symlink(other, f.tokenDir)
		}, m: Mutation{Kind: KindPublish}, wantExit: 1, wantErr: "symbolic link"},
		{name: "subscription root is a symlink", prep: func(f *fixture) {
			other := filepath.Join(f.base, "elsewhere-root")
			mkdirMode(f.t, filepath.Join(other, testToken), 0755)
			os.Remove(f.clashPath())
			os.Remove(f.srPath())
			os.Remove(f.tokenDir)
			os.Remove(f.subRoot)
			os.Symlink(other, f.subRoot)
		}, m: Mutation{Kind: KindPublish}, wantExit: 1, wantErr: "symbolic link"},
		{name: "web root parent writable by group/other", prep: func(f *fixture) { os.Chmod(f.wwwRoot, 0775) }, m: Mutation{Kind: KindPublish}, wantExit: 1, wantErr: "writable"},
	}

	for _, r := range rows {
		r := r
		t.Run(r.name, func(t *testing.T) {
			f := newFixture(t)
			if r.initial != nil {
				f.setNodes(r.initial...)
				f.publishOld(nodes.Enabled(r.initial))
			}
			if r.prep != nil {
				r.prep(f)
			}
			if r.version != "" {
				f.env.ProxyctlVersion = r.version
			}
			before := treeState(t, f.base, true)
			beforeNodesIno := before[filepath.Join("etc", "singbox-sub-manager", "nodes.conf")].Ino

			res := Run(ctxBG(), f.env, Params{Mutation: r.m, MinVersion: r.minVersion})
			if got := res.ExitCode(); got != r.wantExit {
				t.Fatalf("exit = %d, want %d (err=%v cause=%v)", got, r.wantExit, res.Err, res.Cause)
			}
			if r.wantExit != 0 {
				// every rejection leaves the host exactly as it was and never
				// reaches the network
				if r.wantErr != "" && (res.Err == nil || !strings.Contains(res.Err.Error(), r.wantErr)) {
					t.Fatalf("err = %v, want containing %q", res.Err, r.wantErr)
				}
				if !res.PreTransaction {
					t.Errorf("rejection must be reported as pre-transaction: %+v", res)
				}
				f.assertTreeUnchanged(before, true)
				if calls := f.fetch.Calls(); len(calls) != 0 {
					t.Errorf("a rejected publish reached the network: %v", calls)
				}
				return
			}
			f.assertNoResidue()
			var enabled []nodes.Node
			for _, n := range f.loadNodes() {
				if n.Enabled {
					enabled = append(enabled, n)
				}
			}
			var names []string
			for _, n := range f.loadNodes() {
				names = append(names, n.Name)
			}
			if strings.Join(names, ",") != strings.Join(r.wantNodes, ",") {
				t.Errorf("nodes.conf has %v, want %v", names, r.wantNodes)
			}
			var enNames []string
			for _, n := range enabled {
				enNames = append(enNames, n.Name)
			}
			if strings.Join(enNames, ",") != strings.Join(r.wantEn, ",") {
				t.Errorf("enabled = %v, want %v", enNames, r.wantEn)
			}
			f.assertPublished(enabled)
			afterIno := treeState(t, f.base, true)[filepath.Join("etc", "singbox-sub-manager", "nodes.conf")].Ino
			if r.nodesTouch == (afterIno == beforeNodesIno) {
				t.Errorf("nodes.conf rewritten=%v, want %v", afterIno != beforeNodesIno, r.nodesTouch)
			}
			if calls := f.fetch.Calls(); strings.Join(calls, ",") != "clash.yaml,sr.txt" {
				t.Errorf("HTTPS verification fetched %v, want clash.yaml then sr.txt", calls)
			}
			for _, d := range f.fetch.domains {
				if d != testDomain {
					t.Errorf("verification used domain %q, want %q", d, testDomain)
				}
			}
		})
	}
}

// A fixture whose install.json is absent must not grow center identity files
// or a token directory just because publish was run there (spec §4.2 "本版本
// 新增的代码路径都不得创建 install.json、token 或 nodes.conf"; plan T9).
func TestPublishNeverCreatesIdentityFiles(t *testing.T) {
	f := newFixture(t)
	os.Remove(f.installJS)
	os.Remove(f.nodesPath)
	os.RemoveAll(f.tokenDir)
	before := treeState(t, f.base, true)
	res := Run(ctxBG(), f.env, Params{Mutation: Mutation{Kind: KindAdd, Node: hy2Node("X")}})
	if res.ExitCode() != 1 {
		t.Fatalf("exit = %d, want 1 (%v)", res.ExitCode(), res.Err)
	}
	f.assertTreeUnchanged(before, true)
}

// --- successful publication: file effects (spec §10.3 steps 4-8) ---

func TestPublishSuccessEffects(t *testing.T) {
	f := newFixture(t)
	f.env.TrustedOwnerUID, f.env.TrustedOwnerGID = os.Getuid(), os.Getgid()

	var stagingSeen string
	var journalAtRegistered *transaction.Journal
	setHook(func(p string) error {
		switch p {
		case "registered":
			j := readJournal(t, f)
			journalAtRegistered = j
		case "after:stage-sr":
			ents, _ := os.ReadDir(f.wwwRoot)
			for _, e := range ents {
				if strings.HasPrefix(e.Name(), ".proxyctl-publish-") {
					stagingSeen = e.Name()
				}
			}
			if stagingSeen == "" {
				t.Errorf("no staging directory beside the subscription root")
				return nil
			}
			// staging lives outside the web root and nothing leaks into it
			if _, err := os.Stat(filepath.Join(f.subRoot, stagingSeen)); err == nil {
				t.Errorf("staging is inside the web root")
			}
			st, err := os.Lstat(filepath.Join(f.wwwRoot, stagingSeen))
			if err != nil || !st.IsDir() || st.Mode().Perm() != 0700 {
				t.Errorf("staging dir = %v %v, want a 0700 directory", st, err)
			}
			for _, name := range []string{"clash.yaml", "sr.txt"} {
				b, err := os.ReadFile(filepath.Join(f.wwwRoot, stagingSeen, name))
				if err != nil || len(b) == 0 {
					t.Errorf("staged %s missing: %v", name, err)
				}
				fi, _ := os.Lstat(filepath.Join(f.wwwRoot, stagingSeen, name))
				if fi != nil && fi.Mode().Perm() != 0600 {
					t.Errorf("staged %s mode = %v, want 0600", name, fi.Mode().Perm())
				}
			}
		}
		return nil
	})
	res := Run(ctxBG(), f.env, Params{Mutation: Mutation{Kind: KindAdd, Node: hy2Node("X")}})
	if res.ExitCode() != 0 {
		t.Fatalf("exit = %d: %v", res.ExitCode(), res.Err)
	}
	f.assertNoResidue()
	if stagingSeen == "" {
		t.Fatal("staging was never observed")
	}
	if _, err := os.Lstat(filepath.Join(f.wwwRoot, stagingSeen)); !os.IsNotExist(err) {
		t.Errorf("staging not removed after commit: %v", err)
	}
	if journalAtRegistered == nil {
		t.Fatal("journal never observed")
	}

	// modes and ownership (spec §10.3 step 6)
	nst, _ := os.Lstat(f.nodesPath)
	if nst.Mode().Perm() != 0600 {
		t.Errorf("nodes.conf mode = %v, want 0600", nst.Mode().Perm())
	}
	ns := nst.Sys().(*syscall.Stat_t)
	if int(ns.Uid) != os.Getuid() || int(ns.Gid) != os.Getgid() {
		t.Errorf("nodes.conf owner = %d:%d, want the trusted owner", ns.Uid, ns.Gid)
	}
	for _, p := range []string{f.clashPath(), f.srPath()} {
		st, _ := os.Lstat(p)
		if st.Mode().Perm() != 0644 {
			t.Errorf("%s mode = %v, want 0644", filepath.Base(p), st.Mode().Perm())
		}
		s := st.Sys().(*syscall.Stat_t)
		// the previous owner is inherited
		wantUID, wantGID := uint32(os.Getuid()), uint32(os.Getgid())
		if os.Geteuid() == 0 {
			wantUID, wantGID = 4242, 4343
		}
		if s.Uid != wantUID || s.Gid != wantGID {
			t.Errorf("%s owner = %d:%d, want inherited %d:%d", filepath.Base(p), s.Uid, s.Gid, wantUID, wantGID)
		}
	}

	// the journal planned every final write in the file's own directory
	// (temp names are registered per step, none refers to staging)
	var finals int
	for _, s := range journalAtRegistered.Steps {
		if s.Kind != transaction.KindWriteFile || strings.Contains(s.Target, ".proxyctl-publish-") {
			continue
		}
		finals++
		if len(s.Temps) != 1 || !strings.HasPrefix(s.Temps[0], ".proxyctl-txn-") {
			t.Errorf("step %s temps = %v", s.ID, s.Temps)
		}
	}
	if finals != 3 {
		t.Errorf("planned %d final write_file steps, want 3 (nodes.conf, clash.yaml, sr.txt)", finals)
	}
	if journalAtRegistered.Op != "publish" {
		t.Errorf("journal op = %q", journalAtRegistered.Op)
	}
	f.assertPublished(nodes.Enabled(f.loadNodes()))
}

// A previously-absent subscription file is created as the caddy user (spec
// §10.3 step 6) and removed again when the publication fails.
func TestPublishAbsentFilesOwnerAndRollback(t *testing.T) {
	f := newFixture(t)
	os.Remove(f.clashPath())
	os.Remove(f.srPath())
	calls := 0
	f.env.LookupFileOwner = func() (uint32, uint32, error) { calls++; return f.caddyUID, f.caddyGID, nil }

	res := Run(ctxBG(), f.env, Params{Mutation: Mutation{Kind: KindPublish}})
	if res.ExitCode() != 0 {
		t.Fatalf("exit = %d: %v", res.ExitCode(), res.Err)
	}
	if calls == 0 {
		t.Error("the caddy owner was never looked up for the absent files")
	}
	for _, p := range []string{f.clashPath(), f.srPath()} {
		s := mustStat(t, p).Sys().(*syscall.Stat_t)
		if s.Uid != f.caddyUID || s.Gid != f.caddyGID {
			t.Errorf("%s owner = %d:%d, want caddy %d:%d", filepath.Base(p), s.Uid, s.Gid, f.caddyUID, f.caddyGID)
		}
	}

	// now fail the HTTPS leg: both files were absent, so rollback deletes them
	os.Remove(f.clashPath())
	os.Remove(f.srPath())
	before := treeState(t, f.base, false)
	f.fetch.status["sr.txt"] = 503
	res = Run(ctxBG(), f.env, Params{Mutation: Mutation{Kind: KindPublish}})
	if res.ExitCode() != 4 {
		t.Fatalf("exit = %d, want 4 (%v)", res.ExitCode(), res.Err)
	}
	f.assertNoResidue()
	if d := diffTrees(before, treeState(t, f.base, false)); d != "" {
		t.Errorf("rollback did not restore the absent files:\n%s", d)
	}
	for _, p := range []string{f.clashPath(), f.srPath()} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s exists after rollback (%v)", filepath.Base(p), err)
		}
	}
}

func TestPublishOwnerLookupFailure(t *testing.T) {
	f := newFixture(t)
	os.Remove(f.srPath())
	f.env.LookupFileOwner = func() (uint32, uint32, error) { return 0, 0, errors.New("no such user: caddy") }
	before := treeState(t, f.base, false)
	res := Run(ctxBG(), f.env, Params{Mutation: Mutation{Kind: KindPublish}})
	if res.ExitCode() != 1 || res.Err == nil || !strings.Contains(res.Err.Error(), "caddy") {
		t.Fatalf("exit = %d err=%v, want 1 mentioning caddy", res.ExitCode(), res.Err)
	}
	f.assertTreeUnchanged(before, false)
	f.assertNoResidue()
}

func mustStat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	st, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func readJournal(t *testing.T, f *fixture) *transaction.Journal {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.txnDir(), "journal.json"))
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	j, err := transaction.ParseJournal(b)
	if err != nil {
		t.Fatalf("parse journal: %v", err)
	}
	return j
}

// --- TestPublishFailureBoundary (spec §7.4 boundary, §10.3-§10.4, §13.3) ---

func TestPublishFailureBoundary(t *testing.T) {
	boom := errors.New("injected failure")
	mut := Mutation{Kind: KindAdd, Node: hy2Node("X")}

	// Failures before the first managed publish path is modified (staging and
	// snapshots do not count): exit 1, nothing changed, staging and the
	// transaction area gone.
	preBoundary := []struct {
		name  string
		setup func(f *fixture)
	}{
		{"render/validate failure after staging exists", func(f *fixture) {
			f.env.Validate = func([]nodes.Node) (subscriptionvalidate.Rendered, error) {
				return subscriptionvalidate.Rendered{}, boom
			}
			setHook(func(p string) error {
				if p == "after:stage-sr" {
					if ents, _ := os.ReadDir(f.wwwRoot); !hasPrefixEntry(ents, ".proxyctl-publish-") {
						f.t.Errorf("validation ran before staging existed")
					}
				}
				return nil
			})
		}},
		{"staging directory cannot be created", func(f *fixture) { setHook(errAt("before:mkdir-staging", boom)) }},
		{"staging file write fails", func(f *fixture) { setHook(errAt("before:stage-sr", boom)) }},
		{"failure between staging and snapshots", func(f *fixture) { setHook(errAt("validated", boom)) }},
		{"failure after snapshots, before the first write", func(f *fixture) { setHook(errAt("before:write-nodes", boom)) }},
		{"failure before the transaction registers", func(f *fixture) { setHook(errAt("began", boom)) }},
		{"failure registering the plan", func(f *fixture) { setHook(errAt("registered", boom)) }},
	}
	for _, tc := range preBoundary {
		tc := tc
		t.Run("pre/"+tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.setup(f)
			before := treeState(t, f.base, false)
			res := Run(ctxBG(), f.env, Params{Mutation: mut})
			if res.ExitCode() != 1 || !res.PreTransaction {
				t.Fatalf("exit = %d (pre=%v err=%v), want 1 pre-transaction", res.ExitCode(), res.PreTransaction, res.Err)
			}
			f.assertNoResidue()
			if d := diffTrees(before, treeState(t, f.base, false)); d != "" {
				t.Errorf("tree changed:\n%s", d)
			}
			if calls := f.fetch.Calls(); len(calls) != 0 {
				t.Errorf("network reached: %v", calls)
			}
		})
	}

	// Failures after a managed path was modified: automatic rollback, exit 4,
	// local state restored (bytes, mode, owner), transaction removed, no .bak.
	postBoundary := []struct {
		name  string
		setup func(f *fixture)
	}{
		{"after nodes.conf replaced", func(f *fixture) { setHook(errAt("after:write-nodes", boom)) }},
		{"after clash.yaml replaced", func(f *fixture) { setHook(errAt("after:write-clash", boom)) }},
		{"after sr.txt replaced", func(f *fixture) { setHook(errAt("after:write-sr", boom)) }},
		{"HTTPS status not 200 for clash.yaml", func(f *fixture) { f.fetch.status["clash.yaml"] = 503 }},
		{"HTTPS status not 200 for sr.txt", func(f *fixture) { f.fetch.status["sr.txt"] = 404 }},
		{"HTTPS body hash differs for clash.yaml", func(f *fixture) { f.fetch.body["clash.yaml"] = []byte("someone else's content") }},
		{"HTTPS body hash differs for sr.txt (a public response with other content)", func(f *fixture) { f.fetch.body["sr.txt"] = []byte("stale") }},
		{"Caddy unavailable", func(f *fixture) {
			f.fetch.errs["clash.yaml"] = errors.New("dial tcp 127.0.0.1:443: connection refused")
			f.fetch.errs["sr.txt"] = errors.New("dial tcp 127.0.0.1:443: connection refused")
		}},
		{"failure just before the commit flip", func(f *fixture) { setHook(errAt("before-commit", boom)) }},
	}
	for _, tc := range postBoundary {
		tc := tc
		t.Run("post/"+tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.setup(f)
			before := treeState(t, f.base, false)
			res := Run(ctxBG(), f.env, Params{Mutation: mut})
			if res.ExitCode() != 4 {
				t.Fatalf("exit = %d (err=%v cause=%v), want 4", res.ExitCode(), res.Err, res.Cause)
			}
			f.assertNoResidue()
			if d := diffTrees(before, treeState(t, f.base, false)); d != "" {
				t.Errorf("rollback did not restore the previous state:\n%s", d)
			}
			// HTTPS re-verification after the rollback is advisory only: a
			// failure never changes the exit code nor keeps the journal
			// (spec §10.4); the transaction area is already gone above.
		})
	}

	t.Run("re-verification after rollback warns but never fails", func(t *testing.T) {
		f := newFixture(t)
		f.fetch.errs["clash.yaml"] = errors.New("connection refused")
		f.fetch.errs["sr.txt"] = errors.New("connection refused")
		res := Run(ctxBG(), f.env, Params{Mutation: mut})
		if res.ExitCode() != 4 {
			t.Fatalf("exit = %d, want 4", res.ExitCode())
		}
		if len(res.Warnings) == 0 {
			t.Error("a failed post-rollback HTTPS re-verification must be reported as a warning")
		}
		// pre-publication fetches + the re-verification (against the previous
		// hashes) of both files
		if calls := f.fetch.Calls(); len(calls) < 2 {
			t.Errorf("fetch calls = %v", calls)
		}
		f.assertNoResidue()
	})

	t.Run("re-verification expects the previous hashes", func(t *testing.T) {
		f := newFixture(t)
		oldClash, _ := os.ReadFile(f.clashPath())
		oldSum := sha256.Sum256(oldClash)
		var seen []string
		f.env.Fetcher = fetcherFunc(func(ctx context.Context, domain, urlPath string) (FetchResult, error) {
			name := strings.TrimPrefix(urlPath, "/"+testToken+"/")
			b, _ := os.ReadFile(filepath.Join(f.tokenDir, name))
			sum := sha256.Sum256(b)
			seen = append(seen, name+":"+hex.EncodeToString(sum[:])[:8])
			if len(seen) == 1 {
				return FetchResult{Status: 503}, nil // fail the first (publishing) verification
			}
			return FetchResult{Status: 200, SHA256: hex.EncodeToString(sum[:])}, nil
		})
		res := Run(ctxBG(), f.env, Params{Mutation: mut})
		if res.ExitCode() != 4 {
			t.Fatalf("exit = %d, want 4", res.ExitCode())
		}
		if len(res.Warnings) != 0 {
			t.Errorf("unexpected warnings %v: the restored file serves the previous hash", res.Warnings)
		}
		if len(seen) != 3 || !strings.HasPrefix(seen[1], "clash.yaml:"+hex.EncodeToString(oldSum[:])[:8]) {
			t.Errorf("fetch sequence %v: re-verification must see the restored previous content", seen)
		}
	})

	t.Run("rollback success is judged on local state: tampering during rollback is exit 5", func(t *testing.T) {
		f := newFixture(t)
		f.fetch.status["sr.txt"] = 503
		setHook(func(p string) error {
			if p == "wrapup" {
				// an external actor edits clash.yaml after our write and before the rollback
				return os.WriteFile(f.clashPath(), []byte("external edit"), 0644)
			}
			return nil
		})
		before := treeState(t, f.base, false)
		res := Run(ctxBG(), f.env, Params{Mutation: mut})
		if res.ExitCode() != 5 || res.TxnDir == "" {
			t.Fatalf("exit = %d txn=%q err=%v, want 5 naming the transaction area", res.ExitCode(), res.TxnDir, res.Err)
		}
		if _, err := os.Lstat(f.txnDir()); err != nil {
			t.Errorf("the transaction area must be preserved: %v", err)
		}
		if !errors.Is(res.Err, transaction.ErrTampered) {
			t.Errorf("err = %v, want ErrTampered", res.Err)
		}
		// nothing was rolled back over the external edit
		if b, _ := os.ReadFile(f.clashPath()); string(b) != "external edit" {
			t.Errorf("clash.yaml was overwritten despite the tamper: %q", b)
		}
		_ = before
	})

	t.Run("per-file HTTPS timeout is 10 seconds, enforced through the injected clock", func(t *testing.T) {
		f := newFixture(t)
		f.fetch.block = true
		var asked []time.Duration
		f.env.NewTimeout = func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
			asked = append(asked, d)
			return context.WithTimeout(parent, 20*time.Millisecond)
		}
		start := time.Now()
		res := Run(ctxBG(), f.env, Params{Mutation: mut})
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("took %v: the injected clock did not bound the verification", elapsed)
		}
		if res.ExitCode() != 4 {
			t.Fatalf("exit = %d, want 4 (%v)", res.ExitCode(), res.Err)
		}
		if len(asked) == 0 || asked[0] != 10*time.Second {
			t.Fatalf("verification timeouts requested = %v, want a 10s budget per file", asked)
		}
		f.assertNoResidue()
	})

	t.Run("cancellation", func(t *testing.T) {
		cancelAt := func(point string) (context.Context, func(string) error) {
			ctx, cancel := context.WithCancel(ctxBG())
			return ctx, func(p string) error {
				if p == point {
					cancel()
				}
				return nil
			}
		}
		cases := []struct {
			point    string
			wantExit int
		}{
			{"began", 1},
			{"after:mkdir-staging", 1},
			{"after:stage-clash", 1},
			{"after:snap-clash", 1},
			{"after:write-nodes", 4},
			{"after:write-clash", 4},
			{"after:write-sr", 4},
			{"verified-local", 4},
			{"before-commit", 0}, // verification already succeeded: the success boundary wins
			{"after-commit-flip", 0},
		}
		for _, c := range cases {
			c := c
			t.Run(c.point, func(t *testing.T) {
				f := newFixture(t)
				ctx, h := cancelAt(c.point)
				setHook(h)
				before := treeState(t, f.base, false)
				res := Run(ctx, f.env, Params{Mutation: mut})
				if got := res.ExitCode(); got != c.wantExit {
					t.Fatalf("exit = %d (err=%v cause=%v), want %d", got, res.Err, res.Cause, c.wantExit)
				}
				f.assertNoResidue()
				if c.wantExit != 0 {
					if d := diffTrees(before, treeState(t, f.base, false)); d != "" {
						t.Errorf("cancelled publish did not restore the previous state:\n%s", d)
					}
				} else {
					f.assertPublished(nodes.Enabled(f.loadNodes()))
				}
			})
		}
	})

	t.Run("a signal during the rollback does not interrupt it (independent budget)", func(t *testing.T) {
		f := newFixture(t)
		f.fetch.status["sr.txt"] = 503
		ctx, cancel := context.WithCancel(ctxBG())
		setHook(func(p string) error {
			if p == "wrapup" {
				cancel() // the rollback is about to start; the main context dies
			}
			return nil
		})
		before := treeState(t, f.base, false)
		res := Run(ctx, f.env, Params{Mutation: mut})
		if res.ExitCode() != 4 {
			t.Fatalf("exit = %d (err=%v), want 4: the rollback must complete under its own context", res.ExitCode(), res.Err)
		}
		f.assertNoResidue()
		if d := diffTrees(before, treeState(t, f.base, false)); d != "" {
			t.Errorf("rollback did not complete:\n%s", d)
		}
	})

	t.Run("cancelled before the lock-protected work starts changes nothing", func(t *testing.T) {
		f := newFixture(t)
		ctx, cancel := context.WithCancel(ctxBG())
		cancel()
		before := treeState(t, f.base, true)
		res := Run(ctx, f.env, Params{Mutation: mut})
		if res.ExitCode() != 1 {
			t.Fatalf("exit = %d, want 1", res.ExitCode())
		}
		f.assertTreeUnchanged(before, true)
	})

	t.Run("after the commit point only cleanup runs: a cleanup failure is exit 5 and recover finishes it", func(t *testing.T) {
		f := newFixture(t)
		var stray string
		setHook(func(p string) error {
			if p == "after-commit-flip" {
				ents, _ := os.ReadDir(f.wwwRoot)
				for _, e := range ents {
					if strings.HasPrefix(e.Name(), ".proxyctl-publish-") {
						stray = filepath.Join(f.wwwRoot, e.Name(), "stray")
						return os.WriteFile(stray, []byte("x"), 0600)
					}
				}
				t.Errorf("no staging directory at the commit point")
			}
			return nil
		})
		res := Run(ctxBG(), f.env, Params{Mutation: mut})
		if res.ExitCode() != 5 || res.TxnDir == "" {
			t.Fatalf("exit = %d txn=%q err=%v, want 5", res.ExitCode(), res.TxnDir, res.Err)
		}
		// the publication itself is final; nothing was rolled back
		f.assertPublished(nodes.Enabled(f.loadNodes()))
		if got := f.loadNodes(); len(got) != 3 {
			t.Errorf("nodes.conf has %d nodes after a committed publish, want 3", len(got))
		}
		// recover refuses to touch the unexpected file's directory...
		setHook(func(string) error { return nil })
		rres := RunRecover(ctxBG(), f.env, RecoverParams{})
		if rres.ExitCode() != 5 {
			t.Fatalf("recover with the stray file present: exit = %d, want 5", rres.ExitCode())
		}
		// ...and finishes the cleanup once the operator removed it
		if err := os.Remove(stray); err != nil {
			t.Fatal(err)
		}
		rres = RunRecover(ctxBG(), f.env, RecoverParams{})
		if rres.ExitCode() != 0 {
			t.Fatalf("recover after removing the stray file: exit = %d (%v)", rres.ExitCode(), rres.Err)
		}
		f.assertNoResidue()
		f.assertPublished(nodes.Enabled(f.loadNodes()))
	})
}

type fetcherFunc func(ctx context.Context, domain, urlPath string) (FetchResult, error)

func (f fetcherFunc) Fetch(ctx context.Context, domain, urlPath string) (FetchResult, error) {
	return f(ctx, domain, urlPath)
}

func hasPrefixEntry(ents []os.DirEntry, prefix string) bool {
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), prefix) {
			return true
		}
	}
	return false
}

// A Caddy outage is the common failure: local rollback, the transaction is
// gone (so nothing locks nodes.conf management), exit 4, and a following
// recover has nothing left to do (spec §10.4, plan T9).
func TestPublishCaddyOutageLeavesNothingForRecover(t *testing.T) {
	f := newFixture(t)
	f.fetch.errs["clash.yaml"] = errors.New("connection refused")
	res := Run(ctxBG(), f.env, Params{Mutation: Mutation{Kind: KindAdd, Node: hy2Node("X")}})
	if res.ExitCode() != 4 {
		t.Fatalf("exit = %d, want 4", res.ExitCode())
	}
	f.assertNoResidue()
	rres := RunRecover(ctxBG(), f.env, RecoverParams{})
	if rres.ExitCode() != 0 {
		t.Fatalf("recover exit = %d (%v), want 0", rres.ExitCode(), rres.Err)
	}
	// with nothing to recover, --abandon is refused and changes nothing
	before := treeState(t, f.base, true)
	ares := RunRecover(ctxBG(), f.env, RecoverParams{Abandon: true})
	if ares.ExitCode() != 1 {
		t.Fatalf("abandon with nothing to abandon: exit = %d, want 1", ares.ExitCode())
	}
	f.assertTreeUnchanged(before, true)
	// and nodes.conf is no longer locked out of normal publishing
	f.fetch.errs = map[string]error{}
	if res := Run(ctxBG(), f.env, Params{Mutation: Mutation{Kind: KindPublish}}); res.ExitCode() != 0 {
		t.Fatalf("a publish after the rolled-back one: exit = %d (%v)", res.ExitCode(), res.Err)
	}
}

// Legacy CRUD keeps nodes.WriteFile's .bak behavior; the publish path never
// produces one (spec §10.3 step 6).
func TestPublishWritesNoBackup(t *testing.T) {
	f := newFixture(t)
	res := Run(ctxBG(), f.env, Params{Mutation: Mutation{Kind: KindAdd, Node: hy2Node("X")}})
	if res.ExitCode() != 0 {
		t.Fatalf("exit = %d: %v", res.ExitCode(), res.Err)
	}
	for _, dir := range []string{filepath.Dir(f.nodesPath), f.tokenDir} {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if strings.HasSuffix(e.Name(), ".bak") {
				t.Errorf("publish produced %s", filepath.Join(dir, e.Name()))
			}
		}
	}
	// rollback does not leave one either
	f.fetch.status["clash.yaml"] = 500
	if res := Run(ctxBG(), f.env, Params{Mutation: Mutation{Kind: KindAdd, Node: hy2Node("Y")}}); res.ExitCode() != 4 {
		t.Fatalf("exit = %d, want 4", res.ExitCode())
	}
	f.assertNoResidue()
}

// A publish that was cancelled while L1/L2 were still being waited for never
// reaches this package; Run itself must not hold anything open when it
// returns (descriptors are released on every exit path).
func TestPublishReleasesDescriptors(t *testing.T) {
	f := newFixture(t)
	count := func() int {
		ents, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Skip("no /proc/self/fd")
		}
		return len(ents)
	}
	base := count()
	for i := 0; i < 3; i++ {
		Run(ctxBG(), f.env, Params{Mutation: Mutation{Kind: KindPublish}})
		f.fetch.status["sr.txt"] = 503
		Run(ctxBG(), f.env, Params{Mutation: Mutation{Kind: KindPublish}})
		f.fetch.status = map[string]int{}
		setHook(errAt("before:write-clash", errors.New("x")))
		Run(ctxBG(), f.env, Params{Mutation: Mutation{Kind: KindPublish}})
		setHook(func(string) error { return nil })
	}
	if after := count(); after > base {
		t.Errorf("file descriptors leaked: %d before, %d after", base, after)
	}
}

// ApplyMutation is the pure heart of the idempotency matrix; the package
// exposes it so the CLI can build the same mutation from its flags.
func TestApplyMutationDoesNotAliasInput(t *testing.T) {
	cur := []nodes.Node{hy2Node("A"), hy2Node("B")}
	snapshot := append([]nodes.Node(nil), cur...)
	next, changed, err := ApplyMutation(cur, Mutation{Kind: KindDisable, Name: "A"})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if next[0].Enabled {
		t.Error("A was not disabled in the result")
	}
	for i := range cur {
		if cur[i] != snapshot[i] {
			t.Errorf("ApplyMutation modified its input at %d", i)
		}
	}
}

// Precheck runs before the locks (fast reject) and again after them: it is
// pure with respect to the host.
func TestPrecheckIsReadOnly(t *testing.T) {
	f := newFixture(t)
	before := treeState(t, f.base, true)
	plan, err := Precheck(f.env, "v0.10.0")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Layout.Domain != testDomain || plan.Layout.Token != testToken || plan.Layout.SubscriptionRoot != f.subRoot {
		t.Errorf("layout = %+v", plan.Layout)
	}
	if len(plan.Nodes) != 2 {
		t.Errorf("nodes = %d", len(plan.Nodes))
	}
	f.assertTreeUnchanged(before, true)
}

// --- the production HTTPS fetcher (spec §10.3 step 7) ---

func newTLSTestServer(t *testing.T, h http.HandlerFunc) (srv *httptest.Server, pool *x509.CertPool, sniSeen func() string) {
	t.Helper()
	var mu sync.Mutex
	var sni string
	srv = httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{GetConfigForClient: func(hi *tls.ClientHelloInfo) (*tls.Config, error) {
		mu.Lock()
		sni = hi.ServerName
		mu.Unlock()
		return nil, nil
	}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool = x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, pool, func() string { mu.Lock(); defer mu.Unlock(); return sni }
}

func TestLoopbackFetcher(t *testing.T) {
	body := []byte("proxies:\n  - name: A\n")
	var mu sync.Mutex
	var gotHost, gotPath, gotMethod string
	srv, pool, sniSeen := newTLSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotHost, gotPath, gotMethod = r.Host, r.URL.Path, r.Method
		mu.Unlock()
		switch r.URL.Path {
		case "/tok/missing.txt":
			http.NotFound(w, r)
		case "/tok/redirect.txt":
			http.Redirect(w, r, "/tok/clash.yaml", http.StatusFound)
		case "/tok/big.txt":
			w.Write(make([]byte, 100))
		default:
			w.Write(body)
		}
	})
	f := LoopbackFetcher{Addr: srv.Listener.Addr().String(), RootCAs: pool}

	t.Run("status and body hash, domain as SNI and Host, dialing the configured loopback address", func(t *testing.T) {
		res, err := f.Fetch(ctxBG(), "example.com", "/tok/clash.yaml")
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		if res.Status != 200 || res.SHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("res = %+v", res)
		}
		mu.Lock()
		defer mu.Unlock()
		if gotHost != "example.com" || gotPath != "/tok/clash.yaml" || gotMethod != "GET" {
			t.Errorf("server saw host=%q path=%q method=%q", gotHost, gotPath, gotMethod)
		}
		if sniSeen() != "example.com" {
			t.Errorf("SNI = %q, want the subscription domain", sniSeen())
		}
	})
	t.Run("non-200 statuses are reported, redirects are not followed", func(t *testing.T) {
		for path, want := range map[string]int{"/tok/missing.txt": 404, "/tok/redirect.txt": 302} {
			res, err := f.Fetch(ctxBG(), "example.com", path)
			if err != nil || res.Status != want {
				t.Errorf("%s: res=%+v err=%v, want status %d", path, res, err, want)
			}
		}
	})
	t.Run("a certificate for another name is refused", func(t *testing.T) {
		if _, err := f.Fetch(ctxBG(), "other.example.net", "/tok/clash.yaml"); err == nil {
			t.Fatal("a TLS name mismatch must fail the verification")
		}
	})
	t.Run("untrusted certificate is refused", func(t *testing.T) {
		g := LoopbackFetcher{Addr: srv.Listener.Addr().String(), RootCAs: x509.NewCertPool()}
		if _, err := g.Fetch(ctxBG(), "example.com", "/tok/clash.yaml"); err == nil {
			t.Fatal("an untrusted certificate must fail the verification")
		}
	})
	t.Run("the default address is the loopback listener, never the domain's DNS name", func(t *testing.T) {
		var dialed string
		g := LoopbackFetcher{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialed = addr
			return nil, errors.New("refused")
		}}
		if _, err := g.Fetch(ctxBG(), "sub.example.test", "/tok/clash.yaml"); err == nil {
			t.Fatal("expected the dial error")
		}
		if dialed != "127.0.0.1:443" {
			t.Errorf("dialed %q, want 127.0.0.1:443", dialed)
		}
	})
	t.Run("oversized bodies are refused", func(t *testing.T) {
		g := LoopbackFetcher{Addr: srv.Listener.Addr().String(), RootCAs: pool, MaxBody: 10}
		if _, err := g.Fetch(ctxBG(), "example.com", "/tok/big.txt"); err == nil {
			t.Fatal("body over the cap must fail")
		}
	})
}

// A hung server must not hold the fetch past its context, and nothing may be
// left running afterwards (spec §13.2/§13.3: resources released on every
// path).
func TestLoopbackFetcherHonorsContext(t *testing.T) {
	release := make(chan struct{})
	srv, pool, _ := newTLSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	f := LoopbackFetcher{Addr: srv.Listener.Addr().String(), RootCAs: pool}

	time.Sleep(50 * time.Millisecond) // let the server's accept loop settle
	before := runtime.NumGoroutine()

	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctxBG(), 150*time.Millisecond)
		defer cancel()
		start := time.Now()
		res, err := f.Fetch(ctx, "example.com", "/tok/clash.yaml")
		if err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("res=%+v err = %v, want a deadline error", res, err)
		}
		if time.Since(start) > 3*time.Second {
			t.Fatalf("fetch outlived its deadline by far: %v", time.Since(start))
		}
	})
	t.Run("cancellation without a deadline", func(t *testing.T) {
		ctx, cancel := context.WithCancel(ctxBG())
		go func() { time.Sleep(100 * time.Millisecond); cancel() }()
		start := time.Now()
		res, err := f.Fetch(ctx, "example.com", "/tok/clash.yaml")
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("res=%+v err = %v, want a cancellation error", res, err)
		}
		if time.Since(start) > 3*time.Second {
			t.Fatalf("fetch did not stop on cancellation: %v", time.Since(start))
		}
	})
	t.Run("an already-cancelled context never dials", func(t *testing.T) {
		ctx, cancel := context.WithCancel(ctxBG())
		cancel()
		g := f
		g.DialContext = func(context.Context, string, string) (net.Conn, error) {
			t.Error("dialed with a cancelled context")
			return nil, errors.New("x")
		}
		if _, err := g.Fetch(ctx, "example.com", "/x"); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
	// the fetcher's watcher and connection goroutines are gone shortly after
	// Fetch returns; the server side needs a moment to notice the closed
	// connections
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Errorf("goroutines leaked by the fetcher: %d before, %d after", before, after)
	}
}
