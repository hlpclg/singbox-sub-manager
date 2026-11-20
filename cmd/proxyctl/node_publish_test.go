package main

// node_publish_test.go covers plan Task 9's CLI layer: `proxyctl node publish
// [--recover [--abandon]]` and `node add|edit|enable|disable|remove
// --publish` (design spec §4.5, §10, §13.1). Every parse-failure case returns
// before newPublishRuntime is ever called, so none of those tests need root,
// a lock, the network or even a host layout -- exactly §4.5's "错误用法返回
// 2，没有取锁、网络调用，目标文件bytes/mtime不变". The flow tests drive the
// real internal/publish engine against a temporary host layout with a fake
// HTTPS fetcher, root check, role detection and lock.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hlpclg/singbox-sub-manager/internal/hostlock"
	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
	"github.com/hlpclg/singbox-sub-manager/internal/publish"
	"github.com/hlpclg/singbox-sub-manager/internal/realitynode"
	"github.com/hlpclg/singbox-sub-manager/internal/render"
	"github.com/hlpclg/singbox-sub-manager/internal/role"
	"github.com/hlpclg/singbox-sub-manager/internal/subscriptionvalidate"
)

const cliToken = "tok0123456789abcdefABCDEF_-xyz"
const cliDomain = "sub.example.test"

// cliFixture is a standard subscription center under a temp dir plus the
// fake runtime that points the publish CLI at it.
type cliFixture struct {
	t         *testing.T
	base      string
	nodesPath string
	installJS string
	stateDir  string
	wwwRoot   string
	subRoot   string
	tokenDir  string
	log       *callLog
	opts      *publishRuntimeOpts
	fetcher   *cliFetcher
	env       *publish.Env
}

type publishRuntimeOpts struct {
	euid       int
	roles      []role.Status // successive detectRole results; the last repeats
	acquireErr error
	// acquireBlock, when set, is closed once acquire is entered, after which
	// acquire blocks until its ctx is done.
	acquireBlock chan struct{}
	ctx          context.Context
	cancel       context.CancelFunc
}

type cliFetcher struct {
	mu     sync.Mutex
	dir    string
	calls  []string
	status map[string]int
	errs   map[string]error
}

func (f *cliFetcher) Fetch(ctx context.Context, domain, urlPath string) (publish.FetchResult, error) {
	name := strings.TrimPrefix(urlPath, "/"+cliToken+"/")
	f.mu.Lock()
	f.calls = append(f.calls, name)
	status, ferr := f.status[name], f.errs[name]
	f.mu.Unlock()
	if ferr != nil {
		return publish.FetchResult{}, ferr
	}
	b, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil {
		return publish.FetchResult{Status: 404}, nil
	}
	if status == 0 {
		status = 200
	}
	sum := sha256.Sum256(b)
	return publish.FetchResult{Status: status, SHA256: hex.EncodeToString(sum[:])}, nil
}

func cliHy2(name string) nodes.Node {
	return nodes.Node{Name: name, Type: nodes.TypeHysteria2, Server: "203.0.113.5", Port: 443,
		Password: "pw-" + name, ObfsPassword: "ob-" + name, SNI: "www.example.com", Enabled: true}
}

func writeMode(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func installFakePublishRuntime(t *testing.T, o publishRuntimeOpts) *cliFixture {
	t.Helper()
	f := &cliFixture{t: t, log: &callLog{}, opts: &o}
	if len(o.roles) == 0 {
		o.roles = []role.Status{role.StatusYes}
		f.opts.roles = o.roles
	}
	f.base = t.TempDir()
	etcDir := filepath.Join(f.base, "etc", "singbox-sub-manager")
	f.nodesPath = filepath.Join(etcDir, "nodes.conf")
	f.installJS = filepath.Join(etcDir, "install.json")
	f.stateDir = filepath.Join(f.base, "var-lib", "singbox-sub-manager")
	f.wwwRoot = filepath.Join(f.base, "www")
	f.subRoot = filepath.Join(f.wwwRoot, "proxy-sub")
	f.tokenDir = filepath.Join(f.subRoot, cliToken)
	for _, d := range []string{filepath.Join(f.base, "etc"), etcDir, filepath.Dir(f.stateDir), f.stateDir, f.wwwRoot, f.subRoot, f.tokenDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
		os.Chmod(d, 0755)
	}
	tokenFile := filepath.Join(f.stateDir, "token")
	writeMode(t, tokenFile, []byte(cliToken+"\n"), 0600)
	js, _ := json.Marshal(map[string]string{"domain": cliDomain, "subscription_root": f.subRoot, "token_file": tokenFile})
	writeMode(t, f.installJS, js, 0600)
	ns := []nodes.Node{cliHy2("A"), cliHy2("B")}
	writeMode(t, f.nodesPath, []byte(nodes.Serialize(ns)), 0600)
	writeMode(t, filepath.Join(f.tokenDir, "clash.yaml"), []byte(render.Clash(ns)), 0644)
	writeMode(t, filepath.Join(f.tokenDir, "sr.txt"), []byte(render.Shadowrocket(ns)), 0644)

	f.fetcher = &cliFetcher{dir: f.tokenDir, status: map[string]int{}, errs: map[string]error{}}
	f.env = &publish.Env{
		InstallJSON: f.installJS, NodesConf: f.nodesPath, StateDir: f.stateDir,
		TrustedOwnerUID: os.Getuid(), TrustedOwnerGID: os.Getgid(),
		LookupFileOwner: func() (uint32, uint32, error) { return uint32(os.Getuid()), uint32(os.Getgid()), nil },
		Fetcher:         f.fetcher,
		ProxyctlVersion: "v0.10.0",
	}

	roleCalls := 0
	orig := newPublishRuntime
	t.Cleanup(func() { newPublishRuntime = orig })
	newPublishRuntime = func() publishRuntime {
		f.log.add("runtime")
		return publishRuntime{
			geteuid: func() int { f.log.add("geteuid"); return o.euid },
			detectRole: func() role.Result {
				f.log.add("role")
				i := roleCalls
				if i >= len(f.opts.roles) {
					i = len(f.opts.roles) - 1
				}
				roleCalls++
				return role.Result{Status: f.opts.roles[i]}
			},
			acquire: func(ctx context.Context, levels ...hostlock.Level) (*hostlock.Lease, error) {
				var names []string
				for _, l := range levels {
					names = append(names, l.String())
				}
				f.log.add("lock:" + strings.Join(names, "+"))
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
			newEnv: func() *publish.Env { f.log.add("newEnv"); return f.env },
			newContext: func() (context.Context, context.CancelFunc) {
				if o.ctx != nil {
					return o.ctx, o.cancel
				}
				return context.WithCancel(context.Background())
			},
		}
	}
	return f
}

func (f *cliFixture) loadNodes() []nodes.Node {
	f.t.Helper()
	ns, _, err := nodes.Load(f.nodesPath)
	if err != nil {
		f.t.Fatal(err)
	}
	return ns
}

func (f *cliFixture) nodeNames() string {
	var names []string
	for _, n := range f.loadNodes() {
		names = append(names, n.Name)
	}
	return strings.Join(names, ",")
}

func (f *cliFixture) snapshot() map[string]string {
	f.t.Helper()
	out := map[string]string{}
	filepath.Walk(f.base, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(f.base, p)
		if info.IsDir() {
			out[rel] = "dir " + info.Mode().Perm().String()
			return nil
		}
		b, _ := os.ReadFile(p)
		sum := sha256.Sum256(b)
		out[rel] = hex.EncodeToString(sum[:]) + " " + info.Mode().Perm().String() + " " + info.ModTime().String()
		return nil
	})
	return out
}

func sameSnapshots(t *testing.T, before, after map[string]string) {
	t.Helper()
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s changed: %q -> %q", k, v, after[k])
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			t.Errorf("%s appeared", k)
		}
	}
}

// shellFields splits a command line the way a POSIX shell would for the
// restricted shape §10.1 prints: single-quoted words, '\” for an embedded
// quote, whitespace between words.
func shellFields(t *testing.T, line string) []string {
	t.Helper()
	var out []string
	var cur strings.Builder
	inWord, inQuote := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inQuote:
			if c == '\'' {
				inQuote = false
			} else {
				cur.WriteByte(c)
			}
		case c == '\'':
			inQuote, inWord = true, true
		case c == '\\' && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
			inWord = true
		case c == ' ' || c == '\t':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inQuote {
		t.Fatalf("unterminated quote in %q", line)
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

// proxyctlArgs turns a printed "sudo proxyctl ..." command into the argv
// after the program name.
func proxyctlArgs(t *testing.T, cmdline string) []string {
	t.Helper()
	f := shellFields(t, cmdline)
	if len(f) < 3 || f[0] != "sudo" || f[1] != "proxyctl" {
		t.Fatalf("not a sudo proxyctl command: %q", cmdline)
	}
	return f[2:]
}

const cliPubKey = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
const cliUUID = "11111111-2222-4333-8444-555555555555"

func realityAddArgs() []string {
	return []string{"node", "add", "--publish", "--min-version", "v0.10.0", "--type", "vless-reality",
		"--server", "203.0.113.9", "--port", "443", "--uuid", cliUUID, "--public-key", cliPubKey,
		"--short-id", "ab12", "--sni", "www.microsoft.com", "--name", "jp1"}
}

// --- parsing: exit 2, nothing constructed, nothing touched ---

func TestNodePublishStrictParsing(t *testing.T) {
	hy2Add := []string{"node", "add", "--publish", "--server", "203.0.113.5", "--port", "443", "--password", "p", "--obfs-password", "o", "--sni", "x.example.com", "--name", "n1"}
	with := func(base []string, extra ...string) []string { return append(append([]string(nil), base...), extra...) }
	cases := []struct {
		name string
		args []string
	}{
		{"add: unknown flag", with(hy2Add, "--bogus", "x")},
		{"add: duplicate flag", with(hy2Add, "--server", "other")},
		{"add: missing value for the last flag", with(hy2Add, "--short-id")},
		{"add: --publish=x", []string{"node", "add", "--publish=x", "--name", "n1"}},
		{"add: --publish=true", []string{"node", "add", "--publish=true", "--name", "n1"}},
		{"add: -publish is strict but only --publish is accepted", []string{"node", "add", "-publish", "--name", "n1"}},
		{"add: --publish with --nodes", with(hy2Add, "--nodes", "/tmp/x.conf")},
		{"add: --abandon outside recover", with(hy2Add, "--abandon")},
		{"add: positional argument", with(hy2Add, "extra")},
		{"add: --enabled false leaves a positional", with(hy2Add, "--enabled", "false")},
		{"add: --enabled=maybe", with(hy2Add, "--enabled=maybe")},
		{"add: name starting with a dash", []string{"node", "add", "--publish", "--server", "s", "--port", "443", "--password", "p", "--obfs-password", "o", "--sni", "x", "--name", "-evil"}},
		{"add: missing name", []string{"node", "add", "--publish", "--server", "s", "--port", "443", "--password", "p", "--obfs-password", "o", "--sni", "x"}},
		{"add: missing server", []string{"node", "add", "--publish", "--port", "443", "--password", "p", "--obfs-password", "o", "--sni", "x", "--name", "n"}},
		{"add: missing port", []string{"node", "add", "--publish", "--server", "s", "--password", "p", "--obfs-password", "o", "--sni", "x", "--name", "n"}},
		{"add: missing sni", []string{"node", "add", "--publish", "--server", "s", "--port", "443", "--password", "p", "--obfs-password", "o", "--name", "n"}},
		{"add: hysteria2 without password", []string{"node", "add", "--publish", "--server", "s", "--port", "443", "--obfs-password", "o", "--sni", "x", "--name", "n"}},
		{"add: reality without uuid", []string{"node", "add", "--publish", "--type", "vless-reality", "--server", "s", "--port", "443", "--public-key", cliPubKey, "--sni", "x", "--name", "n"}},
		{"add: reality with a password flag", with(realityAddArgs(), "--password", "p")},
		{"add: hysteria2 with a uuid flag", with(hy2Add, "--uuid", cliUUID)},
		{"add: unknown type", with(hy2Add, "--type", "vmess")},
		{"add: non-numeric port", []string{"node", "add", "--publish", "--server", "s", "--port", "http", "--password", "p", "--obfs-password", "o", "--sni", "x", "--name", "n"}},
		{"add: malformed --min-version", with(hy2Add, "--min-version", "v0.10")},
		{"add: empty --min-version", with(hy2Add, "--min-version", "")},
		{"enable: no node name", []string{"node", "enable", "--publish"}},
		{"enable: two node names", []string{"node", "enable", "--publish", "a", "b"}},
		{"enable: unknown flag", []string{"node", "enable", "--publish", "--force", "a"}},
		{"enable: duplicate --min-version", []string{"node", "enable", "--publish", "--min-version", "v0.10.0", "--min-version", "v0.10.0", "a"}},
		{"enable: --nodes conflicts with --publish", []string{"node", "enable", "--publish", "--nodes", "/tmp/n.conf", "a"}},
		{"disable: --publish=false is not a mode switch", []string{"node", "disable", "--publish=false", "a"}},
		{"remove: --abandon", []string{"node", "remove", "--publish", "--abandon", "a"}},
		{"remove: --help together with other arguments", []string{"node", "remove", "--publish", "--help", "a"}},
		{"edit: no node name", []string{"node", "edit", "--publish", "--port", "1"}},
		{"edit: new name starting with a dash", []string{"node", "edit", "--publish", "a", "--name", "-evil"}},
		{"edit: reality field for a hysteria2 edit is decided later, but unknown flag is not", []string{"node", "edit", "--publish", "a", "--type", "vless-reality"}},
		{"edit: non-numeric port", []string{"node", "edit", "--publish", "a", "--port", "x"}},
		{"publish: unknown flag", []string{"node", "publish", "--bogus"}},
		{"publish: positional argument", []string{"node", "publish", "extra"}},
		{"publish: --publish is not a flag of node publish", []string{"node", "publish", "--publish"}},
		{"publish: --nodes", []string{"node", "publish", "--nodes", "x"}},
		{"publish: duplicate --recover", []string{"node", "publish", "--recover", "--recover"}},
		{"publish: --recover=maybe", []string{"node", "publish", "--recover=maybe"}},
		{"publish: --abandon without --recover", []string{"node", "publish", "--abandon"}},
		{"publish: --abandon=false --recover is fine but --abandon alone is not", []string{"node", "publish", "--abandon=true"}},
		{"publish: malformed --min-version", []string{"node", "publish", "--min-version", "0.10.0"}},
		{"publish: --help with other arguments", []string{"node", "publish", "--help", "--recover"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := installFakePublishRuntime(t, publishRuntimeOpts{})
			before := f.snapshot()
			var out, errb bytes.Buffer
			code := run(tc.args, &out, &errb)
			if code != 2 {
				t.Fatalf("code = %d, want 2 (stdout=%q stderr=%q)", code, out.String(), errb.String())
			}
			if ev := f.log.list(); len(ev) != 0 {
				t.Errorf("usage error must not reach the runtime (root check, role, locks, network): %v", ev)
			}
			if len(f.fetcher.calls) != 0 {
				t.Errorf("network reached: %v", f.fetcher.calls)
			}
			sameSnapshots(t, before, f.snapshot())
		})
	}
}

func TestNodePublishHelp(t *testing.T) {
	f := installFakePublishRuntime(t, publishRuntimeOpts{})
	var out, errb bytes.Buffer
	if code := run([]string{"node", "publish", "--help"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d (%s)", code, errb.String())
	}
	for _, want := range []string{"publish", "--recover", "--abandon", "--min-version"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help output lacks %q: %s", want, out.String())
		}
	}
	out.Reset()
	if code := run([]string{"node", "publish", "-h"}, &out, &errb); code != 0 {
		t.Fatalf("-h: code = %d", code)
	}
	if ev := f.log.list(); len(ev) != 0 {
		t.Errorf("help touched the runtime: %v", ev)
	}
}

// --- mode detection: --name --publish is a value, not a mode (spec §4.5) ---

func TestNodePublishModeDetection(t *testing.T) {
	f := installFakePublishRuntime(t, publishRuntimeOpts{})
	var out, errb bytes.Buffer
	code := run([]string{"node", "add", "--nodes", f.nodesPath, "--name", "--publish", "--server", "203.0.113.5", "--port", "443",
		"--password", "p", "--obfs-password", "o", "--sni", "x.example.com"}, &out, &errb)
	// compat (legacy) behavior: "--publish" is the value of --name -- a legal
	// legacy node name -- so the legacy command registers a node called
	// "--publish", prints its usual line and no role line, and never touches
	// the publish runtime (no root check, role detection, lock or network)
	if code != 0 {
		t.Fatalf("code = %d, want the legacy 0 (stderr=%q)", code, errb.String())
	}
	if out.String() != "added node \"--publish\"\n" {
		t.Errorf("legacy stdout = %q", out.String())
	}
	if errb.Len() != 0 || strings.Contains(errb.String(), "role") {
		t.Errorf("a compat-mode command must not print a role line: %q", errb.String())
	}
	if ev := f.log.list(); len(ev) != 0 {
		t.Errorf("compat mode reached the publish runtime: %v", ev)
	}
	if got := f.nodeNames(); got != "A,B,--publish" {
		t.Errorf("legacy add did not run: nodes = %s", got)
	}
	if len(f.fetcher.calls) != 0 {
		t.Errorf("network reached: %v", f.fetcher.calls)
	}
	// the files a publish would write are untouched by the compat command
	if _, err := os.Stat(filepath.Join(f.stateDir, "publish")); !os.IsNotExist(err) {
		t.Errorf("compat mode created a transaction area: %v", err)
	}
}

// Legacy CRUD, with its own parsing, output, .bak and --nodes semantics, is
// untouched by the new code paths.
func TestNodeLegacyCRUDStillLegacy(t *testing.T) {
	f := installFakePublishRuntime(t, publishRuntimeOpts{})
	withOverriddenDefaultNodesPath(t, filepath.Join(f.base, "not-this-one.conf"))
	var out, errb bytes.Buffer
	code := run([]string{"node", "disable", "A", "--nodes", f.nodesPath}, &out, &errb)
	if code != 0 {
		t.Fatalf("code = %d (%s)", code, errb.String())
	}
	if out.String() != "node \"A\" enabled=false\n" {
		t.Errorf("legacy stdout changed: %q", out.String())
	}
	if errb.Len() != 0 {
		t.Errorf("legacy stderr gained output: %q", errb.String())
	}
	if _, err := os.Stat(f.nodesPath + ".bak"); err != nil {
		t.Errorf("legacy CRUD must keep its .bak: %v", err)
	}
	if ev := f.log.list(); len(ev) != 0 {
		t.Errorf("legacy CRUD touched the publish runtime: %v", ev)
	}
}

// --- full flows through the CLI ---

func TestNodePublishAddFromNodeMachineOutput(t *testing.T) {
	f := installFakePublishRuntime(t, publishRuntimeOpts{})
	cmd := realitynode.AddCommand(realitynode.RegisterParams{Server: "203.0.113.9", ListenPort: 443, UUID: cliUUID, PublicKey: cliPubKey, ShortID: "ab12", SNI: "www.microsoft.com", Name: "jp1"})
	var out, errb bytes.Buffer
	code := run(proxyctlArgs(t, cmd), &out, &errb)
	if code != 0 {
		t.Fatalf("code = %d (stderr=%s)", code, errb.String())
	}
	if got := f.nodeNames(); got != "A,B,jp1" {
		t.Errorf("nodes.conf has %s", got)
	}
	clash, _ := os.ReadFile(filepath.Join(f.tokenDir, "clash.yaml"))
	if !strings.Contains(string(clash), "jp1") {
		t.Error("clash.yaml was not republished with the new node")
	}
	// stdout carries the report, stderr the role line (spec §4.1)
	if strings.Contains(out.String(), "role") {
		t.Errorf("stdout has a role line: %q", out.String())
	}
	if !strings.Contains(errb.String(), "subscription center role: yes") {
		t.Errorf("stderr lacks the role line: %q", errb.String())
	}
	if !strings.Contains(out.String(), "jp1") {
		t.Errorf("stdout = %q", out.String())
	}
	// order: root, role, (precheck), L1+L2, role again, then the engine
	if got := f.log.String(); got != "runtime,geteuid,role,newEnv,lock:L1+L2,role" {
		t.Errorf("runtime events = %s", got)
	}
	// the same command twice is a safe republish
	out.Reset()
	errb.Reset()
	if code := run(proxyctlArgs(t, cmd), &out, &errb); code != 0 {
		t.Fatalf("repeat: code = %d (%s)", code, errb.String())
	}
	if got := f.nodeNames(); got != "A,B,jp1" {
		t.Errorf("repeat changed the node list: %s", got)
	}
}

func TestNodePublishPasteTemplatesAllWork(t *testing.T) {
	f := installFakePublishRuntime(t, publishRuntimeOpts{})
	run1 := func(cmd string, want int) {
		t.Helper()
		var out, errb bytes.Buffer
		if code := run(proxyctlArgs(t, cmd), &out, &errb); code != want {
			t.Fatalf("%s: code = %d, want %d (%s)", cmd, code, want, errb.String())
		}
	}
	run1(realitynode.DisableCommand("A"), 0)
	if n, _ := nodes.Find(f.loadNodes(), "A"); f.loadNodes()[n].Enabled {
		t.Error("A still enabled")
	}
	run1(realitynode.DisableCommand("A"), 0) // idempotent
	run1(realitynode.EnableCommand("A"), 0)
	run1(realitynode.EnableCommand("A"), 0)
	run1(realitynode.RemoveCommand("B"), 0)
	run1(realitynode.RemoveCommand("B"), 0) // already gone
	if got := f.nodeNames(); got != "A" {
		t.Errorf("nodes = %s", got)
	}
	run1(realitynode.DisableCommand("A"), 1) // the last enabled node
	run1(realitynode.RemoveCommand("A"), 1)
	run1(realitynode.EnableCommand("ghost"), 1)
	if got := f.nodeNames(); got != "A" {
		t.Errorf("rejections changed nodes: %s", got)
	}
}

func TestNodePublishEditAndRepublish(t *testing.T) {
	f := installFakePublishRuntime(t, publishRuntimeOpts{})
	var out, errb bytes.Buffer
	if code := run([]string{"node", "edit", "--publish", "A", "--port", "8443", "--sni", "new.example.com"}, &out, &errb); code != 0 {
		t.Fatalf("edit: code = %d (%s)", code, errb.String())
	}
	n := f.loadNodes()[0]
	if n.Port != 8443 || n.SNI != "new.example.com" {
		t.Errorf("edit did not apply: %+v", n)
	}
	// flags may follow or precede the node name
	out.Reset()
	if code := run([]string{"node", "edit", "--publish", "--port", "9443", "A"}, &out, &errb); code != 0 {
		t.Fatalf("edit (flag first): code = %d (%s)", code, errb.String())
	}
	if f.loadNodes()[0].Port != 9443 {
		t.Error("flag-before-name edit did not apply")
	}
	// a reality flag on a hysteria2 node is refused, nothing changes
	before := f.snapshot()
	errb.Reset()
	if code := run([]string{"node", "edit", "--publish", "A", "--uuid", cliUUID}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1 (%s)", code, errb.String())
	}
	sameSnapshots(t, before, f.snapshot())

	// node publish
	out.Reset()
	errb.Reset()
	if code := run([]string{"node", "publish", "--min-version", "v0.10.0"}, &out, &errb); code != 0 {
		t.Fatalf("publish: code = %d (%s)", code, errb.String())
	}
	if !strings.Contains(out.String(), "published") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestNodePublishPreconditionsExit1(t *testing.T) {
	t.Run("not root", func(t *testing.T) {
		f := installFakePublishRuntime(t, publishRuntimeOpts{euid: 1000})
		before := f.snapshot()
		var out, errb bytes.Buffer
		if code := run([]string{"node", "publish"}, &out, &errb); code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
		if got := f.log.String(); got != "runtime,geteuid" {
			t.Errorf("events = %s: a non-root run must stop at the root check (no role detection, no lock)", got)
		}
		sameSnapshots(t, before, f.snapshot())
	})
	for _, st := range []role.Status{role.StatusNo, role.StatusUncertain} {
		st := st
		t.Run("role not yes", func(t *testing.T) {
			f := installFakePublishRuntime(t, publishRuntimeOpts{roles: []role.Status{st}})
			before := f.snapshot()
			var out, errb bytes.Buffer
			if code := run([]string{"node", "publish"}, &out, &errb); code != 1 {
				t.Fatalf("code = %d, want 1", code)
			}
			if strings.Contains(f.log.String(), "lock:") {
				t.Errorf("a refused role must not take locks: %s", f.log)
			}
			sameSnapshots(t, before, f.snapshot())
		})
	}
	t.Run("role changes between the quick check and the locks", func(t *testing.T) {
		f := installFakePublishRuntime(t, publishRuntimeOpts{roles: []role.Status{role.StatusYes, role.StatusNo}})
		before := f.snapshot()
		var out, errb bytes.Buffer
		if code := run([]string{"node", "publish"}, &out, &errb); code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
		sameSnapshots(t, before, f.snapshot())
		if len(f.fetcher.calls) != 0 {
			t.Errorf("network reached after a failed recheck: %v", f.fetcher.calls)
		}
	})
	t.Run("--min-version above this build: rejected before any lock", func(t *testing.T) {
		f := installFakePublishRuntime(t, publishRuntimeOpts{})
		before := f.snapshot()
		var out, errb bytes.Buffer
		if code := run([]string{"node", "publish", "--min-version", "v9.0.0"}, &out, &errb); code != 1 {
			t.Fatalf("code = %d, want 1 (%s)", code, errb.String())
		}
		if strings.Contains(f.log.String(), "lock:") {
			t.Errorf("locks taken for a rejected version: %s", f.log)
		}
		if !strings.Contains(errb.String(), "v9.0.0") {
			t.Errorf("stderr = %q", errb.String())
		}
		sameSnapshots(t, before, f.snapshot())
	})
	t.Run("dev build with --min-version", func(t *testing.T) {
		f := installFakePublishRuntime(t, publishRuntimeOpts{})
		f.env.ProxyctlVersion = "dev"
		var out, errb bytes.Buffer
		if code := run([]string{"node", "publish", "--min-version", "v0.10.0"}, &out, &errb); code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
	})
	t.Run("legacy nodes.conf", func(t *testing.T) {
		f := installFakePublishRuntime(t, publishRuntimeOpts{})
		writeMode(t, f.nodesPath, []byte("A|203.0.113.5|443|pw|ob|www.example.com\n"), 0600)
		before := f.snapshot()
		var out, errb bytes.Buffer
		if code := run([]string{"node", "publish"}, &out, &errb); code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
		if !strings.Contains(errb.String(), "migrate") {
			t.Errorf("stderr = %q, want the migrate hint", errb.String())
		}
		sameSnapshots(t, before, f.snapshot())
	})
	t.Run("lock acquisition fails", func(t *testing.T) {
		f := installFakePublishRuntime(t, publishRuntimeOpts{acquireErr: errors.New("timed out")})
		before := f.snapshot()
		var out, errb bytes.Buffer
		if code := run([]string{"node", "publish"}, &out, &errb); code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
		if !strings.Contains(errb.String(), "another operation") {
			t.Errorf("stderr = %q", errb.String())
		}
		sameSnapshots(t, before, f.snapshot())
	})
	t.Run("signal during the lock wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		block := make(chan struct{})
		f := installFakePublishRuntime(t, publishRuntimeOpts{acquireBlock: block, ctx: ctx, cancel: cancel})
		before := f.snapshot()
		done := make(chan int, 1)
		go func() {
			var out, errb bytes.Buffer
			done <- run([]string{"node", "publish"}, &out, &errb)
		}()
		<-block // acquire has been entered
		cancel()
		if code := <-done; code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
		sameSnapshots(t, before, f.snapshot())
	})
}

func TestNodePublishExitCodeMapping(t *testing.T) {
	t.Run("HTTPS failure rolls back: exit 4", func(t *testing.T) {
		f := installFakePublishRuntime(t, publishRuntimeOpts{})
		f.fetcher.status["clash.yaml"] = 503
		before := f.snapshot()
		var out, errb bytes.Buffer
		code := run(realityAddArgs(), &out, &errb)
		if code != 4 {
			t.Fatalf("code = %d, want 4 (%s)", code, errb.String())
		}
		if !strings.Contains(errb.String(), "rolled back") {
			t.Errorf("stderr = %q", errb.String())
		}
		// mtimes legitimately differ for restored files: compare content only
		after := f.snapshot()
		for k, v := range before {
			if a := after[k]; stripTime(a) != stripTime(v) {
				t.Errorf("%s not restored: %q vs %q", k, v, a)
			}
		}
		if out.Len() != 0 {
			t.Errorf("a rolled-back publish printed to stdout: %q", out.String())
		}
	})
	t.Run("exit 5 names the preserved transaction area", func(t *testing.T) {
		f := installFakePublishRuntime(t, publishRuntimeOpts{})
		f.fetcher.status["clash.yaml"] = 503
		f.env.Validate = func(en []nodes.Node) (subscriptionvalidate.Rendered, error) { return subscriptionvalidate.Validate(en) }
		// an external edit between our write and the rollback makes the
		// rollback refuse (tamper)
		f.fetcher.errs["sr.txt"] = nil
		old := f.env.Fetcher
		f.env.Fetcher = fetcherThatTampers{inner: old, path: filepath.Join(f.tokenDir, "clash.yaml")}
		var out, errb bytes.Buffer
		code := run([]string{"node", "publish"}, &out, &errb)
		if code != 5 {
			t.Fatalf("code = %d, want 5 (%s)", code, errb.String())
		}
		if !strings.Contains(errb.String(), filepath.Join(f.stateDir, "publish")) {
			t.Errorf("stderr must name the preserved area: %q", errb.String())
		}
		// and a later publish says how to proceed
		f.env.Fetcher = old
		errb.Reset()
		if code := run([]string{"node", "publish"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "--recover") {
			t.Fatalf("publish with a leftover: code = %d stderr=%q, want 1 pointing at --recover", code, errb.String())
		}
	})
}

type fetcherThatTampers struct {
	inner publish.Fetcher
	path  string
}

func (f fetcherThatTampers) Fetch(ctx context.Context, domain, urlPath string) (publish.FetchResult, error) {
	os.WriteFile(f.path, []byte("external edit"), 0644)
	return publish.FetchResult{Status: 503}, nil
}

func stripTime(s string) string {
	if i := strings.LastIndex(s, " 20"); i > 0 {
		return s[:i]
	}
	return s
}

// --- recover ---

func TestNodePublishRecover(t *testing.T) {
	t.Run("nothing to recover is exit 0", func(t *testing.T) {
		f := installFakePublishRuntime(t, publishRuntimeOpts{})
		var out, errb bytes.Buffer
		if code := run([]string{"node", "publish", "--recover"}, &out, &errb); code != 0 {
			t.Fatalf("code = %d (%s)", code, errb.String())
		}
		if got := f.log.String(); got != "runtime,geteuid,role,newEnv,lock:L1+L2,role" {
			t.Errorf("recover events = %s", got)
		}
	})
	t.Run("--abandon with nothing to abandon is exit 1", func(t *testing.T) {
		installFakePublishRuntime(t, publishRuntimeOpts{})
		var out, errb bytes.Buffer
		if code := run([]string{"node", "publish", "--recover", "--abandon"}, &out, &errb); code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
	})
	t.Run("recovers a leftover empty area and a tombstone", func(t *testing.T) {
		f := installFakePublishRuntime(t, publishRuntimeOpts{})
		os.Mkdir(filepath.Join(f.stateDir, "publish"), 0700)
		tomb := filepath.Join(f.stateDir, ".publish.tomb-abc")
		os.MkdirAll(filepath.Join(tomb, "snap"), 0700)
		var out, errb bytes.Buffer
		if code := run([]string{"node", "publish", "--recover"}, &out, &errb); code != 0 {
			t.Fatalf("code = %d (%s)", code, errb.String())
		}
		for _, p := range []string{filepath.Join(f.stateDir, "publish"), tomb} {
			if _, err := os.Lstat(p); !os.IsNotExist(err) {
				t.Errorf("%s still exists", p)
			}
		}
	})
	t.Run("recover needs root and the subscription center role", func(t *testing.T) {
		f := installFakePublishRuntime(t, publishRuntimeOpts{euid: 1000})
		var out, errb bytes.Buffer
		if code := run([]string{"node", "publish", "--recover"}, &out, &errb); code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
		if strings.Contains(f.log.String(), "lock:") {
			t.Errorf("locks taken without root: %s", f.log)
		}
	})
}

// `proxyctl validate` prints exactly what it always printed: the shared
// validation package supplies the text, the CLI the prefix (spec: "旧CLI
// validate输出不变").
func TestValidateOutputMatchesSharedValidation(t *testing.T) {
	cases := map[string][]nodes.Node{}
	bad := cliHy2("GitHub") // reserved policy-group name
	cases["reserved name"] = []nodes.Node{bad}
	for name, ns := range cases {
		dir := t.TempDir()
		p := filepath.Join(dir, "nodes.conf")
		writeMode(t, p, []byte(nodes.Serialize(ns)), 0644)
		var out, errb bytes.Buffer
		if code := run([]string{"validate", "--nodes", p}, &out, &errb); code != 1 {
			t.Fatalf("%s: code = %d", name, code)
		}
		_, err := subscriptionvalidate.Validate(nodes.Enabled(ns))
		if err == nil || errb.String() != "validation failed: "+err.Error()+"\n" {
			t.Errorf("%s: stderr = %q, shared validation says %v", name, errb.String(), err)
		}
	}
	// and a good file still passes with the same single line
	p := filepath.Join(t.TempDir(), "nodes.conf")
	writeMode(t, p, []byte(nodes.Serialize([]nodes.Node{cliHy2("OK")})), 0644)
	var out, errb bytes.Buffer
	if code := run([]string{"validate", "--nodes", p}, &out, &errb); code != 0 || out.String() != "validation passed\n" {
		t.Fatalf("code = %d out=%q err=%q", code, out.String(), errb.String())
	}
}
