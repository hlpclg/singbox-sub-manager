// Package publish implements design spec §10: the subscription center's
// recoverable publication of node changes. A publish -- `proxyctl node
// publish`, or `node add|edit|enable|disable|remove --publish` -- replaces
// nodes.conf, clash.yaml and sr.txt as one journaled transaction on the same
// engine the Reality node lifecycle uses (internal/transaction: one journal
// schema, one set of step primitives, one judgement and recovery routine).
// Each file is replaced atomically on its own, the transaction as a whole is
// recoverable, and no claim of atomicity across the two subscription
// documents is made (§10.5).
//
// Layering, as for internal/realitynode: this package never takes a lock and
// never talks to root/role detection -- cmd/proxyctl orchestrates those
// (root -> role -> Precheck -> L1+L2 -> role again -> Run). Run repeats
// Precheck itself, so the "recheck after the locks" of §10.3 step 2 cannot be
// forgotten by a caller.
//
// Once a transaction has begun, Run has exactly one way to finish: persist
// the progress, then hand the journal to transaction.Recoverer
// (RecoverDetached, so a SIGINT/SIGTERM can never interrupt a rollback or a
// post-commit cleanup, §13.3). The engine alone decides between "nothing was
// modified" (exit 1), "rolled back" (exit 4) and "committed"; this file never
// re-derives that decision. Recovery of a transaction a previous process left
// behind (`publish --recover`) is the same Recoverer over the same journal
// (RunRecover).
package publish

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/hlpclg/singbox-sub-manager/internal/health"
	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
	"github.com/hlpclg/singbox-sub-manager/internal/render"
	"github.com/hlpclg/singbox-sub-manager/internal/subscriptionvalidate"
	"github.com/hlpclg/singbox-sub-manager/internal/transaction"
)

// Fixed names of the publish transaction area and its tombstones (spec
// §10.3), siblings of the token file under the state directory.
const (
	TxnDirName      = "publish"
	TombstonePrefix = ".publish.tomb-"

	stagingPrefix = ".proxyctl-publish-"
	rootKeyNodes  = "etc"
	rootKeySub    = "www"

	nodesFileMode os.FileMode = 0600
	subFileMode   os.FileMode = 0644
	stagingMode   os.FileMode = 0700
	stagedMode    os.FileMode = 0600

	// verifyTimeout is spec §13.2's per-file HTTPS budget.
	verifyTimeout = 10 * time.Second

	subCaddyUser = "caddy"
)

var (
	nodesFileName = "nodes.conf"
	subFileNames  = [2]string{"clash.yaml", "sr.txt"}
)

// publishHook is a process-global test seam, mirroring internal/transaction's
// stepHook/persistHook and internal/realitynode's lifecycleHook: a no-op in
// production, and a test overrides it to inject a failure or a hard crash
// (os.Exit) at a named point of the orchestration. Tests must not use
// t.Parallel while overriding it.
var publishHook = func(point string) error { return nil }

// TimeoutFunc derives a context bounded by d from parent; production wraps
// context.WithTimeout. Tests inject a fake that records d (proving the spec
// constant is requested) and substitutes a short-lived context so no test
// waits ten real seconds.
type TimeoutFunc func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc)

// Env bundles every dependency of a publish so the default test suite touches
// no real /etc, /var, network or user database.
type Env struct {
	// InstallJSON, NodesConf and StateDir default (when empty) to the fixed
	// production paths of spec §4.2/§10.3/§18.1.
	InstallJSON string
	NodesConf   string
	StateDir    string

	// TrustedOwnerUID/GID: every trusted root must be owned by this uid, and
	// nodes.conf and the staging area are written as this owner. 0/0 in
	// production, the current euid/egid in a non-root test.
	TrustedOwnerUID int
	TrustedOwnerGID int

	// LookupFileOwner supplies the owner of a subscription file that did not
	// exist before this publish (spec §10.3 step 6: "前状态不存在时用
	// caddy:caddy"). nil resolves the "caddy" user.
	LookupFileOwner func() (uid, gid uint32, err error)

	Fetcher Fetcher
	// NewTimeout / FileVerifyTimeout bound each file's HTTPS verification;
	// FileVerifyTimeout zero means the 10 s of spec §13.2.
	NewTimeout        TimeoutFunc
	FileVerifyTimeout time.Duration

	// Validate replaces subscriptionvalidate.Validate (tests only).
	Validate func(enabled []nodes.Node) (subscriptionvalidate.Rendered, error)

	// RollbackBudget overrides the independent 120 s rollback budget (spec
	// §13.2) of every Recoverer this Env builds.
	RollbackBudget time.Duration

	Now      func() time.Time
	NewTxnID func() string

	// ProxyctlVersion is the compiled-in Version, the left-hand side of the
	// --min-version comparison.
	ProxyctlVersion string
}

// ProdEnv returns the production Env for the given proxyctl version.
func ProdEnv(version string) *Env {
	return &Env{ProxyctlVersion: version}
}

func (e *Env) installJSON() string {
	if e.InstallJSON != "" {
		return e.InstallJSON
	}
	return health.ProdDefaults().InstallJSON
}

func (e *Env) nodesConf() string {
	if e.NodesConf != "" {
		return e.NodesConf
	}
	return "/etc/singbox-sub-manager/nodes.conf"
}

func (e *Env) stateDir() string {
	if e.StateDir != "" {
		return e.StateDir
	}
	return "/var/lib/singbox-sub-manager"
}

func (e *Env) fetcher() Fetcher {
	if e.Fetcher != nil {
		return e.Fetcher
	}
	return LoopbackFetcher{}
}

func (e *Env) timeoutFunc() TimeoutFunc {
	if e.NewTimeout != nil {
		return e.NewTimeout
	}
	return func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		return context.WithTimeout(parent, d)
	}
}

func (e *Env) fileVerifyTimeout() time.Duration {
	if e.FileVerifyTimeout > 0 {
		return e.FileVerifyTimeout
	}
	return verifyTimeout
}

func (e *Env) validate(enabled []nodes.Node) (subscriptionvalidate.Rendered, error) {
	if e.Validate != nil {
		return e.Validate(enabled)
	}
	return subscriptionvalidate.Validate(enabled)
}

func (e *Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

var txnIDRE = regexp.MustCompile(`^[0-9A-Za-z]{1,64}$`)

func (e *Env) newTxnID() string {
	if e.NewTxnID != nil {
		return e.NewTxnID()
	}
	var b [8]byte
	_, _ = io.ReadFull(rand.Reader, b[:])
	return hex.EncodeToString(b[:])
}

func (e *Env) ownerLookup() func() (uint32, uint32, error) {
	if e.LookupFileOwner != nil {
		return e.LookupFileOwner
	}
	return lookupCaddy
}

func lookupCaddy() (uint32, uint32, error) {
	u, err := user.Lookup(subCaddyUser)
	if err != nil {
		return 0, 0, fmt.Errorf("cannot resolve the %q user that owns newly created subscription files: %w", subCaddyUser, err)
	}
	uid, uerr := strconv.ParseUint(u.Uid, 10, 32)
	gid, gerr := strconv.ParseUint(u.Gid, 10, 32)
	if uerr != nil || gerr != nil {
		return 0, 0, fmt.Errorf("the %q user has a non-numeric uid/gid", subCaddyUser)
	}
	return uint32(uid), uint32(gid), nil
}

// --- version rule (spec §3) ---

// ParseVersion parses "vMAJOR.MINOR.PATCH" (digits only, each segment a
// non-overflowing number). Anything else -- "dev", "v0.10", "0.10.0",
// "v0.10.0-rc1" -- is rejected.
func ParseVersion(s string) ([3]uint64, error) {
	var v [3]uint64
	if !strings.HasPrefix(s, "v") {
		return v, fmt.Errorf("%q is not of the form vMAJOR.MINOR.PATCH", s)
	}
	parts := strings.Split(s[1:], ".")
	if len(parts) != 3 {
		return v, fmt.Errorf("%q is not of the form vMAJOR.MINOR.PATCH", s)
	}
	for i, p := range parts {
		if p == "" {
			return v, fmt.Errorf("%q is not of the form vMAJOR.MINOR.PATCH", s)
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return v, fmt.Errorf("%q is not of the form vMAJOR.MINOR.PATCH", s)
			}
		}
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return v, fmt.Errorf("%q has a version segment that is too large", s)
		}
		v[i] = n
	}
	return v, nil
}

// ValidMinVersion checks a --min-version value (a parse-time check: a
// malformed requirement is a usage error).
func ValidMinVersion(s string) error {
	_, err := ParseVersion(s)
	return err
}

// CheckMinVersion implements "不低于" (spec §3): the segments are compared
// numerically, left to right, so v0.10.10 is newer than v0.10.9. A local
// version that is not itself vMAJOR.MINOR.PATCH -- a development build -- can
// never prove compatibility and fails.
func CheckMinVersion(have, min string) error {
	want, err := ParseVersion(min)
	if err != nil {
		return fmt.Errorf("invalid --min-version: %w", err)
	}
	got, err := ParseVersion(have)
	if err != nil {
		return fmt.Errorf("this proxyctl build (%q) is not a release version and cannot prove it satisfies --min-version %s", have, min)
	}
	for i := 0; i < 3; i++ {
		if got[i] > want[i] {
			return nil
		}
		if got[i] < want[i] {
			return fmt.Errorf("this proxyctl is %s but %s or newer is required (--min-version); upgrade it first", have, min)
		}
	}
	return nil
}

// --- mutations (spec §10.3 step 3) ---

// MutationKind names the node change a publish carries.
type MutationKind string

const (
	// KindPublish changes no node: `node publish` only republishes.
	KindPublish MutationKind = "publish"
	KindAdd     MutationKind = "add"
	KindEdit    MutationKind = "edit"
	KindEnable  MutationKind = "enable"
	KindDisable MutationKind = "disable"
	KindRemove  MutationKind = "remove"
)

// Edit lists the fields an `edit` changes; nil means "leave as is".
type Edit struct {
	Name, Server, SNI, Password, ObfsPassword, UUID, PublicKey, ShortID *string
	Port                                                                *int
	Enabled                                                             *bool
}

// Mutation is one requested change. Name is the target of edit, enable,
// disable and remove; Node is the node an add registers.
type Mutation struct {
	Kind MutationKind
	Name string
	Node nodes.Node
	Edit Edit
}

func effType(t nodes.NodeType) nodes.NodeType {
	if t == "" {
		return nodes.TypeHysteria2
	}
	return t
}

// sameFields compares two nodes ignoring Enabled and treating the empty type
// as hysteria2.
func sameFields(a, b nodes.Node) bool {
	a.Enabled, b.Enabled = false, false
	a.Type, b.Type = effType(a.Type), effType(b.Type)
	return a == b
}

func normalized(n nodes.Node) nodes.Node {
	n.Type = effType(n.Type)
	return n
}

// ApplyMutation computes the new node list in memory and reports whether
// nodes.conf must change. It implements spec §10.3 step 3's matrix: a
// repeated add of an identical enabled node, enable of an enabled node,
// disable of a disabled node and remove of a missing node are idempotent
// (changed == false, republish only); a disabled or differing same-name add,
// and edit/enable/disable of a missing node, are rejected. cur is never
// modified.
func ApplyMutation(cur []nodes.Node, m Mutation) (next []nodes.Node, changed bool, err error) {
	same := append([]nodes.Node(nil), cur...)
	switch m.Kind {
	case KindPublish:
		return same, false, nil

	case KindAdd:
		name := m.Node.Name
		if idx, ok := nodes.Find(cur, name); ok {
			existing := cur[idx]
			switch {
			case !sameFields(existing, m.Node):
				return nil, false, fmt.Errorf("node %q already exists with different fields; use `proxyctl node edit` to change it", name)
			case !existing.Enabled:
				return nil, false, fmt.Errorf("node %q is already registered but disabled; use `proxyctl node enable` to enable it", name)
			case !m.Node.Enabled:
				return nil, false, fmt.Errorf("node %q is already registered and enabled; use `proxyctl node disable` to disable it", name)
			}
			return same, false, nil
		}
		out, err := nodes.Add(cur, m.Node)
		if err != nil {
			return nil, false, err
		}
		return out, true, nil

	case KindEdit:
		idx, ok := nodes.Find(cur, m.Name)
		if !ok {
			return nil, false, fmt.Errorf("node %q not found", m.Name)
		}
		n := cur[idx]
		e := m.Edit
		switch effType(n.Type) {
		case nodes.TypeVlessReality:
			if e.Password != nil || e.ObfsPassword != nil {
				return nil, false, errors.New("--password/--obfs-password are not allowed on a vless-reality node")
			}
		default:
			if e.UUID != nil || e.PublicKey != nil || e.ShortID != nil {
				return nil, false, errors.New("--uuid/--public-key/--short-id are not allowed on a hysteria2 node")
			}
		}
		setStr := func(dst *string, v *string) {
			if v != nil {
				*dst = *v
			}
		}
		setStr(&n.Name, e.Name)
		setStr(&n.Server, e.Server)
		setStr(&n.SNI, e.SNI)
		setStr(&n.Password, e.Password)
		setStr(&n.ObfsPassword, e.ObfsPassword)
		setStr(&n.UUID, e.UUID)
		setStr(&n.PublicKey, e.PublicKey)
		setStr(&n.ShortID, e.ShortID)
		if e.Port != nil {
			n.Port = *e.Port
		}
		if e.Enabled != nil {
			n.Enabled = *e.Enabled
		}
		out, err := nodes.Replace(cur, m.Name, n)
		if err != nil {
			return nil, false, err
		}
		return out, normalized(n) != normalized(cur[idx]), nil

	case KindEnable, KindDisable:
		want := m.Kind == KindEnable
		idx, ok := nodes.Find(cur, m.Name)
		if !ok {
			return nil, false, fmt.Errorf("node %q not found", m.Name)
		}
		if cur[idx].Enabled == want {
			return same, false, nil
		}
		out, err := nodes.SetEnabled(cur, m.Name, want)
		if err != nil {
			return nil, false, err
		}
		return out, true, nil

	case KindRemove:
		if _, ok := nodes.Find(cur, m.Name); !ok {
			return same, false, nil
		}
		out, err := nodes.Remove(cur, m.Name)
		if err != nil {
			return nil, false, err
		}
		return out, true, nil
	}
	return nil, false, fmt.Errorf("unknown publish mutation %q", m.Kind)
}

// --- layout and prechecks (spec §10.3 step 1) ---

// Layout is the standard deployment this publish works on: domain, token and
// subscription root come from install.json (§18.1), nodes.conf and the state
// directory are the fixed standard paths. Nothing else is read from
// anywhere.
type Layout struct {
	Domain           string
	SubscriptionRoot string
	Token            string
	NodesConf        string
	StateDir         string
}

func (l Layout) nodesRootPath() string { return filepath.Dir(filepath.Dir(l.NodesConf)) }
func (l Layout) nodesComp() string     { return filepath.Base(filepath.Dir(l.NodesConf)) }
func (l Layout) subRootPath() string   { return filepath.Dir(l.SubscriptionRoot) }
func (l Layout) subComp() string       { return filepath.Base(l.SubscriptionRoot) }
func (l Layout) tokenDir() string      { return filepath.Join(l.SubscriptionRoot, l.Token) }
func (l Layout) txnPath() string       { return filepath.Join(l.StateDir, TxnDirName) }

func (l Layout) nodesRel() string { return l.nodesComp() + "/" + filepath.Base(l.NodesConf) }
func (l Layout) subRel(name string) string {
	return l.subComp() + "/" + l.Token + "/" + name
}

var domainRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$`)

func validComponent(c string) bool {
	return c != "" && c != "." && c != ".." && c != "/" && !strings.ContainsRune(c, '/') && !strings.ContainsRune(c, 0)
}

// LoadLayout reads install.json and the token file it points to. It never
// falls back to the Caddyfile or to defaults: a publish that cannot identify
// the deployment must refuse, not guess (spec §10.2).
func LoadLayout(env *Env) (Layout, error) {
	lay := Layout{NodesConf: env.nodesConf(), StateDir: env.stateDir()}
	data, err := os.ReadFile(env.installJSON())
	if err != nil {
		return lay, fmt.Errorf("cannot read install.json (%s): %w", env.installJSON(), err)
	}
	var st struct {
		Domain           string `json:"domain"`
		SubscriptionRoot string `json:"subscription_root"`
		TokenFile        string `json:"token_file"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return lay, fmt.Errorf("install.json is not valid JSON: %w", err)
	}
	if st.Domain == "" || len(st.Domain) > 253 || !domainRE.MatchString(st.Domain) {
		return lay, errors.New("install.json has no usable domain")
	}
	if !filepath.IsAbs(st.SubscriptionRoot) {
		return lay, errors.New("install.json subscription_root is not an absolute path")
	}
	lay.SubscriptionRoot = filepath.Clean(st.SubscriptionRoot)
	if !validComponent(filepath.Base(lay.SubscriptionRoot)) {
		return lay, fmt.Errorf("install.json subscription_root %q is not a directory below a trusted parent", st.SubscriptionRoot)
	}
	if st.TokenFile == "" {
		return lay, errors.New("install.json has no token_file")
	}
	tok, err := os.ReadFile(st.TokenFile)
	if err != nil {
		return lay, errors.New("the subscription token file named by install.json is unreadable")
	}
	lay.Token = strings.TrimSpace(string(tok))
	if !health.ValidToken(lay.Token) {
		return lay, errors.New("the subscription token file does not hold a valid token")
	}
	lay.Domain = st.Domain
	return lay, nil
}

// Plan is Precheck's read-only result.
type Plan struct {
	Layout Layout
	// Nodes is nodes.conf as it is now (sectioned format).
	Nodes []nodes.Node
	// NodesSHA is the SHA-256 of the bytes nodes.conf had when it was read.
	NodesSHA string
}

func lstatNoLink(path, what string) (os.FileInfo, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s (%s) is a symbolic link; refusing to publish through it", what, path)
	}
	return fi, nil
}

func requirePublishDir(path, what string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s %s does not exist; re-run the installer (install-proxy.sh install) to create it -- publish never creates it", what, path)
		}
		return fmt.Errorf("%s %s: %w", what, path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s (%s) is a symbolic link; refusing to publish through it", what, path)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s %s is not a directory; re-run the installer (install-proxy.sh install)", what, path)
	}
	return nil
}

// Precheck is spec §10.3 step 1 (everything that can be decided without
// modifying anything): the version requirement, the standard layout, the
// sectioned nodes.conf, the publish directory, the absence of a leftover
// transaction and the trustworthiness of the three trusted roots. cmd runs it
// before taking the locks (a fast reject) and Run repeats it under them.
func Precheck(env *Env, minVersion string) (*Plan, error) {
	if minVersion != "" {
		if err := CheckMinVersion(env.ProxyctlVersion, minVersion); err != nil {
			return nil, err
		}
	}
	lay, err := LoadLayout(env)
	if err != nil {
		return nil, err
	}

	fi, err := lstatNoLink(lay.NodesConf, "nodes.conf")
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("nodes.conf (%s) is not a regular file", lay.NodesConf)
	}
	ns, format, err := nodes.Load(lay.NodesConf)
	if err != nil {
		return nil, fmt.Errorf("nodes.conf: %w", err)
	}
	switch format {
	case nodes.FormatLegacy:
		return nil, errors.New("legacy nodes.conf detected; run 'proxyctl node migrate' first")
	case nodes.FormatEmpty:
		return nil, errors.New("nodes.conf has no nodes; add one with 'proxyctl node add' first")
	}
	raw, err := os.ReadFile(lay.NodesConf)
	if err != nil {
		return nil, fmt.Errorf("nodes.conf: %w", err)
	}
	sum := sha256.Sum256(raw)

	if err := requirePublishDir(lay.SubscriptionRoot, "subscription root"); err != nil {
		return nil, err
	}
	if err := requirePublishDir(lay.tokenDir(), "publish directory"); err != nil {
		return nil, err
	}
	for _, name := range subFileNames {
		p := filepath.Join(lay.tokenDir(), name)
		fi, err := lstatNoLink(p, name)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("%s (%s) is not a regular file", name, p)
		}
	}

	roots, err := openPublishRoots(env, lay)
	if err != nil {
		return nil, err
	}
	roots.closeAll()

	if err := checkNoLeftover(lay); err != nil {
		return nil, err
	}
	return &Plan{Layout: lay, Nodes: ns, NodesSHA: hex.EncodeToString(sum[:])}, nil
}

func checkNoLeftover(lay Layout) error {
	if _, err := os.Lstat(lay.txnPath()); err == nil {
		return errors.New("a leftover publish transaction exists; run `proxyctl node publish --recover` first")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cannot inspect %s: %w", lay.txnPath(), err)
	}
	if matches, _ := filepath.Glob(filepath.Join(lay.StateDir, TombstonePrefix+"*")); len(matches) > 0 {
		return errors.New("a leftover publish transaction tombstone exists; run `proxyctl node publish --recover` first")
	}
	return nil
}

// --- trusted roots and path resolution ---

type publishRoots struct {
	nodes, sub, state *transaction.TrustedRoot
}

func (r *publishRoots) closeAll() {
	for _, t := range []*transaction.TrustedRoot{r.nodes, r.sub, r.state} {
		if t != nil {
			t.Close()
		}
	}
}

func (r *publishRoots) byKey(key string) *transaction.TrustedRoot {
	switch key {
	case rootKeyNodes:
		return r.nodes
	case rootKeySub:
		return r.sub
	}
	return nil
}

func openPublishRoots(env *Env, lay Layout) (*publishRoots, error) {
	var r publishRoots
	var err error
	if r.nodes, err = transaction.OpenTrustedRoot(lay.nodesRootPath(), env.TrustedOwnerUID); err != nil {
		return nil, fmt.Errorf("nodes.conf's parent %s: %w", lay.nodesRootPath(), err)
	}
	if r.sub, err = transaction.OpenTrustedRoot(lay.subRootPath(), env.TrustedOwnerUID); err != nil {
		r.closeAll()
		return nil, fmt.Errorf("the web root parent %s: %w", lay.subRootPath(), err)
	}
	if r.state, err = transaction.OpenTrustedRoot(lay.StateDir, env.TrustedOwnerUID); err != nil {
		r.closeAll()
		return nil, fmt.Errorf("the state directory %s: %w", lay.StateDir, err)
	}
	return &r, nil
}

func mpFor(roots *publishRoots, rootKey, rel string) transaction.ManagedPath {
	parts := strings.Split(rel, "/")
	comps := parts[:len(parts)-1]
	leaf := parts[len(parts)-1]
	keys := make([]string, len(comps))
	cur := rootKey
	for i, c := range comps {
		cur += "/" + c
		keys[i] = cur
	}
	return transaction.ManagedPath{Root: roots.byKey(rootKey), RootKey: rootKey, Comps: append([]string(nil), comps...), Keys: keys, Leaf: leaf}
}

// resolver maps a journal target key "<rootKey>/<comp>.../<leaf>" to its
// ManagedPath, the shape every v0.10 PathResolver uses.
type resolver struct{ roots *publishRoots }

func (p resolver) Resolve(target string) (transaction.ManagedPath, error) {
	parts := strings.Split(target, "/")
	if len(parts) < 2 {
		return transaction.ManagedPath{}, fmt.Errorf("publish: malformed target %q", target)
	}
	root := p.roots.byKey(parts[0])
	if root == nil {
		return transaction.ManagedPath{}, fmt.Errorf("publish: unknown root key %q in target %q", parts[0], target)
	}
	comps := append([]string(nil), parts[1:len(parts)-1]...)
	keys := make([]string, len(comps))
	cur := parts[0]
	for i, c := range comps {
		cur += "/" + c
		keys[i] = cur
	}
	return transaction.ManagedPath{Root: root, RootKey: parts[0], Comps: comps, Keys: keys, Leaf: parts[len(parts)-1]}, nil
}

// probeDir is the one-time, pre-transaction baseline read that seeds
// Journal.DirIDs (spec §7.1 "事务开始时...登记当前身份"; the same bootstrap
// measurement internal/realitynode documents on its probeIdentity). It
// rejects a symbolic link or a non-directory outright; every later use of the
// directory goes through the Engine, which re-verifies the recorded identity
// through O_NOFOLLOW dirfds immediately before each mutation.
func probeDir(path string) (transaction.DirIdentity, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return transaction.DirIdentity{Exists: false}, nil
		}
		return transaction.DirIdentity{}, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return transaction.DirIdentity{}, fmt.Errorf("%s is a symbolic link", path)
	}
	if !fi.IsDir() {
		return transaction.DirIdentity{}, fmt.Errorf("%s is not a directory", path)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return transaction.DirIdentity{}, fmt.Errorf("cannot read the identity of %s", path)
	}
	return transaction.DirIdentity{Exists: true, Dev: uint64(st.Dev), Ino: uint64(st.Ino)}, nil
}

func bootstrapDirIDs(lay Layout, mps []transaction.ManagedPath) (map[string]transaction.DirIdentity, error) {
	out := map[string]transaction.DirIdentity{}
	for _, mp := range mps {
		base := lay.subRootPath()
		if mp.RootKey == rootKeyNodes {
			base = lay.nodesRootPath()
		}
		for i := range mp.Comps {
			key := mp.Keys[i]
			if _, ok := out[key]; ok {
				continue
			}
			id, err := probeDir(filepath.Join(append([]string{base}, mp.Comps[:i+1]...)...))
			if err != nil {
				return nil, err
			}
			out[key] = id
		}
	}
	return out, nil
}

// --- results ---

// Result is what Run and RunRecover return to cmd/proxyctl. Like
// realitynode.Result it separates "failed before anything managed changed",
// "the engine's outcome" and "needs a human" without error-string matching.
//
//	PreTransaction                      -> 1 (nothing managed was modified)
//	Err != nil (a transaction had begun)-> 5 (recovery could not be confirmed;
//	                                         TxnDir names what is left)
//	Outcome rolled back                 -> 4
//	Outcome committed / Recovered       -> 0
type Result struct {
	PreTransaction bool
	Outcome        transaction.Outcome
	HasOutcome     bool
	Err            error
	// Cause explains why an automatic rollback (or a no-change removal of the
	// transaction area) happened; it never changes the exit code.
	Cause error
	// TxnDir is the transaction area a caller must point the operator at when
	// Err is set after a transaction began (exit 5).
	TxnDir string
	// Summary is the one-line stdout report of a successful publish.
	Summary string
	// Warnings are advisory stderr lines (a failed HTTPS re-verification after
	// a rollback); Notices are informational ones (what recover did).
	Warnings []string
	Notices  []string
	// Recovered/RecoveredOutcome describe a successful recover, which always
	// exits 0 whichever way the leftover was resolved.
	Recovered        bool
	RecoveredOutcome transaction.Outcome
}

func preTxnFail(err error) Result { return Result{PreTransaction: true, Err: err} }

// ExitCode is spec §13.1's exit code for a finished command, from typed
// fields only.
func (r Result) ExitCode() int {
	switch {
	case r.PreTransaction:
		return 1
	case r.Err != nil:
		return 5
	case r.HasOutcome:
		switch r.Outcome {
		case transaction.OutcomeRolledBack:
			return 4
		case transaction.OutcomeNoChange:
			return 1
		}
	}
	return 0
}

// --- Run ---

// Params is one publish request.
type Params struct {
	Mutation Mutation
	// MinVersion is --min-version ("" = none).
	MinVersion string
}

type run struct {
	env        *Env
	lay        Layout
	roots      *publishRoots
	store      *transaction.Store
	engine     *transaction.Engine
	rec        *transaction.Recoverer
	snap       *os.File
	txnID      string
	stagingRel string
	seq        int
	previous   []fileHash // the previous publication's hashes, for the post-rollback re-check
}

func (r *run) nextTemp() string {
	r.seq++
	return transaction.TempName(r.txnID, r.seq)
}

func (r *run) close() {
	if r.snap != nil {
		r.snap.Close()
	}
	if r.store != nil {
		r.store.Close()
	}
	r.roots.closeAll()
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func filePost(data []byte, mode os.FileMode, uid, gid uint32) transaction.PathState {
	return transaction.PathState{Exists: true, Type: transaction.PathTypeFile, Size: int64(len(data)),
		SHA256: hashHex(data), Mode: uint32(mode.Perm()), UID: uid, GID: gid}
}

func absent() transaction.PathState { return transaction.PathState{Exists: false} }

func tombstoneName(txnID string) string { return TombstonePrefix + txnID }

// Run performs one publish (spec §10.3 steps 1-8, §10.4), with the caller
// holding L1 and L2. It repeats Precheck first (step 2's recheck). The
// returned Result maps to the exit codes of §13.1.
func Run(ctx context.Context, env *Env, p Params) Result {
	plan, err := Precheck(env, p.MinVersion)
	if err != nil {
		return preTxnFail(err)
	}
	if err := ctx.Err(); err != nil {
		return preTxnFail(fmt.Errorf("publish cancelled before it started: %w", err))
	}

	next, changed, err := ApplyMutation(plan.Nodes, p.Mutation)
	if err != nil {
		return preTxnFail(err)
	}
	enabled := nodes.Enabled(next)
	if len(enabled) == 0 {
		return preTxnFail(errors.New("no enabled nodes would remain; add or enable another node first (an empty subscription is never published)"))
	}
	// Rendering is a pure function of the enabled nodes: computing the bytes
	// before the transaction starts lets the journal carry every Post state at
	// registration. The validation that gates the publish still runs after
	// staging, in the order of §10.3 step 5.
	clashB := []byte(render.Clash(enabled))
	srB := []byte(render.Shadowrocket(enabled))
	var nodesB []byte
	if changed {
		nodesB = []byte(nodes.Serialize(next))
	}

	r, err := begin(env, plan.Layout)
	if err != nil {
		return preTxnFail(err)
	}
	defer r.close()

	res := r.publish(ctx, plan, p, enabled, nodesB, clashB, srB)
	return res
}

// begin opens the trusted roots and starts the transaction with no steps yet:
// the first journal version carries the schema, the ancestor-directory
// identities and an empty plan. Reading the three files' pre-states has to go
// through the Engine's ancestor-verified, O_NOFOLLOW primitives, which need a
// journal; the full plan is registered (still before any step runs) right
// after, in one journal update.
func begin(env *Env, lay Layout) (*run, error) {
	roots, err := openPublishRoots(env, lay)
	if err != nil {
		return nil, err
	}
	txnID := env.newTxnID()
	if !txnIDRE.MatchString(txnID) {
		roots.closeAll()
		return nil, fmt.Errorf("publish: invalid transaction id %q", txnID)
	}
	r := &run{env: env, lay: lay, roots: roots, txnID: txnID, stagingRel: stagingPrefix + txnID}

	mps := []transaction.ManagedPath{
		mpFor(roots, rootKeyNodes, lay.nodesRel()),
		mpFor(roots, rootKeySub, lay.subRel(subFileNames[0])),
		mpFor(roots, rootKeySub, lay.subRel(subFileNames[1])),
		mpFor(roots, rootKeySub, r.stagingRel),
		mpFor(roots, rootKeySub, r.stagingRel+"/"+subFileNames[0]),
		mpFor(roots, rootKeySub, r.stagingRel+"/"+subFileNames[1]),
	}
	dirIDs, err := bootstrapDirIDs(lay, mps)
	if err != nil {
		roots.closeAll()
		return nil, err
	}
	// the staging directory does not exist yet, and the journal says so
	dirIDs[rootKeySub+"/"+r.stagingRel] = transaction.DirIdentity{Exists: false}

	j := &transaction.Journal{
		Schema: transaction.CurrentSchema, TxnID: txnID, Op: "publish",
		ProxyctlVersion: env.ProxyctlVersion, CreatedAt: env.now(),
		Phase: transaction.PhaseRunning, DirIDs: dirIDs,
		RollbackSystemd: transaction.RollbackSystemdNone,
	}
	store, err := transaction.BeginTransaction(roots.state, TxnDirName, j)
	if err != nil {
		roots.closeAll()
		return nil, err
	}
	r.store = store
	r.engine = transaction.NewEngine(store)
	r.rec = newRecoverer(env, roots, r.engine, txnID, nil)
	return r, nil
}

func newRecoverer(env *Env, roots *publishRoots, engine *transaction.Engine, txnID string, snap *os.File) *transaction.Recoverer {
	return &transaction.Recoverer{
		Engine: engine, Root: roots.state, TombstoneName: tombstoneName(txnID),
		Resolve: resolver{roots: roots}, SnapDir: snap, RollbackBudget: env.RollbackBudget,
	}
}

// wrapUp finishes the transaction after a failure at any point after
// BeginTransaction, always through RecoverDetached (a signal must never cut a
// rollback short). The engine's outcome decides the result; recovery that
// cannot be confirmed is exit 5 with the area named, never 1.
func (r *run) wrapUp(ctx context.Context, cause error) Result {
	_ = publishHook("wrapup")
	outcome, ferr := r.rec.RecoverDetached(ctx)
	if ferr != nil {
		return Result{
			Err:    fmt.Errorf("publish: transaction could not be confirmed recovered (cause: %v): %w", cause, ferr),
			Cause:  cause,
			TxnDir: r.lay.txnPath(),
		}
	}
	res := Result{Outcome: outcome, HasOutcome: true, Cause: cause}
	if outcome == transaction.OutcomeNoChange {
		res.PreTransaction = true
		res.Err = cause
		if res.Err == nil {
			res.Err = errors.New("publish did not complete")
		}
		return res
	}
	// The local state is restored (that alone decides the exit code, §10.4);
	// whether Caddy serves the restored files again is only advisory.
	res.Warnings = r.env.reverify(ctx, r.lay, r.previous)
	return res
}

// commit flips the journal to committed (spec §7.5: the publish commit point
// is the verification having passed) and finishes forward: from here on
// nothing is rolled back, only staging and the transaction area are removed.
func (r *run) commit(ctx context.Context, summary string) Result {
	if err := publishHook("before-commit"); err != nil {
		return r.wrapUp(ctx, err)
	}
	committed := r.store.Journal().Clone()
	committed.Phase = transaction.PhaseCommitted
	if err := r.store.Update(committed); err != nil {
		return r.wrapUp(ctx, fmt.Errorf("commit: %w", err))
	}
	_ = publishHook("after-commit-flip") // committed: nothing may abort the forward cleanup
	outcome, ferr := r.rec.RecoverDetached(ctx)
	if ferr != nil {
		return Result{
			Err:    fmt.Errorf("publish: the subscriptions are published, but the post-commit cleanup did not finish (run `proxyctl node publish --recover` to complete it): %w", ferr),
			TxnDir: r.lay.txnPath(),
		}
	}
	return Result{Outcome: outcome, HasOutcome: true, Summary: summary}
}

func (r *run) step(id string, fn func() error) error {
	if err := publishHook("before:" + id); err != nil {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	return publishHook("after:" + id)
}

func (r *run) mp(rootKey, rel string) transaction.ManagedPath { return mpFor(r.roots, rootKey, rel) }

func (r *run) publish(ctx context.Context, plan *Plan, p Params, enabled []nodes.Node, nodesB, clashB, srB []byte) Result {
	env, lay := r.env, r.lay
	uid, gid := uint32(env.TrustedOwnerUID), uint32(env.TrustedOwnerGID)
	if err := publishHook("began"); err != nil {
		return r.wrapUp(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return r.wrapUp(ctx, err)
	}

	// --- pre-states, read through the engine's O_NOFOLLOW primitives ---
	var nodesPre transaction.PathState
	if nodesB != nil {
		st, err := r.engine.LiveFileState(r.mp(rootKeyNodes, lay.nodesRel()), true, false)
		if err != nil {
			return r.wrapUp(ctx, fmt.Errorf("nodes.conf: %w", err))
		}
		if !st.Exists || st.SHA256 != plan.NodesSHA {
			return r.wrapUp(ctx, errors.New("nodes.conf changed while the publish was being prepared"))
		}
		nodesPre = st
	}
	docs := [2][]byte{clashB, srB}
	var subPre [2]transaction.PathState
	for i, name := range subFileNames {
		st, err := r.engine.LiveFileState(r.mp(rootKeySub, lay.subRel(name)), true, false)
		if err != nil {
			return r.wrapUp(ctx, fmt.Errorf("%s: %w", name, err))
		}
		subPre[i] = st
		if st.Exists {
			r.previous = append(r.previous, fileHash{name: name, sha: st.SHA256})
		}
	}
	var lookedUp bool
	var owner [2]uint32
	subOwner := func(pre transaction.PathState) (uint32, uint32, error) {
		if pre.Exists {
			return pre.UID, pre.GID, nil
		}
		if !lookedUp {
			u, g, err := env.ownerLookup()()
			if err != nil {
				return 0, 0, err
			}
			owner = [2]uint32{u, g}
			lookedUp = true
		}
		return owner[0], owner[1], nil
	}

	// --- the plan (spec §10.3 step 4): every step, its temp names and its
	// pre/post states, registered in one journal update before anything runs
	stageDirT := rootKeySub + "/" + r.stagingRel
	stageDirPost := transaction.PathState{Exists: true, Type: transaction.PathTypeDir, Mode: uint32(stagingMode.Perm()), UID: uid, GID: gid}
	var steps []transaction.Step
	add := func(s transaction.Step) { s.Status = transaction.StatusPending; steps = append(steps, s) }

	add(transaction.Step{ID: "mkdir-staging", Kind: transaction.KindMkdir, Target: stageDirT, Pre: absent(), Post: stageDirPost, Provisional: true})
	var stagePost [2]transaction.PathState
	stageIDs := [2]string{"stage-clash", "stage-sr"}
	for i, name := range subFileNames {
		stagePost[i] = filePost(docs[i], stagedMode, uid, gid)
		add(transaction.Step{ID: stageIDs[i], Kind: transaction.KindWriteFile, Target: stageDirT + "/" + name,
			Pre: absent(), Post: stagePost[i], Temps: []string{r.nextTemp()}, Provisional: true})
	}

	type finalFile struct {
		id, snapID string
		rootKey    string
		rel        string
		pre, post  transaction.PathState
		data       []byte
		tmp, snapT string
	}
	var finals []*finalFile
	if nodesB != nil {
		finals = append(finals, &finalFile{id: "write-nodes", snapID: "snap-nodes", rootKey: rootKeyNodes, rel: lay.nodesRel(),
			pre: nodesPre, post: filePost(nodesB, nodesFileMode, uid, gid), data: nodesB})
	}
	for i, name := range subFileNames {
		fuid, fgid, err := subOwner(subPre[i])
		if err != nil {
			return r.wrapUp(ctx, err)
		}
		finals = append(finals, &finalFile{id: "write-" + strings.TrimSuffix(name, filepath.Ext(name)), snapID: "snap-" + strings.TrimSuffix(name, filepath.Ext(name)),
			rootKey: rootKeySub, rel: lay.subRel(name), pre: subPre[i], post: filePost(docs[i], subFileMode, fuid, fgid), data: docs[i]})
	}
	for _, f := range finals {
		f.tmp = r.nextTemp()
		if f.pre.Exists {
			f.snapT = r.nextTemp()
			add(transaction.Step{ID: f.snapID, Kind: transaction.KindSnapshot, Target: f.rootKey + "/" + f.rel, Pre: f.pre, Post: f.pre, Temps: []string{f.snapT}})
		}
	}
	for _, f := range finals {
		add(transaction.Step{ID: f.id, Kind: transaction.KindWriteFile, Target: f.rootKey + "/" + f.rel, Pre: f.pre, Post: f.post, Temps: []string{f.tmp}})
	}
	// forward cleanup, registered up front like every other step (§7.5)
	unstageIDs := [2]string{"unstage-clash", "unstage-sr"}
	for i, name := range subFileNames {
		add(transaction.Step{ID: unstageIDs[i], Kind: transaction.KindRemoveFile, Target: stageDirT + "/" + name, Pre: stagePost[i], Post: absent(), Provisional: true})
	}
	add(transaction.Step{ID: "rmdir-staging", Kind: transaction.KindRemoveDir, Target: stageDirT, Pre: stageDirPost, Post: absent(), Provisional: true})

	nj := r.store.Journal().Clone()
	nj.Steps = steps
	if err := r.store.Update(nj); err != nil {
		return r.wrapUp(ctx, fmt.Errorf("register the publish plan: %w", err))
	}
	if err := publishHook("registered"); err != nil {
		return r.wrapUp(ctx, err)
	}

	// --- steps 5: staging, render, validate ---
	if err := r.step("mkdir-staging", func() error {
		f, err := r.engine.Mkdir(ctx, "mkdir-staging", r.mp(rootKeySub, r.stagingRel))
		if f != nil {
			f.Close()
		}
		return err
	}); err != nil {
		return r.wrapUp(ctx, err)
	}
	for i, name := range subFileNames {
		i, name := i, name
		if err := r.step(stageIDs[i], func() error {
			tmp := steps[1+i].Temps[0]
			return r.engine.WriteFile(ctx, stageIDs[i], r.mp(rootKeySub, r.stagingRel+"/"+name), tmp, docs[i])
		}); err != nil {
			return r.wrapUp(ctx, err)
		}
	}
	// The validated documents must be the ones that were staged.
	for i, name := range subFileNames {
		st, err := r.engine.LiveFileState(r.mp(rootKeySub, r.stagingRel+"/"+name), true, false)
		if err != nil || st != stagePost[i] {
			return r.wrapUp(ctx, fmt.Errorf("staged %s does not match what was rendered: %v", name, err))
		}
	}
	rendered, err := env.validate(enabled)
	if err != nil {
		return r.wrapUp(ctx, fmt.Errorf("validation failed: %w", err))
	}
	if rendered.Clash != string(clashB) || rendered.Shadowrocket != string(srB) {
		return r.wrapUp(ctx, errors.New("validation failed: the validated documents differ from the staged ones"))
	}
	if err := publishHook("validated"); err != nil {
		return r.wrapUp(ctx, err)
	}

	// --- snapshots of the files that exist (§10.3 step 5, last bullet) ---
	if err := publishHook("mkdir-snap"); err != nil {
		return r.wrapUp(ctx, err)
	}
	snap, err := mkTxnSubdir(r.store.Dir(), "snap", 0700)
	if err != nil {
		return r.wrapUp(ctx, err)
	}
	r.snap = snap
	r.rec.SnapDir = snap
	for _, f := range finals {
		f := f
		if !f.pre.Exists {
			continue
		}
		if err := r.step(f.snapID, func() error {
			return r.engine.Snapshot(ctx, f.snapID, r.mp(f.rootKey, f.rel), snap, f.snapT)
		}); err != nil {
			return r.wrapUp(ctx, err)
		}
	}

	// --- step 6: nodes.conf, clash.yaml, sr.txt, in that order, each an
	// atomic write_file in the target's own directory ---
	for _, f := range finals {
		f := f
		if err := r.step(f.id, func() error {
			return r.engine.WriteFile(ctx, f.id, r.mp(f.rootKey, f.rel), f.tmp, f.data)
		}); err != nil {
			return r.wrapUp(ctx, err)
		}
	}

	// --- step 7: local read-back, then HTTPS ---
	want := make([]fileHash, 0, 2)
	for i, name := range subFileNames {
		stagedNow, err := r.engine.LiveFileState(r.mp(rootKeySub, r.stagingRel+"/"+name), true, false)
		if err != nil {
			return r.wrapUp(ctx, fmt.Errorf("staged %s: %w", name, err))
		}
		live, err := r.engine.LiveFileState(r.mp(rootKeySub, lay.subRel(name)), true, false)
		if err != nil {
			return r.wrapUp(ctx, fmt.Errorf("%s: %w", name, err))
		}
		if live.SHA256 != stagedNow.SHA256 || live.SHA256 != hashHex(docs[i]) {
			return r.wrapUp(ctx, fmt.Errorf("read-back of the published %s does not match the staged copy", name))
		}
		want = append(want, fileHash{name: name, sha: stagedNow.SHA256})
	}
	if err := publishHook("verified-local"); err != nil {
		return r.wrapUp(ctx, err)
	}
	if err := env.verifyHTTPS(ctx, lay, want); err != nil {
		return r.wrapUp(ctx, err)
	}
	if err := publishHook("verified-https"); err != nil {
		return r.wrapUp(ctx, err)
	}

	return r.commit(ctx, summaryOf(p.Mutation, nodesB != nil, len(enabled)))
}

func summaryOf(m Mutation, nodesChanged bool, enabled int) string {
	var what string
	switch m.Kind {
	case KindAdd:
		what = fmt.Sprintf("added node %q", m.Node.Name)
	case KindEdit:
		what = fmt.Sprintf("updated node %q", m.Name)
	case KindEnable:
		what = fmt.Sprintf("enabled node %q", m.Name)
	case KindDisable:
		what = fmt.Sprintf("disabled node %q", m.Name)
	case KindRemove:
		what = fmt.Sprintf("removed node %q", m.Name)
	default:
		what = "republished the subscription"
	}
	if m.Kind != KindPublish && !nodesChanged {
		what = fmt.Sprintf("node %q is already in the requested state", targetName(m))
	}
	return fmt.Sprintf("%s; published %d enabled node(s) to clash.yaml and sr.txt (verified over HTTPS)", what, enabled)
}

func targetName(m Mutation) string {
	if m.Kind == KindAdd {
		return m.Node.Name
	}
	return m.Name
}

// mkTxnSubdir creates and opens a plain subdirectory of the transaction area
// itself (snap/), which spec §7.3 excludes from the tamper judgement.
func mkTxnSubdir(txnDir *os.File, name string, mode os.FileMode) (*os.File, error) {
	if err := unix.Mkdirat(int(txnDir.Fd()), name, uint32(mode.Perm())); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, fmt.Errorf("publish: mkdir %s in the transaction area: %w", name, err)
	}
	fd, err := unix.Openat(int(txnDir.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("publish: open %s in the transaction area: %w", name, err)
	}
	return os.NewFile(uintptr(fd), name), nil
}

// --- recover (spec §10.4, §7.4-§7.6) ---

// RecoverParams is `node publish --recover [--abandon]`'s input.
type RecoverParams struct {
	Abandon    bool
	MinVersion string
}

// PrecheckRecover is the part of recovery that does not touch the host: the
// version requirement and the identification of the deployment (the layout
// the journal's targets are resolved against).
func PrecheckRecover(env *Env, p RecoverParams) (Layout, error) {
	if p.MinVersion != "" {
		if err := CheckMinVersion(env.ProxyctlVersion, p.MinVersion); err != nil {
			return Layout{}, err
		}
	}
	return LoadLayout(env)
}

func recovered(o transaction.Outcome, notices ...string) Result {
	return Result{Recovered: true, RecoveredOutcome: o, Notices: notices}
}

func needsAttention(path string, err error) Result { return Result{Err: err, TxnDir: path} }

// RunRecover recovers a leftover publish transaction, with the caller holding
// L1 and L2. It is the same Recoverer the forward path finishes through: a
// leftover tombstone is deleted wholesale without being read (§7.6); an area
// with no journal.json is discarded only when it holds nothing but
// journal.json.tmp; otherwise the journal is resumed -- rolled back, or, for a
// committed one, only its forward cleanup finished. Success is exit 0;
// anything the engine cannot confirm (tampering, an unknown schema, a
// timeout) is exit 5 naming the preserved area. After a local rollback the
// restored files are re-checked over HTTPS, advisory only. With nothing left
// over, recover succeeds without doing anything; --abandon with nothing to
// abandon is refused (exit 1).
func RunRecover(ctx context.Context, env *Env, p RecoverParams) Result {
	lay, err := PrecheckRecover(env, p)
	if err != nil {
		return preTxnFail(err)
	}
	if err := ctx.Err(); err != nil {
		return preTxnFail(fmt.Errorf("recover cancelled before it started: %w", err))
	}
	roots, err := openPublishRoots(env, lay)
	if err != nil {
		return preTxnFail(err)
	}
	defer roots.closeAll()

	var notices []string
	tombs, _ := filepath.Glob(filepath.Join(lay.StateDir, TombstonePrefix+"*"))
	for _, tomb := range tombs {
		if err := transaction.DeleteTombstone(roots.state, filepath.Base(tomb)); err != nil {
			return needsAttention(tomb, fmt.Errorf("publish: delete tombstone %s: %w", tomb, err))
		}
		notices = append(notices, "deleted the leftover tombstone "+tomb+" (a finished transaction's remains; its contents were not read)")
	}

	if _, err := os.Lstat(lay.txnPath()); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return needsAttention(lay.txnPath(), err)
		}
		switch {
		case p.Abandon:
			return preTxnFail(errors.New("there is no unfinished publish transaction to abandon"))
		case len(tombs) > 0:
			return recovered(transaction.OutcomeCommitted, notices...)
		}
		return recovered(transaction.OutcomeNoChange, "there is no unfinished publish transaction")
	}
	switch err := transaction.RecoverEmptyTransactionArea(roots.state, TxnDirName); {
	case err == nil:
		return recovered(transaction.OutcomeNoChange, append(notices, "removed a transaction area with no journal: the crash happened while the transaction was starting, so nothing was modified")...)
	case errors.Is(err, transaction.ErrJournalPresent):
		// a journal exists: resume it below
	default:
		return needsAttention(lay.txnPath(), err)
	}
	_ = publishHook("recover:opening")

	store, err := transaction.OpenTransaction(roots.state, TxnDirName)
	if err != nil {
		return needsAttention(lay.txnPath(), fmt.Errorf("publish: cannot open the leftover transaction: %w", err))
	}
	defer store.Close()
	engine := transaction.NewEngine(store)
	var snap *os.File
	if fd, oerr := unix.Openat(int(store.Dir().Fd()), "snap", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0); oerr == nil {
		snap = os.NewFile(uintptr(fd), "snap")
		defer snap.Close()
	}
	j := store.Journal()
	rec := newRecoverer(env, roots, engine, j.TxnID, snap)

	if p.Abandon {
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), transaction.DefaultRollbackBudget)
		defer cancel()
		outcome, err := rec.Abandon(actx)
		switch {
		case err == nil:
			return recovered(outcome, notices...)
		case errors.Is(err, transaction.ErrAbandonNotEligible):
			return preTxnFail(err)
		default:
			return needsAttention(lay.txnPath(), err)
		}
	}

	previous := previousHashes(lay, j)
	outcome, err := rec.RecoverDetached(ctx)
	if err != nil {
		return needsAttention(lay.txnPath(), fmt.Errorf("publish: recover did not finish: %w", err))
	}
	res := recovered(outcome, notices...)
	switch outcome {
	case transaction.OutcomeRolledBack:
		res.Notices = append(res.Notices, "rolled the interrupted publish back to its pre-transaction state")
		res.Warnings = env.reverify(ctx, lay, previous)
	case transaction.OutcomeCommitted:
		res.Notices = append(res.Notices, "the interrupted publish had already committed; finished its forward cleanup")
	case transaction.OutcomeNoChange:
		res.Notices = append(res.Notices, "the interrupted publish had modified nothing; its transaction area was removed")
	}
	return res
}

// previousHashes extracts, from a journal's plan, the hashes the two
// subscription files had before the transaction (the first step of each
// file's chain), for the advisory re-check after a rollback.
func previousHashes(lay Layout, j *transaction.Journal) []fileHash {
	var out []fileHash
	for _, name := range subFileNames {
		chain := transaction.PathChain(j.Steps, rootKeySub+"/"+lay.subRel(name))
		if len(chain) == 0 {
			continue
		}
		if pre := chain[0].Pre; pre.Exists && pre.SHA256 != "" {
			out = append(out, fileHash{name: name, sha: pre.SHA256})
		}
	}
	return out
}
