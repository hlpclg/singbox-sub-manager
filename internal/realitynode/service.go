// Package realitynode: service.go implements design spec §8.1 (install),
// §8.2 (reinstall) and §8.3 (show) — Task 7's instance lifecycle, built
// entirely on internal/transaction's already-reviewed journal, safe-path
// primitives and recovery engine (Task 4/5), this package's own download/
// config/systemd/selfcheck building blocks (Task 6), and its read-only
// state/identity helpers (Task 2). Task 8 adds uninstall, uninstall --purge
// (with export.go's --export-backup) and recover [--abandon] (§8.4-§8.6) at
// the end of this file, on the very same planner/Recoverer machinery.
//
// Every forward step this file executes is first appended to the
// transaction's journal in "pending" status and persisted (via the shared
// Store, spec §7.1's "先登记后执行"), then executed through the Engine
// (which itself drives pending->started->done and re-verifies the
// ancestor chain immediately before every mutation, spec §7.1/§7.2). This
// file never bypasses that discipline: it never writes a managed path
// directly, and the only filesystem access outside the Engine's own
// primitives is (a) the one-time bootstrap read of already-existing
// ancestor directories' identity, needed to seed Journal.DirIDs before any
// step can run against them (documented on probeIdentity below), and (b)
// plain, non-managed access to the transaction area's own internal dl/ and
// client/ subdirectories (spec §7.3: "事务区内部...不参与篡改判定").
//
// Once a transaction begins, this file has exactly one way to finish,
// success or failure: persist whatever progress was made, then call
// transaction.Recoverer.Recover (or RecoverDetached, under cancellation).
// Recover already implements every one of spec §7.4's cases from the
// journal's own recorded phase alone (nothing has been modified yet ->
// OutcomeNoChange; something was -> roll back -> OutcomeRolledBack; this
// call itself reached the commit point -> OutcomeCommitted) — this file
// never re-derives that decision itself.
package realitynode

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/hlpclg/singbox-sub-manager/internal/transaction"
)

// SystemdFull is every systemd operation Install/Reinstall/Show need: the
// six methods transaction.Systemd requires for recovery, plus ActiveState/
// MainPID (the §9.2 gate) and Start (§8.1 step 6's initial start).
// realitynode.Systemctl (systemd.go) implements it.
type SystemdFull interface {
	transaction.Systemd
	ActiveState(ctx context.Context, unit string) (string, error)
	MainPID(ctx context.Context, unit string) (int, error)
	Start(ctx context.Context, unit string) error
}

// Env bundles every dependency Install/Reinstall/Show need, all
// injectable so the default test suite touches no real root filesystem,
// systemd, network or clock (spec §14.1: "systemd、下载器、外部命令执行
// 器、HTTP客户端和时钟都通过接口注入").
type Env struct {
	Roots Roots
	FS    StateFS

	// TrustedOwnerUID/TrustedOwnerGID are the uid/gid every trusted root
	// (and every pre-existing asset directory this file bootstraps
	// identity for) must be owned by, and the owner every asset this
	// package creates is written as: 0/0 in production, the current euid/
	// egid in a non-root test (plan: "非root测试用临时目录作可信根，属主取
	// 当前uid"). They are two separate fields (not one, reused for both)
	// because a non-root test process's uid and gid are not generally
	// numerically equal, unlike production's root:root.
	TrustedOwnerUID int
	TrustedOwnerGID int

	Rand     io.Reader        // crypto/rand.Reader in production (identity generation)
	Now      func() time.Time // time.Now in production
	NewTxnID func() string    // random lowercase-hex txn id; nil uses a crypto/rand-backed default

	GOOS, GOARCH      string
	SystemdAvailable  func() bool
	OSRelease         func() (id, versionID string, err error)
	UFWActive         func() (bool, error) // best-effort: err means "could not tell", not fatal
	CheckDiskSpace    func(path string, minBytes uint64) error
	CheckPortBindable func(port int) (ok bool, detail string) // net.Listen("tcp4","0.0.0.0:<port>")

	Downloader Downloader
	Runner     CommandRunner
	Systemd    SystemdFull
	Proc       ProcReader
	Clock      Clock

	InterfaceLister InterfaceLister
	Resolver        Resolver
	PortRange       PortRangeReader
	PortBind        PortBinder
	RandIntn        RandIntn

	TLSDialer TLSDialer
	// DataPath supplies every DataPathConfig field that does NOT vary per
	// call (Listen, FreePort, HTTPClientFactory, RandBytes, EgressURL,
	// ClientStartGrace, Occupant, DestDialer, NewTimeout) — production
	// leaves it zero-valued, letting DataPathConfig's own accessor methods
	// (selfcheck.go) fall back to real implementations; tests set these
	// fields directly. Install/Reinstall copy it and fill in the per-call
	// fields (ListenPort, SelfcheckPort, SNI, UUID, PrivateKey, ShortID,
	// ClientBinaryPath, ClientConfigDir, ClientConfigName).
	DataPath DataPathConfig

	NewTimeout TimeoutFunc

	GatePollBudget   time.Duration
	GatePollInterval time.Duration

	// RollbackBudget overrides the independent 120 s rollback budget (spec
	// §13.2) of every Recoverer this Env builds (forward failure paths and
	// recover alike); zero means the spec's 120 s. Tests inject a short one so
	// a hung systemctl ends in exit 5 without a real two-minute wait.
	RollbackBudget time.Duration

	// ProxyctlVersion is the compiled-in proxyctl Version, used by Show
	// (spec §8.3) only for its own diagnostics; the actual mismatch hint
	// compares manifest.SingboxCurrent against PinnedVersion.
	ProxyctlVersion string

	// PinnedVersion/PinnedSHA256ForArch override this package's compiled-in
	// SingboxVersion/PinnedSHA256 (download.go) — the "target version" a
	// plain install, and a reinstall with no explicit --singbox-version/
	// --singbox-sha256, download and verify against. Both are nil/empty in
	// production, which is what makes Install/Reinstall use the real
	// compiled-in pinned release; tests inject a fake version/hash pair
	// matching a locally-built fake "sing-box" so the full download-verify-
	// write sequence can run against a real (if fake) binary without
	// network access or the real ~40MB release asset (spec §14.1's
	// "下载器...可注入" extends naturally to which release is pinned, since
	// nothing else about §6/§8.1's mechanics depends on the specific
	// version or hash value).
	PinnedVersion       string
	PinnedSHA256ForArch func(goarch string) (string, bool)
}

func (e *Env) pinnedVersion() string {
	if e.PinnedVersion != "" {
		return e.PinnedVersion
	}
	return SingboxVersion
}

func (e *Env) pinnedSHA256(goarch string) (string, bool) {
	if e.PinnedSHA256ForArch != nil {
		return e.PinnedSHA256ForArch(goarch)
	}
	return PinnedSHA256(goarch)
}

func (e *Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Env) newTxnID() string {
	if e.NewTxnID != nil {
		return e.NewTxnID()
	}
	var b [8]byte
	_, _ = io.ReadFull(rand.Reader, b[:])
	return hex.EncodeToString(b[:])
}

func (e *Env) randReader() io.Reader {
	if e.Rand != nil {
		return e.Rand
	}
	return rand.Reader
}

// --- Outcome / result types ---

// Result is what Install/Reinstall/Show return to the CLI layer (Task 7's
// own cmd/proxyctl/node_service.go). It distinguishes "failed before any
// managed path was modified", "engine-reported outcome" and "needs manual
// intervention" without the caller parsing error text, per the plan's
// "内部命令结果区分修改前失败、已恢复、需介入，不靠错误字符串映射".
//
// Exit-code contract (spec §13.1), applied by cmd/proxyctl:
//   - PreTransaction: exit 1 (nothing managed was modified; includes a
//     transaction that was opened and then removed with OutcomeNoChange).
//   - Err != nil && !PreTransaction: exit 5 — a transaction had begun and
//     this process could not confirm it was recovered (tamper, gate,
//     timeout, or any other Recover failure); TxnDir names what is left.
//   - HasOutcome: OutcomeRolledBack -> 4, OutcomeCommitted -> 0.
//   - none of the above: plain success (Show), exit 0.
type Result struct {
	PreTransaction bool
	Outcome        transaction.Outcome
	HasOutcome     bool
	Err            error
	// Cause explains why an automatic rollback (or a no-change removal of
	// the transaction area) happened; it never changes the exit code.
	Cause error
	// TxnDir is the transaction area path a caller must point the operator
	// at when Err is set after a transaction began (exit 5).
	TxnDir string
	// Stdout is the paste-able command block (spec §10.1) to print on a
	// successful (OutcomeCommitted) install/reinstall, or Show's report.
	Stdout string
	// Warnings are stderr-only lines (egress probe failure, ufw active,
	// security-group reminder, version-mismatch reinstall hint) that never
	// change the exit code.
	Warnings []string
	// Notices are informational stderr lines printed on success (unlike
	// Warnings, without a "warning:" prefix): what uninstall left alone, what
	// recover did.
	Notices []string
	// Recovered/RecoveredOutcome describe a successful `recover`: it always
	// exits 0 whichever way the leftover transaction was resolved, so the
	// engine's Outcome is reported here rather than in Outcome/HasOutcome
	// (whose RolledBack means exit 4).
	Recovered        bool
	RecoveredOutcome transaction.Outcome
}

func preTxnFail(err error) Result { return Result{PreTransaction: true, Err: err} }

// ExitCode is spec §13.1's process exit code for a finished command, decided
// purely from Result's typed fields (never from error text). It is the
// single mapping cmd/proxyctl applies, exported so the lifecycle tests can
// assert exit codes at the level the spec states them.
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

// --- trusted roots and path resolution ---

const (
	rootKeyEtc     = "etc"
	rootKeyVarLib  = "var_lib"
	rootKeyUsrLib  = "usr_local_lib"
	rootKeySystemd = "systemd"
)

type openRoots struct {
	etc, varLib, usrLib, systemd *transaction.TrustedRoot
}

func (o *openRoots) closeAll() {
	for _, r := range []*transaction.TrustedRoot{o.etc, o.varLib, o.usrLib, o.systemd} {
		if r != nil {
			r.Close()
		}
	}
}

func (o *openRoots) byKey(key string) *transaction.TrustedRoot {
	switch key {
	case rootKeyEtc:
		return o.etc
	case rootKeyVarLib:
		return o.varLib
	case rootKeyUsrLib:
		return o.usrLib
	case rootKeySystemd:
		return o.systemd
	default:
		return nil
	}
}

func openAllRoots(roots Roots, wantOwner int) (*openRoots, error) {
	etcPath := filepath.Dir(roots.ConfigDir)
	varLibPath := filepath.Dir(roots.StateDir)
	usrLibPath := filepath.Dir(roots.BinDir)
	systemdPath := filepath.Dir(roots.SystemdUnit)

	var o openRoots
	var err error
	if o.etc, err = transaction.OpenTrustedRoot(etcPath, wantOwner); err != nil {
		return nil, fmt.Errorf("open trusted root %s: %w", etcPath, err)
	}
	if o.varLib, err = transaction.OpenTrustedRoot(varLibPath, wantOwner); err != nil {
		o.closeAll()
		return nil, fmt.Errorf("open trusted root %s: %w", varLibPath, err)
	}
	if o.usrLib, err = transaction.OpenTrustedRoot(usrLibPath, wantOwner); err != nil {
		o.closeAll()
		return nil, fmt.Errorf("open trusted root %s: %w", usrLibPath, err)
	}
	if o.systemd, err = transaction.OpenTrustedRoot(systemdPath, wantOwner); err != nil {
		o.closeAll()
		return nil, fmt.Errorf("open trusted root %s: %w", systemdPath, err)
	}
	return &o, nil
}

// pathResolver maps a journal target key ("<rootKey>/<comp1>/.../<leaf>")
// to a transaction.ManagedPath, resolved against openRoots — the same key
// shape as every other v0.10 PathResolver (see recResolver in
// internal/transaction's own tests).
type pathResolver struct{ roots *openRoots }

func (p pathResolver) Resolve(target string) (transaction.ManagedPath, error) {
	parts := strings.Split(target, "/")
	if len(parts) < 2 {
		return transaction.ManagedPath{}, fmt.Errorf("realitynode: malformed target %q", target)
	}
	root := p.roots.byKey(parts[0])
	if root == nil {
		return transaction.ManagedPath{}, fmt.Errorf("realitynode: unknown root key %q in target %q", parts[0], target)
	}
	comps := append([]string(nil), parts[1:len(parts)-1]...)
	leaf := parts[len(parts)-1]
	keys := make([]string, len(comps))
	cur := parts[0]
	for i, c := range comps {
		cur = cur + "/" + c
		keys[i] = cur
	}
	return transaction.ManagedPath{Root: root, RootKey: parts[0], Comps: comps, Keys: keys, Leaf: leaf}, nil
}

func mpFor(roots *openRoots, rootKey, relPath string) transaction.ManagedPath {
	parts := strings.Split(relPath, "/")
	comps := parts[:len(parts)-1]
	leaf := parts[len(parts)-1]
	keys := make([]string, len(comps))
	cur := rootKey
	for i, c := range comps {
		cur = cur + "/" + c
		keys[i] = cur
	}
	return transaction.ManagedPath{Root: roots.byKey(rootKey), RootKey: rootKey, Comps: append([]string(nil), comps...), Keys: keys, Leaf: leaf}
}

func targetKey(rootKey, relPath string) string { return rootKey + "/" + relPath }

// probeIdentity is the one-time, pre-transaction bootstrap read that seeds
// Journal.DirIDs for an ancestor directory that may already exist (spec
// §7.1: "事务开始时...为每个受管路径的每一级祖先目录登记当前身份；当时不
// 存在的目录登记为不存在"). It is deliberately the only place in this
// file that inspects a path by string rather than through the Engine's own
// dir_ids-verified resolution: dir_ids's very first value has nothing
// prior to compare against, by construction (this measurement *is* the
// baseline). Every subsequent read or mutation of the same directory
// during the transaction goes through the Engine, which re-verifies this
// recorded identity from scratch (via O_NOFOLLOW dirfd resolution,
// spec §7.1) immediately before acting — so a symlink swap or directory
// replacement between this probe and the transaction's actual use of the
// path is still caught as tampering (ErrAncestorMismatch / ErrTampered),
// exactly the same "尽力检测,不承诺阻断" boundary spec §7.1 documents for
// the general ancestor-replacement race.
func probeIdentity(path string, wantOwner int) (transaction.DirIdentity, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return transaction.DirIdentity{Exists: false}, nil
		}
		return transaction.DirIdentity{}, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return transaction.DirIdentity{}, fmt.Errorf("realitynode: %s is a symbolic link", path)
	}
	if !fi.IsDir() {
		return transaction.DirIdentity{}, fmt.Errorf("realitynode: %s is not a directory", path)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return transaction.DirIdentity{}, fmt.Errorf("realitynode: cannot read owner/identity of %s", path)
	}
	if int(st.Uid) != wantOwner {
		return transaction.DirIdentity{}, fmt.Errorf("realitynode: %s is owned by uid %d, want %d", path, st.Uid, wantOwner)
	}
	if st.Mode&(syscall.S_IWGRP|syscall.S_IWOTH) != 0 {
		return transaction.DirIdentity{}, fmt.Errorf("realitynode: %s is writable by group or other", path)
	}
	return transaction.DirIdentity{Exists: true, Dev: uint64(st.Dev), Ino: uint64(st.Ino)}, nil
}

// bootstrapDirIDs computes probeIdentity for every ancestor level of every
// mp in mps (deduplicated), returning the map ready to seed a fresh
// Journal.DirIDs.
func bootstrapDirIDs(roots Roots, mps []transaction.ManagedPath, wantOwner int) (map[string]transaction.DirIdentity, error) {
	etcPath := filepath.Dir(roots.ConfigDir)
	varLibPath := filepath.Dir(roots.StateDir)
	usrLibPath := filepath.Dir(roots.BinDir)
	systemdPath := filepath.Dir(roots.SystemdUnit)
	base := func(rootKey string) string {
		switch rootKey {
		case rootKeyEtc:
			return etcPath
		case rootKeyVarLib:
			return varLibPath
		case rootKeyUsrLib:
			return usrLibPath
		case rootKeySystemd:
			return systemdPath
		}
		return ""
	}

	out := map[string]transaction.DirIdentity{}
	for _, mp := range mps {
		for i := range mp.Comps {
			key := mp.Keys[i]
			if _, ok := out[key]; ok {
				continue
			}
			full := filepath.Join(append([]string{base(mp.RootKey)}, mp.Comps[:i+1]...)...)
			id, err := probeIdentity(full, wantOwner)
			if err != nil {
				return nil, err
			}
			out[key] = id
		}
	}
	return out, nil
}

// --- temp name sequencing ---

// seq is a monotonically increasing counter this file uses to name every
// temp file it registers (spec §7.3's fixed ".proxyctl-txn-<txn_id>-<seq>"
// format via transaction.TempName), scoped to a single operation.
type seq struct {
	txnID string
	n     int
}

func (s *seq) next() string {
	s.n++
	return transaction.TempName(s.txnID, s.n)
}

// --- shared plan/execution state ---

// planner holds everything Install/Reinstall need while incrementally
// building and executing the journal: the open Store/Engine (sharing one
// underlying journal, spec's "先登记后执行"), the resolver, the open
// trusted roots, and a temp-name sequence. Every append*/do* method
// persists the journal via store.Update *before* invoking the Engine, so a
// crash between the two always leaves the step at worst "pending" (safe:
// nothing was attempted) rather than executed-but-unregistered.
type planner struct {
	env    *Env
	store  *transaction.Store
	engine *transaction.Engine
	roots  *openRoots
	seq    seq
	// snapDir is the transaction area's own snap/ directory, opened lazily
	// (only reinstall's doOverwriteFile needs it — fresh install never
	// overwrites an existing managed path).
	snapDir *os.File
}

func (p *planner) journal() *transaction.Journal { return p.store.Journal() }

// appendStep persists a new pending step onto the journal's tail.
func (p *planner) appendStep(s transaction.Step) error {
	next := p.journal().Clone()
	next.Steps = append(next.Steps, s)
	return p.store.Update(next)
}

// appendSteps persists several new pending steps in one journal update.
func (p *planner) appendSteps(steps ...transaction.Step) error {
	next := p.journal().Clone()
	next.Steps = append(next.Steps, steps...)
	return p.store.Update(next)
}

// markSystemdStarted/markSystemdDone transition a systemd_* step directly
// via the Store (the Engine has no public method for systemd kinds — spec
// §7.2's systemd rows have no multi-phase syscall sequence of their own to
// drive: the real systemd call itself is this step's only "primitive").
func (p *planner) markSystemdStarted(id string) error {
	next := p.journal().Clone()
	for i := range next.Steps {
		if next.Steps[i].ID == id {
			next.Steps[i].Status = transaction.StatusStarted
		}
	}
	return p.store.Update(next)
}

func (p *planner) markSystemdDone(id string) error {
	next := p.journal().Clone()
	for i := range next.Steps {
		if next.Steps[i].ID == id {
			next.Steps[i].Status = transaction.StatusDone
		}
	}
	return p.store.Update(next)
}

// runSystemdStep appends a pending systemd_* step, marks it started,
// invokes call (the real systemd operation), then marks it done — spec
// §7.2's systemd rows have no multi-phase syscall sequence of their own:
// the real call itself is the step's only primitive.
func (p *planner) runSystemdStep(ctx context.Context, id string, kind transaction.StepKind, target string, pre, post transaction.PathState, call func(context.Context) error) error {
	if err := p.appendStep(transaction.Step{ID: id, Kind: kind, Target: target, Pre: pre, Post: post, Status: transaction.StatusPending}); err != nil {
		return err
	}
	if err := lifecycleHook("reg:" + id); err != nil {
		return err
	}
	if err := p.markSystemdStarted(id); err != nil {
		return err
	}
	if err := lifecycleHook("started:" + id); err != nil {
		return err
	}
	if err := call(ctx); err != nil {
		return err
	}
	if err := lifecycleHook("called:" + id); err != nil {
		return err
	}
	if err := p.markSystemdDone(id); err != nil {
		return err
	}
	return lifecycleHook("done:" + id)
}

// doMkdir appends then executes a mkdir step, returning its open dir fd
// (caller closes).
func (p *planner) doMkdir(ctx context.Context, id, rootKey, relPath string, mode os.FileMode, uid, gid uint32, preExists bool) (*os.File, error) {
	mp := mpFor(p.roots, rootKey, relPath)
	pre := transaction.PathState{Exists: false}
	if preExists {
		pre = transaction.PathState{Exists: true, Type: transaction.PathTypeDir}
	}
	post := transaction.PathState{Exists: true, Type: transaction.PathTypeDir, Mode: uint32(mode.Perm()), UID: uid, GID: gid}
	if err := p.appendStep(transaction.Step{ID: id, Kind: transaction.KindMkdir, Target: mp.TargetKey(), Pre: pre, Post: post, Status: transaction.StatusPending}); err != nil {
		return nil, err
	}
	if err := lifecycleHook("reg:" + id); err != nil {
		return nil, err
	}
	f, err := p.engine.Mkdir(ctx, id, mp)
	if err != nil {
		return f, err
	}
	if err := lifecycleHook("done:" + id); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// doWriteFile appends then executes a write_file step for fresh content
// (pre: does not exist).
// doWriteFile appends then executes a write_file step. When dynamic is
// true, the target is a dynamic-state file (secrets.json/instance.json,
// spec §5.2): Post identifies content by schema name (via
// dynamicSchemaDetector, installed as the Engine's SchemaDetector) instead
// of a content hash. Otherwise Post carries data's sha256.
func (p *planner) doWriteFile(ctx context.Context, id, rootKey, relPath string, data []byte, mode os.FileMode, uid, gid uint32, preState transaction.PathState, dynamic bool) error {
	mp := mpFor(p.roots, rootKey, relPath)
	post := transaction.PathState{Exists: true, Type: transaction.PathTypeFile, Size: int64(len(data)), Mode: uint32(mode.Perm()), UID: uid, GID: gid}
	if dynamic {
		schemaName, err := dynamicSchemaDetector(mp.TargetKey(), data)
		if err != nil {
			return fmt.Errorf("realitynode: internal: generated dynamic-state file does not self-detect: %w", err)
		}
		post.Schema = schemaName
	} else {
		post.SHA256 = hashHex(data)
	}
	tmp := p.seq.next()
	if err := p.appendStep(transaction.Step{ID: id, Kind: transaction.KindWriteFile, Target: mp.TargetKey(), Pre: preState, Post: post, Status: transaction.StatusPending, Temps: []string{tmp}}); err != nil {
		return err
	}
	if err := lifecycleHook("reg:" + id); err != nil {
		return err
	}
	if err := p.engine.WriteFile(ctx, id, mp, tmp, data); err != nil {
		return err
	}
	return lifecycleHook("done:" + id)
}

// dynamicSchemaDetector is the Engine.SchemaDetector installed for every
// v0.10 instance transaction: it parses a dynamic-state file's top-level
// {"schema": N} field (spec §5.2) and returns a schema name that also
// encodes which file it is (via target's own leaf name), so secrets.json
// and instance.json — which happen to share the same schema number — can
// never compare equal to each other by accident.
func dynamicSchemaDetector(target string, data []byte) (string, error) {
	var probe struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return "", err
	}
	if probe.Schema != dynamicFileSchema {
		return "", fmt.Errorf("unknown dynamic-state schema %d", probe.Schema)
	}
	leaf := target
	if idx := strings.LastIndexByte(target, '/'); idx >= 0 {
		leaf = target[idx+1:]
	}
	return fmt.Sprintf("schema%d:%s", probe.Schema, leaf), nil
}

func notExist() transaction.PathState { return transaction.PathState{Exists: false} }

// --- systemd unit name ---

const unitName = "proxyctl-reality.service"

// unitStateTargetKey is the Target key systemd_* steps use — deliberately
// a different key namespace ("systemd_state/...") from the unit *file's*
// own write_file/remove_file target (targetKey(rootKeySystemd, ...)):
// journal.Validate's per-target chain check (validatePathChains) groups
// every step sharing one Target string, regardless of Kind, and requires
// strict Post==Pre equality along that chain — a systemd_enable/disable
// step's PathState carries only UnitFileState (every other field left
// zero, per journal.go's own PathState.UnitFileState doc comment) and
// would therefore never validly chain onto the unit file's own content-
// bearing write_file/remove_file Post. Recovery never resolves a
// systemd-kind step's Target through PathResolver (see recovery.go's
// isSystemdKind()-gated skips before every Resolve call); it only ever
// extracts the bare unit name from it via systemdUnitFor's "text after
// the last '/'", which this key format also satisfies.
func unitStateTargetKey(roots Roots) string {
	return "systemd_state/" + filepath.Base(roots.SystemdUnit)
}

// --- recovery-time gate/systemd plumbing shared by Install/Reinstall ---

func gateFor(env *Env, listenPort int, configPath string, binaryPath func() string) transaction.Gate {
	return BuildGate(GateConfig{
		Systemd:      env.Systemd,
		Unit:         unitName,
		ListenPort:   listenPort,
		ConfigPath:   configPath,
		BinaryPath:   binaryPath,
		Proc:         env.Proc,
		Runner:       env.Runner,
		Clock:        env.Clock,
		PollBudget:   env.GatePollBudget,
		PollInterval: env.GatePollInterval,
	})
}

// finish is the single exit path every lifecycle operation in this file
// uses once a transaction has begun, success or failure: it always runs
// through RecoverDetached, never Recover with the caller's own ctx. A
// SIGINT/SIGTERM that arrives mid-rollback (or mid-committed-cleanup) must
// not be able to interrupt it (spec §13.3: once a managed path was
// modified the rollback runs under an independent 120s context, and a
// committed transaction only ever moves forward), and using the detached
// context unconditionally — rather than only when ctx is already done at
// the moment finish is called — closes the window where ctx is still live
// on entry and gets cancelled a moment later, inside the recovery.
func finish(ctx context.Context, rec *transaction.Recoverer) (transaction.Outcome, error) {
	return rec.RecoverDetached(ctx)
}

// tombstoneName is this instance's fixed §7.6 tombstone naming: the
// TombstoneGlob's ".proxyctl-reality-txn.tomb-*" with the concrete txn id
// substituted for the trailing "*".
func tombstoneName(roots Roots, txnID string) string {
	base := filepath.Base(roots.TombstoneGlob) // ".proxyctl-reality-txn.tomb-*"
	return strings.TrimSuffix(base, "*") + txnID
}

func txnDirName(roots Roots) string { return filepath.Base(roots.TxnDir) }

// sortedCopy returns a sorted copy of ss, for deterministic output
// (address lists, manifest entries).
func sortedCopy(ss []string) []string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}

// --- platform / environment prechecks (spec §8.1) ---

// minInstallDiskBytes is this Task's own implementation choice (the spec
// only says "磁盘空间足够" without a figure): comfortably above the sum of
// a sing-box release tarball, its extracted binary kept in two versions,
// and every other small managed file.
const minInstallDiskBytes = 200 * 1024 * 1024

func checkPlatform(env *Env) error {
	goos := env.GOOS
	if goos == "" {
		goos = "linux"
	}
	if goos != "linux" {
		return fmt.Errorf("realitynode: unsupported OS %q (only linux is supported)", goos)
	}
	arch := env.GOARCH
	if arch != "amd64" && arch != "arm64" {
		return fmt.Errorf("realitynode: unsupported architecture %q (only amd64/arm64 are supported)", arch)
	}
	if env.SystemdAvailable != nil && !env.SystemdAvailable() {
		return errors.New("realitynode: systemd is not available on this host")
	}
	if env.OSRelease != nil {
		id, ver, err := env.OSRelease()
		if err != nil {
			return fmt.Errorf("realitynode: could not determine OS release: %w", err)
		}
		if err := checkSupportedOSRelease(id, ver); err != nil {
			return err
		}
	}
	return nil
}

// checkSupportedOSRelease mirrors install-proxy.sh's require_supported_os
// (Debian 12; Ubuntu 22.04/24.04) — spec §8.1: "OS满足与安装器相同的支持
// 范围".
func checkSupportedOSRelease(id, versionID string) error {
	switch id {
	case "ubuntu":
		if versionID != "22.04" && versionID != "24.04" {
			return fmt.Errorf("realitynode: only Ubuntu 22.04 and 24.04 are supported (got %q)", versionID)
		}
	case "debian":
		if versionID != "12" {
			return fmt.Errorf("realitynode: only Debian 12 is supported (got %q)", versionID)
		}
	default:
		return fmt.Errorf("realitynode: only Ubuntu and Debian are supported (got %q)", id)
	}
	return nil
}

// ParseOSRelease parses /etc/os-release content into (ID, VERSION_ID),
// exported so cmd/proxyctl's production Env wiring and this package's own
// tests share one implementation.
func ParseOSRelease(content string) (id, versionID string) {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		var key, val string
		if idx := strings.IndexByte(line, '='); idx >= 0 {
			key, val = line[:idx], line[idx+1:]
		} else {
			continue
		}
		val = strings.Trim(val, `"`)
		switch key {
		case "ID":
			id = val
		case "VERSION_ID":
			versionID = val
		}
	}
	return id, versionID
}

func defaultCheckDiskSpace(path string, minBytes uint64) error {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return fmt.Errorf("realitynode: statfs %s: %w", path, err)
	}
	avail := uint64(st.Bavail) * uint64(st.Bsize)
	if avail < minBytes {
		return fmt.Errorf("realitynode: insufficient disk space at %s: %d bytes available, want at least %d", path, avail, minBytes)
	}
	return nil
}

// defaultCheckPortBindable implements spec §8.1's "net.Listen(tcp4,
// 0.0.0.0:<listen_port>)成功后立即关闭".
func defaultCheckPortBindable(port int) (bool, string) {
	l, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return false, err.Error()
	}
	_ = l.Close()
	return true, ""
}

// --- transaction-area-internal (non-managed) helpers ---

// mkTxnSubdir creates and opens a plain subdirectory directly under the
// transaction area's own already-open directory (spec §7.3: "事务区内部
// ...不参与篡改判定"), for dl/ (spec §6) and client/ (spec §9.3's
// temporary client config).
func mkTxnSubdir(txnDir *os.File, name string, mode os.FileMode) (*os.File, error) {
	return mkTxnSubdirNamed(txnDir, name, name, mode)
}

// mkTxnSubdirNamed is mkTxnSubdir with an explicit os.File name. The
// temporary client's config directory (DataPathConfig.ClientConfigDir)
// must carry its *real* path as its Name(): selfcheck.go derives the
// `sing-box run -c <path>` argument from ClientConfigDir.Name(), and a bare
// relative "client" would resolve against the process's working directory
// instead of the transaction area.
func mkTxnSubdirNamed(txnDir *os.File, name, displayName string, mode os.FileMode) (*os.File, error) {
	if err := unix.Mkdirat(int(txnDir.Fd()), name, uint32(mode.Perm())); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, fmt.Errorf("realitynode: mkdir %s in transaction area: %w", name, err)
	}
	fd, err := unix.Openat(int(txnDir.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("realitynode: open %s in transaction area: %w", name, err)
	}
	return os.NewFile(uintptr(fd), displayName), nil
}

// writePlainFile writes data into a plain (non-managed) file directly
// under dir, e.g. the throwaway config copy §8.1 step 4's "sing-box check"
// runs against before any managed path exists yet.
func writePlainFile(dir *os.File, name string, data []byte, mode os.FileMode) (string, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_CREAT|unix.O_TRUNC|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return "", err
	}
	return f.Name(), nil
}

func unitStateBeforeOf(loaded string, ufs, as string) transaction.UnitState {
	return transaction.UnitState{LoadState: loaded, UnitFileState: ufs, ActiveState: as}
}

func loadStateString(loaded bool) string {
	if loaded {
		return "loaded"
	}
	return "not-found"
}

func readUnitStateBefore(ctx context.Context, sysd SystemdFull) transaction.UnitState {
	loaded, _ := sysd.LoadState(ctx, unitName)
	ufs, _ := sysd.UnitFileState(ctx, unitName)
	as, _ := sysd.ActiveState(ctx, unitName)
	return unitStateBeforeOf(loadStateString(loaded), ufs, as)
}

// --- manifest construction (spec §5.2) ---

func fileEntry(path string, mode os.FileMode, uid, gid uint32, sha256Hex string) ManifestEntry {
	return ManifestEntry{Path: path, Mode: uint32(mode.Perm()), UID: int(uid), GID: int(gid), SHA256: sha256Hex}
}
func dirEntry(path string, mode os.FileMode, uid, gid uint32) ManifestEntry {
	return ManifestEntry{Path: path, Mode: uint32(mode.Perm()), UID: int(uid), GID: int(gid)}
}
func dynamicEntry(path string, mode os.FileMode, uid, gid uint32) ManifestEntry {
	return ManifestEntry{Path: path, Mode: uint32(mode.Perm()), UID: int(uid), GID: int(gid), Schema: dynamicFileSchema}
}

func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// lifecycleHook is a process-global test seam, mirroring
// internal/transaction's own stepHook/persistHook idiom (crash_test.go):
// a no-op in production, and overridden by a test process re-exec'd as
// the crashing side of a scenario to os.Exit(1) at a named point in
// Install/Reinstall's own orchestration (distinct from, and layered on
// top of, the Engine's own lower-level primitive crash points that
// Task 4/5's crash matrix already covers exhaustively at the engine
// level). This package's tests must not use t.Parallel while overriding
// it, for the same reason as every other consumer of this pattern in this
// repository.
var lifecycleHook = func(point string) error { return nil }

// --- relative-path layout shared by Install and Reinstall ---

// instanceLayout names every managed path's target key and relative path
// under its trusted root, derived from Roots so an injected test Roots
// (different base directories, same leaf names — matching ProdRoots'
// convention) works unchanged.
type instanceLayout struct {
	roots   Roots
	version string

	unitRel, configDirRel, configRel                    string
	stateDirRel, secretsRel, instanceRel, manifestRel   string
	binDirRel, versionsDirRel, versionDirRel, binaryRel string
}

func newInstanceLayout(roots Roots, version string) instanceLayout {
	configDirRel := filepath.Base(roots.ConfigDir)
	stateDirRel := filepath.Base(roots.StateDir)
	binDirRel := filepath.Base(roots.BinDir)
	versionsDirRel := binDirRel + "/versions"
	versionDirRel := versionsDirRel + "/" + version
	return instanceLayout{
		roots:          roots,
		version:        version,
		unitRel:        filepath.Base(roots.SystemdUnit),
		configDirRel:   configDirRel,
		configRel:      configDirRel + "/" + filepath.Base(roots.Config),
		stateDirRel:    stateDirRel,
		secretsRel:     stateDirRel + "/" + filepath.Base(roots.Secrets),
		instanceRel:    stateDirRel + "/" + filepath.Base(roots.Instance),
		manifestRel:    stateDirRel + "/" + filepath.Base(roots.Manifest),
		binDirRel:      binDirRel,
		versionsDirRel: versionsDirRel,
		versionDirRel:  versionDirRel,
		binaryRel:      versionDirRel + "/sing-box",
	}
}

// allManagedPaths returns every ManagedPath this layout touches, for
// dir_ids bootstrap (bootstrapDirIDs) — it needs only the path shape, not
// which of them exist yet.
func (l instanceLayout) allManagedPaths(roots *openRoots) []transaction.ManagedPath {
	return []transaction.ManagedPath{
		mpFor(roots, rootKeySystemd, l.unitRel),
		mpFor(roots, rootKeyEtc, l.configRel),
		mpFor(roots, rootKeyVarLib, l.secretsRel),
		mpFor(roots, rootKeyVarLib, l.instanceRel),
		mpFor(roots, rootKeyVarLib, l.manifestRel),
		mpFor(roots, rootKeyUsrLib, l.binaryRel),
	}
}

func (l instanceLayout) txnAreaVarLibPath() string { return filepath.Dir(l.roots.StateDir) }

// dlDirPath/clientDirPath are the transaction area's internal dl/ and
// client/ subdirectories' real filesystem paths, needed only to hand an
// external command (sing-box) a usable path argument (spec §6/§9.3): exec
// is fundamentally string-path based (the child process itself resolves
// the path), so this is not a "bypass the Engine" access — nothing here
// mutates a managed path via a raw path string.
func dlDirPath(roots Roots, txnID string) string {
	return filepath.Join(filepath.Dir(roots.StateDir), txnDirName(roots), "dl")
}
func clientDirPath(roots Roots, txnID string) string {
	return filepath.Join(filepath.Dir(roots.StateDir), txnDirName(roots), "client")
}

func mustDerivePublicKey(privateKeyB64 string) string {
	pub, err := DerivePublicKey(privateKeyB64)
	if err != nil {
		// Install/Reinstall only ever call this with a key this same
		// process just generated (or, for reinstall, already validated by
		// the manifest/instance-state check that got it here) — a failure
		// here means the identity itself is corrupt, which Show reports
		// as an ordinary error instead of panicking.
		return ""
	}
	return pub
}

// --- shared transaction wiring ---

// txn bundles everything Install/Reinstall need once a transaction has
// begun: the open trusted roots, the Store/Engine pair sharing one journal,
// the planner that registers-then-executes each step, and the Recoverer
// every exit path (success or failure) finishes through.
type txn struct {
	env        *Env
	roots      *openRoots
	store      *transaction.Store
	engine     *transaction.Engine
	p          *planner
	rec        *transaction.Recoverer
	unitBefore transaction.UnitState
	warnings   []string
	uid, gid   uint32
}

type beginOpts struct {
	op                          string
	versions                    []string // every version whose paths this operation touches, for dir_ids bootstrap
	versionBefore, versionAfter string
	recoveryGate                transaction.Gate
}

// managedPathsFor returns every ManagedPath the given versions' layouts
// touch, for dir_ids bootstrap.
func managedPathsFor(roots *openRoots, r Roots, versions []string) []transaction.ManagedPath {
	var out []transaction.ManagedPath
	for _, v := range versions {
		out = append(out, newInstanceLayout(r, v).allManagedPaths(roots)...)
	}
	return out
}

// beginTxn opens the trusted roots, seeds Journal.DirIDs, records the unit's
// pre-transaction state and starts the transaction (spec §7.1). Any error
// returned means no transaction area was left behind by *this* call's own
// successful steps — BeginTransaction's own mkdir/journal failure is the
// only place a partial area could remain, and it is the engine's to report.
func beginTxn(ctx context.Context, env *Env, o beginOpts) (*txn, error) {
	roots, err := openAllRoots(env.Roots, env.TrustedOwnerUID)
	if err != nil {
		return nil, err
	}
	dirIDs, err := bootstrapDirIDs(env.Roots, managedPathsFor(roots, env.Roots, o.versions), env.TrustedOwnerUID)
	if err != nil {
		roots.closeAll()
		return nil, err
	}
	unitBefore := readUnitStateBefore(ctx, env.Systemd)
	txnID := env.newTxnID()

	j := &transaction.Journal{
		Schema: transaction.CurrentSchema, TxnID: txnID, Op: o.op,
		ProxyctlVersion: env.ProxyctlVersion, CreatedAt: env.now(),
		Phase: transaction.PhaseRunning, UnitStateBefore: unitBefore, DirIDs: dirIDs,
		RollbackSystemd:      transaction.RollbackSystemdNone,
		SingboxVersionBefore: o.versionBefore, SingboxVersionAfter: o.versionAfter,
	}
	store, err := transaction.BeginTransaction(roots.varLib, txnDirName(env.Roots), j)
	if err != nil {
		roots.closeAll()
		return nil, err
	}
	engine := transaction.NewEngine(store)
	engine.SetSchemaDetector(dynamicSchemaDetector)
	t := &txn{
		env: env, roots: roots, store: store, engine: engine, unitBefore: unitBefore,
		uid: uint32(env.TrustedOwnerUID), gid: uint32(env.TrustedOwnerGID),
	}
	t.p = &planner{env: env, store: store, engine: engine, roots: roots, seq: seq{txnID: txnID}}
	t.rec = newInstanceRecoverer(env, roots, engine, txnID, nil, o.recoveryGate)
	return t, nil
}

// newInstanceRecoverer is the single constructor for the instance's
// transaction.Recoverer — the trusted-root layout (PathResolver), the §7.6
// tombstone name, snap/ directory, Systemd (Task 6's Systemctl in
// production) and the §9.2 rollback gate — shared by the forward paths
// (Install/Reinstall) and by OpenRecoverer, which Task 8's `service
// recover` reuses, so live rollback and later recovery cannot drift apart
// (G4: one recovery protocol).
func newInstanceRecoverer(env *Env, roots *openRoots, engine *transaction.Engine, txnID string, snapDir *os.File, gate transaction.Gate) *transaction.Recoverer {
	return &transaction.Recoverer{
		Engine: engine, Root: roots.varLib, TombstoneName: tombstoneName(env.Roots, txnID),
		Resolve: pathResolver{roots: roots}, SnapDir: snapDir, Systemd: env.Systemd, Gate: gate,
		RollbackBudget: env.RollbackBudget,
	}
}

// OpenRecoverer opens the instance's leftover transaction area (its
// journal.json must exist) and returns a Recoverer wired exactly like the
// one the forward paths use. The rollback gate targets the pre-transaction
// version recorded in the journal (SingboxVersionBefore; a fresh install
// has none and its gate is never consulted, since the unit was not active
// before). closeFn releases the trusted roots, transaction directory and
// snap/ handle and must always be called.
func OpenRecoverer(env *Env) (rec *transaction.Recoverer, closeFn func(), err error) {
	roots, err := openAllRoots(env.Roots, env.TrustedOwnerUID)
	if err != nil {
		return nil, nil, err
	}
	store, err := transaction.OpenTransaction(roots.varLib, txnDirName(env.Roots))
	if err != nil {
		roots.closeAll()
		return nil, nil, err
	}
	engine := transaction.NewEngine(store)
	engine.SetSchemaDetector(dynamicSchemaDetector)
	var snap *os.File
	if fd, oerr := unix.Openat(int(store.Dir().Fd()), "snap", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0); oerr == nil {
		snap = os.NewFile(uintptr(fd), "snap")
	}
	j := store.Journal()
	version := j.SingboxVersionBefore
	if version == "" {
		version = j.SingboxVersionAfter
	}
	listenPort := 0
	if _, inst, rerr := readSecretsAndInstance(env); rerr == nil {
		listenPort = inst.ListenPort
	}
	gate := gateFor(env, listenPort, env.Roots.Config, func() string { return versionBinaryPath(env.Roots, version) })
	rec = newInstanceRecoverer(env, roots, engine, j.TxnID, snap, gate)
	return rec, func() {
		if snap != nil {
			snap.Close()
		}
		store.Close()
		roots.closeAll()
	}, nil
}

func (t *txn) close() {
	if t.p.snapDir != nil {
		t.p.snapDir.Close()
	}
	t.store.Close()
	t.roots.closeAll()
}

// openSnapDir creates the transaction area's snap/ directory (reinstall
// only). Failing here is a failure *after* BeginTransaction, so callers
// route it through wrapUp, never preTxnFail (which would leave the
// transaction area behind).
func (t *txn) openSnapDir() error {
	if err := lifecycleHook("mkdir-snap"); err != nil {
		return err
	}
	d, err := mkTxnSubdir(t.store.Dir(), "snap", 0700)
	if err != nil {
		return err
	}
	t.p.snapDir = d
	t.rec.SnapDir = d
	return nil
}

// wrapUp finishes the transaction after a failure at any point after
// BeginTransaction, always via RecoverDetached (see finish). Recover's own
// Outcome decides the result — nothing modified yet -> OutcomeNoChange
// (exit 1), modified -> rolled back (exit 4) — and a Recover that cannot
// confirm recovery is reported as Err with the transaction area path
// (exit 5, never 1: exit 1 is reserved for "no managed path modified").
func (t *txn) wrapUp(ctx context.Context, cause error) Result {
	_ = lifecycleHook("wrapup")
	outcome, ferr := finish(ctx, t.rec)
	if ferr != nil {
		return Result{
			Err:      fmt.Errorf("realitynode: transaction could not be confirmed recovered (cause: %v): %w", cause, ferr),
			Cause:    cause,
			TxnDir:   t.env.Roots.TxnDir,
			Warnings: t.warnings,
		}
	}
	r := Result{Outcome: outcome, HasOutcome: true, Cause: cause, Warnings: t.warnings}
	if outcome == transaction.OutcomeNoChange {
		r.PreTransaction = true
		r.Err = cause
		if r.Err == nil {
			r.Err = errors.New("realitynode: operation did not complete")
		}
	}
	return r
}

// commit persists phase=committed and finishes forward (cleanup + txn area
// deletion). Any failure before the flip is durable goes through wrapUp
// (rollback); after the flip only forward cleanup remains, regardless of
// cancellation (finish never uses the caller's ctx).
func (t *txn) commit(ctx context.Context) Result {
	if err := lifecycleHook("before-commit"); err != nil {
		return t.wrapUp(ctx, err)
	}
	committed := t.store.Journal().Clone()
	committed.Phase = transaction.PhaseCommitted
	if err := t.store.Update(committed); err != nil {
		return t.wrapUp(ctx, fmt.Errorf("commit: %w", err))
	}
	_ = lifecycleHook("after-commit-flip") // committed: nothing may abort forward cleanup
	outcome, ferr := finish(ctx, t.rec)
	if ferr != nil {
		return Result{
			Err:      fmt.Errorf("realitynode: committed transaction cleanup did not finish: %w", ferr),
			TxnDir:   t.env.Roots.TxnDir,
			Warnings: t.warnings,
		}
	}
	return Result{Outcome: outcome, HasOutcome: true, Warnings: t.warnings}
}

type serviceStart struct {
	restart                   bool
	listenPort, selfcheckPort int
	sni, uuid, privateKey     string
	shortID                   string
	clientBinary              string // the installed private sing-box (spec §9.3)
	gate                      transaction.Gate
}

// startAndVerify runs spec §8.1 step 6-7 / §8.2: daemon-reload, enable,
// start (install) or restart (reinstall), the §9.2 gate, then §9.3-9.4.
// daemon-reload and start/restart never change UnitFileState (Pre==Post);
// enable carries the change, keeping the shared chain's Sk.Post==S(k+1).Pre.
func (t *txn) startAndVerify(ctx context.Context, s serviceStart) error {
	env := t.env
	uKey := unitStateTargetKey(env.Roots)
	beforeState := transaction.PathState{UnitFileState: t.unitBefore.UnitFileState}
	enabledState := transaction.PathState{UnitFileState: "enabled"}
	if err := t.p.runSystemdStep(ctx, "systemd-daemon-reload", transaction.KindSystemdDaemonReload, uKey, beforeState, beforeState, env.Systemd.DaemonReload); err != nil {
		return err
	}
	if err := t.p.runSystemdStep(ctx, "systemd-enable", transaction.KindSystemdEnable, uKey, beforeState, enabledState,
		func(c context.Context) error { return env.Systemd.SetUnitFileState(c, unitName, "enabled") }); err != nil {
		return err
	}
	if s.restart {
		if err := t.p.runSystemdStep(ctx, "systemd-restart", transaction.KindSystemdRestart, uKey, enabledState, enabledState,
			func(c context.Context) error { return env.Systemd.Restart(c, unitName) }); err != nil {
			return err
		}
	} else {
		if err := t.p.runSystemdStep(ctx, "systemd-start", transaction.KindSystemdStart, uKey, enabledState, enabledState,
			func(c context.Context) error { return env.Systemd.Start(c, unitName) }); err != nil {
			return err
		}
	}

	if err := s.gate(ctx); err != nil {
		return fmt.Errorf("post-start gate check: %w", err)
	}
	if err := lifecycleHook("gate-passed"); err != nil {
		return err
	}

	clientDir, err := mkTxnSubdirNamed(t.store.Dir(), "client", clientDirPath(env.Roots, ""), 0700)
	if err != nil {
		return err
	}
	defer clientDir.Close()
	dp := env.DataPath
	dp.ListenPort = s.listenPort
	dp.SelfcheckPort = s.selfcheckPort
	dp.SNI = s.sni
	dp.UUID = s.uuid
	dp.PrivateKey = s.privateKey
	dp.ShortID = s.shortID
	dp.ClientBinaryPath = s.clientBinary
	dp.ClientConfigDir = clientDir
	dp.ClientConfigName = "client.json"
	if dp.DestDialer == nil {
		dp.DestDialer = env.TLSDialer
	}
	if dp.NewTimeout == nil {
		dp.NewTimeout = env.NewTimeout
	}
	sc := RunSelfcheck(ctx, dp)
	switch sc.Category {
	case CategoryLocalService, CategoryProtocol:
		return fmt.Errorf("selfcheck failed (%v): %w", sc.Category, sc.Err)
	}
	if sc.EgressCategory == CategoryEgress {
		t.warnings = append(t.warnings, "egress probe failed (warning only): "+sc.EgressWarn)
	}
	return lifecycleHook("selfcheck-passed")
}

// --- manifest construction / registered-version handling ---

type versionRecord struct {
	version string
	binHash string
}

// buildManifest assembles spec §5.2's installed manifest. Every version
// directory and binary the instance keeps (current, plus the retained
// previous one) is registered — purge/uninstall and the two-version cleanup
// rely on registration ("只删除manifest登记过的版本目录", §6).
func buildManifest(env *Env, uid, gid uint32, unitBytes, configBytes []byte, cur versionRecord, prev *versionRecord) (Manifest, []byte, error) {
	r := env.Roots
	entries := []ManifestEntry{
		fileEntry(r.SystemdUnit, 0644, uid, gid, hashHex(unitBytes)),
		dirEntry(r.ConfigDir, 0700, uid, gid),
		fileEntry(r.Config, 0600, uid, gid, hashHex(configBytes)),
		dirEntry(r.StateDir, 0700, uid, gid),
		dynamicEntry(r.Secrets, 0600, uid, gid),
		dynamicEntry(r.Instance, 0600, uid, gid),
		dirEntry(r.BinDir, 0755, uid, gid),
		dirEntry(filepath.Join(r.BinDir, "versions"), 0755, uid, gid),
	}
	recs := []versionRecord{cur}
	previous := ""
	if prev != nil && prev.version != "" && prev.version != cur.version {
		recs = append(recs, *prev)
		previous = prev.version
	}
	for _, v := range recs {
		entries = append(entries,
			dirEntry(filepath.Join(r.BinDir, "versions", v.version), 0755, uid, gid),
			fileEntry(filepath.Join(r.BinDir, "versions", v.version, "sing-box"), 0755, uid, gid, v.binHash))
	}
	m := Manifest{Schema: manifestSchema, State: ManifestStateInstalled, SingboxCurrent: cur.version, SingboxPrevious: previous, Entries: entries}
	data, err := json.MarshalIndent(m, "", "  ")
	return m, data, err
}

func manifestEntryFor(m Manifest, path string) (ManifestEntry, bool) {
	for _, e := range m.Entries {
		if e.Path == path {
			return e, true
		}
	}
	return ManifestEntry{}, false
}

func versionBinaryPath(r Roots, version string) string {
	return filepath.Join(r.BinDir, "versions", version, "sing-box")
}

// registeredBinHash returns the sha256 the manifest registers for version's
// private binary.
func registeredBinHash(env *Env, m Manifest, version string) (string, bool) {
	e, ok := manifestEntryFor(m, versionBinaryPath(env.Roots, version))
	if !ok || e.SHA256 == "" {
		return "", false
	}
	return e.SHA256, true
}

// registerVersionCleanup appends pending remove_file/remove_dir steps for a
// superseded version (spec §6: "最多保留两个版本"; cleaned as a committed-
// phase forward step). Only a version the *manifest* registers is ever
// deleted, and each step's Pre is built from the manifest's registered
// mode/owner/sha256 — never from whatever is on disk — so a directory or
// binary whose content, mode or owner has drifted from the manifest makes
// the Engine's pre-state check refuse the removal instead of deleting it.
// Live state is consulted only for the one field the manifest does not
// record (size), and a live/manifest mismatch is reported here, before
// commit, rather than silently adopted.
func (p *planner) registerVersionCleanup(old Manifest, version string) (registered bool, err error) {
	binPath := versionBinaryPath(p.env.Roots, version)
	dirPath := filepath.Dir(binPath)
	binEntry, okB := manifestEntryFor(old, binPath)
	dirEntry, okD := manifestEntryFor(old, dirPath)
	if !okB || !okD || binEntry.SHA256 == "" {
		return false, nil
	}
	rel := func(abs string) string {
		r, _ := filepath.Rel(p.env.Roots.BinDir, abs)
		return filepath.Base(p.env.Roots.BinDir) + "/" + filepath.ToSlash(r)
	}
	binMP := mpFor(p.roots, rootKeyUsrLib, rel(binPath))
	dirMP := mpFor(p.roots, rootKeyUsrLib, rel(dirPath))

	live, err := p.engine.LiveFileState(binMP, true, false)
	if err != nil {
		return false, fmt.Errorf("realitynode: read previous version binary: %w", err)
	}
	binPre := transaction.PathState{
		Exists: true, Type: transaction.PathTypeFile, Size: live.Size, SHA256: binEntry.SHA256,
		Mode: binEntry.Mode, UID: uint32(binEntry.UID), GID: uint32(binEntry.GID),
	}
	if live != binPre {
		return false, fmt.Errorf("realitynode: previous version %s binary no longer matches the manifest; refusing to remove it", version)
	}
	dirPre := transaction.PathState{
		Exists: true, Type: transaction.PathTypeDir,
		Mode: dirEntry.Mode, UID: uint32(dirEntry.UID), GID: uint32(dirEntry.GID),
	}
	steps := []transaction.Step{
		{ID: "cleanup-old-binary", Kind: transaction.KindRemoveFile, Target: binMP.TargetKey(), Pre: binPre, Post: notExist(), Status: transaction.StatusPending},
		{ID: "cleanup-old-versiondir", Kind: transaction.KindRemoveDir, Target: dirMP.TargetKey(), Pre: dirPre, Post: notExist(), Status: transaction.StatusPending},
	}
	if err := p.appendSteps(steps...); err != nil {
		return false, err
	}
	return true, lifecycleHook("cleanup-registered")
}

// doOverwriteFile implements "snapshot当前内容,再write_file新内容" (spec
// §8.2) for a managed file that already exists.
func (p *planner) doOverwriteFile(ctx context.Context, id, rootKey, relPath string, data []byte, mode os.FileMode, uid, gid uint32, dynamic bool) error {
	mp := mpFor(p.roots, rootKey, relPath)
	live, err := p.engine.LiveFileState(mp, !dynamic, dynamic)
	if err != nil {
		return fmt.Errorf("realitynode: read live state of %s: %w", mp.TargetKey(), err)
	}
	snapTmp := p.seq.next()
	if err := p.appendStep(transaction.Step{ID: id + "-snap", Kind: transaction.KindSnapshot, Target: mp.TargetKey(), Pre: live, Post: live, Status: transaction.StatusPending, Temps: []string{snapTmp}}); err != nil {
		return err
	}
	if err := lifecycleHook("reg:" + id + "-snap"); err != nil {
		return err
	}
	if err := p.engine.Snapshot(ctx, id+"-snap", mp, p.snapDir, snapTmp); err != nil {
		return err
	}
	if err := lifecycleHook("done:" + id + "-snap"); err != nil {
		return err
	}
	return p.doWriteFile(ctx, id+"-write", rootKey, relPath, data, mode, uid, gid, live, dynamic)
}

// checkNoInstanceTraces is install's post-lock "目标路径冲突" recheck
// (spec §8 table): none of the instance's asset roots or its transaction
// area/tombstone may exist.
func checkNoInstanceTraces(env *Env) error {
	for _, p := range []string{env.Roots.SystemdUnit, env.Roots.ConfigDir, env.Roots.StateDir, env.Roots.BinDir, env.Roots.TxnDir} {
		if _, err := os.Lstat(p); err == nil {
			return fmt.Errorf("realitynode: %s already exists; refusing to install over it", p)
		}
	}
	if matches, _ := filepath.Glob(env.Roots.TombstoneGlob); len(matches) > 0 {
		return fmt.Errorf("realitynode: leftover transaction tombstone %s exists", matches[0])
	}
	return nil
}

// --- Install (spec §8.1) ---

// InstallPlan is PrecheckInstall's outcome, handed to RunInstall (after the
// caller took its locks) so the post-lock recheck does not redo the
// caller-visible warnings.
type InstallPlan struct {
	Params        InstallParams
	SelfcheckPort int
	Warnings      []string
}

// PrecheckInstall is spec §8's step 3 "快速预检", run *before* any lock is
// taken: parameters, OS/arch/systemd, instance state, disk space,
// listen_port bindability, selfcheck_port selection, ufw warning and the
// §9.1 dest probe. It modifies nothing.
func PrecheckInstall(ctx context.Context, env *Env, params InstallParams) (InstallPlan, error) {
	if err := ValidateInstallParams(params); err != nil {
		return InstallPlan{}, fmt.Errorf("invalid parameters: %w", err)
	}
	if err := checkPlatform(env); err != nil {
		return InstallPlan{}, err
	}
	if state := DetectInstanceState(env.FS, env.Roots); state.Status != StatusNotInstalled {
		return InstallPlan{}, fmt.Errorf("realitynode: instance is not eligible for install (status=%v, reasons=%v)", state.Status, state.Reasons)
	}
	checkDisk := env.CheckDiskSpace
	if checkDisk == nil {
		checkDisk = defaultCheckDiskSpace
	}
	if err := checkDisk(filepath.Dir(env.Roots.StateDir), minInstallDiskBytes); err != nil {
		return InstallPlan{}, err
	}
	checkBind := env.CheckPortBindable
	if checkBind == nil {
		checkBind = defaultCheckPortBindable
	}
	if ok, detail := checkBind(params.Port); !ok {
		return InstallPlan{}, fmt.Errorf("realitynode: listen_port %d not bindable: %s", params.Port, detail)
	}
	selfcheckPort, err := SelectSelfcheckPort(params.Port, env.PortRange, env.PortBind, env.RandIntn)
	if err != nil {
		return InstallPlan{}, err
	}

	plan := InstallPlan{Params: params, SelfcheckPort: selfcheckPort}
	if env.UFWActive != nil {
		if active, _ := env.UFWActive(); active {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("ufw is active on this host; also allow TCP %d in your cloud security group", params.Port))
		}
	}
	destResult, destCat, err := ProbeDest(ctx, params.SNI, env.TLSDialer, env.NewTimeout)
	if err != nil {
		return InstallPlan{}, fmt.Errorf("realitynode: dest probe %s:443 failed (%v): %w", params.SNI, destCat, err)
	}
	if !destResult.NegotiatedH2 {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("dest %s:443 did not negotiate ALPN h2 (informational only)", params.SNI))
	}
	return plan, nil
}

// Install runs PrecheckInstall then RunInstall back to back, for callers
// (tests) that do not need to interpose their own locking.
func Install(ctx context.Context, env *Env, params InstallParams) Result {
	plan, err := PrecheckInstall(ctx, env, params)
	if err != nil {
		return preTxnFail(err)
	}
	return RunInstall(ctx, env, plan)
}

// RunInstall is everything after the locks are held: spec §8's step 5
// recheck (instance state, target path conflicts, both ports) followed by
// the transaction (§8.1 steps 1-8). The subscription-center role recheck
// belongs to the caller (internal/role is deliberately not imported here).
func RunInstall(ctx context.Context, env *Env, plan InstallPlan) Result {
	params := plan.Params
	warnings := append([]string(nil), plan.Warnings...)

	if state := DetectInstanceState(env.FS, env.Roots); state.Status != StatusNotInstalled {
		return preTxnFail(fmt.Errorf("realitynode: instance is no longer eligible for install (status=%v, reasons=%v)", state.Status, state.Reasons))
	}
	if err := checkNoInstanceTraces(env); err != nil {
		return preTxnFail(err)
	}
	checkBind := env.CheckPortBindable
	if checkBind == nil {
		checkBind = defaultCheckPortBindable
	}
	if ok, detail := checkBind(params.Port); !ok {
		return preTxnFail(fmt.Errorf("realitynode: listen_port %d not bindable: %s", params.Port, detail))
	}
	canBind := env.PortBind
	if canBind == nil {
		canBind = defaultPortBinder
	}
	selfcheckPort := plan.SelfcheckPort
	if !canBind(selfcheckPort) {
		return preTxnFail(fmt.Errorf("realitynode: selfcheck_port %d is no longer bindable on 127.0.0.1", selfcheckPort))
	}

	localCIDRs, err := LocalAddressCIDRs(ctx, params.Server, env.InterfaceLister, env.Resolver)
	if err != nil {
		return preTxnFail(fmt.Errorf("realitynode: compute local address reject list: %w", err))
	}
	pinnedSHA, pinnedOK := env.pinnedSHA256(env.GOARCH)
	if !pinnedOK {
		return preTxnFail(fmt.Errorf("realitynode: no pinned sing-box hash for architecture %q", env.GOARCH))
	}

	version := env.pinnedVersion()
	layout := newInstanceLayout(env.Roots, version)
	binaryPath := versionBinaryPath(env.Roots, version)
	gate := gateFor(env, params.Port, env.Roots.Config, func() string { return binaryPath })

	t, err := beginTxn(ctx, env, beginOpts{op: "install", versions: []string{version}, versionAfter: version, recoveryGate: gate})
	if err != nil {
		return preTxnFail(err)
	}
	defer t.close()
	t.warnings = warnings
	p := t.p
	uid32, gid32 := t.uid, t.gid

	if err := lifecycleHook("began"); err != nil {
		return t.wrapUp(ctx, err)
	}

	// --- §6/§8.1 step 1: download + verify, inside dl/ ---
	dlDir, err := mkTxnSubdir(t.store.Dir(), "dl", 0700)
	if err != nil {
		return t.wrapUp(ctx, err)
	}
	defer dlDir.Close()
	dlPath := dlDirPath(env.Roots, "")
	dlResult, err := DownloadSingbox(ctx, dlDir, env.Downloader, env.GOARCH, version, pinnedSHA, env.NewTimeout)
	if err != nil {
		return t.wrapUp(ctx, fmt.Errorf("download sing-box: %w", err))
	}
	extracted := filepath.Join(dlPath, dlResult.Name)
	if err := VerifyVersion(ctx, env.Runner, extracted, dlResult.Version); err != nil {
		return t.wrapUp(ctx, err)
	}
	binData, err := os.ReadFile(extracted)
	if err != nil {
		return t.wrapUp(ctx, err)
	}
	if err := lifecycleHook("downloaded"); err != nil {
		return t.wrapUp(ctx, err)
	}

	// --- §8.1 steps 2-3: identity + config ---
	identity, err := GenerateIdentity(env.randReader())
	if err != nil {
		return t.wrapUp(ctx, err)
	}
	configBytes, err := GenerateServerConfig(ServerConfigParams{
		ListenPort: params.Port, SelfcheckPort: selfcheckPort, SNI: params.SNI,
		UUID: identity.UUID, PrivateKey: identity.PrivateKey, ShortID: identity.ShortID,
		LocalCIDRs: localCIDRs,
	})
	if err != nil {
		return t.wrapUp(ctx, err)
	}

	// --- §8.1 step 4: check, against a throwaway copy inside dl/ ---
	if _, err := writePlainFile(dlDir, "check-config.json", configBytes, 0600); err != nil {
		return t.wrapUp(ctx, err)
	}
	if err := VerifyConfig(ctx, env.Runner, extracted, filepath.Join(dlPath, "check-config.json")); err != nil {
		return t.wrapUp(ctx, err)
	}
	if err := lifecycleHook("checked"); err != nil {
		return t.wrapUp(ctx, err)
	}
	unitBytes := RenderUnit(version)

	// --- §8.1 step 5: write every asset ---
	mkdir := func(id, rootKey, rel string, mode os.FileMode) error {
		f, err := p.doMkdir(ctx, id, rootKey, rel, mode, uid32, gid32, false)
		if f != nil {
			f.Close()
		}
		return err
	}
	if err := mkdir("mkdir-etc", rootKeyEtc, layout.configDirRel, 0700); err != nil {
		return t.wrapUp(ctx, err)
	}
	if err := p.doWriteFile(ctx, "write-config", rootKeyEtc, layout.configRel, configBytes, 0600, uid32, gid32, notExist(), false); err != nil {
		return t.wrapUp(ctx, err)
	}
	if err := mkdir("mkdir-statedir", rootKeyVarLib, layout.stateDirRel, 0700); err != nil {
		return t.wrapUp(ctx, err)
	}
	secretsBytes, _ := json.Marshal(SecretsFile{Schema: dynamicFileSchema, UUID: identity.UUID, PrivateKey: identity.PrivateKey, ShortID: identity.ShortID})
	if err := p.doWriteFile(ctx, "write-secrets", rootKeyVarLib, layout.secretsRel, secretsBytes, 0600, uid32, gid32, notExist(), true); err != nil {
		return t.wrapUp(ctx, err)
	}
	instanceBytes, _ := json.Marshal(InstanceFile{Schema: dynamicFileSchema, Name: params.Name, Server: params.Server, ListenPort: params.Port, SNI: params.SNI, SelfcheckPort: selfcheckPort})
	if err := p.doWriteFile(ctx, "write-instance", rootKeyVarLib, layout.instanceRel, instanceBytes, 0600, uid32, gid32, notExist(), true); err != nil {
		return t.wrapUp(ctx, err)
	}
	for _, m := range []struct{ id, rel string }{
		{"mkdir-bindir", layout.binDirRel}, {"mkdir-versions", layout.versionsDirRel}, {"mkdir-version", layout.versionDirRel},
	} {
		if err := mkdir(m.id, rootKeyUsrLib, m.rel, 0755); err != nil {
			return t.wrapUp(ctx, err)
		}
	}
	if err := p.doWriteFile(ctx, "write-binary", rootKeyUsrLib, layout.binaryRel, binData, 0755, uid32, gid32, notExist(), false); err != nil {
		return t.wrapUp(ctx, err)
	}
	if err := p.doWriteFile(ctx, "write-unit", rootKeySystemd, layout.unitRel, unitBytes, 0644, uid32, gid32, notExist(), false); err != nil {
		return t.wrapUp(ctx, err)
	}
	if err := lifecycleHook("install:after-assets-written"); err != nil {
		return t.wrapUp(ctx, err)
	}

	// --- §8.1 steps 6-7: systemd, gate, selfcheck (client = the installed
	// private sing-box, spec §9.3) ---
	if err := t.startAndVerify(ctx, serviceStart{
		listenPort: params.Port, selfcheckPort: selfcheckPort, sni: params.SNI,
		uuid: identity.UUID, privateKey: identity.PrivateKey, shortID: identity.ShortID,
		clientBinary: binaryPath, gate: gate,
	}); err != nil {
		return t.wrapUp(ctx, err)
	}

	// --- §8.1 step 8: manifest + commit ---
	_, manifestBytes, err := buildManifest(env, uid32, gid32, unitBytes, configBytes, versionRecord{version, hashHex(binData)}, nil)
	if err != nil {
		return t.wrapUp(ctx, err)
	}
	if err := p.doWriteFile(ctx, "write-manifest", rootKeyVarLib, layout.manifestRel, manifestBytes, 0600, uid32, gid32, notExist(), false); err != nil {
		return t.wrapUp(ctx, err)
	}
	result := t.commit(ctx)
	if result.HasOutcome && result.Outcome == transaction.OutcomeCommitted && result.Err == nil {
		result.Stdout = InstallOutput(RegisterParams{
			Server: params.Server, ListenPort: params.Port, UUID: identity.UUID,
			PublicKey: mustDerivePublicKey(identity.PrivateKey), ShortID: identity.ShortID,
			SNI: params.SNI, Name: params.Name,
		})
	}
	return result
}

// --- Show (spec §8.3) ---

// Show implements spec §8.3: read-only (only env.FS.ReadFile is ever
// called — no write, no transaction, no implicit migration), the public
// key always freshly derived from the stored private key. The caller is
// expected to already hold L3 (spec: "show只取L3"); Show itself does not
// touch any lock.
func Show(ctx context.Context, env *Env) Result {
	state := DetectInstanceState(env.FS, env.Roots)
	switch state.Status {
	case StatusInstalled:
		return showInstalled(env)
	case StatusUninstalledKeepIdentity:
		return showUninstalled(env)
	default:
		return preTxnFail(fmt.Errorf("realitynode: instance not available for show (status=%v, reasons=%v)", state.Status, state.Reasons))
	}
}

func readSecretsAndInstance(env *Env) (SecretsFile, InstanceFile, error) {
	var secrets SecretsFile
	var inst InstanceFile
	secretsData, err := env.FS.ReadFile(env.Roots.Secrets)
	if err != nil {
		return secrets, inst, err
	}
	if err := json.Unmarshal(secretsData, &secrets); err != nil {
		return secrets, inst, err
	}
	instanceData, err := env.FS.ReadFile(env.Roots.Instance)
	if err != nil {
		return secrets, inst, err
	}
	if err := json.Unmarshal(instanceData, &inst); err != nil {
		return secrets, inst, err
	}
	return secrets, inst, nil
}

func showInstalled(env *Env) Result {
	secrets, inst, err := readSecretsAndInstance(env)
	if err != nil {
		return preTxnFail(err)
	}
	pub, err := DerivePublicKey(secrets.PrivateKey)
	if err != nil {
		return preTxnFail(fmt.Errorf("realitynode: derive public key: %w", err))
	}
	var warnings []string
	pinned := env.pinnedVersion()
	if manifestData, err := env.FS.ReadFile(env.Roots.Manifest); err == nil {
		var manifest Manifest
		if json.Unmarshal(manifestData, &manifest) == nil && manifest.SingboxCurrent != "" && manifest.SingboxCurrent != pinned {
			warnings = append(warnings, fmt.Sprintf(
				"this instance's sing-box version (%s) differs from what this proxyctl build expects (%s); run 'sudo proxyctl node service reinstall'",
				manifest.SingboxCurrent, pinned))
		}
	}
	return Result{
		Stdout: ShowInstalledOutput(RegisterParams{
			Server: inst.Server, ListenPort: inst.ListenPort, UUID: secrets.UUID,
			PublicKey: pub, ShortID: secrets.ShortID, SNI: inst.SNI, Name: inst.Name,
		}),
		Warnings: warnings,
	}
}

func showUninstalled(env *Env) Result {
	_, inst, err := readSecretsAndInstance(env)
	if err != nil {
		return preTxnFail(err)
	}
	return Result{Stdout: ShowUninstalledOutput(inst.Name)}
}

// --- Reinstall (spec §8.2) ---

// ReinstallParams is reinstall's only accepted input (spec §8.2:
// "不接受--name、--server、--port、--sni（传入返回2）" is enforced by the
// CLI layer's strict parsing, before Reinstall is ever called).
// SingboxVersion/SingboxSHA256 must both be empty (refresh to the
// compiled-in pinned version) or both set (spec §6: "缺少任一个都是退出码
// 2" — also a CLI-layer check; PrecheckReinstall re-enforces it).
type ReinstallParams struct {
	SingboxVersion string
	SingboxSHA256  string
}

// ReinstallPlan is PrecheckReinstall's outcome.
type ReinstallPlan struct {
	Params        ReinstallParams
	FromInstalled bool
}

// PrecheckReinstall is spec §8's step 3 for reinstall, before any lock:
// argument pairing, OS/arch/systemd and that the instance is in one of the
// two eligible states. It modifies nothing.
func PrecheckReinstall(ctx context.Context, env *Env, params ReinstallParams) (ReinstallPlan, error) {
	if (params.SingboxVersion == "") != (params.SingboxSHA256 == "") {
		return ReinstallPlan{}, errors.New("realitynode: --singbox-version and --singbox-sha256 must both be given, or both omitted")
	}
	if err := checkPlatform(env); err != nil {
		return ReinstallPlan{}, err
	}
	state := DetectInstanceState(env.FS, env.Roots)
	switch state.Status {
	case StatusInstalled:
		return ReinstallPlan{Params: params, FromInstalled: true}, nil
	case StatusUninstalledKeepIdentity:
		return ReinstallPlan{Params: params, FromInstalled: false}, nil
	default:
		return ReinstallPlan{}, fmt.Errorf("realitynode: instance not eligible for reinstall (status=%v, reasons=%v)", state.Status, state.Reasons)
	}
}

// Reinstall runs PrecheckReinstall then RunReinstall back to back.
func Reinstall(ctx context.Context, env *Env, params ReinstallParams) Result {
	plan, err := PrecheckReinstall(ctx, env, params)
	if err != nil {
		return preTxnFail(err)
	}
	return RunReinstall(ctx, env, plan)
}

func readManifestFile(env *Env) (Manifest, error) {
	data, err := env.FS.ReadFile(env.Roots.Manifest)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// RunReinstall is everything after the locks are held: the §8 recheck
// (instance state — the source may have changed since PrecheckReinstall —
// and, for the "uninstalled" source, listen_port bindability), then the
// §8.2 transaction. selfcheck_port is re-verified inside and reselected if
// it can no longer be bound.
func RunReinstall(ctx context.Context, env *Env, plan ReinstallPlan) Result {
	params := plan.Params
	if (params.SingboxVersion == "") != (params.SingboxSHA256 == "") {
		return preTxnFail(errors.New("realitynode: --singbox-version and --singbox-sha256 must both be given, or both omitted"))
	}
	var fromInstalled bool
	switch state := DetectInstanceState(env.FS, env.Roots); state.Status {
	case StatusInstalled:
		fromInstalled = true
	case StatusUninstalledKeepIdentity:
		fromInstalled = false
	default:
		return preTxnFail(fmt.Errorf("realitynode: instance is no longer eligible for reinstall (status=%v, reasons=%v)", state.Status, state.Reasons))
	}

	secrets, inst, err := readSecretsAndInstance(env)
	if err != nil {
		return preTxnFail(fmt.Errorf("realitynode: read existing identity: %w", err))
	}
	oldManifest, err := readManifestFile(env)
	if err != nil {
		return preTxnFail(fmt.Errorf("realitynode: read existing manifest: %w", err))
	}

	// Which versions exist on disk *and* are registered (spec §5.2/§6).
	oldCur, oldPrev := "", ""
	if fromInstalled {
		oldCur, oldPrev = oldManifest.SingboxCurrent, oldManifest.SingboxPrevious
	}

	explicit := params.SingboxVersion != ""
	target := params.SingboxVersion
	if target == "" {
		target = env.pinnedVersion()
	}
	targetExists := fromInstalled && (target == oldCur || (oldPrev != "" && target == oldPrev))

	// The retained previous version of the manifest this run writes. Both
	// the manifest and the post-commit cleanup decision derive from this one
	// value, so they can never disagree about which versions are kept.
	newPrev := ""
	switch {
	case !fromInstalled:
		newPrev = ""
	case target == oldCur:
		newPrev = oldPrev
	default: // new version, or switching back to the previous one
		newPrev = oldCur
	}

	var existingHash string
	if targetExists {
		h, ok := registeredBinHash(env, oldManifest, target)
		if !ok {
			return preTxnFail(fmt.Errorf("realitynode: manifest does not register a binary hash for existing version %s", target))
		}
		existingHash = h
	} else if _, err := os.Lstat(filepath.Join(env.Roots.BinDir, "versions", target)); err == nil {
		// An on-disk version directory the manifest does not register
		// would make the mkdir step fail halfway through the transaction;
		// refuse up front instead (spec §7.1: unregistered paths are
		// never adopted).
		return preTxnFail(fmt.Errorf("realitynode: %s exists but is not registered in the manifest; refusing to reinstall over it", filepath.Join(env.Roots.BinDir, "versions", target)))
	}

	if !fromInstalled {
		checkBind := env.CheckPortBindable
		if checkBind == nil {
			checkBind = defaultCheckPortBindable
		}
		if ok, detail := checkBind(inst.ListenPort); !ok {
			return preTxnFail(fmt.Errorf("realitynode: listen_port %d not bindable: %s", inst.ListenPort, detail))
		}
	}
	selfcheckPort := inst.SelfcheckPort
	selfcheckPortChanged := false
	canBind := env.PortBind
	if canBind == nil {
		canBind = defaultPortBinder
	}
	if !canBind(selfcheckPort) {
		newPort, err := SelectSelfcheckPort(inst.ListenPort, env.PortRange, env.PortBind, env.RandIntn)
		if err != nil {
			return preTxnFail(err)
		}
		selfcheckPort = newPort
		selfcheckPortChanged = true
	}

	localCIDRs, err := LocalAddressCIDRs(ctx, inst.Server, env.InterfaceLister, env.Resolver)
	if err != nil {
		return preTxnFail(fmt.Errorf("realitynode: compute local address reject list: %w", err))
	}
	configBytes, err := GenerateServerConfig(ServerConfigParams{
		ListenPort: inst.ListenPort, SelfcheckPort: selfcheckPort, SNI: inst.SNI,
		UUID: secrets.UUID, PrivateKey: secrets.PrivateKey, ShortID: secrets.ShortID,
		LocalCIDRs: localCIDRs,
	})
	if err != nil {
		return preTxnFail(err)
	}

	layout := newInstanceLayout(env.Roots, target)
	targetBin := versionBinaryPath(env.Roots, target)
	forwardGate := gateFor(env, inst.ListenPort, env.Roots.Config, func() string { return targetBin })
	// Rollback re-runs the §9.2 gate against the *pre-transaction*
	// manifest's binary (spec §7.4 step 5).
	rollbackBin := targetBin
	if fromInstalled {
		rollbackBin = versionBinaryPath(env.Roots, oldCur)
	}
	rollbackGate := gateFor(env, inst.ListenPort, env.Roots.Config, func() string { return rollbackBin })

	versions := []string{target}
	for _, v := range []string{oldCur, oldPrev} {
		if v != "" && v != target {
			versions = append(versions, v)
		}
	}
	t, err := beginTxn(ctx, env, beginOpts{
		op: "reinstall", versions: versions,
		versionBefore: oldCur, versionAfter: target, recoveryGate: rollbackGate,
	})
	if err != nil {
		return preTxnFail(err)
	}
	defer t.close()
	p := t.p
	uid32, gid32 := t.uid, t.gid

	if err := lifecycleHook("began"); err != nil {
		return t.wrapUp(ctx, err)
	}
	if err := t.openSnapDir(); err != nil {
		return t.wrapUp(ctx, err)
	}

	// --- binary: reuse an existing registered version, or download ---
	dlDir, err := mkTxnSubdir(t.store.Dir(), "dl", 0700)
	if err != nil {
		return t.wrapUp(ctx, err)
	}
	defer dlDir.Close()
	dlPath := dlDirPath(env.Roots, "")

	var binData []byte // bytes to write when the target version is new
	checkBin := targetBin
	binHash := existingHash
	needDownload := !targetExists || explicit
	if needDownload {
		dlVersion, dlSHA := params.SingboxVersion, params.SingboxSHA256
		if !explicit {
			var ok bool
			dlSHA, ok = env.pinnedSHA256(env.GOARCH)
			if !ok {
				return t.wrapUp(ctx, fmt.Errorf("realitynode: no pinned sing-box hash for architecture %q", env.GOARCH))
			}
			dlVersion = env.pinnedVersion()
		}
		dlResult, err := DownloadSingbox(ctx, dlDir, env.Downloader, env.GOARCH, dlVersion, dlSHA, env.NewTimeout)
		if err != nil {
			return t.wrapUp(ctx, fmt.Errorf("download sing-box: %w", err))
		}
		extracted := filepath.Join(dlPath, dlResult.Name)
		if err := VerifyVersion(ctx, env.Runner, extracted, dlResult.Version); err != nil {
			return t.wrapUp(ctx, err)
		}
		data, err := os.ReadFile(extracted)
		if err != nil {
			return t.wrapUp(ctx, err)
		}
		if targetExists {
			// The version is already installed and registered: the
			// download only proves the caller's explicit hash; the bytes
			// must equal what is installed, otherwise the two cannot both
			// be right and nothing is modified (spec §6).
			if hashHex(data) != existingHash {
				return t.wrapUp(ctx, fmt.Errorf("realitynode: downloaded sing-box %s differs from the installed, registered %s; refusing to reinstall", target, target))
			}
		} else {
			binData = data
			binHash = hashHex(data)
			checkBin = extracted
		}
	}
	if err := lifecycleHook("downloaded"); err != nil {
		return t.wrapUp(ctx, err)
	}

	// --- §6 step 4 / §8.1 step 4: sing-box check of the *new* config with
	// the binary that will run it (the reused installed one, or the freshly
	// downloaded one), strictly before any managed path is modified ---
	if _, err := writePlainFile(dlDir, "check-config.json", configBytes, 0600); err != nil {
		return t.wrapUp(ctx, err)
	}
	if err := VerifyConfig(ctx, env.Runner, checkBin, filepath.Join(dlPath, "check-config.json")); err != nil {
		return t.wrapUp(ctx, err)
	}
	if err := lifecycleHook("checked"); err != nil {
		return t.wrapUp(ctx, err)
	}
	unitBytes := RenderUnit(target)

	// --- config + unit: snapshot-then-write when present, plain write
	// when absent ("如存在") ---
	if fromInstalled {
		if err := p.doOverwriteFile(ctx, "config", rootKeyEtc, layout.configRel, configBytes, 0600, uid32, gid32, false); err != nil {
			return t.wrapUp(ctx, err)
		}
		if err := p.doOverwriteFile(ctx, "unit", rootKeySystemd, layout.unitRel, unitBytes, 0644, uid32, gid32, false); err != nil {
			return t.wrapUp(ctx, err)
		}
	} else {
		f, err := p.doMkdir(ctx, "mkdir-etc", rootKeyEtc, layout.configDirRel, 0700, uid32, gid32, false)
		if f != nil {
			f.Close()
		}
		if err != nil {
			return t.wrapUp(ctx, err)
		}
		if err := p.doWriteFile(ctx, "write-config", rootKeyEtc, layout.configRel, configBytes, 0600, uid32, gid32, notExist(), false); err != nil {
			return t.wrapUp(ctx, err)
		}
		if err := p.doWriteFile(ctx, "write-unit", rootKeySystemd, layout.unitRel, unitBytes, 0644, uid32, gid32, notExist(), false); err != nil {
			return t.wrapUp(ctx, err)
		}
	}

	// --- instance.json: snapshot-then-write, only when selfcheck_port changed ---
	if selfcheckPortChanged {
		instanceBytes, _ := json.Marshal(InstanceFile{Schema: dynamicFileSchema, Name: inst.Name, Server: inst.Server, ListenPort: inst.ListenPort, SNI: inst.SNI, SelfcheckPort: selfcheckPort})
		if err := p.doOverwriteFile(ctx, "instance", rootKeyVarLib, layout.instanceRel, instanceBytes, 0600, uid32, gid32, true); err != nil {
			return t.wrapUp(ctx, err)
		}
	}

	// --- private binary: rebuild the directory chain when coming from the
	// uninstalled source (its bin dir was removed), then the new version's
	// directory and binary. An already-registered version needs no writes.
	if !targetExists {
		mkdir := func(id, rel string) error {
			f, err := p.doMkdir(ctx, id, rootKeyUsrLib, rel, 0755, uid32, gid32, false)
			if f != nil {
				f.Close()
			}
			return err
		}
		if !fromInstalled {
			if err := mkdir("mkdir-bindir", layout.binDirRel); err != nil {
				return t.wrapUp(ctx, err)
			}
			if err := mkdir("mkdir-versions", layout.versionsDirRel); err != nil {
				return t.wrapUp(ctx, err)
			}
		}
		if err := mkdir("mkdir-version", layout.versionDirRel); err != nil {
			return t.wrapUp(ctx, err)
		}
		if err := p.doWriteFile(ctx, "write-binary", rootKeyUsrLib, layout.binaryRel, binData, 0755, uid32, gid32, notExist(), false); err != nil {
			return t.wrapUp(ctx, err)
		}
	}
	if err := lifecycleHook("reinstall:after-assets-written"); err != nil {
		return t.wrapUp(ctx, err)
	}

	if err := t.startAndVerify(ctx, serviceStart{
		restart:    true,
		listenPort: inst.ListenPort, selfcheckPort: selfcheckPort, sni: inst.SNI,
		uuid: secrets.UUID, privateKey: secrets.PrivateKey, shortID: secrets.ShortID,
		clientBinary: targetBin, gate: forwardGate,
	}); err != nil {
		return t.wrapUp(ctx, err)
	}

	// --- manifest: current + retained previous version, both registered ---
	var prevRec *versionRecord
	if newPrev != "" {
		h, ok := registeredBinHash(env, oldManifest, newPrev)
		if !ok {
			return t.wrapUp(ctx, fmt.Errorf("realitynode: manifest does not register a binary hash for retained version %s", newPrev))
		}
		prevRec = &versionRecord{version: newPrev, binHash: h}
	}
	_, manifestBytes, err := buildManifest(env, uid32, gid32, unitBytes, configBytes, versionRecord{version: target, binHash: binHash}, prevRec)
	if err != nil {
		return t.wrapUp(ctx, err)
	}
	// manifest.json exists in both source states, so this is an overwrite.
	if err := p.doOverwriteFile(ctx, "manifest", rootKeyVarLib, layout.manifestRel, manifestBytes, 0600, uid32, gid32, false); err != nil {
		return t.wrapUp(ctx, err)
	}

	// --- forward cleanup (spec §6/§7.5): a registered version that falls
	// out of the two-version window is removed after commit ---
	// A registered version is removed only when the new manifest no longer
	// keeps it (neither as current nor as previous).
	if fromInstalled && oldPrev != "" && oldPrev != newPrev && oldPrev != target {
		registered, err := p.registerVersionCleanup(oldManifest, oldPrev)
		if err != nil {
			return t.wrapUp(ctx, err)
		}
		if !registered {
			t.warnings = append(t.warnings, fmt.Sprintf("previous sing-box version %s is not registered in the manifest; it was left in place", oldPrev))
		}
	}

	result := t.commit(ctx)
	if result.HasOutcome && result.Outcome == transaction.OutcomeCommitted && result.Err == nil {
		rp := RegisterParams{
			Server: inst.Server, ListenPort: inst.ListenPort, UUID: secrets.UUID,
			PublicKey: mustDerivePublicKey(secrets.PrivateKey), ShortID: secrets.ShortID,
			SNI: inst.SNI, Name: inst.Name,
		}
		if fromInstalled {
			result.Stdout = ReinstallFromInstalledOutput(rp)
		} else {
			result.Stdout = ReinstallFromUninstalledOutput(rp)
		}
	}
	return result
}

// --- Uninstall / purge (spec §8.4, §8.5) ---

// UninstallParams is uninstall's input. ExportPath is --export-backup and is
// only meaningful with Purge (the CLI rejects the combination otherwise with
// exit 2; PrecheckUninstall re-enforces it).
type UninstallParams struct {
	Purge      bool
	ExportPath string
}

// UninstallPlan is PrecheckUninstall's outcome.
type UninstallPlan struct {
	Params        UninstallParams
	FromInstalled bool
}

// minTeardownSlack is this Task's own implementation choice on top of the sum
// of the file sizes that are snapshotted (spec §8.5 only says "检查磁盘空
// 间"): room for the journal, the snapshot directories and the archive-free
// bookkeeping.
const minTeardownSlack = 1 << 20

// teardownSet is everything uninstall/purge needs to know about the instance
// before the transaction begins, derived from the manifest and the live tree.
type teardownSet struct {
	fromInstalled bool
	purge         bool
	manifest      Manifest
	inst          InstanceFile
	versions      []string // every registered private sing-box version (installed source only)
	snapBytes     uint64   // sum of the sizes of every file that will be snapshotted
}

func eligibleForTeardown(env *Env, purge bool) (fromInstalled bool, err error) {
	state := DetectInstanceState(env.FS, env.Roots)
	switch {
	case state.Status == StatusInstalled:
		return true, nil
	case state.Status == StatusUninstalledKeepIdentity && purge:
		return false, nil
	case state.Status == StatusUninstalledKeepIdentity:
		return false, errors.New("realitynode: the instance is already uninstalled (identity retained); use 'uninstall --purge' to remove it completely or 'reinstall' to bring it back")
	default:
		return false, fmt.Errorf("realitynode: instance is not eligible for uninstall (status=%v, reasons=%v)", state.Status, state.Reasons)
	}
}

// dirNames lists the names directly inside dir, sorted.
func dirNames(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// requireExactly fails when dir holds anything other than the expected
// names: an unregistered file would make the final rmdir fail after
// modifications had begun, so it is refused up front (exit 1) instead.
func requireExactly(dir string, want []string) error {
	got, err := dirNames(dir)
	if err != nil {
		return fmt.Errorf("realitynode: list %s: %w", dir, err)
	}
	want = sortedCopy(want)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		return fmt.Errorf("realitynode: %s holds %v, but only %v is registered in the manifest; refusing to remove a directory with unregistered content", dir, got, want)
	}
	return nil
}

func fileSizeOf(path string) (uint64, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	return uint64(fi.Size()), nil
}

// inspectTeardown reads the manifest and instance parameters, works out the
// registered versions, verifies the instance directories hold exactly what
// the manifest registers, and totals the snapshot size. It modifies nothing.
func inspectTeardown(env *Env, purge, fromInstalled bool) (teardownSet, error) {
	set := teardownSet{fromInstalled: fromInstalled, purge: purge}
	m, err := readManifestFile(env)
	if err != nil {
		return set, fmt.Errorf("realitynode: read manifest: %w", err)
	}
	set.manifest = m
	_, inst, err := readSecretsAndInstance(env)
	if err != nil {
		return set, fmt.Errorf("realitynode: read identity: %w", err)
	}
	set.inst = inst

	r := env.Roots
	add := func(path string) error {
		n, err := fileSizeOf(path)
		if err != nil {
			return fmt.Errorf("realitynode: stat %s: %w", path, err)
		}
		set.snapBytes += n
		return nil
	}
	if fromInstalled {
		versionsDir := filepath.Join(r.BinDir, "versions")
		seen := map[string]bool{}
		for _, e := range m.Entries {
			if e.SHA256 != "" && filepath.Base(e.Path) == "sing-box" && filepath.Dir(filepath.Dir(e.Path)) == versionsDir {
				v := filepath.Base(filepath.Dir(e.Path))
				if !seen[v] {
					seen[v] = true
					set.versions = append(set.versions, v)
				}
			}
		}
		sort.Strings(set.versions)
		if len(set.versions) == 0 {
			return set, errors.New("realitynode: the manifest registers no private sing-box binary")
		}
		if err := requireExactly(r.ConfigDir, []string{filepath.Base(r.Config)}); err != nil {
			return set, err
		}
		if err := requireExactly(r.BinDir, []string{"versions"}); err != nil {
			return set, err
		}
		if err := requireExactly(versionsDir, set.versions); err != nil {
			return set, err
		}
		for _, v := range set.versions {
			if err := requireExactly(filepath.Join(versionsDir, v), []string{"sing-box"}); err != nil {
				return set, err
			}
			if err := add(versionBinaryPath(r, v)); err != nil {
				return set, err
			}
		}
		for _, p := range []string{r.SystemdUnit, r.Config} {
			if err := add(p); err != nil {
				return set, err
			}
		}
	}
	if purge {
		if err := requireExactly(r.StateDir, []string{filepath.Base(r.Secrets), filepath.Base(r.Instance), filepath.Base(r.Manifest)}); err != nil {
			return set, err
		}
		for _, p := range []string{r.Secrets, r.Instance, r.Manifest} {
			if err := add(p); err != nil {
				return set, err
			}
		}
	} else if err := add(r.Manifest); err != nil {
		return set, err
	}
	return set, nil
}

func checkTeardownDisk(env *Env, set teardownSet) error {
	checkDisk := env.CheckDiskSpace
	if checkDisk == nil {
		checkDisk = defaultCheckDiskSpace
	}
	return checkDisk(filepath.Dir(env.Roots.StateDir), set.snapBytes+minTeardownSlack)
}

func checkTeardownPlatform(env *Env) error {
	if env.GOOS != "" && env.GOOS != "linux" {
		return fmt.Errorf("realitynode: unsupported OS %q (only linux is supported)", env.GOOS)
	}
	return nil
}

// PrecheckUninstall is spec §8's step 3 for uninstall and uninstall --purge,
// before any lock: state eligibility (§4.3), the instance directories hold
// only registered content, disk space, and (with --export-backup) a usable
// target. It modifies nothing and never runs a role check: §8's recheck table
// lists only the instance state for these commands.
func PrecheckUninstall(ctx context.Context, env *Env, params UninstallParams) (UninstallPlan, error) {
	if params.ExportPath != "" && !params.Purge {
		return UninstallPlan{}, errors.New("realitynode: --export-backup is only valid with --purge")
	}
	if err := checkTeardownPlatform(env); err != nil {
		return UninstallPlan{}, err
	}
	fromInstalled, err := eligibleForTeardown(env, params.Purge)
	if err != nil {
		return UninstallPlan{}, err
	}
	set, err := inspectTeardown(env, params.Purge, fromInstalled)
	if err != nil {
		return UninstallPlan{}, err
	}
	if err := checkTeardownDisk(env, set); err != nil {
		return UninstallPlan{}, err
	}
	if params.ExportPath != "" {
		if err := ValidateExportTarget(env, params.ExportPath); err != nil {
			return UninstallPlan{}, err
		}
	}
	return UninstallPlan{Params: params, FromInstalled: fromInstalled}, nil
}

// Uninstall runs PrecheckUninstall then RunUninstall back to back, for callers
// (tests) that do not interpose their own locking.
func Uninstall(ctx context.Context, env *Env, params UninstallParams) Result {
	plan, err := PrecheckUninstall(ctx, env, params)
	if err != nil {
		return preTxnFail(err)
	}
	return RunUninstall(ctx, env, plan)
}

// RunUninstall is everything after the locks are held: the §8 recheck (the
// instance state, plus the same directory/disk safety checks against the now
// stable tree), then — for purge with --export-backup, strictly after the
// recheck and before the transaction — the archive, then the transaction
// (§8.4 / §8.5).
func RunUninstall(ctx context.Context, env *Env, plan UninstallPlan) Result {
	params := plan.Params
	if params.ExportPath != "" && !params.Purge {
		return preTxnFail(errors.New("realitynode: --export-backup is only valid with --purge"))
	}
	fromInstalled, err := eligibleForTeardown(env, params.Purge)
	if err != nil {
		return preTxnFail(fmt.Errorf("realitynode: instance is no longer eligible: %w", err))
	}
	set, err := inspectTeardown(env, params.Purge, fromInstalled)
	if err != nil {
		return preTxnFail(err)
	}
	if err := checkTeardownDisk(env, set); err != nil {
		return preTxnFail(err)
	}

	var warnings, notices []string
	if params.ExportPath != "" {
		if err := ExportBackup(ctx, env, params.ExportPath); err != nil {
			return preTxnFail(fmt.Errorf("export backup: %w", err))
		}
		warnings = append(warnings, fmt.Sprintf("the archive %s contains the instance's private key (config.json and secrets.json); store it securely", params.ExportPath))
		notices = append(notices, "the archive only preserves keys and instance parameters for manual reference; there is no import command, and extracting it back in place is not a supported restore")
	}
	res := runTeardown(ctx, env, set)
	res.Warnings = append(warnings, res.Warnings...)
	if res.HasOutcome && res.Outcome == transaction.OutcomeCommitted && res.Err == nil {
		res.Notices = append(append(res.Notices, notices...), SharedAssetsKeptNotice)
		if set.purge {
			res.Stdout = PurgeOutput(set.inst.Name)
		} else {
			res.Stdout = UninstallOutput(set.inst.Name)
		}
	}
	return res
}

// doSnapshot registers and executes a snapshot step for an existing managed
// file and returns the live state it recorded (the Pre of the removal that
// follows). dynamic files (secrets.json, instance.json) are identified by
// schema name, everything else by content hash.
func (p *planner) doSnapshot(ctx context.Context, id, rootKey, relPath string, dynamic bool) (transaction.PathState, error) {
	mp := mpFor(p.roots, rootKey, relPath)
	live, err := p.engine.LiveFileState(mp, !dynamic, dynamic)
	if err != nil {
		return live, fmt.Errorf("realitynode: read live state of %s: %w", mp.TargetKey(), err)
	}
	snapTmp := p.seq.next()
	sid := id + "-snap"
	if err := p.appendStep(transaction.Step{ID: sid, Kind: transaction.KindSnapshot, Target: mp.TargetKey(), Pre: live, Post: live, Status: transaction.StatusPending, Temps: []string{snapTmp}}); err != nil {
		return live, err
	}
	if err := lifecycleHook("reg:" + sid); err != nil {
		return live, err
	}
	if err := p.engine.Snapshot(ctx, sid, mp, p.snapDir, snapTmp); err != nil {
		return live, err
	}
	return live, lifecycleHook("done:" + sid)
}

// doRemoveFile registers and executes a remove_file step whose Pre is the
// state its snapshot recorded.
func (p *planner) doRemoveFile(ctx context.Context, id, rootKey, relPath string, pre transaction.PathState) error {
	mp := mpFor(p.roots, rootKey, relPath)
	if err := p.appendStep(transaction.Step{ID: id, Kind: transaction.KindRemoveFile, Target: mp.TargetKey(), Pre: pre, Post: notExist(), Status: transaction.StatusPending}); err != nil {
		return err
	}
	if err := lifecycleHook("reg:" + id); err != nil {
		return err
	}
	if err := p.engine.RemoveFile(ctx, id, mp); err != nil {
		return err
	}
	return lifecycleHook("done:" + id)
}

// doRemoveDir registers and executes a remove_dir step. Its Pre is the
// directory's live mode/owner, which is also exactly what a rollback re-creates.
func (p *planner) doRemoveDir(ctx context.Context, id, rootKey, relPath string) error {
	mp := mpFor(p.roots, rootKey, relPath)
	live, _, err := p.engine.LiveDirState(mp)
	if err != nil {
		return fmt.Errorf("realitynode: read live state of %s: %w", mp.TargetKey(), err)
	}
	if !live.Exists {
		return fmt.Errorf("realitynode: %s does not exist", mp.TargetKey())
	}
	if err := p.appendStep(transaction.Step{ID: id, Kind: transaction.KindRemoveDir, Target: mp.TargetKey(), Pre: live, Post: notExist(), Status: transaction.StatusPending}); err != nil {
		return err
	}
	if err := lifecycleHook("reg:" + id); err != nil {
		return err
	}
	if err := p.engine.RemoveDir(ctx, id, mp); err != nil {
		return err
	}
	return lifecycleHook("done:" + id)
}

// buildUninstalledManifest is spec §5.2's uninstalled manifest: only the
// state directory and the two dynamic-state files stay registered.
func buildUninstalledManifest(env *Env, uid, gid uint32, current string) ([]byte, error) {
	r := env.Roots
	m := Manifest{
		Schema: manifestSchema, State: ManifestStateUninstalled, SingboxCurrent: current,
		Entries: []ManifestEntry{
			dirEntry(r.StateDir, 0700, uid, gid),
			dynamicEntry(r.Secrets, 0600, uid, gid),
			dynamicEntry(r.Instance, 0600, uid, gid),
		},
	}
	return json.MarshalIndent(m, "", "  ")
}

// runTeardown is the transaction shared by uninstall (§8.4) and purge (§8.5):
//
//	snapshot -> [stop, disable, remove unit, daemon-reload] -> remove config,
//	private binaries -> (purge) remove identity, manifest, state dir |
//	(uninstall) write the uninstalled manifest -> commit
//
// Every step is journalled before it runs (planner.*), so any failure or
// crash converges through the one shared Recoverer (rollback from the
// snapshots, including systemd) — there is no teardown-specific recovery.
func runTeardown(ctx context.Context, env *Env, set teardownSet) Result {
	oldCur := set.manifest.SingboxCurrent
	if oldCur == "" {
		oldCur = "0.0.0"
	}
	versions := []string{oldCur}
	for _, v := range set.versions {
		if v != oldCur {
			versions = append(versions, v)
		}
	}
	op := "uninstall"
	if set.purge {
		op = "uninstall-purge"
	}
	layout := newInstanceLayout(env.Roots, oldCur)

	rollbackBin := ""
	if set.fromInstalled {
		rollbackBin = versionBinaryPath(env.Roots, set.manifest.SingboxCurrent)
	}
	// The rollback re-runs the §9.2 gate against the pre-transaction binary
	// (spec §7.4 step 5); it is consulted only when the unit was active.
	rollbackGate := gateFor(env, set.inst.ListenPort, env.Roots.Config, func() string { return rollbackBin })

	beforeVersion := ""
	if set.fromInstalled {
		beforeVersion = set.manifest.SingboxCurrent
	}
	t, err := beginTxn(ctx, env, beginOpts{op: op, versions: versions, versionBefore: beforeVersion, recoveryGate: rollbackGate})
	if err != nil {
		return preTxnFail(err)
	}
	defer t.close()
	p := t.p
	uid32, gid32 := t.uid, t.gid

	if err := lifecycleHook("began"); err != nil {
		return t.wrapUp(ctx, err)
	}
	if err := t.openSnapDir(); err != nil {
		return t.wrapUp(ctx, err)
	}

	// --- snapshots of everything that will be modified or removed (before
	// the first modification, so a failure here is still exit 1) ---
	type snap struct{ pre transaction.PathState }
	snaps := map[string]snap{}
	take := func(id, rootKey, rel string, dynamic bool) error {
		pre, err := p.doSnapshot(ctx, id, rootKey, rel, dynamic)
		snaps[id] = snap{pre}
		return err
	}
	if set.fromInstalled {
		if err := take("unit", rootKeySystemd, layout.unitRel, false); err != nil {
			return t.wrapUp(ctx, err)
		}
		if err := take("config", rootKeyEtc, layout.configRel, false); err != nil {
			return t.wrapUp(ctx, err)
		}
		for _, v := range set.versions {
			vl := newInstanceLayout(env.Roots, v)
			if err := take("binary-"+v, rootKeyUsrLib, vl.binaryRel, false); err != nil {
				return t.wrapUp(ctx, err)
			}
		}
	}
	if set.purge {
		if err := take("secrets", rootKeyVarLib, layout.secretsRel, true); err != nil {
			return t.wrapUp(ctx, err)
		}
		if err := take("instance", rootKeyVarLib, layout.instanceRel, true); err != nil {
			return t.wrapUp(ctx, err)
		}
		if err := take("manifest", rootKeyVarLib, layout.manifestRel, false); err != nil {
			return t.wrapUp(ctx, err)
		}
	}
	if err := lifecycleHook("snapshots-done"); err != nil {
		return t.wrapUp(ctx, err)
	}

	if set.fromInstalled {
		// --- stop, disable, remove the unit, daemon-reload (§8.4 steps 2-3).
		// The systemd chain: stop (state unchanged), disable (-> disabled),
		// daemon-reload once the unit file is gone (-> the unit no longer
		// exists, UnitFileState ""), so the journal's chain matches what
		// systemd reports at every crash point. ---
		uKey := unitStateTargetKey(env.Roots)
		before := transaction.PathState{UnitFileState: t.unitBefore.UnitFileState}
		disabled := transaction.PathState{UnitFileState: "disabled"}
		gone := transaction.PathState{}
		if err := p.runSystemdStep(ctx, "systemd-stop", transaction.KindSystemdStop, uKey, before, before, func(c context.Context) error { return env.Systemd.Stop(c, unitName) }); err != nil {
			return t.wrapUp(ctx, err)
		}
		if err := p.runSystemdStep(ctx, "systemd-disable", transaction.KindSystemdDisable, uKey, before, disabled,
			func(c context.Context) error { return env.Systemd.SetUnitFileState(c, unitName, "disabled") }); err != nil {
			return t.wrapUp(ctx, err)
		}
		if err := p.doRemoveFile(ctx, "remove-unit", rootKeySystemd, layout.unitRel, snaps["unit"].pre); err != nil {
			return t.wrapUp(ctx, err)
		}
		if err := p.runSystemdStep(ctx, "systemd-daemon-reload", transaction.KindSystemdDaemonReload, uKey, disabled, gone, env.Systemd.DaemonReload); err != nil {
			return t.wrapUp(ctx, err)
		}

		// --- config file and directory (§8.4 step 4) ---
		if err := p.doRemoveFile(ctx, "remove-config", rootKeyEtc, layout.configRel, snaps["config"].pre); err != nil {
			return t.wrapUp(ctx, err)
		}
		if err := p.doRemoveDir(ctx, "remove-configdir", rootKeyEtc, layout.configDirRel); err != nil {
			return t.wrapUp(ctx, err)
		}

		// --- private binaries: files, version directories, then the two
		// shared directories (§8.4 step 5) ---
		for _, v := range set.versions {
			vl := newInstanceLayout(env.Roots, v)
			if err := p.doRemoveFile(ctx, "remove-binary-"+v, rootKeyUsrLib, vl.binaryRel, snaps["binary-"+v].pre); err != nil {
				return t.wrapUp(ctx, err)
			}
			if err := p.doRemoveDir(ctx, "remove-versiondir-"+v, rootKeyUsrLib, vl.versionDirRel); err != nil {
				return t.wrapUp(ctx, err)
			}
		}
		if err := p.doRemoveDir(ctx, "remove-versionsdir", rootKeyUsrLib, layout.versionsDirRel); err != nil {
			return t.wrapUp(ctx, err)
		}
		if err := p.doRemoveDir(ctx, "remove-bindir", rootKeyUsrLib, layout.binDirRel); err != nil {
			return t.wrapUp(ctx, err)
		}
	}

	if set.purge {
		// --- identity, manifest, then the state directory itself (§8.5
		// step 5); the purge commits only after all of it is gone ---
		if err := p.doRemoveFile(ctx, "remove-secrets", rootKeyVarLib, layout.secretsRel, snaps["secrets"].pre); err != nil {
			return t.wrapUp(ctx, err)
		}
		if err := p.doRemoveFile(ctx, "remove-instance", rootKeyVarLib, layout.instanceRel, snaps["instance"].pre); err != nil {
			return t.wrapUp(ctx, err)
		}
		if err := p.doRemoveFile(ctx, "remove-manifest", rootKeyVarLib, layout.manifestRel, snaps["manifest"].pre); err != nil {
			return t.wrapUp(ctx, err)
		}
		if err := p.doRemoveDir(ctx, "remove-statedir", rootKeyVarLib, layout.stateDirRel); err != nil {
			return t.wrapUp(ctx, err)
		}
	} else {
		// --- the uninstalled manifest is the commit point (§7.5) ---
		manifestBytes, err := buildUninstalledManifest(env, uid32, gid32, set.manifest.SingboxCurrent)
		if err != nil {
			return t.wrapUp(ctx, err)
		}
		if err := p.doOverwriteFile(ctx, "manifest", rootKeyVarLib, layout.manifestRel, manifestBytes, 0600, uid32, gid32, false); err != nil {
			return t.wrapUp(ctx, err)
		}
	}
	return t.commit(ctx)
}

// --- Recover (spec §8.6, §7.4-§7.6) ---

// RecoverParams is recover's input: Abandon is --abandon (spec §7.4's manual
// escape hatch).
type RecoverParams struct {
	Abandon bool
}

// PrecheckRecover is recover's quick precheck and post-lock recheck alike
// (spec §8: "recover: 实例状态必须是未完成事务").
func PrecheckRecover(env *Env) error {
	if state := DetectInstanceState(env.FS, env.Roots); state.Status != StatusUnfinishedTxn {
		return fmt.Errorf("realitynode: there is no unfinished transaction to recover (status=%v, reasons=%v)", state.Status, state.Reasons)
	}
	return nil
}

func needsAttention(path string, err error) Result { return Result{Err: err, TxnDir: path} }

func recovered(o transaction.Outcome, notices ...string) Result {
	return Result{Recovered: true, RecoveredOutcome: o, Notices: notices}
}

// RunRecover recovers a leftover transaction (spec §8.6) with the caller
// holding L1+L3. Everything is delegated to the one shared engine: a leftover
// tombstone is deleted wholesale without being read (§7.6); an area with no
// journal.json is discarded only when it holds nothing but journal.json.tmp
// (§7.6); otherwise OpenRecoverer's Recoverer resumes the journal exactly as
// the forward paths' own failure handling would — RecoverDetached for
// running/rolling_back/committed, Abandon for --abandon. Success is always
// exit 0; a refused --abandon is exit 1 with nothing changed; anything the
// engine cannot confirm (tampering, unknown schema, a failed gate, a timeout)
// is exit 5 naming the area, which is preserved.
func RunRecover(ctx context.Context, env *Env, params RecoverParams) Result {
	if err := PrecheckRecover(env); err != nil {
		return preTxnFail(err)
	}
	if err := ctx.Err(); err != nil {
		return preTxnFail(fmt.Errorf("realitynode: recover cancelled before it started: %w", err))
	}
	roots, err := openAllRoots(env.Roots, env.TrustedOwnerUID)
	if err != nil {
		return preTxnFail(err)
	}
	defer roots.closeAll()

	var notices []string
	tombs, _ := env.FS.Glob(env.Roots.TombstoneGlob)
	sort.Strings(tombs)
	for _, tomb := range tombs {
		if filepath.Dir(tomb) != filepath.Dir(env.Roots.StateDir) {
			return needsAttention(tomb, fmt.Errorf("realitynode: unexpected tombstone location %s", tomb))
		}
		if err := transaction.DeleteTombstone(roots.varLib, filepath.Base(tomb)); err != nil {
			return needsAttention(tomb, fmt.Errorf("realitynode: delete tombstone %s: %w", tomb, err))
		}
		notices = append(notices, "deleted the leftover tombstone "+tomb+" (a finished transaction's remains; its contents were not read)")
	}

	if _, err := os.Lstat(env.Roots.TxnDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return recovered(transaction.OutcomeCommitted, notices...) // only a tombstone was left
		}
		return needsAttention(env.Roots.TxnDir, err)
	}
	switch err := transaction.RecoverEmptyTransactionArea(roots.varLib, txnDirName(env.Roots)); {
	case err == nil:
		return recovered(transaction.OutcomeNoChange, append(notices, "removed a transaction area with no journal: the crash happened while the transaction was starting, so nothing was modified")...)
	case errors.Is(err, transaction.ErrJournalPresent):
		// a journal exists: resume it below
	default:
		return needsAttention(env.Roots.TxnDir, err)
	}
	_ = lifecycleHook("recover:opening")

	rec, closeFn, err := OpenRecoverer(env)
	if err != nil {
		return needsAttention(env.Roots.TxnDir, fmt.Errorf("realitynode: cannot open the leftover transaction: %w", err))
	}
	defer closeFn()

	if params.Abandon {
		// Like every recovery, abandon must not be cut short by a signal
		// halfway through deleting the area.
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), transaction.DefaultRollbackBudget)
		defer cancel()
		outcome, err := rec.Abandon(actx)
		switch {
		case err == nil:
			r := recovered(outcome, notices...)
			r.Warnings = []string{"the transaction was abandoned without restarting the service or running the self-check: the service may not be running; check it with 'systemctl status proxyctl-reality' and 'sudo proxyctl node service show', and run 'reinstall' if needed"}
			return r
		case errors.Is(err, transaction.ErrAbandonNotEligible):
			return preTxnFail(err)
		default:
			return needsAttention(env.Roots.TxnDir, err)
		}
	}

	outcome, err := rec.RecoverDetached(ctx)
	if err != nil {
		return needsAttention(env.Roots.TxnDir, fmt.Errorf("realitynode: recover did not finish: %w", err))
	}
	switch outcome {
	case transaction.OutcomeRolledBack:
		notices = append(notices, "rolled the interrupted operation back to its pre-transaction state")
	case transaction.OutcomeCommitted:
		notices = append(notices, "the interrupted operation had already committed; finished its forward cleanup")
	case transaction.OutcomeNoChange:
		notices = append(notices, "the interrupted operation had modified nothing; its transaction area was removed")
	}
	notices = append(notices, "run 'sudo proxyctl node service show' to see the current state")
	return recovered(outcome, notices...)
}
